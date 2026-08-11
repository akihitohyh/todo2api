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

func TestBuildResponseIncludesToolCallAndTodoMetadata(t *testing.T) {
	reply := &gateway.Reply{
		Model:  "resolved-model",
		TodoID: "todo-1",
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
