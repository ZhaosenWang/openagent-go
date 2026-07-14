package rest

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	openagent "github.com/yusheng-g/openagent-go"
	"github.com/yusheng-g/openagent-go/eventbus"
)

// ── TeamHandler ──

// TeamHandler serves a REST API for an openagent-go Team.
//
// Create with [NewTeamHandler], then register on an [http.ServeMux]:
//
//	templates := []rest.TeamAgentTemplate{
//	    {Name: "researcher", Description: "Finds facts", Agent: researcher},
//	    {Name: "writer", Description: "Writes reports", Agent: writer},
//	}
//	th := rest.NewTeamHandler(mem, templates...)
//	th.Register(mux)
type TeamHandler struct {
	agents []TeamAgentTemplate
	memory openagent.Memory
	model  openagent.Model // from first template, used by dynamically added agents

	sessionIdleTTL time.Duration // 0 = sessions never expire

	store SessionStore // nil = in-memory only

	bus *eventbus.Bus[SSEEvent]

	mu       sync.RWMutex
	sessions map[string]*teamSessionState

	reapOnce sync.Once
}

// TeamAgentTemplate describes an agent to include in every new team session.
type TeamAgentTemplate struct {
	Name        string
	Description string
	Agent       *openagent.Agent // Model, Tools, Instructions, MaxTurns are captured
}

// NewTeamHandler creates a TeamHandler.
// At least one agent template is required (its Model is used for dynamically added agents).
func NewTeamHandler(mem openagent.Memory, agents ...TeamAgentTemplate) *TeamHandler {
	var model openagent.Model
	if len(agents) > 0 {
		model = agents[0].Agent.Model
	}
	return &TeamHandler{
		agents:   agents,
		memory:   mem,
		model:    model,
		bus:      eventbus.New[SSEEvent](500),
		sessions: make(map[string]*teamSessionState),
	}
}

// Register adds the team handler's routes to mux.
func (h *TeamHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /team/sessions", h.handleCreateSession)
	mux.HandleFunc("GET /team/sessions", h.handleListSessions)
	mux.HandleFunc("GET /team/sessions/{id}", h.handleGetSession)
	mux.HandleFunc("GET /team/sessions/{id}/messages", h.handleListMessages)
	mux.HandleFunc("PATCH /team/sessions/{id}", h.handleUpdateSession)
	mux.HandleFunc("DELETE /team/sessions/{id}", h.handleDeleteSession)
	mux.HandleFunc("POST /team/sessions/{id}/chat", h.handleChat)
	mux.HandleFunc("POST /team/sessions/{id}/approve", h.handleApprove)
	mux.HandleFunc("GET /team/sessions/{id}/agents", h.handleListAgents)
	mux.HandleFunc("POST /team/sessions/{id}/agents", h.handleAddAgent)
	mux.HandleFunc("DELETE /team/sessions/{id}/agents", h.handleRemoveAgent)
}

// WithSessionTTL sets the idle duration after which inactive sessions are
// removed from memory. Per-agent private memory partitions are also cleaned up.
// Team-shared messages are not deleted — only the in-memory runtime state
// and agent-private partitions are released.
//
// 0 (the default) disables TTL — sessions never expire.
func (h *TeamHandler) WithSessionTTL(idle time.Duration) *TeamHandler {
	h.sessionIdleTTL = idle
	return h
}

// WithSessionStore attaches a persistent session metadata store.
func (h *TeamHandler) WithSessionStore(s SessionStore) *TeamHandler {
	h.store = s
	return h
}

// ── teamSessionState ──

type teamSessionState struct {
	info       SessionInfo
	team       *openagent.Team
	agentList  []agentInfo
	agentMems  []*teamAgentMemory // per-agent memory wrappers for cleanup
	lastAccess time.Time

	mu              sync.Mutex
	pendingApproval *pendingApproval
}

type agentInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Type        string `json:"type"` // "internal"
}

// ── Session CRUD ──

func (h *TeamHandler) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var req CreateSessionRequest
	if r.Body != nil {
		json.NewDecoder(r.Body).Decode(&req)
	}

	id := generateID()
	now := time.Now()

	info := SessionInfo{
		ID:        id,
		Title:     req.Title,
		Kind:      "team",
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
				log.Printf("rest: failed to persist team session %s: %v", id, err)
			}
		}()
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(info)
}

