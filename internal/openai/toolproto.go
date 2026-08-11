package openai

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
)

// The gateway teaches the upstream agent a strict text protocol for tools that
// must be executed by the OpenAI client, not by todofor.ai itself.

const (
	toolTag      = "TOOL_CALL"
	toolOpenTag  = "<" + toolTag + ">"
	toolCloseTag = "</" + toolTag + ">"
)

type wireToolCall struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type toolBlock struct {
	start int
	raw   string
}

// ToolCallStreamFilter releases ordinary assistant text as soon as it is safe
// to do so while withholding valid TOOL_CALL protocol blocks. A filter is used
// for one assistant turn.
type ToolCallStreamFilter struct {
	pending string
	inBlock bool
	stopped bool
}

// Push consumes one upstream text fragment and returns text that can be sent to
// the client immediately. Tool tags may be split across any number of frames.
func (f *ToolCallStreamFilter) Push(fragment string) string {
	if fragment == "" || f.stopped {
		return ""
	}
	f.pending += fragment

	var out strings.Builder
	for {
		if f.inBlock {
			closeAt := strings.Index(f.pending[len(toolOpenTag):], toolCloseTag)
			if closeAt < 0 {
				return out.String()
			}
			closeAt += len(toolOpenTag)
			end := closeAt + len(toolCloseTag)
			candidate := f.pending[:end]
			_, calls := ParseToolCalls(candidate)
			if len(calls) > 0 {
				f.pending = ""
				f.stopped = true
				return out.String()
			}

			// A closed but malformed block is ordinary model output.
			out.WriteString(candidate)
			f.pending = f.pending[end:]
			f.inBlock = false
			continue
		}

		openAt := strings.Index(f.pending, toolOpenTag)
		if openAt >= 0 {
			out.WriteString(f.pending[:openAt])
			f.pending = f.pending[openAt:]
			f.inBlock = true
			continue
		}

		keep := possibleToolTagPrefix(f.pending)
		emitEnd := len(f.pending) - keep
		out.WriteString(f.pending[:emitEnd])
		f.pending = f.pending[emitEnd:]
		return out.String()
	}
}

// Flush returns any undecided text at the end of a turn. Valid tool blocks
// remain suppressed; incomplete tags and blocks are treated as ordinary text.
func (f *ToolCallStreamFilter) Flush() string {
	if f.stopped {
		return ""
	}
	pending := f.pending
	f.pending = ""
	f.inBlock = false
	return pending
}

func possibleToolTagPrefix(content string) int {
	max := len(toolOpenTag) - 1
	if len(content) < max {
		max = len(content)
	}
	for size := max; size > 0; size-- {
		if strings.HasSuffix(content, toolOpenTag[:size]) {
			return size
		}
	}
	return 0
}

// BuildToolSystemPrompt renders the tool contract injected as a raw system
// message. Upstream device/cloud tools are also denied in AgentSettings.
func BuildToolSystemPrompt(tools []Tool) string {
	if len(tools) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("You have access to the following tools, but they are client-side tools. ")
	b.WriteString("You cannot execute them yourself and must not use any device, cloud, shell, or file tool as a substitute. ")
	b.WriteString("Never claim that you executed a client-side tool. When a tool is needed, output exactly one block with no Markdown fence or surrounding prose:\n")
	b.WriteString(toolOpenTag + "{\"name\":\"<tool>\",\"arguments\":{...}}" + toolCloseTag + "\n")
	b.WriteString("Then stop immediately. The client will execute it and send back the result. ")
	b.WriteString("After receiving a result, either request one more tool in the same format or provide the final answer. ")
	b.WriteString("Only provide a normal answer when no client-side tool is needed.\n\nTools:\n")
	for _, t := range tools {
		fmt.Fprintf(&b, "- %s: %s\n", t.Function.Name, t.Function.Description)
		if len(t.Function.Parameters) > 0 {
			fmt.Fprintf(&b, "  parameters (JSON schema): %s\n", string(t.Function.Parameters))
		}
	}
	return b.String()
}

// ParseToolCalls extracts valid tool blocks from an assistant reply. Text after
// the first valid block is intentionally discarded because the contract says
// the agent must stop after requesting a tool.
func ParseToolCalls(content string) (text string, calls []ToolCall) {
	blocks := findToolBlocks(content)
	firstStart := -1
	for _, block := range blocks {
		var wc wireToolCall
		if err := json.Unmarshal([]byte(block.raw), &wc); err != nil || wc.Name == "" {
			continue
		}
		args := strings.TrimSpace(string(wc.Arguments))
		if args == "" || args == "null" {
			args = "{}"
		}
		if !json.Valid([]byte(args)) {
			continue
		}
		if firstStart < 0 {
			firstStart = block.start
		}
		sum := sha256.Sum256([]byte(block.raw))
		calls = append(calls, ToolCall{
			ID:       fmt.Sprintf("call_%x", sum[:12]),
			Type:     "function",
			Function: FunctionCall{Name: wc.Name, Arguments: args},
		})
	}
	if len(calls) == 0 {
		return content, nil
	}
	return strings.TrimSpace(content[:firstStart]), calls
}

// HasToolCall reports whether a reply contains at least one valid tool block.
func HasToolCall(content string) bool {
	_, calls := ParseToolCalls(content)
	return len(calls) > 0
}

func findToolBlocks(content string) []toolBlock {
	var blocks []toolBlock
	for offset := 0; offset < len(content); {
		open := strings.Index(content[offset:], toolOpenTag)
		if open < 0 {
			break
		}
		open += offset
		rawStart := open + len(toolOpenTag)
		closeAt := strings.Index(content[rawStart:], toolCloseTag)
		if closeAt < 0 {
			break
		}
		closeAt += rawStart
		blocks = append(blocks, toolBlock{start: open, raw: strings.TrimSpace(content[rawStart:closeAt])})
		offset = closeAt + len(toolCloseTag)
	}
	return blocks
}
