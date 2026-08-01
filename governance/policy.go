// Package governance implements the policy layer: the layered approval
// engine (rules → safety → memory → human), guards, and the policy
// interface.
//
// This package holds only interfaces and the engine skeleton in P0; the
// full engine lands in P1 together with kernel.Runtime. The guard and
// approver interfaces stay in the root package until P1 moves Agent out of
// the root package (moving them here would create an import cycle: the
// root Agent struct references them, and this package references root
// types).
package governance

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"

	openagent "github.com/yusheng-g/openagent-go"
)

// ApprovalAction is the outcome of evaluating one tool call against the
// policy chain.
type ApprovalAction string

const (
	// Allow executes the call without human input.
	Allow ApprovalAction = "allow"
	// Deny blocks the call; the tool result reports the rejection reason.
	Deny ApprovalAction = "deny"
	// Ask routes the call to the human layer for a decision.
	Ask ApprovalAction = "ask"
)

// Decision is the policy engine's verdict on one tool call. Unlike the
// legacy boolean approver, a Decision carries semantics: ModifiedArgs lets
// a human approve with edited arguments (e.g. fixing a dangerous path),
// and Reason is always populated for audit.
type Decision struct {
	Action       ApprovalAction  `json:"action"`
	Reason       string          `json:"reason"`
	ModifiedArgs json.RawMessage `json:"modified_args,omitempty"`
}

// Rule is a settings-driven policy rule: when a call's tool name matches
// ToolPattern (glob) and its arguments match ArgsPattern, the rule's
// Action applies. Rules are evaluated in order; the first match wins.
// This is the Claude Code permissions shape.
type Rule struct {
	ToolPattern string         `json:"tool_pattern"`
	ArgsPattern map[string]any `json:"args_pattern,omitempty"`
	Action      ApprovalAction `json:"action"`
	Reason      string         `json:"reason"`
}

// SafetyClass classifies a tool's side-effect profile. The runtime derives
// it from a SafetyClassifier — read-only tools are auto-allowed without
// consulting the human.
type SafetyClass int

const (
	// ReadOnly tools never mutate external state.
	ReadOnly SafetyClass = iota
	// Mutating tools change state but stay within workspace boundaries.
	Mutating
	// Dangerous tools can affect the host (shell, network, keyring...).
	Dangerous
)

// SafetyClassifier decides a tool's safety class. Classification lives in
// the platform, not in the tools: the kernel injects ToolClassifier (a
// whitelist of read-only tools); everything else is Dangerous and
// consults the human layer. Tools never self-declare safety.
type SafetyClassifier interface {
	Classify(def openagent.FunctionDefinition) SafetyClass
}

// ApprovalMemory is the session-scoped approval memory: once a human
// answers "always allow" for a tool, subsequent calls to the same tool
// are auto-allowed within the session (persisted, not just in-memory —
// this fixes the legacy ACP "Allow Always" bug).
type ApprovalMemory interface {
	// Remember stores a decision keyed by tool name within a session.
	Remember(ctx context.Context, sessionID, key string, decision Decision) error
	// Recall returns the remembered decision for a key, if any.
	Recall(ctx context.Context, sessionID, key string) (Decision, bool)
}

// HumanApprover is the human layer of the policy chain. Ask is called when
// rules, safety, and memory all defer to a human. Implementations bridge
// to the application: ACP RequestPermission RPC, REST SSE dialog, TUI
// prompt.
type HumanApprover interface {
	Ask(ctx context.Context, call openagent.ToolCall, def openagent.FunctionDefinition, session openagent.Session) (Decision, error)
}

// Policy evaluates whether a tool call may execute. The layered engine
// (rules → safety → memory → human) implements it; the runtime consults
// Policy instead of calling an approver directly.
type Policy interface {
	Evaluate(ctx context.Context, call openagent.ToolCall, def openagent.FunctionDefinition, session openagent.Session) (Decision, error)
}

// ── Layered policy engine ──

// Engine implements Policy with the layered chain: Rules → Safety →
// Memory → Human. Each layer short-circuits when it reaches a decision.
type Engine struct {
	Rules  []Rule           // first match wins (nil = no rules)
	Safety SafetyClassifier // nil = no safety layer (all calls advance)
	Memory ApprovalMemory   // session-scoped approval memory (nil = no memory layer)
	Human  HumanApprover    // final layer (nil = Ask resolves to Deny)
}

