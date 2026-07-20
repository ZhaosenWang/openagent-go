package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	openagent "github.com/yusheng-g/openagent-go"
	"github.com/yusheng-g/openagent-go/channel"
	"github.com/yusheng-g/openagent-go/channel/feishu"

	"github.com/yusheng-g/openagent-go/cmd/cli/config"
)

// RunChannels starts all configured IM channels.
func RunChannels(ctx context.Context, agent *openagent.Agent, cfg config.ChannelsConfig) error {
	var channels []channel.Channel

	if cfg.Feishu != nil {
		channels = append(channels, feishu.New(cfg.Feishu.AppID, cfg.Feishu.AppSecret))
	}

	if len(channels) == 0 {
		return nil
	}

	for _, ch := range channels {
		log.Printf("channel: starting %s", ch.Name())
		go func(ch channel.Channel) {
			handler := channel.MessageHandler(func(msgCtx context.Context, msg channel.IncomingMessage, reply channel.ReplyFunc) {
				sessionID := ch.Name() + "_" + msg.ChatID

				go func() {
					session := openagent.Session{
						ID:        sessionID,
						CreatedAt: time.Now(),
					}
					stream := agent.RunStream(msgCtx, session, openagent.UserMessage(msg.Text))
					streamReply(reply, stream)
				}()
			})

			if err := ch.Start(ctx, handler); err != nil {
				log.Printf("channel: %s stopped: %v", ch.Name(), err)
			}
		}(ch)
	}

	return nil
}

