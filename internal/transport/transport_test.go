package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"todo2api/internal/config"
	"todo2api/internal/gateway"
	"todo2api/internal/openai"
)

func TestBuildResponseIncludesToolCallAndTodoMetadata(t *testing.T) {
	reply := &gateway.Reply{
		Model:  "resolved-model",
		TodoID: "todo-1",
		Usage:  exactTestUsage(),
		ToolCalls: []openai.ToolCall{{
			ID:       "call-1",
			Type:     "function",
			Function: openai.FunctionCall{Name: "read_file", Arguments: `{"path":"a.txt"}`},
		}},
	}
	resp := buildResponse(reply)
	if resp.Model != "resolved-model" || resp.Metadata[openai.TodoIDMetadataKey] != "todo-1" {
		t.Fatalf("response = %#v", resp)
	}
	if got := *resp.Choices[0].FinishReason; got != "tool_calls" {
		t.Fatalf("finish reason = %q", got)
	}
	if resp.Usage == nil || resp.Usage.PromptTokens != 2388 || resp.Usage.CompletionTokens != 11 || resp.Usage.TotalTokens != 2399 {
		t.Fatalf("usage = %#v", resp.Usage)
	}
	if resp.Usage.PromptTokensDetails == nil || resp.Usage.PromptTokensDetails.CachedTokens != 1536 {
		t.Fatalf("prompt token details = %#v", resp.Usage.PromptTokensDetails)
	}
}

