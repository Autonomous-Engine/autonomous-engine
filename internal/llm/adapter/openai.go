package adapter

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// openAIAdapter implements ModelAdapter for the OpenAI Chat Completions API.
// It is also compatible with vLLM, LM Studio, and other OpenAI-compatible servers.
type openAIAdapter struct {
	endpoint string
	apiKey   string
	model    string
	client   *http.Client
}

// NewOpenAI returns a ModelAdapter backed by the OpenAI Chat Completions API.
// If endpoint is empty it defaults to "https://api.openai.com/v1".
func NewOpenAI(endpoint, apiKey, model string) ModelAdapter {
	if endpoint == "" {
		endpoint = "https://api.openai.com/v1"
	}
	return &openAIAdapter{
		endpoint: strings.TrimRight(endpoint, "/"),
		apiKey:   apiKey,
		model:    model,
		client:   &http.Client{Timeout: 120 * time.Second},
	}
}

// ---- internal JSON types ----

type openAIMessage struct {
	Role       string           `json:"role"`
	Content    string           `json:"content"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	Name       string           `json:"name,omitempty"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
}

type openAIRequest struct {
	Model          string          `json:"model"`
	Messages       []openAIMessage `json:"messages"`
	MaxTokens      int             `json:"max_tokens,omitempty"`
	Temperature    float64         `json:"temperature,omitempty"`
	TopP           float64         `json:"top_p,omitempty"`
	Stream         bool            `json:"stream,omitempty"`
	Stop           []string        `json:"stop,omitempty"`
	ResponseFormat *openAIFormat   `json:"response_format,omitempty"`
	Tools          []openAITool    `json:"tools,omitempty"`
	ToolChoice     interface{}     `json:"tool_choice,omitempty"` // string or object
}

type openAIFormat struct {
	Type string `json:"type"`
}

type openAITool struct {
	Type     string          `json:"type"` // "function"
	Function openAIFunction  `json:"function"`
}

type openAIFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type openAIToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"` // "function"
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type openAIChoice struct {
	Message      *openAIMessage `json:"message,omitempty"`
	Delta        *openAIMessage `json:"delta,omitempty"`
	FinishReason string         `json:"finish_reason"`
}

type openAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

type openAIResponse struct {
	ID      string         `json:"id"`
	Model   string         `json:"model"`
	Choices []openAIChoice `json:"choices"`
	Usage   openAIUsage    `json:"usage"`
}

type openAIError struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// ---- ModelAdapter implementation ----

func (a *openAIAdapter) Name() string {
	return "openai/" + a.model
}

func (a *openAIAdapter) Capabilities() ModelCapabilities {
	return ModelCapabilities{
		MaxContextTokens:  128000,
		SupportsJSON:      true,
		SupportsStreaming: true,
		SupportsFunctions: true,
	}
}

func (a *openAIAdapter) Complete(ctx context.Context, req CompletionRequest) (*CompletionResponse, error) {
	body := a.buildRequest(req, false)
	respBody, statusCode, err := a.doPost(ctx, "/chat/completions", body)
	if err != nil {
		return nil, err
	}
	if statusCode != http.StatusOK {
		return nil, a.parseError(statusCode, respBody)
	}

	var oaResp openAIResponse
	if err := json.Unmarshal(respBody, &oaResp); err != nil {
		return nil, fmt.Errorf("openai: decode response: %w", err)
	}
	if len(oaResp.Choices) == 0 {
		return nil, fmt.Errorf("openai: empty choices in response")
	}

	content := ""
	if oaResp.Choices[0].Message != nil {
		content = oaResp.Choices[0].Message.Content
	}

	resp := &CompletionResponse{
		Content:      content,
		InputTokens:  oaResp.Usage.PromptTokens,
		OutputTokens: oaResp.Usage.CompletionTokens,
		Model:        oaResp.Model,
		FinishReason: oaResp.Choices[0].FinishReason,
	}

	// Extract tool calls from the response message
	if oaResp.Choices[0].Message != nil && len(oaResp.Choices[0].Message.ToolCalls) > 0 {
		resp.FinishReason = "tool_calls"
		for _, tc := range oaResp.Choices[0].Message.ToolCalls {
			resp.ToolCalls = append(resp.ToolCalls, ToolCall{
				ID:        tc.ID,
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			})
		}
	}

	return resp, nil
}