// streamReply drains the agent stream and sends every message as a card.
//
// Text streaming: creates a "🧠 openagent" card on the first text chunk,
// patches it every ~1s/300 chars until a tool call or stream end finalises
// it.
//
// Tool calls: creates a tool card showing the command/input, then patches
// the same card as StreamToolProgress chunks arrive. On StreamToolResult
// the card receives a final update with the result.
//
// plan_create gets its own dedicated plan card.
func streamReply(reply channel.ReplyFunc, stream <-chan openagent.StreamEvent) {
	type tpend struct {
		name string
		args string
	}

	var (
		textCardID       string          // response card, patched as text streams
		textBuf          strings.Builder // accumulated text since last card patch
		textLast         = time.Now()    // last patch time

		thoughtCardID string          // reasoning card, patched during thinking phase
		thoughtBuf    strings.Builder // accumulated reasoning text

		pendingTool = map[string]*tpend{} // toolCallID → {name, args}
		toolCardID  = map[string]string{} // toolCallID → card message ID
		toolBuf     = map[string]string{} // toolCallID → accumulated progress output
	)

	// ── Card send helpers ──

	mkCard := func(title, body string, color channel.CardColor) *channel.Card {
		return &channel.Card{Header: channel.CardHeader{Title: title}, Content: body, Color: color}
	}

	newCard := func(msg channel.ReplyMessage) string {
		id, _ := reply(context.Background(), msg)
		return id
	}

	patchCard := func(msgID, title, body string, color channel.CardColor) {
		msg := channel.ReplyMessage{
			UpdateID: msgID,
			Card:     &channel.Card{Header: channel.CardHeader{Title: title}, Content: body, Color: color},
		}
		_, _ = reply(context.Background(), msg)
	}

	// ── Finalize helpers ──

	finalizeThoughtCard := func() {
		if thoughtCardID == "" {
			return
		}
		patchCard(thoughtCardID, "🤔 thinking — done", thoughtBuf.String(), channel.CardColorYellow)
		thoughtCardID = ""
		thoughtBuf.Reset()
	}

	finalizeTextCard := func() {
		if textCardID == "" {
			return
		}
		patchCard(textCardID, "🧠 openagent", textBuf.String(), channel.CardColorGrey)
		textCardID = ""
		textBuf.Reset()
	}

	flushText := func() {
		if textBuf.Len() == 0 {
			return
		}
		body := textBuf.String()
		if textCardID == "" {
			textCardID = newCard(channel.ReplyMessage{Card: mkCard("🧠 openagent", body, channel.CardColorGrey)})
		} else {
			patchCard(textCardID, "🧠 openagent", body, channel.CardColorGrey)
		}
		textLast = time.Now()
	}

	for evt := range stream {
		switch evt.Type {
		case openagent.StreamThought:
			thoughtBuf.WriteString(evt.Text)
			body := thoughtBuf.String()
			if thoughtCardID == "" {
				thoughtCardID = newCard(channel.ReplyMessage{Card: mkCard("🤔 thinking", body, channel.CardColorYellow)})
			} else {
				patchCard(thoughtCardID, "🤔 thinking", body, channel.CardColorYellow)
			}

		case openagent.StreamTextDelta:
			finalizeThoughtCard()
			textBuf.WriteString(evt.Text)
			if time.Since(textLast) >= 80*time.Millisecond || textBuf.Len() >= 50 {
				flushText()
			}

		case openagent.StreamToolCall:
			finalizeThoughtCard()
			finalizeThoughtCard()
	finalizeTextCard()
			for _, tc := range evt.Message.ToolCalls {
				if tc.Function.Name == "plan_create" {
					goal, steps := parsePlanCreate(tc.Function.Arguments)
					if goal != "" {
						newCard(channel.ReplyMessage{Card: mkCard("📋 " + goal, steps, channel.CardColorBlue)})
					}
					continue
				}
				pendingTool[tc.ID] = &tpend{name: tc.Function.Name, args: tc.Function.Arguments}
				toolBuf[tc.ID] = ""
			}

		case openagent.StreamToolProgress:
			t, ok := pendingTool[evt.ToolCallID]
			if !ok {
				continue
			}
			toolBuf[evt.ToolCallID] += evt.Text

			card := toolCard(t.name, t.args, "in_progress", toolBuf[evt.ToolCallID])
			if msgID, exists := toolCardID[evt.ToolCallID]; exists {
				patchCard(msgID, card.Header.Title, card.Content, card.Color)
			} else {
				id := newCard(channel.ReplyMessage{Card: card})
				if id != "" {
					toolCardID[evt.ToolCallID] = id
				}
			}

		case openagent.StreamToolResult:
			t, ok := pendingTool[evt.Message.ToolCallID]
			if !ok {
				continue
			}
			delete(pendingTool, evt.Message.ToolCallID)
			delete(toolBuf, evt.Message.ToolCallID)

			if t.name == "plan_update" {
				continue
			}

			output := evt.Message.Content
			status := "completed"
			if strings.HasPrefix(output, "error: ") {
				status = "failed"
			}

			card := toolCard(t.name, t.args, status, output)
			if msgID := toolCardID[evt.Message.ToolCallID]; msgID != "" {
				delete(toolCardID, evt.Message.ToolCallID)
				patchCard(msgID, card.Header.Title, card.Content, card.Color)
			} else {
				newCard(channel.ReplyMessage{Card: card})
			}

		case openagent.StreamRetrying:
			finalizeThoughtCard()
			finalizeThoughtCard()
	finalizeTextCard()
			errMsg := "retrying..."
			if evt.Error != nil {
				errMsg = fmt.Sprintf("retrying: %v", evt.Error)
			}
			newCard(channel.ReplyMessage{Card: mkCard("⚠️ retrying", errMsg, channel.CardColorYellow)})

		case openagent.StreamDone:
			finalizeThoughtCard()
			finalizeThoughtCard()
	finalizeTextCard()

		case openagent.StreamError:
			finalizeThoughtCard()
			finalizeThoughtCard()
	finalizeTextCard()
			if evt.Error != nil {
				newCard(channel.ReplyMessage{Card: mkCard("❌ error", fmt.Sprintf("%v", evt.Error), channel.CardColorRed)})
			}
			return

		case openagent.StreamAborted:
			finalizeThoughtCard()
			finalizeThoughtCard()
	finalizeTextCard()
			return
		}
	}

	finalizeThoughtCard()
	finalizeTextCard()
}