// NewEngine creates an engine. When human is nil, Ask decisions resolve to
// Deny (fail closed).
func NewEngine(rules []Rule, safety SafetyClassifier, mem ApprovalMemory, human HumanApprover) *Engine {
	return &Engine{Rules: rules, Safety: safety, Memory: mem, Human: human}
}

// matchesRule reports whether a rule matches the tool name and args.
// Glob forms on the tool name: "*" (any), exact "name", prefix "name*",
// suffix "*name", and contains "*name*". An empty ArgsPattern matches any
// args; otherwise every key in ArgsPattern must exist in the call args
// with the same value.
func matchesRule(rule Rule, call openagent.ToolCall) bool {
	if rule.ToolPattern == "" {
		return false
	}
	p := rule.ToolPattern
	matched := p == "*" || p == call.Function.Name
	if !matched {
		switch {
		case strings.HasPrefix(p, "*") && strings.HasSuffix(p, "*"):
			core := strings.Trim(p, "*")
			matched = core != "" && strings.Contains(call.Function.Name, core)
		case strings.HasPrefix(p, "*"):
			matched = strings.HasSuffix(call.Function.Name, strings.TrimPrefix(p, "*"))
		case strings.HasSuffix(p, "*"):
			matched = strings.HasPrefix(call.Function.Name, strings.TrimSuffix(p, "*"))
		}
	}
	if !matched {
		return false
	}
	if len(rule.ArgsPattern) == 0 {
		return true
	}
	// Args pattern: every key must exist in the call args with the same value.
	var args map[string]any
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return false
	}
	for k, v := range rule.ArgsPattern {
		got, ok := args[k]
		if !ok || !reflect.DeepEqual(got, v) {
			return false
		}
	}
	return true
}

// Evaluate runs the layered chain. The resulting Decision is final: Allow
// executes the call (with ModifiedArgs if the human edited them), Deny
// blocks it, Ask (from Rules) routes to the human layer.
func (e *Engine) Evaluate(ctx context.Context, call openagent.ToolCall, def openagent.FunctionDefinition, session openagent.Session) (Decision, error) {
	// 1) Rules layer.
	for _, rule := range e.Rules {
		if !matchesRule(rule, call) {
			continue
		}
		if rule.Action == Ask {
			return e.askHuman(ctx, call, def, session, rule.Reason)
		}
		return Decision{Action: rule.Action, Reason: rule.Reason}, nil
	}

	// 2) Safety layer: read-only tools auto-allow.
	if e.Safety != nil {
		if e.Safety.Classify(def) == ReadOnly {
			return Decision{Action: Allow, Reason: "read-only tool"}, nil
		}
	}

	// 3) Memory layer: remembered decision for this tool. A remembered
	// Ask never short-circuits — it still routes to the human layer
	// (otherwise an Ask in memory would silently bypass approval).
	if e.Memory != nil {
		key := call.Function.Name
		if d, ok := e.Memory.Recall(ctx, session.ID, key); ok {
			if d.Action == Ask {
				return e.askHuman(ctx, call, def, session, "remembered ask")
			}
			return d, nil
		}
	}

	// 4) Human layer.
	return e.askHuman(ctx, call, def, session, "requires approval")
}

// askHuman routes to the human layer. A nil human fails closed (Deny).
func (e *Engine) askHuman(ctx context.Context, call openagent.ToolCall, def openagent.FunctionDefinition, session openagent.Session, reason string) (Decision, error) {
	if e.Human == nil {
		return Decision{Action: Deny, Reason: reason + " (no approver configured)"}, nil
	}
	return e.Human.Ask(ctx, call, def, session)
}

// ToolClassifier is the default platform-side safety classification: a
// whitelist of read-only tool names. Read-only tools are auto-allowed by
// the policy chain (Safety layer); everything else is Dangerous and
// consults the human layer. The classification lives here in the
// platform, not in the tools themselves (tools never self-declare
// safety; that is the legacy CanSelfApprove pattern).
type ToolClassifier struct {
	ReadOnlyNames map[string]bool
}

// NewToolClassifier creates a classifier with the built-in read-only set.
func NewToolClassifier() *ToolClassifier {
	return &ToolClassifier{ReadOnlyNames: map[string]bool{
		"read": true, "ls": true, "grep": true,
		"webfetch": true, "websearch": true,
		"recall": true, "load_skill": true, "reload_skills": true,
	}}
}

// Classify implements SafetyClassifier.
func (c *ToolClassifier) Classify(def openagent.FunctionDefinition) SafetyClass {
	if c.ReadOnlyNames[def.Name] {
		return ReadOnly
	}
	return Dangerous
}
