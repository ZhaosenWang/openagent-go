// Package artifact provides a RunHooks implementation that automatically
// saves large tool results to disk. When a single tool result exceeds
// 5% of the model's context window, it is saved to
// <ArtifactRoot()>/sess-<sessionID>/artifact-<rand>.txt and replaced with a
// short pointer. The model can then read or grep the file on demand.
//
// The session directory layout (<ArtifactRoot()>/sess-<sessionID>/) mirrors
// the process manager's per-session output dir, so all session-scoped
// ephemeral state lives under one path and can be cleaned together.
//
// The threshold is derived dynamically from the session's active model
// (session.Model.ContextWindow()). When the context window is unknown
// or the session is unavailable the threshold defaults to 128 KB.
//
// Usage:
//
//	agent := openagent.NewAgent("bot",
//	    openagent.WithRunHooks(artifact.NewHook()),
//	    ...
//	)
package artifact

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	openagent "github.com/yusheng-g/openagent-go"
	opentool "github.com/yusheng-g/openagent-go/tool"
)

// fraction is the percentage of the model's context window that a single
// tool result is allowed to consume before triggering an artifact save.
const fraction = 5

// Hook saves large tool results to disk. Implements openagent.RunHooks.
type Hook struct{}

// NewHook creates a Hook.
func NewHook() *Hook { return &Hook{} }

// OnAgentStart is a no-op.
func (h *Hook) OnAgentStart(ctx context.Context, req openagent.ChatCompletionRequest) (any, error) {
	return nil, nil
}

// OnAgentEnd is a no-op.
func (h *Hook) OnAgentEnd(ctx context.Context, req openagent.ChatCompletionRequest, resp *openagent.ChatCompletionResponse, runErr error, startState any) {
}

// OnToolStart is a no-op.
func (h *Hook) OnToolStart(ctx context.Context, tool openagent.FunctionDefinition, args json.RawMessage) (any, error) {
	return nil, nil
}

// OnToolEnd checks the result size against the model's context window.
// If it exceeds 5% of the window, the result is saved to disk and
// replaced with a pointer.
func (h *Hook) OnToolEnd(ctx context.Context, tool openagent.FunctionDefinition, args json.RawMessage, result *string, err *error, startState any) {
	if result == nil || *result == "" {
		return
	}
	if isReadingArtifact(args) {
		return
	}

	threshold := 128 << 10 // fallback default
	session, ok := openagent.SessionFromContext(ctx)
	if ok && session.Model != nil {
		if cw := session.Model.ContextWindow(); cw > 0 {
			threshold = cw * fraction / 100
		}
	}

	if len(*result) <= threshold {
		return
	}

	dir := filepath.Join(opentool.ArtifactRoot(), sessionDirName(session.ID))
	if err := os.MkdirAll(dir, 0755); err != nil {
		return
	}

	name := fmt.Sprintf("artifact-%s.txt", randHex(8))
	path := filepath.Join(dir, name)

	raw := *result
	if err := os.WriteFile(path, []byte(raw), 0644); err != nil {
		return
	}

	sizeKB := (len(raw) + 1023) / 1024
	lines := strings.Count(raw, "\n") + 1
	*result = fmt.Sprintf(
		"Output saved to %s (%d KB, %d lines). Use read or grep to search.",
		path, sizeKB, lines,
	)
}

func isReadingArtifact(args json.RawMessage) bool {
	var params struct {
		Path string `json:"path"`
	}
	json.Unmarshal(args, &params)
	return params.Path != "" && strings.HasPrefix(params.Path, opentool.ArtifactRoot())
}

// sessionDirName returns the per-session artifact directory name. It uses
// the same "sess-<sessionID>" layout as the process manager so all of a
// session's ephemeral state (background processes + artifacts) lives
// under one sibling directory and can be cleaned together. sessionID is
// sanitized defensively in case a client hands an id containing a path
// separator.
func sessionDirName(sessionID string) string {
	return "sess-" + sanitize(sessionID)
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// rand.Read shouldn't fail in practice; fall back to a stable
		// but still-acceptable name rather than dropping the artifact.
		return "0000000000000000"[:n*2]
	}
	return hex.EncodeToString(b)
}

// sanitize replaces path separators (and NUL) with '_' so a hostile or
// malformed session id cannot escape its session directory.
func sanitize(name string) string {
	return strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == ':' || r == 0 {
			return '_'
		}
		return r
	}, name)
}

// Remove cleans up the artifact directory for a session.
func Remove(sessionID string) error {
	return os.RemoveAll(filepath.Join(opentool.ArtifactRoot(), sessionDirName(sessionID)))
}
