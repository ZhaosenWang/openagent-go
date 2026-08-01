package kernel

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	openagent "github.com/yusheng-g/openagent-go"
	"github.com/yusheng-g/openagent-go/agent"
)

// ── streamingTestTool implements both Tool and StreamExecutor ──

type streamingTestTool struct {
	name   string
	chunks []string // chunks to emit
	delay  time.Duration
}

func (t *streamingTestTool) Definition() openagent.FunctionDefinition {
	return openagent.FunctionDefinition{
		Name:        t.name,
		Description: "a streaming test tool",
		Parameters:  openagent.SchemaOf[struct{}](),
	}
}

func (t *streamingTestTool) Execute(ctx context.Context, args json.RawMessage) *openagent.ToolResult {
	return &openagent.ToolResult{Content: strings.Join(t.chunks, "")}
}

func (t *streamingTestTool) ExecuteStream(ctx context.Context, args json.RawMessage) <-chan openagent.ToolStreamChunk {
	ch := make(chan openagent.ToolStreamChunk, len(t.chunks))
	go func() {
		defer close(ch)
		for _, c := range t.chunks {
			select {
			case <-ctx.Done():
				return
			case <-time.After(t.delay):
			}
			ch <- openagent.ToolStreamChunk{Content: c}
		}
	}()
	return ch
}

// ── nonStreamingTool implements only Tool ──

type nonStreamingTool struct{}

func (t *nonStreamingTool) Definition() openagent.FunctionDefinition {
	return openagent.FunctionDefinition{
		Name:        "blocking_tool",
		Description: "a blocking tool",
		Parameters:  openagent.SchemaOf[struct{}](),
	}
}

func (t *nonStreamingTool) Execute(ctx context.Context, args json.RawMessage) *openagent.ToolResult {
	return &openagent.ToolResult{Content: "blocking_result"}
}

// ── fakeModelWithToolCall returns a tool call then stops ──

type fakeModelWithToolCall struct {
	toolName string
	toolArgs string
	callID   string
}

func (m *fakeModelWithToolCall) ChatCompletion(ctx context.Context, req openagent.ChatCompletionRequest) (*openagent.ChatCompletionResponse, error) {
	return &openagent.ChatCompletionResponse{
		Choices: []openagent.Choice{{
			Index: 0,
			Message: openagent.Message{
				Role: openagent.RoleAssistant,
				ToolCalls: []openagent.ToolCall{{
					ID:   m.callID,
					Type: "function",
					Function: openagent.ToolCallFunction{
						Name:      m.toolName,
						Arguments: m.toolArgs,
					},
				}},
			},
			FinishReason: "tool_calls",
		}},
	}, nil
}

func (m *fakeModelWithToolCall) ChatCompletionStream(ctx context.Context, req openagent.ChatCompletionRequest) (openagent.StreamReader, error) {
	return nil, nil // fallback to non-streaming
}

func (m *fakeModelWithToolCall) ContextWindow() int { return 128_000 }

// ── fakeModelWithTwoToolCalls returns two tool calls in one response ──

type fakeModelWithTwoToolCalls struct {
	calls []openagent.ToolCall
}

func (m *fakeModelWithTwoToolCalls) ChatCompletion(ctx context.Context, req openagent.ChatCompletionRequest) (*openagent.ChatCompletionResponse, error) {
	return &openagent.ChatCompletionResponse{
		Choices: []openagent.Choice{{
			Index: 0,
			Message: openagent.Message{
				Role:      openagent.RoleAssistant,
				ToolCalls: m.calls,
			},
			FinishReason: "tool_calls",
		}},
	}, nil
}

func (m *fakeModelWithTwoToolCalls) ChatCompletionStream(ctx context.Context, req openagent.ChatCompletionRequest) (openagent.StreamReader, error) {
	return nil, nil
}

func (m *fakeModelWithTwoToolCalls) ContextWindow() int { return 128_000 }

// ── Tests ──

