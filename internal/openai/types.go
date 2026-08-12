package openai

import "encoding/json"

type ChatRequest struct {
	Model         string            `json:"model"`
	Messages      []ChatMessage     `json:"messages"`
	Stream        bool              `json:"stream"`
	StreamOptions *StreamOptions    `json:"stream_options,omitempty"`
	Tools         []Tool            `json:"tools,omitempty"`
	Metadata      map[string]string `json:"metadata,omitempty"`
	System        string            `json:"system,omitempty"` // Anthropic-style system parameter
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
