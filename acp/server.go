// Package acp provides openagent Agent ↔ ACP protocol integration.
//
// AgentServer wraps an [openagent.Agent] as an [openacp.AgentHandler],
// implementing the full ACP v1 protocol lifecycle.
package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	openacp "github.com/yusheng-g/openagent-go/acp/sdk"
	openagent "github.com/yusheng-g/openagent-go"
	"github.com/yusheng-g/openagent-go/session"
)

// AgentServer wraps an [openagent.Agent] as an [openacp.AgentHandler].
//
// Usage:
//
//	srv := acp.NewAgentServer(agent, mem, sessionStore)
//	server := openacpsdk.NewServer("my-agent", "1.0.0", srv)
//	server.Run(ctx)
type AgentServer struct {
	Agent        *openagent.Agent
	Mem          openagent.Memory
	SessionStore session.Store

	mu       sync.Mutex
	sessions map[openacp.SessionId]*agentSession
	nextID   int64

	// clientRPC is set by the SDK mux via ClientRPCUser.
	clientRPC openacp.ClientRequester
}

// agentSession holds per-session runtime state.
type agentSession struct {
	id        openacp.SessionId
	cwd       string
	createdAt time.Time
	mode      string // "chat" or "plan"
	config    map[openacp.SessionConfigId]any // config option values
	cancel    context.CancelFunc
}

// NewAgentServer creates an AgentServer wrapping the given agent.
func NewAgentServer(agent *openagent.Agent, mem openagent.Memory, store session.Store) *AgentServer {
	return &AgentServer{
		Agent:        agent,
		Mem:          mem,
		SessionStore: store,
		sessions:     make(map[openacp.SessionId]*agentSession),
	}
}

// SetClientRequester implements [openacp.ClientRPCUser].
func (s *AgentServer) SetClientRequester(r openacp.ClientRequester) {
	s.clientRPC = r
}

var _ openacp.ClientRPCUser = (*AgentServer)(nil)
var _ openacp.AgentHandler = (*AgentServer)(nil)

// ── Session helpers ──

func (s *AgentServer) newSessionID() openacp.SessionId {
	s.mu.Lock()
	s.nextID++
	id := s.nextID
	s.mu.Unlock()
	return openacp.SessionId(fmt.Sprintf("acp_%d_%d", time.Now().UnixNano(), id))
}

func (s *AgentServer) saveMeta(id string, cwd string, kind string) {
	if s.SessionStore == nil {
		return
	}
	now := time.Now()
	info := session.SessionInfo{
		ID:        id,
		Cwd:       cwd,
		CreatedAt: now,
		UpdatedAt: now,
	}
	info.SetMeta("kind", kind)
	_ = s.SessionStore.Save(context.Background(), info)
}

func (s *AgentServer) putSession(id openacp.SessionId, ss *agentSession) {
	s.mu.Lock()
	s.sessions[id] = ss
	s.mu.Unlock()
}

func (s *AgentServer) getSession(id openacp.SessionId) *agentSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[id]
}

func (s *AgentServer) removeSession(id openacp.SessionId) {
	s.mu.Lock()
	ss := s.sessions[id]
	delete(s.sessions, id)
	s.mu.Unlock()
	if ss != nil && ss.cancel != nil {
		ss.cancel()
	}
}

// ── openacp.AgentHandler ──

func (s *AgentServer) OnInitialize(ctx context.Context, req openacp.InitializeRequest) (*openacp.InitializeResponse, error) {
	caps := openacp.AgentCapabilities{
		LoadSession: true,
		PromptCapabilities: openacp.PromptCapabilities{
			Image:           true,
			Audio:           false,
			EmbeddedContext: true,
		},
		McpCapabilities: openacp.McpCapabilities{
			HTTP: false,
			SSE:  false,
		},
		SessionCapabilities: openacp.SessionCapabilities{
			Close:  &openacp.SessionCloseCapabilities{},
			Delete: &openacp.SessionDeleteCapabilities{},
			List:   &openacp.SessionListCapabilities{},
			Resume: &openacp.SessionResumeCapabilities{},
		},
		Auth: openacp.AgentAuthCapabilities{
			Logout: &openacp.LogoutCapabilities{},
		},
	}
	return &openacp.InitializeResponse{
		ProtocolVersion:   1,
		AgentCapabilities: caps,
		AgentInfo: &openacp.Implementation{
			Name:    "openagent-acp",
			Version: "1.0.0",
		},
	}, nil
}

// ── Session CRUD ──