func (a *openAIAdapter) Stream(ctx context.Context, req CompletionRequest) (<-chan StreamChunk, error) {
	body := a.buildRequest(req, true)
	httpReq, err := a.newRequest(ctx, "/chat/completions", body)
	if err != nil {
		return nil, err
	}

	resp, err := a.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openai: stream request: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, a.parseError(resp.StatusCode, b)
	}

	ch := make(chan StreamChunk, 32)
	go func() {
		defer close(ch)
		defer resp.Body.Close()

		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := strings.TrimPrefix(line, "data: ")
			if data == "[DONE]" {
				ch <- StreamChunk{Done: true}
				return
			}
			var chunk openAIResponse
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				continue
			}
			if len(chunk.Choices) == 0 {
				continue
			}
			delta := ""
			if chunk.Choices[0].Delta != nil {
				delta = chunk.Choices[0].Delta.Content
			}
			sc := StreamChunk{Content: delta}
			if chunk.Choices[0].FinishReason != "" {
				sc.Done = true
				sc.InputTokens = chunk.Usage.PromptTokens
				sc.OutputTokens = chunk.Usage.CompletionTokens
			}
			select {
			case ch <- sc:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, nil
}

func (a *openAIAdapter) HealthCheck(ctx context.Context) error {
	// GET /models is a free lightweight endpoint (no token cost). The
	// previous implementation fired a Completion with max_tokens=5 which
	// burned quota on every /health probe — noisy and expensive once the
	// health endpoint is hit on a timer by Docker / Kubernetes.
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(pingCtx, http.MethodGet, a.endpoint+"/models", nil)
	if err != nil {
		return fmt.Errorf("openai: health check request: %w", err)
	}
	if a.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+a.apiKey)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("openai: health check: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	// 200 = key valid and API reachable. 401 = auth misconfigured (surface).
	// 404 on a self-hosted OpenAI-compatible server that lacks /models is
	// also acceptable liveness — the server is reachable, even if the
	// endpoint is un-routed.
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNotFound {
		return nil
	}
	return fmt.Errorf("openai: health check status %d", resp.StatusCode)
}

// ---- helpers ----

func (a *openAIAdapter) buildRequest(req CompletionRequest, stream bool) openAIRequest {
	msgs := make([]openAIMessage, 0, len(req.Messages))

	// Prepend any tool results as role=tool messages before the final user message
	for _, m := range req.Messages {
		msg := openAIMessage{
			Role:       string(m.Role),
			Content:    m.Content,
			ToolCallID: m.ToolCallID,
			Name:       m.Name,
		}
		if m.Role == RoleAssistant && len(m.ToolCalls) > 0 {
			for _, tc := range m.ToolCalls {
				msg.ToolCalls = append(msg.ToolCalls, openAIToolCall{
					ID:   tc.ID,
					Type: "function",
					Function: struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					}{
						Name:      tc.Name,
						Arguments: tc.Arguments,
					},
				})
			}
		}
		msgs = append(msgs, msg)
	}

	// Append tool results that haven't been embedded into the messages list yet
	for _, tr := range req.ToolResults {
		msgs = append(msgs, openAIMessage{
			Role:       "tool",
			Content:    tr.Content,
			ToolCallID: tr.ToolCallID,
		})
	}

	r := openAIRequest{
		Model:       a.model,
		Messages:    msgs,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stream:      stream,
		Stop:        req.Stop,
	}
	if req.JSONMode {
		r.ResponseFormat = &openAIFormat{Type: "json_object"}
	}

	// Convert tools
	if len(req.Tools) > 0 {
		r.Tools = make([]openAITool, len(req.Tools))
		for i, t := range req.Tools {
			r.Tools[i] = openAITool{
				Type: "function",
				Function: openAIFunction{
					Name:        t.Name,
					Description: t.Description,
					Parameters:  t.Parameters,
				},
			}
		}
	}

	// Pass through tool choice
	if req.ToolChoice != "" {
		r.ToolChoice = req.ToolChoice
	}

	return r
}

func (a *openAIAdapter) newRequest(ctx context.Context, path string, body interface{}) (*http.Request, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("openai: marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint+path, bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("openai: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if a.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+a.apiKey)
	}
	return req, nil
}

func (a *openAIAdapter) doPost(ctx context.Context, path string, body interface{}) ([]byte, int, error) {
	req, err := a.newRequest(ctx, path, body)
	if err != nil {
		return nil, 0, err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("openai: http: %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("openai: read body: %w", err)
	}
	return b, resp.StatusCode, nil
}

func (a *openAIAdapter) parseError(statusCode int, body []byte) error {
	var oaErr openAIError
	if json.Unmarshal(body, &oaErr) == nil && oaErr.Error.Message != "" {
		return fmt.Errorf("openai: status %d: %s", statusCode, oaErr.Error.Message)
	}
	return fmt.Errorf("openai: status %d: %s", statusCode, string(body))
}
