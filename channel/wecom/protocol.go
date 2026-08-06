package wecom

import "encoding/json"

// WS endpoint for the smart-robot long connection.
const wsEndpoint = "wss://openws.work.weixin.qq.com"

// Wire commands.
const (
	cmdSubscribe          = "aibot_subscribe"
	cmdMsgCallback        = "aibot_msg_callback"
	cmdEventCallback      = "aibot_event_callback"
	cmdRespondMsg         = "aibot_respond_msg"
	cmdRespondWelcomeMsg  = "aibot_respond_welcome_msg"
	cmdRespondUpdateMsg   = "aibot_respond_update_msg"
	cmdSendMsg            = "aibot_send_msg"
	cmdPing               = "ping"
	cmdPong               = "pong"
)

// Frame is the envelope for every WS message (requests and callbacks).
type Frame struct {
	Cmd     string          `json:"cmd"`
	Headers FrameHeaders    `json:"headers"`
	Body    json.RawMessage `json:"body,omitempty"`
}

// FrameHeaders carries the request id used to correlate callbacks and
// replies: EVERY reply to a callback must echo the callback's req_id.
type FrameHeaders struct {
	ReqID string `json:"req_id"`
}

// SubscribeBody is the aibot_subscribe request payload.
type SubscribeBody struct {
	BotID  string `json:"bot_id"`
	Secret string `json:"secret"`
}

// SubscribeResponse is the aibot_subscribe acknowledgment.
type SubscribeResponse struct {
	Headers FrameHeaders `json:"headers"`
	ErrCode int          `json:"errcode"`
	ErrMsg  string       `json:"errmsg"`
}

// MsgCallbackBody is the aibot_msg_callback payload (user message).
type MsgCallbackBody struct {
	MsgID    string    `json:"msgid"`
	BotID    string    `json:"aibotid"`
	ChatID   string    `json:"chatid"`
	ChatType string    `json:"chattype"` // "single" | "group"
	From     MsgFrom   `json:"from"`
	MsgType  string    `json:"msgtype"` // text | image | mixed | voice | file | video
	Text     *TextBody `json:"text,omitempty"`
}

// MsgFrom identifies the sender.
type MsgFrom struct {
	UserID string `json:"userid"`
}

// TextBody is the text message content.
type TextBody struct {
	Content string `json:"content"`
}

// EventCallbackBody is the aibot_event_callback payload (interaction
// events: enter_chat, template_card clicks, ...).
type EventCallbackBody struct {
	MsgID    string   `json:"msgid"`
	BotID    string   `json:"aibotid"`
	From     MsgFrom  `json:"from"`
	MsgType  string   `json:"msgtype"`
	Event    EventObj `json:"event"`
}

// EventObj carries the event discriminator.
type EventObj struct {
	EventType string `json:"eventtype"`
}

// StreamReplyBody is the aibot_respond_msg payload for a streaming text
// reply. The same stream.id refreshes one message; finish=true ends it.
type StreamReplyBody struct {
	MsgType string     `json:"msgtype"`
	Stream  StreamItem `json:"stream"`
}

// StreamItem is the stream message content.
type StreamItem struct {
	ID      string `json:"id"`
	Finish  bool   `json:"finish"`
	Content string `json:"content"`
}

// Ack is the common response envelope for subscribe and replies.
type Ack struct {
	Headers FrameHeaders `json:"headers"`
	ErrCode int          `json:"errcode"`
	ErrMsg  string       `json:"errmsg"`
}
