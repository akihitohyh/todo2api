package transport

import (
	"encoding/json"
	"fmt"
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
