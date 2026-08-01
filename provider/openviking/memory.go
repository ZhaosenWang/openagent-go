package openviking

import (
	"context"

	ctxpkg "github.com/yusheng-g/openagent-go/context"
)

// Memory implements context.MemoryProvider backed by OpenViking's memory
// index: Recall runs the server's semantic search (find, memory type),
// Store persists a message via remember.
type Memory struct {
	client *Client
}

// NewMemory creates the memory provider.
func NewMemory(client *Client) *Memory {
	return &Memory{client: client}
}

// Recall implements context.MemoryProvider.
func (m *Memory) Recall(ctx context.Context, scope ctxpkg.ContextScope, query string, limit int) ([]ctxpkg.MemoryEntry, error) {
	items, err := m.client.Search(ctx, query, limit, "memory")
	if err != nil {
		return nil, err
	}
	entries := make([]ctxpkg.MemoryEntry, 0, len(items))
	for _, it := range items {
		entries = append(entries, ctxpkg.MemoryEntry{
			Kind:    ctxpkg.MemoryKind(it.Kind),
			Content: it.Content,
			Score:   it.Score,
		})
	}
	return entries, nil
}

// Store implements context.MemoryProvider. OpenViking's memory is a
// shared long-term knowledge base scoped by the server's own identity
// (account/user), not by ContextScope — the scope is not applied on the
// wire. Deployments needing per-user isolation scope the server side.
func (m *Memory) Store(ctx context.Context, _ ctxpkg.ContextScope, item ctxpkg.MemoryItem) error {
	_, err := m.client.Remember(ctx, item.Content)
	return err
}
