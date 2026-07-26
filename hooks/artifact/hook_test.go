package artifact

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	openagent "github.com/yusheng-g/openagent-go"
	"github.com/yusheng-g/openagent-go/tool"
)

// fakeWindowModel is a Model with a fixed, small context window so the
// artifact threshold (cw * fraction / 100) can be made trivially small in
// tests without writing megabytes of "result".
type fakeWindowModel struct{ cw int }

func (m *fakeWindowModel) ChatCompletion(context.Context, openagent.ChatCompletionRequest) (*openagent.ChatCompletionResponse, error) {
	return nil, nil
}
func (m *fakeWindowModel) ChatCompletionStream(context.Context, openagent.ChatCompletionRequest) (openagent.StreamReader, error) {
	return nil, nil
}
func (m *fakeWindowModel) ContextWindow() int { return m.cw }

// TestHook_UsesSessionModelContextWindow asserts the artifact hook reads the
// context window from session.Model (the root-cause path: ACP OnPrompt now
// populates oaSession.Model). Before the fix, session.Model was always nil
// and the hook silently fell back to the 128KB default — i.e. a hook that
// was supposed to be model-aware never actually looked at the model.
func TestHook_UsesSessionModelContextWindow(t *testing.T) {
	// Override TMPDIR so ArtifactRoot() (which reads os.TempDir) lands
	// inside a scratch dir — the test does not leak under /tmp/openagent.
	scratch := t.TempDir()
	t.Setenv("TMPDIR", scratch)
	root := tool.ArtifactRoot()
	if !strings.HasPrefix(root, scratch) {
		t.Fatalf("ArtifactRoot=%q not under TMPDIR scratch %q", root, scratch)
	}

	// Window = 10_000 tokens → threshold = 10_000 * 5 / 100 = 500 bytes.
	// A result of 600 bytes should trigger a save; one of 400 must NOT.
	const big = 600
	const small = 400

	sess := openagent.Session{
		ID:    "s-test",
		Model: &fakeWindowModel{cw: 10_000},
	}
	ctx := openagent.WithSession(context.Background(), sess)
	def := openagent.FunctionDefinition{Name: "giant_tool"}

	// Big result → saved, replaced with pointer.
	bigRes := strings.Repeat("x", big)
	bigOut := bigRes
	h := NewHook()
	h.OnToolEnd(ctx, def, nil, &bigOut, nil, nil)
	if bigOut == bigRes {
		t.Fatal("big result not replaced with artifact pointer")
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
	// Filename must carry a non-empty random suffix, not just "artifact-.txt".
	if name := filepath.Base(matches[0]); len(name) <= len("artifact-.txt") {
		t.Fatalf("artifact filename %q has no random suffix", name)
	}
	got, _ := os.ReadFile(matches[0])
	if string(got) != bigRes {
		t.Fatalf("artifact content mismatch: got len=%d want len=%d", len(got), len(bigRes))
	}

	// Small result → NOT saved, unchanged.
	smallRes := strings.Repeat("y", small)
	smallOut := smallRes
	h.OnToolEnd(ctx, def, nil, &smallOut, nil, nil)
	if smallOut != smallRes {
		t.Fatalf("small result was modified: %q", smallOut)
	}
	matches2, _ := filepath.Glob(filepath.Join(sessDir, "*.txt"))
	if len(matches2) != 1 {
		t.Fatalf("small result should not have been saved; file count = %d", len(matches2))
	}
}