package openai

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseToolCalls(t *testing.T) {
	content := `thinking first
<TOOL_CALL>{"name":"read_file","arguments":{"path":"a.txt","options":{"limit":10}}}</TOOL_CALL>
ignored trailing text`

	text, calls := ParseToolCalls(content)
	if text != "thinking first" {
		t.Fatalf("text = %q", text)
	}
	if len(calls) != 1 {
		t.Fatalf("calls = %#v", calls)
	}
	if calls[0].Function.Name != "read_file" {
		t.Fatalf("name = %q", calls[0].Function.Name)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &args); err != nil {
		t.Fatal(err)
	}
	if args["path"] != "a.txt" {
		t.Fatalf("arguments = %#v", args)
	}
	if !strings.HasPrefix(calls[0].ID, "call_") {
		t.Fatalf("id = %q", calls[0].ID)
	}

	_, again := ParseToolCalls(content)
	if again[0].ID != calls[0].ID {
		t.Fatalf("tool call IDs are not stable: %q != %q", again[0].ID, calls[0].ID)
	}
}

func TestParseToolCallsRejectsMalformedBlock(t *testing.T) {
	content := `<TOOL_CALL>{not-json}</TOOL_CALL>`
	text, calls := ParseToolCalls(content)
	if text != content || calls != nil {
		t.Fatalf("got text=%q calls=%#v", text, calls)
	}
	if HasToolCall(content) {
		t.Fatal("malformed block reported as a tool call")
	}
}

func TestBuildToolSystemPromptIsStrict(t *testing.T) {
	prompt := BuildToolSystemPrompt([]Tool{{
		Type: "function",
		Function: FunctionDecl{
			Name:        "read_file",
			Description: "Read a local file",
			Parameters:  json.RawMessage(`{"type":"object"}`),
		},
	}})
	for _, want := range []string{
		"cannot execute them yourself",
		"must not use any device, cloud, shell, or file tool as a substitute",
		`<TOOL_CALL>{"name":"<tool>","arguments":{...}}</TOOL_CALL>`,
		"read_file: Read a local file",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt does not contain %q:\n%s", want, prompt)
		}
	}
}

func TestToolCallStreamFilterHandlesSplitTag(t *testing.T) {
	var filter ToolCallStreamFilter
	fragments := []string{
		"I need the file.\n<TO",
		"OL_CALL>{\"name\":\"read_file\",\"arguments\":{\"path\":\"a.txt\"}}",
		"</TOOL_",
		"CALL>ignored trailing text",
	}
	var got strings.Builder
	for _, fragment := range fragments {
		got.WriteString(filter.Push(fragment))
	}
	got.WriteString(filter.Flush())
	if got.String() != "I need the file.\n" {
		t.Fatalf("streamed text = %q", got.String())
	}
}

func TestToolCallStreamFilterOnlyDelaysPossiblePrefix(t *testing.T) {
	var filter ToolCallStreamFilter
	if got := filter.Push("hello world"); got != "hello world" {
		t.Fatalf("first fragment = %q", got)
	}
	if got := filter.Push(" and <TO"); got != " and " {
		t.Fatalf("partial tag fragment = %q", got)
	}
	if got := filter.Push("X ordinary"); got != "<TOX ordinary" {
		t.Fatalf("disproved tag fragment = %q", got)
	}
}

func TestToolCallStreamFilterFlushesMalformedAndUnclosedBlocks(t *testing.T) {
	tests := []string{
		`before <TOOL_CALL>{not-json}</TOOL_CALL> after`,
		`before <TOOL_CALL>{"name":"read_file"}`,
	}
	for _, input := range tests {
		var filter ToolCallStreamFilter
		var got strings.Builder
		for _, fragment := range []string{input[:len(input)/2], input[len(input)/2:]} {
			got.WriteString(filter.Push(fragment))
		}
		got.WriteString(filter.Flush())
		if got.String() != input {
			t.Fatalf("input %q streamed as %q", input, got.String())
		}
	}
}

func TestFormatToolResultsRecoversFunctionName(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "user", Content: "Read a.txt"},
		{
			Role: "assistant",
			ToolCalls: []ToolCall{{
				ID:       "call_123",
				Type:     "function",
				Function: FunctionCall{Name: "read_file", Arguments: `{"path":"a.txt"}`},
			}},
		},
		{Role: "tool", ToolCallID: "call_123", Content: "hello"},
	}
	if got, want := FormatToolResults(msgs), "[tool result for read_file]\nhello"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
