package kernel

import (
	"context"
	"errors"
	"fmt"
	"time"

	openagent "github.com/yusheng-g/openagent-go"
)

// callModel calls the model with streaming preferred, retrying on
// transient (RetryableError) failures with exponential backoff. The
// returned retries is the number of attempts before success (0 = first
// try) — surfaced to the model.call stage detail so observers can track
// provider reliability without counting StreamRetrying events.
func (rt *Runtime) callModel(ctx context.Context, req openagent.ChatCompletionRequest, ch chan<- openagent.StreamEvent) (*openagent.ChatCompletionResponse, int, error) {
	const maxRetries = 3
	var lastErr error

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<uint(attempt-1)) * time.Second
			var re *openagent.RetryableError
			if errors.As(lastErr, &re) && re.RetryAfter > 0 {
				backoff = re.RetryAfter
			}
			chSend(ctx, ch, openagent.StreamEvent{Type: openagent.StreamRetrying, Error: lastErr})
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return nil, attempt, ctx.Err()
			}
		}

		resp, err := rt.callModelOnce(ctx, req, ch)
		if err == nil {
			return resp, attempt, nil
		}
		var re *openagent.RetryableError
		if !errors.As(err, &re) {
			return nil, attempt, err
		}
		lastErr = err
	}
	return nil, maxRetries, fmt.Errorf("max retries exceeded: %w", lastErr)
}

// callModelOnce tries streaming first, falls back to non-streaming.
func (rt *Runtime) callModelOnce(ctx context.Context, req openagent.ChatCompletionRequest, ch chan<- openagent.StreamEvent) (*openagent.ChatCompletionResponse, error) {
	reader, err := rt.runModel.ChatCompletionStream(ctx, req)
	if err != nil {
		return nil, err
	}
	if reader != nil {
		defer reader.Close()
		return accumulateStream(ctx, reader, ch)
	}
	// Non-streaming fallback: emit the full response as single text deltas
	// so consumers see output immediately.
	resp, err := rt.runModel.ChatCompletion(ctx, req)
	if err != nil {
		return nil, err
	}
	if ch != nil && len(resp.Choices) > 0 {
		if rc := resp.Choices[0].Message.ReasoningContent; rc != "" {
			chSend(ctx, ch, openagent.StreamEvent{Type: openagent.StreamThought, Text: rc})
		}
		if resp.Choices[0].Message.Content != "" {
			chSend(ctx, ch, openagent.StreamEvent{Type: openagent.StreamTextDelta, Text: resp.Choices[0].Message.Content})
		}
	}
	return resp, nil
}

// accumulateStream drains a StreamReader, assembling the full response.
// Sends text_delta events to ch as content arrives; all sends are
// cancellable via ctx to prevent deadlocks.
func accumulateStream(ctx context.Context, reader openagent.StreamReader, ch chan<- openagent.StreamEvent) (*openagent.ChatCompletionResponse, error) {
	var (
		content      string
		reasoning    string
		finishReason string
		usage        openagent.Usage
	)
	toolAcc := make(map[int]*openagent.ToolCall)

	for reader.Next() {
		chunk := reader.Current()
		if chunk.Usage != nil {
			usage = *chunk.Usage
		}
		for _, delta := range chunk.Choices {
			content += delta.Content
			reasoning += delta.ReasoningContent
			if delta.FinishReason != "" {
				finishReason = delta.FinishReason
			}
			if ch != nil {
				if delta.ReasoningContent != "" {
					chSend(ctx, ch, openagent.StreamEvent{Type: openagent.StreamThought, Text: delta.ReasoningContent})
				}
				if delta.Content != "" {
					chSend(ctx, ch, openagent.StreamEvent{Type: openagent.StreamTextDelta, Text: delta.Content})
				}
			}
			for _, tcd := range delta.ToolCalls {
				tc := toolAcc[tcd.Index]
				if tc == nil {
					tc = &openagent.ToolCall{}
					toolAcc[tcd.Index] = tc
				}
				if tcd.ID != "" {
					tc.ID = tcd.ID
				}
				if tcd.Type != "" {
					tc.Type = tcd.Type
				}
				if tcd.Function.Name != "" {
					tc.Function.Name = tcd.Function.Name
				}
				tc.Function.Arguments += tcd.Function.Arguments
			}
		}
	}
	if err := reader.Err(); err != nil {
		return nil, err
	}

	var toolCalls []openagent.ToolCall
	for i := 0; i < len(toolAcc); i++ {
		if tc, ok := toolAcc[i]; ok {
			toolCalls = append(toolCalls, *tc)
		}
	}

	return &openagent.ChatCompletionResponse{
		Choices: []openagent.Choice{{
			Index:        0,
			Message:      openagent.Message{Role: openagent.RoleAssistant, Content: content, ReasoningContent: reasoning, ToolCalls: toolCalls},
			FinishReason: finishReason,
		}},
		Usage: usage,
	}, nil
}
