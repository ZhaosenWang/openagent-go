// Package redact provides a RunHooks implementation that masks sensitive
// environment-variable values in tool results. Each configured env-var name
// is resolved at redaction time via os.Getenv; if its value appears in the
// tool result, every occurrence is replaced with "[REDACTED]" and a hint is
// appended telling the model the value was sensitive and must not be
// reconstructed.
//
// The hook stores env-var *names* only — secret values are never held in
// struct state and are resolved lazily so construction order (e.g. env set
// after agent creation) does not matter.
//
// # Coverage scope — read carefully
//
// This hook ONLY redacts the *result* (output) and *error* of a tool call.
// It does NOT redact:
//   - tool call *arguments* (args) — the LLM may place secrets in args
//     (e.g. shell command, http headers, DB DSN); those are stored in
//     memory and sent to the client unredacted.
//   - secrets that were not pre-registered as env-var names. Only the exact
//     value of a named env var is matched (plain substring, no regex).
//     PII, credit cards, AWS keys, private keys, JWTs, etc. are NOT
//     detected unless their value happens to be an env var listed here.
//   - secrets whose value contains JSON special characters (", \, control
//     chars) when the result is JSON. The env-var holds the raw (unescaped)
//     value, but inside a JSON string it is escaped (e.g. `"` → `\"`), so
//     the byte-level substring match silently fails and the secret passes
//     through. This is a known limitation of text-level matching; a
//     JSON-aware redactor would be needed to cover it.
//
// For a hook that redacts args or matches regex patterns, extend this
// package or compose another hook — that is out of scope for this one.
//
// # Ordering with other hooks
//
// redact must run FIRST (before slog, artifact) so downstream observers
// never see raw secrets in logs or on disk. This ordering is established by
// the caller in buildOpts, not by this hook.
//
// # Failure mode
//
// redaction never fails: it uses only os.Getenv + strings.ReplaceAll, which
// cannot error. If this hook is ever extended with regex or JSON parsing,
// the degradation policy is: on any internal failure, replace the whole
// result with a safe placeholder rather than return the original (leaking)
// data. Never fall back to the unredacted input.
//
// Usage:
//
//	agent := openagent.NewAgent("bot",
//	    openagent.WithRunHooks(redact.NewHook([]string{"AWS_SECRET_ACCESS_KEY"})),
//	    ...
//	)
package redact

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	openagent "github.com/yusheng-g/openagent-go"
)

// redacted is the placeholder substituted for every sensitive value.
const redacted = "[REDACTED]"

// hint is appended once to the result when any redaction occurred.
const hint = "\n\n[hint] 以上结果包含敏感信息，已脱敏。不要试图复原该值。"

// Hook masks sensitive env-var values in tool results. Implements
// openagent.RunHooks and ResultBufferingHook.
type Hook struct {
	envNames []string
}

// NewHook creates a Hook that redacts the values of the given env-var names.
// A nil or empty slice yields a hook that never modifies results.
func NewHook(envNames []string) *Hook {
	// Dedup and drop empty names so OnToolEnd does no redundant work and
	// os.Getenv("") is never called.
	seen := make(map[string]struct{}, len(envNames))
	clean := make([]string, 0, len(envNames))
	for _, n := range envNames {
		if n == "" {
			continue
		}
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		clean = append(clean, n)
	}
	return &Hook{envNames: clean}
}

// BufferToolResult reports true: this hook mutates tool results in OnToolEnd,
// so the runner must buffer streaming output and only expose the final,
// redacted result. Without buffering, raw secret-bearing chunks would reach
// the client/LLM before OnToolEnd runs. Implements ResultBufferingHook.
func (h *Hook) BufferToolResult() bool { return len(h.envNames) > 0 }

// OnAgentStart is a no-op.
func (h *Hook) OnAgentStart(ctx context.Context, req openagent.ChatCompletionRequest) (any, error) {
	return nil, nil
}

// OnAgentEnd is a no-op.
func (h *Hook) OnAgentEnd(ctx context.Context, req openagent.ChatCompletionRequest, resp *openagent.ChatCompletionResponse, runErr error, startState any) {
}

// OnToolStart is a no-op. Tool args are not redacted by this hook (see
// package doc — Coverage scope).
func (h *Hook) OnToolStart(ctx context.Context, tool openagent.FunctionDefinition, args json.RawMessage) (any, error) {
	return nil, nil
}

// OnToolEnd replaces every occurrence of each sensitive env-var value in
// *result with "[REDACTED]". If any replacement happened, a hint is appended
// once (idempotently). The error string is redacted likewise when *err != nil.
//
// Values are resolved lazily via os.Getenv so secrets never live in hook
// state and envvars set after construction are still honored. Empty values
// (unset or blank env vars) are skipped — redacting the empty string would
// corrupt every result.
//
// If the result is valid JSON, the hint is NOT appended as a trailing string
// (that would break JSON validity); the redaction still happens in-place
// and the result stays parseable.
func (h *Hook) OnToolEnd(ctx context.Context, tool openagent.FunctionDefinition, args json.RawMessage, result *string, err *error, startState any) {
	if len(h.envNames) == 0 {
		return
	}
	if result != nil && *result != "" {
		if r, ok := redactString(*result, h.envNames); ok {
			*result = applyHint(r, *result)
		}
	}
	if err != nil && *err != nil {
		if s, ok := redactString((*err).Error(), h.envNames); ok {
			// err is free-form text, never JSON-parsed downstream, so a
			// trailing hint is always safe.
			*err = fmt.Errorf("%s%s", s, hint)
		}
	}
}

// applyHint appends the redaction hint to s unless s already contains it
// (idempotent) or s is valid JSON (appending would break JSON validity).
// orig is the pre-redaction string, used only to avoid double-hinting when
// the original already carried a hint from a prior redaction.
func applyHint(s, orig string) string {
	if strings.Contains(orig, "[hint]") {
		// Already hinted in a prior pass — don't stack.
		return s
	}
	if json.Valid([]byte(s)) {
		// Result is (still) valid JSON: don't append trailing text.
		return s
	}
	return s + hint
}

// redactString replaces every sensitive value in s with "[REDACTED]" and
// reports whether any replacement occurred.
func redactString(s string, envNames []string) (string, bool) {
	changed := false
	for _, name := range envNames {
		v := os.Getenv(name)
		if v == "" {
			continue
		}
		if strings.Contains(s, v) {
			s = strings.ReplaceAll(s, v, redacted)
			changed = true
		}
	}
	return s, changed
}