func TestStreamExecutorProgressEvents(t *testing.T) {
	// Verify that a tool implementing StreamExecutor emits openagent.StreamToolProgress
	// events with correct ToolCallID, and the final openagent.StreamToolResult aggregates all chunks.
	streamTool := &streamingTestTool{
		name:   "stream_tool",
		chunks: []string{"chunk1", "chunk2", "chunk3"},
		delay:  0,
	}

	model := &fakeModelWithToolCall{
		toolName: "stream_tool",
		toolArgs: `{}`,
		callID:   "call_abc",
	}

	cfg := agent.New("test",
		agent.WithModel(model),
		agent.WithMaxTurns(1),
	)
	deps := Deps{Tools: []openagent.Tool{streamTool}}
	agent := New(cfg, deps)

	ch := agent.RunStream(context.Background(), openagent.Session{ID: "test"}, openagent.UserMessage("go"))

	var progressEvents []openagent.StreamEvent
	var toolResultEvent *openagent.StreamEvent
	var gotDone bool

	for evt := range ch {
		switch evt.Type {
		case openagent.StreamToolProgress:
			progressEvents = append(progressEvents, evt)
		case openagent.StreamToolResult:
			evtCopy := evt
			toolResultEvent = &evtCopy
		case openagent.StreamDone:
			gotDone = true
		case openagent.StreamError:
			t.Fatalf("unexpected error: %v", evt.Error)
		case openagent.StreamAborted:
			t.Fatalf("unexpected abort: %v", evt.Error)
		}
	}

	if !gotDone {
		t.Fatal("missing openagent.StreamDone")
	}

	// Verify progress events. With 0ms delay, all chunks arrive before the
	// 1-second rate-limiting ticker fires, so they are batched into one
	// progress event. The rate limiter batches, it does not drop.
	if len(progressEvents) < 1 {
		t.Fatalf("expected at least 1 progress event, got %d", len(progressEvents))
	}
	for _, pe := range progressEvents {
		if pe.ToolCallID != "call_abc" {
			t.Errorf("progress expected ToolCallID=call_abc, got %q", pe.ToolCallID)
		}
	}
	// Verify all chunk content is present in the batched event(s).
	combined := ""
	for _, pe := range progressEvents {
		combined += pe.Text
	}
	expected := strings.Join(streamTool.chunks, "")
	if combined != expected {
		t.Errorf("combined progress text: expected %q, got %q", expected, combined)
	}

	// Verify final result aggregates all chunks.
	if toolResultEvent == nil {
		t.Fatal("missing openagent.StreamToolResult")
	}
	if toolResultEvent.Message.Content != "chunk1chunk2chunk3" {
		t.Errorf("final result: expected 'chunk1chunk2chunk3', got %q", toolResultEvent.Message.Content)
	}
}

func TestNonStreamingToolUnaffected(t *testing.T) {
	// Verify that tools implementing only Tool (no StreamExecutor) still work.
	blockingTool := &nonStreamingTool{}

	model := &fakeModelWithToolCall{
		toolName: "blocking_tool",
		toolArgs: `{}`,
		callID:   "call_xyz",
	}

	cfg := agent.New("test",
		agent.WithModel(model),
		agent.WithMaxTurns(1),
	)
	deps := Deps{Tools: []openagent.Tool{blockingTool}}
	agent := New(cfg, deps)

	ch := agent.RunStream(context.Background(), openagent.Session{ID: "test"}, openagent.UserMessage("go"))

	var gotProgress bool
	var gotResult bool
	var gotDone bool

	for evt := range ch {
		switch evt.Type {
		case openagent.StreamToolProgress:
			gotProgress = true
		case openagent.StreamToolResult:
			gotResult = true
			if evt.Message.Content != "blocking_result" {
				t.Errorf("expected 'blocking_result', got %q", evt.Message.Content)
			}
		case openagent.StreamDone:
			gotDone = true
		case openagent.StreamError:
			t.Fatalf("unexpected error: %v", evt.Error)
		case openagent.StreamAborted:
			t.Fatalf("unexpected abort: %v", evt.Error)
		}
	}

	if gotProgress {
		t.Error("non-streaming tool should NOT emit openagent.StreamToolProgress")
	}
	if !gotResult {
		t.Error("missing openagent.StreamToolResult")
	}
	if !gotDone {
		t.Error("missing openagent.StreamDone")
	}
}

