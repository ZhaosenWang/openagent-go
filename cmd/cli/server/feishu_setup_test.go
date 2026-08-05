package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// isolateSettings points the settings file at a temp directory via
// OPENAGENT_CLI_CONFIG so real user settings are never touched.
func isolateSettings(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "settings.json")
	t.Setenv("OPENAGENT_CLI_CONFIG", p)
	return p
}

// isolateProfiles points profile resolution at a temp directory: CWD is
// moved (resolveProfilesDir prefers $(pwd)/profiles) and HOME is set so
// the fallback lands in the same sandbox. Real credentials are never
// touched.
func isolateProfiles(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("HOME", dir)
	return ".openagent/profile"
}

// saveFeishuToSettings must preserve every other settings field (user
// settings, unknown future fields) and write atomically.
func TestSaveFeishuToSettingsPreservesOtherFields(t *testing.T) {
	p := isolateSettings(t)
	existing := map[string]any{
		"provider":             map[string]any{"openai": map[string]any{"api_key": "sk-old"}},
		"unknown_future_field": map[string]any{"keep": "me"},
	}
	data, _ := json.MarshalIndent(existing, "", "  ")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := saveFeishuToSettings("cli_new", "secret_new"); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("settings not valid JSON: %v", err)
	}
	// Unknown / unrelated fields survive (valid JSON, same content).
	var unknown map[string]string
	if err := json.Unmarshal(got["unknown_future_field"], &unknown); err != nil {
		t.Fatalf("unknown field mangled: %s (%v)", got["unknown_future_field"], err)
	}
	if unknown["keep"] != "me" {
		t.Fatalf("unknown field content lost: %+v", unknown)
	}
	if !json.Valid(got["provider"]) {
		t.Fatalf("provider field mangled: %s", got["provider"])
	}
	// channels.feishu carries the new credentials.
	var channels map[string]struct {
		AppID     string `json:"app_id"`
		AppSecret string `json:"app_secret"`
	}
	if err := json.Unmarshal(got["channels"], &channels); err != nil {
		t.Fatalf("channels not valid: %v", err)
	}
	feishu, ok := channels["feishu"]
	if !ok {
		t.Fatalf("channels.feishu missing: %+v", channels)
	}
	if feishu.AppID != "cli_new" || feishu.AppSecret != "secret_new" {
		t.Fatalf("feishu = %+v", feishu)
	}
}

// Creating the settings file from scratch works too.
func TestSaveFeishuToSettingsCreatesFile(t *testing.T) {
	p := isolateSettings(t)
	if err := saveFeishuToSettings("cli_fresh", "secret_fresh"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("settings not created: %v", err)
	}
}

// Concurrent submissions must not lose updates (read-modify-write
// serialized by the package mutex).
func TestSaveFeishuToSettingsConcurrent(t *testing.T) {
	p := isolateSettings(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := saveFeishuToSettings("cli_keep", "secret_keep"); err != nil {
				t.Errorf("save %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	// The file must be valid JSON after the concurrent storm (no torn
	// writes / interleaved cycles).
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(raw) {
		t.Fatalf("settings corrupt after concurrent saves: %s", raw)
	}
}