func (s *AgentServer) OnNewSession(ctx context.Context, req openacp.NewSessionRequest) (*openacp.NewSessionResponse, error) {
	id := s.newSessionID()
	ss := &agentSession{
		id:        id,
		cwd:       req.Cwd,
		createdAt: time.Now(),
		mode:      "chat",
		config:    map[openacp.SessionConfigId]any{"thought_level": "medium"},
	}
	s.putSession(id, ss)
	s.saveMeta(string(id), req.Cwd, "acp")

	return &openacp.NewSessionResponse{
		SessionID:     id,
		ConfigOptions: s.buildConfigOptions(id),
		Modes:         s.buildModeState(id),
	}, nil
}

func (s *AgentServer) OnLoadSession(ctx context.Context, req openacp.LoadSessionRequest, sender openacp.SessionEventSender) (*openacp.LoadSessionResponse, error) {
	ss := s.getSession(req.SessionID)
	if ss == nil {
		ss = &agentSession{
			id:        req.SessionID,
			cwd:       req.Cwd,
			createdAt: time.Now(),
			mode:      "chat",
		config:    map[openacp.SessionConfigId]any{"thought_level": "medium"},
		}
		s.putSession(req.SessionID, ss)
	}

	// Replay history from Memory if available.
	if s.Mem != nil {
		msgs, _ := s.Mem.Recent(ctx, string(req.SessionID), 200, 0)
		for i, msg := range msgs {
			mid := fmt.Sprintf("hist_%d", i)
			switch msg.Role {
			case openagent.RoleUser:
				sender.SendHistoryMessage("user_message_chunk", msg.Content, mid)
			case openagent.RoleAssistant:
				if msg.Content != "" {
					sender.SendHistoryMessage("agent_message_chunk", msg.Content, mid)
				}
			}
			// Tool calls and results are replayed below
		}
	}

	return &openacp.LoadSessionResponse{
		ConfigOptions: s.buildConfigOptions(req.SessionID),
		Modes:         s.buildModeState(req.SessionID),
	}, nil
}

func (s *AgentServer) OnResumeSession(ctx context.Context, req openacp.ResumeSessionRequest) (*openacp.ResumeSessionResponse, error) {
	if s.getSession(req.SessionID) == nil {
		ss := &agentSession{
			id:        req.SessionID,
			cwd:       req.Cwd,
			createdAt: time.Now(),
			mode:      "chat",
		config:    map[openacp.SessionConfigId]any{"thought_level": "medium"},
		}
		s.putSession(req.SessionID, ss)
	}
	return &openacp.ResumeSessionResponse{
		ConfigOptions: s.buildConfigOptions(req.SessionID),
		Modes:         s.buildModeState(req.SessionID),
	}, nil
}

func (s *AgentServer) OnCloseSession(ctx context.Context, req openacp.CloseSessionRequest) (*openacp.CloseSessionResponse, error) {
	s.removeSession(req.SessionID)
	if s.SessionStore != nil {
		_ = s.SessionStore.Delete(ctx, string(req.SessionID))
	}
	if s.Mem != nil {
		_ = s.Mem.DeleteSession(ctx, string(req.SessionID))
	}
	return &openacp.CloseSessionResponse{}, nil
}

func (s *AgentServer) OnDeleteSession(ctx context.Context, req openacp.DeleteSessionRequest) (*openacp.DeleteSessionResponse, error) {
	s.removeSession(req.SessionID)
	if s.SessionStore != nil {
		_ = s.SessionStore.Delete(ctx, string(req.SessionID))
	}
	if s.Mem != nil {
		_ = s.Mem.DeleteSession(ctx, string(req.SessionID))
	}
	return &openacp.DeleteSessionResponse{}, nil
}

func (s *AgentServer) OnListSessions(ctx context.Context, req openacp.ListSessionsRequest) (*openacp.ListSessionsResponse, error) {
	if s.SessionStore == nil {
		return &openacp.ListSessionsResponse{Sessions: []openacp.SessionInfo{}}, nil
	}
	list, err := s.SessionStore.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	out := make([]openacp.SessionInfo, 0, len(list))
	for _, si := range list {
		cwd := si.Cwd
		if cwd == "" {
			cwd = "/"
		}
		out = append(out, openacp.SessionInfo{
			SessionID: openacp.SessionId(si.ID),
			Cwd:       cwd,
			Title:     si.Title,
			UpdatedAt: si.UpdatedAt.Format(time.RFC3339),
		})
	}
	return &openacp.ListSessionsResponse{Sessions: out}, nil
}

// ── Config & modes ──

