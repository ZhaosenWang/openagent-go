package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

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

func TestSaveAndLoadFeishuAppFile(t *testing.T) {
	profiles := isolateProfiles(t)
	path := feishuAppPath(profiles)
	if !filepath.IsAbs(path) {
		t.Fatalf("credential path not absolute: %q", path)
	}
	if filepath.Dir(path) != filepath.Join(profilesDir(t, profiles), "channel", "feishu") {
		t.Fatalf("credential path not under $profile/channel/feishu: %q", path)
	}

	// Initial load should return false.
	_, ok := loadFeishuAppFile(profiles)
	if ok {
		t.Fatal("expected no credentials before save")
	}

	// Save credentials.
	creds := FeishuCredentials{AppID: "cli_test123", AppSecret: "secret456"}
	saveFeishuAppFile(profiles, creds)

	// Verify file exists, is valid JSON, and is 0600.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read saved file: %v", err)
	}
	var decoded FeishuCredentials
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("saved file is not valid JSON: %v", err)
	}
	if decoded.AppID != "cli_test123" || decoded.AppSecret != "secret456" {
		t.Errorf("decoded = %+v", decoded)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("credential file perms = %v, want 0600", info.Mode().Perm())
	}

	// Load should succeed.
	loaded, ok := loadFeishuAppFile(profiles)
	if !ok {
		t.Fatal("expected credentials after save")
	}
	if loaded.AppID != "cli_test123" || loaded.AppSecret != "secret456" {
		t.Errorf("loaded = %+v", loaded)
	}
}

func TestLoadFeishuAppFileEmptyFields(t *testing.T) {
	profiles := isolateProfiles(t)

	// Save empty credentials — load should return false.
	p := feishuAppPath(profiles)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(`{"app_id":"","app_secret":""}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, ok := loadFeishuAppFile(profiles)
	if ok {
		t.Fatal("empty credentials should not be considered valid")
	}
}

func TestLoadFeishuAppFileMissing(t *testing.T) {
	profiles := isolateProfiles(t)
	_, ok := loadFeishuAppFile(profiles)
	if ok {
		t.Fatal("missing file should return false")
	}
}

// Legacy credentials (~/.openagent/data/feishu_app.json) must migrate
// into the profile location on first load.
func TestLoadFeishuAppFileMigratesLegacy(t *testing.T) {
	profiles := isolateProfiles(t)
	home, _ := os.UserHomeDir()
	legacy := filepath.Join(home, ".openagent", "data", "feishu_app.json")
	if err := os.MkdirAll(filepath.Dir(legacy), 0o755); err != nil {
		t.Fatal(err)
	}
	legacyCreds := FeishuCredentials{AppID: "cli_legacy", AppSecret: "legacy_secret"}
	data, _ := json.Marshal(legacyCreds)
	if err := os.WriteFile(legacy, data, 0o600); err != nil {
		t.Fatal(err)
	}

	loaded, ok := loadFeishuAppFile(profiles)
	if !ok {
		t.Fatal("legacy credentials should load")
	}
	if loaded.AppID != "cli_legacy" {
		t.Fatalf("loaded = %+v", loaded)
	}
	// Migrated copy exists in the profile location; legacy file untouched.
	if _, err := os.Stat(feishuAppPath(profiles)); err != nil {
		t.Fatalf("migrated copy missing: %v", err)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatalf("legacy file removed: %v", err)
	}
}

// profilesDir resolves the profile root the same way feishuAppPath does.
func profilesDir(t *testing.T, profiles string) string {
	t.Helper()
	return resolveProfilesDir(profiles)
}
