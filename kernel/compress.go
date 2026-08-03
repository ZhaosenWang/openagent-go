package kernel

import (
	"context"

	openagent "github.com/yusheng-g/openagent-go"
)

// CompressStats reports the outcome of a manual compaction.
type CompressStats struct {
	Compressed    int // messages covered by the new summary
	FreedTokens   int // prompt tokens removed from the working set (approx)
	SummaryTokens int // tokens the new summary occupies
}

// CompressAll compacts the ENTIRE session history into a summary — the
// manual counterpart to prepareMemory's overflow-only auto-compaction.
// The next run starts from summary + working set (the summary is loaded
// unconditionally in prepareMemory, so it survives even when the remaining
// messages fit the budget).
//
// Returns nil stats when there is no compressor (compression unavailable).
// No-op on an empty session.
func (rt *Runtime) CompressAll(ctx context.Context, sessionID string) (*CompressStats, error) {
	// Same nil contract as prepareMemory: no store = no persistence.
	if rt.deps.SessionStore == nil {
		return nil, nil
	}
	if rt.deps.Compressor == nil {
		return nil, nil
	}

	total, err := rt.deps.SessionStore.Count(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if total == 0 {
		return &CompressStats{}, nil
	}
	fetchN := total
	if fetchN > 5000 {
		fetchN = 5000
	}
	msgs, err := rt.deps.SessionStore.Recent(ctx, sessionID, fetchN, 0)
	if err != nil || len(msgs) == 0 {
		return nil, err
	}
	globalOffset := total - len(msgs)

	// Record the pre-compaction marker so a repeat /compact (all history
	// already compressed) reports zero instead of claiming a fresh pass.
	oldTI := 0
	if cc, err := rt.deps.Compressor.Compressed(ctx, sessionID); err == nil && cc != nil {
		oldTI = cc.ThroughIndex
	}

	// Compress everything. SafeCompressionBoundary is a no-op at the full
	// range (all tool exchanges are already inside), so pairs stay intact.
	// messages=nil: the backend re-fetches from the session head, so the
	// global throughIndex always aligns (passing the recent-window slice
	// here would misalign when the session exceeds the fetch cap).
	globalCutoff := globalOffset + len(msgs)
	if err := rt.deps.Compressor.Compact(ctx, sessionID, globalCutoff, nil); err != nil {
		return nil, err
	}

	// Compact is a silent no-op without a summarizer (or nothing new to
	// compress) — verify the summary actually advanced before claiming
	// success.
	cc, err := rt.deps.Compressor.Compressed(ctx, sessionID)
	if err != nil || cc == nil || cc.ThroughIndex <= oldTI {
		return &CompressStats{}, nil
	}

	// Resolve the tokenizer from the effective run model (session override
	// wins), falling back to the config model outside a run. Config model
	// read under the lock: SetModel can run concurrently from the serve
	// loop while /compact is in flight.
	rt.mu.RLock()
	cfgModel := rt.cfg.Model
	rt.mu.RUnlock()
	model := rt.runModel
	if model == nil {
		model = cfgModel
	}
	modelID := openagent.TokenizerModelID(model)

	// Stats report only what THIS pass newly compressed: compaction is
	// incremental (Compact starts at the previous ThroughIndex), so a
	// second /compact after N messages + one exchange covers just the 2
	// new messages — reporting the whole session count would claim
	// already-summarized messages were compressed again.
	newStart := oldTI - globalOffset
	if newStart < 0 {
		newStart = 0
	}
	if newStart > len(msgs) {
		newStart = len(msgs)
	}
	newMsgs := msgs[newStart:]
	st := &CompressStats{
		Compressed:  cc.ThroughIndex - oldTI,
		FreedTokens: openagent.CountMessages(modelID, newMsgs),
	}
	st.SummaryTokens = openagent.CountMessageTokens(modelID, openagent.Message{
		Role:    openagent.RoleSystem,
		Content: cc.Summary,
	})
	st.FreedTokens -= st.SummaryTokens
	if st.FreedTokens < 0 {
		st.FreedTokens = 0
	}
	return st, nil
}

// ContextUsage reports the current per-layer context usage for a session:
// summary tokens (compressed history), working tokens (uncompressed
// messages), and the model's context window. Powers /context.
func (rt *Runtime) ContextUsage(ctx context.Context, sessionID string) (summary, working, window int, err error) {
	rt.mu.RLock()
	cfgModel := rt.cfg.Model
	rt.mu.RUnlock()
	model := rt.runModel
	if model == nil {
		model = cfgModel
	}
	modelID := openagent.TokenizerModelID(model)
	if rt.deps.Compressor != nil {
		if cc, err := rt.deps.Compressor.Compressed(ctx, sessionID); err == nil && cc != nil {
			summary = openagent.CountMessageTokens(modelID, openagent.Message{
				Role:    openagent.RoleSystem,
				Content: cc.Summary,
			})
		}
	}
	if rt.deps.SessionStore != nil {
		total, err := rt.deps.SessionStore.Count(ctx, sessionID)
		if err != nil {
			return 0, 0, window, err
		}
		msgs, err := rt.deps.SessionStore.Recent(ctx, sessionID, total, 0)
		if err == nil {
			working = openagent.CountMessages(modelID, msgs)
		}
	}
	if model != nil {
		window = model.ContextWindow()
	}
	return summary, working, window, nil
}
