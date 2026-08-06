package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	openagent "github.com/yusheng-g/openagent-go"
	"github.com/yusheng-g/openagent-go/agent"
	"github.com/yusheng-g/openagent-go/channel"
	"github.com/yusheng-g/openagent-go/kernel"

	"github.com/yusheng-g/openagent-go/cmd/cli/config"
)

// RunChannels wires the feishu and wechat connection managers. Both
// managers are ALWAYS created — the frontend control panel needs the
// status/connect endpoints even when no channel is configured at
// startup; connection is only auto-started when the channel is
// configured (--channel flag or settings channels.<name>).
//
// Start failure is fail-fast for an explicitly flagged channel
// (--channel feishu/wechat): the user asked for the bot, so running
// silently without it would read as "connected" while delivering
// nothing. A settings-only channel degrades to a warning and the server
// continues.
func RunChannels(ctx context.Context, profiles string, cfg *agent.Agent, deps kernel.Deps, channelsCfg config.ChannelsConfig) (*FeishuManager, *WechatManager, error) {
	feishuMgr := NewFeishuManager(ctx, profiles, channelsCfg.Feishu, cfg, deps)
	if channelsCfg.Feishu != nil {
		if err := feishuMgr.Connect(); err != nil {
			if channelsCfg.Feishu.Explicit {
				return nil, nil, err
			}
			slog.Warn("feishu channel not started (settings-only channel)", "error", err)
		}
	}

	wechatMgr := NewWechatManager(ctx, profiles, channelsCfg.Wechat, cfg, deps)
	if channelsCfg.Wechat != nil {
		if err := wechatMgr.Connect(); err != nil {
			if channelsCfg.Wechat.Explicit {
				return nil, nil, err
			}
			slog.Warn("wechat channel not started (settings-only channel)", "error", err)
		}
	}
	return feishuMgr, wechatMgr, nil
}

// patchQueue decouples card rendering from Feishu API calls.
// Updates to the same card within 500ms are collapsed — only the
// latest version is sent. Card creation (which returns a message ID)
// is synchronous; patches are debounced via time.AfterFunc.
//
// No background goroutine — the timer is started on first mark and
// fires once, sending all dirty cards in batch.
type patchQueue struct {
	reply   channel.ReplyFunc
	mu      sync.Mutex
	dirty   map[string]*channel.Card
	timer   *time.Timer
	stopped bool
}

func newPatchQueue(reply channel.ReplyFunc) *patchQueue {
	return &patchQueue{
		reply: reply,
		dirty: make(map[string]*channel.Card),
	}
}

func (pq *patchQueue) mark(msgID string, card *channel.Card) {
	pq.mu.Lock()
	defer pq.mu.Unlock()
	if pq.stopped {
		return
	}
	pq.dirty[msgID] = card
	if pq.timer == nil {
		pq.timer = time.AfterFunc(500*time.Millisecond, pq.flush)
	}
}

func (pq *patchQueue) create(msg channel.ReplyMessage) string {
	id, _ := pq.reply(context.Background(), msg)
	return id
}

func (pq *patchQueue) flush() {
	pq.mu.Lock()
	if pq.stopped {
		pq.mu.Unlock()
		return
	}
	if len(pq.dirty) == 0 {
		pq.timer = nil
		pq.mu.Unlock()
		return
	}
	batch := pq.dirty
	pq.dirty = make(map[string]*channel.Card)
	pq.timer = nil
	pq.mu.Unlock()

	for msgID, card := range batch {
		msg := channel.ReplyMessage{UpdateID: msgID, Card: card}
		_, _ = pq.reply(context.Background(), msg)
	}
}

func (pq *patchQueue) stop() {
	pq.mu.Lock()
	pq.stopped = true
	if pq.timer != nil {
		pq.timer.Stop()
		pq.timer = nil
	}
	batch := pq.dirty
	pq.dirty = nil
	pq.mu.Unlock()

	for msgID, card := range batch {
		msg := channel.ReplyMessage{UpdateID: msgID, Card: card}
		_, _ = pq.reply(context.Background(), msg)
	}
}

