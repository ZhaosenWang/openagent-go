// Package rest provides HTTP handlers and types for openagent-go agents.
//
// SessionStore persists session metadata across server restarts.
// nil SessionStore preserves the current in-memory-only behavior.
package rest

import "context"

// SessionStore persists session metadata (SessionInfo) across server restarts.
// Implementations must be safe for concurrent use.
type SessionStore interface {
	// Save upserts session metadata.
	Save(ctx context.Context, info SessionInfo) error

	// Get returns the session metadata, or (nil, nil) if not found.
	Get(ctx context.Context, id string) (*SessionInfo, error)

	// List returns all sessions of the given kind ("single", "team", "plan").
	List(ctx context.Context, kind string) ([]SessionInfo, error)

	// Delete removes session metadata. No-op if the session doesn't exist.
	Delete(ctx context.Context, id string) error

	// Close releases resources held by the store.
	Close() error
}
