// Package channel defines the interface for IM platform adapters.
//
// A Channel connects to an IM platform (Feishu, WeChat Work, DingTalk, etc.),
// receives incoming messages, normalizes them to IncomingMessage, and forwards
// them to a MessageHandler. Replies are sent back through ReplyFunc.
//
// Sub-packages (channel/feishu, channel/wecom, etc.) provide concrete
// implementations for each platform. The CLI layer wires channels to the
// Agent — Channel itself knows nothing about LLM, Memory, or sessions.
package channel

import (
	"context"
	"time"
)

// Channel is an IM platform adapter.
type Channel interface {
	// Name returns a human-readable label (e.g. "feishu", "wecom").
	Name() string

	// Start connects to the IM platform and begins forwarding incoming
	// messages to handler. It blocks until ctx is cancelled or the
	// connection is permanently lost.
	//
	// The handler is called in the same goroutine as the platform event
	// loop. Long-running work (e.g. agent.Run) MUST be launched in a
	// separate goroutine to avoid blocking the transport.
	Start(ctx context.Context, handler MessageHandler) error

	// Stop gracefully shuts down the channel. After Stop returns, no
	// further calls to the handler will be made.
	Stop() error
}

// MessageHandler receives normalized incoming messages. Every call happens
// in the transport goroutine — the handler is responsible for spawning its
// own goroutines for heavyweight work.
type MessageHandler func(ctx context.Context, msg IncomingMessage, reply ReplyFunc)

// ReplyFunc sends a response back to the original chat and returns the
// platform-assigned message ID. If ReplyMessage.UpdateID is set and Card
// is non-nil, the channel updates the existing card rather than sending
// a new message.
type ReplyFunc func(ctx context.Context, msg ReplyMessage) (string, error)

// ReplyMessage carries the content to send back to an IM user.
// If Card is set the channel renders it as a platform-specific card;
// otherwise the message is sent as plain text.
//
// When UpdateID is set and Card is non-nil, the channel patches the
// existing card identified by UpdateID instead of creating a new message.
// Text is ignored when UpdateID is set.
type ReplyMessage struct {
	Text     string
	Card     *Card
	UpdateID string // update existing card instead of creating new message
}

// Card is a platform-neutral structured message. Each channel translates
// it into the platform's native card format (e.g. Feishu interactive card,
// WeChat Work template card).
type Card struct {
	Header  CardHeader
	Content string   // markdown body
	Footer  string   // optional note at the bottom
	Color   CardColor
}

// CardHeader is the title area of a card.
type CardHeader struct {
	Title    string
	Subtitle string
}

// CardColor controls the header accent colour.
type CardColor string

const (
	CardColorBlue   CardColor = "blue"
	CardColorGreen  CardColor = "green"
	CardColorRed    CardColor = "red"
	CardColorYellow CardColor = "yellow"
	CardColorOrange CardColor = "orange"
	CardColorPurple CardColor = "purple"
	CardColorGrey   CardColor = "grey"
)

// IncomingMessage is a platform-neutral message received from an IM channel.
type IncomingMessage struct {
	ID        string    // platform message ID
	ChatID    string    // conversation identifier (group chat or private chat)
	ChatType  string    // "group" or "private"
	UserID    string    // sender identifier
	UserName  string    // sender display name
	Text      string    // plain text content (mentions stripped)
	Mentions  []string  // IDs mentioned in the message
	Timestamp time.Time // when the message was sent
	Raw       any       // original platform event, nil if not needed
}
