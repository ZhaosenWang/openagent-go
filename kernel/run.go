package kernel

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	openagent "github.com/yusheng-g/openagent-go"
	ctxpkg "github.com/yusheng-g/openagent-go/context"
	"github.com/yusheng-g/openagent-go/eventbus"
	"github.com/yusheng-g/openagent-go/execution"
	"github.com/yusheng-g/openagent-go/governance"
)

// run is the 8-node mainline loop. It orchestrates; each node is a method
// so stages can be unit-tested and extended independently.
//
//	① Memory fetch (context Build: compaction + working set)
//	② Prompt build
//	③ Guard.in
//	④ Model call (streaming with retry)
//	⑤ Guard.out
//	⑥ Policy/Approval + ⑦ Tool execution (concurrent)
//	⑧ Memory store (Commit)
func (rt *Runtime) run(ctx context.Context, session openagent.Session, prefix []openagent.Message, input openagent.Message, ch chan<- openagent.StreamEvent) (_ *openagent.RunResult, runErr error) {
	maxTurns := rt.cfg.MaxTurns
	if maxTurns <= 0 {
		maxTurns = 20 // agent.New's default; guard for zero-value configs
	}

	// Resolve model for this run.
	rt.runModel = rt.cfg.Model
	if session.Model != nil {
		rt.runModel = session.Model
	}

	// Skill tools (load_skill/reload_skills) mount when a provider exists;
	// the catalog itself is matched per-goal by the context runtime.
	if rt.deps.SkillProvider != nil {
		rt.builtinTools = execution.BuiltinSkillToolDefs()
	}
	if rt.deps.MemoryProvider != nil {
		rt.builtinTools = append(rt.builtinTools, execution.BuiltinRecallDef())
	}

	result := &openagent.RunResult{}
	rt.state.SessionID = session.ID

	// Append initial user input to memory.
	rt.commit(ctx, session, input)
	rt.logEvent(ctx, session.ID, eventbus.EventUserInput, input.Content, nil)

	// Track last request/response for RunHooks.OnAgentEnd.
	var lastReq openagent.ChatCompletionRequest
	var lastResp *openagent.ChatCompletionResponse
	var agentHookState any

	// ── RunHooks.OnAgentStart ──
	if rt.deps.Hooks != nil {
		var err error
		agentHookState, err = rt.deps.Hooks.OnAgentStart(ctx, lastReq)
		if err != nil {
			// Hook infrastructure failure must not kill the run, but must
			// not stay silent either.
			slog.Warn("openagent: OnAgentStart hook failed", "error", err)
		}
	}
	defer func() {
		if rt.deps.Hooks != nil {
			rt.deps.Hooks.OnAgentEnd(ctx, lastReq, lastResp, runErr, agentHookState)
		}
	}()

	var workingMessages []openagent.Message
	var ac *ctxpkg.AgentContext

	// ── Main loop ──
	for turn := 0; turn < maxTurns; turn++ {
		result.TurnCount = turn + 1
		// Cancel compensation: persist unresolved tool results.
		if ctx.Err() != nil {
			rt.cancelCompensation(ctx, session, workingMessages, ch)
			return nil, ctx.Err()
		}

		if turn == 0 {
			// ① Memory fetch — compaction + working set (turn 1 only).
			messages, ci, err := rt.prepareMemory(ctx, session)
			if err != nil {
				runErr = err
				return nil, err
			}
			workingMessages = append(workingMessages, messages...)
			// Strip the just-appended input so history is history.
			workingMessages = ctxpkg.ExcludeInput(workingMessages, input)
			// Orphan cleanup: leading assistant tool_calls without results.
			workingMessages = ctxpkg.TrimOrphanToolCalls(workingMessages)
			rt.compressed = ci.compressed
			if ci.err != nil {
				slog.Error("openagent: compaction failed", "error", ci.err)
			}
			// ③ Guard.in — once per run.
			if ok, msg := rt.guardInput(ctx, input); !ok {
				return nil, msg
			}
			// ② Prompt build (turn 1: prefix + input).
			promptMsgs := append(append([]openagent.Message{}, prefix...), input)
			workingMessages = append(workingMessages, promptMsgs...)
			// ① Context Build: assemble the AgentContext (knowledge recall,
			// skill match, resources) — the single input to prompt assembly.
			ac, err = rt.context.Build(ctx, ctxpkg.BuildRequest{
				Session:    session,
				Goal:       input.Content,
				WorkingSet: workingMessages,
			})
			if err != nil {
				return nil, fmt.Errorf("context build: %w", err)
			}
		}

		// Keep the AgentContext in sync with the growing working set.
		ac.Messages = workingMessages

		// ② Prompt build — consumes the AgentContext (v2.0: Context is the
		// agent input; the kernel never assembles prompt fragments itself).
		prompt, err := rt.buildPrompt(ctx, session, ac)
		if err != nil {
			slog.Error("openagent: prompt build failed", "error", err)
			chSend(ctx, ch, openagent.StreamEvent{Type: openagent.StreamError, Error: err})
			return nil, err
		}
		// Hard limit check: a prompt that exceeds the model's context window
		// is a configuration problem, not something to paper over by
		// silently dropping messages (dropped messages stay in the store,
		// get re-read next turn, and the summary references history the
		// model never saw). Fail loudly instead — compaction (working-set
		// budget) and MaxCompressedTokens (summary cap) are the correct
		// controls.
		if rt.runModel != nil && rt.runModel.ContextWindow() > 0 {
			modelID := openagent.TokenizerModelID(rt.runModel)
			if n := openagent.CountMessages(modelID, prompt); n > rt.runModel.ContextWindow() {
				err := fmt.Errorf("prompt exceeds model context window: %d > %d tokens (increase MaxWorkingTokens or reduce system prompts / compressed summary)", n, rt.runModel.ContextWindow())
				chSend(ctx, ch, openagent.StreamEvent{Type: openagent.StreamError, Error: err})
				return nil, err
			}
		}
		req := rt.buildModelRequest(session, prompt)
		lastReq = req

		// ④ Model call.
		resp, err := rt.callModel(ctx, req, ch)
		if err != nil {
			if ctx.Err() != nil {
				chSend(ctx, ch, openagent.StreamEvent{Type: openagent.StreamAborted, Error: ctx.Err()})
				return nil, ctx.Err()
			}
			chSend(ctx, ch, openagent.StreamEvent{Type: openagent.StreamError, Error: err})
			return nil, err
		}
		if len(resp.Choices) == 0 {
			err := fmt.Errorf("model returned no choices")
			chSend(ctx, ch, openagent.StreamEvent{Type: openagent.StreamError, Error: err})
			return nil, err
		}
		lastResp = resp
		result.Usage = resp.Usage

		choice := resp.Choices[0].Message
		finishReason := resp.Choices[0].FinishReason

		// ⑤ Guard.out — on model output. Applied BEFORE the message is
		// persisted, streamed, or added to the working set: a blocked
		// content must never reach the store (it would be re-read into the
		// model's context next turn), the result, or the tool-call event.
		// (Live-streamed text deltas are already on the wire and cannot be
		// recalled — the guard governs what is stored and reused.)
		if blocked, reason, tripwire := rt.guardOutput(ctx, choice); blocked {
			if tripwire {
				return nil, fmt.Errorf("output guard tripwire: %s", reason)
			}
			choice.Content = "[blocked: " + reason + "]"
		}
		result.FinalOutput = choice.Content

		for _, tc := range choice.ToolCalls {
			chSend(ctx, ch, openagent.StreamEvent{Type: openagent.StreamToolCall, Message: openagent.Message{Role: openagent.RoleAssistant, Content: choice.Content, ToolCalls: []openagent.ToolCall{tc}}})
		}
		result.Messages = append(result.Messages, choice)
		workingMessages = append(workingMessages, choice)
		rt.commit(ctx, session, choice)
		rt.logEvent(ctx, session.ID, eventbus.EventAssistantMessage, choice.Content, nil)

		// No tool calls: stop.
		if len(choice.ToolCalls) == 0 {
			if finishReason != "" && finishReason != "stop" {
				result.StopReason = finishReason
				msg := openagent.Message{Role: openagent.RoleSystem, Content: "Model stopped with reason: " + finishReason}
				workingMessages = append(workingMessages, msg)
				rt.commit(ctx, session, msg)
			}
			break
		}

		// ⑥⑦ Tool execution (approval + concurrent execution).
		results := rt.executeTools(ctx, session, choice.ToolCalls, ch)
		for _, r := range results {
			if blocked, reason, tripwire := rt.guardOutput(ctx, r); blocked {
				if tripwire {
					return nil, fmt.Errorf("output guard tripwire on tool result: %s", reason)
				}
				r.Content = "[blocked: " + reason + "]"
			}
			result.Messages = append(result.Messages, r)
			workingMessages = append(workingMessages, r)
			rt.commit(ctx, session, r)
			chSend(ctx, ch, openagent.StreamEvent{Type: openagent.StreamToolResult, Message: r})
		}

		// Handoff: an executed tool with EndTurn terminates the turn.
		if rt.checkHandoff(choice.ToolCalls, results) {
			result.StopReason = "handoff"
			break
		}
	}

	result.ContextWindow = rt.runModel.ContextWindow()
	rt.state.Turn = result.TurnCount
	// Self-evolution: store durable knowledge from this finished run.
	// Knowledge is user-level (cross-session long-term memory) — the
	// session ID is NOT part of the scope, or every new session would be
	// filtered away from the knowledge it should recall.
	//
	// The call is fire-and-forget: AsyncExtractor (the standard wiring)
	// enqueues and extracts on its background worker, so this never
	// delays the run's return. Applications wire Deps.Extractor once per
	// server (never per run).
	if rt.deps.Extractor != nil && len(workingMessages) > 0 {
		rt.deps.Extractor.Extract(ctx, ctxpkg.ContextScope{
			UserID: session.UserID,
		}, workingMessages)
	}
	chSend(ctx, ch, openagent.StreamEvent{Type: openagent.StreamDone, Result: result})
	return result, nil
}

