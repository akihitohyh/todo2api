package openai

import (
	"bytes"
	"encoding/json"
	"fmt"
)

type ChatRequest struct {
	Model         string            `json:"model"`
	Messages      []ChatMessage     `json:"messages"`
	Stream        bool              `json:"stream"`
	StreamOptions *StreamOptions    `json:"stream_options,omitempty"`
	Tools         []Tool            `json:"tools,omitempty"`
	Metadata      map[string]string `json:"metadata,omitempty"`
	System        string            `json:"system,omitempty"` // Anthropic-style system parameter
	Attachments   []AttachmentInput `json:"-"`
}

// AttachmentInput contains binary data extracted from a multimodal request.
type AttachmentInput struct {
	Name     string
	MIMEType string
	Data     []byte
}

type StreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

type Tool struct {
	Type     string       `json:"type"` // "function"
	Function FunctionDecl `json:"function"`
}

type FunctionDecl struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type ChatMessage struct {
	Role string `json:"role"` // system|user|assistant|tool
	// Content is a string for most roles. For tool results it's the output.
	Content string `json:"content"`
	// Assistant tool-call turns.
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	// For role == "tool": which call this result answers.
	ToolCallID string `json:"tool_call_id,omitempty"`
	Name       string `json:"name,omitempty"`
	// Parts holds the raw OpenAI content parts when the request used the array
	// content form. The transport layer renders them into Content text and
	// request attachments; it is never serialized back out.
	Parts []ContentPart `json:"-"`
}

// ContentPart is one element of an OpenAI content-parts array, e.g.
// [{"type":"text","text":"..."},{"type":"image_url","image_url":{"url":"data:image/png;base64,..."}}].
// Relays commonly convert Anthropic content blocks into this form.
type ContentPart struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	ImageURL json.RawMessage `json:"image_url,omitempty"`
}

// UnmarshalJSON accepts both plain string content and the OpenAI content-parts
// array form. String content is kept as-is; array content is retained in Parts
// for the transport layer to render (text joins, image attachments).
func (m *ChatMessage) UnmarshalJSON(data []byte) error {
	var raw struct {
		Role       string          `json:"role"`
		Content    json.RawMessage `json:"content"`
		ToolCalls  []ToolCall      `json:"tool_calls"`
		ToolCallID string          `json:"tool_call_id"`
		Name       string          `json:"name"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	m.Role = raw.Role
	m.ToolCalls = raw.ToolCalls
	m.ToolCallID = raw.ToolCallID
	m.Name = raw.Name
	content := bytes.TrimSpace(raw.Content)
	if len(content) == 0 || bytes.Equal(content, []byte("null")) {
		return nil
	}
	if content[0] == '"' {
		return json.Unmarshal(content, &m.Content)
	}
	if content[0] != '[' {
		return fmt.Errorf("content must be a string or an array of content parts")
	}
	return json.Unmarshal(content, &m.Parts)
}

type ToolCall struct {
	ID       string       `json:"id"`
	Index    *int         `json:"index,omitempty"`
	Type     string       `json:"type"` // "function"
	Function FunctionCall `json:"function"`
}

type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON-encoded string
}

type ChatResponse struct {
	ID       string            `json:"id"`
	Object   string            `json:"object"`
	Created  int64             `json:"created"`
	Model    string            `json:"model"`
	Choices  []Choice          `json:"choices"`
	Usage    *Usage            `json:"usage,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

const TodoIDMetadataKey = "todo2api.todo_id"

type Choice struct {
	Index        int          `json:"index"`
	Message      *ChatMessage `json:"message,omitempty"`
	Delta        *Delta       `json:"delta,omitempty"`
	FinishReason *string      `json:"finish_reason"`
}

type Delta struct {
	Role      string     `json:"role,omitempty"`
	Content   string     `json:"content,omitempty"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

type Usage struct {
	PromptTokens            int                      `json:"prompt_tokens"`
	CompletionTokens        int                      `json:"completion_tokens"`
	TotalTokens             int                      `json:"total_tokens"`
	PromptTokensDetails     *PromptTokensDetails     `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *CompletionTokensDetails `json:"completion_tokens_details,omitempty"`
}

type PromptTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

type CompletionTokensDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

type ModelList struct {
	Object string  `json:"object"`
	Data   []Model `json:"data"`
}

type Model struct {
	ID                  string `json:"id"`
	Object              string `json:"object"`
	Created             int64  `json:"created,omitempty"`
	OwnedBy             string `json:"owned_by"`
	Name                string `json:"name,omitempty"`
	ContextLength       int64  `json:"context_length,omitempty"`
	MaxCompletionTokens int64  `json:"max_completion_tokens,omitempty"`
}

type APIError struct {
	Error ErrorBody `json:"error"`
}

type ErrorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
}
