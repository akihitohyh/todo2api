package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

// HTTPError preserves an error response from the todofor.ai API so callers
// can distinguish account failures from malformed requests.
type HTTPError struct {
	Method     string
	Path       string
	StatusCode int
	Message    string
	Code       string
	Body       string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("upstream %s %s: %d %s", e.Method, e.Path, e.StatusCode, e.Body)
}

type errorResponse struct {
	Message string `json:"message"`
	Code    string `json:"code"`
}

type errorEnvelope struct {
	Message string         `json:"message"`
	Code    string         `json:"code"`
	Error   *errorResponse `json:"error"`
}

func newHTTPError(method, path string, statusCode int, data []byte) *HTTPError {
	body := strings.TrimSpace(string(data))
	var response errorEnvelope
	_ = json.Unmarshal(data, &response)
	message, code := response.Message, response.Code
	if response.Error != nil {
		if response.Error.Message != "" {
			message = response.Error.Message
		}
		if response.Error.Code != "" {
			code = response.Error.Code
		}
	}
	return &HTTPError{
		Method: method, Path: path, StatusCode: statusCode,
		Message: message, Code: code, Body: body,
	}
}

// Block is a message content block (text / tool / bash ...).
type Block struct {
	Type    string `json:"type"`
	Content string `json:"content"`
	Result  string `json:"result"`
	Status  string `json:"status"`
}

// AttachmentFrame is the metadata reference accepted by todo requests.
type AttachmentFrame struct {
	ID           string `json:"id"`
	URI          string `json:"uri"`
	OriginalName string `json:"originalName"`
	MIMEType     string `json:"mimeType"`
	FileSize     int64  `json:"fileSize"`
	CreatedAt    int64  `json:"createdAt,omitempty"`
	IsPublic     bool   `json:"isPublic,omitempty"`
	Status       string `json:"status,omitempty"`
}

type AttachmentUpload struct {
	Name     string
	MIMEType string
	Data     []byte
}

type registerAttachmentResp struct {
	AttachmentID string `json:"attachmentId"`
	ID           string `json:"id"`
	URI          string `json:"uri"`
	OriginalName string `json:"originalName"`
	MIMEType     string `json:"mimeType"`
	FileSize     int64  `json:"fileSize"`
	CreatedAt    int64  `json:"createdAt"`
	IsPublic     bool   `json:"isPublic"`
	Status       string `json:"status"`
}

// RegisterAttachment uploads bytes through the attachment endpoint used by
// the todofor.ai web client and returns the frame used in todo requests.
func (c *Client) RegisterAttachment(ctx context.Context, upload AttachmentUpload, todoID string) (AttachmentFrame, error) {
	if len(upload.Data) == 0 {
		return AttachmentFrame{}, fmt.Errorf("attachment %q is empty", upload.Name)
	}
	name := filepath.Base(strings.TrimSpace(upload.Name))
	if name == "." || name == "" {
		name = "attachment"
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", name)
	if err != nil {
		return AttachmentFrame{}, err
	}
	if _, err := part.Write(upload.Data); err != nil {
		return AttachmentFrame{}, err
	}
	if upload.MIMEType != "" {
		_ = writer.WriteField("mimeType", upload.MIMEType)
	}
	if todoID != "" {
		_ = writer.WriteField("todoId", todoID)
	}
	if err := writer.Close(); err != nil {
		return AttachmentFrame{}, err
	}
	path := "/resources/register"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, &body)
	if err != nil {
		return AttachmentFrame{}, err
	}
	req.Header.Set("X-API-Key", c.apiKey)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := c.http.Do(req)
	if err != nil {
		return AttachmentFrame{}, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return AttachmentFrame{}, err
	}
	if resp.StatusCode >= 300 {
		return AttachmentFrame{}, newHTTPError(http.MethodPost, path, resp.StatusCode, data)
	}
	var result registerAttachmentResp
	if err := json.Unmarshal(data, &result); err != nil {
		return AttachmentFrame{}, fmt.Errorf("decode registered attachment: %w", err)
	}
	if result.ID == "" {
		result.ID = result.AttachmentID
	}
	if result.ID == "" || result.URI == "" {
		return AttachmentFrame{}, fmt.Errorf("upstream returned incomplete attachment metadata")
	}
	if result.OriginalName == "" {
		result.OriginalName = name
	}
	if result.MIMEType == "" {
		result.MIMEType = upload.MIMEType
	}
	if result.FileSize == 0 {
		result.FileSize = int64(len(upload.Data))
	}
	return AttachmentFrame{ID: result.ID, URI: result.URI, OriginalName: result.OriginalName, MIMEType: result.MIMEType, FileSize: result.FileSize, CreatedAt: result.CreatedAt, IsPublic: result.IsPublic, Status: result.Status}, nil
}

