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

	// ── Load any existing summary unconditionally ──
	// Manual compaction (/compact) can leave a session whose remaining
	// messages fit the budget — without this load the summary would not
	// be injected and history would silently vanish from the prompt.
	// Auto-compaction below overwrites ci.compressed with the new summary.
	if rt.deps.Compressor != nil {
		if cc, err := rt.deps.Compressor.Compressed(ctx, session.ID); err == nil && cc != nil && cc.Summary != "" {
			ci.compressed = cc
		}
	}
	// Make the summary visible to the prompt-overhead estimate below
	// (estimatePromptOverhead reads rt.compressed, which the loop assigns
	// only AFTER prepareMemory returns). A freshly loaded summary must
	// count against this turn's budget, or a small-window model plus a big
	// summary overflows and hard-fails every run after /compact.
	if ci.compressed != nil {
		rt.compressed = ci.compressed
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
	// The token scan starts past the summary's coverage: already-compressed
	// messages stay in the store but must not consume the working budget —
	// counting them makes the overflow boundary drift into compressed
	// territory, where Compact no-ops and the summary stalls.
	overflow := len(msgs)
	tokens := 0
	startIdx := 0
	if ci.compressed != nil {
		if ti := ci.compressed.ThroughIndex - globalOffset; ti > startIdx {
			startIdx = ti
		}
	}
	for i := len(msgs) - 1; i >= startIdx; i-- {
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
			// messages=nil: the backend re-fetches from the session head.
			// globalCutoff is a GLOBAL message index, but msgs is the recent
			// window — the backend's prefetch branch only trusts a slice
			// that starts at the session head, so passing msgs here would
			// misalign (cut the wrong range, and the head would silently
			// vanish from both summary and working set).
			ci.err = rt.deps.Compressor.Compact(ctx, session.ID, globalCutoff, nil)
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

	// ── Working set: trim to token budget and past the summary ──
	// No compressor = trimming drops the head with no way to recover it
	// (fail-loud philosophy: the prompt hard window check errors instead
	// of silently forgetting). With a compressor, the start advances past
	// both the overflow point and the summary's coverage — otherwise a
	// session whose history fits the budget after /compact would inject
	// the full transcript AND a summary of the same transcript.
	if rt.deps.Compressor == nil {
		return msgs, ci, nil
	}
	start := overflow
	if start == len(msgs) {
		// No token trim this turn — start from the summary boundary
		// instead of the (meaningless) full-slice overflow point.
		start = 0
	}
	if ci.compressed != nil {
		if ti := ci.compressed.ThroughIndex - globalOffset; ti > start {
			start = ti
		}
	}
	// start must never overshoot: with an existing summary AND no overflow
	// this turn (history fits the budget), start advances only to the
	// summary's coverage — messages after it are NOT summarized and must
	// stay in the working set.
	if start > len(msgs) {
		start = len(msgs)
	}
	return msgs[start:], ci, nil
}
