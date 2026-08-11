package transport

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"todo2api/internal/gateway"
	"todo2api/internal/openai"
)

func TestResponsesRequestConvertsItemsAndTools(t *testing.T) {
	store := false
	req := responsesRequest{
		Model:        "public-model",
		Instructions: json.RawMessage(`"system prompt"`),
		Input: json.RawMessage(`[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"read it"}]},
			{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read_file","arguments":"{\"path\":\"a.txt\"}"},
			{"type":"function_call_output","call_id":"call_1","output":"file contents"}
		]`),
		Tools: []responsesTool{{
			Type: "function", Name: "read_file", Description: "Read a file",
			Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`),
		}, {
			Type: "custom", Name: "shell", Description: "Run shell input",
		}},
		Store: &store,
	}

	chat, toolTargets, err := req.chatRequest("todo-1")
	if err != nil {
		t.Fatal(err)
	}
	if chat.Metadata[openai.TodoIDMetadataKey] != "todo-1" || len(chat.Messages) != 4 {
		t.Fatalf("chat request = %#v", chat)
	}
	if chat.Messages[2].Role != "assistant" || len(chat.Messages[2].ToolCalls) != 1 {
		t.Fatalf("assistant call = %#v", chat.Messages[2])
	}
	if chat.Messages[3].Role != "tool" || chat.Messages[3].Name != "read_file" {
		t.Fatalf("tool output = %#v", chat.Messages[3])
	}
	if len(chat.Tools) != 2 || toolTargets["shell"].Type != "custom" {
		t.Fatalf("tools = %#v, targets = %#v", chat.Tools, toolTargets)
	}
	if got := string(chat.Tools[1].Function.Parameters); !strings.Contains(got, `"input"`) {
		t.Fatalf("custom parameters = %s", got)
	}
}

func TestResponsesRequestMergesAdditionalTools(t *testing.T) {
	req := responsesRequest{
		Model: "public-model",
		Input: json.RawMessage(`[
			{"type":"additional_tools","role":"developer","tools":[
				{"type":"function","name":"read_file","description":"duplicate","parameters":{"type":"object"}},
				{"type":"function","name":"write_file","parameters":{"type":"object"}},
				{"type":"custom","name":"exec","description":"Run code"},
				{"type":"namespace","name":"collaboration","description":"Agent coordination","tools":[
					{"type":"function","name":"wait_agent","description":"Wait for an agent","parameters":{"type":"object"}}
				]}
			]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}
		]`),
		Tools: []responsesTool{{
			Type: "function", Name: "read_file", Description: "top-level definition",
			Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`),
		}},
	}

	chat, toolTargets, err := req.chatRequest("")
	if err != nil {
		t.Fatal(err)
	}
	if len(chat.Messages) != 1 || chat.Messages[0].Role != "user" || chat.Messages[0].Content != "hello" {
		t.Fatalf("messages = %#v", chat.Messages)
	}
	if len(chat.Tools) != 4 {
		t.Fatalf("tools = %#v", chat.Tools)
	}
	if chat.Tools[0].Function.Description != "top-level definition" {
		t.Fatalf("duplicate tool did not preserve the first definition: %#v", chat.Tools[0])
	}
	if chat.Tools[1].Function.Name != "write_file" || chat.Tools[2].Function.Name != "exec" || chat.Tools[3].Function.Name != "collaboration.wait_agent" {
		t.Fatalf("merged tools = %#v", chat.Tools)
	}
	if toolTargets["exec"].Type != "custom" {
		t.Fatalf("custom tool target = %#v", toolTargets)
	}
	if got := toolTargets["collaboration.wait_agent"]; got.Type != "function" || got.Name != "wait_agent" || got.Namespace != "collaboration" {
		t.Fatalf("namespace tool target = %#v", got)
	}
}