// logEvent records an audit event (no-op without a logger).
func (rt *Runtime) logEvent(ctx context.Context, sessionID string, typ eventbus.EventType, payload any, meta map[string]string) {
	if rt.deps.EventLogger == nil {
		return
	}
	rt.deps.EventLogger.Append(ctx, eventbus.Event{
		SessionID: sessionID,
		Type:      typ,
		Payload:   payload,
		Metadata:  meta,
	})
}

// chSend is a cancellable blocking event send (bounded backpressure):
// a slow consumer backpressures the run instead of silently dropping
// events. The run context cancels on client disconnect (the REST layer
// derives it from the request context), so a dead consumer cannot
// deadlock the producer — the send aborts on ctx.Done().
func chSend(ctx context.Context, ch chan<- openagent.StreamEvent, ev openagent.StreamEvent) {
	if ch == nil {
		return
	}
	select {
	case ch <- ev:
	case <-ctx.Done():
	}
}

// guardInput runs the input guard once per run. Returns (ok, error message).
func (rt *Runtime) guardInput(ctx context.Context, input openagent.Message) (bool, error) {
	if rt.cfg.InGuard == nil {
		return true, nil
	}
	res := rt.cfg.InGuard.Check(ctx, governance.GuardInput{Input: input})
	if !res.Allowed {
		return false, fmt.Errorf("input guard blocked: %s", res.Reason)
	}
	if res.Tripwire {
		return false, fmt.Errorf("input guard tripwire: %s", res.Reason)
	}
	return true, nil
}

