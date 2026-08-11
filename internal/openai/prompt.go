package openai

import (
	"fmt"
	"strings"
)

// FlattenTurn renders the messages that must be sent to the upstream for the
// current turn. tool-result messages are formatted so the agent can read them.
func FlattenTurn(msgs []ChatMessage) string {
	var b strings.Builder
	toolNames := toolNamesByID(msgs)
	for i, m := range msgs {
		switch m.Role {
		case "system":
			b.WriteString("[system] " + m.Content)
		case "assistant":
			if len(m.ToolCalls) > 0 {
				b.WriteString("[assistant tool request] ")
				for _, tc := range m.ToolCalls {
					fmt.Fprintf(&b, "%s(%s) ", tc.Function.Name, tc.Function.Arguments)
				}
			} else {
				b.WriteString("[assistant] " + m.Content)
			}
		case "tool":
			fmt.Fprintf(&b, "[tool result for %s]\n%s", toolResultName(m, toolNames), m.Content)
		default:
			b.WriteString(m.Content)
		}
		if i < len(msgs)-1 {
			b.WriteString("\n\n")
		}
	}
	return b.String()
}

// LastToolResults returns the trailing tool-result messages (a follow-up turn).
func LastToolResults(msgs []ChatMessage) []ChatMessage {
	start := len(msgs)
	for start > 0 && msgs[start-1].Role == "tool" {
		start--
	}
	return msgs[start:]
}

// FormatToolResults renders tool-result messages into a single follow-up body.
// Pass the complete OpenAI history so tool names can be recovered from the
// preceding assistant tool_calls when tool result messages omit name.
func FormatToolResults(msgs []ChatMessage) string {
	results := LastToolResults(msgs)
	if len(results) == 0 {
		results = msgs
	}
	toolNames := toolNamesByID(msgs)
	var b strings.Builder
	for i, m := range results {
		fmt.Fprintf(&b, "[tool result for %s]\n%s", toolResultName(m, toolNames), m.Content)
		if i < len(results)-1 {
			b.WriteString("\n\n")
		}
	}
	return b.String()
}

func toolNamesByID(msgs []ChatMessage) map[string]string {
	names := make(map[string]string)
	for _, msg := range msgs {
		for _, call := range msg.ToolCalls {
			if call.ID != "" {
				names[call.ID] = call.Function.Name
			}
		}
	}
	return names
}

func toolResultName(msg ChatMessage, names map[string]string) string {
	if msg.Name != "" {
		return msg.Name
	}
	if name := names[msg.ToolCallID]; name != "" {
		return name
	}
	if msg.ToolCallID != "" {
		return msg.ToolCallID
	}
	return "unknown tool"
}
