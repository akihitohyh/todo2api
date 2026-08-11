package upstream

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestFrontendWSURL(t *testing.T) {
	got, err := frontendWSURL("https://api.todofor.ai/api/v1", "tab-1")
	if err != nil {
		t.Fatal(err)
	}
	if got != "wss://api.todofor.ai/ws/v1/frontend?tabId=tab-1" {
		t.Fatalf("URL = %q", got)
	}
}

func TestFrontendSubscriptionProtocol(t *testing.T) {
	const apiKey = "test-api-key"
	var (
		mu          sync.Mutex
		connections = map[string]*websocket.Conn{}
	)
	upgrader := websocket.Upgrader{
		CheckOrigin:  func(*http.Request) bool { return true },
		Subprotocols: []string{apiKey},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws/v1/frontend", func(w http.ResponseWriter, r *http.Request) {
		tabID := r.URL.Query().Get("tabId")
		if tabID == "" {
			t.Error("missing WebSocket tabId")
		}
		if !strings.Contains(r.Header.Get("Sec-WebSocket-Protocol"), apiKey) {
			t.Errorf("WebSocket subprotocol = %q", r.Header.Get("Sec-WebSocket-Protocol"))
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		connections[tabID] = conn
		mu.Unlock()
	})
	mux.HandleFunc("/api/v1/todos/todo-1/subscribe", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		if r.Header.Get("X-API-Key") != apiKey {
			t.Errorf("X-API-Key = %q", r.Header.Get("X-API-Key"))
		}
		tabID := r.Header.Get("X-Tab-ID")
		var body subscribeTodoReq
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.TodoID != "todo-1" {
			t.Errorf("body todoId = %q", body.TodoID)
		}
		mu.Lock()
		conn := connections[tabID]
		mu.Unlock()
		if conn == nil {
			t.Errorf("no WebSocket for tab %q", tabID)
			http.Error(w, "missing socket", http.StatusConflict)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": "todo-1", "status": "RUNNING"})
		go func() {
			conn.WriteJSON(map[string]any{
				"type":    "block:message",
				"payload": map[string]any{"todoId": "todo-1", "content": "hello"},
			})
			conn.WriteJSON(map[string]any{
				"type":    "todo:status",
				"payload": map[string]any{"todoId": "todo-1", "status": "READY"},
			})
		}()
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	client := New(server.URL+"/api/v1", apiKey)
	sub, err := client.PrepareSubscription(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()

	events := make(chan Event, 2)
	errCh := make(chan error, 1)
	go func() { errCh <- sub.Subscribe(ctx, "todo-1", events) }()

	for _, want := range []string{"block:message", "todo:status"} {
		select {
		case ev := <-events:
			if ev.Type != want {
				t.Fatalf("event type = %q, want %q", ev.Type, want)
			}
		case err := <-errCh:
			t.Fatalf("subscription ended early: %v", err)
		case <-ctx.Done():
			t.Fatal("timed out waiting for event")
		}
	}
}
