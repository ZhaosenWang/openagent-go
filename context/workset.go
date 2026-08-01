package context

import (
	openagent "github.com/yusheng-g/openagent-go"
)

// ExcludeInput removes the just-persisted user input from the tail of a
// history view. The kernel commits the input before fetching history so it
// is durable even if the run is cancelled immediately; ExcludeInput keeps
// it from appearing twice (history + the live prompt).
func ExcludeInput(msgs []openagent.Message, input openagent.Message) []openagent.Message {
	if len(msgs) == 0 {
		return msgs
	}
	last := msgs[len(msgs)-1]
	if last.Role == input.Role && last.Content == input.Content && len(last.ToolCalls) == 0 {
		return msgs[:len(msgs)-1]
	}
	return msgs
}

// TrimOrphanToolCalls removes a leading assistant message with tool_calls
// and its tool results when the results are missing (e.g. after a crash).
// Leading orphan tool_calls violate the API conversation format.
func TrimOrphanToolCalls(msgs []openagent.Message) []openagent.Message {
	for len(msgs) > 0 && msgs[0].Role == openagent.RoleAssistant && len(msgs[0].ToolCalls) > 0 {
		msgs = msgs[1:]
		for len(msgs) > 0 && msgs[0].Role == openagent.RoleTool {
			msgs = msgs[1:]
		}
	}
	return msgs
}
