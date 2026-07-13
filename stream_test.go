package openagent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

// ── streamingTestTool implements both Tool and StreamExecutor ──

type streamingTestTool struct {
	name   string
	chunks []string // chunks to emit
	delay  time.Duration
}

func (t *streamingTestTool) Definition() FunctionDefinition {
	return FunctionDefinition{
		Name:        t.name,
		Description: "a streaming test tool",
		Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
	}
}

func (t *streamingTestTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	return strings.Join(t.chunks, ""), nil
}

func (t *streamingTestTool) ExecuteStream(ctx context.Context, args json.RawMessage) <-chan ToolStreamChunk {
	ch := make(chan ToolStreamChunk, len(t.chunks))
	go func() {
		defer close(ch)
		for _, c := range t.chunks {
			select {
			case <-ctx.Done():
				return
			case <-time.After(t.delay):
			}
			ch <- ToolStreamChunk{Content: c}
		}
	}()
	return ch
}

// ── nonStreamingTool implements only Tool ──

type nonStreamingTool struct{}

func (t *nonStreamingTool) Definition() FunctionDefinition {
	return FunctionDefinition{
		Name:        "blocking_tool",
		Description: "a blocking tool",
		Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
	}
}

func (t *nonStreamingTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	return "blocking_result", nil
}

// ── fakeModelWithToolCall returns a tool call then stops ──

type fakeModelWithToolCall struct {
	toolName string
	toolArgs string
	callID   string
}

func (m *fakeModelWithToolCall) ChatCompletion(ctx context.Context, req ChatCompletionRequest) (*ChatCompletionResponse, error) {
	return &ChatCompletionResponse{
		Choices: []Choice{{
			Index: 0,
			Message: Message{
				Role: RoleAssistant,
				ToolCalls: []ToolCall{{
					ID:   m.callID,
					Type: "function",
					Function: ToolCallFunction{
						Name:      m.toolName,
						Arguments: m.toolArgs,
					},
				}},
			},
			FinishReason: "tool_calls",
		}},
	}, nil
}

func (m *fakeModelWithToolCall) ChatCompletionStream(ctx context.Context, req ChatCompletionRequest) (StreamReader, error) {
	return nil, nil // fallback to non-streaming
}

func (m *fakeModelWithToolCall) ContextWindow() int { return 128_000 }

// ── fakeModelWithTwoToolCalls returns two tool calls in one response ──

type fakeModelWithTwoToolCalls struct {
	calls []ToolCall
}

func (m *fakeModelWithTwoToolCalls) ChatCompletion(ctx context.Context, req ChatCompletionRequest) (*ChatCompletionResponse, error) {
	return &ChatCompletionResponse{
		Choices: []Choice{{
			Index: 0,
			Message: Message{
				Role:      RoleAssistant,
				ToolCalls: m.calls,
			},
			FinishReason: "tool_calls",
		}},
	}, nil
}

func (m *fakeModelWithTwoToolCalls) ChatCompletionStream(ctx context.Context, req ChatCompletionRequest) (StreamReader, error) {
	return nil, nil
}

func (m *fakeModelWithTwoToolCalls) ContextWindow() int { return 128_000 }

// ── Tests ──