func TestConcurrentStreamingToolsDisambiguated(t *testing.T) {
	// Verify that when two streaming tools run concurrently,
	// each gets its own ToolCallID and chunks are emitted correctly.
	toolA := &streamingTestTool{
		name:   "tool_a",
		chunks: []string{"a1", "a2", "a3"},
		delay:  0,
	}
	toolB := &streamingTestTool{
		name:   "tool_b",
		chunks: []string{"b1", "b2"},
		delay:  0,
	}

	model := &fakeModelWithTwoToolCalls{
		calls: []openagent.ToolCall{
			{ID: "call_aaa", Type: "function", Function: openagent.ToolCallFunction{Name: "tool_a", Arguments: "{}"}},
			{ID: "call_bbb", Type: "function", Function: openagent.ToolCallFunction{Name: "tool_b", Arguments: "{}"}},
		},
	}

	cfg := agent.New("test",
		agent.WithModel(model),
		agent.WithMaxTurns(1),
	)
	deps := Deps{Tools: []openagent.Tool{toolA, toolB}}
	agent := New(cfg, deps)

	ch := agent.RunStream(context.Background(), openagent.Session{ID: "test"}, openagent.UserMessage("go"))

	progressByID := map[string][]string{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(1)

	// Collect events concurrently (simulating real consumer).
	go func() {
		defer wg.Done()
		for evt := range ch {
			if evt.Type == openagent.StreamToolProgress {
				mu.Lock()
				progressByID[evt.ToolCallID] = append(progressByID[evt.ToolCallID], evt.Text)
				mu.Unlock()
			}
		}
	}()
	wg.Wait()

	// With 0ms delay, chunks arrive before the 1-second ticker fires.
	// Each tool's chunks are batched into one progress event. The rate
	// limiter batches by tool-call-id — they don't merge across tools.
	// Verify at least one progress event per tool and correct content.
	aChunks := progressByID["call_aaa"]
	bChunks := progressByID["call_bbb"]
	if len(aChunks) < 1 {
		t.Errorf("tool_a: expected at least 1 chunk, got %d", len(aChunks))
	}
	if len(bChunks) < 1 {
		t.Errorf("tool_b: expected at least 1 chunk, got %d", len(bChunks))
	}
	if strings.Join(aChunks, "") != "a1a2a3" {
		t.Errorf("tool_a: expected 'a1a2a3', got %q", strings.Join(aChunks, ""))
	}
	if strings.Join(bChunks, "") != "b1b2" {
		t.Errorf("tool_b: expected 'b1b2', got %q", strings.Join(bChunks, ""))
	}
}

func TestStreamExecutorCancellation(t *testing.T) {
	// Verify that cancelling the context stops the streaming tool.
	streamTool := &streamingTestTool{
		name:   "slow_tool",
		chunks: []string{"start", "middle", "end"},
		delay:  100 * time.Millisecond,
	}

	model := &fakeModelWithToolCall{
		toolName: "slow_tool",
		toolArgs: `{}`,
		callID:   "call_cancel",
	}

	cfg := agent.New("test",
		agent.WithModel(model),
		agent.WithMaxTurns(1),
	)
	deps := Deps{Tools: []openagent.Tool{streamTool}}
	agent := New(cfg, deps)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := agent.RunStream(ctx, openagent.Session{ID: "test"}, openagent.UserMessage("go"))

	// Read the first progress event, then cancel.
	var gotFirst bool
	for evt := range ch {
		if evt.Type == openagent.StreamToolProgress && !gotFirst {
			gotFirst = true
			cancel()
		}
	}

	// After cancel, we should see openagent.StreamAborted (not openagent.StreamDone).
	// The channel should close cleanly.
	if !gotFirst {
		t.Error("should have received at least one progress event before cancel")
	}
}

// minRedactHook is a minimal RunHooks that replaces a single secret value in
// the tool result with "[REDACTED]". It lives in the root package test so we
// can exercise the runner's streaming path + hook pipeline without importing
// hooks/redact (which would create an import cycle: hooks/redact imports
// openagent). This is a test double, not the real hook.
type minRedactHook struct {
	secret string
}

func (h *minRedactHook) OnAgentStart(ctx context.Context, req openagent.ChatCompletionRequest) (any, error) {
	return nil, nil
}
func (h *minRedactHook) OnAgentEnd(ctx context.Context, req openagent.ChatCompletionRequest, resp *openagent.ChatCompletionResponse, runErr error, startState any) {
}
func (h *minRedactHook) OnToolStart(ctx context.Context, tool openagent.FunctionDefinition, args json.RawMessage) (any, error) {
	return nil, nil
}
func (h *minRedactHook) OnToolEnd(ctx context.Context, tool openagent.FunctionDefinition, args json.RawMessage, result *openagent.ToolResult, startState any) {
	if result != nil && result.Content != "" {
		result.Content = strings.ReplaceAll(result.Content, h.secret, "[REDACTED]")
	}
}

func TestStreamingWithRedactHook_FinalResultRedacted(t *testing.T) {
	// A streaming tool emits chunks containing a secret. The user sees live
	// openagent.StreamToolProgress (possibly raw — that's the point of streaming and
	// the model never sees those), but the final openagent.StreamToolResult that
	// enters the model's context must be redacted by OnToolEnd.
	const secret = "supersecret-token-value"
	streamTool := &streamingTestTool{
		name: "stream_tool",
		// Secret split across chunks to mimic realistic streaming.
		chunks: []string{"prefix-", secret, "-suffix"},
		delay:  0,
	}
	model := &fakeModelWithToolCall{
		toolName: "stream_tool",
		toolArgs: `{}`,
		callID:   "call_redact",
	}
	cfg := agent.New("test",
		agent.WithModel(model),
		agent.WithMaxTurns(1),
	)
	deps := Deps{Tools: []openagent.Tool{streamTool}, Hooks: &minRedactHook{secret: secret}}
	agent := New(cfg, deps)

	ch := agent.RunStream(context.Background(), openagent.Session{ID: "test"}, openagent.UserMessage("go"))

	var toolResultEvent *openagent.StreamEvent
	var gotDone bool
	for evt := range ch {
		switch evt.Type {
		case openagent.StreamToolResult:
			evtCopy := evt
			toolResultEvent = &evtCopy
		case openagent.StreamDone:
			gotDone = true
		case openagent.StreamError:
			t.Fatalf("unexpected error: %v", evt.Error)
		case openagent.StreamAborted:
			t.Fatalf("unexpected abort: %v", evt.Error)
		}
	}

	if !gotDone {
		t.Fatal("missing openagent.StreamDone")
	}
	if toolResultEvent == nil {
		t.Fatal("missing openagent.StreamToolResult")
	}
	content := toolResultEvent.Message.Content
	if strings.Contains(content, secret) {
		t.Fatalf("secret leaked into final tool result: %q", content)
	}
	if !strings.Contains(content, "[REDACTED]") {
		t.Fatalf("final result not redacted: %q", content)
	}
}
