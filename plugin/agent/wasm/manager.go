package wasm

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	openagent "github.com/yusheng-g/openagent-go"
	"github.com/yusheng-g/openagent-go/plugin/wasmhost"
)

// Manager discovers and manages WASM plugins from a directory.
type Manager struct {
	dir string

	mu        sync.Mutex
	ldr       loader
	tools     []openagent.Tool
	observers []*wasmObserver

	hostAPI *wasmhost.HostAPI

	onAbort func(reason string)

	// observeCh feeds the single background observer worker. Created by
	// Observer(); never closed (a close racing a send would panic — the
	// worker goroutine lives for the manager's lifetime and drains on
	// process exit; Close only detaches the channel so no new events are
	// accepted).
	observeCh chan openagent.StageEvent
}

// NewManager creates a Manager for the given plugin directory.
func NewManager(dir string) *Manager {
	return &Manager{dir: dir}
}

// WithHostAPI configures the host exports (keyring_get/set, http_request,
// log_info/warn/error) that WASM plugins can import via the "host" module.
// Call before [Manager.Discover].
func (m *Manager) WithHostAPI(h *wasmhost.HostAPI) *Manager {
	m.hostAPI = h
	return m
}

// OnAbort registers a callback invoked when a stage plugin returns action=abort.
func (m *Manager) OnAbort(fn func(reason string)) {
	m.mu.Lock()
	m.onAbort = fn
	m.mu.Unlock()
}

// Discover scans the plugin directory for .wasm files, instantiates each one,
// reads its metadata, and registers it as a Tool or Stage plugin.
func (m *Manager) Discover(ctx context.Context) error {
	if m.dir == "" {
		return nil
	}

	entries, err := os.ReadDir(m.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("plugin dir: %w", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Lazy-init wazero runtime.
	if m.ldr.runtime == nil {
		ldr, err := newLoader(ctx)
		if err != nil {
			return fmt.Errorf("init wazero: %w", err)
		}
		// Register host exports BEFORE loading any plugin module.
		if m.hostAPI != nil {
			if err := m.hostAPI.RegisterHostModule(ctx, ldr.runtime); err != nil {
				ldr.Close(ctx)
				return fmt.Errorf("register host module: %w", err)
			}
		}
		m.ldr = ldr
	}

	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".wasm" {
			continue
		}
		path := filepath.Join(m.dir, entry.Name())
		if err := m.loadOne(ctx, path); err != nil {
			// One broken plugin must not disable the rest: skip it and
			// keep discovering.
			slog.Error("openagent: wasm plugin load failed, skipping", "plugin", entry.Name(), "error", err)
		}
	}

	return nil
}

func (m *Manager) loadOne(ctx context.Context, path string) error {
	wasmBytes, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}

	mod, err := m.ldr.loadModule(ctx, filepath.Base(path), wasmBytes)
	if err != nil {
		return err
	}

	meta, err := mod.parseMeta(ctx)
	if err != nil {
		return err
	}

	switch meta.Type {
	case PluginTypeTools:
		m.tools = append(m.tools, &wasmTool{mod: mod, meta: meta})
	case PluginTypeObservers:
		m.observers = append(m.observers, &wasmObserver{mod: mod, meta: meta, name: meta.Name})
	default:
		slog.Info("wasm skipping unknown plugin type", "file", filepath.Base(path), "type", meta.Type)
		return nil
	}

	return nil
}

// Tools returns loaded Tool plugins as openagent.Tool values (a copy —
// callers must not mutate the manager's internal slice).
func (m *Manager) Tools() []openagent.Tool {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]openagent.Tool, len(m.tools))
	copy(out, m.tools)
	return out
}

// Observer returns a RunObserver that dispatches to matching Stage plugins.
func (m *Manager) Observer() openagent.RunObserver {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.observers) == 0 {
		return nil
	}
	if m.observeCh == nil {
		m.observeCh = make(chan openagent.StageEvent, 64)
		go m.observeLoop()
	}
	return &observerRouter{mgr: m}
}

// Close releases the wazero runtime and detaches the observer queue (no
// new events accepted; the worker drains and exits with the process).
func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.observeCh = nil
	if m.ldr.runtime == nil {
		return nil
	}
	return m.ldr.Close(context.Background())
}

// observerRouter dispatches stage events to matching WASM stage plugins on
// a background worker: a slow or broken plugin must not block the agent
// main loop. Events stay ordered (single worker); a panic in one plugin is
// contained and logged. An abort action stops dispatch for that event and
// fires the registered callback.
type observerRouter struct {
	mgr *Manager
}

func (o *observerRouter) ObserveStage(_ context.Context, event openagent.StageEvent) {
	o.mgr.dispatch(event)
}

// dispatch enqueues a stage event for the background worker. When the
// queue is full (a stuck observer), the event is dropped with a warning —
// observing must never stall the run.
func (m *Manager) dispatch(event openagent.StageEvent) {
	m.mu.Lock()
	ch := m.observeCh
	m.mu.Unlock()
	if ch == nil {
		return // manager closed
	}
	select {
	case ch <- event:
	default:
		slog.Warn("openagent: wasm observer queue full, dropping stage event", "stage", event.Name, "phase", event.Phase)
	}
}

// observeLoop is the single worker consuming stage events in order.
func (m *Manager) observeLoop() {
	for ev := range m.observeCh {
		m.runObservers(ev)
	}
}

func (m *Manager) runObservers(event openagent.StageEvent) {
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("openagent: wasm observer panicked", "panic", rec)
		}
	}()
	m.mu.Lock()
	stages := m.observers
	onAbort := m.onAbort
	m.mu.Unlock()

	for _, s := range stages {
		if !s.matches(event) {
			continue
		}
		out, err := s.invoke(context.Background(), event)
		if err != nil {
			slog.Error("wasm observer error", "plugin", s.meta.Name, "stage", event.Name, "phase", event.Phase, "error", err)
			continue
		}
		if out != nil && out.Action == ActionAbort {
			slog.Info("wasm observer abort", "plugin", s.meta.Name, "stage", event.Name, "reason", out.Reason)
			if onAbort != nil {
				onAbort(out.Reason)
			}
			return // abort stops dispatch for this event
		}
		slog.Info("wasm observer", "plugin", s.meta.Name, "stage", event.Name, "phase", event.Phase, "action", out.Action)
	}
}