func TestResponsesRequestConvertsNamespacedCallHistory(t *testing.T) {
	req := responsesRequest{
		Model: "public-model",
		Input: json.RawMessage(`[
			{"type":"function_call","id":"fc_1","call_id":"call_1","name":"wait_agent","namespace":"collaboration","arguments":"{\"timeout_ms\":10000}"},
			{"type":"function_call_output","call_id":"call_1","output":"timed out"}
		]`),
	}

	chat, _, err := req.chatRequest("")
	if err != nil {
		t.Fatal(err)
	}
	if len(chat.Messages) != 2 || len(chat.Messages[0].ToolCalls) != 1 {
		t.Fatalf("messages = %#v", chat.Messages)
	}
	if got := chat.Messages[0].ToolCalls[0].Function.Name; got != "collaboration.wait_agent" {
		t.Fatalf("qualified call name = %q", got)
	}
	if got := chat.Messages[1].Name; got != "collaboration.wait_agent" {
		t.Fatalf("qualified result name = %q", got)
	}
}

func TestResponsesResponseFunctionAndCustomCalls(t *testing.T) {
	store := false
	req := responsesRequest{
		Model: "public-model",
		Input: json.RawMessage(`"use tools"`),
		Store: &store,
		Tools: []responsesTool{
			{Type: "function", Name: "read_file"},
			{Type: "custom", Name: "shell"},
		},
	}
	reply := &gateway.Reply{
		Model: "resolved-model", TodoID: "todo-1", Content: "Calling tools.",
		ToolCalls: []openai.ToolCall{
			{ID: "call_1", Type: "function", Function: openai.FunctionCall{Name: "read_file", Arguments: `{"path":"a.txt"}`}},
			{ID: "call_2", Type: "function", Function: openai.FunctionCall{Name: "shell", Arguments: `{"input":"pwd"}`}},
			{ID: "call_3", Type: "function", Function: openai.FunctionCall{Name: "collaboration.wait_agent", Arguments: `{"timeout_ms":10000}`}},
		},
	}
	response := buildResponsesResponse(req, reply, map[string]responsesToolTarget{
		"shell":                    {Type: "custom", Name: "shell"},
		"collaboration.wait_agent": {Type: "function", Name: "wait_agent", Namespace: "collaboration"},
	})
	if response.Object != "response" || response.Model != "public-model" || response.Status != "completed" {
		t.Fatalf("response = %#v", response)
	}
	if len(response.Output) != 4 {
		t.Fatalf("output = %#v", response.Output)
	}
	if response.Output[1].Type != "function_call" || response.Output[1].CallID != "call_1" {
		t.Fatalf("function call = %#v", response.Output[1])
	}
	if response.Output[2].Type != "custom_tool_call" || response.Output[2].Input != "pwd" {
		t.Fatalf("custom call = %#v", response.Output[2])
	}
	if response.Output[3].Type != "function_call" || response.Output[3].Name != "wait_agent" || response.Output[3].Namespace != "collaboration" {
		t.Fatalf("namespace call = %#v", response.Output[3])
	}
	if response.Metadata[openai.TodoIDMetadataKey] != "todo-1" || response.Store {
		t.Fatalf("metadata/store = %#v/%v", response.Metadata, response.Store)
	}

	todoID, ok := todoIDFromResponseID(response.ID)
	if !ok || todoID != "todo-1" {
		t.Fatalf("response id %q decoded to %q, %v", response.ID, todoID, ok)
	}
	if _, ok := todoIDFromResponseID("resp_external"); ok {
		t.Fatal("accepted an external response id")
	}
}

