package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"todo2api/internal/config"
	"todo2api/internal/openai"
	"todo2api/internal/pool"
	"todo2api/internal/session"
	"todo2api/internal/storage"
	"todo2api/internal/upstream"
)

type Gateway struct {
	cfg      *config.Config
	pool     *pool.Pool
	sess     *session.Store
	recorder CallRecorder
}

type CallRecorder interface {
	RecordCall(context.Context, string, storage.Usage, bool) error
}

var ErrAccountsUnavailable = errors.New("all upstream accounts are unavailable")

func New(cfg *config.Config, p *pool.Pool, s *session.Store, recorders ...CallRecorder) *Gateway {
	g := &Gateway{cfg: cfg, pool: p, sess: s}
	if len(recorders) > 0 {
		g.recorder = recorders[0]
	}
	return g
}

// Reply carries either final text or a set of tool calls for the client to run.
type Reply struct {
	Content   string
	ToolCalls []openai.ToolCall
	Model     string
	TodoID    string
	Usage     TokenUsage
}

func (r *Reply) IsToolCall() bool { return len(r.ToolCalls) > 0 }

// TokenUsage is the exact per-turn usage reported by the upstream assistant
// message. Available is false when the authoritative message metadata could
// not be read; callers must not replace missing usage with an estimate.
type TokenUsage struct {
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int
	Available        bool
	Cost             float64
}

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
	var completedUsage TokenUsage
	succeeded := false
	defer func() {
		if g.recorder == nil {
			return
		}
		_ = g.recorder.RecordCall(context.Background(), publicModel, storage.Usage{
			InputTokens: completedUsage.InputTokens, OutputTokens: completedUsage.OutputTokens,
			CacheReadTokens: completedUsage.CacheReadTokens, CacheWriteTokens: completedUsage.CacheWriteTokens,
			Cost: completedUsage.Cost,
		}, succeeded)
	}()
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
	}
	if resuming {
		req.Messages = enrichToolResultNames(req.Messages, entry.TodoID, g.sess)
	}

	runCtx, cancel := context.WithTimeout(ctx, g.cfg.Upstream.PollTimeout)
	defer cancel()

	var sub *upstream.Subscription
	var runtime pool.AccountRuntime
	todoID := entry.TodoID
	if resuming {
		acc.Acquire()
		defer acc.Release()
		runtime = acc.Runtime()
		if runtime.Client == nil {
			return nil, fmt.Errorf("session account client is unavailable")
		}
		var err error
		sub, err = runtime.Client.PrepareSubscription(runCtx)
		if err != nil {
			if handleErr := g.handleAccountFailure(acc, err); handleErr != nil {
				return nil, handleErr
			}
			return nil, err
		}
		defer sub.Close()

		agent, filteredTools := g.accountRequestSettings(runtime, runnerModel, req)
		content := followUpBody(req.Messages)
		if content == "" {
			return nil, fmt.Errorf("resumed request has no new user or tool-result messages")
		}
		// Only the follow-up turn's attachments are re-uploaded: everything
		// before the last assistant was already registered when the
		// conversation was created.
		attachments, err := registerAttachments(runCtx, runtime.Client, followUpAttachments(req.Messages), todoID)
		if err != nil {
			return nil, err
		}
		if _, err := runtime.Client.AddMessageWithAttachments(runCtx, runtime.ProjectID, todoID, content, agent, attachments, filteredTools); err != nil {
			if handleErr := g.handleAccountFailure(acc, err); handleErr != nil {
				return nil, handleErr
			}
			return nil, err
		}
	} else {
		var err error
		acc, runtime, sub, todoID, err = g.startNewConversation(runCtx, req, runnerModel)
		if err != nil {
			return nil, err
		}
		defer acc.Release()
		defer sub.Close()
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
	result, err := g.waitAssistant(runCtx, sub, runtime.Client, todoID, emitText)
	if err != nil {
		if handleErr := g.handleAccountFailure(acc, err); handleErr != nil {
			return nil, handleErr
		}
		return nil, err
	}
	content := result.Content
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
		conversationKeyWith(req.System, req.Messages, assistant),
		session.Entry{TodoID: todoID, Account: accountIndex, ExpiresAt: time.Now().Add(30 * time.Minute)},
	)
	if len(calls) > 0 {
		names := make(map[string]string, len(calls))
		for _, call := range calls {
			names[call.ID] = call.Function.Name
		}
		g.sess.PutToolNames(todoID, names)
	}

	completedUsage = result.Usage
	succeeded = true
	if len(calls) > 0 {
		return &Reply{
			Content: text, ToolCalls: calls, Model: publicModel, TodoID: todoID, Usage: result.Usage,
		}, nil
	}
	return &Reply{Content: content, Model: publicModel, TodoID: todoID, Usage: result.Usage}, nil
}

