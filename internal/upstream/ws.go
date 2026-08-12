package upstream

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Event is one frontend WebSocket envelope from todofor.ai.
type Event struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// Subscription is a connected frontend WebSocket. It is intentionally opened
// before creating or resuming a todo so early run events cannot be missed.
type Subscription struct {
	client    *Client
	tabID     string
	conn      *websocket.Conn
	closed    chan struct{}
	closeOnce sync.Once
}

// PrepareSubscription opens the shared frontend WebSocket. The API key is sent
// as the WebSocket subprotocol, matching the official todofor.ai CLI.
func (c *Client) PrepareSubscription(ctx context.Context) (*Subscription, error) {
	tabID, err := newTabID()
	if err != nil {
		return nil, err
	}
	wsEndpoint, err := frontendWSURL(c.baseURL, tabID)
	if err != nil {
		return nil, err
	}

	dialer := websocket.Dialer{
		HandshakeTimeout: 15 * time.Second,
		Subprotocols:     []string{c.apiKey},
	}
	conn, resp, err := dialer.DialContext(ctx, wsEndpoint, nil)
	if err != nil {
		if resp != nil {
			data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			return nil, newHTTPError(http.MethodGet, wsEndpoint, resp.StatusCode, data)
		}
		return nil, fmt.Errorf("frontend ws dial %s: %w", wsEndpoint, err)
	}
	conn.SetReadLimit(5 * 1024 * 1024)

	sub := &Subscription{
		client: c,
		tabID:  tabID,
		conn:   conn,
		closed: make(chan struct{}),
	}
	go func() {
		select {
		case <-ctx.Done():
			sub.Close()
		case <-sub.closed:
		}
	}()
	return sub, nil
}

// Subscribe is a convenience method for callers that do not need to connect
// before starting the upstream run.
func (c *Client) Subscribe(ctx context.Context, todoID string, out chan<- Event) error {
	sub, err := c.PrepareSubscription(ctx)
	if err != nil {
		return err
	}
	defer sub.Close()
	return sub.Subscribe(ctx, todoID, out)
}

// Subscribe binds this frontend tab to todoID over HTTP, then forwards its
// WebSocket envelopes until the context is canceled or the socket closes.
func (s *Subscription) Subscribe(ctx context.Context, todoID string, out chan<- Event) error {
	if err := s.client.subscribeTodo(ctx, todoID, s.tabID); err != nil {
		return err
	}
	for {
		_, data, err := s.conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("frontend ws read: %w", err)
		}
		var ev Event
		if err := json.Unmarshal(data, &ev); err != nil || ev.Type == "" {
			continue
		}
		if err := sendEvent(ctx, out, ev); err != nil {
			return err
		}
	}
}

func (s *Subscription) Close() {
	s.closeOnce.Do(func() {
		close(s.closed)
		s.conn.Close()
	})
}

type subscribeTodoReq struct {
	TodoID string `json:"todoId"`
}

func (c *Client) subscribeTodo(ctx context.Context, todoID, tabID string) error {
	body, err := json.Marshal(subscribeTodoReq{TodoID: todoID})
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/todos/%s/subscribe", url.PathEscape(todoID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("X-API-Key", c.apiKey)
	req.Header.Set("X-Tab-ID", tabID)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("subscribe todo: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusMultipleChoices {
		data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if err != nil {
			return err
		}
		return newHTTPError(http.MethodPost, path, resp.StatusCode, data)
	}
	_, err = io.Copy(io.Discard, resp.Body)
	return err
}

func frontendWSURL(baseURL, tabID string) (string, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("parse upstream base URL: %w", err)
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	default:
		return "", fmt.Errorf("unsupported upstream URL scheme %q", u.Scheme)
	}
	u.Path = strings.TrimSuffix(strings.TrimRight(u.Path, "/"), "/api/v1") + "/ws/v1/frontend"
	u.RawPath = ""
	q := u.Query()
	q.Set("tabId", tabID)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func newTabID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", fmt.Errorf("generate frontend tab id: %w", err)
	}
	id[6] = (id[6] & 0x0f) | 0x40
	id[8] = (id[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", id[0:4], id[4:6], id[6:8], id[8:10], id[10:16]), nil
}

func sendEvent(ctx context.Context, out chan<- Event, ev Event) error {
	select {
	case out <- ev:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
