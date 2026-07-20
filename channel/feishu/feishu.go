// Package feishu implements channel.Channel for Feishu (Lark) via
// WebSocket long connection using the official larksuite SDK v3.
//
// The SDK handles authentication, keep-alive, reconnection, and event
// deserialization automatically. This package only needs to register
// event handlers and normalize messages for the Agent.
package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"

	"github.com/yusheng-g/openagent-go/channel"
)

// Channel implements channel.Channel for Feishu via WebSocket long connection.
type Channel struct {
	appID     string
	appSecret string

	client *lark.Client
	ws     *larkws.Client
	once   sync.Once
}

// New returns a Feishu Channel. The Channel must be started via Start() to
// begin receiving messages.
func New(appID, appSecret string) *Channel {
	return &Channel{appID: appID, appSecret: appSecret}
}

// Name implements channel.Channel.
func (c *Channel) Name() string { return "feishu" }

// Start implements channel.Channel. It opens a WebSocket connection to
// Feishu and blocks until ctx is cancelled.
func (c *Channel) Start(ctx context.Context, handler channel.MessageHandler) error {
	c.client = lark.NewClient(c.appID, c.appSecret)

	dh := dispatcher.NewEventDispatcher("", "").
		OnP2MessageReceiveV1(func(ctx context.Context, event *larkim.P2MessageReceiveV1) error {
			msg := toIncoming(event)
			if msg == nil {
				return nil
			}
			handler(ctx, *msg, c.buildReply(ctx, event))
			return nil
		})

	c.ws = larkws.NewClient(c.appID, c.appSecret,
		larkws.WithEventHandler(dh),
		larkws.WithLogLevel(larkcore.LogLevelError),
	)

	return c.ws.Start(ctx)
}

// Stop implements channel.Channel.
func (c *Channel) Stop() error {
	return nil
}

// ── Normalization ──

// toIncoming converts a Feishu message event to a channel.IncomingMessage.
// Returns nil if the message should be ignored.
func toIncoming(event *larkim.P2MessageReceiveV1) *channel.IncomingMessage {
	if event == nil || event.Event == nil || event.Event.Message == nil {
		return nil
	}
	msg := event.Event.Message

	text := extractText(msg)
	if strings.TrimSpace(text) == "" {
		return nil
	}

	chatType := "private"
	if msg.ChatType != nil {
		chatType = *msg.ChatType
	}

	chatID := ""
	if msg.ChatId != nil {
		chatID = *msg.ChatId
	}

	userID, userName := extractSender(event)

	msgID := ""
	if msg.MessageId != nil {
		msgID = *msg.MessageId
	}

	var mentions []string
	if msg.Mentions != nil {
		for _, m := range msg.Mentions {
			if m.Id != nil {
				if m.Id.OpenId != nil {
					mentions = append(mentions, *m.Id.OpenId)
				} else if m.Id.UnionId != nil {
					mentions = append(mentions, *m.Id.UnionId)
				} else if m.Id.UserId != nil {
					mentions = append(mentions, *m.Id.UserId)
				}
			}
		}
	}

	return &channel.IncomingMessage{
		ID:       msgID,
		ChatID:   chatID,
		ChatType: chatType,
		UserID:   userID,
		UserName: userName,
		Text:     text,
		Mentions: mentions,
		Raw:      event,
	}
}

// extractSender pulls user ID and display name from the event sender.
func extractSender(event *larkim.P2MessageReceiveV1) (string, string) {
	if event.Event.Sender == nil || event.Event.Sender.SenderId == nil {
		return "", ""
	}
	sid := event.Event.Sender.SenderId
	switch {
	case sid.OpenId != nil:
		return *sid.OpenId, *sid.OpenId
	case sid.UnionId != nil:
		return *sid.UnionId, *sid.UnionId
	case sid.UserId != nil:
		return *sid.UserId, *sid.UserId
	}
	return "", ""
}

// extractText pulls plain text from a Feishu EventMessage.
// EventMessage.Content holds a JSON string with the actual message body.
// For text messages the JSON shape is {"text":"..."}.
func extractText(msg *larkim.EventMessage) string {
	if msg == nil || msg.Content == nil {
		return ""
	}
	raw := *msg.Content
	if raw == "" {
		return ""
	}

	// Try to extract the "text" key from the JSON content.
	var body struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(raw), &body); err == nil && body.Text != "" {
		return strings.TrimSpace(body.Text)
	}

	// Fallback: return the raw content (might be non-text message).
	return strings.TrimSpace(raw)
}

