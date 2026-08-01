package context

import (
	"context"
	"log/slog"

	openagent "github.com/yusheng-g/openagent-go"
	"github.com/yusheng-g/openagent-go/provider/resource"
	"github.com/yusheng-g/openagent-go/provider/skill"
	"github.com/yusheng-g/openagent-go/session"
)

// Config wires the Context Runtime to its providers.
type Config struct {
	SessionStore     session.SessionStore
	Compressor       session.Compressor
	MemoryProvider   MemoryProvider
	SkillProvider    skill.Provider
	ResourceProvider resource.Provider
	Observer         openagent.RunObserver
}

// BuildRequest is the per-turn input to Build.
type BuildRequest struct {
	Session    openagent.Session
	Goal       string
	WorkingSet []openagent.Message // assembled working messages (kernel's compaction result)
}

// ContextRuntime assembles the [AgentContext] an agent sees: working
// messages plus durable knowledge recalled from the MemoryProvider,
// scoped by the session. It owns nothing the kernel doesn't hand it —
// compaction stays in the kernel; Build only layers knowledge on top and
// Commit persists messages.
type ContextRuntime struct {
	cfg Config
}

// NewContextRuntime creates a ContextRuntime.
func NewContextRuntime(cfg Config) *ContextRuntime {
	return &ContextRuntime{cfg: cfg}
}

// Build returns the assembled AgentContext: working set + recalled
// knowledge (preferences/facts/lessons) scoped to the session. Knowledge
// recall is best-effort — a provider failure degrades to no memories.
func (c *ContextRuntime) Build(ctx context.Context, req BuildRequest) (*AgentContext, error) {
	ac := &AgentContext{
		Messages: req.WorkingSet,
	}
	// Query from the goal when present, else the latest user input.
	query := req.Goal
	if query == "" && len(req.WorkingSet) > 0 {
		for i := len(req.WorkingSet) - 1; i >= 0; i-- {
			if req.WorkingSet[i].Role == openagent.RoleUser {
				query = req.WorkingSet[i].Content
				break
			}
		}
	}
	if query == "" {
		return ac, nil
	}

	// Knowledge recall (durable memory, user-level: knowledge is stored
	// and recalled across sessions, not scoped to the current one).
	if c.cfg.MemoryProvider != nil {
		scope := ContextScope{
			UserID: req.Session.UserID,
		}
		memories, err := c.cfg.MemoryProvider.Recall(ctx, scope, query, 5)
		if err != nil {
			// Best-effort layer: a provider failure degrades to no
			// memories, but must not fail the run or stay silent.
			slog.Warn("openagent: knowledge recall failed", "error", err)
		} else if len(memories) > 0 {
			ac.Memories = memories
		}
	}

	// Skills: the FULL catalog goes into context every turn (industry
	// pattern — Claude Code / OpenAI Agent Skills list every skill's
	// name+description and let the model decide). The intent query is NOT
	// used here: a continuation turn ("continue", "ok") carries no intent
	// and would wipe the catalog from the prompt. Provider.Match stays for
	// future scale (hundreds of skills) but is not the injection path.
	if c.cfg.SkillProvider != nil {
		skills, err := c.cfg.SkillProvider.Discover(ctx)
		if err != nil {
			slog.Warn("openagent: skill discover failed", "error", err)
		} else if len(skills) > 0 {
			ac.Skills = skills
		}
	}

	// Resource search: external reference material relevant to the goal.
	if c.cfg.ResourceProvider != nil {
		resources, err := c.cfg.ResourceProvider.Search(ctx, query, 5)
		if err != nil {
			slog.Warn("openagent: resource search failed", "error", err)
		} else if len(resources) > 0 {
			ac.Resources = resources
		}
	}

	return ac, nil
}

// Runtime is the Context Runtime interface — the kernel consumes this,
// not the concrete implementation, so applications can substitute their
// own context assembly (e.g. a server-backed implementation).
type Runtime interface {
	Build(ctx context.Context, req BuildRequest) (*AgentContext, error)
}

// Compile-time assertion: the concrete runtime implements the interface.
var _ Runtime = (*ContextRuntime)(nil)
