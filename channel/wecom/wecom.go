package wecom

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/yusheng-g/openagent-go/channel"
)

func cryptoRandRead(b []byte) (int, error) { return rand.Read(b) }

// Channel implements channel.Channel for WeCom smart robots over the
// official WebSocket long connection.
//
// Connection: dial wss://openws.work.weixin.qq.com, send aibot_subscribe
// (bot_id + secret) — the connection then stays open; the server pushes
// aibot_msg_callback / aibot_event_callback frames and answers ping.
// The client must ping every ~30s to keep the connection alive.
//
// Replies use the streaming mechanism: one stream.id = one message that
// can be refreshed until finish=true (the message the user sees grows in
// place — WeCom supports true streaming, unlike personal WeChat).
type Channel struct {
	botID  string
	secret string

	conn   *websocket.Conn
	mu     sync.Mutex // guards conn writes
	closed bool

	onReady        func()
	onReconnecting func()
	onError        func(err error)
}

// New returns a WeCom Channel bound to the robot credentials. Must be
// started via Start().
func New(botID, secret string) *Channel {
	return &Channel{botID: botID, secret: secret}
}

// SetOnReady registers the ready callback (nil clears it). Fired once
// the subscribe handshake succeeds.
func (c *Channel) SetOnReady(f func()) { c.onReady = f }

// SetOnReconnecting registers the reconnecting callback (nil clears it).
// Fired when the connection drops and the loop reconnects.
func (c *Channel) SetOnReconnecting(f func()) { c.onReconnecting = f }

// SetOnError registers the error callback (nil clears it). Fired on
// connection/subscribe failures; used by the manager to surface
// first-connect failures (bad credentials).
func (c *Channel) SetOnError(f func(err error)) { c.onError = f }

// Name implements channel.Channel.
func (c *Channel) Name() string { return "wecom" }

// Stop implements channel.Channel. Closes the underlying WebSocket —
// this wakes a ReadMessage blocked in Start (gorilla's ReadMessage does
// NOT respond to context cancellation, so cancelling the Start context
// alone would leave the connection goroutine stuck forever, holding the
// machine lock).
func (c *Channel) Stop() error {
	c.closeConn()
	return nil
}

// Start implements channel.Channel. Connects, subscribes, and runs the
// read loop (plus heartbeats) until ctx is cancelled or the connection
// is permanently lost. Reconnects automatically with backoff on drops.
func (c *Channel) Start(ctx context.Context, handler channel.MessageHandler) error {
	retryDelay := time.Second
	everReady := false
	for {
		err := c.runOnce(ctx, handler, &everReady)
		if err == nil || ctx.Err() != nil {
			return nil // clean shutdown
		}
		if !everReady && c.onError != nil {
			c.onError(err)
		}
		if everReady && c.onReconnecting != nil {
			c.onReconnecting()
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(retryDelay):
		}
		retryDelay = min(retryDelay*2, 10*time.Second)
	}
}

// runOnce is one connection lifetime: dial → subscribe → read loop →
// heartbeat until the connection drops or ctx is cancelled.
func (c *Channel) runOnce(ctx context.Context, handler channel.MessageHandler, everReady *bool) error {
	c.mu.Lock()
	c.closed = false
	c.mu.Unlock()
	defer c.closeConn()

	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, _, err := dialer.DialContext(ctx, wsEndpoint, nil)
	if err != nil {
		return fmt.Errorf("wecom dial: %w", err)
	}
	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()

	// Subscribe with a fresh req_id.
	subID := newReqID()
	sub := Frame{Cmd: cmdSubscribe, Headers: FrameHeaders{ReqID: subID},
		Body: mustJSON(SubscribeBody{BotID: c.botID, Secret: c.secret})}
	if err := c.writeJSON(sub); err != nil {
		return fmt.Errorf("wecom subscribe: %w", err)
	}

	// The first frame must be the subscribe ack (errcode 0). The ack
	// frame has NO "cmd" field (only headers/errcode/errmsg) — it is
	// simply the first frame after subscribe, so we parse it directly
	// instead of matching on cmd.
	if err := c.expectSubscribeAck(); err != nil {
		return err
	}
	if !*everReady {
		*everReady = true
		if c.onReady != nil {
			c.onReady()
		}
	}

	// Heartbeat every 30s (server drops silent connections).
	hbCtx, hbCancel := context.WithCancel(ctx)
	defer hbCancel()
	go c.heartbeat(hbCtx)

	// Context watcher: cancelling the Start context must terminate the
	// read loop — gorilla's ReadMessage blocks forever otherwise (a
	// disconnected manager would leave the connection goroutine stuck,
	// holding the machine lock). Closing the connection wakes it.
	watchDone := make(chan struct{})
	defer close(watchDone)
	go func() {
		select {
		case <-ctx.Done():
			c.closeConn()
		case <-watchDone:
		}
	}()

	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("wecom read: %w", err)
		}
		if err := c.handleFrame(raw, handler); err != nil {
			return err
		}
	}
}

