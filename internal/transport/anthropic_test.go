package transport

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"todo2api/internal/config"
	"todo2api/internal/gateway"
	"todo2api/internal/openai"
)

func TestAnthropicImageBlockBecomesAttachment(t *testing.T) {
	req := anthropicRequest{Model: "public-model", Messages: []anthropicInputMessage{{Role: "user", Content: json.RawMessage(`[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}},{"type":"text","text":"describe"}]`)}}}
	chat, err := req.chatRequest("")
	if err != nil {
		t.Fatal(err)
	}
	if len(chat.Attachments) != 1 || chat.Attachments[0].MIMEType != "image/png" || len(chat.Attachments[0].Data) != 3 {
		t.Fatalf("attachments=%#v", chat.Attachments)
	}
	if !strings.Contains(chat.Messages[0].Content, "image attached") {
		t.Fatalf("content=%q", chat.Messages[0].Content)
	}
}

func TestAnthropicRequestConvertsToolHistory(t *testing.T) {
	req := anthropicRequest{
		Model:  "public-model",
		System: json.RawMessage(`[{"type":"text","text":"system prompt"}]`),
		Messages: []anthropicInputMessage{
			{Role: "user", Content: json.RawMessage(`"read the file"`)},
			{Role: "assistant", Content: json.RawMessage(`[
				{"type":"text","text":"I'll read it."},
				{"type":"tool_use","id":"toolu_1","name":"read_file","input":{"path":"a.txt"}}
			]`)},
			{Role: "user", Content: json.RawMessage(`[
				{"type":"tool_result","tool_use_id":"toolu_1","content":"file contents"}
			]`)},
		},
		Tools: []anthropicTool{{
			Name: "read_file", Description: "Read a file",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`),
		}},
	}

	chat, err := req.chatRequest("todo-1")
	if err != nil {
		t.Fatal(err)
	}
	if chat.Model != "public-model" || chat.Metadata[openai.TodoIDMetadataKey] != "todo-1" {
		t.Fatalf("chat request = %#v", chat)
	}
	if chat.System != "system prompt" {
		t.Fatalf("system = %#v", chat.System)
	}
	if len(chat.Messages) != 3 {
		t.Fatalf("messages = %#v", chat.Messages)
	}
	if chat.Messages[0].Role != "user" || chat.Messages[0].Content != "read the file" {
		t.Fatalf("user message = %#v", chat.Messages[0])
	}
	assistant := chat.Messages[1]
	if assistant.Content != "I'll read it." || len(assistant.ToolCalls) != 1 {
		t.Fatalf("assistant message = %#v", assistant)
	}
	if assistant.ToolCalls[0].ID != "toolu_1" || assistant.ToolCalls[0].Function.Arguments != `{"path":"a.txt"}` {
		t.Fatalf("tool call = %#v", assistant.ToolCalls[0])
	}
	result := chat.Messages[2]
	if result.Role != "tool" || result.Name != "read_file" || result.Content != "file contents" {
		t.Fatalf("tool result = %#v", result)
	}
	if len(chat.Tools) != 1 || chat.Tools[0].Function.Name != "read_file" {
		t.Fatalf("tools = %#v", chat.Tools)
	}
}

func TestAnthropicRequestExtractsClaudeCodeSystemMessages(t *testing.T) {
	var req anthropicRequest
	if err := json.Unmarshal([]byte(`{
		"model":"public-model",
		"max_tokens":4096,
		"system":[
			{"type":"text","text":"top-level system","cache_control":{"type":"ephemeral"}}
		],
		"messages":[
			{"role":"system","content":[{"type":"text","text":"mid-conversation system","cache_control":{"type":"ephemeral"}}]},
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"private reasoning","signature":"signature"},
				{"type":"text","text":"Using a tool."},
				{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":"README.md"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"file contents"}],"cache_control":{"type":"ephemeral"}},
				{"type":"text","text":"continue"}
			]}
		],
		"tools":[{
			"name":"Read",
			"description":"Read a file",
			"input_schema":{"type":"object","properties":{"file_path":{"type":"string"}}},
			"cache_control":{"type":"ephemeral"}
		}],
		"tool_choice":{"type":"auto"},
		"thinking":{"type":"enabled","budget_tokens":1024},
		"metadata":{"user_id":"claude-code"},
		"stream":true
	}`), &req); err != nil {
		t.Fatal(err)
	}

	chat, err := req.chatRequest("todo-1")
	if err != nil {
		t.Fatal(err)
	}
	if chat.System != "top-level system\n\nmid-conversation system" {
		t.Fatalf("system = %q", chat.System)
	}
	if len(chat.Messages) != 3 {
		t.Fatalf("messages = %#v", chat.Messages)
	}
	for _, message := range chat.Messages {
		if message.Role == "system" {
			t.Fatalf("system leaked into messages: %#v", chat.Messages)
		}
	}
	if chat.Messages[0].Role != "assistant" || len(chat.Messages[0].ToolCalls) != 1 {
		t.Fatalf("assistant history = %#v", chat.Messages[0])
	}
	if chat.Messages[1].Role != "tool" || chat.Messages[1].Content != "file contents" {
		t.Fatalf("tool result = %#v", chat.Messages[1])
	}
	if chat.Messages[2].Role != "user" || chat.Messages[2].Content != "continue" {
		t.Fatalf("user continuation = %#v", chat.Messages[2])
	}
	if len(chat.Tools) != 1 || chat.Tools[0].Function.Name != "Read" {
		t.Fatalf("tools = %#v", chat.Tools)
	}
}

func TestAnthropicResponseAndStreamToolUse(t *testing.T) {
	usage := exactTestUsage()
	usage.CacheWriteTokens = 64
	reply := &gateway.Reply{
		Model: "resolved-model", TodoID: "todo-1", Content: "Using a tool.", Usage: usage,
		ToolCalls: []openai.ToolCall{{
			ID: "toolu_1", Type: "function",
			Function: openai.FunctionCall{Name: "read_file", Arguments: `{"path":"a.txt"}`},
		}},
	}
	response := buildAnthropicResponse("public-model", reply)
	if response.Model != "public-model" || response.StopReason == nil || *response.StopReason != "tool_use" {
		t.Fatalf("response = %#v", response)
	}
	if len(response.Content) != 2 || response.Content[1].Name != "read_file" {
		t.Fatalf("content = %#v", response.Content)
	}
	if response.Usage.InputTokens != 852 || response.Usage.CacheReadInputTokens != 1536 || response.Usage.CacheCreationInputTokens != 64 || response.Usage.OutputTokens != 11 {
		t.Fatalf("usage = %#v", response.Usage)
	}
	var input map[string]string
	if err := json.Unmarshal(response.Content[1].Input, &input); err != nil || input["path"] != "a.txt" {
		t.Fatalf("tool input = %#v, err = %v", input, err)
	}

	recorder := httptest.NewRecorder()
	stream := &anthropicSSE{w: recorder, flusher: recorder, requestedModel: "public-model"}
	if err := stream.start(reply.Model, reply.TodoID); err != nil {
		t.Fatal(err)
	}
	if err := stream.textDelta(reply.Content); err != nil {
		t.Fatal(err)
	}
	if err := stream.finish(reply); err != nil {
		t.Fatal(err)
	}
	body := recorder.Body.String()
	for _, want := range []string{
		"event: message_start",
		"event: content_block_start",
		`"type":"input_json_delta"`,
		`"stop_reason":"tool_use"`,
		"event: message_stop",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("stream does not contain %q:\n%s", want, body)
		}
	}
	deltaUsage := anthropicUsageEvent(t, body, "message_delta")
	if deltaUsage.InputTokens != 852 || deltaUsage.CacheReadInputTokens != 1536 || deltaUsage.CacheCreationInputTokens != 64 || deltaUsage.OutputTokens != 11 {
		t.Fatalf("message_delta usage = %#v", deltaUsage)
	}
}

func TestAnthropicSSEWritesTextDeltasBeforeStop(t *testing.T) {
	recorder := httptest.NewRecorder()
	stream := &anthropicSSE{
		w: recorder, flusher: recorder, requestedModel: "public-model",
	}
	if err := stream.start("resolved-model", "todo-1"); err != nil {
		t.Fatal(err)
	}
	if err := stream.textDelta("hello"); err != nil {
		t.Fatal(err)
	}
	partial := recorder.Body.String()
	if !strings.Contains(partial, `"text":"hello"`) || strings.Contains(partial, "event: message_stop") {
		t.Fatalf("partial stream = %s", partial)
	}
	if err := stream.textDelta(" world"); err != nil {
		t.Fatal(err)
	}
	if err := stream.finish(&gateway.Reply{Model: "resolved-model", TodoID: "todo-1"}); err != nil {
		t.Fatal(err)
	}
	body := recorder.Body.String()
	first := strings.Index(body, `"text":"hello"`)
	second := strings.Index(body, `"text":" world"`)
	stop := strings.Index(body, "event: message_stop")
	if first < 0 || second <= first || stop <= second {
		t.Fatalf("event order is wrong:\n%s", body)
	}
	if recorder.Header().Get(todoIDHeader) != "todo-1" || recorder.Header().Get("X-Accel-Buffering") != "no" {
		t.Fatalf("SSE headers = %#v", recorder.Header())
	}
}

func TestAnthropicMisspellingAndAPIKeyAuth(t *testing.T) {
	s := &Server{cfg: &config.Config{Server: config.ServerConfig{ClientTokens: []string{"client-key"}}}}
	handler := s.Handler()
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messeges/count_tokens", strings.NewReader(`{
		"model":"public-model","messages":[{"role":"user","content":"hello"}]
	}`))
	req.Header.Set("X-API-Key", "client-key")
	handler.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response map[string]int
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response["input_tokens"] <= 0 || recorder.Header().Get("X-Todo2API-Token-Estimate") != "true" {
		t.Fatalf("response = %#v, headers = %#v", response, recorder.Header())
	}
}

func anthropicUsageEvent(t *testing.T, body, eventType string) anthropicUsage {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event struct {
			Type  string         `json:"type"`
			Usage anthropicUsage `json:"usage"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			t.Fatalf("decode Anthropic event %q: %v", line, err)
		}
		if event.Type == eventType {
			return event.Usage
		}
	}
	t.Fatalf("event %q not found in:\n%s", eventType, body)
	return anthropicUsage{}
}
