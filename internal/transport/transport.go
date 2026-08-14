package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"todo2api/internal/config"
	"todo2api/internal/gateway"
	"todo2api/internal/openai"
)

type Server struct {
	cfg *config.Config
	gw  gatewayClient
}

// gatewayClient is the subset of *gateway.Gateway the HTTP layer depends on.
// Keeping it an interface lets tests observe what the handlers pass to the
// gateway without standing up an upstream.
type gatewayClient interface {
	Complete(ctx context.Context, req openai.ChatRequest) (*gateway.Reply, error)
	Stream(ctx context.Context, req openai.ChatRequest, emit func(gateway.StreamEvent) error) (*gateway.Reply, error)
	Models() []openai.Model
}

const todoIDHeader = "X-Todo2API-Todo-ID"

func New(cfg *config.Config, gw *gateway.Gateway) *Server {
	// A nil *gateway.Gateway must not be stored as a typed nil inside the
	// gatewayClient interface: s.gw != nil would then be true and the config
	// model fallback would never run.
	var client gatewayClient
	if gw != nil {
		client = gw
	}
	return &Server{cfg: cfg, gw: client}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.Register(mux)
	return mux
}

func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/v1/models", s.auth(s.handleModels))
	mux.HandleFunc("/v1/chat/completions", s.auth(s.handleChat))
	mux.HandleFunc("/v1/messages/count_tokens", s.auth(s.handleMessagesCountTokens))
	mux.HandleFunc("/v1/messages", s.auth(s.handleMessages))
	// Keep the common misspelling as a compatibility alias.
	mux.HandleFunc("/v1/messeges/count_tokens", s.auth(s.handleMessagesCountTokens))
	mux.HandleFunc("/v1/messeges", s.auth(s.handleMessages))
	mux.HandleFunc("/v1/responses", s.auth(s.handleResponses))
}

// auth accepts OpenAI bearer tokens and Anthropic x-api-key tokens.
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	tokens := map[string]struct{}{}
	configured := len(s.cfg.Server.ClientTokens) > 0
	for _, t := range s.cfg.Server.ClientTokens {
		if t = strings.TrimSpace(t); t != "" {
			tokens[t] = struct{}{}
		}
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if configured {
			var bearer string
			scheme, credential, found := strings.Cut(strings.TrimSpace(r.Header.Get("Authorization")), " ")
			if found && strings.EqualFold(scheme, "Bearer") {
				bearer = strings.TrimSpace(credential)
			}
			xAPIKey := strings.TrimSpace(r.Header.Get("X-API-Key"))
			_, bearerOK := tokens[bearer]
			_, apiKeyOK := tokens[xAPIKey]
			if bearer == "" && xAPIKey == "" || !bearerOK && !apiKeyOK {
				if strings.HasPrefix(r.URL.Path, "/v1/messages") || strings.HasPrefix(r.URL.Path, "/v1/messeges") {
					writeAnthropicErr(w, http.StatusUnauthorized, "authentication_error", "invalid api key")
				} else {
					writeErr(w, http.StatusUnauthorized, "invalid api key")
				}
				return
			}
		}
		next(w, r)
	}
}

