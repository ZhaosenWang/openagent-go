package redact

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	openagent "github.com/yusheng-g/openagent-go"
)

// withEnv sets env vars for a test; t.Setenv handles save/restore.
func withEnv(t *testing.T, kv ...string) {
	t.Helper()
	if len(kv)%2 != 0 {
		t.Fatal("withEnv: odd number of kv args")
	}
	for i := 0; i < len(kv); i += 2 {
		t.Setenv(kv[i], kv[i+1])
	}
}

func TestOnToolEnd_NoEnvNames_NoOp(t *testing.T) {
	h := NewHook(nil)
	out := "secret: abc"
	res := out
	h.OnToolEnd(context.Background(), openagent.FunctionDefinition{}, nil, &res, nil, nil)
	if res != out {
		t.Fatalf("nil envNames modified result: %q", res)
	}

	h2 := NewHook([]string{})
	res2 := out
	h2.OnToolEnd(context.Background(), openagent.FunctionDefinition{}, nil, &res2, nil, nil)
	if res2 != out {
		t.Fatalf("empty envNames modified result: %q", res2)
	}
}

func TestOnToolEnd_EmptyEnvValue_Skipped(t *testing.T) {
	// An unset/empty env var must be skipped — redacting "" would insert
	// "[REDACTED]" between every rune and corrupt all output.
	withEnv(t, "REDACT_EMPTY", "")
	h := NewHook([]string{"REDACT_EMPTY"})
	out := "nothing to see here"
	res := out
	h.OnToolEnd(context.Background(), openagent.FunctionDefinition{}, nil, &res, nil, nil)
	if res != out {
		t.Fatalf("empty-valued env var corrupted result: %q", res)
	}
}

func TestOnToolEnd_EmptyResult_ShortCircuit(t *testing.T) {
	// Empty result must not enter the redaction loop (🟡-1).
	withEnv(t, "REDACT_ER", "x")
	h := NewHook([]string{"REDACT_ER"})
	res := ""
	h.OnToolEnd(context.Background(), openagent.FunctionDefinition{}, nil, &res, nil, nil)
	if res != "" {
		t.Fatalf("empty result modified: %q", res)
	}
}

func TestOnToolEnd_SingleHit_RedactedWithHint(t *testing.T) {
	withEnv(t, "REDACT_TOKEN", "supersecret")
	h := NewHook([]string{"REDACT_TOKEN"})
	res := "the token is supersecret and that is bad"
	h.OnToolEnd(context.Background(), openagent.FunctionDefinition{}, nil, &res, nil, nil)
	if strings.Contains(res, "supersecret") {
		t.Fatalf("secret leaked in result: %q", res)
	}
	if !strings.Contains(res, "[REDACTED]") {
		t.Fatalf("result missing [REDACTED]: %q", res)
	}
	if !strings.Contains(res, "[hint]") {
		t.Fatalf("result missing hint: %q", res)
	}
}

func TestOnToolEnd_MultipleOccurrences_AllReplaced(t *testing.T) {
	withEnv(t, "REDACT_TOK", "abc")
	h := NewHook([]string{"REDACT_TOK"})
	res := "abc and abc and again abc"
	h.OnToolEnd(context.Background(), openagent.FunctionDefinition{}, nil, &res, nil, nil)
	want := strings.ReplaceAll("abc and abc and again abc", "abc", "[REDACTED]") + hint
	if res != want {
		t.Fatalf("got %q want %q", res, want)
	}
}

func TestOnToolEnd_MultipleSecrets_HintAppendedOnce(t *testing.T) {
	withEnv(t, "REDACT_A", "AAA", "REDACT_B", "BBB")
	h := NewHook([]string{"REDACT_A", "REDACT_B"})
	res := "AAA here, BBB there, AAA again"
	h.OnToolEnd(context.Background(), openagent.FunctionDefinition{}, nil, &res, nil, nil)
	if strings.Contains(res, "AAA") || strings.Contains(res, "BBB") {
		t.Fatalf("secret leaked: %q", res)
	}
	if c := strings.Count(res, "[hint]"); c != 1 {
		t.Fatalf("hint count = %d, want 1: %q", c, res)
	}
}

func TestOnToolEnd_NoHit_Unchanged(t *testing.T) {
	withEnv(t, "REDACT_X", "xyz")
	h := NewHook([]string{"REDACT_X"})
	out := "completely unrelated output"
	res := out
	h.OnToolEnd(context.Background(), openagent.FunctionDefinition{}, nil, &res, nil, nil)
	if res != out {
		t.Fatalf("no-hit result modified: %q", res)
	}
}

