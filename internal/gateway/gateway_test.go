package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"todo2api/internal/config"
	"todo2api/internal/openai"
	"todo2api/internal/pool"
	"todo2api/internal/session"
	"todo2api/internal/storage"
	"todo2api/internal/upstream"
)

type recordedCall struct {
	model   string
	usage   storage.Usage
	success bool
}

type testCallRecorder struct {
	mu    sync.Mutex
	calls []recordedCall
}

func (r *testCallRecorder) RecordCall(_ context.Context, model string, usage storage.Usage, success bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, recordedCall{model: model, usage: usage, success: success})
	return nil
}

func TestCompleteToolCallContinuationAndMetadataFallback(t *testing.T) {
	mock := newMockUpstream(t)
	cfg := &config.Config{
		Upstream: config.UpstreamConfig{
			BaseURL:     mock.server.URL + "/api/v1",
			PollTimeout: 3 * time.Second,
		},
		Pool: config.PoolConfig{
			Strategy: "round_robin",
			Keys: []config.AccountKey{{
				APIKey:    "upstream-key",
				ProjectID: "project-1",
			}},
		},
		Models: config.ModelsConfig{
			Default: "openai:vendor/upstream-model",
			Aliases: map[string]string{"public-model": "openai:vendor/upstream-model"},
		},
		ToolProtocol: config.ToolProtocolConfig{
			DenyUpstreamTools: []string{"device:*", "cloud:*"},
		},
		Edge: config.EdgeConfig{
			Enabled: true,
			EdgeID:  "edge-1",
		},
	}
	p, err := pool.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	gw := New(cfg, p, session.New())
	tools := []openai.Tool{{
		Type: "function",
		Function: openai.FunctionDecl{
			Name:        "read_file",
			Description: "Read a local file",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`),
		},
	}}

	firstReq := openai.ChatRequest{
		Model:    "public-model",
		Messages: []openai.ChatMessage{{Role: "user", Content: "Read a.txt"}},
		Tools:    tools,
	}
	first, err := gw.Complete(context.Background(), firstReq)
	if err != nil {
		t.Fatal(err)
	}
	if !first.IsToolCall() || len(first.ToolCalls) != 1 {
		t.Fatalf("first reply = %#v", first)
	}
	if first.ToolCalls[0].Function.Name != "read_file" || first.TodoID != "todo-1" {
		t.Fatalf("first reply = %#v", first)
	}
	if first.Model != "public-model" {
		t.Fatalf("public response model = %q", first.Model)
	}
	if first.Usage != (TokenUsage{
		InputTokens: 852, OutputTokens: 11, CacheReadTokens: 1536, Available: true, Cost: 1.25,
	}) {
		t.Fatalf("first usage = %#v", first.Usage)
	}

	secondReq := openai.ChatRequest{
		Model: "public-model",
		Messages: []openai.ChatMessage{
			{Role: "user", Content: "Read a.txt"},
			{Role: "assistant", Content: first.Content, ToolCalls: first.ToolCalls},
			{Role: "tool", ToolCallID: first.ToolCalls[0].ID, Content: "hello from file"},
		},
		Tools: tools,
	}
	second, err := gw.Complete(context.Background(), secondReq)
	if err != nil {
		t.Fatal(err)
	}
	if second.Content != "The file says hello." || second.IsToolCall() {
		t.Fatalf("second reply = %#v", second)
	}
	if second.Usage.InputTokens != 852 || second.Usage.OutputTokens != 11 || !second.Usage.Available {
		t.Fatalf("continuation usage = %#v", second.Usage)
	}

	thirdReq := openai.ChatRequest{
		Model:    "public-model",
		Messages: []openai.ChatMessage{{Role: "user", Content: "Say it again."}},
		Metadata: map[string]string{openai.TodoIDMetadataKey: second.TodoID},
	}
	third, err := gw.Complete(context.Background(), thirdReq)
	if err != nil {
		t.Fatal(err)
	}
	if third.Content != "Still here." {
		t.Fatalf("third reply = %#v", third)
	}

	mock.mu.Lock()
	defer mock.mu.Unlock()
	if mock.createCount != 1 || len(mock.addBodies) != 2 {
		t.Fatalf("create count = %d, add count = %d", mock.createCount, len(mock.addBodies))
	}
	if got := mock.addBodies[0]["content"]; got != "[tool result for read_file]\nhello from file" {
		t.Fatalf("tool follow-up content = %#v", got)
	}
	if got := mock.addBodies[1]["content"]; got != "Say it again." {
		t.Fatalf("metadata follow-up content = %#v", got)
	}
	agent := mock.createBody["agentSettings"].(map[string]any)
	if agent["model"] != "openai:vendor/upstream-model" {
		t.Fatalf("upstream runner model = %#v", agent["model"])
	}
	if agent["systemMessageMode"] != "raw" {
		t.Fatalf("agent settings = %#v", agent)
	}
	permissions := agent["permissions"].(map[string]any)
	if deny := permissions["deny"].([]any); len(deny) != 2 || deny[0] != "device:*" || deny[1] != "cloud:*" {
		t.Fatalf("permissions deny = %#v", deny)
	}
	if agent["id"] != "agent-1" {
		t.Fatalf("agent template id was not preserved: %#v", agent)
	}
	mcpConfigs, ok := agent["mcpConfigs"].(map[string]any)
	if !ok || mcpConfigs["remote"] == nil {
		t.Fatalf("agent MCP template was not preserved: %#v", agent["mcpConfigs"])
	}
	if _, ok := mock.createBody["filteredEdgeTools"]; ok {
		t.Fatalf("client-tool create forwarded Edge tools: %#v", mock.createBody["filteredEdgeTools"])
	}
	if _, ok := mock.addBodies[0]["filteredEdgeTools"]; ok {
		t.Fatalf("client-tool follow-up forwarded Edge tools: %#v", mock.addBodies[0]["filteredEdgeTools"])
	}
	if _, ok := mock.addBodies[1]["filteredEdgeTools"].(map[string]any); !ok {
		t.Fatalf("normal follow-up did not forward Edge tools: %#v", mock.addBodies[1]["filteredEdgeTools"])
	}
	if mock.prematureFetch {
		t.Fatal("gateway fetched the final message on an intermediate READY status")
	}
}

type mockUpstream struct {
	t      *testing.T
	server *httptest.Server

	mu                sync.Mutex
	sockets           map[string]*websocket.Conn
	createCount       int
	createBody        map[string]any
	addBodies         []map[string]any
	subscriptionCount int
	assistantContent  string
	prematureFetch    bool
	streamFragments   []string
	streamDelay       time.Duration
	beforeReady       chan struct{}
	createFailures    map[string]bool
	createAttempts    map[string]int
}

func newMockUpstream(t *testing.T) *mockUpstream {
	t.Helper()
	m := &mockUpstream{
		t: t, sockets: map[string]*websocket.Conn{},
		createFailures: map[string]bool{}, createAttempts: map[string]int{},
	}
	upgrader := websocket.Upgrader{
		CheckOrigin:  func(*http.Request) bool { return true },
		Subprotocols: []string{"upstream-key"},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws/v1/frontend", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		m.mu.Lock()
		m.sockets[r.URL.Query().Get("tabId")] = conn
		m.mu.Unlock()
	})
	mux.HandleFunc("/api/v1/agents", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]any{{
			"id": "agent-1", "name": "Gateway Agent", "ownerId": "owner-1",
			"model": "template-model",
			"mcpConfigs": map[string]any{
				"remote": map[string]any{"enabled": true},
			},
			"edgesMcpConfigs": map[string]any{
				"edge-1": map[string]any{"filesystem": map[string]any{"enabled": true}},
			},
			"createdAt": 1, "updatedAt": 1,
		}})
	})
	mux.HandleFunc("/api/v1/models", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data": []map[string]any{
				{
					"id": "anthropic/claude-sonnet-4.6", "object": "model",
					"created": 123, "owned_by": "anthropic",
					"name":           "Anthropic: Claude Sonnet 4.6",
					"context_length": 1000000, "max_completion_tokens": 128000,
				},
				{
					"id": "openai/gpt-5.6-sol", "object": "model",
					"created": 456, "owned_by": "openai",
				},
			},
		})
	})
	mux.HandleFunc("/api/v1/edges/edge-1", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"id": "edge-1", "name": "Local Edge", "status": "ONLINE",
			"installedMCPs": map[string]any{
				"fs": map[string]any{
					"serverId": "filesystem",
					"tools": []map[string]any{{
						"name": "read_file", "description": "Read a file",
						"inputSchema": map[string]any{"type": "object"},
					}},
				},
			},
		})
	})
	mux.HandleFunc("/api/v1/projects/project-1/todos", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		apiKey := r.Header.Get("X-API-Key")
		m.createAttempts[apiKey]++
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body["todoId"] == "todo-1" {
			m.addBodies = append(m.addBodies, body)
		} else {
			m.createCount++
			m.createBody = body
		}
		if m.createFailures[apiKey] {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]any{
				"message": "Insufficient balance. Please add funds or subscribe.",
				"code":    "INTERNAL_SERVER_ERROR",
			})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"id": "todo-1", "projectId": "project-1", "status": "RUNNING",
		})
	})
	mux.HandleFunc("/api/v1/todos/todo-1/messages", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.assistantContent == "premature READY" {
			m.prematureFetch = true
		}
		json.NewEncoder(w).Encode(map[string]any{
			"messages": []map[string]any{
				{
					"id": "older-assistant-message", "todoId": "todo-1", "role": "assistant", "content": "old reply",
					"runMeta": []map[string]any{{
						"type":   "todo:msg_meta_ai",
						"extras": map[string]any{"inputTokens": 9999, "outputTokens": 9999, "cacheReadTokens": 9999},
					}},
				},
				{
					"id": "assistant-message", "todoId": "todo-1", "role": "assistant", "content": m.assistantContent,
					"runMeta": []map[string]any{{
						"type": "todo:msg_meta_ai", "cost": 1.25,
						"extras": map[string]any{
							"inputTokens": 852, "outputTokens": 11, "cacheReadTokens": 1536, "contextTokens": 2399,
						},
					}},
				},
			},
			"hasMore": false,
		})
	})
	mux.HandleFunc("/api/v1/todos/todo-1/subscribe", func(w http.ResponseWriter, r *http.Request) {
		tabID := r.Header.Get("X-Tab-ID")
		m.mu.Lock()
		conn := m.sockets[tabID]
		m.subscriptionCount++
		var content string
		fragments := append([]string(nil), m.streamFragments...)
		delay := m.streamDelay
		beforeReady := m.beforeReady
		if len(fragments) > 0 {
			content = strings.Join(fragments, "")
			m.assistantContent = content
		} else {
			switch m.subscriptionCount {
			case 1:
				content = `<TOOL_CALL>{"name":"read_file","arguments":{"path":"a.txt"}}</TOOL_CALL>`
				m.assistantContent = "premature READY"
			case 2:
				content = "The file says hello."
				m.assistantContent = content
			default:
				content = "Still here."
				m.assistantContent = content
			}
		}
		pendingCycle := len(fragments) == 0 && m.subscriptionCount == 1
		m.mu.Unlock()
		if conn == nil {
			http.Error(w, "socket not found", http.StatusConflict)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"id": "todo-1", "status": "RUNNING"})
		go func() {
			if len(fragments) > 0 {
				for _, fragment := range fragments {
					conn.WriteJSON(map[string]any{
						"type": "block:message",
						"payload": map[string]any{
							"todoId": "todo-1", "content": fragment,
						},
					})
					if delay > 0 {
						time.Sleep(delay)
					}
				}
				if beforeReady != nil {
					close(beforeReady)
				}
				conn.WriteJSON(map[string]any{
					"type": "todo:status",
					"payload": map[string]any{
						"todoId": "todo-1", "status": "READY",
					},
				})
				return
			}
			if pendingCycle {
				conn.WriteJSON(map[string]any{
					"type": "BLOCK_UPDATE",
					"payload": map[string]any{
						"todoId": "todo-1", "blockId": "blocked-device-tool",
						"updates": map[string]any{"status": "AWAITING_APPROVAL"},
					},
				})
				conn.WriteJSON(map[string]any{
					"type":    "todo:status",
					"payload": map[string]any{"todoId": "todo-1", "status": "READY"},
				})
				time.Sleep(50 * time.Millisecond)
				conn.WriteJSON(map[string]any{
					"type": "BLOCK_UPDATE",
					"payload": map[string]any{
						"todoId": "todo-1", "blockId": "blocked-device-tool",
						"updates": map[string]any{"status": "DENIED"},
					},
				})
				conn.WriteJSON(map[string]any{
					"type":    "todo:status",
					"payload": map[string]any{"todoId": "todo-1", "status": "RUNNING"},
				})
				m.mu.Lock()
				m.assistantContent = content
				m.mu.Unlock()
			}
			conn.WriteJSON(map[string]any{
				"type":    "block:message",
				"payload": map[string]any{"todoId": "todo-1", "content": content},
			})
			conn.WriteJSON(map[string]any{
				"type":    "todo:status",
				"payload": map[string]any{"todoId": "todo-1", "status": "READY"},
			})
		}()
	})
	m.server = httptest.NewServer(mux)
	t.Cleanup(func() {
		m.mu.Lock()
		for _, conn := range m.sockets {
			conn.Close()
		}
		m.mu.Unlock()
		m.server.Close()
	})
	return m
}

func TestNewConversationFailsOverFromInsufficientBalance(t *testing.T) {
	mock := newMockUpstream(t)
	mock.mu.Lock()
	mock.createFailures["bad-key"] = true
	mock.mu.Unlock()

	cfg := &config.Config{
		Upstream: config.UpstreamConfig{
			BaseURL: mock.server.URL + "/api/v1", PollTimeout: 3 * time.Second,
		},
		Pool: config.PoolConfig{
			Strategy: "least_busy",
			Keys: []config.AccountKey{
				{APIKey: "bad-key", ProjectID: "project-1"},
				{APIKey: "good-key", ProjectID: "project-1"},
			},
		},
		Models: config.ModelsConfig{
			Default: "openai:vendor/upstream-model",
			Aliases: map[string]string{"public-model": "openai:vendor/upstream-model"},
		},
	}
	p, err := pool.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	gw := New(cfg, p, session.New())

	reply, err := gw.Complete(context.Background(), openai.ChatRequest{
		Model: "public-model", Messages: []openai.ChatMessage{{Role: "user", Content: "hello"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if reply.TodoID != "todo-1" {
		t.Fatalf("reply = %#v", reply)
	}

	mock.mu.Lock()
	badAttempts := mock.createAttempts["bad-key"]
	goodAttempts := mock.createAttempts["good-key"]
	mock.mu.Unlock()
	if badAttempts != 1 || goodAttempts != 1 {
		t.Fatalf("create attempts: bad=%d good=%d", badAttempts, goodAttempts)
	}
	if len(cfg.Pool.Keys) != 1 || cfg.Pool.Keys[0].APIKey != "good-key" {
		t.Fatalf("runtime config keys = %#v", cfg.Pool.Keys)
	}
	if p.At(0) != nil {
		t.Fatal("removed account remained addressable")
	}
	if got := p.Pick(); got != p.At(1) {
		t.Fatalf("removed account remained selectable: got=%p want=%p", got, p.At(1))
	}
}

func TestAccountFailurePolicy(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		action accountFailureAction
		min    time.Duration
	}{
		{
			name: "balance in 500 body",
			err: &upstream.HTTPError{
				StatusCode: http.StatusInternalServerError,
				Message:    "Insufficient balance. Please add funds or subscribe.",
			},
			action: accountFailureRemove,
		},
		{
			name: "rate limit", err: &upstream.HTTPError{StatusCode: http.StatusTooManyRequests},
			action: accountFailureCooldown, min: time.Minute,
		},
		{
			name: "generic upstream failure",
			err:  &upstream.HTTPError{StatusCode: http.StatusInternalServerError, Message: "internal error"},
		},
		{name: "network error", err: errors.New("connection reset")},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			action, duration := accountFailurePolicy(test.err)
			if action != test.action || duration < test.min {
				t.Fatalf("action = %d, cooldown = %s", action, duration)
			}
		})
	}
}

func TestStreamEmitsTextBeforeReady(t *testing.T) {
	mock := newMockUpstream(t)
	ready := make(chan struct{})
	mock.mu.Lock()
	mock.streamFragments = []string{"hello", " world"}
	mock.streamDelay = 40 * time.Millisecond
	mock.beforeReady = ready
	mock.mu.Unlock()

	gw := newTestGateway(t, mock)
	var events []StreamEvent
	firstDeltaBeforeReady := false
	reply, err := gw.Stream(context.Background(), openai.ChatRequest{
		Model: "public-model", Messages: []openai.ChatMessage{{Role: "user", Content: "hello"}},
	}, func(event StreamEvent) error {
		events = append(events, event)
		if event.Type == StreamTextDelta && !firstDeltaBeforeReady {
			select {
			case <-ready:
				t.Fatal("first text delta arrived after READY")
			default:
				firstDeltaBeforeReady = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !firstDeltaBeforeReady || reply.Content != "hello world" {
		t.Fatalf("reply=%#v events=%#v", reply, events)
	}
	if len(events) != 3 || events[0].Type != StreamStart || events[1].Delta != "hello" || events[2].Delta != " world" {
		t.Fatalf("events = %#v", events)
	}
}

func TestCallRecorderCountsEachGatewayInvocationOnce(t *testing.T) {
	mock := newMockUpstream(t)
	mock.mu.Lock()
	mock.streamFragments = []string{"hello"}
	mock.mu.Unlock()

	recorder := &testCallRecorder{}
	gw := newTestGateway(t, mock)
	gw.recorder = recorder
	if _, err := gw.Complete(context.Background(), openai.ChatRequest{
		Model: "public-model", Messages: []openai.ChatMessage{{Role: "user", Content: "first"}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := gw.Stream(context.Background(), openai.ChatRequest{
		Model: "public-model", Messages: []openai.ChatMessage{{Role: "user", Content: "second"}},
	}, func(StreamEvent) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := gw.Complete(context.Background(), openai.ChatRequest{
		Model: "public-model", Metadata: map[string]string{openai.TodoIDMetadataKey: "missing"},
	}); err == nil {
		t.Fatal("unknown explicit todo unexpectedly succeeded")
	}

	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if len(recorder.calls) != 3 {
		t.Fatalf("recorded calls=%#v", recorder.calls)
	}
	for i, call := range recorder.calls[:2] {
		if !call.success || call.model != "public-model" || call.usage.InputTokens != 852 || call.usage.OutputTokens != 11 || call.usage.Cost != 1.25 {
			t.Fatalf("success call %d=%#v", i, call)
		}
	}
	if recorder.calls[2].success {
		t.Fatalf("failed call recorded as success: %#v", recorder.calls[2])
	}
}

func TestStreamSuppressesSplitToolProtocol(t *testing.T) {
	mock := newMockUpstream(t)
	mock.mu.Lock()
	mock.streamFragments = []string{
		"<TO", `OL_CALL>{"name":"read_file","arguments":{"path":"a.txt"}}`, "</TOOL_CALL>",
	}
	mock.mu.Unlock()

	gw := newTestGateway(t, mock)
	var text strings.Builder
	reply, err := gw.Stream(context.Background(), openai.ChatRequest{
		Model: "public-model", Messages: []openai.ChatMessage{{Role: "user", Content: "read it"}},
		Tools: []openai.Tool{{
			Type: "function", Function: openai.FunctionDecl{Name: "read_file", Parameters: json.RawMessage(`{"type":"object"}`)},
		}},
	}, func(event StreamEvent) error {
		if event.Type == StreamTextDelta {
			text.WriteString(event.Delta)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if text.Len() != 0 || !reply.IsToolCall() || reply.ToolCalls[0].Function.Name != "read_file" {
		t.Fatalf("streamed=%q reply=%#v", text.String(), reply)
	}
}

func TestStreamStopsOnEmitterErrorAndCancellation(t *testing.T) {
	t.Run("emitter error", func(t *testing.T) {
		mock := newMockUpstream(t)
		mock.mu.Lock()
		mock.streamFragments = []string{"hello", " world"}
		mock.streamDelay = 20 * time.Millisecond
		mock.mu.Unlock()
		gw := newTestGateway(t, mock)
		writeErr := errors.New("client write failed")
		_, err := gw.Stream(context.Background(), openai.ChatRequest{
			Model: "public-model", Messages: []openai.ChatMessage{{Role: "user", Content: "hello"}},
		}, func(event StreamEvent) error {
			if event.Type == StreamTextDelta {
				return writeErr
			}
			return nil
		})
		if !errors.Is(err, writeErr) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("context cancellation", func(t *testing.T) {
		mock := newMockUpstream(t)
		mock.mu.Lock()
		mock.streamFragments = []string{"hello"}
		mock.streamDelay = 100 * time.Millisecond
		mock.mu.Unlock()
		gw := newTestGateway(t, mock)
		ctx, cancel := context.WithCancel(context.Background())
		_, err := gw.Stream(ctx, openai.ChatRequest{
			Model: "public-model", Messages: []openai.ChatMessage{{Role: "user", Content: "hello"}},
		}, func(event StreamEvent) error {
			if event.Type == StreamStart {
				cancel()
			}
			return nil
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v", err)
		}
	})
}

func newTestGateway(t *testing.T, mock *mockUpstream) *Gateway {
	t.Helper()
	cfg := &config.Config{
		Upstream: config.UpstreamConfig{
			BaseURL: mock.server.URL + "/api/v1", PollTimeout: 3 * time.Second,
		},
		Pool: config.PoolConfig{
			Strategy: "round_robin",
			Keys: []config.AccountKey{{
				APIKey: "upstream-key", ProjectID: "project-1",
			}},
		},
		Models: config.ModelsConfig{
			Default: "openai:vendor/upstream-model",
			Aliases: map[string]string{"public-model": "openai:vendor/upstream-model"},
		},
		ToolProtocol: config.ToolProtocolConfig{
			DenyUpstreamTools: []string{"device:*", "cloud:*"},
		},
	}
	p, err := pool.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return New(cfg, p, session.New())
}

func TestConversationKeyIncludesToolCalls(t *testing.T) {
	base := []openai.ChatMessage{{Role: "user", Content: "hello"}}
	withRead := append(base, openai.ChatMessage{
		Role: "assistant",
		ToolCalls: []openai.ToolCall{{
			ID: "call-1", Type: "function",
			Function: openai.FunctionCall{Name: "read", Arguments: `{}`},
		}},
	})
	withWrite := append(base, openai.ChatMessage{
		Role: "assistant",
		ToolCalls: []openai.ToolCall{{
			ID: "call-2", Type: "function",
			Function: openai.FunctionCall{Name: "write", Arguments: `{}`},
		}},
	})
	if strings.EqualFold(conversationKey("", withRead), conversationKey("", withWrite)) {
		t.Fatal("conversation key ignored tool calls")
	}
}

func TestConversationKeyIncludesSystemPrompt(t *testing.T) {
	messages := []openai.ChatMessage{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi"},
	}
	if strings.EqualFold(conversationKey("be concise", messages), conversationKey("be detailed", messages)) {
		t.Fatal("conversation key ignored the system prompt")
	}
}

func TestTokenUsageAggregatesOnlyAIMetadata(t *testing.T) {
	usage := tokenUsage([]upstream.RunMeta{
		{
			Type: "todo:msg_meta_ai",
			Extras: upstream.RunMetaExtras{
				InputTokens: 800, OutputTokens: 10, CacheReadTokens: 1500,
				CacheWriteTokens: 60, ContextTokens: 999999,
			},
		},
		{
			Type: "todo:msg_meta_tool",
			Extras: upstream.RunMetaExtras{
				InputTokens: 5000, OutputTokens: 5000, CacheReadTokens: 5000,
			},
		},
		{
			Type: "todo:msg_meta_ai",
			Extras: upstream.RunMetaExtras{
				InputTokens: 52, OutputTokens: 1, CacheReadTokens: 36, CacheWriteTokens: 4,
			},
		},
	})
	want := TokenUsage{
		InputTokens: 852, OutputTokens: 11, CacheReadTokens: 1536,
		CacheWriteTokens: 64, Available: true,
	}
	if usage != want {
		t.Fatalf("usage = %#v, want %#v", usage, want)
	}
	if missing := tokenUsage(nil); missing.Available {
		t.Fatalf("missing metadata reported available: %#v", missing)
	}
}

func TestEnrichToolResultNamesFromSession(t *testing.T) {
	store := session.New()
	store.PutToolNames("todo-1", map[string]string{"call-1": "read_file"})
	messages := []openai.ChatMessage{{
		Role: "tool", ToolCallID: "call-1", Name: "tool", Content: "result",
	}}

	got := enrichToolResultNames(messages, "todo-1", store)
	if got[0].Name != "read_file" {
		t.Fatalf("tool name = %q, want read_file", got[0].Name)
	}
	if messages[0].Name != "tool" {
		t.Fatal("enrichment mutated the caller's message slice")
	}
}

func TestCompleteRejectsUnknownExplicitTodo(t *testing.T) {
	gw := &Gateway{cfg: &config.Config{}, sess: session.New()}
	_, err := gw.Complete(context.Background(), openai.ChatRequest{
		Model:    "model",
		Messages: []openai.ChatMessage{{Role: "user", Content: "hello"}},
		Metadata: map[string]string{openai.TodoIDMetadataKey: "missing-todo"},
	})
	if err == nil || !strings.Contains(err.Error(), "session for todo missing-todo is unavailable") {
		t.Fatalf("error = %v", err)
	}
}

func TestGatewayDiscoveredModelsAndAliases(t *testing.T) {
	mock := newMockUpstream(t)
	gw := newTestGateway(t, mock)

	models := gw.Models()
	byID := make(map[string]openai.Model, len(models))
	for _, model := range models {
		byID[model.ID] = model
	}
	claude, ok := byID["claude-sonnet-4.6"]
	if !ok || claude.ContextLength != 1000000 || claude.MaxCompletionTokens != 128000 || claude.OwnedBy != "anthropic" {
		t.Fatalf("Claude model = %#v", claude)
	}
	if _, ok := byID["public-model"]; !ok {
		t.Fatalf("configured alias missing from %#v", models)
	}
	if _, ok := byID["gpt-5.6-sol"]; !ok {
		t.Fatalf("short upstream ID missing from %#v", models)
	}
	if _, ok := byID["openai/gpt-5.6-sol"]; ok {
		t.Fatalf("full upstream ID was advertised in %#v", models)
	}
	if got := gw.resolveModel("claude-sonnet-4.6"); got != "anthropic:anthropic/claude-sonnet-4.6" {
		t.Fatalf("resolved short Claude model = %q", got)
	}
	if got := gw.resolveModel("anthropic/claude-sonnet-4.6"); got != "anthropic:anthropic/claude-sonnet-4.6" {
		t.Fatalf("resolved Claude model = %q", got)
	}
	if got := gw.resolveModel("public-model"); got != "openai:vendor/upstream-model" {
		t.Fatalf("resolved configured alias = %q", got)
	}
}

func TestConfiguredAliasOverridesImplicitDefaultShortName(t *testing.T) {
	gw := &Gateway{cfg: &config.Config{Models: config.ModelsConfig{
		Default: "openai:openai/gpt-5.6-sol",
		Aliases: map[string]string{
			"gpt-5.6-sol": "anthropic:anthropic/claude-sonnet-4.6",
		},
	}}}

	if got := gw.resolveModel("gpt-5.6-sol"); got != "anthropic:anthropic/claude-sonnet-4.6" {
		t.Fatalf("resolved explicit alias = %q", got)
	}
	if got := gw.publicModelID("openai:openai/gpt-5.6-sol", "openai:openai/gpt-5.6-sol"); got != "gpt-5.6-sol" {
		t.Fatalf("public compatibility model = %q", got)
	}
}
