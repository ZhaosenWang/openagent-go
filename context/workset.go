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

// TrimOrphanToolCalls removes assistant tool_calls messages that are not
// fully answered by tool messages, together with their collected tool
// responses and any orphan tool messages. OpenAI/DeepSeek reject histories
// where an assistant tool_calls message is not followed by a tool message
// for every call id ("insufficient tool messages following tool_calls
// message"), and DeepSeek additionally rejects repeated tool names.
//
// Orphans arise when a run ends mid-tool-call: the assistant message is
// committed before its tools execute, so a max-turns-exhausted, cancelled,
// or crashed run leaves it in the session store without results. The next
// run reads that history and must sanitize it.
func TrimOrphanToolCalls(msgs []openagent.Message) []openagent.Message {
	out := make([]openagent.Message, 0, len(msgs))
	var pending []string // tool_call ids of the current group awaiting responses
	groupStart := -1     // index in out where the current group begins
	dropGroup := func() {
		if groupStart >= 0 {
			out = out[:groupStart]
		}
		groupStart = -1
		pending = nil
	}
	for _, m := range msgs {
		switch m.Role {
		case openagent.RoleAssistant:
			if len(m.ToolCalls) > 0 {
				if len(pending) > 0 {
					// previous group was incomplete — drop it
					dropGroup()
				}
				groupStart = len(out)
				pending = nil
				for _, tc := range m.ToolCalls {
					pending = append(pending, tc.ID)
				}
				out = append(out, m)
			} else {
				if len(pending) > 0 {
					dropGroup()
				}
				out = append(out, m)
			}
		case openagent.RoleTool:
			if len(pending) > 0 && containsID(pending, m.ToolCallID) {
				out = append(out, m)
				pending = removeID(pending, m.ToolCallID)
				if len(pending) == 0 {
					groupStart = -1 // group fully answered
				}
			}
			// orphan tool message (no pending call): drop
		default: // user / system
			if len(pending) > 0 {
				dropGroup()
			}
			out = append(out, m)
		}
	}
	if len(pending) > 0 {
		dropGroup() // trailing incomplete group
	}
	// A conversation must start with a user or system message — an
	// assistant tool_calls group at the head is invalid regardless of
	// completeness, so drop it together with its tool responses.
	for len(out) > 0 && out[0].Role == openagent.RoleAssistant && len(out[0].ToolCalls) > 0 {
		var ids []string
		for _, tc := range out[0].ToolCalls {
			ids = append(ids, tc.ID)
		}
		out = out[1:]
		for len(out) > 0 && out[0].Role == openagent.RoleTool && containsID(ids, out[0].ToolCallID) {
			out = out[1:]
		}
	}
	return out
}

func containsID(ids []string, id string) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}

func removeID(ids []string, id string) []string {
	for i, v := range ids {
		if v == id {
			return append(ids[:i], ids[i+1:]...)
		}
	}
	return ids
}