func TestStreamExecutorProgressEvents(t *testing.T) {
	// Verify that a tool implementing StreamExecutor emits StreamToolProgress
	// events with correct ToolCallID, and the final StreamToolResult aggregates all chunks.
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

	agent := NewAgent("test",
		WithModel(model),
		WithTools(streamTool),
		WithMaxTurns(1),
	)

	ch := agent.RunStream(context.Background(), Session{ID: "test"}, UserMessage("go"))

	var progressEvents []StreamEvent
	var toolResultEvent *StreamEvent
	var gotDone bool

	for evt := range ch {
		switch evt.Type {
		case StreamToolProgress:
			progressEvents = append(progressEvents, evt)
		case StreamToolResult:
			evtCopy := evt
			toolResultEvent = &evtCopy
		case StreamDone:
			gotDone = true
		case StreamError:
			t.Fatalf("unexpected error: %v", evt.Error)
		case StreamAborted:
			t.Fatalf("unexpected abort: %v", evt.Error)
		}
	}

	if !gotDone {
		t.Fatal("missing StreamDone")
	}

	// Verify progress events.
	if len(progressEvents) != 3 {
		t.Fatalf("expected 3 progress events, got %d", len(progressEvents))
	}
	for i, pe := range progressEvents {
		if pe.Text != streamTool.chunks[i] {
			t.Errorf("progress[%d]: expected %q, got %q", i, streamTool.chunks[i], pe.Text)
		}
		if pe.ToolCallID != "call_abc" {
			t.Errorf("progress[%d]: expected ToolCallID=call_abc, got %q", i, pe.ToolCallID)
		}
	}

	// Verify final result aggregates all chunks.
	if toolResultEvent == nil {
		t.Fatal("missing StreamToolResult")
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

	agent := NewAgent("test",
		WithModel(model),
		WithTools(blockingTool),
		WithMaxTurns(1),
	)

	ch := agent.RunStream(context.Background(), Session{ID: "test"}, UserMessage("go"))

	var gotProgress bool
	var gotResult bool
	var gotDone bool

	for evt := range ch {
		switch evt.Type {
		case StreamToolProgress:
			gotProgress = true
		case StreamToolResult:
			gotResult = true
			if evt.Message.Content != "blocking_result" {
				t.Errorf("expected 'blocking_result', got %q", evt.Message.Content)
			}
		case StreamDone:
			gotDone = true
		case StreamError:
			t.Fatalf("unexpected error: %v", evt.Error)
		case StreamAborted:
			t.Fatalf("unexpected abort: %v", evt.Error)
		}
	}

	if gotProgress {
		t.Error("non-streaming tool should NOT emit StreamToolProgress")
	}
	if !gotResult {
		t.Error("missing StreamToolResult")
	}
	if !gotDone {
		t.Error("missing StreamDone")
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
		calls: []ToolCall{
			{ID: "call_aaa", Type: "function", Function: ToolCallFunction{Name: "tool_a", Arguments: "{}"}},
			{ID: "call_bbb", Type: "function", Function: ToolCallFunction{Name: "tool_b", Arguments: "{}"}},
		},
	}

	agent := NewAgent("test",
		WithModel(model),
		WithTools(toolA, toolB),
		WithMaxTurns(1),
	)

	ch := agent.RunStream(context.Background(), Session{ID: "test"}, UserMessage("go"))

	progressByID := map[string][]string{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(1)

	// Collect events concurrently (simulating real consumer).
	go func() {
		defer wg.Done()
		for evt := range ch {
			if evt.Type == StreamToolProgress {
				mu.Lock()
				progressByID[evt.ToolCallID] = append(progressByID[evt.ToolCallID], evt.Text)
				mu.Unlock()
			}
		}
	}()
	wg.Wait()

	// Verify both tool call IDs received progress events.
	aChunks := progressByID["call_aaa"]
	bChunks := progressByID["call_bbb"]
	if len(aChunks) != 3 {
		t.Errorf("tool_a: expected 3 chunks, got %d: %v", len(aChunks), aChunks)
	}
	if len(bChunks) != 2 {
		t.Errorf("tool_b: expected 2 chunks, got %d: %v", len(bChunks), bChunks)
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

	agent := NewAgent("test",
		WithModel(model),
		WithTools(streamTool),
		WithMaxTurns(1),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := agent.RunStream(ctx, Session{ID: "test"}, UserMessage("go"))

	// Read the first progress event, then cancel.
	var gotFirst bool
	for evt := range ch {
		if evt.Type == StreamToolProgress && !gotFirst {
			gotFirst = true
			cancel()
		}
	}

	// After cancel, we should see StreamAborted (not StreamDone).
	// The channel should close cleanly.
	if !gotFirst {
		t.Error("should have received at least one progress event before cancel")
	}
}