// expectSubscribeAck waits for the subscribe ack — the first frame after
// aibot_subscribe. The ack has no "cmd" field, so the frame is parsed as
// an Ack directly (a subscribe rejection surfaces as errcode != 0).
func (c *Channel) expectSubscribeAck() error {
	_, raw, err := c.readMessage()
	if err != nil {
		return fmt.Errorf("wecom subscribe ack: %w", err)
	}
	slog.Debug("wecom: subscribe ack frame", "raw", string(raw))
	var ack Ack
	if err := json.Unmarshal(raw, &ack); err != nil {
		return fmt.Errorf("wecom subscribe ack decode: %w (raw=%s)", err, raw)
	}
	if ack.ErrCode != 0 {
		return fmt.Errorf("wecom subscribe rejected: errcode=%d errmsg=%s", ack.ErrCode, ack.ErrMsg)
	}
	return nil
}

// handleFrame dispatches one server frame.
func (c *Channel) handleFrame(raw []byte, handler channel.MessageHandler) error {
	var f Frame
	if err := json.Unmarshal(raw, &f); err != nil {
		slog.Warn("wecom: malformed frame", "raw", string(raw))
		return nil // malformed frame — ignore
	}
	switch f.Cmd {
	case cmdMsgCallback:
		var body MsgCallbackBody
		if err := json.Unmarshal(f.Body, &body); err != nil {
			slog.Warn("wecom: msg_callback decode failed", "body", string(f.Body))
			return nil
		}
		slog.Info("wecom: message received", "msgid", body.MsgID, "msgtype", body.MsgType,
			"chattype", body.ChatType, "from", body.From.UserID, "req_id", f.Headers.ReqID)
		msg := toIncoming(&body)
		if msg == nil {
			slog.Warn("wecom: message dropped by toIncoming",
				"msgtype", body.MsgType, "has_text", body.Text != nil, "content", textPreview(&body))
			return nil
		}
		handler(context.Background(), *msg, c.buildReply(f.Headers.ReqID, &body))
		return nil

	case cmdEventCallback:
		// v1: acknowledge interaction events (enter_chat etc.) without
		// acting on them. Acknowledging keeps the server's state clean.
		slog.Debug("wecom: event callback", "req_id", f.Headers.ReqID, "body", string(f.Body))
		return c.writeJSON(Frame{Cmd: cmdPong, Headers: FrameHeaders{ReqID: f.Headers.ReqID}})

	case cmdPing:
		// Answer pings immediately (keep-alive from the server side).
		return c.writeJSON(Frame{Cmd: cmdPong, Headers: FrameHeaders{ReqID: f.Headers.ReqID}})

	default:
		slog.Debug("wecom: unhandled frame", "cmd", f.Cmd)
		return nil
	}
}

// textPreview extracts a short content preview for diagnostics.
func textPreview(body *MsgCallbackBody) string {
	if body.Text != nil {
		s := body.Text.Content
		if len(s) > 40 {
			return s[:40] + "..."
		}
		return s
	}
	return ""
}

// heartbeat sends a ping every 30s until ctx is cancelled.
func (c *Channel) heartbeat(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = c.writeJSON(Frame{Cmd: cmdPing, Headers: FrameHeaders{ReqID: newReqID()}})
		}
	}
}

// ── Reply ──

