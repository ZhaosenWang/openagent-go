package execution

import (
	"context"
	"fmt"
	"strings"

	openagent "github.com/yusheng-g/openagent-go"
	ctxpkg "github.com/yusheng-g/openagent-go/context"
)

// ── Built-in tool definitions ──

var (
	builtinLoadSkillDef = openagent.FunctionDefinition{
		Name:        "load_skill",
		Description: "Load a skill's full instructions from its SKILL.md. Use when you need detailed guidance on a specific topic.",
		Parameters:  openagent.SchemaOf[LoadSkillParams](),
	}
	builtinReloadSkillsDef = openagent.FunctionDefinition{
		Name:        "reload_skills",
		Description: "Rescan the skills directory for newly installed or removed skills. Use after installing or uninstalling a skill.",
		Parameters:  openagent.SchemaOf[ReloadSkillsParams](),
	}
	builtinRecallDef = openagent.FunctionDefinition{
		Name:        "recall",
		Description: "Search the long-term memory store (knowledge extracted from past sessions) for durable facts about the user and the project — preferences, decisions, technical details. It does NOT search this session's raw message history: details omitted by the conversation summary are not retrievable via recall. Returns ranked results with relevance scores.",
		Parameters:  openagent.SchemaOf[RecallParams](),
	}
)

// LoadSkillParams are the arguments to load_skill.
type LoadSkillParams struct {
	Name string `json:"name" jsonschema:"description=Name of the skill to load"`
}

// ReloadSkillsParams is empty (no arguments).
type ReloadSkillsParams struct{}

// RecallParams are the arguments to recall.
type RecallParams struct {
	Query string `json:"query" jsonschema:"description=Specific keywords to find (e.g. 'kubectl rollout restart', 'benchmark_2024.csv', 'port 5432')"`
}

// builtinDef returns the definition for a built-in tool name.
func (e *ExecutionRuntime) builtinDef(name string) openagent.FunctionDefinition {
	switch name {
	case "load_skill":
		return builtinLoadSkillDef
	case "reload_skills":
		return builtinReloadSkillsDef
	case "recall":
		return builtinRecallDef
	}
	return openagent.FunctionDefinition{}
}

// builtinHandler returns the execution handler for a built-in tool.
func (e *ExecutionRuntime) builtinHandler(name string) BuiltinHandler {
	switch name {
	case "load_skill":
		return e.executeLoadSkill
	case "reload_skills":
		return e.executeReloadSkills
	case "recall":
		return e.executeRecall
	}
	return nil
}

func builtinSkillToolDefs() []openagent.FunctionDefinition {
	return []openagent.FunctionDefinition{builtinLoadSkillDef, builtinReloadSkillsDef}
}

// ── load_skill ──

func (e *ExecutionRuntime) executeLoadSkill(ctx context.Context, session openagent.Session, call openagent.ToolCall, ch chan<- openagent.StreamEvent) openagent.Message {
	args, err := openagent.ParseArgs[LoadSkillParams]([]byte(call.Function.Arguments))
	if err != nil {
		return openagent.Message{Role: openagent.RoleTool, ToolCallID: call.ID, Content: fmt.Sprintf("load_skill: %v", err)}
	}

	// Idempotent: return cached.
	e.loadedSkillsMu.RLock()
	body, ok := e.loadedSkills[args.Name]
	e.loadedSkillsMu.RUnlock()
	if ok {
		return openagent.Message{Role: openagent.RoleTool, ToolCallID: call.ID, Content: body}
	}

	// Find skill in catalog.
	var info openagent.SkillInfo
	found := false
	if e.cfg.SkillProvider != nil {
		skills, err := e.cfg.SkillProvider.Discover(ctx)
		if err != nil {
			return openagent.Message{Role: openagent.RoleTool, ToolCallID: call.ID, Content: fmt.Sprintf("load_skill: discover failed: %v", err)}
		}
		for _, s := range skills {
			if s.Name == args.Name {
				info = s
				found = true
				break
			}
		}
	}
	if !found {
		return openagent.Message{Role: openagent.RoleTool, ToolCallID: call.ID, Content: fmt.Sprintf("load_skill: skill %q not found", args.Name)}
	}

	loaded, err := e.cfg.SkillProvider.Load(ctx, info)
	if err != nil {
		return openagent.Message{Role: openagent.RoleTool, ToolCallID: call.ID, Content: fmt.Sprintf("load_skill: %v", err)}
	}
	body = fmt.Sprintf("**Directory:** %s\n\n%s", info.Path, loaded)
	e.loadedSkillsMu.Lock()
	e.loadedSkills[args.Name] = body
	e.loadedSkillsMu.Unlock()
	return openagent.Message{Role: openagent.RoleTool, ToolCallID: call.ID, Content: body}
}

