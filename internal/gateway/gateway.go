package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"todo2api/internal/config"
	"todo2api/internal/openai"
	"todo2api/internal/pool"
	"todo2api/internal/session"
	"todo2api/internal/upstream"
)

type Gateway struct {
	cfg  *config.Config
	pool *pool.Pool
	sess *session.Store
}

func New(cfg *config.Config, p *pool.Pool, s *session.Store) *Gateway {
	return &Gateway{cfg: cfg, pool: p, sess: s}
}

// Reply carries either final text or a set of tool calls for the client to run.
type Reply struct {
	Content   string
	ToolCalls []openai.ToolCall
	Model     string
	TodoID    string
}

func (r *Reply) IsToolCall() bool { return len(r.ToolCalls) > 0 }

// Models returns short discovered IDs plus configured public aliases.
// Configured aliases take precedence when IDs overlap.
func (g *Gateway) Models() []openai.Model {
	models := make(map[string]openai.Model)
	if g.pool != nil {
		for _, model := range g.pool.Models() {
			models[model.ID] = openAIModel(model.ID, model)
		}
	}

	for alias, target := range g.cfg.Models.Aliases {
		model := openai.Model{ID: alias, Object: "model", OwnedBy: "todofor.ai"}
		if g.pool != nil {
			if info, ok := g.pool.Model(target); ok {
				model = openAIModel(alias, info)
			}
		}
		models[alias] = model
	}

	if defaultModel := g.cfg.Models.Default; defaultModel != "" {
		publicID := ""
		if g.pool != nil {
			publicID, _ = g.pool.PublicModelID(defaultModel)
		}
		if publicID == "" {
			publicID = configuredModelShortID(defaultModel)
		}
		if _, exists := models[publicID]; !exists {
			models[publicID] = openai.Model{ID: publicID, Object: "model", OwnedBy: "todofor.ai"}
		}
	}

	result := make([]openai.Model, 0, len(models))
	for _, model := range models {
		result = append(result, model)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func openAIModel(id string, model upstream.ModelInfo) openai.Model {
	ownedBy := model.OwnedBy
	if ownedBy == "" {
		ownedBy = "todofor.ai"
	}
	return openai.Model{
		ID: id, Object: "model", Created: model.Created, OwnedBy: ownedBy,
		Name: model.Name, ContextLength: model.ContextLength,
		MaxCompletionTokens: model.MaxCompletionTokens,
	}
}

type StreamEventType string

const (
	StreamStart     StreamEventType = "start"
	StreamTextDelta StreamEventType = "text_delta"
)

// StreamEvent is emitted synchronously while an upstream turn is running.
// Returning an error from the emitter aborts the turn.
type StreamEvent struct {
	Type   StreamEventType
	Model  string
	TodoID string
	Delta  string
}

// Complete runs one OpenAI turn, resuming an existing upstream todo when the
// history hash or todo2api metadata identifies one.
func (g *Gateway) Complete(ctx context.Context, req openai.ChatRequest) (*Reply, error) {
	return g.complete(ctx, req, nil)
}

// Stream runs one OpenAI turn and emits assistant text as upstream WebSocket
// fragments arrive. The returned Reply contains the authoritative final text
// and any parsed client-side tool calls.
func (g *Gateway) Stream(ctx context.Context, req openai.ChatRequest, emit func(StreamEvent) error) (*Reply, error) {
	if emit == nil {
		return nil, fmt.Errorf("stream emitter must not be nil")
	}
	return g.complete(ctx, req, emit)
}

func (g *Gateway) complete(ctx context.Context, req openai.ChatRequest, emit func(StreamEvent) error) (*Reply, error) {
	runnerModel := g.resolveModel(req.Model)
	publicModel := g.publicModelID(req.Model, runnerModel)
	entry, resuming := g.sessionEntry(req)
	explicitTodoID := strings.TrimSpace(req.Metadata[openai.TodoIDMetadataKey])
	if explicitTodoID != "" && !resuming {
		return nil, fmt.Errorf("session for todo %s is unavailable", explicitTodoID)
	}
	var acc *pool.Account
	if resuming {
		acc = g.pool.At(entry.Account)
		if acc == nil {
			return nil, fmt.Errorf("session references unavailable account %d", entry.Account)
		}
	} else {
		acc = g.pool.Pick()
		if acc == nil {
			return nil, fmt.Errorf("account pool is empty")
		}
	}
	if resuming {
		req.Messages = enrichToolResultNames(req.Messages, entry.TodoID, g.sess)
	}

	acc.Acquire()
	defer acc.Release()
	agent := g.agentSettings(acc.Agent, runnerModel, req.Tools)
	filteredTools := acc.EdgeTools
	if len(req.Tools) > 0 {
		filteredTools = nil
	}

	runCtx, cancel := context.WithTimeout(ctx, g.cfg.Upstream.PollTimeout)
	defer cancel()
	sub, err := acc.Client.PrepareSubscription(runCtx)
	if err != nil {
		return nil, err
	}
	defer sub.Close()

	todoID := entry.TodoID
	if !resuming {
		content := openai.FlattenTurn(req.Messages)
		todo, err := acc.Client.CreateTodo(runCtx, acc.ProjectID, content, agent, filteredTools)
		if err != nil {
			return nil, err
		}
		todoID = todo.ID
	} else {
		content := followUpBody(req.Messages)
		if content == "" {
			return nil, fmt.Errorf("resumed request has no new user or tool-result messages")
		}
		if _, err := acc.Client.AddMessage(runCtx, acc.ProjectID, todoID, content, agent, filteredTools); err != nil {
			return nil, err
		}
	}
	if todoID == "" {
		return nil, fmt.Errorf("upstream returned an empty todo id")
	}
	if emit != nil {
		if err := emit(StreamEvent{Type: StreamStart, Model: publicModel, TodoID: todoID}); err != nil {
			return nil, fmt.Errorf("emit stream start: %w", err)
		}
	}

	var emitText func(string) error
	if emit != nil {
		emitText = func(delta string) error {
			return emit(StreamEvent{
				Type: StreamTextDelta, Model: publicModel, TodoID: todoID, Delta: delta,
			})
		}
	}
	content, err := g.waitAssistant(runCtx, sub, acc.Client, todoID, emitText)
	if err != nil {
		return nil, err
	}
	text, calls := openai.ParseToolCalls(content)

	assistant := openai.ChatMessage{Role: "assistant", Content: content}
	if len(calls) > 0 {
		assistant.Content = text
		assistant.ToolCalls = calls
	}
	accountIndex := g.pool.IndexOf(acc)
	if accountIndex < 0 {
		return nil, fmt.Errorf("selected account is not in the pool")
	}
	g.sess.Put(
		conversationKeyWith(req.Messages, assistant),
		session.Entry{TodoID: todoID, Account: accountIndex},
	)
	if len(calls) > 0 {
		names := make(map[string]string, len(calls))
		for _, call := range calls {
			names[call.ID] = call.Function.Name
		}
		g.sess.PutToolNames(todoID, names)
	}

	if len(calls) > 0 {
		return &Reply{Content: text, ToolCalls: calls, Model: publicModel, TodoID: todoID}, nil
	}
	return &Reply{Content: content, Model: publicModel, TodoID: todoID}, nil
}

func (g *Gateway) resolveModel(requested string) string {
	model := g.cfg.Models.Resolve(requested)
	if model == requested && requested != "" && requested == configuredModelShortID(g.cfg.Models.Default) {
		// Discovery normally resolves this implicit alias. Retain it as a
		// fallback when the upstream catalog was temporarily unavailable.
		if g.pool == nil {
			model = g.cfg.Models.Default
		} else if _, defaultKnown := g.pool.Model(g.cfg.Models.Default); !defaultKnown {
			model = g.cfg.Models.Default
		}
	}
	if g.pool != nil {
		return g.pool.ResolveModel(model)
	}
	return model
}

func (g *Gateway) publicModelID(requested, resolved string) string {
	if _, explicitAlias := g.cfg.Models.Aliases[requested]; explicitAlias {
		return requested
	}
	if g.pool != nil {
		if publicID, ok := g.pool.PublicModelID(resolved); ok {
			return publicID
		}
	}
	if requested != "" {
		return configuredModelShortID(requested)
	}
	return configuredModelShortID(resolved)
}

func configuredModelShortID(id string) string {
	if _, runnerID, ok := strings.Cut(id, ":"); ok {
		id = runnerID
	}
	if _, short, ok := strings.Cut(id, "/"); ok && short != "" {
		return short
	}
	return id
}

func enrichToolResultNames(msgs []openai.ChatMessage, todoID string, store *session.Store) []openai.ChatMessage {
	copyMessages := append([]openai.ChatMessage(nil), msgs...)
	for i := range copyMessages {
		message := &copyMessages[i]
		if message.Role != "tool" || (message.Name != "" && message.Name != "tool") {
			continue
		}
		if name, ok := store.ToolName(todoID, message.ToolCallID); ok {
			message.Name = name
		}
	}
	return copyMessages
}

func (g *Gateway) agentSettings(template upstream.AgentSettings, model string, tools []openai.Tool) upstream.AgentSettings {
	agent := template
	agent.Model = model
	if len(tools) == 0 {
		return agent
	}
	agent.SystemMessage = openai.BuildToolSystemPrompt(tools)
	agent.SystemMessageMode = "raw"
	agent.Permissions = &upstream.ToolPermissions{
		Allow: []string{},
		Deny:  append([]string(nil), g.cfg.ToolProtocol.DenyUpstreamTools...),
	}
	return agent
}

func (g *Gateway) sessionEntry(req openai.ChatRequest) (session.Entry, bool) {
	if todoID := strings.TrimSpace(req.Metadata[openai.TodoIDMetadataKey]); todoID != "" {
		if entry, ok := g.sess.GetByTodoID(todoID); ok {
			return entry, true
		}
	}
	if key := conversationKey(req.Messages); key != "" {
		return g.sess.Get(key)
	}
	return session.Entry{}, false
}

func followUpBody(msgs []openai.ChatMessage) string {
	if len(openai.LastToolResults(msgs)) > 0 {
		return openai.FormatToolResults(msgs)
	}
	start := 0
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "assistant" {
			start = i + 1
			break
		}
	}
	if start >= len(msgs) {
		return ""
	}
	return openai.FlattenTurn(msgs[start:])
}

type frontendPayload struct {
	TodoID  string `json:"todoId"`
	TodoID2 string `json:"todo_id"`
	Status  string `json:"status"`
	Content string `json:"content"`
	BlockID string `json:"blockId"`
	Updates struct {
		Status string `json:"status"`
	} `json:"updates"`
}

func (g *Gateway) waitAssistant(
	ctx context.Context,
	sub *upstream.Subscription,
	cli *upstream.Client,
	todoID string,
	emit func(string) error,
) (string, error) {
	events := make(chan upstream.Event, 32)
	errc := make(chan error, 1)
	go func() { errc <- sub.Subscribe(ctx, todoID, events) }()

	var buf strings.Builder
	var filter openai.ToolCallStreamFilter
	pendingBlocks := make(map[string]struct{})
	for {
		select {
		case ev := <-events:
			var payload frontendPayload
			if err := json.Unmarshal(ev.Payload, &payload); err != nil {
				continue
			}
			eventTodoID := payload.TodoID
			if eventTodoID == "" {
				eventTodoID = payload.TodoID2
			}
			if eventTodoID != "" && eventTodoID != todoID {
				continue
			}
			switch ev.Type {
			case "block:message":
				buf.WriteString(payload.Content)
				if emit != nil {
					if delta := filter.Push(payload.Content); delta != "" {
						if err := emit(delta); err != nil {
							return "", fmt.Errorf("emit stream text: %w", err)
						}
					}
				}
			case "BLOCK_UPDATE":
				if payload.BlockID == "" {
					continue
				}
				switch payload.Updates.Status {
				case "AWAITING_APPROVAL":
					pendingBlocks[payload.BlockID] = struct{}{}
				case "COMPLETED", "DENIED", "FAILED", "ERROR", "CANCELLED":
					delete(pendingBlocks, payload.BlockID)
				}
			case "todo:status":
				switch payload.Status {
				case "READY", "READY_CHECKED", "DONE":
					if len(pendingBlocks) > 0 && payload.Status != "DONE" {
						continue
					}
					return finishAssistant(ctx, cli, todoID, buf.String(), &filter, emit)
				case "CANCELLED", "CANCELLED_CHECKED", "ERROR", "ERROR_CHECKED":
					return "", fmt.Errorf("upstream todo %s ended with status %s", todoID, payload.Status)
				}
			}
		case err := <-errc:
			if ctx.Err() != nil {
				return "", assistantWaitError(ctx)
			}
			content, restErr := finishAssistant(ctx, cli, todoID, buf.String(), &filter, emit)
			if restErr != nil {
				return "", restErr
			}
			if content == "" && err != nil {
				return "", err
			}
			return content, nil
		case <-ctx.Done():
			return "", assistantWaitError(ctx)
		}
	}
}

func finishAssistant(
	ctx context.Context,
	cli *upstream.Client,
	todoID string,
	streamed string,
	filter *openai.ToolCallStreamFilter,
	emit func(string) error,
) (string, error) {
	content, err := finalAssistant(ctx, cli, todoID, streamed)
	if err != nil {
		return "", err
	}
	if emit == nil {
		return content, nil
	}

	// REST is authoritative at completion. Only append a missing suffix when it
	// agrees with everything already received over the WebSocket; emitted bytes
	// cannot be retracted when the two sources diverge.
	if strings.HasPrefix(content, streamed) {
		if delta := filter.Push(content[len(streamed):]); delta != "" {
			if err := emit(delta); err != nil {
				return "", fmt.Errorf("emit final stream text: %w", err)
			}
		}
	}
	if tail := filter.Flush(); tail != "" {
		if err := emit(tail); err != nil {
			return "", fmt.Errorf("flush stream text: %w", err)
		}
	}
	return content, nil
}

func assistantWaitError(ctx context.Context) error {
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("timed out waiting for assistant reply: %w", ctx.Err())
	}
	return fmt.Errorf("waiting for assistant reply: %w", ctx.Err())
}

func finalAssistant(ctx context.Context, cli *upstream.Client, todoID, fallback string) (string, error) {
	msgs, err := cli.Messages(ctx, todoID)
	if err != nil {
		if fallback != "" {
			return fallback, nil
		}
		return "", err
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != "assistant" {
			continue
		}
		if msgs[i].Content != "" {
			return msgs[i].Content, nil
		}
		var b strings.Builder
		for _, block := range msgs[i].Blocks {
			if block.Type == "text" || block.Type == "markdown" {
				b.WriteString(block.Content)
			}
		}
		if b.Len() > 0 {
			return b.String(), nil
		}
	}
	return fallback, nil
}

func conversationKey(msgs []openai.ChatMessage) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "assistant" {
			return hashMessages(msgs[:i+1])
		}
	}
	return ""
}

func conversationKeyWith(msgs []openai.ChatMessage, assistant openai.ChatMessage) string {
	extended := append([]openai.ChatMessage{}, msgs...)
	extended = append(extended, assistant)
	return hashMessages(extended)
}

func hashMessages(msgs []openai.ChatMessage) string {
	data, _ := json.Marshal(msgs)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
