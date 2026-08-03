// Package summarizer provides LLM-based conversation compression
// implementing openagent.Summarizer.
//
// The Compressor uses the agent's Model to generate incremental,
// rolling summaries of older messages. Each summary subsumes the
// previous one so the context window stays compact without losing
// long-term history.
package summarizer

import (
	"context"
	"fmt"
	"strings"

	openagent "github.com/yusheng-g/openagent-go"
)

// Compressor implements openagent.Summarizer by calling the configured
// Model to produce incremental summaries.
type Compressor struct {
	model     openagent.Model
	maxTokens int // 0 = no hint; non-zero = prompt the model to keep the summary under this
}

// New creates a Compressor backed by m.
func New(m openagent.Model) *Compressor {
	return &Compressor{model: m}
}

// WithMaxTokens sets a SOFT target for the summary size: the budget is
// passed to the model as a prompt hint, NOT as ChatCompletionRequest
// MaxTokens. A hard output cap truncates the JSON envelope mid-stream,
// which parseSummary then rejects — the real length limit is enforced
// when the summary enters the prompt (kernel's MaxCompressedTokens
// truncation). Default is 0 (no hint).
func (c *Compressor) WithMaxTokens(n int) *Compressor {
	c.maxTokens = n
	return c
}

// Summarize implements openagent.Summarizer.
//
// When previous is nil this is the first compression pass — a fresh
// summary is generated. Otherwise the new messages are folded into the
// existing summary, producing an updated CompressedContext whose
// ThroughIndex is left at zero (the caller sets it).
func (c *Compressor) Summarize(ctx context.Context, messages []openagent.Message, previous *openagent.CompressedContext) (*openagent.CompressedContext, error) {
	if c.model == nil {
		return nil, fmt.Errorf("summarizer: no model configured")
	}
	if len(messages) == 0 {
		return nil, fmt.Errorf("summarizer: no messages to summarize")
	}

	prompt := c.buildSummarizePrompt(messages, previous)
	// No MaxTokens on purpose: a hard output cap truncates the JSON
	// envelope mid-stream and parseSummary rejects it. Length control is
	// the prompt hint below plus the prompt-side truncation in kernel.
	resp, err := c.model.ChatCompletion(ctx, openagent.ChatCompletionRequest{
		Messages: []openagent.Message{
			{Role: openagent.RoleSystem, Content: summarizeSystemPrompt},
			{Role: openagent.RoleUser, Content: prompt},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("summarizer: model call: %w", err)
	}

	content := ""
	if len(resp.Choices) > 0 {
		content = resp.Choices[0].Message.Content
	}
	content = strings.TrimSpace(content)
	if content == "" {
		return nil, fmt.Errorf("summarizer: model returned empty summary")
	}
	return &openagent.CompressedContext{Summary: content}, nil
}

// ── Prompt ──

const summarizeSystemPrompt = `You are a conversation summarizer. Your job is to produce a concise,
structured summary of a conversation so an AI assistant can resume the
thread without re-reading every message.

The summary is the assistant's OWN memory of the conversation (it is
injected back to the same assistant). Refer to the user as "the user";
narrate your own actions with no third-person subject ("searched memory,
found...", NOT "the assistant searched memory") — the reader is yourself.

Structure the summary text with exactly these eight sections, in order
(the Claude Code compaction format; user messages are NOT listed
verbatim — recent messages stay full-fidelity in the working set, older
ones are summarized by intent within the sections below):
1. Primary Request and Intent — what the user originally wanted and the deeper goal
2. Key Technical Concepts — frameworks, patterns, algorithms, architectures
3. Files and Code Sections — every relevant file by path and why it matters
4. Errors and Fixes — errors, how they were resolved, user reactions
5. Problem Solving — reasoning chains, alternatives, debugging strategies
6. Pending Tasks — unfinished or deferred work
7. Current Work — precise description of work in progress at conversation end
8. Optional Next Step — aligned with the user's most recent explicit requests

Be concise. The summary is injected into a system prompt.

Output your summary as plain text only: the eight numbered sections, no
intro label, no JSON, no markdown code fences — the text is injected
directly into a system prompt and JSON escaping would corrupt it.`

func (c *Compressor) buildSummarizePrompt(messages []openagent.Message, prev *openagent.CompressedContext) string {
	var b strings.Builder
	if c.maxTokens > 0 {
		fmt.Fprintf(&b, "Target length: keep the summary under %d tokens.\n\n", c.maxTokens)
	}
	if prev != nil && prev.Summary != "" {
		b.WriteString("## Existing Summary\n")
		b.WriteString(prev.Summary)
		b.WriteString("\n\n## New Messages (incorporate into the summary)\n\n")
	} else {
		b.WriteString("## Messages to Summarize\n\n")
	}
	for _, m := range messages {
		switch m.Role {
		case openagent.RoleUser:
			b.WriteString("User: ")
		case openagent.RoleAssistant:
			b.WriteString("Assistant: ")
		case openagent.RoleTool:
			if m.ToolCallID != "" {
				fmt.Fprintf(&b, "Tool result (%s): ", m.ToolCallID)
			} else {
				b.WriteString("System: ")
			}
		case openagent.RoleSystem:
			b.WriteString("System: ")
		}
		b.WriteString(truncateContent(m.Content, 300))
		b.WriteString("\n")
		if len(m.ToolCalls) > 0 {
			for _, tc := range m.ToolCalls {
				fmt.Fprintf(&b, "  [called tool %s]\n", tc.Function.Name)
			}
		}
	}
	return b.String()
}

func truncateContent(s string, n int) string {
	s = strings.TrimSpace(s)
	// Truncate by rune (byte-slicing cuts multi-byte UTF-8 in half).
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	if n <= 3 {
		return "..."
	}
	return string(runes[:n-3]) + "..."
}