// streamReply drains the agent stream and sends every message as a card.
//
// Card patches are debounced — updates to the same card within 500ms
// are collapsed so the Feishu API sees at most 2 PATCH/s per card.
// This prevents the event loop from blocking on HTTP latency.
func streamReply(reply channel.ReplyFunc, stream <-chan openagent.StreamEvent) {
	type tpend struct {
		name string
		args string
	}

	var (
		pq = newPatchQueue(reply)
		// Make sure final flush happens.
		_ = pq.stop // used via defer-like pattern below
	)

	// One card per agent run. Title tracks the stage; body stacks
	// thinking (collapsed) → toolcalls (collapsed) → answer (open).
	var (
		runCardID   string
		thoughtBuf  strings.Builder
		textBuf     strings.Builder
		pendingTool = map[string]*tpend{} // toolCallID → {name, args}
		toolCalls   []toolCallEntry
		toolCallIdx = map[string]int{} // toolCallID → index in toolCalls

		// Stage only advances (thinking < toolcalling < answering < done),
		// so a second round of reasoning mid-turn doesn't flicker the title
		// back to "思考中".
		stage    = stageThinking
		lastErr  string
		lastTime = time.Now()
	)

	// flushRunCard rebuilds the single run card from current state and
	// creates-or-patches it. The 500ms patch debounce bounds API rate.
	flushRunCard := func() {
		card := runCard(stage, thoughtBuf.String(), toolCalls, textBuf.String(), lastErr)
		if runCardID == "" {
			runCardID = pq.create(channel.ReplyMessage{Card: card})
		} else {
			pq.mark(runCardID, card)
		}
		lastTime = time.Now()
	}

	// maybeFlush throttles patches during streaming output.
	maybeFlush := func() {
		if time.Since(lastTime) >= 80*time.Millisecond || textBuf.Len() >= 50 {
			flushRunCard()
		}
	}

	for evt := range stream {
		switch evt.Type {
		case openagent.StreamThought:
			stage = stageThinking
			thoughtBuf.WriteString(evt.Text)
			flushRunCard()

		case openagent.StreamTextDelta:
			stage = stageAnswering
			textBuf.WriteString(evt.Text)
			maybeFlush()

		case openagent.StreamToolCall:
			stage = stageToolCalling
			for _, tc := range evt.Message.ToolCalls {
				switch tc.Function.Name {
				case "plan_create":
					goal, steps := parsePlanCreate(tc.Function.Arguments)
					if goal != "" {
						pq.create(channel.ReplyMessage{Card: mkCard("📋 "+goal, steps, channel.CardColorBlue)})
					}
					continue
				case "plan_update", "enter_plan_mode":
					pendingTool[tc.ID] = &tpend{name: tc.Function.Name, args: tc.Function.Arguments}
					continue
				}
				pendingTool[tc.ID] = &tpend{name: tc.Function.Name, args: tc.Function.Arguments}
				toolCallIdx[tc.ID] = len(toolCalls)
				toolCalls = append(toolCalls, toolCallEntry{
					name:   tc.Function.Name,
					args:   tc.Function.Arguments,
					status: "in_progress",
				})
			}
			flushRunCard()

		case openagent.StreamToolProgress:
			if _, ok := pendingTool[evt.ToolCallID]; !ok {
				continue
			}
			idx, ok := toolCallIdx[evt.ToolCallID]
			if !ok {
				continue
			}
			toolCalls[idx].output += evt.Text
			flushRunCard()

		case openagent.StreamToolResult:
			t, ok := pendingTool[evt.Message.ToolCallID]
			if !ok {
				continue
			}
			delete(pendingTool, evt.Message.ToolCallID)
			if t.name == "plan_update" || t.name == "enter_plan_mode" {
				continue
			}
			output := evt.Message.Content
			status := "completed"
			if strings.HasPrefix(output, "error: ") {
				status = "failed"
			}
			if idx, ok := toolCallIdx[evt.Message.ToolCallID]; ok {
				toolCalls[idx].status = status
				toolCalls[idx].output = output
			}
			flushRunCard()

		case openagent.StreamRetrying:
			if evt.Error != nil {
				lastErr = fmt.Sprintf("retrying: %v", evt.Error)
			} else {
				lastErr = "retrying..."
			}
			flushRunCard()

		case openagent.StreamDone:
			stage = stageDone
			flushRunCard()
			pq.flush()

		case openagent.StreamError:
			stage = stageDone
			if evt.Error != nil {
				lastErr = fmt.Sprintf("error: %v", evt.Error)
			}
			flushRunCard()
			pq.stop()
			return

		case openagent.StreamAborted:
			stage = stageDone
			lastErr = "aborted"
			flushRunCard()
			pq.stop()
			return
		}
	}

	// Stream closed without StreamDone/Error/Aborted — finalize the card so
	// the title doesn't freeze on "回答中". If a terminal event already
	// set stageDone this is a harmless re-mark.
	if runCardID != "" && stage != stageDone {
		stage = stageDone
		flushRunCard()
	}
	pq.stop()
}