// RunMeta records one measured operation attached to an upstream message.
type RunMeta struct {
	Cost   float64       `json:"cost"`
	Type   string        `json:"type"`
	Extras RunMetaExtras `json:"extras"`
}

// BillingUsage is the authenticated account's current billing state.
type BillingUsage struct {
	TotalBalance              float64      `json:"totalBalance"`
	ManualBalance             float64      `json:"manualBalance"`
	SubscriptionBalance       float64      `json:"subscriptionBalance"`
	Tier                      string       `json:"tier"`
	HasActivePaidSubscription bool         `json:"hasActivePaidSubscription"`
	CreditLimit               float64      `json:"creditLimit"`
	MonthlyTopUp              *float64     `json:"monthlyTopUp"`
	Session                   *UsageWindow `json:"session"`
	Weekly                    *UsageWindow `json:"weekly"`
}

type UsageWindow struct {
	UsedPercent float64 `json:"usedPercent"`
	ResetsAt    float64 `json:"resetsAt"`
}

func (c *Client) BillingUsage(ctx context.Context) (BillingUsage, error) {
	var usage BillingUsage
	if err := c.do(ctx, http.MethodGet, "/billing/usage", nil, &usage); err != nil {
		return BillingUsage{}, err
	}
	return usage, nil
}

// RunMetaExtras contains the token counters reported for an AI operation.
type RunMetaExtras struct {
	Model            string `json:"model"`
	InputTokens      int    `json:"inputTokens"`
	OutputTokens     int    `json:"outputTokens"`
	CacheReadTokens  int    `json:"cacheReadTokens"`
	CacheWriteTokens int    `json:"cacheWriteTokens"`
	ContextTokens    int    `json:"contextTokens"`
}

// AddMessage resumes an existing todo with a user/tool-result message.
func (c *Client) AddMessage(ctx context.Context, projectID, todoID, content string, agent AgentSettings, filteredTools ...FilteredEdgeTools) (*Todo, error) {
	return c.AddMessageWithAttachments(ctx, projectID, todoID, content, agent, nil, filteredTools...)
}

func (c *Client) AddMessageWithAttachments(ctx context.Context, projectID, todoID, content string, agent AgentSettings, attachments []AttachmentFrame, filteredTools ...FilteredEdgeTools) (*Todo, error) {
	body := createTodoReq{
		TodoID:        todoID,
		ProjectID:     projectID,
		Content:       content,
		AgentSettings: agent,
		FilteredTools: firstFilteredTools(filteredTools),
		Attachments:   attachments,
	}
	var todo Todo
	path := fmt.Sprintf("/projects/%s/todos", url.PathEscape(projectID))
	if err := c.do(ctx, http.MethodPost, path, body, &todo); err != nil {
		return nil, err
	}
	return &todo, nil
}

