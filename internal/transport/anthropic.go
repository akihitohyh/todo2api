package transport

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"todo2api/internal/gateway"
	"todo2api/internal/openai"
)

type anthropicRequest struct {
	Model     string                  `json:"model"`
	MaxTokens int                     `json:"max_tokens"`
	System    json.RawMessage         `json:"system,omitempty"`
	Messages  []anthropicInputMessage `json:"messages"`
	Tools     []anthropicTool         `json:"tools,omitempty"`
	Stream    bool                    `json:"stream,omitempty"`
	Metadata  map[string]any          `json:"metadata,omitempty"`
}

type anthropicInputMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	Source    json.RawMessage `json:"source,omitempty"`
	MediaType string          `json:"media_type,omitempty"`
	Data      string          `json:"data,omitempty"`
	URL       string          `json:"url,omitempty"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
	OutputTokens             int `json:"output_tokens"`
}

type anthropicOutputBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

type anthropicMessageResponse struct {
	ID           string                 `json:"id"`
	Type         string                 `json:"type"`
	Role         string                 `json:"role"`
	Model        string                 `json:"model"`
	Content      []anthropicOutputBlock `json:"content"`
	StopReason   *string                `json:"stop_reason"`
	StopSequence *string                `json:"stop_sequence"`
	Usage        anthropicUsage         `json:"usage"`
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAnthropicErr(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}

	var req anthropicRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAnthropicErr(w, http.StatusBadRequest, "invalid_request_error", "invalid request body")
		return
	}
	chatReq, err := req.chatRequest(requestTodoID(r, req.Metadata))
	if err != nil {
		writeAnthropicErr(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.Upstream.PollTimeout+30*time.Second)
	defer cancel()
	if req.Stream {
		if flusher, ok := w.(http.Flusher); ok {
			s.streamAnthropic(w, flusher, ctx, req, chatReq)
			return
		}
	}
	reply, err := s.gw.Complete(ctx, chatReq)
	if err != nil {
		writeAnthropicGatewayErr(w, err)
		return
	}
	w.Header().Set(todoIDHeader, reply.TodoID)

	resp := buildAnthropicResponse(req.Model, reply)
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleMessagesCountTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAnthropicErr(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	var req anthropicRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAnthropicErr(w, http.StatusBadRequest, "invalid_request_error", "invalid request body")
		return
	}
	b, _ := json.Marshal(req)
	// This endpoint is a compatibility estimate; upstream does not expose a tokenizer.
	estimate := (utf8.RuneCount(b) + 3) / 4
	if estimate == 0 {
		estimate = 1
	}
	w.Header().Set("X-Todo2API-Token-Estimate", "true")
	writeJSON(w, http.StatusOK, map[string]int{"input_tokens": estimate})
}

func (r anthropicRequest) chatRequest(todoID string) (openai.ChatRequest, error) {
	if strings.TrimSpace(r.Model) == "" {
		return openai.ChatRequest{}, fmt.Errorf("model must not be empty")
	}
	if len(r.Messages) == 0 {
		return openai.ChatRequest{}, fmt.Errorf("messages must not be empty")
	}

	chat := openai.ChatRequest{Model: r.Model, Stream: r.Stream}
	if todoID != "" {
		chat.Metadata = map[string]string{openai.TodoIDMetadataKey: todoID}
	}
	if len(bytes.TrimSpace(r.System)) > 0 && !bytes.Equal(bytes.TrimSpace(r.System), []byte("null")) {
		system, err := anthropicText(r.System)
		if err != nil {
			return openai.ChatRequest{}, fmt.Errorf("invalid system content: %w", err)
		}
		appendAnthropicSystem(&chat.System, system)
	}

	callNames := make(map[string]string)
	for _, message := range r.Messages {
		blocks, err := decodeAnthropicContent(message.Content)
		if err != nil {
			return openai.ChatRequest{}, fmt.Errorf("invalid %s content: %w", message.Role, err)
		}
		switch message.Role {
		case "assistant":
			converted := openai.ChatMessage{Role: "assistant"}
			for _, block := range blocks {
				switch block.Type {
				case "text":
					converted.Content += block.Text
				case "tool_use":
					if block.ID == "" || block.Name == "" {
						return openai.ChatRequest{}, fmt.Errorf("tool_use requires id and name")
					}
					args := compactJSON(block.Input, "{}")
					converted.ToolCalls = append(converted.ToolCalls, openai.ToolCall{
						ID: block.ID, Type: "function",
						Function: openai.FunctionCall{Name: block.Name, Arguments: args},
					})
					callNames[block.ID] = block.Name
				case "thinking", "redacted_thinking":
					// Thinking blocks are not sent back to the upstream model.
				default:
					return openai.ChatRequest{}, fmt.Errorf("unsupported assistant content block %q", block.Type)
				}
			}
			chat.Messages = append(chat.Messages, converted)
		case "user":
			var text strings.Builder
			flushText := func() {
				if text.Len() == 0 {
					return
				}
				chat.Messages = append(chat.Messages, openai.ChatMessage{Role: "user", Content: text.String()})
				text.Reset()
			}
			for _, block := range blocks {
				switch block.Type {
				case "text":
					text.WriteString(block.Text)
				case "image":
					attachment, err := anthropicImageAttachment(block)
					if err != nil {
						return openai.ChatRequest{}, fmt.Errorf("invalid image content: %w", err)
					}
					chat.Attachments = append(chat.Attachments, attachment)
					if text.Len() > 0 {
						text.WriteString("\n\n")
					}
					text.WriteString("[image attached: " + attachment.Name + "]")
				case "tool_result":
					flushText()
					if block.ToolUseID == "" {
						return openai.ChatRequest{}, fmt.Errorf("tool_result requires tool_use_id")
					}
					name := callNames[block.ToolUseID]
					if name == "" {
						name = "tool"
					}
					result, err := anthropicText(block.Content)
					if err != nil {
						return openai.ChatRequest{}, fmt.Errorf("invalid tool_result content: %w", err)
					}
					if block.IsError {
						result = "[tool error] " + result
					}
					chat.Messages = append(chat.Messages, openai.ChatMessage{
						Role: "tool", ToolCallID: block.ToolUseID, Name: name, Content: result,
					})
				default:
					return openai.ChatRequest{}, fmt.Errorf("unsupported user content block %q", block.Type)
				}
			}
			flushText()
		case "system":
			system, err := anthropicText(message.Content)
			if err != nil {
				return openai.ChatRequest{}, fmt.Errorf("invalid system message content: %w", err)
			}
			appendAnthropicSystem(&chat.System, system)
		default:
			return openai.ChatRequest{}, fmt.Errorf("unsupported message role %q", message.Role)
		}
	}

	for _, tool := range r.Tools {
		if strings.TrimSpace(tool.Name) == "" {
			return openai.ChatRequest{}, fmt.Errorf("tool name must not be empty")
		}
		parameters := tool.InputSchema
		if len(bytes.TrimSpace(parameters)) == 0 {
			parameters = json.RawMessage(`{"type":"object"}`)
		}
		chat.Tools = append(chat.Tools, openai.Tool{
			Type: "function",
			Function: openai.FunctionDecl{
				Name: tool.Name, Description: tool.Description, Parameters: parameters,
			},
		})
	}
	return chat, nil
}

func anthropicImageAttachment(block anthropicContentBlock) (openai.AttachmentInput, error) {
	source := bytes.TrimSpace(block.Source)
	if len(source) == 0 || bytes.Equal(source, []byte("null")) {
		return openai.AttachmentInput{}, fmt.Errorf("image source is required")
	}
	var sourceObject struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
		URL       string `json:"url"`
	}
	if err := json.Unmarshal(source, &sourceObject); err != nil {
		return openai.AttachmentInput{}, err
	}
	if sourceObject.Type == "url" || sourceObject.URL != "" {
		return openai.AttachmentInput{}, fmt.Errorf("remote image URLs are not downloadable by this gateway; send base64 source")
	}
	if sourceObject.Type != "base64" && sourceObject.Data == "" {
		return openai.AttachmentInput{}, fmt.Errorf("unsupported image source type %q", sourceObject.Type)
	}
	if sourceObject.Data == "" {
		return openai.AttachmentInput{}, fmt.Errorf("base64 image data is empty")
	}
	mimeType := sourceObject.MediaType
	if mimeType == "" {
		mimeType = block.MediaType
	}
	if !strings.HasPrefix(strings.ToLower(mimeType), "image/") {
		return openai.AttachmentInput{}, fmt.Errorf("image media_type must be image/*")
	}
	data, err := base64.StdEncoding.DecodeString(sourceObject.Data)
	if err != nil {
		return openai.AttachmentInput{}, fmt.Errorf("decode image data: %w", err)
	}
	if len(data) == 0 {
		return openai.AttachmentInput{}, fmt.Errorf("image data is empty")
	}
	if len(data) > 20<<20 {
		return openai.AttachmentInput{}, fmt.Errorf("image is larger than 20 MiB")
	}
	ext := "bin"
	if i := strings.LastIndexByte(mimeType, '/'); i >= 0 {
		ext = strings.ToLower(mimeType[i+1:])
	}
	return openai.AttachmentInput{Name: "image." + ext, MIMEType: mimeType, Data: data}, nil
}

func appendAnthropicSystem(system *string, content string) {
	if content == "" {
		return
	}
	if *system != "" {
		*system += "\n\n"
	}
	*system += content
}

func decodeAnthropicContent(raw json.RawMessage) ([]anthropicContentBlock, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, nil
	}
	if raw[0] == '"' {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return nil, err
		}
		return []anthropicContentBlock{{Type: "text", Text: text}}, nil
	}
	var blocks []anthropicContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, err
	}
	return blocks, nil
}

func anthropicText(raw json.RawMessage) (string, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return "", nil
	}
	if raw[0] == '"' {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return "", err
		}
		return text, nil
	}
	blocks, err := decodeAnthropicContent(raw)
	if err != nil {
		return "", err
	}
	var text strings.Builder
	for _, block := range blocks {
		if block.Type == "text" {
			text.WriteString(block.Text)
		}
	}
	if text.Len() > 0 {
		return text.String(), nil
	}
	return string(raw), nil
}

func buildAnthropicResponse(requestedModel string, reply *gateway.Reply) anthropicMessageResponse {
	model := requestedModel
	if model == "" {
		model = reply.Model
	}
	stopReason := "end_turn"
	content := make([]anthropicOutputBlock, 0, len(reply.ToolCalls)+1)
	if reply.Content != "" || !reply.IsToolCall() {
		content = append(content, anthropicOutputBlock{Type: "text", Text: reply.Content})
	}
	if reply.IsToolCall() {
		stopReason = "tool_use"
		for _, call := range reply.ToolCalls {
			content = append(content, anthropicOutputBlock{
				Type: "tool_use", ID: call.ID, Name: call.Function.Name,
				Input: json.RawMessage(compactJSON(json.RawMessage(call.Function.Arguments), "{}")),
			})
		}
	}
	return anthropicMessageResponse{
		ID: "msg_" + fmt.Sprint(time.Now().UnixNano()), Type: "message", Role: "assistant",
		Model: model, Content: content, StopReason: &stopReason, Usage: anthropicTokenUsage(reply.Usage),
	}
}

func anthropicTokenUsage(usage gateway.TokenUsage) anthropicUsage {
	if !usage.Available {
		return anthropicUsage{}
	}
	return anthropicUsage{
		InputTokens:              usage.InputTokens,
		CacheCreationInputTokens: usage.CacheWriteTokens,
		CacheReadInputTokens:     usage.CacheReadTokens,
		OutputTokens:             usage.OutputTokens,
	}
}

func (s *Server) streamAnthropic(
	w http.ResponseWriter,
	flusher http.Flusher,
	ctx context.Context,
	req anthropicRequest,
	chatReq openai.ChatRequest,
) {
	stream := &anthropicSSE{w: w, flusher: flusher, requestedModel: req.Model}
	reply, err := s.gw.Stream(ctx, chatReq, stream.onGatewayEvent)
	if err != nil {
		if !stream.started {
			writeAnthropicGatewayErr(w, err)
			return
		}
		_ = stream.emitError(err)
		return
	}
	_ = stream.finish(reply)
}

type anthropicSSE struct {
	w              http.ResponseWriter
	flusher        http.Flusher
	requestedModel string
	messageID      string
	model          string
	todoID         string
	started        bool
	textStarted    bool
	text           strings.Builder
}

func (s *anthropicSSE) onGatewayEvent(event gateway.StreamEvent) error {
	switch event.Type {
	case gateway.StreamStart:
		return s.start(event.Model, event.TodoID)
	case gateway.StreamTextDelta:
		return s.textDelta(event.Delta)
	default:
		return fmt.Errorf("unsupported gateway stream event %q", event.Type)
	}
}

func (s *anthropicSSE) start(model, todoID string) error {
	if s.started {
		return fmt.Errorf("Anthropic stream already started")
	}
	s.started = true
	s.model = s.requestedModel
	if s.model == "" {
		s.model = model
	}
	s.todoID = todoID
	s.messageID = "msg_" + fmt.Sprint(time.Now().UnixNano())
	setSSEHeaders(s.w)
	s.w.Header().Set(todoIDHeader, todoID)
	message := anthropicMessageResponse{
		ID: s.messageID, Type: "message", Role: "assistant", Model: s.model,
		Content: []anthropicOutputBlock{}, StopReason: nil, Usage: anthropicUsage{},
	}
	return emitAnthropicEventE(s.w, s.flusher, "message_start", map[string]any{
		"type": "message_start", "message": message,
	})
}

func (s *anthropicSSE) textDelta(delta string) error {
	if delta == "" {
		return nil
	}
	if err := s.startTextBlock(); err != nil {
		return err
	}
	s.text.WriteString(delta)
	return emitAnthropicEventE(s.w, s.flusher, "content_block_delta", map[string]any{
		"type": "content_block_delta", "index": 0,
		"delta": map[string]any{"type": "text_delta", "text": delta},
	})
}

func (s *anthropicSSE) startTextBlock() error {
	if s.textStarted {
		return nil
	}
	if !s.started {
		return fmt.Errorf("Anthropic stream has not started")
	}
	s.textStarted = true
	return emitAnthropicEventE(s.w, s.flusher, "content_block_start", map[string]any{
		"type": "content_block_start", "index": 0,
		"content_block": map[string]any{"type": "text", "text": ""},
	})
}

func (s *anthropicSSE) finish(reply *gateway.Reply) error {
	index := 0
	if s.textStarted || !reply.IsToolCall() {
		if err := s.startTextBlock(); err != nil {
			return err
		}
		if err := emitAnthropicEventE(s.w, s.flusher, "content_block_stop", map[string]any{
			"type": "content_block_stop", "index": index,
		}); err != nil {
			return err
		}
		index++
	}

	for _, call := range reply.ToolCalls {
		input := compactJSON(json.RawMessage(call.Function.Arguments), "{}")
		if err := emitAnthropicEventE(s.w, s.flusher, "content_block_start", map[string]any{
			"type": "content_block_start", "index": index,
			"content_block": map[string]any{
				"type": "tool_use", "id": call.ID, "name": call.Function.Name,
				"input": map[string]any{},
			},
		}); err != nil {
			return err
		}
		if err := emitAnthropicEventE(s.w, s.flusher, "content_block_delta", map[string]any{
			"type": "content_block_delta", "index": index,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": input},
		}); err != nil {
			return err
		}
		if err := emitAnthropicEventE(s.w, s.flusher, "content_block_stop", map[string]any{
			"type": "content_block_stop", "index": index,
		}); err != nil {
			return err
		}
		index++
	}

	stopReason := "end_turn"
	if reply.IsToolCall() {
		stopReason = "tool_use"
	}
	if err := emitAnthropicEventE(s.w, s.flusher, "message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": anthropicTokenUsage(reply.Usage),
	}); err != nil {
		return err
	}
	return emitAnthropicEventE(s.w, s.flusher, "message_stop", map[string]any{"type": "message_stop"})
}

func (s *anthropicSSE) emitError(streamErr error) error {
	return emitAnthropicEventE(s.w, s.flusher, "error", map[string]any{
		"type":  "error",
		"error": map[string]string{"type": "api_error", "message": streamErr.Error()},
	})
}

func emitAnthropicEventE(w http.ResponseWriter, flusher http.Flusher, event string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

func writeAnthropicErr(w http.ResponseWriter, code int, errorType, message string) {
	writeJSON(w, code, map[string]any{
		"type":  "error",
		"error": map[string]string{"type": errorType, "message": message},
	})
}

func compactJSON(raw json.RawMessage, fallback string) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || !json.Valid(raw) {
		return fallback
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return fallback
	}
	return buf.String()
}
