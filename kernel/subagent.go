package kernel

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"

	openagent "github.com/yusheng-g/openagent-go"
	"github.com/yusheng-g/openagent-go/agent"
)

// globalAgentSeq generates unique sub-agent session ids across runtimes.
var globalAgentSeq atomic.Int64

// runChild runs a nested Runtime as a sub-agent: fresh session id under
// the parent's user/context scope, capped turns, no nested delegation.
// deps must be pre-built by the caller (tools resolved; MemoryProvider,
// policy, and approver inherited from the parent — v2.0 §22). emit
// receives child stream events for UI progress (nil = synchronous run).
// The returned string is the child's final answer.
func runChild(ctx context.Context, cfg *agent.Agent, deps Deps, session openagent.Session, task string, emit func(openagent.StreamEvent)) (string, error) {
	child := session
	child.ID = fmt.Sprintf("%s-%d", cfg.Name, globalAgentSeq.Add(1))
	sub := New(cfg, deps)

	if emit == nil {
		res, err := sub.Run(ctx, child, openagent.UserMessage(task))
		if err != nil {
			return "", err
		}
		return res.FinalOutput, nil
	}

	var output strings.Builder
	subCh := sub.RunStream(ctx, child, openagent.UserMessage(task))
	for {
		select {
		case ev, ok := <-subCh:
			if !ok {
				// Channel closed without StreamDone: the run ended
				// abnormally — don't hand back a partial transcript as
				// success.
				return output.String(), fmt.Errorf("sub-agent %s: stream ended without a result", cfg.Name)
			}
			switch ev.Type {
			case openagent.StreamThought, openagent.StreamTextDelta:
				output.WriteString(ev.Text)
				emit(ev)
			case openagent.StreamToolResult:
				output.WriteString(ev.Message.Content)
				emit(ev)
			case openagent.StreamError:
				if ev.Error != nil {
					return output.String(), ev.Error
				}
			case openagent.StreamAborted:
				if ev.Error != nil {
					return output.String(), ev.Error
				}
				return output.String(), ctx.Err()
			case openagent.StreamDone:
				if ev.Result != nil {
					return ev.Result.FinalOutput, nil
				}
				return output.String(), nil
			}
		case <-ctx.Done():
			// Cancellation is an error, not a successful partial answer.
			return output.String(), ctx.Err()
		}
	}
}
