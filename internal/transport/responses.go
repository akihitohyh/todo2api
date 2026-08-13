package transport

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"todo2api/internal/gateway"
	"todo2api/internal/openai"
)

type responsesRequest struct {
	Model              string          `json:"model"`
	Instructions       json.RawMessage `json:"instructions,omitempty"`
	Input              json.RawMessage `json:"input"`
	Stream             bool            `json:"stream,omitempty"`
	Tools              []responsesTool `json:"tools,omitempty"`
	PreviousResponseID string          `json:"previous_response_id,omitempty"`
	Metadata           map[string]any  `json:"metadata,omitempty"`
	MaxOutputTokens    *int            `json:"max_output_tokens,omitempty"`
	Store              *bool           `json:"store,omitempty"`
	ParallelToolCalls  *bool           `json:"parallel_tool_calls,omitempty"`
	ToolChoice         json.RawMessage `json:"tool_choice,omitempty"`
	Reasoning          json.RawMessage `json:"reasoning,omitempty"`
	Text               json.RawMessage `json:"text,omitempty"`
}

type responsesTool struct {
	Type        string                 `json:"type"`
	Name        string                 `json:"name,omitempty"`
	Description string                 `json:"description,omitempty"`
	Parameters  json.RawMessage        `json:"parameters,omitempty"`
	Strict      *bool                  `json:"strict,omitempty"`
	Function    *responsesFunctionTool `json:"function,omitempty"`
	Tools       []responsesTool        `json:"tools,omitempty"`
}

type responsesFunctionTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type responsesInputItem struct {
	Type      string          `json:"type,omitempty"`
	Role      string          `json:"role,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	ID        string          `json:"id,omitempty"`
	CallID    string          `json:"call_id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Namespace string          `json:"namespace,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	Output    json.RawMessage `json:"output,omitempty"`
	Tools     []responsesTool `json:"tools,omitempty"`
}

type responsesToolTarget struct {
	Type      string
	Name      string
	Namespace string
}

type responsesContentPart struct {
	Type           string          `json:"type"`
	Text           string          `json:"text,omitempty"`
	Refusal        string          `json:"refusal,omitempty"`
	ImageURL       json.RawMessage `json:"image_url,omitempty"`
	ImageURLCompat json.RawMessage `json:"imageUrl,omitempty"` // common camelCase client variant
	FileID         string          `json:"file_id,omitempty"`
	Detail         string          `json:"detail,omitempty"`
	Name           string          `json:"name,omitempty"`
}

type responsesUsage struct {
	InputTokens         int                         `json:"input_tokens"`
	InputTokensDetails  responsesInputTokenDetails  `json:"input_tokens_details"`
	OutputTokens        int                         `json:"output_tokens"`
	OutputTokensDetails responsesOutputTokenDetails `json:"output_tokens_details"`
	TotalTokens         int                         `json:"total_tokens"`
}

type responsesInputTokenDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

type responsesOutputTokenDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

type responsesOutputContent struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	Annotations []any  `json:"annotations"`
	Logprobs    []any  `json:"logprobs"`
}

type responsesOutputItem struct {
	ID        string                   `json:"id"`
	Type      string                   `json:"type"`
	Status    string                   `json:"status,omitempty"`
	Role      string                   `json:"role,omitempty"`
	Content   []responsesOutputContent `json:"content,omitempty"`
	CallID    string                   `json:"call_id,omitempty"`
	Name      string                   `json:"name,omitempty"`
	Namespace string                   `json:"namespace,omitempty"`
	Arguments string                   `json:"arguments,omitempty"`
	Input     string                   `json:"input,omitempty"`
}

type responsesResponse struct {
	ID                 string                `json:"id"`
	Object             string                `json:"object"`
	CreatedAt          int64                 `json:"created_at"`
	CompletedAt        *int64                `json:"completed_at"`
	Status             string                `json:"status"`
	Error              any                   `json:"error"`
	IncompleteDetails  any                   `json:"incomplete_details"`
	Model              string                `json:"model"`
	PreviousResponseID string                `json:"previous_response_id,omitempty"`
	Instructions       json.RawMessage       `json:"instructions,omitempty"`
	Output             []responsesOutputItem `json:"output"`
	OutputText         string                `json:"output_text,omitempty"`
	ParallelToolCalls  bool                  `json:"parallel_tool_calls"`
	MaxOutputTokens    *int                  `json:"max_output_tokens,omitempty"`
	ToolChoice         any                   `json:"tool_choice"`
	Tools              []responsesTool       `json:"tools"`
	Metadata           map[string]any        `json:"metadata"`
	Reasoning          any                   `json:"reasoning"`
	Store              bool                  `json:"store"`
	Temperature        *float64              `json:"temperature"`
	Text               any                   `json:"text"`
	TopP               *float64              `json:"top_p"`
	Truncation         string                `json:"truncation"`
	Usage              *responsesUsage       `json:"usage"`
}

