package server

import (
	"testing"

	"github.com/yusheng-g/openagent-go/cmd/cli/config"
	"github.com/yusheng-g/openagent-go/kernel"
)

// TestApplyContextProviders_EndpointSwitchesAll: a configured OpenViking
// endpoint switches memory/skill/resource to OpenViking by default — one
// address is enough.
func TestApplyContextProviders_EndpointSwitchesAll(t *testing.T) {
	cfg := &config.Config{
		OpenViking: config.OpenVikingConfig{Endpoint: "http://127.0.0.1:1933"},
	}
	var deps kernel.Deps
	if err := applyContextProviders(cfg, &deps); err != nil {
		t.Fatal(err)
	}
	if deps.MemoryProvider == nil {
		t.Error("memory provider not switched to openviking")
	}
	if deps.SkillProvider == nil {
		t.Error("skill provider not switched to openviking")
	}
	if deps.ResourceProvider == nil {
		t.Error("resource provider not switched to openviking")
	}
}

// TestApplyContextProviders_BuiltinOverride: an explicit "builtin" for a
// domain keeps the local backend even with an endpoint configured.
func TestApplyContextProviders_BuiltinOverride(t *testing.T) {
	cfg := &config.Config{
		ContextProviders: config.ContextProviderConfig{
			Memory: "builtin",
		},
		OpenViking: config.OpenVikingConfig{Endpoint: "http://127.0.0.1:1933"},
	}
	var deps kernel.Deps
	if err := applyContextProviders(cfg, &deps); err != nil {
		t.Fatal(err)
	}
	if deps.MemoryProvider != nil {
		t.Error("memory provider should stay builtin (nil) when overridden")
	}
	if deps.SkillProvider == nil {
		t.Error("skill provider should still switch to openviking")
	}
	if deps.ResourceProvider == nil {
		t.Error("resource provider should still switch to openviking")
	}
}

// TestApplyContextProviders_NoEndpoint: no endpoint = fully local, the
// openviking provider is never constructed.
func TestApplyContextProviders_NoEndpoint(t *testing.T) {
	cfg := &config.Config{}
	var deps kernel.Deps
	if err := applyContextProviders(cfg, &deps); err != nil {
		t.Fatal(err)
	}
	if deps.MemoryProvider != nil || deps.SkillProvider != nil || deps.ResourceProvider != nil {
		t.Error("no endpoint must leave all providers nil (local)")
	}
}
