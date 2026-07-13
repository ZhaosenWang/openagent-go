package rest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	openagent "github.com/yusheng-g/openagent-go"
	"github.com/yusheng-g/openagent-go/eventbus"
)

// ── Handler ──

// Handler serves a REST API for an openagent-go Agent.
//
// Create with [NewHandler], then register on an [http.ServeMux]:
//
//	handler := rest.NewHandler(agent)
//	mux := http.NewServeMux()
//	handler.Register(mux)
//	http.ListenAndServe(":8080", mux)
// ModelInfo describes a registered model for the frontend.
type ModelInfo struct {
	ID       string `json:"id"`
	Provider string `json:"provider,omitempty"`
}

type Handler struct {
	defaultModel openagent.Model
	models       map[string]openagent.Model // modelID → model instance
	modelList    []ModelInfo                // ordered list for /models endpoint

	memory       openagent.Memory
	tools        []openagent.Tool
	instructions string
	name         string
	maxTurns     int

	sessionIdleTTL time.Duration // 0 = sessions never expire

	store SessionStore // nil = in-memory only

	bus *eventbus.Bus[SSEEvent]

	mu       sync.RWMutex
	sessions map[string]*sessionState

	reapOnce sync.Once
}

// NewHandler creates a Handler from a configured Agent.
// The agent's Model, Memory, Tools, Instructions, Name, and MaxTurns
// are captured as the template for per-session Agent instances.
func NewHandler(agent *openagent.Agent) *Handler {
	return &Handler{
		defaultModel: agent.Model,
		models:    make(map[string]openagent.Model),
		modelList: nil,
		memory:       agent.Memory,
		tools:        agent.Tools,
		instructions: agent.Instructions,
		name:         agent.Name,
		maxTurns:     agent.MaxTurns,
		bus:          eventbus.New[SSEEvent](500),
		sessions:     make(map[string]*sessionState),
	}
}

// RegisterModel adds a model to the handler's registry.
// id is the string the frontend sends as modelID (e.g. "deepseek-v3").
// provider is optional metadata shown in /models (e.g. "openai", "anthropic").
func (h *Handler) RegisterModel(id string, model openagent.Model, provider string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.models[id] = model
	h.modelList = append(h.modelList, ModelInfo{ID: id, Provider: provider})
}

// Register adds the handler's routes to mux using Go 1.22+ patterns.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /sessions", h.handleCreateSession)
	mux.HandleFunc("GET /sessions", h.handleListSessions)
	mux.HandleFunc("GET /sessions/{id}", h.handleGetSession)
	mux.HandleFunc("PATCH /sessions/{id}", h.handleUpdateSession)
	mux.HandleFunc("GET /sessions/{id}/messages", h.handleListMessages)
	mux.HandleFunc("DELETE /sessions/{id}", h.handleDeleteSession)
	mux.HandleFunc("POST /sessions/{id}/chat", h.handleChat)
	mux.HandleFunc("POST /sessions/{id}/approve", h.handleApprove)
	mux.HandleFunc("GET /models", h.handleListModels)
}

// WithSessionTTL sets the idle duration after which inactive sessions are
// removed from memory. The messages themselves are not deleted — only the
// in-memory agent runtime state is released. A client reconnecting with the
// same session ID triggers a transparent rebuild from persistent Memory.
//
// 0 (the default) disables TTL — sessions never expire.
func (h *Handler) WithSessionTTL(idle time.Duration) *Handler {
	h.sessionIdleTTL = idle
	return h
}

// WithSessionStore attaches a persistent session metadata store.
// nil (the default) preserves the current in-memory-only behavior.
func (h *Handler) WithSessionStore(s SessionStore) *Handler {
	h.store = s
	return h
}

// ── sessionState ──

// sessionState holds the per-session runtime state.
// Events are published to the Handler-level bus so that multiple
// SSE connections (e.g. browser tabs) all receive the full stream.
type sessionState struct {
	info       SessionInfo // ModelID is the session's model preference; empty → handler default
	agent      *openagent.Agent
	lastAccess time.Time

	mu              sync.Mutex
	pendingApproval *pendingApproval
}