func TestStreamReplyIncludesToolIndexAndMetadata(t *testing.T) {
	reply := &gateway.Reply{
		Model:  "resolved-model",
		TodoID: "todo-1",
		ToolCalls: []openai.ToolCall{{
			ID:       "call-1",
			Type:     "function",
			Function: openai.FunctionCall{Name: "read_file", Arguments: `{}`},
		}},
	}
	recorder := httptest.NewRecorder()
	stream := &chatSSE{w: recorder, flusher: recorder}
	if err := stream.start(reply.Model, reply.TodoID); err != nil {
		t.Fatal(err)
	}
	if err := stream.finish(reply); err != nil {
		t.Fatal(err)
	}
	body := recorder.Body.String()
	for _, want := range []string{
		`"tool_calls"`,
		`"index":0`,
		`"todo2api.todo_id":"todo-1"`,
		"data: [DONE]",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("stream does not contain %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, `"message":`) {
		t.Fatalf("stream contains a non-streaming message object:\n%s", body)
	}
}

func TestChatSSEWritesEachDeltaBeforeFinish(t *testing.T) {
	recorder := httptest.NewRecorder()
	stream := &chatSSE{w: recorder, flusher: recorder}
	if err := stream.start("resolved-model", "todo-1"); err != nil {
		t.Fatal(err)
	}
	if err := stream.onGatewayEvent(gateway.StreamEvent{
		Type: gateway.StreamTextDelta, Delta: "hello",
	}); err != nil {
		t.Fatal(err)
	}
	partial := recorder.Body.String()
	if !strings.Contains(partial, `"content":"hello"`) || strings.Contains(partial, "data: [DONE]") {
		t.Fatalf("partial stream = %s", partial)
	}
	if err := stream.onGatewayEvent(gateway.StreamEvent{
		Type: gateway.StreamTextDelta, Delta: " world",
	}); err != nil {
		t.Fatal(err)
	}
	if err := stream.finish(&gateway.Reply{Model: "resolved-model", TodoID: "todo-1"}); err != nil {
		t.Fatal(err)
	}
	body := recorder.Body.String()
	first := strings.Index(body, `"content":"hello"`)
	second := strings.Index(body, `"content":" world"`)
	stop := strings.Index(body, `"finish_reason":"stop"`)
	if first < 0 || second <= first || stop <= second {
		t.Fatalf("event order is wrong:\n%s", body)
	}
	if recorder.Header().Get("X-Accel-Buffering") != "no" || !strings.Contains(recorder.Header().Get("Cache-Control"), "no-transform") {
		t.Fatalf("SSE headers = %#v", recorder.Header())
	}
}

func TestChatSSEUsageChunkRequiresIncludeUsage(t *testing.T) {
	for _, includeUsage := range []bool{false, true} {
		t.Run(fmt.Sprint(includeUsage), func(t *testing.T) {
			recorder := httptest.NewRecorder()
			stream := &chatSSE{w: recorder, flusher: recorder, includeUsage: includeUsage}
			if err := stream.start("resolved-model", "todo-1"); err != nil {
				t.Fatal(err)
			}
			if err := stream.finish(&gateway.Reply{
				Model: "resolved-model", TodoID: "todo-1", Usage: exactTestUsage(),
			}); err != nil {
				t.Fatal(err)
			}

			usageChunks := 0
			for _, chunk := range decodeChatChunks(t, recorder.Body.String()) {
				if chunk.Usage == nil {
					continue
				}
				usageChunks++
				if len(chunk.Choices) != 0 || chunk.Usage.PromptTokens != 2388 || chunk.Usage.CompletionTokens != 11 || chunk.Usage.TotalTokens != 2399 {
					t.Fatalf("usage chunk = %#v", chunk)
				}
			}
			want := 0
			if includeUsage {
				want = 1
			}
			if usageChunks != want {
				t.Fatalf("usage chunks = %d, want %d; stream:\n%s", usageChunks, want, recorder.Body.String())
			}
		})
	}
}

func TestUnavailableChatUsageIsOmitted(t *testing.T) {
	response := buildResponse(&gateway.Reply{Model: "model", TodoID: "todo-1"})
	b, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"usage"`) {
		t.Fatalf("unavailable usage was serialized: %s", b)
	}
}

func TestGatewayUnavailableMapsToRetryable503(t *testing.T) {
	recorder := httptest.NewRecorder()
	writeAnthropicGatewayErr(recorder, fmt.Errorf("create failed: %w", gateway.ErrAccountsUnavailable))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", recorder.Code)
	}
	if retryAfter := recorder.Header().Get("Retry-After"); retryAfter != "60" {
		t.Fatalf("Retry-After = %q, want 60", retryAfter)
	}
	if !strings.Contains(recorder.Body.String(), `"type":"api_error"`) {
		t.Fatalf("body = %s", recorder.Body.String())
	}
}

func TestAuthNeverAcceptsEmptyConfiguredToken(t *testing.T) {
	s := &Server{cfg: &config.Config{Server: config.ServerConfig{ClientTokens: []string{""}}}}
	called := false
	handler := s.auth(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	})

	recorder := httptest.NewRecorder()
	handler(recorder, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if recorder.Code != http.StatusUnauthorized || called {
		t.Fatalf("status=%d called=%v", recorder.Code, called)
	}
}

func TestAuthRequiresBearerScheme(t *testing.T) {
	s := &Server{cfg: &config.Config{Server: config.ServerConfig{ClientTokens: []string{"client-token"}}}}
	handler := s.auth(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })

	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("Authorization", "client-token")
	recorder := httptest.NewRecorder()
	handler(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("bare authorization status=%d", recorder.Code)
	}

	request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("Authorization", "bearer client-token")
	recorder = httptest.NewRecorder()
	handler(recorder, request)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("bearer authorization status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestModelsIncludesDefaultAndIsSorted(t *testing.T) {
	s := &Server{cfg: &config.Config{Models: config.ModelsConfig{
		Default: "model-default",
		Aliases: map[string]string{
			"model-z": "upstream-z",
			"model-a": "upstream-a",
		},
	}}}
	recorder := httptest.NewRecorder()
	s.handleModels(recorder, httptest.NewRequest(http.MethodGet, "/v1/models", nil))

	var list openai.ModelList
	if err := json.NewDecoder(recorder.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusOK || len(list.Data) != 3 {
		t.Fatalf("status=%d list=%#v", recorder.Code, list)
	}
	want := []string{"model-a", "model-default", "model-z"}
	for i, model := range list.Data {
		if model.ID != want[i] {
			t.Fatalf("model[%d] = %q, want %q", i, model.ID, want[i])
		}
	}
}

func exactTestUsage() gateway.TokenUsage {
	return gateway.TokenUsage{
		InputTokens: 852, OutputTokens: 11, CacheReadTokens: 1536, Available: true,
	}
}

// claudeCodeRelayChatFixture mirrors the OpenAI-format body new-api produces
// when it converts the real Claude Code 2.1.226 /v1/messages request (captured
// from the live deployment) into a chat completions call: user messages carry
// content as an array of text parts instead of a plain string. Redacted —
// no credentials, user ids, or prompt text.
const claudeCodeRelayChatFixture = `{
	"model": "claude-opus-5",
	"max_tokens": 64000,
	"stream": true,
	"messages": [
		{"role": "system", "content": "You are Claude Code, an expert software engineering agent. The following deferred tools are now available via ToolSearch."},
		{"role": "user", "content": [
			{"type": "text", "text": "Today is 2026-08-14."},
			{"type": "text", "text": "Reply with exactly: PONG"}
		]},
		{"role": "assistant", "tool_calls": [
			{"id": "toolu_01", "type": "function", "function": {"name": "Bash", "arguments": "{\"command\":\"ls\"}"}}
		]}
	],
	"tools": [
		{"type": "function", "function": {"name": "Bash", "description": "Run shell commands", "parameters": {"type": "object", "properties": {"command": {"type": "string"}}}}},
		{"type": "function", "function": {"name": "Read", "description": "Read a file", "parameters": {"type": "object", "properties": {"file_path": {"type": "string"}}}}}
	]
}`

func TestChatCompletionsAcceptsContentParts(t *testing.T) {
	var req openai.ChatRequest
	if err := decodeJSONBody(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(claudeCodeRelayChatFixture)), &req); err != nil {
		t.Fatalf("decode relay fixture: %v", err)
	}
	if req.Model != "claude-opus-5" || !req.Stream {
		t.Fatalf("request = %#v", req)
	}
	if len(req.Messages) != 3 {
		t.Fatalf("messages = %#v", req.Messages)
	}
	system := req.Messages[0]
	if system.Role != "system" || len(system.Parts) != 0 || !strings.HasPrefix(system.Content, "You are Claude Code") {
		t.Fatalf("system message = %#v", system)
	}
	user := req.Messages[1]
	if user.Role != "user" || len(user.Parts) != 2 {
		t.Fatalf("user message = %#v", user)
	}
	assistant := req.Messages[2]
	if assistant.Role != "assistant" || len(assistant.ToolCalls) != 1 || assistant.ToolCalls[0].Function.Name != "Bash" {
		t.Fatalf("assistant message = %#v", assistant)
	}
	if len(req.Tools) != 2 || req.Tools[1].Function.Name != "Read" {
		t.Fatalf("tools = %#v", req.Tools)
	}

	text, attachments, err := chatContentParts(user.Parts)
	if err != nil {
		t.Fatal(err)
	}
	if text != "Today is 2026-08-14.\n\nReply with exactly: PONG" {
		t.Fatalf("rendered content = %q", text)
	}
	if len(attachments) != 0 {
		t.Fatalf("attachments = %#v", attachments)
	}
}

func TestChatContentPartsRendering(t *testing.T) {
	text, attachments, err := chatContentParts([]openai.ContentPart{
		{Type: "text", Text: "first"},
		{Type: "text", Text: "second"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if text != "first\n\nsecond" || len(attachments) != 0 {
		t.Fatalf("text parts: text=%q attachments=%#v", text, attachments)
	}

	text, attachments, err = chatContentParts([]openai.ContentPart{
		{Type: "text", Text: "see this"},
		{Type: "image_url", ImageURL: json.RawMessage(`{"url":"data:image/png;base64,AAAA"}`)},
		{Type: "text", Text: "and this"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if text != "see this\n\n[image attached: image-1.png]\n\nand this" {
		t.Fatalf("interleaved text = %q", text)
	}
	if len(attachments) != 1 || attachments[0].MIMEType != "image/png" || len(attachments[0].Data) != 3 {
		t.Fatalf("attachments = %#v", attachments)
	}

	text, attachments, err = chatContentParts([]openai.ContentPart{
		{Type: "image_url", ImageURL: json.RawMessage(`{"url":"https://example.com/a.png"}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(attachments) != 0 || !strings.Contains(text, "[image URL provided but not uploaded]") {
		t.Fatalf("remote image: text=%q attachments=%#v", text, attachments)
	}

	if _, _, err := chatContentParts([]openai.ContentPart{{Type: "audio", ImageURL: json.RawMessage(`{}`)}}); err == nil || !strings.Contains(err.Error(), `unsupported content part type "audio"`) {
		t.Fatalf("unsupported part error = %v", err)
	}
}

func TestChatCompletionsDecodeErrorIsDiagnostic(t *testing.T) {
	s := &Server{cfg: &config.Config{Server: config.ServerConfig{ClientTokens: []string{"client-key"}}}}
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"stream":"yes","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`))
	req.Header.Set("Authorization", "Bearer client-key")
	s.Handler().ServeHTTP(recorder, req)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "invalid request body: field stream must be a boolean, got string") {
		t.Fatalf("body = %s", recorder.Body.String())
	}
}

