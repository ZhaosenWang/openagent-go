// Package tokenizer provides model-aware token counting using tiktoken.
//
// It maps model IDs to the correct BPE encoding (o200k_base for GPT-4o,
// cl100k_base for GPT-4/3.5, etc.) and caches encoder instances. For
// non-OpenAI models whose tokenizer is unknown, it falls back to cl100k_base
// which provides a reasonable approximation for English text.
//
// This package only works with strings. Message-level counting (including
// tool calls and formatting overhead) lives in the root openagent package.
package tokenizer

import (
	"log/slog"
	"sync"

	"github.com/pkoukk/tiktoken-go"
)

var (
	mu    sync.RWMutex
	cache = make(map[string]*tiktoken.Tiktoken)
)

// ForModel returns a Tiktoken instance for the given model ID.
// Falls back to cl100k_base when the model's tokenizer is unknown.
func ForModel(modelID string) *tiktoken.Tiktoken {
	// Fast path: cache hit.
	mu.RLock()
	tke, ok := cache[modelID]
	mu.RUnlock()
	if ok {
		return tke
	}

	mu.Lock()
	defer mu.Unlock()

	// Double-check (another goroutine may have populated it).
	if tke, ok = cache[modelID]; ok {
		return tke
	}

	// Try model-specific encoding first.
	tke, err := tiktoken.EncodingForModel(modelID)
	if err != nil {
		// Unknown model — fall back to cl100k_base (reasonable for most
		// modern models that use a GPT-4-like tokenizer). All unknown
		// models share ONE cl100k instance (cached under a canonical key)
		// instead of one duplicate encoding per model name; the original
		// key still points at the same instance for the fast path.
		key := modelID
		modelID = "cl100k_base"
		tke, err = tiktoken.GetEncoding(tiktoken.MODEL_CL100K_BASE)
		if err != nil {
			// Absolute last resort: o200k_base is always available.
			tke, err = tiktoken.GetEncoding(tiktoken.MODEL_O200K_BASE)
			modelID = "o200k_base"
			if err != nil {
				slog.Warn("openagent: tokenizer unavailable", "error", err)
				cache[key] = nil
				return nil
			}
		}
		cache[key] = tke
	}

	cache[modelID] = tke
	return tke
}

// Count returns the token count for text using the tokenizer appropriate
// for the given model ID. Returns 0 for empty strings.
//
// If the tokenizer cannot be loaded (e.g. no network for downloading encoder
// files on first use), it falls back to a heuristic (~4 chars per token for
// ASCII, ~1.5 for CJK). This is safe but less accurate.
func Count(modelID, text string) (n int) {
	if text == "" {
		return 0
	}

	tke := ForModel(modelID)
	if tke == nil {
		return heuristicCount(text)
	}

	// Recover around the encode call only: a tiktoken panic (nil encoder,
	// malformed input) falls back to the heuristic — without swallowing
	// unrelated panics from other code.
	defer func() {
		if r := recover(); r != nil {
			slog.Warn("openagent: tiktoken encode panicked, using heuristic", "model", modelID, "panic", r)
			n = heuristicCount(text)
		}
	}()
	return len(tke.EncodeOrdinary(text))
}

// heuristicCount is a fast fallback (~4 chars/token, conservative for CJK).
func heuristicCount(text string) int {
	n := 0
	for _, c := range text {
		if c > 127 {
			n += 2 // CJK and other multibyte: ~1.5-2 chars/token
		} else {
			n += 1
		}
	}
	return n/4 + 1
}