func requestTodoID(r *http.Request, metadata map[string]any) string {
	if todoID := strings.TrimSpace(r.Header.Get(todoIDHeader)); todoID != "" {
		return todoID
	}
	if value, ok := metadata[openai.TodoIDMetadataKey].(string); ok {
		return strings.TrimSpace(value)
	}
	return ""
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	list := openai.ModelList{Object: "list"}
	if s.gw != nil {
		list.Data = s.gw.Models()
		writeJSON(w, http.StatusOK, list)
		return
	}
	seen := map[string]bool{}
	ids := make([]string, 0, len(s.cfg.Models.Aliases)+1)
	for alias := range s.cfg.Models.Aliases {
		if seen[alias] {
			continue
		}
		seen[alias] = true
		ids = append(ids, alias)
	}
	if model := s.cfg.Models.Default; model != "" && !seen[model] {
		ids = append(ids, model)
	}
	sort.Strings(ids)
	for _, id := range ids {
		list.Data = append(list.Data, openai.Model{ID: id, Object: "model", OwnedBy: "todofor.ai"})
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	requestID := anthropicRequestID(r)
	w.Header().Set("X-Request-ID", requestID)
	var req openai.ChatRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if len(req.Messages) == 0 {
		writeErr(w, http.StatusBadRequest, "messages must not be empty")
		return
	}
	for i := range req.Messages {
		message := &req.Messages[i]
		if len(message.Parts) == 0 {
			continue
		}
		text, attachments, err := chatContentParts(message.Parts)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		message.Content = text
		message.Attachments = attachments
	}
	if s.gw == nil {
		writeErr(w, http.StatusServiceUnavailable, "gateway is not configured")
		return
	}
	if todoID := strings.TrimSpace(r.Header.Get(todoIDHeader)); todoID != "" {
		if req.Metadata == nil {
			req.Metadata = map[string]string{}
		}
		if req.Metadata[openai.TodoIDMetadataKey] == "" {
			req.Metadata[openai.TodoIDMetadataKey] = todoID
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.Upstream.PollTimeout+30*time.Second)
	defer cancel()

	if req.Stream {
		if flusher, ok := w.(http.Flusher); ok {
			s.streamChat(w, flusher, ctx, req)
			return
		}
	}

	reply, err := s.gw.Complete(ctx, req)
	if err != nil {
		writeGatewayErr(w, err)
		return
	}
	w.Header().Set(todoIDHeader, reply.TodoID)

	writeJSON(w, http.StatusOK, buildResponse(reply))
}

// chatContentParts renders OpenAI content parts into message text and
// attachments. Text parts are joined with blank lines; image_url parts become
// base64 attachments (or an inline note when the URL is remote), mirroring
// responsesInputContent for the chat completions endpoint. A part without a
// type is accepted only when it carries an explicit text field; everything
// else is rejected instead of being silently dropped.
func chatContentParts(parts []openai.ContentPart) (string, []openai.AttachmentInput, error) {
	var text strings.Builder
	var attachments []openai.AttachmentInput
	for _, part := range parts {
		switch part.Type {
		case "text", "input_text":
			if part.Text == "" {
				return "", nil, fmt.Errorf("content part of type %q has no text", part.Type)
			}
			if text.Len() > 0 {
				text.WriteString("\n\n")
			}
			text.WriteString(part.Text)
		case "":
			if part.Text == "" {
				return "", nil, fmt.Errorf("content part has no type and no text")
			}
			if text.Len() > 0 {
				text.WriteString("\n\n")
			}
			text.WriteString(part.Text)
		case "image_url":
			attachment, reference, err := chatImageAttachment(part, len(attachments)+1)
			if err != nil {
				return "", nil, err
			}
			if text.Len() > 0 {
				text.WriteString("\n\n")
			}
			if reference != "" {
				text.WriteString(reference)
			} else {
				text.WriteString("[image attached: " + attachment.Name + "]")
			}
			if attachment != nil {
				attachments = append(attachments, *attachment)
			}
		default:
			return "", nil, fmt.Errorf("unsupported content part type %q", part.Type)
		}
	}
	return text.String(), attachments, nil
}

// chatImageAttachment converts an OpenAI image_url content part into an
// attachment, mirroring responsesImageAttachment for chat requests.
func chatImageAttachment(part openai.ContentPart, index int) (*openai.AttachmentInput, string, error) {
	source := bytes.TrimSpace(part.ImageURL)
	if len(source) == 0 || bytes.Equal(source, []byte("null")) {
		source = bytes.TrimSpace(part.ImageURLCompat)
	}
	if len(source) == 0 || bytes.Equal(source, []byte("null")) {
		return nil, "", fmt.Errorf("image_url part requires an image_url value")
	}
	value, err := responsesStringValue(source)
	if err != nil {
		return nil, "", fmt.Errorf("invalid image_url: %w", err)
	}
	value = strings.TrimSpace(value)
	if strings.HasPrefix(strings.ToLower(value), "data:") {
		mimeType, data, err := decodeImageDataURL(value)
		if err != nil {
			return nil, "", err
		}
		return &openai.AttachmentInput{
			Name: fmt.Sprintf("image-%d.%s", index, imageExtension(mimeType)), MIMEType: mimeType, Data: data,
		}, "", nil
	}
	if !isHTTPURL(value) {
		return nil, "", fmt.Errorf("image_url must be an http(s) or data URL")
	}
	return nil, "[image URL provided but not uploaded]\n" + value, nil
}

func buildResponse(reply *gateway.Reply) openai.ChatResponse {
	choice := openai.Choice{Index: 0}
	if reply.IsToolCall() {
		tc := "tool_calls"
		choice.Message = &openai.ChatMessage{Role: "assistant", Content: reply.Content, ToolCalls: reply.ToolCalls}
		choice.FinishReason = &tc
	} else {
		stop := "stop"
		choice.Message = &openai.ChatMessage{Role: "assistant", Content: reply.Content}
		choice.FinishReason = &stop
	}
	return openai.ChatResponse{
		ID:      "chatcmpl-" + fmt.Sprint(time.Now().UnixNano()),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   reply.Model,
		Choices: []openai.Choice{choice},
		Usage:   chatUsage(reply.Usage),
		Metadata: map[string]string{
			openai.TodoIDMetadataKey: reply.TodoID,
		},
	}
}

func chatUsage(usage gateway.TokenUsage) *openai.Usage {
	if !usage.Available {
		return nil
	}
	promptTokens := usage.InputTokens + usage.CacheReadTokens + usage.CacheWriteTokens
	return &openai.Usage{
		PromptTokens:     promptTokens,
		CompletionTokens: usage.OutputTokens,
		TotalTokens:      promptTokens + usage.OutputTokens,
		PromptTokensDetails: &openai.PromptTokensDetails{
			CachedTokens: usage.CacheReadTokens,
		},
		CompletionTokensDetails: &openai.CompletionTokensDetails{},
	}
}

func (s *Server) streamChat(w http.ResponseWriter, flusher http.Flusher, ctx context.Context, req openai.ChatRequest) {
	stream := &chatSSE{
		w: w, flusher: flusher,
		includeUsage: req.StreamOptions != nil && req.StreamOptions.IncludeUsage,
	}
	reply, err := s.gw.Stream(ctx, req, stream.onGatewayEvent)
	if err != nil {
		if !stream.started {
			writeGatewayErr(w, err)
			return
		}
		_ = stream.emitError(err)
		_ = stream.done()
		return
	}
	_ = stream.finish(reply)
}

type chatSSE struct {
	w            http.ResponseWriter
	flusher      http.Flusher
	id           string
	created      int64
	model        string
	todoID       string
	started      bool
	includeUsage bool
}

func (s *chatSSE) onGatewayEvent(event gateway.StreamEvent) error {
	switch event.Type {
	case gateway.StreamStart:
		return s.start(event.Model, event.TodoID)
	case gateway.StreamTextDelta:
		return s.emit(openai.Choice{
			Index: 0, Delta: &openai.Delta{Content: event.Delta},
		})
	default:
		return fmt.Errorf("unsupported gateway stream event %q", event.Type)
	}
}

func (s *chatSSE) start(model, todoID string) error {
	if s.started {
		return fmt.Errorf("chat stream already started")
	}
	s.id = "chatcmpl-" + fmt.Sprint(time.Now().UnixNano())
	s.created = time.Now().Unix()
	s.model = model
	s.todoID = todoID
	s.started = true
	setSSEHeaders(s.w)
	s.w.Header().Set(todoIDHeader, todoID)
	return s.emit(openai.Choice{
		Index: 0, Delta: &openai.Delta{Role: "assistant"},
	})
}

func (s *chatSSE) emit(choice openai.Choice) error {
	return s.emitChunk([]openai.Choice{choice}, nil)
}

func (s *chatSSE) emitChunk(choices []openai.Choice, usage *openai.Usage) error {
	if !s.started {
		return fmt.Errorf("chat stream has not started")
	}
	chunk := openai.ChatResponse{
		ID: s.id, Object: "chat.completion.chunk", Created: s.created,
		Model: s.model, Choices: choices, Usage: usage,
		Metadata: map[string]string{openai.TodoIDMetadataKey: s.todoID},
	}
	b, err := json.Marshal(chunk)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(s.w, "data: %s\n\n", b); err != nil {
		return err
	}
	s.flusher.Flush()
	return nil
}

func (s *chatSSE) finish(reply *gateway.Reply) error {
	if reply.IsToolCall() {
		calls := make([]openai.ToolCall, len(reply.ToolCalls))
		for i, call := range reply.ToolCalls {
			index := i
			call.Index = &index
			calls[i] = call
		}
		if err := s.emit(openai.Choice{Index: 0, Delta: &openai.Delta{ToolCalls: calls}}); err != nil {
			return err
		}
		toolCalls := "tool_calls"
		if err := s.emit(openai.Choice{Index: 0, Delta: &openai.Delta{}, FinishReason: &toolCalls}); err != nil {
			return err
		}
	} else {
		stop := "stop"
		if err := s.emit(openai.Choice{Index: 0, Delta: &openai.Delta{}, FinishReason: &stop}); err != nil {
			return err
		}
	}
	if s.includeUsage {
		if usage := chatUsage(reply.Usage); usage != nil {
			if err := s.emitChunk([]openai.Choice{}, usage); err != nil {
				return err
			}
		}
	}
	return s.done()
}

func (s *chatSSE) emitError(streamErr error) error {
	payload := openai.APIError{Error: openai.ErrorBody{Message: streamErr.Error(), Type: "error"}}
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(s.w, "data: %s\n\n", b); err != nil {
		return err
	}
	s.flusher.Flush()
	return nil
}

func (s *chatSSE) done() error {
	if _, err := fmt.Fprint(s.w, "data: [DONE]\n\n"); err != nil {
		return err
	}
	s.flusher.Flush()
	return nil
}

func setSSEHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, openai.APIError{Error: openai.ErrorBody{Message: msg, Type: "error"}})
}

func gatewayErrorStatus(w http.ResponseWriter, err error) int {
	if errors.Is(err, gateway.ErrAccountsUnavailable) {
		w.Header().Set("Retry-After", "60")
		return http.StatusServiceUnavailable
	}
	return http.StatusBadGateway
}

func writeGatewayErr(w http.ResponseWriter, err error) {
	writeErr(w, gatewayErrorStatus(w, err), err.Error())
}

func writeAnthropicGatewayErr(w http.ResponseWriter, err error) {
	writeAnthropicErr(w, gatewayErrorStatus(w, err), "api_error", err.Error())
}