// ── reload_skills ──

func (e *ExecutionRuntime) executeReloadSkills(ctx context.Context, session openagent.Session, call openagent.ToolCall, ch chan<- openagent.StreamEvent) openagent.Message {
	if e.cfg.SkillProvider == nil {
		return openagent.Message{Role: openagent.RoleTool, ToolCallID: call.ID, Content: "reload_skills: no skill provider configured"}
	}
	skills, err := e.cfg.SkillProvider.Discover(ctx)
	if err != nil {
		return openagent.Message{Role: openagent.RoleTool, ToolCallID: call.ID, Content: fmt.Sprintf("reload_skills: %v", err)}
	}
	// Prune loaded skills that no longer exist.
	seen := make(map[string]bool, len(skills))
	for _, s := range skills {
		seen[s.Name] = true
	}
	e.loadedSkillsMu.Lock()
	for name := range e.loadedSkills {
		if !seen[name] {
			delete(e.loadedSkills, name)
		}
	}
	e.loadedSkillsMu.Unlock()
	return openagent.Message{Role: openagent.RoleTool, ToolCallID: call.ID, Content: fmt.Sprintf("reloaded %d skills", len(skills))}
}

// ── recall ──

func (e *ExecutionRuntime) executeRecall(ctx context.Context, session openagent.Session, call openagent.ToolCall, ch chan<- openagent.StreamEvent) openagent.Message {
	if e.cfg.MemoryProvider == nil {
		return openagent.Message{Role: openagent.RoleTool, ToolCallID: call.ID, Content: "recall: no memory provider configured"}
	}
	args, err := openagent.ParseArgs[RecallParams]([]byte(call.Function.Arguments))
	if err != nil {
		return openagent.Message{Role: openagent.RoleTool, ToolCallID: call.ID, Content: fmt.Sprintf("recall: %v", err)}
	}
	// User-level scope: knowledge is stored by user (extractor), not by
	// session — session-scoped recall would find nothing across sessions.
	results, err := e.cfg.MemoryProvider.Recall(ctx, ctxpkg.ContextScope{UserID: session.UserID}, args.Query, 5)
	if err != nil {
		return openagent.Message{Role: openagent.RoleTool, ToolCallID: call.ID, Content: fmt.Sprintf("recall: %v", err)}
	}
	if len(results) == 0 {
		return openagent.Message{Role: openagent.RoleTool, ToolCallID: call.ID, Content: "No results found in memory."}
	}

	var b strings.Builder
	for _, r := range results {
		fmt.Fprintf(&b, "--- [%.2f] %s ---\n%s\n\n", r.Score, r.Kind, r.Content)
	}
	return openagent.Message{Role: openagent.RoleTool, ToolCallID: call.ID, Content: strings.TrimSpace(b.String())}
}

// BuiltinSkillToolDefs returns load_skill + reload_skills definitions.
func BuiltinSkillToolDefs() []openagent.FunctionDefinition { return builtinSkillToolDefs() }

// BuiltinRecallDef returns the recall tool definition.
func BuiltinRecallDef() openagent.FunctionDefinition { return builtinRecallDef }