func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req responsesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}

	todoID := requestTodoID(r, req.Metadata)
	if todoID == "" && req.PreviousResponseID != "" {
		var ok bool
		todoID, ok = todoIDFromResponseID(req.PreviousResponseID)
		if !ok {
			writeErr(w, http.StatusBadRequest, "previous_response_id was not issued by todo2api")
			return
		}
	}
	chatReq, toolTargets, err := req.chatRequest(todoID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.Upstream.PollTimeout+30*time.Second)
	defer cancel()
	if req.Stream {
		if flusher, ok := w.(http.Flusher); ok {
			s.streamResponses(w, flusher, ctx, req, chatReq, toolTargets)
			return
		}
	}
	reply, err := s.gw.Complete(ctx, chatReq)
	if err != nil {
		writeGatewayErr(w, err)
		return
	}
	w.Header().Set(todoIDHeader, reply.TodoID)

	resp := buildResponsesResponse(req, reply, toolTargets)
	writeJSON(w, http.StatusOK, resp)
}

func (r responsesRequest) chatRequest(todoID string) (openai.ChatRequest, map[string]responsesToolTarget, error) {
	if strings.TrimSpace(r.Model) == "" {
		return openai.ChatRequest{}, nil, fmt.Errorf("model must not be empty")
	}
	chat := openai.ChatRequest{Model: r.Model, Stream: r.Stream}
	effectiveTools := append([]responsesTool(nil), r.Tools...)
	if todoID != "" {
		chat.Metadata = map[string]string{openai.TodoIDMetadataKey: todoID}
	}

	if len(bytes.TrimSpace(r.Instructions)) > 0 && !bytes.Equal(bytes.TrimSpace(r.Instructions), []byte("null")) {
		instructions, err := responsesText(r.Instructions)
		if err != nil {
			return openai.ChatRequest{}, nil, fmt.Errorf("invalid instructions: %w", err)
		}
		if instructions != "" {
			chat.Messages = append(chat.Messages, openai.ChatMessage{Role: "system", Content: instructions})
		}
	}

	input := bytes.TrimSpace(r.Input)
	if len(input) == 0 || bytes.Equal(input, []byte("null")) {
		return openai.ChatRequest{}, nil, fmt.Errorf("input must not be empty")
	}
	if input[0] == '"' {
		var text string
		if err := json.Unmarshal(input, &text); err != nil {
			return openai.ChatRequest{}, nil, fmt.Errorf("invalid input: %w", err)
		}
		chat.Messages = append(chat.Messages, openai.ChatMessage{Role: "user", Content: text})
	} else {
		var rawItems []json.RawMessage
		if err := json.Unmarshal(input, &rawItems); err != nil {
			return openai.ChatRequest{}, nil, fmt.Errorf("input must be a string or item array")
		}
		callNames := make(map[string]string)
		for _, raw := range rawItems {
			var item responsesInputItem
			if err := json.Unmarshal(raw, &item); err != nil {
				return openai.ChatRequest{}, nil, fmt.Errorf("invalid input item: %w", err)
			}
			switch item.Type {
			case "", "message", "easy_input_message":
				role := item.Role
				if role == "developer" {
					role = "system"
				}
				if role != "system" && role != "user" && role != "assistant" {
					return openai.ChatRequest{}, nil, fmt.Errorf("unsupported input message role %q", item.Role)
				}
				text, attachments, err := responsesInputContent(item.Content)
				if err != nil {
					return openai.ChatRequest{}, nil, fmt.Errorf("invalid message content: %w", err)
				}
				chat.Attachments = append(chat.Attachments, attachments...)
				chat.Messages = append(chat.Messages, openai.ChatMessage{Role: role, Content: text})
			case "agent_message":
				text, err := responsesAgentMessageText(item.Content)
				if err != nil {
					return openai.ChatRequest{}, nil, fmt.Errorf("invalid agent_message content: %w", err)
				}
				if text != "" {
					chat.Messages = append(chat.Messages, openai.ChatMessage{Role: "assistant", Content: text})
				}
			case "function_call":
				callID := item.CallID
				if callID == "" {
					callID = item.ID
				}
				if callID == "" || item.Name == "" {
					return openai.ChatRequest{}, nil, fmt.Errorf("function_call requires call_id and name")
				}
				qualifiedName := responsesQualifiedToolName(item.Namespace, item.Name)
				appendResponseToolCall(&chat.Messages, openai.ToolCall{
					ID: callID, Type: "function",
					Function: openai.FunctionCall{
						Name: qualifiedName, Arguments: responsesString(item.Arguments, "{}"),
					},
				})
				callNames[callID] = qualifiedName
			case "custom_tool_call":
				callID := item.CallID
				if callID == "" {
					callID = item.ID
				}
				if callID == "" || item.Name == "" {
					return openai.ChatRequest{}, nil, fmt.Errorf("custom_tool_call requires call_id and name")
				}
				customInput := responsesString(item.Input, "")
				args, _ := json.Marshal(map[string]string{"input": customInput})
				qualifiedName := responsesQualifiedToolName(item.Namespace, item.Name)
				appendResponseToolCall(&chat.Messages, openai.ToolCall{
					ID: callID, Type: "function",
					Function: openai.FunctionCall{Name: qualifiedName, Arguments: string(args)},
				})
				callNames[callID] = qualifiedName
			case "function_call_output", "custom_tool_call_output":
				if item.CallID == "" {
					return openai.ChatRequest{}, nil, fmt.Errorf("%s requires call_id", item.Type)
				}
				name := callNames[item.CallID]
				if name == "" {
					name = "tool"
				}
				chat.Messages = append(chat.Messages, openai.ChatMessage{
					Role: "tool", ToolCallID: item.CallID, Name: name,
					Content: responsesString(item.Output, ""),
				})
			case "additional_tools":
				effectiveTools = append(effectiveTools, item.Tools...)
			case "reasoning", "item_reference":
				// These items carry OpenAI-internal state that todofor.ai cannot consume.
			default:
				return openai.ChatRequest{}, nil, fmt.Errorf("unsupported input item type %q", item.Type)
			}
		}
	}
	if len(chat.Messages) == 0 {
		return openai.ChatRequest{}, nil, fmt.Errorf("input contains no usable messages")
	}

	toolTargets := make(map[string]responsesToolTarget)
	seenTools := make(map[string]struct{}, len(effectiveTools))
	for _, tool := range effectiveTools {
		if err := appendResponsesTool(&chat, tool, "", "", seenTools, toolTargets); err != nil {
			return openai.ChatRequest{}, nil, err
		}
	}
	return chat, toolTargets, nil
}