type pendingApproval struct {
	respond chan approveResponse
}

type approveResponse struct {
	allowed bool
	reason  string
}

// ── Session CRUD handlers ──

func (h *Handler) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var req CreateSessionRequest
	if r.Body != nil {
		json.NewDecoder(r.Body).Decode(&req)
	}

	id := generateID()
	now := time.Now()
	info := SessionInfo{
		ID:        id,
		Title:     req.Title,
		Kind:      "single",
		ModelID:   req.ModelID,
		CreatedAt: now,
		UpdatedAt: now,
	}

	h.mu.Lock()
	h.sessions[id] = h.newSession(info)
	h.mu.Unlock()

	if h.store != nil {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := h.store.Save(ctx, info); err != nil {
				log.Printf("rest: failed to persist session %s: %v", id, err)
			}
		}()
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(info)
}

func (h *Handler) handleListSessions(w http.ResponseWriter, r *http.Request) {
	if h.store != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		list, err := h.store.List(ctx, "single")
		if err != nil {
			http.Error(w, `{"error":"failed to list sessions"}`, http.StatusInternalServerError)
			return
		}
		if list == nil {
			list = []SessionInfo{}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(list)
		return
	}

	h.mu.RLock()
	defer h.mu.RUnlock()

	list := make([]SessionInfo, 0, len(h.sessions))
	for _, s := range h.sessions {
		list = append(list, s.info)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(list)
}

func (h *Handler) handleGetSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	h.mu.RLock()
	s, ok := h.sessions[id]
	h.mu.RUnlock()

	if ok {
		detail := SessionDetail{SessionInfo: s.info}
		if s.agent != nil && s.agent.Model != nil {
			detail.ContextWindow = s.agent.Model.ContextWindow()
		}
		if h.memory != nil {
			if n, err := h.memory.Count(context.Background(), s.info.ID); err == nil {
				detail.MessageCount = n
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(detail)
		return
	}

	// Not in memory — try persistent store (e.g. after restart).
	if h.store != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		info, err := h.store.Get(ctx, id)
		if err != nil {
			http.Error(w, `{"error":"failed to get session"}`, http.StatusInternalServerError)
			return
		}
		if info != nil {
			detail := SessionDetail{SessionInfo: *info}
			if h.memory != nil {
				if n, err := h.memory.Count(context.Background(), id); err == nil {
					detail.MessageCount = n
				}
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(detail)
			return
		}
	}

	http.Error(w, `{"error":"session not found"}`, http.StatusNotFound)
}

func (h *Handler) handleUpdateSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var body struct {
		Title string `json:"title"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid body"}`, http.StatusBadRequest)
		return
	}

	h.mu.RLock()
	s, ok := h.sessions[id]
	h.mu.RUnlock()
	if !ok {
		http.Error(w, `{"error":"session not found"}`, http.StatusNotFound)
		return
	}

	s.mu.Lock()
	s.info.Title = body.Title
	if body.Title != "" {
		s.info.UpdatedAt = time.Now()
	}
	currentInfo := s.info
	s.mu.Unlock()

	if h.store != nil {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := h.store.Save(ctx, currentInfo); err != nil {
				log.Printf("rest: failed to persist session %s: %v", id, err)
			}
		}()
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(currentInfo)
}

