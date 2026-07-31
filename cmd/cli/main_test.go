package main

import (
	"testing"

	"github.com/spf13/cobra"
	"github.com/yusheng-g/openagent-go/cmd/cli/server"
	"github.com/yusheng-g/openagent-go/version"
)

// newCapCmd creates a cobra.Command with capability flags and calls
// addCapabilityFlags + parseCapabilities, returning the result.
func newCapCmd(args []string) server.Capabilities {
	cmd := &cobra.Command{}
	addCapabilityFlags(cmd)
	// Cobra parses os.Args by default; override with explicit args.
	cmd.SetArgs(args)
	_ = cmd.Execute()

	var caps server.Capabilities
	parseCapabilities(cmd, &caps)
	return caps
}

func TestParseCapabilities_Defaults(t *testing.T) {
	caps := newCapCmd([]string{})
	if !caps.OnMemory() {
		t.Error("memory default: want on")
	}
	if !caps.OnSummarizer() {
		t.Error("summarizer default: want on")
	}
	if !caps.OnSkills() {
		t.Error("skills default: want on")
	}
	if !caps.OnMCP() {
		t.Error("mcp default: want on")
	}
	if caps.OnGuard() {
		t.Error("guard default: want off")
	}
	if caps.OnApprover() {
		t.Error("approver default: want off")
	}
}

func TestParseCapabilities_ExplicitOnOff(t *testing.T) {
	caps := newCapCmd([]string{
		"--memory=off", "--summarizer=off",
		"--skills=off", "--mcp=off",
		"--guard=on", "--approver=on",
	})
	if caps.OnMemory() {
		t.Error("memory=off: want off")
	}
	if caps.OnSummarizer() {
		t.Error("summarizer=off: want off")
	}
	if caps.OnSkills() {
		t.Error("skills=off: want off")
	}
	if caps.OnMCP() {
		t.Error("mcp=off: want off")
	}
	if !caps.OnGuard() {
		t.Error("guard=on: want on")
	}
	if !caps.OnApprover() {
		t.Error("approver=on: want on")
	}
}

func TestParseCapabilities_InvalidValue(t *testing.T) {
	// Invalid values should be ignored → Capabilities uses defaults.
	caps := newCapCmd([]string{
		"--memory=maybe",  // invalid → use default (on)
		"--guard=invalid", // invalid → use default (off)
	})
	if !caps.OnMemory() {
		t.Error("memory=maybe (invalid) → should fall back to default on")
	}
	if caps.OnGuard() {
		t.Error("guard=invalid → should fall back to default off")
	}
}

func TestParseCapabilities_PartialOverride(t *testing.T) {
	// Only override one flag; others stay default.
	caps := newCapCmd([]string{"--memory=off"})
	if caps.OnMemory() {
		t.Error("memory=off: want off")
	}
	// All others should use defaults.
	if !caps.OnSummarizer() {
		t.Error("summarizer default: want on")
	}
	if caps.OnGuard() {
		t.Error("guard default: want off")
	}
}

func TestParseCapabilities_CaseInsensitive(t *testing.T) {
	// "ON"/"OFF", "On"/"Off" should all be accepted (case-insensitive).
	caps := newCapCmd([]string{
		"--memory=ON", "--summarizer=On",
		"--skills=OFF", "--mcp=on",
		"--guard=ON", "--approver=On",
	})
	if !caps.OnMemory() {
		t.Error("memory=ON → should be on")
	}
	if !caps.OnSummarizer() {
		t.Error("summarizer=On → should be on")
	}
	if caps.OnSkills() {
		t.Error("skills=OFF → should be off")
	}
	if !caps.OnMCP() {
		t.Error("mcp=on → should be on")
	}
	if !caps.OnGuard() {
		t.Error("guard=ON → should be on")
	}
	if !caps.OnApprover() {
		t.Error("approver=On → should be on")
	}
}

// TestRootCmdVersionWired ensures rootCmd.Version mirrors version.Version
// so cobra's --version path prints the right string.
func TestRootCmdVersionWired(t *testing.T) {
	if rootCmd.Version != version.Version {
		t.Errorf("rootCmd.Version = %q, want %q", rootCmd.Version, version.Version)
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
	if f.Usage != "show version" {
		t.Errorf("version flag usage = %q, want %q", f.Usage, "show version")
	}
}
