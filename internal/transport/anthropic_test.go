package transport

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
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

// claudeCodeRequestBody mirrors the shape Claude Code 2.1.226 sends to
// POST /v1/messages?beta=true: system as a block array, default tools with
// cache_control, and top-level fields this gateway tolerates but does not
// interpret (tool_choice, thinking, context_management, metadata).
const claudeCodeRequestBody = `{
	"model": "claude-sonnet-4-5-20250929",
	"max_tokens": 32768,
	"stream": true,
	"system": [
		{"type": "text", "text": "You are Claude Code, an expert software engineering agent.", "cache_control": {"type": "ephemeral"}}
	],
	"messages": [
		{"role": "user", "content": [{"type": "text", "text": "hello", "cache_control": {"type": "ephemeral"}}]}
	],
	"tools": [
		{"name": "Bash", "description": "Run shell commands", "input_schema": {"type": "object", "properties": {"command": {"type": "string"}}}, "cache_control": {"type": "ephemeral"}},
		{"name": "Read", "description": "Read a file", "input_schema": {"type": "object", "properties": {"file_path": {"type": "string"}}}},
		{"name": "Task", "description": "Delegate work to a subagent", "input_schema": {"type": "object", "properties": {"description": {"type": "string"}}}}
	],
	"tool_choice": {"type": "auto"},
	"thinking": {"type": "enabled", "budget_tokens": 1024},
	"context_management": {"enabled": true, "strategy": "auto"},
	"metadata": {"user_id": "test-user", "session_id": "sess_123"}
}`

func anthropicTestServer() *Server {
	return &Server{cfg: &config.Config{Server: config.ServerConfig{ClientTokens: []string{"client-key"}}}}
}

func TestAnthropicClaudeCodeRequestDecodes(t *testing.T) {
	// The real request URL carries ?beta=true; decode must not care.
	r := httptest.NewRequest(http.MethodPost, "/v1/messages?beta=true", strings.NewReader(claudeCodeRequestBody))
	req, err := decodeAnthropicRequest(r)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if req.Model != "claude-sonnet-4-5-20250929" {
		t.Fatalf("model = %q", req.Model)
	}
	if req.MaxTokens != 32768 || !req.Stream {
		t.Fatalf("max_tokens=%d stream=%v", req.MaxTokens, req.Stream)
	}
	if len(req.Messages) != 1 {
		t.Fatalf("messages = %#v", req.Messages)
	}
	if len(req.Tools) != 3 {
		t.Fatalf("tools = %#v", req.Tools)
	}
	if len(req.Metadata) != 2 || req.Metadata["user_id"] != "test-user" {
		t.Fatalf("metadata = %#v", req.Metadata)
	}
	if len(bytes.TrimSpace(req.System)) == 0 {
		t.Fatal("system not decoded")
	}
}

func TestAnthropicClaudeCodeRequestOverHTTP(t *testing.T) {
	handler := anthropicTestServer().Handler()
	for _, auth := range []struct{ name, header, value string }{
		{"bearer", "Authorization", "Bearer client-key"},
		{"api-key", "X-API-Key", "client-key"},
	} {
		t.Run(auth.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens?beta=true", strings.NewReader(claudeCodeRequestBody))
			req.Header.Set(auth.header, auth.value)
			handler.ServeHTTP(recorder, req)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
			if id := recorder.Header().Get("X-Request-ID"); !strings.HasPrefix(id, "req_") {
				t.Fatalf("request id = %q", id)
			}
			var response map[string]int
			if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
				t.Fatal(err)
			}
			if response["input_tokens"] <= 0 {
				t.Fatalf("input_tokens = %#v", response)
			}
		})
	}
}

