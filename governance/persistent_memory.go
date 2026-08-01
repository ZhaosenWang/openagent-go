package governance

import (
	"context"
	"fmt"
	"sync"

	"github.com/yusheng-g/openagent-go/session"
)

// PersistentApprovalMemory persists session-scoped approval decisions
// ("allow always") in the session metadata store, so decisions survive
// server restarts — the Allow-Always contract holds across process
// lifetimes, not just within one run.
//
// Decisions are stored under the session's "_meta.approvals" map, keyed
// by tool name. The in-process fallback (used when no store is wired)
// keeps the same semantics within the process.
type PersistentApprovalMemory struct {
	store    session.Runtime
	fallback ApprovalMemory
	mu       sync.Mutex // serializes read-modify-write on session meta
}

// NewPersistentApprovalMemory creates a store-backed approval memory.
// When store is nil it degrades to an in-process memory (decisions last
// for the process lifetime).
func NewPersistentApprovalMemory(store session.Runtime) *PersistentApprovalMemory {
	return &PersistentApprovalMemory{
		store:    store,
		fallback: NewSessionApprovalMemory(),
	}
}

// approvalsKey is the session metadata key holding the approvals map.
const approvalsKey = "approvals"

// Remember implements ApprovalMemory.
func (m *PersistentApprovalMemory) Remember(ctx context.Context, sessionID, key string, decision Decision) error {
	if m.store == nil {
		return m.fallback.Remember(ctx, sessionID, key, decision)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	info, err := m.store.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("approval memory: get session: %w", err)
	}
	if info == nil {
		// Session not yet persisted — create a minimal record so the
		// decision has somewhere to live.
		info = &session.SessionInfo{ID: sessionID, Meta: map[string]any{}}
	}

	approvals := map[string]Decision{}
	if got, ok := session.GetMeta[map[string]Decision](*info, approvalsKey); ok {
		approvals = got
	}
	approvals[key] = decision
	info.SetMeta(approvalsKey, approvals)

	if err := m.store.Save(ctx, *info); err != nil {
		return fmt.Errorf("approval memory: save session: %w", err)
	}
	return nil
}

// Recall implements ApprovalMemory.
func (m *PersistentApprovalMemory) Recall(ctx context.Context, sessionID, key string) (Decision, bool) {
	if m.store == nil {
		return m.fallback.Recall(ctx, sessionID, key)
	}

	info, err := m.store.Get(ctx, sessionID)
	if err != nil || info == nil {
		return Decision{}, false
	}
	approvals, ok := session.GetMeta[map[string]Decision](*info, approvalsKey)
	if !ok {
		return Decision{}, false
	}
	d, ok := approvals[key]
	return d, ok
}