func (h *Handler) handleListMessages(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	h.mu.RLock()
	_, ok := h.sessions[id]
	h.mu.RUnlock()
	if !ok {
		http.Error(w, `{"error":"session not found"}`, http.StatusNotFound)
		return
	}
	if h.memory == nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]openagent.Message{})
		return
	}

	limit := 50
	if l, err := parseIntParam(r, "limit", 1, 200); err == nil {
		limit = l
	}
	before := 0
	if b, err := parseIntParam(r, "before", 0, 100000); err == nil {
		before = b
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	msgs, err := h.memory.Recent(ctx, id, limit, before)
	if err != nil {
		http.Error(w, `{"error":"failed to fetch messages"}`, http.StatusInternalServerError)
		return
	}
	if msgs == nil {
		msgs = []openagent.Message{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(msgs)
}


// parseIntParam parses an integer query parameter with bounds.
func parseIntParam(r *http.Request, name string, min, max int) (int, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return 0, fmt.Errorf("missing")
	}
	var n int
	if _, err := fmt.Sscanf(raw, "%d", &n); err != nil {
		return 0, fmt.Errorf("invalid integer")
	}
	if n < min {
		n = min
	}
	if n > max {
		n = max
	}
	return n, nil
}

func (h *Handler) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	// Delete from store first — if it fails, don't touch the in-memory map.
	if h.store != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if err := h.store.Delete(ctx, id); err != nil {
			http.Error(w, `{"error":"failed to delete session"}`, http.StatusInternalServerError)
			return
		}
	}

	h.mu.Lock()
	delete(h.sessions, id)
	h.mu.Unlock()

	if h.memory != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		_ = h.memory.DeleteSession(ctx, id)
	}

	w.WriteHeader(http.StatusNoContent)
}

// ── Chat handler ──

func (h *Handler) handleChat(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var body ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Message == "" {
		http.Error(w, `{"error":"message is required"}`, http.StatusBadRequest)
		return
	}

	s := h.getOrCreateSession(id)

	// Reset pending approval for the new chat message.
	s.mu.Lock()
	s.pendingApproval = nil
	s.mu.Unlock()

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, `{"error":"streaming not supported"}`, http.StatusInternalServerError)
		return
	}
	setSSEHeaders(w)
	flusher.Flush() // flush headers immediately so the client sees streaming start

	// Subscribe to the session's event bus. Live-only — history is NOT
	// replayed because this is a new chat, not a reconnection. Replaying
	// old "done" events would cause the handler to return before the
	// current chat's events arrive.
	sub := h.bus.SubscribeLive(id)
	defer h.bus.Unsubscribe(id, sub)

	// Resolve model: chat-level override > session default > handler default.
	modelID := body.ModelID
	if modelID == "" {
		modelID = s.info.ModelID
	}
	h.mu.RLock()
	model := h.models[modelID]
	h.mu.RUnlock()
	if model == nil {
		model = h.defaultModel
		modelID = ""
	}

	// Persist the resolved model so GET /sessions reflects the actual model.
	s.mu.Lock()
	s.info.ModelID = modelID
	currentInfo := s.info
	s.mu.Unlock()

	if h.store != nil {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := h.store.Save(ctx, currentInfo); err != nil {
				log.Printf("rest: failed to sync modelID for session %s: %v", id, err)
			}
		}()
	}

	// Start the agent run in a background goroutine.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		oaSession := openagent.Session{
			ID:        id,

			ModelID:   modelID,
			Model:     model,
			CreatedAt: s.info.CreatedAt,
		}

		ch := s.agent.RunStream(ctx, oaSession, openagent.UserMessage(body.Message))
		for evt := range ch {
			se := streamToSSE(evt)
			select {
			case <-r.Context().Done():
				// Client disconnected — stop publishing.
				// Agent continues with its own ctx; timeout cleans up.
				return
			default:
			}
			h.bus.Publish(id, se)
		}
	}()

	// Stream events to the SSE response until done/error/disconnect.
	for se := range sub.C {
		if err := writeSSE(w, flusher, se); err != nil {
			return
		}
		if se.Type == "done" || se.Type == "error" {
			return
		}
	}
}

// ── Approve handler ──