func TestAnthropicMessagesDecodeErrors(t *testing.T) {
	handler := anthropicTestServer().Handler()
	cases := []struct{ name, body, want string }{
		{"empty body", "", "request body is empty"},
		{"whitespace only", "  \n\t", "request body is empty"},
		{"truncated JSON", `{"model":`, "unexpected end of JSON input at byte offset 9"},
		{"max_tokens wrong type", `{"max_tokens":"4096","model":"m","messages":[{"role":"user","content":"hi"}]}`, "field max_tokens must be a number, got string"},
		{"messages wrong type", `{"model":"m","messages":{}}`, "field messages must be an array, got object"},
		{"stream wrong type", `{"model":"m","stream":"yes","messages":[]}`, "field stream must be a boolean, got string"},
		{"nested role wrong type", `{"model":"m","messages":[{"role":5}]}`, "field messages.role must be a string, got number"},
		{"top-level array", `[]`, "field request must be an object, got array"},
		{"multiple JSON values", `{"model":"m"}{"model":"x"}`, "request body contains multiple JSON values"},
		{"trailing garbage", `{"model":"m"} xyz`, "request body contains trailing data"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/messages?beta=true", strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer client-key")
			handler.ServeHTTP(recorder, req)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
			if recorder.Header().Get("X-Request-ID") == "" {
				t.Fatal("missing X-Request-ID on error response")
			}
			body := recorder.Body.String()
			if !strings.Contains(body, `"type":"invalid_request_error"`) {
				t.Fatalf("error type missing: %s", body)
			}
			if want := "invalid request body: " + tc.want; !strings.Contains(body, want) {
				t.Fatalf("body %s does not contain %q", body, want)
			}
		})
	}
}

func TestAnthropicCountTokensSharesDecodeErrors(t *testing.T) {
	handler := anthropicTestServer().Handler()
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(`{"max_tokens":"4096"}`))
	req.Header.Set("X-API-Key", "client-key")
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "invalid request body: field max_tokens must be a number, got string") {
		t.Fatalf("body = %s", recorder.Body.String())
	}
}

func TestAnthropicOversizedBodyRejected(t *testing.T) {
	old := maxJSONBodyBytes
	maxJSONBodyBytes = 1024
	t.Cleanup(func() { maxJSONBodyBytes = old })

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(strings.Repeat("x", 1025)))
	req.Header.Set("Authorization", "Bearer client-key")
	anthropicTestServer().Handler().ServeHTTP(recorder, req)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "invalid request body: request body exceeds 1 MiB") {
		t.Fatalf("body = %s", recorder.Body.String())
	}
}

func TestAnthropicContentEncoding(t *testing.T) {
	handler := anthropicTestServer().Handler()

	// gzip support is only added once a real failing request confirms it; until
	// then the encoding must fail with an explicit, actionable error.
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	req.Header.Set("X-API-Key", "client-key")
	req.Header.Set("Content-Encoding", "gzip")
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), `unsupported Content-Encoding \"gzip\"`) {
		t.Fatalf("gzip: status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("X-API-Key", "client-key")
	req.Header.Set("Content-Encoding", "identity")
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("identity: status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestAnthropicDecodeErrorDoesNotLeakBodyOrCredentials(t *testing.T) {
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	handler := anthropicTestServer().Handler()
	cases := []struct{ name, body, want string }{
		// Value would leak as "string" or "number 987654321" if printed raw.
		{"string value", `{"max_tokens":"SK-SECRET-STRING","model":"m"}`, "field max_tokens must be a number, got string"},
		{"number value", `{"messages":987654321,"model":"m"}`, "field messages must be an array, got number"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer client-key")
			req.Header.Set("X-API-Key", "client-key")
			req.Header.Set("X-Request-ID", "req-leak-test")
			handler.ServeHTTP(recorder, req)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), tc.want) {
				t.Fatalf("response lacks diagnostic %q: %s", tc.want, recorder.Body.String())
			}
			for _, secret := range []string{"SK-SECRET-STRING", "987654321", "client-key"} {
				if strings.Contains(recorder.Body.String(), secret) {
					t.Fatalf("response leaks %q: %s", secret, recorder.Body.String())
				}
			}
		})
	}

	logged := logs.String()
	for _, want := range []string{"req-leak-test", "/v1/messages", "max_tokens", "content-type=", "content-encoding=", "content-length="} {
		if !strings.Contains(logged, want) {
			t.Fatalf("log lacks %q:\n%s", want, logged)
		}
	}
	for _, secret := range []string{"SK-SECRET-STRING", "987654321", "client-key"} {
		if strings.Contains(logged, secret) {
			t.Fatalf("log leaks %q:\n%s", secret, logged)
		}
	}
}

func TestAnthropicRequestIDEchoedAndSanitized(t *testing.T) {
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"max_tokens":"x"}`))
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("X-Request-ID", "req-123\r\nX-Injected: yes")
	anthropicTestServer().Handler().ServeHTTP(recorder, req)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", recorder.Code)
	}
	got := recorder.Header().Get("X-Request-ID")
	if got != "req-123X-Injected: yes" {
		t.Fatalf("sanitized request id = %q", got)
	}
	if strings.ContainsAny(got, "\r\n") {
		t.Fatalf("request id allows header injection: %q", got)
	}
}