func (s *AgentServer) buildConfigOptions(sid openacp.SessionId) []openacp.SessionConfigOption {
	ss := s.getSession(sid)
	mode := "chat"
	thoughtLevel := "medium"
	if ss != nil {
		mode = ss.mode
		if v, ok := ss.config["thought_level"]; ok {
			if s, ok := v.(string); ok {
				thoughtLevel = s
			}
		}
	}
	return []openacp.SessionConfigOption{
		{
			ID:           "mode",
			Name:         "Session Mode",
			Description:  "Chat mode for conversation, Plan mode for goal-driven execution",
			Category:     "mode",
			Type:         "select",
			CurrentValue: mode,
			Options: []openacp.SessionConfigOptValue{
				{Value: "chat", Name: "Chat", Description: "Conversational agent with tools"},
				{Value: "plan", Name: "Plan", Description: "Goal decomposition and DAG execution"},
			},
		},
		{
			ID:           "thought_level",
			Name:         "Reasoning Level",
			Description:  "Controls the amount of reasoning the model produces",
			Category:     "thought_level",
			Type:         "select",
			CurrentValue: thoughtLevel,
			Options: []openacp.SessionConfigOptValue{
				{Value: "low", Name: "Low"},
				{Value: "medium", Name: "Medium"},
				{Value: "high", Name: "High"},
			},
		},
	}
}

func (s *AgentServer) buildModeState(sid openacp.SessionId) *openacp.SessionModeState {
	ss := s.getSession(sid)
	current := "chat"
	if ss != nil {
		current = ss.mode
	}
	return &openacp.SessionModeState{
		CurrentModeID: openacp.SessionModeId(current),
		AvailableModes: []openacp.SessionMode{
			{ID: "chat", Name: "Chat", Description: "Conversational agent with tools"},
			{ID: "plan", Name: "Plan", Description: "Goal decomposition and DAG execution"},
		},
	}
}

func (s *AgentServer) OnSetSessionMode(ctx context.Context, req openacp.SetSessionModeRequest) (*openacp.SetSessionModeResponse, error) {
	ss := s.getSession(req.SessionID)
	if ss == nil {
		return nil, fmt.Errorf("session %s not found", req.SessionID)
	}
	ss.mode = string(req.ModeID)
	return &openacp.SetSessionModeResponse{}, nil
}

func (s *AgentServer) OnSetSessionConfigOption(ctx context.Context, req openacp.SetSessionConfigOptionRequest) (*openacp.SetSessionConfigOptionResponse, error) {
	ss := s.getSession(req.SessionID)
	if ss == nil {
		return nil, fmt.Errorf("session %s not found", req.SessionID)
	}

	// Per ACP spec: Type "boolean" selects the boolean variant; absent/empty
	// defaults to select (value_id).  Value is bool for boolean, string for select.
	switch req.Type {
	case "boolean":
		if b, ok := req.Value.(bool); ok {
			ss.config[req.ConfigID] = b
		}
	default:
		if s, ok := req.Value.(string); ok {
			ss.config[req.ConfigID] = s
		}
	}

	return &openacp.SetSessionConfigOptionResponse{
		ConfigOptions: s.buildConfigOptions(req.SessionID),
	}, nil
}

// ── Prompt ──