// FinishMarker is the UpdateID sentinel that ends a streaming message:
// reply(ReplyMessage{UpdateID: FinishMarker, Text: final}) sends
// finish=true so the message can no longer be refreshed. Any other
// UpdateID refreshes the stream created by the first call.
const FinishMarker = "~finish"

// buildReply returns a channel.ReplyFunc using the WeCom streaming
// mechanism — one stream.id is one message that grows in place:
//
//   - first call (no UpdateID): creates the stream message (finish=false)
//   - calls with msg.UpdateID == the returned id: refresh that message
//   - call with msg.UpdateID == FinishMarker: ends it (finish=true)
//
// reqID is the callback's req_id — echoed verbatim in every reply.
func (c *Channel) buildReply(reqID string, cb *MsgCallbackBody) channel.ReplyFunc {
	var streamID string
	var mu sync.Mutex

	return func(ctx context.Context, msg channel.ReplyMessage) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if msg.UpdateID == FinishMarker {
			// Terminal: finish=true. A stream that never had an update is
			// created-and-finished in one shot (content is complete).
			if streamID == "" {
				streamID = newReqID()
			}
			body := StreamReplyBody{
				MsgType: "stream",
				Stream:  StreamItem{ID: streamID, Finish: true, Content: msg.Text},
			}
			if err := c.writeJSON(Frame{Cmd: cmdRespondMsg, Headers: FrameHeaders{ReqID: reqID}, Body: mustJSON(body)}); err != nil {
				return "", err
			}
			return streamID, nil
		}
		if streamID == "" {
			streamID = newReqID()
		}
		if msg.UpdateID != "" && msg.UpdateID != streamID {
			streamID = msg.UpdateID
		}
		body := StreamReplyBody{
			MsgType: "stream",
			Stream: StreamItem{
				ID:      streamID,
				Finish:  false,
				Content: msg.Text,
			},
		}
		if err := c.writeJSON(Frame{Cmd: cmdRespondMsg, Headers: FrameHeaders{ReqID: reqID}, Body: mustJSON(body)}); err != nil {
			return "", err
		}
		return streamID, nil
	}
}

// ── Internal ──

func (c *Channel) writeJSON(v any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil || c.closed {
		return fmt.Errorf("wecom: connection closed")
	}
	return c.conn.WriteJSON(v)
}

func (c *Channel) readMessage() (int, []byte, error) {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return 0, nil, fmt.Errorf("wecom: no connection")
	}
	return conn.ReadMessage()
}

func (c *Channel) closeConn() {
	c.mu.Lock()
	c.closed = true
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
	}
	c.mu.Unlock()
}

// toIncoming converts a message callback to a channel.IncomingMessage.
// Text messages only for now (media handling is a later iteration).
func toIncoming(body *MsgCallbackBody) *channel.IncomingMessage {
	if body.MsgType != "text" || body.Text == nil {
		return nil
	}
	chatType := "private"
	if body.ChatType == "group" {
		chatType = "group"
	}
	// chatid is only present for GROUP chats — a single chat must key its
	// conversation on the sender (otherwise every single-chat user would
	// share one session and their histories would bleed into each other).
	chatID := body.ChatID
	if chatID == "" {
		chatID = body.From.UserID
	}
	text := body.Text.Content
	if chatType == "group" {
		// Group messages arrive with the @-mention prefix — strip it so
		// the agent sees the actual question.
		text = stripMention(text)
	}
	return &channel.IncomingMessage{
		ID:       body.MsgID,
		ChatID:   chatID,
		ChatType: chatType,
		UserID:   body.From.UserID,
		UserName: body.From.UserID, // wire carries no display name
		Text:     text,
		Raw:      body,
	}
}

// stripMention removes a leading "@<bot>" mention (e.g. "@RobotA hello").
func stripMention(text string) string {
	t := strings.TrimSpace(text)
	if !strings.HasPrefix(t, "@") {
		return text
	}
	rest := strings.TrimSpace(t[1:])
	if i := strings.IndexAny(rest, " \t\n"); i >= 0 {
		return strings.TrimSpace(rest[i+1:])
	}
	return "" // only a mention, no content
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // marshal of plain structs cannot fail
	}
	return b
}

func newReqID() string {
	var b [16]byte
	_, _ = cryptoRandRead(b[:])
	return fmt.Sprintf("%x", b)
}

func min(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