func appendResponsesTool(
	chat *openai.ChatRequest,
	tool responsesTool,
	namespace string,
	namespaceDescription string,
	seen map[string]struct{},
	targets map[string]responsesToolTarget,
) error {
	name := tool.Name
	description := tool.Description
	parameters := tool.Parameters
	if tool.Function != nil {
		if name == "" {
			name = tool.Function.Name
		}
		if description == "" {
			description = tool.Function.Description
		}
		if len(parameters) == 0 {
			parameters = tool.Function.Parameters
		}
	}
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("tool name must not be empty")
	}

	key := tool.Type + "\x00" + namespace + "\x00" + name
	if _, exists := seen[key]; exists {
		return nil
	}
	seen[key] = struct{}{}

	if tool.Type == "namespace" {
		if namespace != "" {
			return fmt.Errorf("nested Responses namespace %q is not supported", name)
		}
		for _, child := range tool.Tools {
			if child.Type != "function" {
				return fmt.Errorf("unsupported Responses namespace tool type %q", child.Type)
			}
			if err := appendResponsesTool(chat, child, name, description, seen, targets); err != nil {
				return err
			}
		}
		return nil
	}

	switch tool.Type {
	case "function":
		if len(bytes.TrimSpace(parameters)) == 0 {
			parameters = json.RawMessage(`{"type":"object"}`)
		}
	case "custom":
		if namespace != "" {
			return fmt.Errorf("custom tools inside a Responses namespace are not supported")
		}
		parameters = json.RawMessage(`{"type":"object","properties":{"input":{"type":"string"}},"required":["input"],"additionalProperties":false}`)
	default:
		return fmt.Errorf("unsupported Responses tool type %q", tool.Type)
	}

	qualifiedName := responsesQualifiedToolName(namespace, name)
	target := responsesToolTarget{Type: tool.Type, Name: name, Namespace: namespace}
	if existing, exists := targets[qualifiedName]; exists && existing != target {
		return fmt.Errorf("Responses tool name collision for %q", qualifiedName)
	}
	if namespaceDescription != "" {
		if description == "" {
			description = namespaceDescription
		} else {
			description = namespaceDescription + "\n\n" + description
		}
	}
	targets[qualifiedName] = target
	chat.Tools = append(chat.Tools, openai.Tool{
		Type: "function",
		Function: openai.FunctionDecl{
			Name: qualifiedName, Description: description, Parameters: parameters,
		},
	})
	return nil
}