func TestResponsesStreamIncludesTypedEvents(t *testing.T) {
	recorder := httptest.NewRecorder()
	stream := &responsesSSE{
		w: recorder, flusher: recorder,
		request: responsesRequest{Model: "public-model", Input: json.RawMessage(`"hello"`)},
		toolTargets: map[string]responsesToolTarget{
			"files.read_file": {Type: "function", Name: "read_file", Namespace: "files"},
		},
	}
	if err := stream.start("resolved-model", "todo-1"); err != nil {
		t.Fatal(err)
	}
	if err := stream.textDelta("hello"); err != nil {
		t.Fatal(err)
	}
	if err := stream.finish(&gateway.Reply{
		Model: "resolved-model", TodoID: "todo-1", Content: "hello",
		ToolCalls: []openai.ToolCall{{
			ID: "call_1", Type: "function",
			Function: openai.FunctionCall{Name: "files.read_file", Arguments: `{"path":"a.txt"}`},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	body := recorder.Body.String()
	for _, want := range []string{
		"event: response.created",
		"event: response.output_text.delta",
		"event: response.function_call_arguments.done",
		`"namespace":"files"`,
		"event: response.output_item.done",
		"event: response.completed",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("stream does not contain %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "data: [DONE]") {
		t.Fatalf("Responses stream contains Chat Completions terminator:\n%s", body)
	}
}

func TestResponsesSSEWritesTextDeltasBeforeCompletion(t *testing.T) {
	recorder := httptest.NewRecorder()
	stream := &responsesSSE{
		w: recorder, flusher: recorder,
		request:     responsesRequest{Model: "public-model", Input: json.RawMessage(`"hello"`)},
		toolTargets: map[string]responsesToolTarget{},
	}
	if err := stream.start("resolved-model", "todo-1"); err != nil {
		t.Fatal(err)
	}
	if err := stream.textDelta("hello"); err != nil {
		t.Fatal(err)
	}
	partial := recorder.Body.String()
	if !strings.Contains(partial, `"delta":"hello"`) || strings.Contains(partial, "response.completed") {
		t.Fatalf("partial stream = %s", partial)
	}
	if err := stream.textDelta(" world"); err != nil {
		t.Fatal(err)
	}
	if err := stream.finish(&gateway.Reply{
		Model: "resolved-model", TodoID: "todo-1", Content: "hello world",
	}); err != nil {
		t.Fatal(err)
	}
	body := recorder.Body.String()
	created := strings.Index(body, "event: response.created")
	first := strings.Index(body, `"delta":"hello"`)
	second := strings.Index(body, `"delta":" world"`)
	completed := strings.Index(body, "event: response.completed")
	if created < 0 || first <= created || second <= first || completed <= second {
		t.Fatalf("event order is wrong:\n%s", body)
	}
	if !strings.Contains(body, `"output_text":"hello world"`) {
		t.Fatalf("completed response does not match deltas:\n%s", body)
	}
}

func TestResponsesSSECompletesCustomAndNamespaceTools(t *testing.T) {
	recorder := httptest.NewRecorder()
	stream := &responsesSSE{
		w: recorder, flusher: recorder,
		request: responsesRequest{
			Model: "public-model", Input: json.RawMessage(`"use tools"`),
			Tools: []responsesTool{{Type: "custom", Name: "shell"}},
		},
		toolTargets: map[string]responsesToolTarget{
			"shell":                    {Type: "custom", Name: "shell"},
			"collaboration.wait_agent": {Type: "function", Name: "wait_agent", Namespace: "collaboration"},
		},
	}
	if err := stream.start("resolved-model", "todo-1"); err != nil {
		t.Fatal(err)
	}
	reply := &gateway.Reply{
		Model: "resolved-model", TodoID: "todo-1",
		ToolCalls: []openai.ToolCall{
			{ID: "call_1", Type: "function", Function: openai.FunctionCall{Name: "shell", Arguments: `{"input":"pwd"}`}},
			{ID: "call_2", Type: "function", Function: openai.FunctionCall{Name: "collaboration.wait_agent", Arguments: `{"timeout_ms":10}`}},
		},
	}
	if err := stream.finish(reply); err != nil {
		t.Fatal(err)
	}
	body := recorder.Body.String()
	for _, want := range []string{
		"response.custom_tool_call_input.done",
		`"input":"pwd"`,
		`"namespace":"collaboration"`,
		`"name":"wait_agent"`,
		"response.function_call_arguments.done",
		"response.completed",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("stream does not contain %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "TOOL_CALL") {
		t.Fatalf("tool protocol leaked into Responses stream:\n%s", body)
	}
}
