package rest

import (
	"time"
)

// FeishuPhase is the connection state machine phase of the feishu
// channel.
type FeishuPhase string

const (
	// FeishuIdle: never connected in this process.
	FeishuIdle FeishuPhase = "idle"
	// FeishuConnecting: flow started (WebSocket handshake / QR
	// registration in flight). The SDK exposes no "connected" callback,
	// so this phase covers the whole live window of Start.
	FeishuConnecting FeishuPhase = "connecting"
	// FeishuConnected: the connection has been up (phase kept as
	// connected once Start is running without error so far).
	FeishuConnected FeishuPhase = "connected"
	// FeishuDisconnected: connection ended (error or Disconnect).
	FeishuDisconnected FeishuPhase = "disconnected"
)

// FeishuStatus is the queryable state of the feishu channel, returned by
// GET /api/channels/feishu/status. The connection is a process-level
// daemon — the status survives page refreshes and is independent of any
// browser tab.
type FeishuStatus struct {
	Phase          FeishuPhase `json:"phase"`
	Connected      bool        `json:"connected"` // convenience: phase == connected
	AppID          string      `json:"app_id,omitempty"`
	CredentialFrom string      `json:"credential_from,omitempty"` // settings | profile | registered
	// ConnectedAt is the last successful connection time (nil when never
	// connected). Pointer so the zero time is omitted from JSON.
	ConnectedAt *time.Time `json:"connected_at,omitempty"`
	LastError   string     `json:"last_error,omitempty"`
}

// FeishuChannel is the process-level feishu connection manager as seen
// by the REST layer. It is implemented in cmd/cli/server (where the
// flock, credential resolution, and QR registration live); the interface
// keeps rest free of an import cycle (server imports rest).
type FeishuChannel interface {
	// Status returns the current connection state.
	Status() FeishuStatus

	// ConnectAsync starts the connection flow. It returns immediately
	// and the connection runs on the PROCESS context (not the caller's
	// request context) — the HTTP handler returning must never tear the
	// connection down.
	//
	// registrationStarted is true when the flow entered QR registration:
	// the QR URL is then delivered via onQR shortly after (nil = render
	// in the terminal), and the connection completes once the user scans
	// it. When false, credentials existed and the connection flow was
	// started directly.
	ConnectAsync(onQR func(url string, expireIn int)) (registrationStarted bool, err error)

	// Subscribe registers a callback invoked on every state change
	// (connecting / connected / disconnected / error).
	Subscribe(fn func(FeishuStatus))
}
