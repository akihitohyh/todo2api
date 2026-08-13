package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHTTPErrorPreservesUpstreamDetails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{
			"message": "Insufficient balance. Please add funds or subscribe.",
			"code":    "INTERNAL_SERVER_ERROR",
		})
	}))
	defer server.Close()

	_, err := New(server.URL, "key").CreateTodo(
		context.Background(), "project-1", "hello", AgentSettings{},
	)
	var upstreamErr *HTTPError
	if !errors.As(err, &upstreamErr) {
		t.Fatalf("error = %T %v, want *HTTPError", err, err)
	}
	if upstreamErr.StatusCode != http.StatusInternalServerError ||
		upstreamErr.Code != "INTERNAL_SERVER_ERROR" ||
		!strings.Contains(upstreamErr.Message, "Insufficient balance") {
		t.Fatalf("HTTP error = %#v", upstreamErr)
	}
}

func TestHTTPErrorPreservesNestedDetails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `{"error":{"message":"Upstream HTTP/2 stream failed","code":"upstream_http2_stream_error"}}`)
	}))
	defer server.Close()

	_, err := New(server.URL, "key").CreateTodo(context.Background(), "project-1", "hello", AgentSettings{})
	var upstreamErr *HTTPError
	if !errors.As(err, &upstreamErr) {
		t.Fatalf("error = %T %v, want *HTTPError", err, err)
	}
	if upstreamErr.Code != "upstream_http2_stream_error" || upstreamErr.Message != "Upstream HTTP/2 stream failed" {
		t.Fatalf("nested HTTP error = %#v", upstreamErr)
	}
}

