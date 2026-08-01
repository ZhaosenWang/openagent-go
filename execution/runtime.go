// Package execution implements the Execution Runtime: running tool calls
// with hooks, streaming, result policy, and built-in tools. It is consumed
// by kernel.Runtime, which owns approval orchestration and the tools slice.
//
// P1: synchronous Execute. P3: Job-ization (Start/Wait/Cancel) for
// long-running tools, retry, and background execution.
package execution

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	openagent "github.com/yusheng-g/openagent-go"
	ctxpkg "github.com/yusheng-g/openagent-go/context"
	"github.com/yusheng-g/openagent-go/provider/skill"
)

// BuiltinHandler runs a built-in tool (load_skill, reload_skills, recall).
type BuiltinHandler func(ctx context.Context, session openagent.Session, call openagent.ToolCall, ch chan<- openagent.StreamEvent) openagent.Message

// Config wires the Execution Runtime to its dependencies.
type Config struct {
	// ToolSnapshot returns the current registered tool set (kernel
	// injects rt.SnapshotTools).
	ToolSnapshot func() []openagent.Tool
	// SkillProvider resolves skills for load_skill/reload_skills.
	SkillProvider skill.Provider
	// MemoryProvider feeds the recall built-in (durable knowledge).
	MemoryProvider ctxpkg.MemoryProvider
	// Hooks/Observer receive tool lifecycle events.
	Hooks    openagent.RunHooks
	Observer openagent.RunObserver
	// ResultPolicy truncates oversized results (after hooks).
	ResultPolicy openagent.ResultPolicy
	// SessionFromContext is optional; nil uses openagent.SessionFromContext.
	SessionFromContext func(ctx context.Context) (openagent.Session, bool)
}

// ExecutionRuntime executes tool calls.
type ExecutionRuntime struct {
	cfg Config

	// loadedSkills/loadedSkillsMu: skill bodies cached by load_skill,
	// refreshed by reload_skills (shared with the kernel prompt builder).
	loadedSkills   map[string]string
	loadedSkillsMu sync.RWMutex
}

// New creates an ExecutionRuntime.
func New(cfg Config) *ExecutionRuntime {
	return &ExecutionRuntime{
		cfg:          cfg,
		loadedSkills: make(map[string]string),
	}
}

// Resolve resolves a tool name to its definition and tool instance.
// builtin=true for built-in tools (executed by Execute's builtin path).
type ResolvedCall struct {
	Def     openagent.FunctionDefinition
	Tool    openagent.Tool
	Builtin bool
}

// Resolve returns nil if the tool is neither built-in nor registered.
func (e *ExecutionRuntime) Resolve(name string) *ResolvedCall {
	switch name {
	case "load_skill", "reload_skills", "recall":
		return &ResolvedCall{Def: e.builtinDef(name), Builtin: true}
	}
	if e.cfg.ToolSnapshot == nil {
		return nil
	}
	for _, t := range e.cfg.ToolSnapshot() {
		if t.Definition().Name == name {
			d := t.Definition()
			return &ResolvedCall{Def: d, Tool: t}
		}
	}
	return nil
}

// Start launches a tool call as a background job (P3 Job model). The
// kernel starts all approved calls concurrently, then waits in call order.
func (e *ExecutionRuntime) Start(ctx context.Context, session openagent.Session, call openagent.ToolCall, ch chan<- openagent.StreamEvent) ExecutionHandle {
	return e.startJob(ctx, session, call, ch)
}

// execute is the synchronous single-call pipeline. Retryable tool errors
// (ToolResult.Error.Retryable) are retried with backoff up to 2 times.
func (e *ExecutionRuntime) execute(ctx context.Context, session openagent.Session, call openagent.ToolCall, ch chan<- openagent.StreamEvent) openagent.Message {
	const maxRetries = 2
	var msg openagent.Message
	for attempt := 0; attempt <= maxRetries; attempt++ {
		msg = e.executeOnce(ctx, session, call, ch)
		if !isRetryable(msg) {
			return msg
		}
		if attempt < maxRetries {
			select {
			case <-time.After(time.Duration(1<<uint(attempt)) * 500 * time.Millisecond):
			case <-ctx.Done():
				return msg
			}
		}
	}
	return msg
}

