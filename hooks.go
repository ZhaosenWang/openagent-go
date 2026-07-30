package openagent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
)

// RunHooks provides lifecycle callbacks in the Runner mainline.
// Naming follows OpenAI Agents SDK RunHooks conventions.
// nil RunHooks = no callbacks.
//
// OnAgentStart and OnToolStart return an opaque value that the Runner
// hands back to the corresponding End method. Implementations use this
// to carry state from start to finish: an OTEL span, a start timestamp,
// a WASM guest handle — the Runner never inspects it.
//
// OnToolEnd receives result and err as pointers so that hooks can
// mutate them (redaction, truncation, metadata injection) before the
// result is stored in memory.
type RunHooks interface {
	// OnAgentStart is called once when agent.Run() begins, before the loop.
	OnAgentStart(ctx context.Context, req ChatCompletionRequest) (any, error)
	// OnAgentEnd is called once when agent.Run() finishes (success, error, or cancel).
	OnAgentEnd(ctx context.Context, req ChatCompletionRequest, resp *ChatCompletionResponse, runErr error, startState any)
	// OnToolStart is called before each Tool.Execute.
	OnToolStart(ctx context.Context, tool FunctionDefinition, args json.RawMessage) (any, error)
	// OnToolEnd is called after each Tool.Execute finishes.
	// result and err are pointers — hooks may mutate them before memory storage.
	OnToolEnd(ctx context.Context, tool FunctionDefinition, args json.RawMessage, result *string, err *error, startState any)
}

// MultiHooks combines multiple RunHooks into one. Each hook is called in
// order; one hook returning an error does not prevent subsequent hooks
// from running. Nil hooks are skipped.
//
// Start/End state pairing: OnAgentStart returns a []any, one entry per
// hook. OnAgentEnd receives the same slice and distributes each entry
// back to its hook. Same for OnToolStart/OnToolEnd.
func MultiHooks(hooks ...RunHooks) RunHooks {
	var filtered []RunHooks
	for _, h := range hooks {
		if h != nil {
			filtered = append(filtered, h)
		}
	}
	switch len(filtered) {
	case 0:
		return nil
	case 1:
		return filtered[0]
	default:
		return &multiHooks{list: filtered}
	}
}

type multiHooks struct {
	list []RunHooks
}

func (m *multiHooks) OnAgentStart(ctx context.Context, req ChatCompletionRequest) (any, error) {
	states := make([]any, len(m.list))
	var firstErr error
	for i, h := range m.list {
		s, err := h.OnAgentStart(ctx, req)
		states[i] = s
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return states, firstErr
}

func (m *multiHooks) OnAgentEnd(ctx context.Context, req ChatCompletionRequest, resp *ChatCompletionResponse, runErr error, startState any) {
	states, ok := startState.([]any)
	if !ok || len(states) != len(m.list) {
		// State shape mismatch: the start value was not produced by
		// this multiHooks instance (e.g. a single hook was set via
		// WithRunHook and then wrapped, or a caller passed a foreign
		// startState). Distribute nil to every hook so none receives a
		// wrong sibling's state, and surface the mismatch loudly rather
		// than silently dropping per-hook state.
		slog.Warn("openagent: MultiHooks.OnAgentEnd startState shape mismatch",
			"got", fmt.Sprintf("%T len=%d", startState, 0),
			"want", fmt.Sprintf("[]any len=%d", len(m.list)),
		)
		states = nil
	}
	for i, h := range m.list {
		var s any
		if i < len(states) {
			s = states[i]
		}
		h.OnAgentEnd(ctx, req, resp, runErr, s)
	}
}

func (m *multiHooks) OnToolStart(ctx context.Context, tool FunctionDefinition, args json.RawMessage) (any, error) {
	states := make([]any, len(m.list))
	var firstErr error
	for i, h := range m.list {
		s, err := h.OnToolStart(ctx, tool, args)
		states[i] = s
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return states, firstErr
}

func (m *multiHooks) OnToolEnd(ctx context.Context, tool FunctionDefinition, args json.RawMessage, result *string, err *error, startState any) {
	states, ok := startState.([]any)
	if !ok || len(states) != len(m.list) {
		// Same rationale as OnAgentEnd: never hand a hook a sibling's or
		// foreign state — distribute nil and warn. Silently zero-filling
		// (the old behavior) would mask a misconfigured hook pipeline
		// (e.g. nested MultiHooks) where every hook quietly loses state.
		slog.Warn("openagent: MultiHooks.OnToolEnd startState shape mismatch",
			"tool", tool.Name,
			"got", fmt.Sprintf("%T", startState),
			"want", fmt.Sprintf("[]any len=%d", len(m.list)),
		)
		states = nil
	}
	for i, h := range m.list {
		var s any
		if i < len(states) {
			s = states[i]
		}
		h.OnToolEnd(ctx, tool, args, result, err, s)
	}
}