func decodeChatChunks(t *testing.T, body string) []openai.ChatResponse {
	t.Helper()
	var chunks []openai.ChatResponse
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
			continue
		}
		var chunk openai.ChatResponse
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk); err != nil {
			t.Fatalf("decode chunk %q: %v", line, err)
		}
		chunks = append(chunks, chunk)
	}
	return chunks
}

// recordingGateway captures the ChatRequest handlers pass to the gateway so
// tests can observe the transport→gateway wiring without an upstream.
type recordingGateway struct {
	mu    sync.Mutex
	last  openai.ChatRequest
	reply *gateway.Reply
	err   error
}

func (g *recordingGateway) Complete(_ context.Context, req openai.ChatRequest) (*gateway.Reply, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.last = req
	return g.reply, g.err
}

func (g *recordingGateway) Stream(context.Context, openai.ChatRequest, func(gateway.StreamEvent) error) (*gateway.Reply, error) {
	return g.reply, g.err
}

func (g *recordingGateway) Models() []openai.Model { return nil }

func (g *recordingGateway) lastRequest() openai.ChatRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.last
}

func TestChatCompletionsWiresPartsIntoMessage(t *testing.T) {
	gw := &recordingGateway{reply: &gateway.Reply{Content: "ok", Model: "claude-opus-5", TodoID: "todo-1"}}
	handler := (&Server{
		cfg: &config.Config{Server: config.ServerConfig{ClientTokens: []string{"client-key"}}},
		gw:  gw,
	}).Handler()

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model":"claude-opus-5",
		"messages":[{"role":"user","content":[
			{"type":"text","text":"see this"},
			{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}},
			{"type":"text","text":"and this"}
		]}]
	}`))
	req.Header.Set("Authorization", "Bearer client-key")
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	got := gw.lastRequest()
	if len(got.Messages) != 1 {
		t.Fatalf("messages = %#v", got.Messages)
	}
	message := got.Messages[0]
	if message.Content != "see this\n\n[image attached: image-1.png]\n\nand this" {
		t.Fatalf("content = %q", message.Content)
	}
	if len(message.Attachments) != 1 || message.Attachments[0].MIMEType != "image/png" || len(message.Attachments[0].Data) != 3 {
		t.Fatalf("attachments = %#v", message.Attachments)
	}
}

func TestChatCompletionsCamelCaseImageUrlWiresAttachment(t *testing.T) {
	gw := &recordingGateway{reply: &gateway.Reply{Content: "ok", Model: "m", TodoID: "todo-1"}}
	handler := (&Server{
		cfg: &config.Config{Server: config.ServerConfig{ClientTokens: []string{"client-key"}}},
		gw:  gw,
	}).Handler()

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model":"m",
		"messages":[{"role":"user","content":[
			{"type":"image_url","imageUrl":{"url":"data:image/png;base64,AAAA"}}
		]}]
	}`))
	req.Header.Set("X-API-Key", "client-key")
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	attachments := gw.lastRequest().Messages[0].Attachments
	if len(attachments) != 1 || attachments[0].MIMEType != "image/png" || len(attachments[0].Data) != 3 {
		t.Fatalf("attachments = %#v", attachments)
	}
}

