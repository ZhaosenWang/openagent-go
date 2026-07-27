package server

import (
	"strings"
	"testing"
)

func TestToolCardCompleted(t *testing.T) {
	card := toolCallSubCard(toolCallEntry{name: "shell", args: `{"command":"echo hello"}`, status: "completed", output: "hello\n"})
	if card.Header.Title != "shell ✓" {
		t.Errorf("title = %q", card.Header.Title)
	}
	if !strings.Contains(card.Content, "echo hello") {
		t.Errorf("content should contain command: %s", card.Content)
	}
	if !strings.Contains(card.Content, "hello") {
		t.Errorf("content should contain output: %s", card.Content)
	}
}

func TestToolCardFailed(t *testing.T) {
	card := toolCallSubCard(toolCallEntry{name: "write", args: `{"path":"/tmp/x"}`, status: "failed", output: "error: permission denied"})
	if !strings.Contains(card.Header.Title, "✗") {
		t.Errorf("failed card should have ✗ in title: %s", card.Header.Title)
	}
}

func TestToolCardInProgress(t *testing.T) {
	card := toolCallSubCard(toolCallEntry{name: "shell", args: `{"command":"sleep 10"}`, status: "in_progress", output: "running..."})
	if !strings.Contains(card.Header.Title, "shell") {
		t.Errorf("in-progress title should contain name: %s", card.Header.Title)
	}
}

func TestFormatInputShell(t *testing.T) {
	result := formatInput("shell", `{"command":"ls -la"}`)
	if !strings.Contains(result, "ls -la") {
		t.Errorf("should contain command: %s", result)
	}
}

func TestFormatInputRead(t *testing.T) {
	result := formatInput("read", `{"path":"/src/main.go"}`)
	if !strings.Contains(result, "/src/main.go") {
		t.Errorf("should contain path: %s", result)
	}
}

func TestFormatInputGrep(t *testing.T) {
	result := formatInput("grep", `{"query":"TODO","path":"/src"}`)
	if !strings.Contains(result, "TODO") && !strings.Contains(result, "`") {
		t.Errorf("should contain query: %s", result)
	}
}

func TestFormatInputSubagent(t *testing.T) {
	result := formatInput("subagent", `{"name":"reviewer","task":"review auth.go"}`)
	if !strings.Contains(result, "reviewer") {
		t.Errorf("should contain agent name: %s", result)
	}
	if !strings.Contains(result, "review auth.go") {
		t.Errorf("should contain task: %s", result)
	}
}

func TestFormatInputUnknown(t *testing.T) {
	result := formatInput("unknown_tool", `{"key":"val"}`)
	if !strings.Contains(result, "```") {
		t.Errorf("unknown tool should show raw args in code block: %s", result)
	}
}

func TestParsePlanCreateValid(t *testing.T) {
	args := `{"goal":"refactor auth","steps":[{"id":"1","content":"extract middleware","priority":"high"},{"id":"2","content":"add tests","priority":"low"}]}`
	goal, steps := parsePlanCreate(args)
	if goal != "refactor auth" {
		t.Errorf("goal = %q", goal)
	}
	if !strings.Contains(steps, "extract middleware") {
		t.Errorf("steps should contain step 1: %s", steps)
	}
	if !strings.Contains(steps, "add tests") {
		t.Errorf("steps should contain step 2: %s", steps)
	}
}

func TestParsePlanCreateEmpty(t *testing.T) {
	goal, steps := parsePlanCreate(`{}`)
	if goal != "" || steps != "" {
		t.Errorf("expected empty, got goal=%q steps=%q", goal, steps)
	}
}

func TestParsePlanCreateInvalidJSON(t *testing.T) {
	goal, steps := parsePlanCreate(`not json`)
	if goal != "" || steps != "" {
		t.Errorf("expected empty for invalid JSON, got goal=%q steps=%q", goal, steps)
	}
}

func TestParsePlanCreatePriorityEmoji(t *testing.T) {
	args := `{"goal":"test","steps":[{"id":"h","content":"high","priority":"high"},{"id":"m","content":"med","priority":"medium"},{"id":"l","content":"low","priority":"low"}]}`
	_, steps := parsePlanCreate(args)
	if !strings.Contains(steps, "🔴") {
		t.Errorf("high priority should have red circle emoji: %s", steps)
	}
	if !strings.Contains(steps, "🟡") {
		t.Errorf("medium priority should have yellow circle emoji: %s", steps)
	}
	if !strings.Contains(steps, "🟢") {
		t.Errorf("low priority should have green circle emoji: %s", steps)
	}
}