// guardOutput runs the output guard on a model output or tool result.
// Returns (blocked, reason, tripwire).
func (rt *Runtime) guardOutput(ctx context.Context, msg openagent.Message) (bool, string, bool) {
	if rt.cfg.OutGuard == nil {
		return false, "", false
	}
	res := rt.cfg.OutGuard.Check(ctx, governance.GuardOutput{Output: msg})
	if !res.Allowed {
		return true, res.Reason, res.Tripwire
	}
	if res.Tripwire {
		return true, res.Reason, true
	}
	return false, "", false
}

// checkHandoff reports whether an executed tool carried EndTurn.
func (rt *Runtime) checkHandoff(calls []openagent.ToolCall, results []openagent.Message) bool {
	for i := range calls {
		if i < len(results) {
			if d := rt.toolDef(calls[i].Function.Name); d != nil && d.EndTurn {
				return true
			}
		}
	}
	return false
}

// observe emits a stage event to the observer (no-op if none).
func (rt *Runtime) observe(ctx context.Context, stage string, phase string, detail map[string]any, start time.Time, err error) {
	if rt.deps.Observer == nil {
		return
	}
	rt.deps.Observer.ObserveStage(ctx, openagent.StageEvent{
		Name:     stage,
		Phase:    phase,
		Detail:   detail,
		Duration: time.Since(start),
		Err:      err,
	})
}

// commit appends a message to memory (Transient messages and nil memory skip).
func (rt *Runtime) commit(ctx context.Context, session openagent.Session, msg openagent.Message) {
	if msg.Transient || rt.deps.SessionStore == nil {
		return
	}
	start := time.Now()
	rt.observe(ctx, openagent.StageMemoryAppend, "enter", nil, time.Time{}, nil)
	err := rt.deps.SessionStore.Append(ctx, session.ID, msg)
	rt.observe(ctx, openagent.StageMemoryAppend, "leave", nil, start, err)
	if err != nil {
		slog.Error("openagent: memory append failed", "error", err)
	}
}