func TestChatCompletionsRejectsInvalidContentParts(t *testing.T) {
	handler := (&Server{
		cfg: &config.Config{Server: config.ServerConfig{ClientTokens: []string{"client-key"}}},
		gw:  &recordingGateway{reply: &gateway.Reply{Content: "ok", Model: "m", TodoID: "todo-1"}},
	}).Handler()
	cases := []struct{ name, part, want string }{
		{"no type no text", `{"foo":"bar"}`, "content part 0 has no type and no text"},
		{"empty object", `{}`, "content part 0 has no type and no text"},
		{"unknown type", `{"type":"audio"}`, `unsupported content part type \"audio\"`},
		{"text without text", `{"type":"text"}`, `content part of type \"text\" has no text`},
		{"input_text without text", `{"type":"input_text","text":""}`, `content part of type \"input_text\" has no text`},
		{"image without url", `{"type":"image_url"}`, "image_url part requires an image_url value"},
		{"invalid image url", `{"type":"image_url","image_url":{"url":123}}`, "invalid image_url"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
				`{"model":"m","messages":[{"role":"user","content":[`+tc.part+`]}]}`,
			))
			req.Header.Set("Authorization", "Bearer client-key")
			handler.ServeHTTP(recorder, req)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
			if recorder.Header().Get("X-Request-ID") == "" {
				t.Fatal("missing X-Request-ID on error response")
			}
			if !strings.Contains(recorder.Body.String(), tc.want) {
				t.Fatalf("body %s does not contain %q", recorder.Body.String(), tc.want)
			}
		})
	}
}