func responsesQualifiedToolName(namespace, name string) string {
	if namespace == "" {
		return name
	}
	return namespace + "." + name
}

func appendResponseToolCall(messages *[]openai.ChatMessage, call openai.ToolCall) {
	if len(*messages) > 0 && (*messages)[len(*messages)-1].Role == "assistant" {
		last := &(*messages)[len(*messages)-1]
		last.ToolCalls = append(last.ToolCalls, call)
		return
	}
	*messages = append(*messages, openai.ChatMessage{Role: "assistant", ToolCalls: []openai.ToolCall{call}})
}

func responsesText(raw json.RawMessage) (string, error) {
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
	var parts []responsesContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", err
	}
	var text strings.Builder
	for _, part := range parts {
		var value string
		switch part.Type {
		case "input_text", "output_text", "text":
			value = part.Text
		case "refusal":
			value = part.Refusal
		case "input_image":
			ref, err := responsesImageReference(part)
			if err != nil {
				return "", err
			}
			value = ref
		default:
			return "", fmt.Errorf("unsupported content part %q", part.Type)
		}
		if value != "" && text.Len() > 0 {
			text.WriteString("\n\n")
		}
		text.WriteString(value)
	}
	return text.String(), nil
}

func responsesInputContent(raw json.RawMessage) (string, []openai.AttachmentInput, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return "", nil, nil
	}
	if raw[0] == '"' {
		text, err := responsesText(raw)
		return text, nil, err
	}
	var parts []responsesContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", nil, err
	}
	var text strings.Builder
	var attachments []openai.AttachmentInput
	for _, part := range parts {
		switch part.Type {
		case "input_text", "output_text", "text":
			if part.Text != "" {
				if text.Len() > 0 {
					text.WriteString("\n\n")
				}
				text.WriteString(part.Text)
			}
		case "refusal":
			if part.Refusal != "" {
				if text.Len() > 0 {
					text.WriteString("\n\n")
				}
				text.WriteString(part.Refusal)
			}
		case "input_image":
			attachment, reference, err := responsesImageAttachment(part, len(attachments)+1)
			if err != nil {
				return "", nil, err
			}
			if attachment != nil {
				attachments = append(attachments, *attachment)
				if text.Len() > 0 {
					text.WriteString("\n\n")
				}
				text.WriteString("[image attached: " + attachment.Name + "]")
			} else if reference != "" {
				if text.Len() > 0 {
					text.WriteString("\n\n")
				}
				text.WriteString(reference)
			}
		default:
			return "", nil, fmt.Errorf("unsupported content part %q", part.Type)
		}
	}
	return text.String(), attachments, nil
}

func responsesImageAttachment(part responsesContentPart, index int) (*openai.AttachmentInput, string, error) {
	source := bytes.TrimSpace(part.ImageURL)
	if len(source) == 0 || bytes.Equal(source, []byte("null")) {
		source = bytes.TrimSpace(part.ImageURLCompat)
	}
	if len(source) == 0 || bytes.Equal(source, []byte("null")) {
		if part.FileID != "" {
			return nil, "", fmt.Errorf("input_image file_id %q is not supported; send image_url instead", part.FileID)
		}
		return nil, "", fmt.Errorf("input_image requires image_url or file_id")
	}
	value, err := responsesStringValue(source)
	if err != nil {
		return nil, "", fmt.Errorf("invalid input_image image_url: %w", err)
	}
	value = strings.TrimSpace(value)
	if strings.HasPrefix(strings.ToLower(value), "data:") {
		mimeType, data, err := decodeImageDataURL(value)
		if err != nil {
			return nil, "", err
		}
		name := part.Name
		if name == "" {
			name = fmt.Sprintf("image-%d.%s", index, imageExtension(mimeType))
		}
		return &openai.AttachmentInput{Name: name, MIMEType: mimeType, Data: data}, "", nil
	}
	if !isHTTPURL(value) {
		return nil, "", fmt.Errorf("input_image image_url must be an http(s) or data URL")
	}
	return nil, "[image URL provided but not uploaded]\n" + value, nil
}