// ── Reply ──

// buildReply returns a channel.ReplyFunc that sends a message back
// to the same chat. If UpdateID is set the channel patches the existing
// card; otherwise a new message is sent. Returns the platform message ID.
func (c *Channel) buildReply(ctx context.Context, event *larkim.P2MessageReceiveV1) channel.ReplyFunc {
	return func(replyCtx context.Context, msg channel.ReplyMessage) (string, error) {
		if c.client == nil {
			return "", fmt.Errorf("feishu: client not initialized")
		}

		received := event.Event.Message
		if received == nil {
			return "", fmt.Errorf("feishu: no message in event")
		}

		receiveIDType, receiveID := resolveReceive(received, event)
		if receiveID == "" {
			return "", fmt.Errorf("feishu: cannot determine receive ID")
		}

		// Card update: patch existing card.
		if msg.UpdateID != "" && msg.Card != nil {
			cardJSON, err := BuildCard(msg.Card)
			if err != nil {
				return "", err
			}
			return msg.UpdateID, c.patchCard(replyCtx, msg.UpdateID, cardJSON)
		}

		if msg.Card != nil {
			return c.sendCard(replyCtx, receiveIDType, receiveID, msg.Card)
		}
		return c.sendText(replyCtx, receiveIDType, receiveID, msg.Text)
	}
}

func resolveReceive(msg *larkim.EventMessage, event *larkim.P2MessageReceiveV1) (receiveIDType, receiveID string) {
	if msg.ChatType != nil && *msg.ChatType == "group" && msg.ChatId != nil {
		receiveIDType = "chat_id"
		receiveID = *msg.ChatId
	} else {
		receiveIDType = "open_id"
		userID, _ := extractSender(event)
		receiveID = userID
	}
	return
}

func (c *Channel) sendText(ctx context.Context, receiveIDType, receiveID, text string) (string, error) {
	if text == "" {
		return "", nil
	}
	content := fmt.Sprintf(`{"text":"%s"}`, escapeJSON(text))
	return c.sendMessage(ctx, receiveIDType, receiveID, "text", content)
}

// escapeJSON escapes a string for safe embedding in a JSON string.
func escapeJSON(s string) string {
	return strings.NewReplacer(
		`\`, `\\`,
		`"`, `\"`,
		"\n", `\n`,
		"\r", `\r`,
		"\t", `\t`,
	).Replace(s)
}

func (c *Channel) sendCard(ctx context.Context, receiveIDType, receiveID string, card *channel.Card) (string, error) {
	cardJSON, err := BuildCard(card)
	if err != nil {
		return "", fmt.Errorf("feishu: build card: %w", err)
	}
	if cardJSON == "" {
		return "", nil
	}
	return c.sendMessage(ctx, receiveIDType, receiveID, "interactive", cardJSON)
}

func (c *Channel) sendMessage(ctx context.Context, receiveIDType, receiveID, msgType, content string) (string, error) {
	resp, err := c.client.Im.Message.Create(ctx,
		larkim.NewCreateMessageReqBuilder().
			ReceiveIdType(receiveIDType).
			Body(larkim.NewCreateMessageReqBodyBuilder().
				MsgType(msgType).
				ReceiveId(receiveID).
				Content(content).
				Build()).
			Build())
	if err != nil {
		return "", fmt.Errorf("feishu: send message: %w", err)
	}
	if !resp.Success() {
		return "", fmt.Errorf("feishu: send message failed: code=%d msg=%s", resp.Code, resp.Msg)
	}
	if resp.Data != nil && resp.Data.MessageId != nil {
		return *resp.Data.MessageId, nil
	}
	return "", nil
}

// patchCard updates an existing interactive card message by message ID.
// https://open.feishu.cn/document/server-docs/im-v1/message/patch
func (c *Channel) patchCard(ctx context.Context, messageID, cardJSON string) error {
	content := cardJSON // PATCH body just needs the new card content
	resp, err := c.client.Im.Message.Patch(ctx,
		larkim.NewPatchMessageReqBuilder().
			MessageId(messageID).
			Body(larkim.NewPatchMessageReqBodyBuilder().
				Content(content).
				Build()).
			Build())
	if err != nil {
		return fmt.Errorf("feishu: patch card: %w", err)
	}
	if !resp.Success() {
		return fmt.Errorf("feishu: patch card failed: code=%d msg=%s", resp.Code, resp.Msg)
	}
	return nil
}