func TestCreateAndAddMessageWireFormat(t *testing.T) {
	var createBody, addBody map[string]any
	var todoPostCount int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "upstream-key" {
			t.Errorf("X-API-Key = %q", r.Header.Get("X-API-Key"))
		}
		switch r.URL.Path {
		case "/api/v1/projects":
			json.NewEncoder(w).Encode([]map[string]any{{
				"project":  map[string]any{"id": "project-1", "name": "Default Project"},
				"settings": map[string]any{}, "doneCount": 0,
			}})
		case "/api/v1/projects/project-1/todos":
			todoPostCount++
			body := &createBody
			if todoPostCount == 2 {
				body = &addBody
			}
			if err := json.NewDecoder(r.Body).Decode(body); err != nil {
				t.Error(err)
			}
			json.NewEncoder(w).Encode(Todo{ID: "todo-1", ProjectID: "project-1", Status: "RUNNING"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := New(server.URL+"/api/v1", "upstream-key")
	projectID, err := client.FirstProject(context.Background())
	if err != nil || projectID != "project-1" {
		t.Fatalf("project id = %q, err = %v", projectID, err)
	}
	agent := AgentSettings{
		ID:                "agent-1",
		Name:              "Gateway Agent",
		OwnerID:           "owner-1",
		Model:             "model-1",
		SystemMessage:     "strict tool prompt",
		SystemMessageMode: "raw",
		MCPConfigs: map[string]any{
			"remote": map[string]any{"enabled": true},
		},
		EdgesMCPConfigs: map[string]map[string]any{},
		Permissions: &ToolPermissions{
			Allow: []string{},
			Deny:  []string{"device:*", "cloud:*"},
		},
	}
	filtered := FilteredEdgeTools{
		"filesystem": {{Name: "read_file", Description: "Read", InputSchema: map[string]any{"type": "object"}}},
	}

	if _, err := client.CreateTodo(context.Background(), "project-1", "hello", agent, filtered); err != nil {
		t.Fatal(err)
	}
	if _, err := client.AddMessage(context.Background(), "project-1", "todo-1", "result", agent, filtered); err != nil {
		t.Fatal(err)
	}

	assertStringField(t, createBody, "projectId", "project-1")
	assertStringField(t, createBody, "content", "hello")
	if _, ok := createBody["todoId"]; ok {
		t.Fatalf("create todoId = %#v, want omitted", createBody["todoId"])
	}
	assertStringField(t, addBody, "todoId", "todo-1")
	assertStringField(t, addBody, "projectId", "project-1")
	for name, body := range map[string]map[string]any{"create": createBody, "add": addBody} {
		agentBody, ok := body["agentSettings"].(map[string]any)
		if !ok {
			t.Fatalf("%s agentSettings = %#v", name, body["agentSettings"])
		}
		assertStringField(t, agentBody, "systemMessageMode", "raw")
		assertStringField(t, agentBody, "id", "agent-1")
		if configs, ok := agentBody["mcpConfigs"].(map[string]any); !ok || configs["remote"] == nil {
			t.Fatalf("%s mcpConfigs = %#v", name, agentBody["mcpConfigs"])
		}
		permissions := agentBody["permissions"].(map[string]any)
		deny := permissions["deny"].([]any)
		if len(deny) != 2 || deny[0] != "device:*" || deny[1] != "cloud:*" {
			t.Fatalf("%s deny = %#v", name, deny)
		}
		if _, ok := body["filteredEdgeTools"].(map[string]any); !ok {
			t.Fatalf("%s filteredEdgeTools = %#v", name, body["filteredEdgeTools"])
		}
	}
}

func TestRegisterAttachmentAndAttachFrame(t *testing.T) {
	var uploadData []byte
	var todoBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/resources/register":
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Fatal(err)
			}
			file, _, err := r.FormFile("file")
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			uploadData, _ = io.ReadAll(file)
			json.NewEncoder(w).Encode(map[string]any{"attachmentId": "att-1", "uri": "todoforai:todos/todo-1/att-1", "originalName": "cat.png", "mimeType": "image/png", "fileSize": 3})
		case "/api/v1/projects/project-1/todos":
			if err := json.NewDecoder(r.Body).Decode(&todoBody); err != nil {
				t.Fatal(err)
			}
			json.NewEncoder(w).Encode(Todo{ID: "todo-1", ProjectID: "project-1", Status: "RUNNING"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := New(server.URL+"/api/v1", "upstream-key")
	frame, err := client.RegisterAttachment(context.Background(), AttachmentUpload{Name: "cat.png", MIMEType: "image/png", Data: []byte{0, 0, 0}}, "todo-1")
	if err != nil {
		t.Fatal(err)
	}
	if string(uploadData) != "\x00\x00\x00" || frame.ID != "att-1" {
		t.Fatalf("upload=%v frame=%#v", uploadData, frame)
	}
	if _, err := client.CreateTodoWithAttachments(context.Background(), "project-1", "[image attached]", AgentSettings{}, []AttachmentFrame{frame}); err != nil {
		t.Fatal(err)
	}
	attachments, ok := todoBody["attachments"].([]any)
	if !ok || len(attachments) != 1 {
		t.Fatalf("todo attachments=%#v", todoBody["attachments"])
	}
}

func TestMessagesDecodesRunMeta(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/todos/todo-1/messages" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{
			"messages":[{
				"id":"message-1","role":"assistant","content":"","blocks":[{"type":"text","content":"hello"}],
				"runMeta":[{
					"type":"todo:msg_meta_ai",
					"extras":{"model":"openai:openai/gpt-5.6-sol","inputTokens":852,"outputTokens":11,"cacheReadTokens":1536,"cacheWriteTokens":64,"contextTokens":2463}
				}]
			}],
			"hasMore":false
		}`)
	}))
	defer server.Close()

	messages, err := New(server.URL+"/api/v1", "upstream-key").Messages(context.Background(), "todo-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || len(messages[0].RunMeta) != 1 {
		t.Fatalf("messages = %#v", messages)
	}
	extras := messages[0].RunMeta[0].Extras
	if extras.InputTokens != 852 || extras.OutputTokens != 11 || extras.CacheReadTokens != 1536 || extras.CacheWriteTokens != 64 || extras.ContextTokens != 2463 {
		t.Fatalf("runMeta extras = %#v", extras)
	}
}

func TestEdgeDiscoveryAndFiltering(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/edges":
			json.NewEncoder(w).Encode([]Edge{
				{ID: "offline", Status: "OFFLINE"},
				{ID: "edge-1", Status: "ONLINE"},
			})
		case "/api/v1/edges/edge-1":
			json.NewEncoder(w).Encode(Edge{
				ID: "edge-1",
				InstalledMCPs: map[string]InstalledMCP{
					"fs": {
						ServerID: "filesystem",
						Tools: []MCPToolSkeleton{
							{Name: "read_file"},
							{Name: "write_file"},
						},
					},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := New(server.URL+"/api/v1", "key")
	edgeID, err := client.FirstOnlineEdge(context.Background())
	if err != nil || edgeID != "edge-1" {
		t.Fatalf("edge = %q, err = %v", edgeID, err)
	}
	tools, err := client.EdgeTools(context.Background(), edgeID, []string{"read_*"})
	if err != nil {
		t.Fatal(err)
	}
	if len(tools["filesystem"]) != 1 || tools["filesystem"][0].Name != "read_file" {
		t.Fatalf("filtered tools = %#v", tools)
	}
}

func TestModelsAndRunnerModelID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/models" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("X-API-Key") != "upstream-key" {
			t.Errorf("X-API-Key = %q", r.Header.Get("X-API-Key"))
		}
		json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data": []map[string]any{{
				"id": "anthropic/claude-sonnet-4.6", "object": "model",
				"created": 123, "owned_by": "anthropic",
				"name":           "Anthropic: Claude Sonnet 4.6",
				"context_length": 1000000, "max_completion_tokens": 128000,
			}},
		})
	}))
	defer server.Close()

	models, err := New(server.URL+"/api/v1", "upstream-key").Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].ID != "anthropic/claude-sonnet-4.6" || models[0].ContextLength != 1000000 {
		t.Fatalf("models = %#v", models)
	}
	for input, want := range map[string]string{
		"anthropic/claude-sonnet-4.6": "anthropic:anthropic/claude-sonnet-4.6",
		"openai/gpt-5.6-sol":          "openai:openai/gpt-5.6-sol",
		"qwen3.5:397b":                "qwen3.5:397b",
		"openai:openai/gpt-5.6-sol":   "openai:openai/gpt-5.6-sol",
	} {
		if got := RunnerModelID(input); got != want {
			t.Fatalf("RunnerModelID(%q) = %q, want %q", input, got, want)
		}
	}
}

func assertStringField(t *testing.T, object map[string]any, field, want string) {
	t.Helper()
	if got := object[field]; got != want {
		t.Fatalf("%s = %#v, want %q", field, got, want)
	}
}