func decodeImageDataURL(value string) (string, []byte, error) {
	comma := strings.IndexByte(value, ',')
	if comma <= len("data:") {
		return "", nil, fmt.Errorf("input_image data URL is malformed")
	}
	header := value[len("data:"):comma]
	mimeType := strings.SplitN(header, ";", 2)[0]
	if !strings.HasPrefix(strings.ToLower(mimeType), "image/") {
		return "", nil, fmt.Errorf("input_image data URL must contain an image MIME type")
	}
	encoded := value[comma+1:]
	var data []byte
	var err error
	if strings.Contains(strings.ToLower(header), ";base64") {
		data, err = base64.StdEncoding.DecodeString(encoded)
	} else {
		var decoded string
		decoded, err = url.PathUnescape(encoded)
		data = []byte(decoded)
	}
	if err != nil {
		return "", nil, fmt.Errorf("decode input_image data: %w", err)
	}
	if len(data) == 0 {
		return "", nil, fmt.Errorf("input_image data URL is empty")
	}
	if len(data) > 20<<20 {
		return "", nil, fmt.Errorf("input_image is larger than 20 MiB")
	}
	return mimeType, data, nil
}

func imageExtension(mimeType string) string {
	if i := strings.LastIndexByte(mimeType, '/'); i >= 0 && i+1 < len(mimeType) {
		return strings.ToLower(mimeType[i+1:])
	}
	return "bin"
}

// responsesImageReference keeps an input_image useful when the upstream todo
// API cannot receive OpenAI's multipart/image content directly. HTTP(S) URLs
// are passed through as a fetchable reference; data URLs are summarized so a
// large base64 payload does not consume the agent context. file_id needs an
// upload/attachment lookup that this gateway does not currently implement.
func responsesImageReference(part responsesContentPart) (string, error) {
	source := bytes.TrimSpace(part.ImageURL)
	if len(source) == 0 || bytes.Equal(source, []byte("null")) {
		source = bytes.TrimSpace(part.ImageURLCompat)
	}
	if len(source) > 0 && !bytes.Equal(source, []byte("null")) {
		value, err := responsesStringValue(source)
		if err != nil {
			return "", fmt.Errorf("invalid input_image image_url: %w", err)
		}
		value = strings.TrimSpace(value)
		if value == "" {
			return "", fmt.Errorf("input_image image_url must not be empty")
		}
		if strings.HasPrefix(strings.ToLower(value), "data:") {
			return dataURLImageReference(value)
		}
		if !isHTTPURL(value) {
			return "", fmt.Errorf("input_image image_url must be an http(s) or data URL")
		}
		if part.Detail != "" {
			return fmt.Sprintf("[input image detail=%s]\n%s", part.Detail, value), nil
		}
		return "[input image]\n" + value, nil
	}
	if part.FileID != "" {
		return "", fmt.Errorf("input_image file_id %q is not supported; send image_url instead", part.FileID)
	}
	return "", fmt.Errorf("input_image requires image_url or file_id")
}

func responsesStringValue(raw json.RawMessage) (string, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return "", nil
	}
	if raw[0] == '"' {
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return "", err
		}
		return value, nil
	}
	// Chat-style clients sometimes send image_url as {"url":"..."}.
	var object struct {
		URL    string `json:"url"`
		Detail string `json:"detail,omitempty"`
	}
	if err := json.Unmarshal(raw, &object); err != nil {
		return "", err
	}
	return object.URL, nil
}

func isHTTPURL(value string) bool {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" {
		return false
	}
	return strings.EqualFold(parsed.Scheme, "https") || strings.EqualFold(parsed.Scheme, "http")
}

