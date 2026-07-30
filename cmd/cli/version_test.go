package main

import (
	"regexp"
	"strings"
	"testing"
)

// TestVersionPopulated verifies init populates version. For the default
// dev build it must match 0.0.0-dev.YYYYMMDDHHMMSS; when injected via
// ldflags any non-empty value is accepted.
func TestVersionPopulated(t *testing.T) {
	if version == "" {
		t.Fatal("version is empty; init should populate it")
	}
	// An ldflags-injected version won't carry the dev prefix; nothing
	// more to validate in that case.
	if !strings.HasPrefix(version, "0.0.0-dev.") {
		return
	}
	re := regexp.MustCompile(`^0\.0\.0-dev\.\d{14}$`)
	if !re.MatchString(version) {
		t.Errorf("default version %q does not match 0.0.0-dev.YYYYMMDDHHMMSS", version)
	}
}

// TestRootCmdVersionWired ensures rootCmd.Version mirrors the version
// var so cobra's --version path prints the right string.
func TestRootCmdVersionWired(t *testing.T) {
	if rootCmd.Version != version {
		t.Errorf("rootCmd.Version = %q, want %q", rootCmd.Version, version)
	}
}

// TestVersionFlagShorthand ensures the -v shorthand and usage text are
// registered on the root command.
func TestVersionFlagShorthand(t *testing.T) {
	f := rootCmd.Flags().Lookup("version")
	if f == nil {
		t.Fatal("version flag not registered")
	}
	if f.Shorthand != "v" {
		t.Errorf("version flag shorthand = %q, want %q", f.Shorthand, "v")
	}
	if f.Usage != "show version number" {
		t.Errorf("version flag usage = %q, want %q", f.Usage, "show version number")
	}
}
