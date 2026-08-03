package context

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	openagent "github.com/yusheng-g/openagent-go"
)

// Knowledge extraction: the self-evolution loop. After a session finishes,
// the extractor scans the conversation for durable knowledge (user
// preferences, project facts, successful approaches) and stores it via
// the MemoryProvider so future sessions recall it automatically.
//
// The LLM extractor follows the industry pattern (Mem0): one pass extracts
// candidate facts AND classifies each against existing knowledge — add
// (new topic), update (same topic, new/changed information → the store
// upserts by scope+topic), or skip (already covered / not durable). This
// prevents memory bloat: repeated sessions about the same topic converge
// on one entry instead of accumulating duplicates.

// Extractor scans finished conversations for durable knowledge.
//
// Extract is fire-and-forget: implementations may run synchronously
// (LLMExtractor) or enqueue to a background worker (AsyncExtractor).
// Errors are logged inside, never returned — extraction must not affect
// the agent run.
type Extractor interface {
	// Extract stores durable knowledge under scope. Best-effort: failures
	// never abort the run.
	Extract(ctx context.Context, scope ContextScope, messages []openagent.Message)
}

// ExtractionItem is one knowledge decision from the extraction pass.
type ExtractionItem struct {
	Op      string `json:"op"`      // "add" | "update" | "skip"
	Kind    string `json:"kind"`    // "preference" | "fact" | "lesson"
	Content string `json:"content"` // concise, self-contained statement
	Topic   string `json:"topic"`   // topic key; update reuses the existing entry's topic
}

// extractionPrompt instructs the model to extract durable knowledge and
// classify it against existing knowledge (Mem0-style ADD/UPDATE/SKIP).
const extractionPrompt = `You are a knowledge curator. From the conversation below, extract durable knowledge worth remembering across future sessions.

Extraction criteria:
- User preferences, project facts, lessons learned — things that will matter later
- Rewrite each as a concise, self-contained statement (never a verbatim quote)
- Skip: small talk, one-time requests, temporary state, anything not reusable

Existing knowledge (topic → content) — compare each candidate against it:
- Same topic with new or changed information → "update": reuse the SAME topic, provide the updated content
- New topic → "add": give a short topic key
- Already covered, nothing new → "skip"

Respond with ONLY a JSON array, no markdown fences:
[{"op":"add","kind":"preference","content":"...","topic":"..."}]
"kind" is one of "preference", "fact", "lesson".`

// extractionBudget caps the transcript tokens fed to the model.
const extractionBudget = 4000

// maxKnowledgeItems caps how many items one extraction pass stores.
const maxKnowledgeItems = 10

// LLMExtractor extracts knowledge with the runtime model.
type LLMExtractor struct {
	// Model is the model used for extraction (the runtime's resolved model).
	Model openagent.Model
	// Provider stores extracted knowledge.
	Provider MemoryProvider
	// MaxItems caps stored items per pass (default 10).
	MaxItems int
}

// NewLLMExtractor creates an extractor. A nil provider yields a no-op.
func NewLLMExtractor(model openagent.Model, p MemoryProvider) *LLMExtractor {
	return &LLMExtractor{Model: model, Provider: p, MaxItems: maxKnowledgeItems}
}