func dataURLImageReference(value string) (string, error) {
	comma := strings.IndexByte(value, ',')
	if comma <= len("data:") {
		return "", fmt.Errorf("input_image data URL is malformed")
	}
	header := value[len("data:"):comma]
	mime := strings.SplitN(header, ";", 2)[0]
	if !strings.HasPrefix(strings.ToLower(mime), "image/") {
		return "", fmt.Errorf("input_image data URL must contain an image MIME type")
	}
	encoded := value[comma+1:]
	if strings.Contains(strings.ToLower(header), ";base64") {
		// Base64 is four characters per three bytes; this is only a size hint.
		approxBytes := (len(encoded) * 3) / 4
		return fmt.Sprintf("[input image data URL: %s, approximately %d bytes]", mime, approxBytes), nil
	}
	return fmt.Sprintf("[input image data URL: %s]", mime), nil
}

func responsesAgentMessageText(raw json.RawMessage) (string, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return "", nil
	}
	if raw[0] == '"' {
		return responsesText(raw)
	}
	var parts []responsesContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", err
	}
	var text strings.Builder
	for _, part := range parts {
		switch part.Type {
		case "input_text", "output_text", "text":
			text.WriteString(part.Text)
		case "encrypted_content":
			// Codex may retain opaque state beside the readable agent text.
		default:
			return "", fmt.Errorf("unsupported agent_message content part %q", part.Type)
		}
	}
	return text.String(), nil
}

func responsesString(raw json.RawMessage, fallback string) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return fallback
	}
	if raw[0] == '"' {
		var text string
		if err := json.Unmarshal(raw, &text); err == nil {
			return text
		}
		return fallback
	}
	return compactJSON(raw, fallback)
}

func buildResponsesResponse(req responsesRequest, reply *gateway.Reply, toolTargets map[string]responsesToolTarget) responsesResponse {
	now := time.Now()
	completedAt := now.Unix()
	model := req.Model
	if model == "" {
		model = reply.Model
	}
	metadata := make(map[string]any, len(req.Metadata)+1)
	for key, value := range req.Metadata {
		metadata[key] = value
	}
	metadata[openai.TodoIDMetadataKey] = reply.TodoID

	response := responsesResponse{
		ID: responseID(reply.TodoID, now), Object: "response",
		CreatedAt: now.Unix(), CompletedAt: &completedAt, Status: "completed",
		Model: model, PreviousResponseID: req.PreviousResponseID, Instructions: req.Instructions,
		Output: []responsesOutputItem{}, ParallelToolCalls: true,
		MaxOutputTokens: req.MaxOutputTokens, ToolChoice: "auto", Tools: append([]responsesTool{}, req.Tools...),
		Metadata: metadata, Reasoning: map[string]any{"effort": nil, "summary": nil},
		Store:      req.Store == nil || *req.Store,
		Text:       map[string]any{"format": map[string]string{"type": "text"}},
		Truncation: "disabled", Usage: responseUsage(reply.Usage),
	}
	if req.ParallelToolCalls != nil {
		response.ParallelToolCalls = *req.ParallelToolCalls
	}
	if len(bytes.TrimSpace(req.ToolChoice)) > 0 {
		var choice any
		if json.Unmarshal(req.ToolChoice, &choice) == nil {
			response.ToolChoice = choice
		}
	}
	if len(bytes.TrimSpace(req.Reasoning)) > 0 {
		var reasoning any
		if json.Unmarshal(req.Reasoning, &reasoning) == nil {
			response.Reasoning = reasoning
		}
	}
	if len(bytes.TrimSpace(req.Text)) > 0 {
		var textConfig any
		if json.Unmarshal(req.Text, &textConfig) == nil {
			response.Text = textConfig
		}
	}

	itemBase := now.UnixNano()
	if reply.Content != "" || !reply.IsToolCall() {
		response.OutputText = reply.Content
		response.Output = append(response.Output, responsesOutputItem{
			ID: fmt.Sprintf("msg_%d", itemBase), Type: "message", Status: "completed", Role: "assistant",
			Content: []responsesOutputContent{{
				Type: "output_text", Text: reply.Content, Annotations: []any{}, Logprobs: []any{},
			}},
		})
	}
	for i, call := range reply.ToolCalls {
		callID := call.ID
		if callID == "" {
			callID = fmt.Sprintf("call_%d", i)
		}
		target := responseToolTarget(call.Function.Name, toolTargets)
		item := responsesOutputItem{
			ID: fmt.Sprintf("fc_%d_%d", itemBase, i), Status: "completed",
			CallID: callID, Name: target.Name, Namespace: target.Namespace,
		}
		if target.Type == "custom" {
			item.Type = "custom_tool_call"
			item.Input = customToolInput(call.Function.Arguments)
		} else {
			item.Type = "function_call"
			item.Arguments = call.Function.Arguments
		}
		response.Output = append(response.Output, item)
	}
	return response
}