func (g *Gateway) startNewConversation(
	ctx context.Context,
	req openai.ChatRequest,
	runnerModel string,
) (*pool.Account, pool.AccountRuntime, *upstream.Subscription, string, error) {
	excluded := make(map[*pool.Account]struct{})
	content := openai.FlattenTurn(req.Messages)
	attempts := g.pool.Len()
	var lastErr error

	for attempt := 0; attempt < attempts; attempt++ {
		acc := g.pool.PickExcept(excluded)
		if acc == nil {
			break
		}
		acc.Acquire()
		runtime := acc.Runtime()
		if runtime.Client == nil {
			acc.Release()
			excluded[acc] = struct{}{}
			lastErr = fmt.Errorf("account client is unavailable")
			continue
		}

		sub, err := runtime.Client.PrepareSubscription(ctx)
		if err == nil {
			agent, filteredTools := g.accountRequestSettings(runtime, runnerModel, req)
			// A new conversation carries the full history, so every message's
			// attachments must be registered.
			attachments, uploadErr := registerAttachments(ctx, runtime.Client, allAttachments(req.Messages), "")
			if uploadErr != nil {
				err = uploadErr
			} else {
				var todo *upstream.Todo
				todo, err = runtime.Client.CreateTodoWithAttachments(ctx, runtime.ProjectID, content, agent, attachments, filteredTools)
				if err == nil {
					return acc, runtime, sub, todo.ID, nil
				}
			}
		}

		if sub != nil {
			sub.Close()
		}
		acc.Release()
		action, cooldown := accountFailurePolicy(err)
		if action == accountFailureNone {
			return nil, pool.AccountRuntime{}, nil, "", err
		}

		if handleErr := g.applyAccountFailure(acc, action, cooldown, err); handleErr != nil {
			return nil, pool.AccountRuntime{}, nil, "", handleErr
		}
		excluded[acc] = struct{}{}
		lastErr = err
	}

	if lastErr != nil {
		return nil, pool.AccountRuntime{}, nil, "", fmt.Errorf("%w: %v", ErrAccountsUnavailable, lastErr)
	}
	return nil, pool.AccountRuntime{}, nil, "", ErrAccountsUnavailable
}

func registerAttachments(ctx context.Context, client *upstream.Client, inputs []openai.AttachmentInput, todoID string) ([]upstream.AttachmentFrame, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	frames := make([]upstream.AttachmentFrame, 0, len(inputs))
	for _, input := range inputs {
		frame, err := client.RegisterAttachment(ctx, upstream.AttachmentUpload{
			Name: input.Name, MIMEType: input.MIMEType, Data: input.Data,
		}, todoID)
		if err != nil {
			return nil, fmt.Errorf("register attachment %q: %w", input.Name, err)
		}
		frames = append(frames, frame)
	}
	return frames, nil
}

func (g *Gateway) handleAccountFailure(acc *pool.Account, cause error) error {
	action, cooldown := accountFailurePolicy(cause)
	if action == accountFailureNone {
		return nil
	}
	return g.applyAccountFailure(acc, action, cooldown, cause)
}