func (h *Handler) handleApprove(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var body ApproveRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"allowed is required"}`, http.StatusBadRequest)
		return
	}

	h.mu.RLock()
	s, ok := h.sessions[id]
	h.mu.RUnlock()

	if !ok {
		http.Error(w, `{"error":"session not found"}`, http.StatusNotFound)
		return
	}

	s.mu.Lock()
	p := s.pendingApproval
	s.pendingApproval = nil
	s.mu.Unlock()

	if p == nil {
		http.Error(w, `{"error":"no pending approval"}`, http.StatusBadRequest)
		return
	}

	reason := "denied"
	if body.Feedback != "" {
		reason = "denied: " + body.Feedback
	}
	if body.Allowed {
		reason = "approved"
	}
	p.respond <- approveResponse{allowed: body.Allowed, reason: reason}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": reason})
}

// ── Models ──

func (h *Handler) handleListModels(w http.ResponseWriter, r *http.Request) {
	h.mu.RLock()
	models := make([]ModelInfo, len(h.modelList))
	copy(models, h.modelList)
	h.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"models": models})
}

// ── Session management ──

func (h *Handler) getOrCreateSession(id string) *sessionState {
	h.mu.Lock()
	if s, ok := h.sessions[id]; ok {
		s.lastAccess = time.Now()
		h.mu.Unlock()
		return s
	}
	h.mu.Unlock()

	// Try to restore from persistent store.
	var info SessionInfo
	restored := false
	if h.store != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if stored, err := h.store.Get(ctx, id); err == nil && stored != nil {
			info = *stored
			restored = true
		}
	}

	if !restored {
		info = SessionInfo{
			ID:        id,
			Kind:      "single",
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		}
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	// Double-check: someone else might have created it while we were querying the store.
	if s, ok := h.sessions[id]; ok {
		s.lastAccess = time.Now()
		return s
	}

	s := h.newSession(info)
	s.lastAccess = time.Now()
	h.sessions[id] = s
	h.maybeStartReaper()

	if !restored && h.store != nil {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := h.store.Save(ctx, info); err != nil {
				log.Printf("rest: failed to persist session %s: %v", id, err)
			}
		}()
	}

	return s
}

func (h *Handler) newSession(info SessionInfo) *sessionState {
	s := &sessionState{info: info}

	s.agent = openagent.NewAgent(h.name,
		openagent.WithModel(h.defaultModel),
		openagent.WithMemory(h.memory),
		openagent.WithTools(h.tools...),
		openagent.WithInstructions(h.instructions),
		openagent.WithMaxTurns(h.maxTurns),
			openagent.WithRunObserver(&stageObserver{bus: h.bus, sid: info.ID}),
		openagent.WithApprover(&restApprover{
			submit: func(call openagent.ToolCall, resp chan approveResponse) {
				h.submitApproval(s, call, resp)
			},
		}),
	)

	return s
}

// ── Approval bridge ──

type restApprover struct {
	submit func(call openagent.ToolCall, resp chan approveResponse)
}

func (a *restApprover) Approve(ctx context.Context, call openagent.ToolCall, def openagent.FunctionDefinition, session openagent.Session) (bool, string) {
	resp := make(chan approveResponse, 1)
	a.submit(call, resp)

	select {
	case <-ctx.Done():
		return false, "cancelled"
	case r := <-resp:
		return r.allowed, r.reason
	}
}

func (h *Handler) submitApproval(s *sessionState, call openagent.ToolCall, resp chan approveResponse) {
	tcj := &SSEToolCall{
		ID: call.ID,
		Function: SSEToolCallFunction{
			Name:      call.Function.Name,
			Arguments: call.Function.Arguments,
		},
	}

	evt := SSEEvent{
		Type:     "tool_approval",
		ToolCall: tcj,
	}

	s.mu.Lock()
	s.pendingApproval = &pendingApproval{respond: resp}
	s.mu.Unlock()

	h.bus.Publish(s.info.ID, evt)
}

// ── SSE conversion ──

func streamToSSE(evt openagent.StreamEvent) SSEEvent {
	switch evt.Type {
		case openagent.StreamThought:
		return SSEEvent{Type: "thought", Text: evt.Text}

	case openagent.StreamTextDelta:
		return SSEEvent{Type: "text_delta", Text: evt.Text}

	case openagent.StreamToolCall:
		tc := evt.Message.ToolCalls[0]
		return SSEEvent{
			Type: "tool_call",
			ToolCall: &SSEToolCall{
				ID: tc.ID,
				Function: SSEToolCallFunction{
					Name:      tc.Function.Name,
					Arguments: tc.Function.Arguments,
				},
			},
		}

	case openagent.StreamToolResult:
		return SSEEvent{
			Type:       "tool_result",
			ToolCallID: evt.Message.ToolCallID,
			Text:       evt.Message.Content,
		}

	case openagent.StreamRetrying:
		msg := "retrying"
		if evt.Error != nil {
			msg = evt.Error.Error()
		}
		return SSEEvent{Type: "retrying", Text: msg}

	case openagent.StreamToolProgress:
		return SSEEvent{Type: "tool_progress", Text: evt.Text, ToolCallID: evt.ToolCallID}

	case openagent.StreamAborted:
		se := SSEEvent{Type: "aborted"}
		if evt.Error != nil {
			se.Text = evt.Error.Error()
		}
		return se

	case openagent.StreamDone:
		se := SSEEvent{Type: "done"}
		if evt.Result != nil {
			se.FinalOutput = evt.Result.FinalOutput
			se.PromptTokens = evt.Result.Usage.PromptTokens
			se.ContextWindow = evt.Result.ContextWindow
		}
		return se

	case openagent.StreamError:
		msg := "unknown error"
		if evt.Error != nil {
			msg = evt.Error.Error()
		}
		return SSEEvent{Type: "error", Text: msg}

	default:
		return SSEEvent{}
	}
}

// ── Helpers ──

func generateID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// stageObserver publishes pipeline stage events to the SSE bus so
// frontends can render a live pipeline visualization.
type stageObserver struct {
	bus *eventbus.Bus[SSEEvent]
	sid string
}

func (o *stageObserver) ObserveStage(ctx context.Context, evt openagent.StageEvent) {
	sd := struct {
		Name       string         `json:"name"`
		Phase      string         `json:"phase"`
		Detail     map[string]any `json:"detail,omitempty"`
		DurationMs int64          `json:"duration_ms,omitempty"`
		Err        string         `json:"error,omitempty"`
	}{
		Name:   evt.Name,
		Phase:  evt.Phase,
		Detail: evt.Detail,
	}
	if evt.Phase == "leave" {
		sd.DurationMs = evt.Duration.Milliseconds()
	}
	if evt.Err != nil {
		sd.Err = evt.Err.Error()
	}
	b, err := json.Marshal(sd)
	if err != nil {
		o.bus.Publish(o.sid, SSEEvent{Type: "error", Text: "stage marshal failed: " + err.Error()})
		return
	}
	o.bus.Publish(o.sid, SSEEvent{Type: "stage", Stage: b})
}

var _ openagent.RunObserver = (*stageObserver)(nil)

// ── Session TTL ──

// maybeStartReaper lazily starts a background goroutine that reclaims
// idle sessionState entries. It's a no-op when sessionIdleTTL is 0.
func (h *Handler) maybeStartReaper() {
	if h.sessionIdleTTL <= 0 {
		return
	}
	h.reapOnce.Do(func() {
		go h.reapLoop()
	})
}

func (h *Handler) reapLoop() {
	const interval = 1 * time.Minute
	for {
		time.Sleep(interval)
		h.reap()
	}
}

func (h *Handler) reap() {
	now := time.Now()

	// Phase 1: collect expired IDs under read lock.
	h.mu.RLock()
	var expired []string
	for id, s := range h.sessions {
		if now.Sub(s.lastAccess) > h.sessionIdleTTL {
			expired = append(expired, id)
		}
	}
	h.mu.RUnlock()

	if len(expired) == 0 {
		return
	}

	// Phase 2: under write lock, re-verify lastAccess to prevent
	// TOCTOU races with getOrCreateSession.
	h.mu.Lock()
	reaped := 0
	for _, id := range expired {
		s, ok := h.sessions[id]
		if !ok {
			continue
		}
		if now.Sub(s.lastAccess) < h.sessionIdleTTL {
			continue // refreshed between RLock→Lock
		}
		delete(h.sessions, id)
		reaped++
	}
	h.mu.Unlock()

	if reaped > 0 {
		log.Printf("rest: reaped %d idle sessions", reaped)
	}
}
