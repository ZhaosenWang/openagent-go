package rest

import (
	"errors"
	"time"
)

// ErrFeishuRegistrationInFlight marks a SetCredentials rejection while
// QR registration is in flight (the registration would overwrite the
// submitted values when it completes). Typed so handlers can map it to
// 409 without string matching.
var ErrFeishuRegistrationInFlight = errors.New("feishu: QR registration in progress — wait for it to finish or refresh")

// FeishuPhase is the connection state machine phase of the feishu
// channel.
type FeishuPhase string

const (
	// FeishuIdle: never connected in this process.
	FeishuIdle FeishuPhase = "idle"
	// FeishuRegistering: QR registration in flight (no credentials yet);
	// SetCredentials is rejected (409) during this phase.
	FeishuRegistering FeishuPhase = "registering"
	// FeishuConnecting: WebSocket flow started. The SDK exposes no
	// "connected" callback, so this phase covers the whole live window
	// of Start (plus auto-reconnect).
	FeishuConnecting FeishuPhase = "connecting"
	// FeishuConnected: the connection has been up.
	FeishuConnected FeishuPhase = "connected"
	// FeishuDisconnected: connection ended (error or Disconnect).
	FeishuDisconnected FeishuPhase = "disconnected"
)

// FeishuStatus is the queryable state of the feishu channel, returned by
// GET /api/channels/feishu/status. The connection is a process-level
// daemon — the status survives page refreshes and is independent of any
// browser tab.
type FeishuStatus struct {
	Phase     FeishuPhase `json:"phase"`
	Connected bool        `json:"connected"` // convenience: phase == connected
	AppID     string      `json:"app_id,omitempty"`
	// ConnectedAt is the last successful connection time (nil when never
	// connected). Pointer so the zero time is omitted from JSON.
	ConnectedAt *time.Time `json:"connected_at,omitempty"`
	LastError   string     `json:"last_error,omitempty"`
}

// FeishuChannel is the process-level feishu connection manager as seen
// by the CLI REST layer. Defined here (the consumer) per Go convention —
// cmd/cli/server implements it, which keeps the dependency acyclic
// (server → rest for HTTP wiring, never rest → server).
type FeishuChannel interface {
	// Status returns the current connection state.
	Status() FeishuStatus

	// ConnectAsync starts the connection flow (runs on the process
	// context, never the request context). onQR receives the QR URL when
	// registration is needed; returns registrationStarted.
	ConnectAsync(onQR func(url string, expireIn int)) (registrationStarted bool, err error)

	// Disconnect tears down the connection.
	Disconnect()

	// Credentials returns the currently effective credentials (from
	// settings — the single source). Empty values when none configured.
	Credentials() (appID, appSecret string)

	// SetCredentials stores the submitted credentials to settings.json
	// and applies them to the in-memory copy (next connect uses them).
	SetCredentials(appID, appSecret string) error

	// QR returns the cached registration QR (URL + base64 PNG image);
	// empty strings when no registration is in flight.
	QR() (url, imgBase64 string)
}
