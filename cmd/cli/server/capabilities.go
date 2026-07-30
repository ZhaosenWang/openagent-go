package server

// Capabilities controls which pluggable modules are enabled at startup.
// Hooks, tools, and observer are always on (no switch).
// Zero value means "use defaults" (some on, some off).
type Capabilities struct {
	Memory     *bool // default on
	Summarizer *bool // default on
	Skills     *bool // default on
	MCP        *bool // default on
	Guard      *bool // default off
	Approver   *bool // default off
}

func (c Capabilities) on(field *bool, defaultOn bool) bool {
	if field != nil {
		return *field
	}
	return defaultOn
}

// OnMemory reports whether Memory is enabled.
func (c Capabilities) OnMemory() bool { return c.on(c.Memory, true) }

// OnSummarizer reports whether Summarizer is enabled.
func (c Capabilities) OnSummarizer() bool { return c.on(c.Summarizer, true) }

// OnSkills reports whether SkillLoader is enabled.
func (c Capabilities) OnSkills() bool { return c.on(c.Skills, true) }

// OnMCP reports whether MCP tools are enabled.
func (c Capabilities) OnMCP() bool { return c.on(c.MCP, true) }

// OnGuard reports whether LLM Guard is enabled.
func (c Capabilities) OnGuard() bool { return c.on(c.Guard, false) }

// OnApprover reports whether Approver is enabled.
func (c Capabilities) OnApprover() bool { return c.on(c.Approver, false) }
