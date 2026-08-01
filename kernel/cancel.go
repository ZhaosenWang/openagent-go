package kernel

import (
	"context"
	"log/slog"
	"time"

	openagent "github.com/yusheng-g/openagent-go"
)

// cancelCompensation persists unresolved tool results when the run is
// cancelled mid-execution, so a restarted session sees what was in flight.
// It uses context.Background() deliberately: the run context is cancelled
// and the compensation write must still complete.
func (rt *Runtime) cancelCompensation(ctx context.Context, session openagent.Session, workingMessages []openagent.Message, ch chan<- openagent.StreamEvent) {
	if rt.deps.SessionStore == nil {
		chSend(ctx, ch, openagent.StreamEvent{Type: openagent.StreamAborted, Error: ctx.Err()})
		return
	}
	// Find assistant tool_calls in the working set whose results are missing
	// (RoleTool with a matching ToolCallID) — those were interrupted.
	covered := make(map[string]bool)
	for _, m := range workingMessages {
		if m.Role == openagent.RoleTool {
			covered[m.ToolCallID] = true
		}
	}
	for _, m := range workingMessages {
		if m.Role != openagent.RoleAssistant {
			continue
		}
		for _, tc := range m.ToolCalls {
			if covered[tc.ID] {
				continue
			}
			msg := openagent.Message{
				Role:       openagent.RoleTool,
				ToolCallID: tc.ID,
				Content:    "cancelled by user",
			}
			start := time.Now()
			rt.observe(ctx, openagent.StageMemoryAppend, "enter", nil, time.Time{}, nil)
			err := rt.deps.SessionStore.Append(context.Background(), session.ID, msg)
			rt.observe(ctx, openagent.StageMemoryAppend, "leave", nil, start, err)
			if err != nil {
				slog.Error("openagent: cancel compensation append failed", "error", err)
			}
			chSend(ctx, ch, openagent.StreamEvent{Type: openagent.StreamToolResult, Message: msg})
			covered[tc.ID] = true
		}
	}
	chSend(ctx, ch, openagent.StreamEvent{Type: openagent.StreamAborted, Error: ctx.Err()})
}