func TestOnToolEnd_NilResult_NoPanic(t *testing.T) {
	withEnv(t, "REDACT_N", "n")
	h := NewHook([]string{"REDACT_N"})
	h.OnToolEnd(context.Background(), openagent.FunctionDefinition{}, nil, nil, nil, nil)
}

func TestOnToolEnd_ErrorString_Redacted(t *testing.T) {
	withEnv(t, "REDACT_E", "leaked-in-error")
	h := NewHook([]string{"REDACT_E"})
	e := errors.New("tool failed: leaked-in-error")
	h.OnToolEnd(context.Background(), openagent.FunctionDefinition{}, nil, nil, &e, nil)
	if e == nil {
		t.Fatal("err became nil")
	}
	if strings.Contains(e.Error(), "leaked-in-error") {
		t.Fatalf("secret leaked in error: %q", e.Error())
	}
	if !strings.Contains(e.Error(), "[REDACTED]") {
		t.Fatalf("error missing [REDACTED]: %q", e.Error())
	}
}

func TestOnToolEnd_LazyResolution_EnvSetAfterConstruction(t *testing.T) {
	// Hook constructed before the env var exists; value set after must
	// still be honored at redaction time (lazy os.Getenv).
	t.Setenv("REDACT_LATE", "")
	h := NewHook([]string{"REDACT_LATE"})
	t.Setenv("REDACT_LATE", "late-value")
	res := "contains late-value here"
	h.OnToolEnd(context.Background(), openagent.FunctionDefinition{}, nil, &res, nil, nil)
	if strings.Contains(res, "late-value") {
		t.Fatalf("lazily-resolved secret leaked: %q", res)
	}
}

func TestOnToolEnd_JSONResult_NoTrailingHint(t *testing.T) {
	// A JSON result must stay valid JSON after redaction — no trailing
	// hint appended (🟡-3).
	withEnv(t, "REDACT_J", "supersecret")
	h := NewHook([]string{"REDACT_J"})
	res := `{"token":"supersecret","ok":true}`
	h.OnToolEnd(context.Background(), openagent.FunctionDefinition{}, nil, &res, nil, nil)
	if strings.Contains(res, "supersecret") {
		t.Fatalf("secret leaked: %q", res)
	}
	if !strings.Contains(res, "[REDACTED]") {
		t.Fatalf("missing [REDACTED]: %q", res)
	}
	if strings.Contains(res, "[hint]") {
		t.Fatalf("hint appended to JSON result, breaking validity: %q", res)
	}
}

func TestOnToolEnd_HintIdempotent_AlreadyHinted(t *testing.T) {
	// If the result already carries a hint (e.g. reprocessed), don't
	// stack a second one (🟡-2).
	withEnv(t, "REDACT_I", "sec")
	h := NewHook([]string{"REDACT_I"})
	res := "sec here" + hint
	h.OnToolEnd(context.Background(), openagent.FunctionDefinition{}, nil, &res, nil, nil)
	if c := strings.Count(res, "[hint]"); c != 1 {
		t.Fatalf("hint count = %d, want 1 (idempotent): %q", c, res)
	}
}

func TestNewHook_DedupAndDropEmpty(t *testing.T) {
	// 🔵-1: duplicate and empty names are collapsed.
	withEnv(t, "REDACT_D", "val")
	h := NewHook([]string{"REDACT_D", "REDACT_D", "", "REDACT_D"})
	res := "val"
	h.OnToolEnd(context.Background(), openagent.FunctionDefinition{}, nil, &res, nil, nil)
	if strings.Contains(res, "val") {
		t.Fatalf("secret leaked despite dup/empty names: %q", res)
	}
	// Functional check: envNames deduped to 1, no panic, no double hint.
	if c := strings.Count(res, "[hint]"); c != 1 {
		t.Fatalf("hint count = %d, want 1: %q", c, res)
	}
}

func TestOnToolEnd_ConcurrentSafe(t *testing.T) {
	// 🟡-4: multiple goroutines using the same Hook must be safe.
	// Hook.envNames is read-only and os.Getenv is concurrency-safe; this
	// test pins the invariant so future mutable state (e.g. regex cache)
	// won't silently introduce a data race.
	withEnv(t, "REDACT_C", "concurrent-secret")
	h := NewHook([]string{"REDACT_C"})
	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			res := "prefix concurrent-secret suffix"
			h.OnToolEnd(context.Background(), openagent.FunctionDefinition{}, nil, &res, nil, nil)
			if strings.Contains(res, "concurrent-secret") {
				t.Errorf("secret leaked under concurrency")
			}
		}()
	}
	wg.Wait()
}
