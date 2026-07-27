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
// Card structure (schema 2.0):
//
//	schema  → "2.0"
//	header  → title + subtitle + colour template
//	body.elements → markdown(content) [→ hr → note(footer)]
//
// When Card.Collapsed is set, the body markdown is wrapped in a
// collapsible_panel (expanded:false) so verbose content (tool output,
// reasoning) is hidden behind a clickable bar by default.
//
// See: https://open.feishu.cn/document/uAjLw4CM/ukzMukzMukzM/feishu-cards/card-components/overview
func BuildCard(c *channel.Card) (string, error) {
	if c == nil {
		return "", fmt.Errorf("card is nil")
	}

	hdr := buildHeader(c.Header, c.Color)

	// Body — single markdown element. Feishu's markdown tag supports
	// CommonMark: headers, tables, code blocks, lists, bold, etc.
	// A single element keeps multi-line constructs (tables, code blocks)
	// intact so they render correctly.
	body := strings.TrimSpace(c.Content)
	if body == "" {
		body = "(empty)"
	}
	bodyElem := map[string]any{
		"tag":     "markdown",
		"content": body,
	}

	var elems []map[string]any
	if len(c.Panels) > 0 {
		// Nested panels: each sub-card becomes an inner collapsible_panel.
		inner := make([]map[string]any, 0, len(c.Panels))
		for i := range c.Panels {
			inner = append(inner, subPanel(&c.Panels[i]))
		}
		if c.Collapsed {
			elems = append(elems, collapsiblePanelList(c.Header.Title, inner))
		} else {
			elems = append(elems, inner...)
		}
		// When the card also carries a markdown body (e.g. the run card's
		// answer section), append it after the panels.
		if body != "" && body != "(empty)" {
			elems = append(elems, bodyElem)
		}
	} else if c.Collapsed {
		elems = append(elems, collapsiblePanel(bodyElem))
	} else {
		elems = append(elems, bodyElem)
	}

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
		"schema": "2.0",
		"header": hdr,
		"body":   map[string]any{"elements": elems},
	}

	b, err := json.Marshal(card)
	if err != nil {
		return "", fmt.Errorf("feishu card marshal: %w", err)
	}
	return string(b), nil
}

// panel builds a default-collapsed collapsible_panel with the given header
// title and body elements. The chevron icon sits on the right; clicking the
// bar expands the content.
func panel(title string, elements []map[string]any) map[string]any {
	return map[string]any{
		"tag":     "collapsible_panel",
		"expanded": false,
		"header": map[string]any{
			"title": map[string]any{
				"tag":     "plain_text",
				"content": title,
			},
			"vertical_align": "center",
			"icon": map[string]any{
				"tag":   "standard_icon",
				"token": "down-small-ccm_outlined",
				"size":  "16px 16px",
			},
			"icon_position":       "right",
			"icon_expanded_angle": -180,
		},
		"border": map[string]any{
			"color":         "grey",
			"corner_radius": "5px",
		},
		"padding":          "8px 8px 8px 8px",
		"vertical_spacing": "8px",
		"elements":         elements,
	}
}

// collapsiblePanel wraps a single markdown body element in a default-collapsed
// panel. The panel header shows a one-line preview of the content.
func collapsiblePanel(body map[string]any) map[string]any {
	return panel(panelPreview(body), []map[string]any{body})
}

// collapsiblePanelList wraps a list of inner panels in one outer collapsed
// panel — used to group multiple tool calls under a single "toolcalls (N)" bar.
func collapsiblePanelList(title string, inner []map[string]any) map[string]any {
	return panel(title, inner)
}

// subPanel renders a nested Card as an inner collapsed panel. The sub-card's
// header title becomes the panel bar label; an empty title falls back to a
// one-line preview of the content. Nested panels recurse.
func subPanel(c *channel.Card) map[string]any {
	var elements []map[string]any
	if len(c.Panels) > 0 {
		elements = make([]map[string]any, 0, len(c.Panels))
		for i := range c.Panels {
			elements = append(elements, subPanel(&c.Panels[i]))
		}
	} else {
		body := strings.TrimSpace(c.Content)
		if body == "" {
			body = "(empty)"
		}
		elements = []map[string]any{{"tag": "markdown", "content": body}}
	}
	title := c.Header.Title
	if title == "" {
		title = panelPreview(elements[0])
	}
	return panel(title, elements)
}

// panelPreview extracts a one-line preview from a markdown body element.
// Used as the collapsed panel's header text so the user sees what's inside
// without expanding. Falls back to a generic label when content is empty.
func panelPreview(body map[string]any) string {
	content, _ := body["content"].(string)
	if content == "" {
		return "展开查看"
	}
	// Take the first non-empty line, strip markdown fences/noise, truncate.
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "```" || strings.HasPrefix(line, "```") {
			continue
		}
		if len(line) > 80 {
			line = line[:80] + "…"
		}
		return line
	}
	return "展开查看"
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
