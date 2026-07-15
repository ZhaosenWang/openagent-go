package openagent

import (
	"context"
	"encoding/json"
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