// Extract implements Extractor.
func (e *LLMExtractor) Extract(ctx context.Context, scope ContextScope, messages []openagent.Message) {
	if e == nil || e.Model == nil || e.Provider == nil || len(messages) == 0 {
		return
	}
	max := e.MaxItems
	if max <= 0 {
		max = maxKnowledgeItems
	}

	// Build the transcript within the token budget (recent messages win).
	transcript := buildTranscript(messages)
	if openagent.CountTokens("gpt-4", transcript) > extractionBudget {
		transcript = trimTranscript(transcript, extractionBudget)
	}
	if transcript == "" {
		return
	}

	// Existing knowledge, so the model can classify add/update/skip.
	query := recallQuery(messages)
	existing, _ := e.Provider.Recall(ctx, scope, query, 20)
	var existingText strings.Builder
	for _, m := range existing {
		fmt.Fprintf(&existingText, "- %s → %s\n", m.Topic, m.Content)
	}
	if existingText.Len() == 0 {
		existingText.WriteString("(none)")
	}

	input := "Conversation:\n" + transcript +
		"\n\nExisting knowledge:\n" + existingText.String()

	resp, err := e.Model.ChatCompletion(ctx, openagent.ChatCompletionRequest{
		Messages: []openagent.Message{
			{Role: openagent.RoleSystem, Content: extractionPrompt},
			{Role: openagent.RoleUser, Content: input},
		},
		MaxTokens: 2048,
	})
	if err != nil || resp == nil || len(resp.Choices) == 0 {
		slog.Warn("openagent: knowledge extract failed", "error", err)
		return
	}

	items, err := parseExtractionItems(resp.Choices[0].Message.Content)
	if err != nil {
		raw := resp.Choices[0].Message.Content
		if len(raw) > 300 {
			raw = raw[:300]
		}
		slog.Warn("openagent: knowledge extract parse failed", "error", err, "raw", raw)
		return
	}

	stored := 0
	seen := make(map[string]bool)
	for _, it := range items {
		if stored >= max {
			break
		}
		if it.Op == "skip" || strings.TrimSpace(it.Content) == "" {
			continue
		}
		content := strings.TrimSpace(it.Content)
		if len(content) < 12 || seen[content] {
			continue
		}
		seen[content] = true
		err := e.Provider.Store(ctx, scope, MemoryItem{
			Kind:    it.Kind,
			Content: content,
			Topic:   strings.TrimSpace(it.Topic),
			Meta:    map[string]any{"source": "extractor"},
		})
		if err != nil {
			// A failed store is invisible to the run (extraction is
			// fire-and-forget) — log it or the user silently loses the
			// extracted knowledge.
			slog.Warn("openagent: knowledge store failed", "error", err)
			continue
		}
		stored++
	}
}

// buildTranscript renders messages as a compact transcript (role: content).
func buildTranscript(messages []openagent.Message) string {
	var b strings.Builder
	for _, m := range messages {
		if strings.TrimSpace(m.Content) == "" {
			continue
		}
		b.WriteString(string(m.Role))
		b.WriteString(": ")
		b.WriteString(m.Content)
		b.WriteString("\n")
	}
	return b.String()
}

// trimTranscript keeps the most recent content within the token budget.
// The transcript is in chronological order, so walking from the END keeps
// the latest messages (the oldest overflow is dropped) — matching the
// "recent messages win" contract at the call site.
func trimTranscript(s string, budget int) string {
	lines := strings.Split(s, "\n")
	var keep []string
	total := 0
	for i := len(lines) - 1; i >= 0; i-- {
		n := openagent.CountTokens("gpt-4", lines[i])
		if total+n > budget {
			break // budget exhausted — drop everything older
		}
		total += n
		keep = append(keep, lines[i])
	}
	// Reverse back to chronological order.
	for i, j := 0, len(keep)-1; i < j; i, j = i+1, j-1 {
		keep[i], keep[j] = keep[j], keep[i]
	}
	return strings.Join(keep, "\n")
}

// recallQuery derives the recall query from the most recent user message.
func recallQuery(messages []openagent.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == openagent.RoleUser && strings.TrimSpace(messages[i].Content) != "" {
			c := messages[i].Content
			if len(c) > 200 {
				c = c[:200]
			}
			return c
		}
	}
	return ""
}

// parseExtractionItems parses the model's JSON array response (markdown
// fences tolerated).
func parseExtractionItems(raw string) ([]ExtractionItem, error) {
	content := strings.TrimSpace(raw)
	content = strings.TrimPrefix(content, "```json")
	content = strings.TrimPrefix(content, "```")
	content = strings.TrimSuffix(content, "```")
	content = strings.TrimSpace(content)
	// Find the JSON array if the model wrapped it in prose.
	if i := strings.Index(content, "["); i >= 0 {
		if j := strings.LastIndex(content, "]"); j > i {
			content = content[i : j+1]
		}
	}
	var items []ExtractionItem
	if err := json.Unmarshal([]byte(content), &items); err != nil {
		return nil, fmt.Errorf("parse extraction output: %w", err)
	}
	return items, nil
}