// ── Run card ──

// mkCard is a plain card builder used for standalone cards (plan_create).
func mkCard(title, body string, color channel.CardColor) *channel.Card {
	return &channel.Card{Header: channel.CardHeader{Title: title}, Content: body, Color: color}
}

// stage tracks the agent run's progress for the run card title.
type stage int

const (
	stageThinking    stage = iota // 🤔 思考中
	stageToolCalling              // 🔧 调用工具中
	stageAnswering                // 💬 回答中
	stageDone                     // ✅ 已完成
)

func (s stage) title() string {
	switch s {
	case stageThinking:
		return "🤔 思考中"
	case stageToolCalling:
		return "🔧 调用工具中"
	case stageAnswering:
		return "💬 回答中"
	case stageDone:
		return "✅ 已完成"
	}
	return "🤔 思考中"
}

func (s stage) color() channel.CardColor {
	if s == stageDone {
		return channel.CardColorGrey
	}
	return channel.CardColorYellow
}

// runCard builds the single card for an agent run. The body stacks three
// sections in fixed order: thinking (collapsed) → toolcalls (collapsed,
// nested) → answer (open). Empty sections are omitted. errMsg, when set,
// is appended at the end.
func runCard(s stage, thought string, calls []toolCallEntry, answer, errMsg string) *channel.Card {
	var panels []channel.Card

	if thought != "" {
		panels = append(panels, channel.Card{
			// Title left empty — subPanel falls back to a content preview.
			Content:   thought,
			Collapsed: true,
		})
	}
	if len(calls) > 0 {
		panels = append(panels, toolCallsSection(calls))
	}

	body := answer
	if errMsg != "" {
		if body != "" {
			body += "\n\n"
		}
		body += errMsg
	}

	return &channel.Card{
		Header:  channel.CardHeader{Title: s.title()},
		Color:   s.color(),
		Content: body,
		Panels:  panels,
	}
}

// ── Tool card ──

// toolCallEntry is the per-call state collected for the run card.
type toolCallEntry struct {
	name   string
	args   string
	status string // "in_progress" | "completed" | "failed"
	output string
}

// toolCallSubCard builds the inner collapsed Card for one tool call.
// Title is the tool name + status marker (no emoji); body is input + output.
func toolCallSubCard(e toolCallEntry) channel.Card {
	title := e.name
	switch e.status {
	case "completed":
		title = e.name + " ✓"
	case "failed":
		title = e.name + " ✗"
	}

	body := formatInput(e.name, e.args)
	if e.output != "" {
		body += "\n```\n" + e.output + "\n```"
	}

	return channel.Card{
		Header:    channel.CardHeader{Title: title},
		Content:   body,
		Collapsed: true,
	}
}

// toolCallsSection builds the collapsed sub-card for the toolcalls section.
// Title "toolcalls (N)"; expanding reveals one nested panel per call.
func toolCallsSection(entries []toolCallEntry) channel.Card {
	subs := make([]channel.Card, 0, len(entries))
	for _, e := range entries {
		subs = append(subs, toolCallSubCard(e))
	}
	return channel.Card{
		Header:    channel.CardHeader{Title: fmt.Sprintf("toolcalls (%d)", len(entries))},
		Collapsed: true,
		Panels:    subs,
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
	case "websearch":
		if q := jsonStr(m, "query"); q != "" {
			return "`" + q + "`"
		}
	case "webfetch":
		if u := jsonStr(m, "url"); u != "" {
			return "`" + u + "`"
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
	case "websearch":
		return "🌐"
	case "webfetch":
		return "🔗"
	case "recall":
		return "🧠"
	case "load_skill":
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