type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// ModelInfo is one model advertised by the upstream OpenAI-compatible proxy.
type ModelInfo struct {
	ID                  string `json:"id"`
	Object              string `json:"object"`
	Created             int64  `json:"created"`
	OwnedBy             string `json:"owned_by"`
	Name                string `json:"name,omitempty"`
	ContextLength       int64  `json:"context_length,omitempty"`
	MaxCompletionTokens int64  `json:"max_completion_tokens,omitempty"`
}

type modelListResp struct {
	Object string      `json:"object"`
	Data   []ModelInfo `json:"data"`
}

// All account clients target the same upstream host. Sharing one transport
// keeps connection limits meaningful when the pool contains thousands of
// keys; one transport per key otherwise multiplies these limits by the number
// of accounts and leaves a large number of idle upstream connections open.
var sharedHTTPTransport = &http.Transport{
	MaxIdleConns:          64,
	MaxIdleConnsPerHost:   32,
	MaxConnsPerHost:       64,
	IdleConnTimeout:       90 * time.Second,
	TLSHandshakeTimeout:   10 * time.Second,
	ExpectContinueTimeout: 1 * time.Second,
	DisableCompression:    false,
	ForceAttemptHTTP2:     true,
}

func New(baseURL, apiKey string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		http: &http.Client{
			Timeout:   30 * time.Second,
			Transport: sharedHTTPTransport,
		},
	}
}

// Models returns the model catalog used by the official todofor.ai CLI.
func (c *Client) Models(ctx context.Context) ([]ModelInfo, error) {
	var response modelListResp
	if err := c.do(ctx, http.MethodGet, "/models", nil, &response); err != nil {
		return nil, err
	}
	return response.Data, nil
}

// RunnerModelID converts an OpenAI proxy model ID into the provider-qualified
// value expected by AgentSettings. Local IDs without a slash remain unchanged.
func RunnerModelID(modelID string) string {
	provider, _, ok := strings.Cut(modelID, "/")
	if !ok || provider == "" || strings.Contains(provider, ":") {
		return modelID
	}
	return provider + ":" + modelID
}

type AgentSettings struct {
	ID                string                    `json:"id,omitempty"`
	Name              string                    `json:"name,omitempty"`
	OwnerID           string                    `json:"ownerId,omitempty"`
	Model             string                    `json:"model,omitempty"`
	SystemMessage     string                    `json:"systemMessage,omitempty"`
	SystemMessageMode string                    `json:"systemMessageMode,omitempty"`
	ThinkingLevel     string                    `json:"thinkingLevel,omitempty"`
	Temperature       *float64                  `json:"temperature,omitempty"`
	SmartSystemPrompt *bool                     `json:"smartSystemPrompt,omitempty"`
	MCPConfigs        map[string]any            `json:"mcpConfigs"`
	EdgesMCPConfigs   map[string]map[string]any `json:"edgesMcpConfigs"`
	DevicesConfig     map[string]any            `json:"devicesConfig,omitempty"`
	Permissions       *ToolPermissions          `json:"permissions,omitempty"`
	SpecID            string                    `json:"specId,omitempty"`
	Color             string                    `json:"color,omitempty"`
	CreatedAt         int64                     `json:"createdAt,omitempty"`
	UpdatedAt         int64                     `json:"updatedAt,omitempty"`
}

type ToolPermissions struct {
	Allow []string `json:"allow"`
	Ask   []string `json:"ask,omitempty"`
	Deny  []string `json:"deny,omitempty"`
}

type createTodoReq struct {
	TodoID        string            `json:"todoId,omitempty"`
	ProjectID     string            `json:"projectId"`
	Content       string            `json:"content"`
	AgentSettings AgentSettings     `json:"agentSettings"`
	FilteredTools FilteredEdgeTools `json:"filteredEdgeTools,omitempty"`
	Attachments   []AttachmentFrame `json:"attachments,omitempty"`
	AutoDone      bool              `json:"autoDone,omitempty"`
}

type Todo struct {
	ID        string `json:"id"`
	ProjectID string `json:"projectId"`
	Status    string `json:"status"`
}

