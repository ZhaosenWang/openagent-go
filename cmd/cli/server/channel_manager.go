package server

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	openagent "github.com/yusheng-g/openagent-go"
	"github.com/yusheng-g/openagent-go/agent"
	"github.com/yusheng-g/openagent-go/channel"
	"github.com/yusheng-g/openagent-go/channel/feishu"
	"github.com/yusheng-g/openagent-go/cmd/cli/config"
	"github.com/yusheng-g/openagent-go/kernel"
	"github.com/yusheng-g/openagent-go/rest"
)

// FeishuManager owns the feishu connection lifecycle. One instance per
// process; three entry points share it:
//
//  1. --channel feishu flag      — connect immediately at startup
//  2. settings channels.feishu   — configured = enabled, connect at startup
//  3. POST /api/channels/feishu/connect — frontend-triggered
//
// The connection is a process-level daemon: it runs on the base context
// (the serve process ctx), NEVER on an HTTP request context — the
// frontend only triggers and observes (status + SSE events); closing a
// page or a handler returning never affects it. The machine-level flock
// guarantees a single live connection per profile — a second instance
// fails fast instead of silently stealing events from the first.
type FeishuManager struct {
	baseCtx   context.Context
	profiles  string
	cfg       *agent.Agent
	deps      kernel.Deps
	feishuCfg *config.FeishuConfig // settings.json channels.feishu (may be nil)

	mu     sync.Mutex
	lock   *ChannelLock
	status rest.FeishuStatus
	cancel context.CancelFunc
	subs   []func(rest.FeishuStatus)
}

var _ rest.FeishuChannel = (*FeishuManager)(nil)

// NewFeishuManager creates the process-level feishu connection manager.
// baseCtx is the serve process context — the connection and the QR
// registration run on it, so neither is torn down by an HTTP request
// returning. feishuCfg is the settings.json channels.feishu block (nil
// when the user did not configure credentials — the manager then falls
// back to the profile credential file or the QR registration flow).
func NewFeishuManager(baseCtx context.Context, profiles string, feishuCfg *config.FeishuConfig, cfg *agent.Agent, deps kernel.Deps) *FeishuManager {
	return &FeishuManager{
		baseCtx:   baseCtx,
		profiles:  profiles,
		feishuCfg: feishuCfg,
		cfg:       cfg,
		deps:      deps,
		status:    rest.FeishuStatus{Phase: rest.FeishuIdle},
	}
}

// Status returns a snapshot of the current connection state.
func (m *FeishuManager) Status() rest.FeishuStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status
}

// Subscribe registers a callback invoked on every state change
// (connecting / connected / disconnected / error). Used by the REST
// layer to emit feishu.status events. Callbacks run on the caller's
// goroutine — keep them quick.
func (m *FeishuManager) Subscribe(fn func(rest.FeishuStatus)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.subs = append(m.subs, fn)
}

func (m *FeishuManager) setStatus(s rest.FeishuStatus) {
	s.Connected = s.Phase == rest.FeishuConnected
	m.mu.Lock()
	m.status = s
	subs := append([]func(rest.FeishuStatus){}, m.subs...)
	m.mu.Unlock()
	for _, fn := range subs {
		fn(s)
	}
}

// Connect establishes the feishu connection (idempotent: a live
// connection returns nil) and returns once the flow is started. When
// registration is needed the QR code is rendered to the terminal (the
// no-frontend entry point) and the connection completes asynchronously
// once the user scans it. Frontend callers use ConnectAsync with their
// own onQR callback instead.
func (m *FeishuManager) Connect() error {
	_, err := m.ConnectAsync(nil)
	return err
}