func (h *TeamHandler) handleListSessions(w http.ResponseWriter, r *http.Request) {
	if h.store != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		list, err := h.store.List(ctx, "team")
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

func (h *TeamHandler) handleGetSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	h.mu.RLock()
	s, ok := h.sessions[id]
	h.mu.RUnlock()

	if ok {
		w.Header().Set("Content-Type", "application/json")
		detail := SessionDetail{SessionInfo: s.info}
		if h.memory != nil {
			if n, err := h.memory.Count(context.Background(), s.info.ID); err == nil {
				detail.MessageCount = n
			}
		}
		json.NewEncoder(w).Encode(detail)
		return
	}

	// Not in memory — try persistent store.
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

func (h *TeamHandler) handleUpdateSession(w http.ResponseWriter, r *http.Request) {
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
				log.Printf("rest: failed to persist team session %s: %v", id, err)
			}
		}()
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(currentInfo)
}

func (h *TeamHandler) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
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
	s := h.sessions[id]
	delete(h.sessions, id)
	h.mu.Unlock()

	// Session may not exist (double-delete is harmless).
	if s == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	// Clean up agent-private memory partitions.
	for _, tam := range s.agentMems {
		_ = tam.DeleteSession(ctx, id)
	}

	// Clean up team-shared memory.
	if h.memory != nil {
		_ = h.memory.DeleteSession(ctx, id)
	}

	w.WriteHeader(http.StatusNoContent)
}

func (h *TeamHandler) handleListMessages(w http.ResponseWriter, r *http.Request) {
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

// ── Chat ──

func (h *TeamHandler) handleChat(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var body ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Message == "" {
		http.Error(w, `{"error":"message is required"}`, http.StatusBadRequest)
		return
	}

	s := h.getOrCreateSession(id)

	s.mu.Lock()
	s.pendingApproval = nil
	s.mu.Unlock()

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, `{"error":"streaming not supported"}`, http.StatusInternalServerError)
		return
	}
	setSSEHeaders(w)
	flusher.Flush() // flush headers immediately

	sub := h.bus.SubscribeLive(id)
	defer h.bus.Unsubscribe(id, sub)

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		oaSession := openagent.Session{
			ID:        id,
			CreatedAt: s.info.CreatedAt,
		}

		ch := s.team.RunStream(ctx, oaSession, openagent.UserMessage(body.Message))
		for evt := range ch {
			se := teamEventToSSE(evt)
			if se.Type == "" {
				continue
			}
			h.bus.Publish(id, se)
		}
	}()

	for se := range sub.C {
		if err := writeSSE(w, flusher, se); err != nil {
			return
		}
		if se.Type == "done" || se.Type == "error" {
			return
		}
	}
}

// ── Approve ──

func (h *TeamHandler) handleApprove(w http.ResponseWriter, r *http.Request) {
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

// ── Agents ──

func (h *TeamHandler) handleListAgents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	h.mu.RLock()
	s, ok := h.sessions[id]
	h.mu.RUnlock()

	if !ok {
		http.Error(w, `{"error":"session not found"}`, http.StatusNotFound)
		return
	}

	s.mu.Lock()
	list := make([]agentInfo, len(s.agentList))
	copy(list, s.agentList)
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"agents": list})
}