func (g *Gateway) applyAccountFailure(
	acc *pool.Account,
	action accountFailureAction,
	cooldown time.Duration,
	cause error,
) error {
	index := g.pool.IndexOf(acc) + 1
	if action == accountFailureRemove {
		if err := g.pool.Remove(acc); err != nil {
			return fmt.Errorf("remove exhausted upstream account: %w", err)
		}
		log.Printf("upstream account %d permanently removed: %v", index, cause)
		return nil
	}
	var upstreamErr *upstream.HTTPError
	if errors.As(cause, &upstreamErr) && (upstreamErr.StatusCode == http.StatusUnauthorized || upstreamErr.StatusCode == http.StatusForbidden) {
		g.pool.MarkInvalid(context.Background(), acc, cause)
	}
	acc.CoolDown(cooldown)
	log.Printf("upstream account %d disabled for %s: %v", index, cooldown, cause)
	return nil
}

func (g *Gateway) accountRequestSettings(
	runtime pool.AccountRuntime,
	runnerModel string,
	req openai.ChatRequest,
) (upstream.AgentSettings, upstream.FilteredEdgeTools) {
	agent := g.agentSettings(runtime.Agent, runnerModel, req.System, req.Tools)
	filteredTools := runtime.EdgeTools
	if len(req.Tools) > 0 {
		filteredTools = nil
	}
	return agent, filteredTools
}

type accountFailureAction uint8

const (
	accountFailureNone accountFailureAction = iota
	accountFailureCooldown
	accountFailureRemove
)

func accountFailurePolicy(err error) (accountFailureAction, time.Duration) {
	var upstreamErr *upstream.HTTPError
	if !errors.As(err, &upstreamErr) {
		return accountFailureNone, 0
	}

	switch upstreamErr.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return accountFailureCooldown, time.Hour
	case http.StatusPaymentRequired:
		return accountFailureRemove, 0
	case http.StatusTooManyRequests:
		return accountFailureCooldown, 2 * time.Minute
	}

	detail := strings.ToLower(upstreamErr.Message + " " + upstreamErr.Body)
	for _, marker := range []string{
		"insufficient balance", "add funds", "insufficient credit",
		"subscription required",
	} {
		if strings.Contains(detail, marker) {
			return accountFailureRemove, 0
		}
	}
	return accountFailureNone, 0
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

func (g *Gateway) agentSettings(template upstream.AgentSettings, model string, systemPrompt string, tools []openai.Tool) upstream.AgentSettings {
	agent := template
	agent.Model = model

	// Merge user-provided system prompt with tool instructions
	if systemPrompt != "" && len(tools) > 0 {
		// Both system and tools: combine them
		agent.SystemMessage = systemPrompt + "\n\n" + openai.BuildToolSystemPrompt(tools)
		agent.SystemMessageMode = "raw"
	} else if systemPrompt != "" {
		// Only system prompt
		agent.SystemMessage = systemPrompt
		agent.SystemMessageMode = "raw"
	} else if len(tools) > 0 {
		// Only tools
		agent.SystemMessage = openai.BuildToolSystemPrompt(tools)
		agent.SystemMessageMode = "raw"
	}

	if len(tools) > 0 {
		agent.Permissions = &upstream.ToolPermissions{
			Allow: []string{},
			Deny:  append([]string(nil), g.cfg.ToolProtocol.DenyUpstreamTools...),
		}
	}
	return agent
}

func (g *Gateway) sessionEntry(req openai.ChatRequest) (session.Entry, bool) {
	if todoID := strings.TrimSpace(req.Metadata[openai.TodoIDMetadataKey]); todoID != "" {
		if entry, ok := g.sess.GetByTodoID(todoID); ok {
			return entry, true
		}
	}
	if key := conversationKey(req.System, req.Messages); key != "" {
		return g.sess.Get(key)
	}
	return session.Entry{}, false
}

