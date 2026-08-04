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
	// enter/leave pair for the memory-fetch stage (leave duration covers
	// compaction + working-set trim; the detail reports the fetch window —
	// total stored messages vs the post-summary increment actually read —
	// plus the compaction outcome of this pass).
	var (
		totalCount int
		from       int
		msgs       []openagent.Message
		err        error
	)
	start := time.Now()
	rt.observe(ctx, openagent.StageMemoryFetch, "enter", nil, time.Time{}, nil)
	defer func() {
		rt.observe(ctx, openagent.StageMemoryFetch, "leave", map[string]any{
			"total":     totalCount,
			"from":      from,
			"fetched":   len(msgs),
			"compacted": ci.count, // messages newly covered by the summary
			"comp_from": ci.from,  // global index range of the new summary
			"comp_to":   ci.to,    // (exclusive end of the range)
		}, start, err)
	}()

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

	// Fetch total count and the post-summary increment. Messages are never
	// deleted, so RecentAfter skips everything the summary already covers
	// (no 5000 cap: the overflow scan below must see every un-summarized
	// message to decide the boundary, and the head can no longer fall
	// outside the fetch window).
	totalCount, err = rt.deps.SessionStore.Count(ctx, session.ID)
	if err != nil {
		return nil, ci, err
	}
	if totalCount == 0 {
		return nil, ci, nil
	}
	from = 0
	if ci.compressed != nil {
		from = ci.compressed.ThroughIndex
	}
	msgs, err = rt.deps.SessionStore.RecentAfter(ctx, session.ID, from, totalCount-from)
	if err != nil || len(msgs) == 0 {
		return nil, ci, err
	}
	// msgs starts at global index `from` (RecentAfter does no trimming),
	// so local index == global index - from.

	// ── Compaction pass: compress overflow messages ──
	// The token scan walks only the post-summary increment (already
	// compressed messages are not fetched at all).
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
		globalCutoff := from + overflow
		if rt.deps.Compressor != nil {
			// messages=nil: the backend re-fetches from the session head.
			// globalCutoff is a GLOBAL message index, but msgs is the
			// post-summary window — the backend's prefetch branch only
			// trusts a slice that starts at the session head, so passing
			// msgs here would misalign.
			ci.err = rt.deps.Compressor.Compact(ctx, session.ID, globalCutoff, nil)
		}
		if rt.deps.Compressor != nil {
			if ci.err == nil {
				if cc, err := rt.deps.Compressor.Compressed(ctx, session.ID); err == nil && cc != nil {
					ci.compressed = cc
					if cc.ThroughIndex > oldTI {
						ci.count = cc.ThroughIndex - oldTI
						ci.from = oldTI
						ci.to = cc.ThroughIndex
						// The backend may advance ThroughIndex past the
						// overflow point (SafeCompressionBoundary
						// adjustments). The working set must not re-inject
						// what the NEW summary already covers — the summary
						// and the raw messages would double-count against
						// this turn's budget.
						if ti := cc.ThroughIndex - from; ti > overflow {
							overflow = ti
							if overflow > len(msgs) {
								overflow = len(msgs)
							}
						}
					} else {
						// Compact was a silent no-op (no summarizer
						// configured, or nothing new to compress): trimming
						// here would drop the head with no summary to cover
						// it — those messages would vanish from the prompt
						// forever. Keep the whole working set instead; the
						// prompt hard window check errors rather than
						// silently forgetting (fail-loud).
						overflow = len(msgs)
					}
				} else {
					// No summary came back (read failed / backend has no
					// marker): same no-advance treatment.
					overflow = len(msgs)
				}
			} else {
				// Compaction failed: trimming would drop the head with no
				// summary to cover it — keep the whole working set and let
				// the hard window check surface the overflow (fail-loud).
				overflow = len(msgs)
			}
		}
	}

	// ── Working set: trim to token budget ──
	// No compressor = trimming drops the head with no way to recover it
	// (fail-loud philosophy: the prompt hard window check errors instead
	// of silently forgetting). With a compressor, msgs already excludes
	// the OLD summary's coverage; overflow carries either the token-trim
	// point or the new summary's coverage (whichever is later — the
	// compaction pass above reconciles them).
	//
	// ci.count > 0 is the discriminator for the overflow == len(msgs)
	// cases: budget fits / compaction did NOT advance (keep everything —
	// trimming would drop the head with no summary to cover it) vs.
	// compaction advanced and the summary covers everything (working set
	// must be empty, or the summary and the raw messages double-count).
	if rt.deps.Compressor == nil {
		return msgs, ci, nil
	}
	if ci.count == 0 {
		return msgs, ci, nil
	}
	return msgs[overflow:], ci, nil
}