func TestAnthropicWiresImagesPerMessage(t *testing.T) {
	gw := &recordingGateway{reply: &gateway.Reply{Content: "ok", Model: "m", TodoID: "todo-1"}}
	handler := (&Server{
		cfg: &config.Config{Server: config.ServerConfig{ClientTokens: []string{"client-key"}}},
		gw:  gw,
	}).Handler()

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"m",
		"messages":[{"role":"user","content":[
			{"type":"text","text":"describe this"},
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}
		]}]
	}`))
	req.Header.Set("Authorization", "Bearer client-key")
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	got := gw.lastRequest()
	if len(got.Messages) != 1 {
		t.Fatalf("messages = %#v", got.Messages)
	}
	message := got.Messages[0]
	if !strings.Contains(message.Content, "describe this") || !strings.Contains(message.Content, "[image attached: image.png]") {
		t.Fatalf("content = %q", message.Content)
	}
	if len(message.Attachments) != 1 || message.Attachments[0].MIMEType != "image/png" || len(message.Attachments[0].Data) != 3 {
		t.Fatalf("attachments = %#v", message.Attachments)
	}
}

func TestResponsesWiresImagesPerMessage(t *testing.T) {
	gw := &recordingGateway{reply: &gateway.Reply{Content: "ok", Model: "m", TodoID: "todo-1"}}
	handler := (&Server{
		cfg: &config.Config{Server: config.ServerConfig{ClientTokens: []string{"client-key"}}},
		gw:  gw,
	}).Handler()

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"m",
		"input":[
			{"type":"message","role":"user","content":[
				{"type":"input_text","text":"see this"},
				{"type":"input_image","image_url":"data:image/png;base64,AAAA"}
			]}
		]
	}`))
	req.Header.Set("Authorization", "Bearer client-key")
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	got := gw.lastRequest()
	if len(got.Messages) != 1 {
		t.Fatalf("messages = %#v", got.Messages)
	}
	message := got.Messages[0]
	if !strings.Contains(message.Content, "see this") || !strings.Contains(message.Content, "[image attached: image-1.png]") {
		t.Fatalf("content = %q", message.Content)
	}
	if len(message.Attachments) != 1 || message.Attachments[0].MIMEType != "image/png" || len(message.Attachments[0].Data) != 3 {
		t.Fatalf("attachments = %#v", message.Attachments)
	}
}