func followUpBody(msgs []openai.ChatMessage) string {
	turn := followUpMessages(msgs)
	if len(turn) == 0 {
		return ""
	}
	if allToolMessages(turn) {
		// The turn is a pure tool-result follow-up: render it with the full
		// history so tool names are recovered from the preceding assistant
		// tool_calls.
		return openai.FormatToolResults(msgs)
	}
	// A mixed turn (user content/images plus tool results) must keep all of
	// it; sending only the trailing tool results would drop this turn's user
	// message and image placeholders.
	return openai.FlattenTurn(turn)
}

// followUpMessages returns the current follow-up turn: everything after the
// last assistant message, or the whole request when the history has no
// assistant yet (an explicit-todo resume). Tool results that trail a user
// message belong to the same turn and are kept together with it.
func followUpMessages(msgs []openai.ChatMessage) []openai.ChatMessage {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "assistant" {
			return msgs[i+1:]
		}
	}
	return msgs
}

// allToolMessages reports whether every message in the turn is a tool result.
func allToolMessages(msgs []openai.ChatMessage) bool {
	for _, m := range msgs {
		if m.Role != "tool" {
			return false
		}
	}
	return true
}

// followUpAttachments returns the attachments introduced by the current
// follow-up turn only. Historical messages (up to and including the last
// assistant message) already had their attachments registered when the
// conversation was created.
func followUpAttachments(msgs []openai.ChatMessage) []openai.AttachmentInput {
	var out []openai.AttachmentInput
	for _, m := range followUpMessages(msgs) {
		out = append(out, m.Attachments...)
	}
	return out
}

// allAttachments returns every message's attachments; used when starting a new
// conversation whose history must be preserved in full.
func allAttachments(msgs []openai.ChatMessage) []openai.AttachmentInput {
	var out []openai.AttachmentInput
	for _, m := range msgs {
		out = append(out, m.Attachments...)
	}
	return out
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
) (assistantResult, error) {
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
							return assistantResult{}, fmt.Errorf("emit stream text: %w", err)
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
					return assistantResult{}, fmt.Errorf("upstream todo %s ended with status %s", todoID, payload.Status)
				}
			}
		case err := <-errc:
			if ctx.Err() != nil {
				return assistantResult{}, assistantWaitError(ctx)
			}
			result, restErr := finishAssistant(ctx, cli, todoID, buf.String(), &filter, emit)
			if restErr != nil {
				return assistantResult{}, restErr
			}
			if result.Content == "" && err != nil {
				return assistantResult{}, err
			}
			return result, nil
		case <-ctx.Done():
			return assistantResult{}, assistantWaitError(ctx)
		}
	}
}

type assistantResult struct {
	Content string
	Usage   TokenUsage
}

func finishAssistant(
	ctx context.Context,
	cli *upstream.Client,
	todoID string,
	streamed string,
	filter *openai.ToolCallStreamFilter,
	emit func(string) error,
) (assistantResult, error) {
	result, err := finalAssistant(ctx, cli, todoID, streamed)
	if err != nil {
		return assistantResult{}, err
	}
	if emit == nil {
		return result, nil
	}

	// REST is authoritative at completion. Only append a missing suffix when it
	// agrees with everything already received over the WebSocket; emitted bytes
	// cannot be retracted when the two sources diverge.
	if strings.HasPrefix(result.Content, streamed) {
		if delta := filter.Push(result.Content[len(streamed):]); delta != "" {
			if err := emit(delta); err != nil {
				return assistantResult{}, fmt.Errorf("emit final stream text: %w", err)
			}
		}
	}
	if tail := filter.Flush(); tail != "" {
		if err := emit(tail); err != nil {
			return assistantResult{}, fmt.Errorf("flush stream text: %w", err)
		}
	}
	return result, nil
}

func assistantWaitError(ctx context.Context) error {
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("timed out waiting for assistant reply: %w", ctx.Err())
	}
	return fmt.Errorf("waiting for assistant reply: %w", ctx.Err())
}