// executeOnce runs a single attempt of the call pipeline.
func (e *ExecutionRuntime) executeOnce(ctx context.Context, session openagent.Session, call openagent.ToolCall, ch chan<- openagent.StreamEvent) openagent.Message {
	rc := e.Resolve(call.Function.Name)
	if rc == nil {
		return openagent.Message{
			Role:       openagent.RoleTool,
			ToolCallID: call.ID,
			Content:    fmt.Sprintf("tool %q not found", call.Function.Name),
		}
	}

	args := json.RawMessage(call.Function.Arguments)
	toolCtx := withSession(ctx, session)

	// ── Built-in tools ──
	if rc.Builtin {
		if h := e.builtinHandler(call.Function.Name); h != nil {
			tc := e.fireToolHooks(toolCtx, rc.Def, args)
			msg := h(toolCtx, session, call, ch)
			result := &openagent.ToolResult{Content: msg.Content}
			e.fireToolHooksEnd(toolCtx, rc.Def, args, result, tc)
			msg.Content = result.Content
			msg.Result = result
			return msg
		}
		return openagent.Message{
			Role:       openagent.RoleTool,
			ToolCallID: call.ID,
			Content:    fmt.Sprintf("tool %q not found", call.Function.Name),
		}
	}

	var toolHookState any
	if e.cfg.Hooks != nil {
		var err error
		toolHookState, err = e.cfg.Hooks.OnToolStart(toolCtx, rc.Def, args)
		if err != nil {
			slog.Warn("openagent: OnToolStart hook failed", "tool", call.Function.Name, "error", err)
		}
	}

	teStart := time.Now()
	e.observe(toolCtx, openagent.StageToolExecute, "enter", map[string]any{"tool": call.Function.Name}, time.Time{}, nil)
	var result *openagent.ToolResult

	// ── Streaming path (optional interface) ──
	// Streaming tools run only when the parent run is itself streaming
	// (ch != nil): under a synchronous run their progress chunks would be
	// concatenated with the final output, duplicating content in the tool
	// result (e.g. the sub-agent streams deltas then the final answer).
	if se, ok := rc.Tool.(openagent.StreamExecutor); ok && ch != nil {
		toolCh := se.ExecuteStream(toolCtx, args)
		var buf strings.Builder
		// Rate-limit progress events (1/sec) to avoid flooding the channel.
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		var pending string
		flush := func() {
			if pending != "" && ch != nil {
				select {
				case ch <- openagent.StreamEvent{Type: openagent.StreamToolProgress, Text: pending, ToolCallID: call.ID}:
				case <-toolCtx.Done():
				}
				pending = ""
			}
		}
		done := false
		for !done {
			select {
			case chunk, ok := <-toolCh:
				if !ok {
					done = true
				} else if chunk.Error != nil {
					result = openagent.ErrorResult(chunk.Error, false, "")
					done = true
				} else {
					buf.WriteString(chunk.Content)
					pending += chunk.Content
				}
			case <-ticker.C:
				flush()
			case <-toolCtx.Done():
				flush()
				done = true
			}
		}
		flush()
		if result == nil {
			result = &openagent.ToolResult{Content: buf.String()}
		}
	} else {
		// ── Blocking path (default) ──
		result = rc.Tool.Execute(toolCtx, args)
	}

	e.observe(ctx, openagent.StageToolExecute, "leave", map[string]any{"tool": call.Function.Name}, teStart, result.AsError())

	if e.cfg.Hooks != nil {
		e.cfg.Hooks.OnToolEnd(toolCtx, rc.Def, args, result, toolHookState)
	}

	// Result policy (truncation) — after hooks so redaction happens first.
	if e.cfg.ResultPolicy != nil && result != nil {
		result = e.cfg.ResultPolicy.Apply(toolCtx, session, result)
	}

	return toolResultMessage(call, result)
}

// toolResultMessage assembles the RoleTool message from a structured
// result. Single error channel: failures live in result.Error and render
// as "error: <message>" for the model.
func toolResultMessage(call openagent.ToolCall, result *openagent.ToolResult) openagent.Message {
	content := ""
	if result != nil {
		if result.Error != nil {
			content = "error: " + result.Error.Message
		} else {
			content = result.Content
		}
	}
	return openagent.Message{
		Role:       openagent.RoleTool,
		ToolCallID: call.ID,
		Content:    content,
		Result:     result,
	}
}

// ── hooks/observer plumbing ──

type toolHookCtx struct {
	start     time.Time
	hookState any
}

func (e *ExecutionRuntime) fireToolHooks(ctx context.Context, def openagent.FunctionDefinition, args json.RawMessage) toolHookCtx {
	tc := toolHookCtx{start: time.Now()}
	if e.cfg.Hooks != nil {
		var err error
		tc.hookState, err = e.cfg.Hooks.OnToolStart(ctx, def, args)
		if err != nil {
			slog.Warn("openagent: OnToolStart hook failed", "tool", def.Name, "error", err)
		}
	}
	e.observe(ctx, openagent.StageToolExecute, "enter", map[string]any{"tool": def.Name}, time.Time{}, nil)
	return tc
}

func (e *ExecutionRuntime) fireToolHooksEnd(ctx context.Context, def openagent.FunctionDefinition, args json.RawMessage, result *openagent.ToolResult, tc toolHookCtx) {
	e.observe(ctx, openagent.StageToolExecute, "leave", map[string]any{"tool": def.Name}, tc.start, result.AsError())
	if e.cfg.Hooks != nil {
		e.cfg.Hooks.OnToolEnd(ctx, def, args, result, tc.hookState)
	}
}

func (e *ExecutionRuntime) observe(ctx context.Context, stage, phase string, detail map[string]any, start time.Time, err error) {
	if e.cfg.Observer == nil {
		return
	}
	e.cfg.Observer.ObserveStage(ctx, openagent.StageEvent{
		Name: stage, Phase: phase, Detail: detail, Duration: time.Since(start), Err: err,
	})
}

// withSession injects the session into the tool context so tools and
// hooks can retrieve it via SessionFromContext.
func withSession(ctx context.Context, session openagent.Session) context.Context {
	return openagent.WithSession(ctx, session)
}

// Runtime is the Execution Runtime interface — the kernel consumes this,
// not the concrete implementation, so applications can substitute their
// own execution layer (e.g. a remote executor).
type Runtime interface {
	Start(ctx context.Context, session openagent.Session, call openagent.ToolCall, ch chan<- openagent.StreamEvent) ExecutionHandle
	Resolve(name string) *ResolvedCall
}

// Compile-time assertion: the concrete runtime implements the interface.
var _ Runtime = (*ExecutionRuntime)(nil)