func TestChatCompletionsAcceptsMissingTypeTextPart(t *testing.T) {
	gw := &recordingGateway{reply: &gateway.Reply{Content: "ok", Model: "m", TodoID: "todo-1"}}
	handler := (&Server{
		cfg: &config.Config{Server: config.ServerConfig{ClientTokens: []string{"client-key"}}},
		gw:  gw,
	}).Handler()

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model":"m",
		"messages":[{"role":"user","content":[{"text":"bare text part"}]}]
	}`))
	req.Header.Set("Authorization", "Bearer client-key")
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if got := gw.lastRequest().Messages[0].Content; got != "bare text part" {
		t.Fatalf("content = %q", got)
	}
}

func TestNewWithNilGatewayUsesConfigModels(t *testing.T) {
	s := New(&config.Config{Models: config.ModelsConfig{
		Default: "model-default",
		Aliases: map[string]string{"model-a": "upstream-a", "model-z": "upstream-z"},
	}}, nil)
	recorder := httptest.NewRecorder()
	s.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var list openai.ModelList
	if err := json.NewDecoder(recorder.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	for _, model := range list.Data {
		seen[model.ID] = true
	}
	for _, id := range []string{"model-a", "model-default", "model-z"} {
		if !seen[id] {
			t.Fatalf("models = %#v, missing %q", list.Data, id)
		}
	}
}

func TestAnthropicHistoryConversionWiresImageAndToolResult(t *testing.T) {
	gw := &recordingGateway{reply: &gateway.Reply{Content: "ok", Model: "m", TodoID: "todo-1"}}
	handler := (&Server{
		cfg: &config.Config{Server: config.ServerConfig{ClientTokens: []string{"client-key"}}},
		gw:  gw,
	}).Handler()

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"m",
		"messages":[
			{"role":"user","content":[
				{"type":"text","text":"history context"},
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}
			]},
			{"role":"assistant","content":[
				{"type":"tool_use","id":"toolu_1","name":"read_file","input":{"path":"a.txt"}}
			]},
			{"role":"user","content":[
				{"type":"text","text":"look at this"},
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}},
				{"type":"tool_result","tool_use_id":"toolu_1","content":"file contents"}
			]}
		]
	}`))
	req.Header.Set("Authorization", "Bearer client-key")
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	messages := gw.lastRequest().Messages
	if len(messages) != 4 {
		t.Fatalf("messages = %#v", messages)
	}
	if messages[0].Role != "user" || !strings.Contains(messages[0].Content, "history context") ||
		!strings.Contains(messages[0].Content, "[image attached: image.png]") ||
		len(messages[0].Attachments) != 1 || messages[0].Attachments[0].MIMEType != "image/png" {
		t.Fatalf("history user message = %#v", messages[0])
	}
	if messages[1].Role != "assistant" || len(messages[1].ToolCalls) != 1 ||
		messages[1].ToolCalls[0].ID != "toolu_1" || messages[1].ToolCalls[0].Function.Name != "read_file" ||
		messages[1].ToolCalls[0].Function.Arguments != `{"path":"a.txt"}` {
		t.Fatalf("assistant message = %#v", messages[1])
	}
	if messages[2].Role != "user" || !strings.Contains(messages[2].Content, "look at this") ||
		!strings.Contains(messages[2].Content, "[image attached: image.png]") ||
		len(messages[2].Attachments) != 1 || len(messages[2].Attachments[0].Data) != 3 {
		t.Fatalf("current user message = %#v", messages[2])
	}
	if messages[3].Role != "tool" || messages[3].ToolCallID != "toolu_1" ||
		messages[3].Name != "read_file" || messages[3].Content != "file contents" {
		t.Fatalf("tool message = %#v", messages[3])
	}
}

