package kernel

import (
	"context"
	"time"

	openagent "github.com/yusheng-g/openagent-go"
)

// compactionInfo carries compaction observability back to the loop.
type compactionInfo struct {
	err        error
	count      int                          // number of messages newly compressed
	from, to   int                          // global index range covered (for observability)
	compressed *openagent.CompressedContext // summary after this pass (nil if none)
}

// workingTokenBudget returns the token budget for the working message set.
// Explicit MaxWorkingTokens wins; otherwise 70% of the model context
// window; falls back to 20000.
func (rt *Runtime) workingTokenBudget() int {
	if rt.cfg.MaxWorkingTokens > 0 {
		return rt.cfg.MaxWorkingTokens
	}
	if cw := rt.runModel.ContextWindow(); cw > 0 {
		return cw * 7 / 10 // 70%
	}
	return 20000
}

// prepareMemory fetches the working message set, triggers token-based
// compaction if needed, and trims to the token budget. Messages are NEVER
// deleted — compaction only updates the summary.
//
// The returned compactionInfo.err carries a compaction failure if one
// occurred (observability only; the working set is still usable).
func (rt *Runtime) prepareMemory(ctx context.Context, session openagent.Session) ([]openagent.Message, compactionInfo, error) {
	var ci compactionInfo
	if rt.deps.SessionStore == nil {
		return nil, ci, nil
	}

	budget := rt.workingTokenBudget()

	// ── Subtract fixed overhead that the prompt adds ──
	modelID := openagent.TokenizerModelID(rt.runModel)
	overhead := rt.estimatePromptOverhead(ctx, session, modelID)
	budget -= overhead
	if budget < 500 {
		budget = 500 // keep a minimal working window
	}

	// Fetch total count and recent messages — one Recent() for both
	// compaction and working-set trimming.
	totalCount, err := rt.deps.SessionStore.Count(ctx, session.ID)
	if err != nil {
		rt.observe(ctx, openagent.StageMemoryFetch, "leave",
			map[string]any{"error": err.Error()}, time.Now(), err)
		return nil, ci, err
	}
	if totalCount == 0 {
		return nil, ci, nil
	}
	fetchN := totalCount
	if fetchN > 5000 {
		fetchN = 5000
	}
	msgs, err := rt.deps.SessionStore.Recent(ctx, session.ID, fetchN, 0)
	if err != nil || len(msgs) == 0 {
		return nil, ci, err
	}
	globalOffset := totalCount - len(msgs)

	// ── Compaction pass: compress overflow messages ──
	overflow := len(msgs)
	tokens := 0
	for i := len(msgs) - 1; i >= 0; i-- {
		tokens += openagent.CountMessageTokens(openagent.TokenizerModelID(rt.runModel), msgs[i])
		if tokens > budget {
			overflow = i + 1
			break
		}
	}
	if overflow < len(msgs) {
		overflow = openagent.SafeCompressionBoundary(msgs, overflow)
		oldTI := 0
		if rt.deps.Compressor != nil {
			if cc, err := rt.deps.Compressor.Compressed(ctx, session.ID); err == nil && cc != nil {
				oldTI = cc.ThroughIndex
			}
		}
		globalCutoff := globalOffset + overflow
		if rt.deps.Compressor != nil {
			ci.err = rt.deps.Compressor.Compact(ctx, session.ID, globalCutoff, msgs)
		}
		if ci.err == nil && rt.deps.Compressor != nil {
			if cc, err := rt.deps.Compressor.Compressed(ctx, session.ID); err == nil && cc != nil {
				ci.compressed = cc
				if cc.ThroughIndex > oldTI {
					ci.count = cc.ThroughIndex - oldTI
					ci.from = globalOffset + oldTI
					ci.to = globalOffset + cc.ThroughIndex
				}
			}
		}
	}

	// ── Working set: trim to token budget ──
	if overflow >= len(msgs) {
		return msgs, ci, nil
	}
	return msgs[overflow:], ci, nil
}