func responseUsage(usage gateway.TokenUsage) *responsesUsage {
	if !usage.Available {
		return nil
	}
	inputTokens := usage.InputTokens + usage.CacheReadTokens + usage.CacheWriteTokens
	return &responsesUsage{
		InputTokens: inputTokens,
		InputTokensDetails: responsesInputTokenDetails{
			CachedTokens: usage.CacheReadTokens,
		},
		OutputTokens:        usage.OutputTokens,
		OutputTokensDetails: responsesOutputTokenDetails{},
		TotalTokens:         inputTokens + usage.OutputTokens,
	}
}

func responseToolTarget(name string, targets map[string]responsesToolTarget) responsesToolTarget {
	if target, ok := targets[name]; ok {
		return target
	}
	if namespace, child, ok := strings.Cut(name, "."); ok && namespace != "" && child != "" {
		return responsesToolTarget{Type: "function", Name: child, Namespace: namespace}
	}
	return responsesToolTarget{Type: "function", Name: name}
}

func customToolInput(arguments string) string {
	var parsed struct {
		Input string `json:"input"`
	}
	if json.Unmarshal([]byte(arguments), &parsed) == nil && parsed.Input != "" {
		return parsed.Input
	}
	return arguments
}

func responseID(todoID string, now time.Time) string {
	return fmt.Sprintf("resp_%s_%d", todoID, now.UnixNano())
}

func todoIDFromResponseID(id string) (string, bool) {
	value := strings.TrimPrefix(id, "resp_")
	if value == id {
		return "", false
	}
	separator := strings.LastIndexByte(value, '_')
	if separator <= 0 || separator == len(value)-1 {
		return "", false
	}
	return value[:separator], true
}

func (s *Server) streamResponses(
	w http.ResponseWriter,
	flusher http.Flusher,
	ctx context.Context,
	req responsesRequest,
	chatReq openai.ChatRequest,
	toolTargets map[string]responsesToolTarget,
) {
	stream := &responsesSSE{
		w: w, flusher: flusher, request: req, toolTargets: toolTargets,
	}
	reply, err := s.gw.Stream(ctx, chatReq, stream.onGatewayEvent)
	if err != nil {
		if !stream.started {
			writeGatewayErr(w, err)
			return
		}
		_ = stream.emitError(err)
		return
	}
	_ = stream.finish(reply)
}

type responsesSSE struct {
	w           http.ResponseWriter
	flusher     http.Flusher
	request     responsesRequest
	toolTargets map[string]responsesToolTarget
	sequence    int
	started     bool
	response    responsesResponse
	messageID   string
	textStarted bool
	text        strings.Builder
}

func (s *responsesSSE) onGatewayEvent(event gateway.StreamEvent) error {
	switch event.Type {
	case gateway.StreamStart:
		return s.start(event.Model, event.TodoID)
	case gateway.StreamTextDelta:
		return s.textDelta(event.Delta)
	default:
		return fmt.Errorf("unsupported gateway stream event %q", event.Type)
	}
}

func (s *responsesSSE) start(model, todoID string) error {
	if s.started {
		return fmt.Errorf("Responses stream already started")
	}
	placeholder := &gateway.Reply{Model: model, TodoID: todoID}
	s.response = buildResponsesResponse(s.request, placeholder, s.toolTargets)
	if len(s.response.Output) > 0 {
		s.messageID = s.response.Output[0].ID
	}
	s.started = true
	setSSEHeaders(s.w)
	s.w.Header().Set(todoIDHeader, todoID)

	inProgress := s.response
	inProgress.CompletedAt = nil
	inProgress.Status = "in_progress"
	inProgress.Output = []responsesOutputItem{}
	inProgress.OutputText = ""
	inProgress.Usage = nil
	if err := s.emit("response.created", map[string]any{"response": inProgress}); err != nil {
		return err
	}
	return s.emit("response.in_progress", map[string]any{"response": inProgress})
}

func (s *responsesSSE) textDelta(delta string) error {
	if delta == "" {
		return nil
	}
	if err := s.startTextItem(); err != nil {
		return err
	}
	s.text.WriteString(delta)
	return s.emit("response.output_text.delta", map[string]any{
		"item_id": s.messageID, "output_index": 0,
		"content_index": 0, "delta": delta, "logprobs": []any{},
	})
}

