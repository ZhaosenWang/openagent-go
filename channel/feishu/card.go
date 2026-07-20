package feishu

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/yusheng-g/openagent-go/channel"
)

// BuildCard converts a channel.Card into a Feishu interactive card JSON
// string ready for the message API content field.
//
// Card structure:
//
//	header  → title + subtitle + colour template
//	elements → markdown(content) → hr → note(footer)
//
// See: https://open.feishu.cn/document/uAjLw4CM/ukzMukzMukzM/feishu-cards/card-components/overview
func BuildCard(c *channel.Card) (string, error) {
	if c == nil {
		return "", fmt.Errorf("card is nil")
	}

	cfg := cardConfig{WideScreenMode: true}
	hdr := buildHeader(c.Header, c.Color)

	var elems []map[string]any

	// Body — single markdown element. Feishu's markdown tag supports
	// CommonMark: headers, tables, code blocks, lists, bold, etc.
	// A single element keeps multi-line constructs (tables, code blocks)
	// intact so they render correctly.
	body := strings.TrimSpace(c.Content)
	if body == "" {
		body = "(empty)"
	}
	elems = append(elems, map[string]any{
		"tag":     "markdown",
		"content": body,
	})

	// Separator + footer note.
	if c.Footer != "" {
		elems = append(elems, map[string]any{"tag": "hr"})
		elems = append(elems, map[string]any{
			"tag": "note",
			"elements": []map[string]any{
				{
					"tag":     "plain_text",
					"content": c.Footer,
				},
			},
		})
	}

	card := map[string]any{
		"config":   cfg,
		"header":   hdr,
		"elements": elems,
	}

	b, err := json.Marshal(card)
	if err != nil {
		return "", fmt.Errorf("feishu card marshal: %w", err)
	}
	return string(b), nil
}

type cardConfig struct {
	WideScreenMode bool `json:"wide_screen_mode"`
}

func buildHeader(h channel.CardHeader, color channel.CardColor) map[string]any {
	hdr := map[string]any{
		"title": map[string]any{
			"tag":     "plain_text",
			"content": h.Title,
		},
		"template": string(color),
	}
	if h.Subtitle != "" {
		hdr["subtitle"] = map[string]any{
			"tag":     "plain_text",
			"content": h.Subtitle,
		}
	}
	return hdr
}

// ── Convenience builders ──

// BuildPlanCard renders a plan_create result as a Feishu card.
// entries is markdown already formatted by the plan package (one line per entry).
func BuildPlanCard(goal string, entriesMarkdown string) (string, error) {
	return BuildCard(&channel.Card{
		Header: channel.CardHeader{
			Title:    "Plan: " + goal,
			Subtitle: "",
		},
		Content: entriesMarkdown,
		Footer:  "The plan has been created. The agent will proceed to execute each step.",
		Color:   channel.CardColorBlue,
	})
}

// BuildToolCallCard renders a tool call result as a compact card.
func BuildToolCallCard(toolName, params, result string) (string, error) {
	content := fmt.Sprintf("**Tool:** `%s`\n\n**Args:**\n```\n%s\n```\n\n**Result:**\n%s",
		toolName, truncateForCard(params, 300), truncateForCard(result, 500))
	return BuildCard(&channel.Card{
		Header:  channel.CardHeader{Title: "Tool: " + toolName},
		Content: content,
		Footer:  "",
		Color:   channel.CardColorGrey,
	})
}

func truncateForCard(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