// ConnectAsync starts the feishu connection flow. Resolution order for
// credentials: settings.json → profile credential file → QR registration.
//
// Returns immediately; the flow runs on the process base context, so
// the caller (an HTTP handler) returning does not tear it down.
//
// The machine lock is taken for the whole connection lifetime and
// released on Disconnect / process exit. When another instance holds
// the lock the returned error is a fail-fast signal — the caller
// decides whether to abort the process or continue without the channel.
//
// onQR receives the registration QR URL when the flow has to register a
// new app (nil = render the QR in the terminal). registrationStarted is
// true in that case; the connection completes asynchronously once the
// user scans the QR — callers observe it via Status / Subscribe.
func (m *FeishuManager) ConnectAsync(onQR func(url string, expireIn int)) (bool, error) {
	m.mu.Lock()
	phase := m.status.Phase
	m.mu.Unlock()
	switch phase {
	case rest.FeishuConnecting, rest.FeishuConnected:
		return false, nil // already starting / connected (idempotent)
	}

	// Machine-level single instance: a second connection to the same
	// app would silently steal events from the first.
	lock, err := AcquireChannelLock(m.profiles, "feishu")
	if err != nil {
		return false, err
	}

	// Resolve credentials: settings.json wins, then the profile
	// credential file, then QR registration.
	if m.feishuCfg != nil && m.feishuCfg.AppID != "" && m.feishuCfg.AppSecret != "" {
		m.startConnection(lock, FeishuCredentials{AppID: m.feishuCfg.AppID, AppSecret: m.feishuCfg.AppSecret}, "settings")
		return false, nil
	}
	if c, ok := loadFeishuAppFile(m.profiles); ok {
		m.startConnection(lock, c, "profile")
		return false, nil
	}

	// No persisted credentials — QR registration. Runs on the process
	// base context (not the caller's request context): the HTTP handler
	// must be able to return with the QR URL while the user scans it.
	m.setStatus(rest.FeishuStatus{Phase: rest.FeishuConnecting, CredentialFrom: "registering"})
	go func() {
		reg, rerr := ResolveFeishuCredentials(m.baseCtx, m.profiles, onQR)
		if rerr != nil {
			lock.Release()
			m.setStatus(rest.FeishuStatus{Phase: rest.FeishuDisconnected, LastError: fmt.Sprintf("feishu: %v", rerr)})
			slog.Error("feishu: registration failed", "error", rerr)
			return
		}
		m.startConnection(lock, reg, "registered")
	}()
	return true, nil
}

// startConnection launches the connection goroutine on the process base
// context and marks the state. The caller owns lock.
func (m *FeishuManager) startConnection(lock *ChannelLock, creds FeishuCredentials, from string) {
	connCtx, cancel := context.WithCancel(m.baseCtx)
	m.mu.Lock()
	m.lock = lock
	m.cancel = cancel
	m.mu.Unlock()
	m.setStatus(rest.FeishuStatus{Phase: rest.FeishuConnecting, AppID: creds.AppID, CredentialFrom: from})

	go func() {
		// The connection blocks until the process context is cancelled
		// or the connection is permanently lost; the SDK reconnects on
		// transient failures internally.
		ch := feishu.New(creds.AppID, creds.AppSecret)
		ch.SetOnReady(func() {
			// The SDK flips to ready after the WebSocket connects (and
			// after every reconnect) — this is the only place the
			// connected state is observable, since Start() blocks for
			// the whole connection lifetime.
			now := time.Now()
			m.setStatus(rest.FeishuStatus{Phase: rest.FeishuConnected, AppID: creds.AppID, CredentialFrom: from, ConnectedAt: &now})
		})
		ch.SetOnReconnecting(func() {
			// Auto-reconnect kicked in after a drop: the frontend must
			// see "connecting" (not a stale "connected") while the SDK
			// is re-establishing the WebSocket.
			m.setStatus(rest.FeishuStatus{Phase: rest.FeishuConnecting, AppID: creds.AppID, CredentialFrom: from})
		})
		err := ch.Start(connCtx, feishuMessageHandler(m.cfg, m.deps))
		lock.Release()
		m.mu.Lock()
		m.lock = nil
		m.cancel = nil
		m.mu.Unlock()
		m.setStatus(rest.FeishuStatus{Phase: rest.FeishuDisconnected, AppID: creds.AppID, LastError: errString(err)})
		slog.Warn("feishu: connection closed", "error", err)
	}()
}

// Disconnect tears down the feishu connection. State flips to
// disconnected immediately (a subsequent Connect must not short-circuit
// on a stale "connected"); the connection goroutine releases the machine
// lock as it exits.
func (m *FeishuManager) Disconnect() {
	m.mu.Lock()
	cancel := m.cancel
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	m.setStatus(rest.FeishuStatus{Phase: rest.FeishuDisconnected})
}

// feishuMessageHandler routes incoming Feishu messages to the agent,
// one ephemeral run per message.
func feishuMessageHandler(cfg *agent.Agent, deps kernel.Deps) channel.MessageHandler {
	return func(msgCtx context.Context, msg channel.IncomingMessage, reply channel.ReplyFunc) {
		sessionID := "feishu_" + msg.ChatID
		go func() {
			// Carry the resolved Model instance so downstream consumers
			// (RunHooks via SessionFromContext, e.g. the artifact hook's
			// context-window threshold) read the same model the runner
			// uses.
			session := openagent.Session{
				ID:        sessionID,
				Model:     cfg.Model,
				CreatedAt: time.Now(),
			}
			stream := kernel.New(cfg, deps).RunStream(msgCtx, session, openagent.UserMessage(msg.Text))
			streamReply(reply, stream)
		}()
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
