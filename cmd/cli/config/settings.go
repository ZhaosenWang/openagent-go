package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// settingsMu serializes read-modify-write cycles on the settings file
// process-wide. Every writer (feishu credentials, future settings
// commands) goes through UpdateSettings, so concurrent updates never
// lose each other's fields. The file write itself is atomic (temp +
// rename); the lock protects the whole load-edit-store cycle.
var settingsMu sync.Mutex

// UpdateSettings reads the settings file, applies fn to its JSON map,
// and writes it back atomically. Unknown and unrelated fields are
// preserved verbatim (the map is only touched by fn). Concurrent-safe.
//
// fn must only mutate the map passed to it. Returning an error aborts
// the update (nothing is written).
func UpdateSettings(fn func(raw map[string]json.RawMessage) error) error {
	settingsMu.Lock()
	defer settingsMu.Unlock()

	p, err := Path()
	if err != nil {
		return fmt.Errorf("settings path: %w", err)
	}
	raw := map[string]json.RawMessage{}
	if data, rerr := os.ReadFile(p); rerr == nil {
		if err := json.Unmarshal(data, &raw); err != nil {
			return fmt.Errorf("settings parse: %w", err)
		}
	} else if !os.IsNotExist(rerr) {
		return fmt.Errorf("settings read: %w", rerr)
	}
	if err := fn(raw); err != nil {
		return err
	}
	data, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return fmt.Errorf("settings marshal: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return fmt.Errorf("settings dir: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), "settings-*.tmp")
	if err != nil {
		return fmt.Errorf("settings temp: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("settings write: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("settings close: %w", err)
	}
	if err := os.Rename(tmpName, p); err != nil {
		return fmt.Errorf("settings save: %w", err)
	}
	return nil
}
