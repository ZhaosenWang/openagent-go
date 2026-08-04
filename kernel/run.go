package kernel

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	openagent "github.com/yusheng-g/openagent-go"
	ctxpkg "github.com/yusheng-g/openagent-go/context"
	"github.com/yusheng-g/openagent-go/eventbus"
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
	// Snapshot the mutable config under the lock: SetModel/SetMaxTurns
	// (session config RPCs, wasm exports) can run concurrently from the
	// serve loop or tool callbacks. The current turn uses the snapshot;
	// the next turn picks up any change.
	rt.mu.RLock()
	maxTurns := rt.cfg.MaxTurns
	cfgModel := rt.cfg.Model
	rt.mu.RUnlock()
	if maxTurns <= 0 {
		maxTurns = 20 // agent.New's default; guard for zero-value configs
	}

	// Resolve model for this run (Model() reads it under the same lock).
	rt.mu.Lock()
	rt.runModel = cfgModel
	if session.Model != nil {
		rt.runModel = session.Model
	}
	rt.mu.Unlock()

	result := &openagent.RunResult{}
	rt.state.SessionID = session.ID

	// Guard.in BEFORE persisting the input: a blocked input must never
	// reach the store — it would be re-read into the model's context next
	// turn (same principle as the guard.out comment below).
	gStart := time.Now()
	rt.observe(ctx, openagent.StageGuardIn, "enter", nil, time.Time{}, nil)
	ok, msg := rt.guardInput(ctx, input)
	// The block reason rides in Err for blocked runs (observers read
	// Err.Message); allowed is structured so aggregators can count
	// pass/block without parsing error strings.
	rt.observe(ctx, openagent.StageGuardIn, "leave", map[string]any{"allowed": ok}, gStart, msg)
	if !ok {
		return nil, msg
	}

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
		pStart := time.Now()
		rt.observe(ctx, openagent.StagePromptBuild, "enter", nil, time.Time{}, nil)
		prompt, err := rt.buildPrompt(ctx, session, ac)
		// Composition counts (skills/memories/resources) let observers
		// attribute prompt growth to a context source without inspecting
		// the content.
		rt.observe(ctx, openagent.StagePromptBuild, "leave", map[string]any{
			"messages":  len(prompt),
			"tokens":    openagent.CountMessages(openagent.TokenizerModelID(rt.runModel), prompt),
			"skills":    len(ac.Skills),
			"memories":  len(ac.Memories),
			"resources": len(ac.Resources),
		}, pStart, err)
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

		// ④ Model call. The stage wraps the whole call — retries and
		// streaming included — so the observed duration is the real
		// model latency of the turn.
		mStart := time.Now()
		rt.observe(ctx, openagent.StageModelCall, "enter", map[string]any{"model": req.Model}, time.Time{}, nil)
		resp, retries, err := rt.callModel(ctx, req, ch)
		mDetail := map[string]any{"model": req.Model, "retries": retries}
		if resp != nil {
			mDetail["choices"] = len(resp.Choices)
			if len(resp.Choices) > 0 {
				if fr := resp.Choices[0].FinishReason; fr != "" {
					mDetail["finish_reason"] = fr
				}
				mDetail["tool_calls"] = len(resp.Choices[0].Message.ToolCalls)
				// The full model output is observation data — observers
				// are read-only (unlike hooks which mutate), so no
				// truncation here; consumers filter as they see fit.
				if content := resp.Choices[0].Message.Content; content != "" {
					mDetail["content_chars"] = len([]rune(content))
					mDetail["content"] = content
				}
				if rc := resp.Choices[0].Message.ReasoningContent; rc != "" {
					mDetail["reasoning_chars"] = len([]rune(rc))
				}
			}
			if u := resp.Usage; u.TotalTokens > 0 {
				mDetail["total_tokens"] = u.TotalTokens
				mDetail["prompt_tokens"] = u.PromptTokens
				mDetail["completion_tokens"] = u.CompletionTokens
				mDetail["cache_read_tokens"] = u.CacheReadTokens
			}
		}
		rt.observe(ctx, openagent.StageModelCall, "leave", mDetail, mStart, err)
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
		goStart := time.Now()
		rt.observe(ctx, openagent.StageGuardOut, "enter", map[string]any{"target": "model"}, time.Time{}, nil)
		blocked, reason, tripwire := rt.guardOutput(ctx, choice)
		// reason + tripwire are structured for security auditing: which
		// rule blocked (or tripped) the output.
		rt.observe(ctx, openagent.StageGuardOut, "leave", map[string]any{"target": "model", "blocked": blocked, "reason": reason, "tripwire": tripwire}, goStart, nil)
		if blocked {
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
			// Guard.out on each tool result (same per-item granularity as
			// tool.execute).
			goStart := time.Now()
			rt.observe(ctx, openagent.StageGuardOut, "enter", map[string]any{"target": "tool"}, time.Time{}, nil)
			blocked, reason, tripwire := rt.guardOutput(ctx, r)
			rt.observe(ctx, openagent.StageGuardOut, "leave", map[string]any{"target": "tool", "blocked": blocked, "reason": reason, "tripwire": tripwire}, goStart, nil)
			if blocked {
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

	// The turn loop can end while the run was cancelled during the LAST
	// turn's tool execution — the loop-top cancellation check (and its
	// compensation) never runs again. Surface the cancellation instead of
	// reporting a normal completion: StreamDone below would otherwise be
	// randomly dropped (cancelled ctx) and the client never sees a
	// terminal event.
	if ctx.Err() != nil {
		rt.cancelCompensation(ctx, session, workingMessages, ch)
		return nil, ctx.Err()
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
	// "enter" events pass a zero start (callers mark the start separately
	// and pass it to the matching "leave"). time.Since(zero) is ~1.7e9
	// years — a nonsense duration that observers must ignore today; make
	// it explicit zero instead so nothing has to special-case enter.
	d := time.Duration(0)
	if !start.IsZero() {
		d = time.Since(start)
	}
	rt.deps.Observer.ObserveStage(ctx, openagent.StageEvent{
		Name:     stage,
		Phase:    phase,
		Detail:   detail,
		Duration: d,
		Err:      err,
	})
}

// commit appends a message to memory (Transient messages and nil memory skip).
func (rt *Runtime) commit(ctx context.Context, session openagent.Session, msg openagent.Message) {
	if msg.Transient || rt.deps.SessionStore == nil {
		return
	}
	// A cancelled run still persists what it already produced. The loop
	// commits a message only after it EXISTS (the model call returned, the
	// tool job finished with a real result — executeTools waits for jobs
	// to complete even when cancelled). A cancelled ctx would fail the
	// backend's transaction immediately and the message would be lost:
	// the assistant tool_calls message lands in the store but its result
	// does not, and cancelCompensation skips it (it sees the result in the
	// in-memory working set) — an orphan tool_call in history, re-read by
	// the model next turn. Same deliberate background-ctx pattern as
	// cancelCompensation. The write is a bounded local operation.
	if ctx.Err() != nil {
		ctx = context.Background()
	}
	start := time.Now()
	rt.observe(ctx, openagent.StageMemoryAppend, "enter", nil, time.Time{}, nil)
	err := rt.deps.SessionStore.Append(ctx, session.ID, msg)
	// Metadata only (role + size), never the content: the audit trail
	// must show WHAT was written without duplicating the payload.
	rt.observe(ctx, openagent.StageMemoryAppend, "leave", map[string]any{
		"role":  string(msg.Role),
		"chars": len([]rune(msg.Content)),
	}, start, err)
	if err != nil {
		slog.Error("openagent: memory append failed", "error", err)
	}
}