func TestResponsesHistoryConversionWiresImageAndFunctionCall(t *testing.T) {
	gw := &recordingGateway{reply: &gateway.Reply{Content: "ok", Model: "m", TodoID: "todo-1"}}
	handler := (&Server{
		cfg: &config.Config{Server: config.ServerConfig{ClientTokens: []string{"client-key"}}},
		gw:  gw,
	}).Handler()

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"m",
		"input":[
			{"type":"function_call","call_id":"call_1","name":"read_file","arguments":"{\"path\":\"a.txt\"}"},
			{"type":"message","role":"user","content":[
				{"type":"input_text","text":"look at this"},
				{"type":"input_image","image_url":"data:image/png;base64,AAAA"}
			]},
			{"type":"function_call_output","call_id":"call_1","output":"file contents"}
		]
	}`))
	req.Header.Set("Authorization", "Bearer client-key")
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	messages := gw.lastRequest().Messages
	if len(messages) != 3 {
		t.Fatalf("messages = %#v", messages)
	}
	if messages[0].Role != "assistant" || len(messages[0].ToolCalls) != 1 ||
		messages[0].ToolCalls[0].ID != "call_1" || messages[0].ToolCalls[0].Function.Name != "read_file" ||
		messages[0].ToolCalls[0].Function.Arguments != `{"path":"a.txt"}` {
		t.Fatalf("assistant message = %#v", messages[0])
	}
	if messages[1].Role != "user" || !strings.Contains(messages[1].Content, "look at this") ||
		!strings.Contains(messages[1].Content, "[image attached: image-1.png]") ||
		len(messages[1].Attachments) != 1 || messages[1].Attachments[0].MIMEType != "image/png" {
		t.Fatalf("current user message = %#v", messages[1])
	}
	if messages[2].Role != "tool" || messages[2].ToolCallID != "call_1" ||
		messages[2].Name != "read_file" || messages[2].Content != "file contents" {
		t.Fatalf("tool message = %#v", messages[2])
	}
}

func TestChatCompletionsRejectsEmptyContentArray(t *testing.T) {
	handler := (&Server{
		cfg: &config.Config{Server: config.ServerConfig{ClientTokens: []string{"client-key"}}},
		gw:  &recordingGateway{reply: &gateway.Reply{Content: "ok", Model: "m", TodoID: "todo-1"}},
	}).Handler()

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model":"m",
		"messages":[{"role":"user","content":[]}]
	}`))
	req.Header.Set("Authorization", "Bearer client-key")
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("X-Request-ID") == "" {
		t.Fatal("missing X-Request-ID on error response")
	}
	if !strings.Contains(recorder.Body.String(), "content array must not be empty") {
		t.Fatalf("body = %s", recorder.Body.String())
	}

	// Compat regression: string content and content-less assistant tool-call
	// messages must keep working.
	for _, body := range []string{
		`{"model":"m","messages":[{"role":"user","content":"plain string"}]}`,
		`{"model":"m","messages":[{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]}]}`,
		`{"model":"m","messages":[{"role":"user","content":null}]}`,
	} {
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("X-API-Key", "client-key")
		handler.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusOK {
			t.Fatalf("body %s: status = %d, response = %s", body, recorder.Code, recorder.Body.String())
		}
	}
}

func TestNilGatewayEndpoints(t *testing.T) {
	s := New(&config.Config{}, nil) // no client tokens: auth is disabled
	handler := s.Handler()

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("healthz status = %d", recorder.Code)
	}
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("models status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	cases := []struct{ name, path, body string }{
		{"chat completions", "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`},
		{"messages", "/v1/messages", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`},
		{"responses", "/v1/responses", `{"model":"m","input":[{"type":"message","role":"user","content":"hi"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			handler.ServeHTTP(recorder, req)
			if recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), "gateway is not configured") {
				t.Fatalf("body = %s", recorder.Body.String())
			}
		})
	}
}