func (s *responsesSSE) startTextItem() error {
	if s.textStarted {
		return nil
	}
	if !s.started {
		return fmt.Errorf("Responses stream has not started")
	}
	if s.messageID == "" {
		s.messageID = "msg_" + fmt.Sprint(time.Now().UnixNano())
	}
	s.textStarted = true
	if err := s.emit("response.output_item.added", map[string]any{
		"output_index": 0,
		"item": map[string]any{
			"id": s.messageID, "type": "message", "status": "in_progress",
			"role": "assistant", "content": []responsesOutputContent{},
		},
	}); err != nil {
		return err
	}
	part := responsesOutputContent{
		Type: "output_text", Text: "", Annotations: []any{}, Logprobs: []any{},
	}
	return s.emit("response.content_part.added", map[string]any{
		"item_id": s.messageID, "output_index": 0,
		"content_index": 0, "part": part,
	})
}

func (s *responsesSSE) finish(reply *gateway.Reply) error {
	streamReply := *reply
	streamReply.Content = s.text.String()
	response := buildResponsesResponse(s.request, &streamReply, s.toolTargets)
	response.ID = s.response.ID
	response.CreatedAt = s.response.CreatedAt

	for outputIndex := range response.Output {
		item := &response.Output[outputIndex]
		if item.Type == "message" {
			item.ID = s.messageID
			if err := s.finishTextItem(outputIndex, *item); err != nil {
				return err
			}
			continue
		}
		if err := s.emitCompletedToolItem(outputIndex, *item); err != nil {
			return err
		}
	}
	return s.emit("response.completed", map[string]any{"response": response})
}

func (s *responsesSSE) finishTextItem(outputIndex int, item responsesOutputItem) error {
	if err := s.startTextItem(); err != nil {
		return err
	}
	text := s.text.String()
	if err := s.emit("response.output_text.done", map[string]any{
		"item_id": item.ID, "output_index": outputIndex,
		"content_index": 0, "text": text, "logprobs": []any{},
	}); err != nil {
		return err
	}
	part := responsesOutputContent{
		Type: "output_text", Text: text, Annotations: []any{}, Logprobs: []any{},
	}
	if err := s.emit("response.content_part.done", map[string]any{
		"item_id": item.ID, "output_index": outputIndex,
		"content_index": 0, "part": part,
	}); err != nil {
		return err
	}
	return s.emit("response.output_item.done", map[string]any{
		"output_index": outputIndex, "item": item,
	})
}

func (s *responsesSSE) emitCompletedToolItem(outputIndex int, item responsesOutputItem) error {
	added := item
	added.Status = "in_progress"
	if item.Type == "function_call" {
		added.Arguments = ""
	} else {
		added.Input = ""
	}
	if err := s.emit("response.output_item.added", map[string]any{
		"output_index": outputIndex, "item": added,
	}); err != nil {
		return err
	}
	switch item.Type {
	case "function_call":
		if err := s.emit("response.function_call_arguments.delta", map[string]any{
			"item_id": item.ID, "output_index": outputIndex, "delta": item.Arguments,
		}); err != nil {
			return err
		}
		if err := s.emit("response.function_call_arguments.done", map[string]any{
			"item_id": item.ID, "output_index": outputIndex, "arguments": item.Arguments,
		}); err != nil {
			return err
		}
	case "custom_tool_call":
		if err := s.emit("response.custom_tool_call_input.delta", map[string]any{
			"item_id": item.ID, "output_index": outputIndex, "delta": item.Input,
		}); err != nil {
			return err
		}
		if err := s.emit("response.custom_tool_call_input.done", map[string]any{
			"item_id": item.ID, "output_index": outputIndex, "input": item.Input,
		}); err != nil {
			return err
		}
	}
	return s.emit("response.output_item.done", map[string]any{
		"output_index": outputIndex, "item": item,
	})
}

func (s *responsesSSE) emitError(streamErr error) error {
	return s.emit("error", map[string]any{
		"code": "server_error", "message": streamErr.Error(), "param": nil,
	})
}

func (s *responsesSSE) emit(eventType string, fields map[string]any) error {
	fields["type"] = eventType
	fields["sequence_number"] = s.sequence
	s.sequence++
	return emitResponsesEventE(s.w, s.flusher, eventType, fields)
}

func emitResponsesEventE(w http.ResponseWriter, flusher http.Flusher, event string, payload any) error {
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
