package wasm

import (
	"context"
	"fmt"

	"github.com/tetratelabs/wazero"

	"github.com/yusheng-g/openagent-go/keyring"
	"github.com/yusheng-g/openagent-go/plugin/wasmhost"
)

// Runtime wraps a wazero runtime with the host module pre-registered.
// All CLI plugins (.wasm files) share this runtime.
type Runtime struct {
	rt   wazero.Runtime
	host *wasmhost.HostAPI
}

// NewRuntime creates a wazero runtime and registers the "host" module
// with a standard HostAPI: system keyring (in-memory fallback), net/http
// client, and slog-backed logger. Callers that need a custom HostAPI can
// construct one via wasmhost.NewHostAPI and register it directly.
func NewRuntime(ctx context.Context) (*Runtime, error) {
	rt := wazero.NewRuntime(ctx)

	h := wasmhost.NewHostAPI(keyring.NewKeyring())
	if err := h.RegisterHostModule(ctx, rt); err != nil {
		rt.Close(ctx)
		return nil, fmt.Errorf("register host module: %w", err)
	}

	return &Runtime{rt: rt, host: h}, nil
}

// Close releases the wazero runtime.
func (r *Runtime) Close(ctx context.Context) error {
	return r.rt.Close(ctx)
}