// ── Tool card ──

func toolCard(name, args, status, output string) *channel.Card {
	title := toolEmoji(name) + " " + name
	color := channel.CardColorGrey
	switch status {
	case "completed":
		title = toolEmoji(name) + " " + name + " ✓"
		color = channel.CardColorGreen
	case "failed":
		title = toolEmoji(name) + " " + name + " ✗"
		color = channel.CardColorRed
	case "in_progress":
		color = channel.CardColorPurple
	}

	body := formatInput(name, args)
	if output != "" {
		body += "\n```\n" + output + "\n```"
	}

	return &channel.Card{
		Header:  channel.CardHeader{Title: title},
		Content: body,
		Color:   color,
	}
}

func formatInput(name, args string) string {
	m := jsonMap(args)
	switch name {
	case "shell", "terminal_create":
		cmd := jsonStr(m, "command")
		if cmd != "" {
			return "```\n" + trunc(cmd, 500) + "\n```"
		}
	case "read", "read_client_file":
		path := jsonStr(m, "path")
		if path == "" {
			path = jsonStr(m, "uri")
		}
		if path != "" {
			return "`" + path + "`"
		}
	case "write", "write_client_file":
		path := jsonStr(m, "path")
		if path == "" {
			path = jsonStr(m, "uri")
		}
		if path != "" {
			return "`" + path + "`"
		}
	case "grep":
		q := jsonStr(m, "query")
		if q == "" {
			q = jsonStr(m, "pattern")
		}
		path := jsonStr(m, "path")
		if path == "" {
			path = jsonStr(m, "dir")
		}
		if q != "" {
			return "`" + q + "`" + pathStr(path)
		}
	case "recall":
		q := jsonStr(m, "query")
		if q != "" {
			return "`" + q + "`"
		}
	case "ls":
		path := jsonStr(m, "path")
		if path == "" {
			path = jsonStr(m, "dir")
		}
		if path != "" {
			return "`" + path + "`"
		}
	case "subagent":
		n := jsonStr(m, "name")
		t := jsonStr(m, "task")
		if n != "" {
			return "**" + n + "** — " + trunc(t, 200)
		}
		return trunc(t, 200)
	}
	return "```\n" + trunc(args, 200) + "\n```"
}

func pathStr(p string) string {
	if p != "" {
		return " in `" + p + "`"
	}
	return ""
}

func toolEmoji(name string) string {
	switch name {
	case "read", "read_client_file":
		return "📖"
	case "write", "write_client_file":
		return "✏️"
	case "shell", "terminal_create":
		return "💻"
	case "grep":
		return "🔍"
	case "ls":
		return "📂"
	case "recall":
		return "🧠"
	case "subagent":
		return "🤖"
	case "use_skill":
		return "📦"
	default:
		return "🔧"
	}
}

// ── Helpers ──

func jsonMap(raw string) map[string]any {
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil
	}
	return m
}

func jsonStr(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, _ := m[key].(string)
	return v
}

func parsePlanCreate(args string) (goal string, steps string) {
	var p struct {
		Goal  string `json:"goal"`
		Steps []struct {
			Content  string `json:"content"`
			Priority string `json:"priority"`
		} `json:"steps"`
	}
	if err := json.Unmarshal([]byte(args), &p); err != nil || p.Goal == "" {
		return "", ""
	}

	var b strings.Builder
	for i, s := range p.Steps {
		emoji := "⬜"
		switch s.Priority {
		case "high":
			emoji = "🔴"
		case "medium":
			emoji = "🟡"
		case "low":
			emoji = "🟢"
		}
		fmt.Fprintf(&b, "%s **Step %d:** %s\n", emoji, i+1, s.Content)
	}
	return p.Goal, b.String()
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