type Message struct {
	ID        string    `json:"id"`
	Role      string    `json:"role"`
	Content   string    `json:"content"`
	CreatedAt int64     `json:"createdAt"`
	Blocks    []Block   `json:"blocks"`
	RunMeta   []RunMeta `json:"runMeta"`
}

type messagesResp struct {
	Messages []Message `json:"messages"`
	HasMore  bool      `json:"hasMore"`
}

type Project struct {
	ID string `json:"id"`
}

type projectListItem struct {
	Project Project `json:"project"`
	ID      string  `json:"id"` // Backward compatibility with older flat responses.
}

// Agent returns a specific saved AgentSettings template.
func (c *Client) Agent(ctx context.Context, agentID string) (AgentSettings, error) {
	var agent AgentSettings
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/agents/%s", url.PathEscape(agentID)), nil, &agent); err != nil {
		return AgentSettings{}, err
	}
	return agent, nil
}

// FirstAgent returns the account's first saved AgentSettings template.
func (c *Client) FirstAgent(ctx context.Context) (AgentSettings, error) {
	var agents []AgentSettings
	if err := c.do(ctx, http.MethodGet, "/agents", nil, &agents); err != nil {
		return AgentSettings{}, err
	}
	if len(agents) == 0 {
		return AgentSettings{}, fmt.Errorf("account has no agent settings")
	}
	return agents[0], nil
}

// FirstProject returns the account's first project id.
func (c *Client) FirstProject(ctx context.Context) (string, error) {
	var projects []projectListItem
	if err := c.do(ctx, http.MethodGet, "/projects", nil, &projects); err != nil {
		return "", err
	}
	if len(projects) == 0 {
		return "", fmt.Errorf("account has no projects")
	}
	id := projects[0].Project.ID
	if id == "" {
		id = projects[0].ID
	}
	if id == "" {
		return "", fmt.Errorf("account's first project has no id")
	}
	return id, nil
}

// CreateTodo starts a new conversation and returns the created todo.
func (c *Client) CreateTodo(ctx context.Context, projectID, content string, agent AgentSettings, filteredTools ...FilteredEdgeTools) (*Todo, error) {
	return c.CreateTodoWithAttachments(ctx, projectID, content, agent, nil, filteredTools...)
}

func (c *Client) CreateTodoWithAttachments(ctx context.Context, projectID, content string, agent AgentSettings, attachments []AttachmentFrame, filteredTools ...FilteredEdgeTools) (*Todo, error) {
	body := createTodoReq{
		ProjectID:     projectID,
		Content:       content,
		AgentSettings: agent,
		FilteredTools: firstFilteredTools(filteredTools),
		Attachments:   attachments,
	}
	var todo Todo
	path := fmt.Sprintf("/projects/%s/todos", url.PathEscape(projectID))
	if err := c.do(ctx, http.MethodPost, path, body, &todo); err != nil {
		return nil, err
	}
	return &todo, nil
}

// Messages fetches the message list of a todo.
func (c *Client) Messages(ctx context.Context, todoID string) ([]Message, error) {
	var resp messagesResp
	path := fmt.Sprintf("/todos/%s/messages", url.PathEscape(todoID))
	if err := c.do(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Messages, nil
}

// GetTodo returns the current run status. It is used as a REST fallback when
// the frontend WebSocket misses a terminal todo:status event.
func (c *Client) GetTodo(ctx context.Context, todoID string) (*Todo, error) {
	var todo Todo
	path := fmt.Sprintf("/todos/%s", url.PathEscape(todoID))
	if err := c.do(ctx, http.MethodGet, path, nil, &todo); err != nil {
		return nil, err
	}
	return &todo, nil
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("X-API-Key", c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return newHTTPError(method, path, resp.StatusCode, data)
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

func firstFilteredTools(tools []FilteredEdgeTools) FilteredEdgeTools {
	if len(tools) == 0 {
		return nil
	}
	return tools[0]
}