func finalAssistant(ctx context.Context, cli *upstream.Client, todoID, fallback string) (assistantResult, error) {
	msgs, err := cli.Messages(ctx, todoID)
	if err != nil {
		if fallback != "" {
			return assistantResult{Content: fallback}, nil
		}
		return assistantResult{}, err
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != "assistant" {
			continue
		}
		result := assistantResult{Usage: tokenUsage(msgs[i].RunMeta)}
		if msgs[i].Content != "" {
			result.Content = msgs[i].Content
			return result, nil
		}
		var b strings.Builder
		for _, block := range msgs[i].Blocks {
			if block.Type == "text" || block.Type == "markdown" {
				b.WriteString(block.Content)
			}
		}
		if b.Len() > 0 {
			result.Content = b.String()
			return result, nil
		}
		if fallback != "" {
			result.Content = fallback
			return result, nil
		}
	}
	return assistantResult{Content: fallback}, nil
}

func tokenUsage(meta []upstream.RunMeta) TokenUsage {
	var usage TokenUsage
	for _, item := range meta {
		if item.Type != "todo:msg_meta_ai" {
			continue
		}
		usage.Available = true
		usage.Cost += item.Cost
		usage.InputTokens += item.Extras.InputTokens
		usage.OutputTokens += item.Extras.OutputTokens
		usage.CacheReadTokens += item.Extras.CacheReadTokens
		usage.CacheWriteTokens += item.Extras.CacheWriteTokens
	}
	return usage
}

func conversationKey(system string, msgs []openai.ChatMessage) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "assistant" {
			return hashConversation(system, msgs[:i+1])
		}
	}
	return ""
}

func conversationKeyWith(system string, msgs []openai.ChatMessage, assistant openai.ChatMessage) string {
	extended := append([]openai.ChatMessage{}, msgs...)
	extended = append(extended, assistant)
	return hashConversation(system, extended)
}

// hashableMessage is the stable, explicit view of a ChatMessage fed into the
// conversation hash. Field tags mirror ChatMessage exactly (including the
// non-omitempty content), so attachment-free histories hash identically to the
// legacy algorithm that marshaled ChatMessage directly.
type hashableMessage struct {
	Role        string               `json:"role"`
	Content     string               `json:"content"`
	ToolCalls   []openai.ToolCall    `json:"tool_calls,omitempty"`
	ToolCallID  string               `json:"tool_call_id,omitempty"`
	Name        string               `json:"name,omitempty"`
	Attachments []hashableAttachment `json:"attachments,omitempty"`
}

// hashableAttachment is the digest view of an attachment in the conversation
// hash. Raw bytes are never marshaled into the hash input; only the SHA-256
// digest is, so histories that differ only in image data still produce
// different continuation keys.
type hashableAttachment struct {
	Name       string `json:"name"`
	MIMEType   string `json:"mime_type,omitempty"`
	DataDigest string `json:"data_digest"`
}

func hashableMessages(msgs []openai.ChatMessage) []hashableMessage {
	out := make([]hashableMessage, len(msgs))
	for i, m := range msgs {
		out[i] = hashableMessage{
			Role: m.Role, Content: m.Content, ToolCalls: m.ToolCalls,
			ToolCallID: m.ToolCallID, Name: m.Name,
			Attachments: hashableAttachments(m.Attachments),
		}
	}
	return out
}

func hashableAttachments(atts []openai.AttachmentInput) []hashableAttachment {
	if len(atts) == 0 {
		return nil
	}
	out := make([]hashableAttachment, len(atts))
	for i, a := range atts {
		sum := sha256.Sum256(a.Data)
		out[i] = hashableAttachment{
			Name: a.Name, MIMEType: a.MIMEType, DataDigest: hex.EncodeToString(sum[:]),
		}
	}
	return out
}

func hashConversation(system string, msgs []openai.ChatMessage) string {
	data, _ := json.Marshal(struct {
		System   string            `json:"system,omitempty"`
		Messages []hashableMessage `json:"messages"`
	}{System: system, Messages: hashableMessages(msgs)})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
