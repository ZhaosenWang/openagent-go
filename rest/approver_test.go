package rest

import (
	"context"
	"encoding/json"
	"testing"

	openagent "github.com/yusheng-g/openagent-go"
	"github.com/yusheng-g/openagent-go/governance"
)

// scriptedSubmit answers the approval request with the given action.
func scriptedSubmit(action string) func(openagent.ToolCall, chan approveResponse) {
	return func(_ openagent.ToolCall, resp chan approveResponse) {
		resp <- approveResponse{action: action, reason: "user"}
	}
}

func sess(id string) openagent.Session {
	return openagent.Session{ID: id}
}

// "allow" grants THIS call only — nothing is remembered, the same
// tool+args asks again (ACP allow_once semantics).
func TestRestApproverAllowIsOnce(t *testing.T) {
	mem := governance.NewSessionApprovalMemory()
	a := &restApprover{submit: scriptedSubmit("allow_once"), memory: mem}
	ctx := context.Background()

	d, err := a.Ask(ctx, openagent.ToolCall{ID: "c1", Function: openagent.ToolCallFunction{Name: "shell", Arguments: `{"command":"ls"}`}}, openagent.FunctionDefinition{}, sess("s1"))
	if err != nil || d.Action != governance.Allow {
		t.Fatalf("allow action = %v err = %v", d.Action, err)
	}
	// Nothing remembered: an identical call must ask again.
	key := governance.ApprovalKey("shell", json.RawMessage(`{"command":"ls"}`))
	if _, ok := mem.Recall(ctx, "s1", key); ok {
		t.Fatal("allow must not write session memory")
	}
}

// "always" remembers the exact tool+args for the session (ACP
// allow_always semantics); a CHANGED argument is a different operation.
// "deny" and "edit" never write memory.
func TestRestApproverAlwaysSessionScoped(t *testing.T) {
	mem := governance.NewSessionApprovalMemory()
	a := &restApprover{submit: scriptedSubmit("allow_always"), memory: mem}
	ctx := context.Background()

	if _, err := a.Ask(ctx, openagent.ToolCall{ID: "c1", Function: openagent.ToolCallFunction{Name: "shell", Arguments: `{"command":"ls -la"}`}}, openagent.FunctionDefinition{}, sess("s1")); err != nil {
		t.Fatal(err)
	}
	// Exact match remembered (session-scoped).
	key := governance.ApprovalKey("shell", json.RawMessage(`{"command":"ls -la"}`))
	if _, ok := mem.Recall(ctx, "s1", key); !ok {
		t.Fatal("always must write session memory under tool+args key")
	}
	// Another session is NOT covered (session-scoped).
	if _, ok := mem.Recall(ctx, "s2", key); ok {
		t.Fatal("session grant leaked to another session")
	}
	// A changed argument is a different key.
	if _, ok := mem.Recall(ctx, "s1", governance.ApprovalKey("shell", json.RawMessage(`{"command":"rm -rf x"}`))); ok {
		t.Fatal("changed args must not match")
	}
	// deny / edit never write memory.
	a2 := &restApprover{submit: scriptedSubmit("deny"), memory: mem}
	if _, err := a2.Ask(ctx, openagent.ToolCall{ID: "c2", Function: openagent.ToolCallFunction{Name: "shell", Arguments: `{"command":"rm"}`}}, openagent.FunctionDefinition{}, sess("s1")); err != nil {
		t.Fatal(err)
	}
	if _, ok := mem.Recall(ctx, "s1", governance.ApprovalKey("shell", json.RawMessage(`{"command":"rm"}`))); ok {
		t.Fatal("deny must not write memory")
	}
	a3 := &restApprover{submit: scriptedSubmit("edit"), memory: mem}
	if _, err := a3.Ask(ctx, openagent.ToolCall{ID: "c3", Function: openagent.ToolCallFunction{Name: "shell", Arguments: `{"command":"mv"}`}}, openagent.FunctionDefinition{}, sess("s1")); err != nil {
		t.Fatal(err)
	}
	if _, ok := mem.Recall(ctx, "s1", governance.ApprovalKey("shell", json.RawMessage(`{"command":"mv"}`))); ok {
		t.Fatal("edit must not write memory")
	}
}

// An unrecognized action fails CLOSED (deny) — an approval UI hiccup or
// a malformed request must never auto-execute the tool.
func TestRestApproverUnknownActionFailsClosed(t *testing.T) {
	a := &restApprover{submit: scriptedSubmit("maybe"), memory: governance.NewSessionApprovalMemory()}
	d, err := a.Ask(context.Background(), openagent.ToolCall{ID: "c1", Function: openagent.ToolCallFunction{Name: "shell", Arguments: `{}`}}, openagent.FunctionDefinition{}, sess("s1"))
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != governance.Deny {
		t.Fatalf("unknown action = %v, want Deny (fail closed)", d.Action)
	}
}

// countingHuman counts Ask invocations.
type countingHuman struct{ asked int }

func (h *countingHuman) Ask(context.Context, openagent.ToolCall, openagent.FunctionDefinition, openagent.Session) (governance.Decision, error) {
	h.asked++
	return governance.Decision{Action: governance.Allow, Reason: "human"}, nil
}

// The end-to-end equivalence: the key the approver writes on Ask is
// EXACTLY the key the policy engine looks up on the next Evaluate. Both
// sides derive it independently (approver and engine live in different
// packages) — this test pins the contract so a canonicalization change
// cannot silently break remembered grants.
func TestRestApproverAskToEvaluateRoundTrip(t *testing.T) {
	mem := governance.NewSessionApprovalMemory()
	human := &countingHuman{}
	engine := governance.NewEngine(nil, governance.NewToolClassifier(), mem, human)
	ctx := context.Background()
	s := sess("s1")

	call := openagent.ToolCall{
		ID:       "c1",
		Type:     "function",
		Function: openagent.ToolCallFunction{Name: "shell", Arguments: `{"command":"ls -la","timeout":10}`},
	}
	def := openagent.FunctionDefinition{Name: "shell"}

	// First call: engine defers to human, user picks "always", the bridge
	// remembers under its own ApprovalKey derivation.
	if _, err := engine.Evaluate(ctx, call, def, s); err != nil {
		t.Fatal(err)
	}
	if human.asked != 1 {
		t.Fatalf("first call human asks = %d, want 1", human.asked)
	}
	a := &restApprover{submit: scriptedSubmit("allow_always"), memory: mem}
	if _, err := a.Ask(ctx, call, def, s); err != nil {
		t.Fatal(err)
	}

	// Second call with the SAME call (same args byte-for-byte): the
	// engine's own key derivation must hit what the approver wrote.
	if _, err := engine.Evaluate(ctx, call, def, s); err != nil {
		t.Fatal(err)
	}
	if human.asked != 1 {
		t.Fatalf("second call human asks = %d, want 1 (memory must hit)", human.asked)
	}
}