func (h *TeamHandler) handleAddAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var body struct {
		Name         string `json:"name"`
		Description  string `json:"description"`
		Instructions string `json:"instructions"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" {
		http.Error(w, `{"error":"name is required"}`, http.StatusBadRequest)
		return
	}

	h.mu.RLock()
	s, ok := h.sessions[id]
	h.mu.RUnlock()

	if !ok {
		http.Error(w, `{"error":"session not found"}`, http.StatusNotFound)
		return
	}

	if h.model == nil {
		http.Error(w, `{"error":"no model available for new agent"}`, http.StatusInternalServerError)
		return
	}

	// Wrap memory for agent-private persistence, same as newSession.
	var agentMem openagent.Memory
	var tam *teamAgentMemory
	if h.memory != nil {
		tam = newTeamAgentMemory(body.Name, h.memory)
		agentMem = tam
	}
	agent := openagent.NewAgent(body.Name,
		openagent.WithModel(h.model),
		openagent.WithMemory(agentMem),
		openagent.WithInstructions(body.Instructions),
		openagent.WithMaxTurns(3),
		openagent.WithApprover(&restApprover{
			submit: func(call openagent.ToolCall, resp chan approveResponse) {
				h.submitApproval(s, call, resp)
			},
		}),
	)

	if err := s.team.AddAgent(body.Name, body.Description, agent); err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	s.agentList = append(s.agentList, agentInfo{
		Name: body.Name, Description: body.Description, Type: "internal",
	})
	if tam != nil {
		s.agentMems = append(s.agentMems, tam)
	}
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{"status": "added"})
}

func (h *TeamHandler) handleRemoveAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	name := r.URL.Query().Get("name")
	if name == "" {
		http.Error(w, `{"error":"name query param is required"}`, http.StatusBadRequest)
		return
	}

	h.mu.RLock()
	s, ok := h.sessions[id]
	h.mu.RUnlock()

	if !ok {
		http.Error(w, `{"error":"session not found"}`, http.StatusNotFound)
		return
	}

	s.team.RemoveAgent(name)

	s.mu.Lock()
	// Clean up the agent's private memory partition.
	filteredMems := s.agentMems[:0]
	for _, tam := range s.agentMems {
		if tam.agentName == name {
			ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
			_ = tam.DeleteSession(ctx, id)
			cancel()
		} else {
			filteredMems = append(filteredMems, tam)
		}
	}
	s.agentMems = filteredMems

	// Remove from the agent list.
	filtered := s.agentList[:0]
	for _, a := range s.agentList {
		if a.Name != name {
			filtered = append(filtered, a)
		}
	}
	s.agentList = filtered
	s.mu.Unlock()

	w.WriteHeader(http.StatusNoContent)
}

// ── Session management ──

func (h *TeamHandler) getOrCreateSession(id string) *teamSessionState {
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
			Kind:      "team",
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
				log.Printf("rest: failed to persist team session %s: %v", id, err)
			}
		}()
	}

	return s
}

func (h *TeamHandler) newSession(info SessionInfo) *teamSessionState {
	s := &teamSessionState{
		info: info,
	}

	teamOpts := make([]openagent.TeamOption, 0, len(h.agents)+1)
	teamOpts = append(teamOpts, openagent.WithTeamMaxHandoffs(10))

	for _, t := range h.agents {
		// Wrap memory so each agent gets agent-private persistence
		// (tool calls/results) separate from team-shared messages
		// (user input, handoffs, text output).
		var agentMem openagent.Memory
		var tam *teamAgentMemory
		if h.memory != nil {
			tam = newTeamAgentMemory(t.Name, h.memory)
			agentMem = tam
		} else if t.Agent.Memory != nil {
			agentMem = t.Agent.Memory
		}
		agent := cloneAgentForSession(t.Agent, agentMem, s, h.submitApproval)
		teamOpts = append(teamOpts,
			openagent.WithTeamAgent(t.Name, t.Description, agent),
		)
		s.agentList = append(s.agentList, agentInfo{
			Name: t.Name, Description: t.Description, Type: "internal",
		})
		if tam != nil {
			s.agentMems = append(s.agentMems, tam)
		}
	}

	s.team = openagent.NewTeam(teamOpts...)
	return s
}

func cloneAgentForSession(tmpl *openagent.Agent, mem openagent.Memory, s *teamSessionState, submitFn func(*teamSessionState, openagent.ToolCall, chan approveResponse)) *openagent.Agent {
	// mem is already resolved by the caller — either a teamAgentMemory
	// wrapper for agent-private persistence or the template's own memory.
	return openagent.NewAgent(tmpl.Name,
		openagent.WithModel(tmpl.Model),
		openagent.WithMemory(mem),
		openagent.WithTools(tmpl.Tools...),
		openagent.WithInstructions(tmpl.Instructions),
		openagent.WithMaxTurns(tmpl.MaxTurns),
		openagent.WithApprover(&restApprover{
			submit: func(call openagent.ToolCall, resp chan approveResponse) {
				submitFn(s, call, resp)
			},
		}),
	)
}

// ── Approval bridge ──

func (h *TeamHandler) submitApproval(s *teamSessionState, call openagent.ToolCall, resp chan approveResponse) {
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

// ── TeamEvent → SSE ──

func teamEventToSSE(evt openagent.TeamEvent) SSEEvent {
	switch evt.Type {
	case openagent.TeamAgentStart:
		return SSEEvent{Type: "agent_start", Agent: evt.Agent}

	case openagent.TeamAgentEnd:
		se := SSEEvent{Type: "agent_end", Agent: evt.Agent}
		if evt.Error != nil {
			se.Error = evt.Error.Error()
		}
		return se
	case openagent.TeamThought:
		return SSEEvent{Type: "thought", Agent: evt.Agent, Text: evt.Text}

	case openagent.TeamTextDelta:
		return SSEEvent{Type: "text_delta", Agent: evt.Agent, Text: evt.Text}

	case openagent.TeamToolCall:
		var tcj *SSEToolCall
		if evt.ToolCall != nil {
			tcj = &SSEToolCall{
				ID: evt.ToolCall.ID,
				Function: SSEToolCallFunction{
					Name:      evt.ToolCall.Function.Name,
					Arguments: evt.ToolCall.Function.Arguments,
				},
			}
		}
		return SSEEvent{Type: "tool_call", Agent: evt.Agent, ToolCall: tcj}

	case openagent.TeamToolProgress:
		return SSEEvent{Type: "tool_progress", Agent: evt.Agent, ToolCallID: evt.ToolCallID, Text: evt.Text}

	case openagent.TeamToolResult:
		return SSEEvent{Type: "tool_result", Agent: evt.Agent, ToolCallID: evt.ToolCallID, Text: evt.Text}

	case openagent.TeamRetrying:
		msg := "retrying"
		if evt.Error != nil {
			msg = evt.Error.Error()
		}
		return SSEEvent{Type: "retrying", Agent: evt.Agent, Text: msg}

	case openagent.TeamHandoff:
		return SSEEvent{Type: "handoff", Agent: evt.Agent, HandoffTo: evt.Target}

	case openagent.TeamDone:
		se := SSEEvent{Type: "done"}
		if evt.Result != nil {
			se.FinalOutput = evt.Result.FinalOutput
			se.PromptTokens = evt.Result.Usage.PromptTokens
		}
		return se

	case openagent.TeamError:
		se := SSEEvent{Type: "error"}
		if evt.Error != nil {
			se.Text = evt.Error.Error()
		}
		return se

	default:
		return SSEEvent{}
	}
}

// ── Session TTL ──

// maybeStartReaper lazily starts a background goroutine that reclaims
// idle teamSessionState entries. It also cleans up per-agent private
// memory partitions (teamAgentMemory) so SQLite keys like
// sessionID::analyst don't leak. Team-shared messages are not deleted.
func (h *TeamHandler) maybeStartReaper() {
	if h.sessionIdleTTL <= 0 {
		return
	}
	h.reapOnce.Do(func() {
		go h.reapLoop()
	})
}

func (h *TeamHandler) reapLoop() {
	const interval = 1 * time.Minute
	for {
		time.Sleep(interval)
		h.reap()
	}
}

func (h *TeamHandler) reap() {
	now := time.Now()

	// Phase 1: collect expired IDs and their agentMems under read lock.
	h.mu.RLock()
	type toReap struct {
		id        string
		agentMems []*teamAgentMemory
	}
	var expired []toReap
	for id, s := range h.sessions {
		if now.Sub(s.lastAccess) > h.sessionIdleTTL {
			// Copy agentMems slice so we can use it after RUnlock.
			mems := make([]*teamAgentMemory, len(s.agentMems))
			copy(mems, s.agentMems)
			expired = append(expired, toReap{id: id, agentMems: mems})
		}
	}
	h.mu.RUnlock()

	if len(expired) == 0 {
		return
	}

	// Phase 2: under write lock, re-verify lastAccess (TOCTOU guard).
	h.mu.Lock()
	var confirmed []toReap
	for _, e := range expired {
		s, ok := h.sessions[e.id]
		if !ok {
			continue
		}
		if now.Sub(s.lastAccess) < h.sessionIdleTTL {
			continue // refreshed between RLock→Lock
		}
		delete(h.sessions, e.id)
		confirmed = append(confirmed, e)
	}
	h.mu.Unlock()

	// Phase 3: I/O outside the lock — clean up agent-private memory partitions.
	for _, e := range confirmed {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		for _, tam := range e.agentMems {
			_ = tam.DeleteSession(ctx, e.id)
		}
		cancel()
	}

	if len(confirmed) > 0 {
		log.Printf("team: reaped %d idle sessions", len(confirmed))
	}
}
