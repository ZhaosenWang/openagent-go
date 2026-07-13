package openagent

import (
	"context"
	"io"
)

// Memory stores and retrieves conversation history. Three-layer model:
//
//	Layer 1: Working    — recent context, kept verbatim. The Runner manages
//	                       the working set by token budget, not message count.
//	                       Recent() is a pure query — it does not trigger compaction.
//	Layer 2: Compressed — summary + retrieval hints for history beyond Working.
//	                       Updated by Compact() when the Runner detects the working
//	                       set exceeds the token budget (incremental/rolling compression).
//	Layer 3: Archive    — full history, searchable on demand via Search() and
//	                       the recall_memory tool. Original messages are NEVER deleted.
//
// All methods are scoped by sessionID. nil Memory = no persistence,
// each run starts fresh.
//
// Memory embeds io.Closer — implementations that hold external resources
// (database connections, file handles) must release them in Close().
// Callers that receive a Memory interface should defer Close() to prevent
// resource leaks. Implementations that don't hold resources may return nil.
type Memory interface {
	io.Closer

	// Layer 1: Working memory — returns up to n most recent messages, skipping
	// the first offset messages from the end. offset=0 returns the latest n.
	Recent(ctx context.Context, sessionID string, n int, offset int) ([]Message, error)

	// Count returns the total number of stored messages for a session.
	Count(ctx context.Context, sessionID string) (int, error)

	// Layer 2: Compact compresses messages up to throughIndex into a summary.
	// messages is an optional pre-fetched slice (from the caller) to avoid
	// a redundant read. When nil, the backend fetches messages internally.
	// The backend records throughIndex as the new ThroughIndex marker.
	// Original messages are NEVER deleted.
	Compact(ctx context.Context, sessionID string, throughIndex int, messages []Message) error

	// Compressed returns the stored CompressedContext, or nil if none exists.
	Compressed(ctx context.Context, sessionID string) (*CompressedContext, error)

	// Layer 3: Archive search — full-text / vector search over all stored messages.
	Search(ctx context.Context, sessionID, query string, limit int) ([]SearchResult, error)

	// Append adds a message to the conversation history.
	Append(ctx context.Context, sessionID string, msg Message) error

	// DeleteSession removes all data for the given session. After this call,
	// Recent, Compressed, Search, Compact, and Append all operate on a clean slate.
	// It is safe to call on a session that doesn't exist.
	DeleteSession(ctx context.Context, sessionID string) error
}

// CompressedContext bundles a summary with retrieval hints for the model.
type CompressedContext struct {
	Summary      string          `json:"summary"`
	Hints        []RetrievalHint `json:"hints"`
	ThroughIndex int             `json:"through_index"`
	// ThroughIndex marks how many messages have been covered by this summary.
	// The next compression pass only compresses messages after this index.
	// 0 means no compression has occurred (or the summary was produced by
	// an older version that didn't track this value).
}

// SearchResult is a single match from Memory.Search.
type SearchResult struct {
	Message  Message `json:"message"`
	Score    float64 `json:"score"`
	Turn     int     `json:"turn"`
}

// SafeCompressionBoundary adjusts the overflow index so compression doesn't
// break tool_call/tool_result pairs. If the last message in the compression
// range is an assistant with tool_calls, the boundary extends forward to
// include all consecutive tool results so the summary captures the complete
// tool exchange. all is in chronological order.
//
// Returns the adjusted overflow index (may be larger than input).
func SafeCompressionBoundary(all []Message, overflow int) int {
	if overflow <= 0 || overflow >= len(all) {
		return overflow
	}

	lastCompressed := all[overflow-1]

	// If the last compressed message is an assistant with tool_calls,
	// its tool results (RoleTool) are in the working window. Extend
	// the boundary to include them so the summary captures the complete
	// tool exchange.
	if lastCompressed.Role == RoleAssistant && len(lastCompressed.ToolCalls) > 0 {
		for i := overflow; i < len(all); i++ {
			if all[i].Role == RoleTool {
				overflow = i + 1
			} else {
				break
			}
		}
	}

	return overflow
}