func (s *AgentServer) OnPrompt(ctx context.Context, req openacp.PromptRequest, sender openacp.SessionEventSender) (*openacp.PromptResponse, error) {
	var input string
	for _, b := range req.Prompt {
		if b.Text != "" {
			input = b.Text
			break
		}
	}
	if input == "" {
		return nil, fmt.Errorf("no text in prompt")
	}

	ss := s.getSession(req.SessionID)
	if ss == nil {
		return nil, fmt.Errorf("session %s not found", req.SessionID)
	}

	// Per-prompt cancellable context.
	ctx, cancel := context.WithCancel(ctx)
	ss.cancel = cancel
	defer func() {
		ss.cancel = nil
		cancel()
	}()

	// Build the agent clone for this turn — inject ACP-based approval.
	agent := s.agentForTurn(req.SessionID)

	oaSession := openagent.Session{
		ID:        string(req.SessionID),
		CreatedAt: ss.createdAt,
	}

	// Update session title from first user message.
	if s.SessionStore != nil {
		title := input
		if len(title) > 80 {
			title = title[:80]
		}
		info, _ := s.SessionStore.Get(ctx, string(req.SessionID))
		if info != nil && info.Title == "" {
			info.Title = title
			info.UpdatedAt = time.Now()
			_ = s.SessionStore.Save(ctx, *info)
		}
	}

	ch := agent.RunStream(ctx, oaSession, openagent.UserMessage(input))
	var usage openagent.Usage
	for evt := range ch {
		switch evt.Type {
		case openagent.StreamTextDelta:
			sender.SendAgentMessage(evt.Text)

		case openagent.StreamThought:
			sender.SendAgentThought(evt.Text)

		case openagent.StreamToolCall:
			if len(evt.Message.ToolCalls) > 0 {
				for _, tc := range evt.Message.ToolCalls {
					sender.SendToolCall(openacp.ToolCallUpdate{
						ToolCallID: tc.ID,
						Title:      tc.Function.Name,
						Kind:       "execute",
						Status:     "in_progress",
						RawInput:   json.RawMessage(tc.Function.Arguments),
					})
				}
			}

		case openagent.StreamToolProgress:
			// Forward as in_progress tool call update.
			sender.SendToolCall(openacp.ToolCallUpdate{
				ToolCallID: evt.ToolCallID,
				Status:     "in_progress",
				RawOutput:  map[string]any{"chunk": evt.Text},
			})

		case openagent.StreamToolResult:
			sender.SendToolCall(openacp.ToolCallUpdate{
				ToolCallID: evt.Message.ToolCallID,
				Status:     "completed",
				RawOutput:  map[string]any{"result": evt.Message.Content},
			})

		case openagent.StreamDone:
			if evt.Result != nil {
				usage = evt.Result.Usage
			}

		case openagent.StreamError:
			return nil, evt.Error

		case openagent.StreamAborted:
			return &openacp.PromptResponse{StopReason: openacp.StopReasonCancelled}, nil
		}
	}

	// Report usage if available.
	if usage.TotalTokens > 0 {
		cw := 0
		if agent.Model != nil {
			cw = agent.Model.ContextWindow()
		}
		sender.SendUsageUpdate(usage.TotalTokens, cw, nil)
	}

	if ctx.Err() != nil {
		return &openacp.PromptResponse{StopReason: openacp.StopReasonCancelled}, nil
	}
	return &openacp.PromptResponse{StopReason: openacp.StopReasonEndTurn}, nil
}

// ── Cancel ──

func (s *AgentServer) OnCancel(ctx context.Context, sid openacp.SessionId) error {
	ss := s.getSession(sid)
	if ss != nil && ss.cancel != nil {
		ss.cancel()
	}
	return nil
}

// ── Auth ──

func (s *AgentServer) OnAuthenticate(ctx context.Context, req openacp.AuthenticateRequest) (*openacp.AuthenticateResponse, error) {
	// No authentication required for local agent.
	return &openacp.AuthenticateResponse{}, nil
}

func (s *AgentServer) OnLogout(ctx context.Context, req openacp.LogoutRequest) (*openacp.LogoutResponse, error) {
	return &openacp.LogoutResponse{}, nil
}

// ── Internal ──

func (s *AgentServer) agentForTurn(sid openacp.SessionId) *openagent.Agent {
	clone := s.Agent.Clone()
	if s.clientRPC != nil {
		clone.Approver = &acpApprover{client: s.clientRPC, sessionID: sid}
	}
	return clone
}

// ── acpApprover ──

type acpApprover struct {
	client    openacp.ClientRequester
	sessionID openacp.SessionId
}

func (a *acpApprover) Approve(ctx context.Context, call openagent.ToolCall, def openagent.FunctionDefinition, session openagent.Session) (bool, string) {
	if a.client == nil {
		return true, ""
	}
	resp, err := a.client.RequestPermission(ctx, openacp.RequestPermissionRequest{
		SessionID: a.sessionID,
		ToolCall: openacp.ToolCallUpdate{
			ToolCallID: call.ID,
			Title:      def.Name,
			Kind:       "execute",
			Status:     "pending",
			RawInput:   json.RawMessage(call.Function.Arguments),
		},
		Options: []openacp.PermissionOption{
			{OptionID: "allow", Name: "Allow", Kind: openacp.PermissionAllowOnce},
			{OptionID: "always", Name: "Allow Always", Kind: openacp.PermissionAllowAlways},
			{OptionID: "reject", Name: "Reject", Kind: openacp.PermissionRejectOnce},
		},
	})
	if err != nil {
		return false, fmt.Sprintf("permission request failed: %v", err)
	}
	if resp.Outcome.Cancelled {
		return false, "cancelled"
	}
	if resp.Outcome.OptionID == nil {
		return false, "no option selected"
	}
	switch *resp.Outcome.OptionID {
	case "allow", "always":
		return true, ""
	case "reject":
		return false, "rejected by user"
	default:
		return false, fmt.Sprintf("unknown option: %s", *resp.Outcome.OptionID)
	}
}
