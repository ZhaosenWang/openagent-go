package openagent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeWindowModel is a Model with a fixed, small context window so the
// result policy threshold (cw * artifactFraction / 100) can be made
// trivially small in tests without writing megabytes of "result".
type fakeWindowModel struct{ cw int }

func (m *fakeWindowModel) ChatCompletion(context.Context, ChatCompletionRequest) (*ChatCompletionResponse, error) {
	return nil, nil
}
func (m *fakeWindowModel) ChatCompletionStream(context.Context, ChatCompletionRequest) (StreamReader, error) {
	return nil, nil
}
func (m *fakeWindowModel) ContextWindow() int { return m.cw }

// TestDefaultResultPolicy_UsesSessionModelContextWindow asserts the built-in
// result policy reads the context window from session.Model, truncating
// oversized tool output by saving it to disk and replacing Content with a
// pointer.
func TestDefaultResultPolicy_UsesSessionModelContextWindow(t *testing.T) {
	scratch := t.TempDir()
	t.Setenv("TMPDIR", scratch)
	root := ArtifactRoot()
	if !strings.HasPrefix(root, scratch) {
		t.Fatalf("ArtifactRoot=%q not under TMPDIR scratch %q", root, scratch)
	}

	// Window = 10_000 tokens → threshold = 10_000 * 5 / 100 = 500 tokens.
	// 5000 ASCII bytes ≈ 625 tokens > 500 → saved; 1000 bytes ≈ 250
	// tokens < 500 → untouched.
	const big = 5000
	const small = 1000

	sess := Session{
		ID:    "s-test",
		Model: &fakeWindowModel{cw: 10_000},
	}
	ctx := context.Background()

	// Big result → truncated, saved to disk, FileRef set.
	bigRes := strings.Repeat("x", big)
	policy := &DefaultResultPolicy{}
	res := policy.Apply(ctx, sess, &ToolResult{Content: bigRes})
	if !res.Truncated {
		t.Fatal("big result not flagged Truncated")
	}
	if res.FileRef == "" {
		t.Fatal("big result missing FileRef")
	}
	if strings.Contains(res.Content, strings.Repeat("x", 100)) {
		t.Fatal("Content still carries the raw big output")
	}
	// Layout: <ArtifactRoot()>/sess-<sessionID>/artifact-<8hex>.txt
	sessDir := filepath.Join(root, "sess-"+sess.ID)
	matches, err := filepath.Glob(filepath.Join(sessDir, "artifact-*.txt"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected 1 saved artifact, got %d (%v)", len(matches), matches)
	}
	if name := filepath.Base(matches[0]); len(name) <= len("artifact-.txt") {
		t.Fatalf("artifact filename %q has no random suffix", name)
	}
	got, _ := os.ReadFile(matches[0])
	if string(got) != bigRes {
		t.Fatalf("artifact content mismatch: got len=%d want len=%d", len(got), len(bigRes))
	}

	// Small result → NOT truncated, unchanged.
	smallRes := strings.Repeat("y", small)
	res2 := policy.Apply(ctx, sess, &ToolResult{Content: smallRes})
	if res2.Truncated || res2.FileRef != "" || res2.Content != smallRes {
		t.Fatalf("small result was truncated: %+v", res2)
	}
	matches2, _ := filepath.Glob(filepath.Join(sessDir, "*.txt"))
	if len(matches2) != 1 {
		t.Fatalf("small result should not have been saved; file count = %d", len(matches2))
	}
}

// TestDefaultResultPolicy_NilAndErrorResultsPassthrough asserts error results
// and nil results are never truncated.
func TestDefaultResultPolicy_NilAndErrorResultsPassthrough(t *testing.T) {
	policy := &DefaultResultPolicy{}
	sess := Session{ID: "s-nil", Model: &fakeWindowModel{cw: 100}}

	if got := policy.Apply(context.Background(), sess, nil); got != nil {
		t.Fatalf("nil result became non-nil: %+v", got)
	}

	errRes := &ToolResult{Error: &ToolError{Message: "boom"}}
	got := policy.Apply(context.Background(), sess, errRes)
	if got.Truncated || got.FileRef != "" {
		t.Fatalf("error result was truncated: %+v", got)
	}
}
