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

const anthropicVersion = "2023-06-01"

// anthropicAdapter implements ModelAdapter for the Anthropic Messages API.
type anthropicAdapter struct {
	endpoint string
	apiKey   string
	model    string
	client   *http.Client
}

// NewAnthropic returns a ModelAdapter backed by the Anthropic Messages API.
// If endpoint is empty it defaults to "https://api.anthropic.com".
func NewAnthropic(endpoint, apiKey, model string) ModelAdapter {
	if endpoint == "" {
		endpoint = "https://api.anthropic.com"
	}
	return &anthropicAdapter{
		endpoint: strings.TrimRight(endpoint, "/"),
		apiKey:   apiKey,
		model:    model,
		client:   &http.Client{Timeout: 120 * time.Second},
	}
}

// ---- internal JSON types ----

// anthropicMessageContent can be either a plain string or a slice of content blocks.
// We use json.RawMessage so we can marshal either form.
type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string or []anthropicContentBlock
}

type anthropicRequest struct {
	Model     string             `json:"model"`
	Messages  []anthropicMessage `json:"messages"`
	System    string             `json:"system,omitempty"`
	MaxTokens int                `json:"max_tokens"`
	Stream    bool               `json:"stream,omitempty"`
	Stop      []string           `json:"stop_sequences,omitempty"`
	Tools     []anthropicTool    `json:"tools,omitempty"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// anthropicContentBlock covers both text and tool_use (request) and tool_result (response) blocks.
type anthropicContentBlock struct {
	Type       string          `json:"type"`                  // "text", "tool_use", "tool_result"
	Text       string          `json:"text,omitempty"`        // type=text
	ID         string          `json:"id,omitempty"`          // type=tool_use
	Name       string          `json:"name,omitempty"`        // type=tool_use
	Input      json.RawMessage `json:"input,omitempty"`       // type=tool_use
	ToolUseID  string          `json:"tool_use_id,omitempty"` // type=tool_result
	Content    string          `json:"content,omitempty"`     // type=tool_result
	IsError    bool            `json:"is_error,omitempty"`    // type=tool_result
}

type anthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type anthropicResponse struct {
	ID         string                  `json:"id"`
	Model      string                  `json:"model"`
	Content    []anthropicContentBlock `json:"content"`
	Usage      anthropicUsage          `json:"usage"`
	StopReason string                  `json:"stop_reason"`
}

type anthropicError struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// SSE event types for Anthropic streaming
type anthropicDelta struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type anthropicStreamEvent struct {
	Type  string          `json:"type"`
	Index int             `json:"index,omitempty"`
	Delta *anthropicDelta `json:"delta,omitempty"`
	Usage *anthropicUsage `json:"usage,omitempty"`
}

// ---- ModelAdapter implementation ----

func (a *anthropicAdapter) Name() string {
	return "anthropic/" + a.model
}

func (a *anthropicAdapter) Capabilities() ModelCapabilities {
	return ModelCapabilities{
		MaxContextTokens:  200000,
		SupportsJSON:      true,
		SupportsStreaming: true,
		SupportsFunctions: true,
	}
}

func (a *anthropicAdapter) Complete(ctx context.Context, req CompletionRequest) (*CompletionResponse, error) {
	antReq := a.buildRequest(req, false)

	var respBody []byte
	var statusCode int
	var err error

	for _, delay := range []time.Duration{0, 15 * time.Second, 30 * time.Second, 60 * time.Second} {
		if delay > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}
		respBody, statusCode, err = a.doPost(ctx, "/v1/messages", antReq)
		if err != nil {
			return nil, err
		}
		if statusCode != http.StatusTooManyRequests {
			break
		}
	}
	if statusCode != http.StatusOK {
		return nil, a.parseError(statusCode, respBody)
	}

	var antResp anthropicResponse
	if err := json.Unmarshal(respBody, &antResp); err != nil {
		return nil, fmt.Errorf("anthropic: decode response: %w", err)
	}

	content := ""
	var toolCalls []ToolCall
	for _, block := range antResp.Content {
		switch block.Type {
		case "text":
			content += block.Text
		case "tool_use":
			// Marshal the Input back to a JSON string for the ToolCall Arguments field
			args := ""
			if block.Input != nil {
				args = string(block.Input)
			}
			toolCalls = append(toolCalls, ToolCall{
				ID:        block.ID,
				Name:      block.Name,
				Arguments: args,
			})
		}
	}

	finishReason := antResp.StopReason
	if len(toolCalls) > 0 {
		finishReason = "tool_calls"
	}

	return &CompletionResponse{
		Content:      content,
		InputTokens:  antResp.Usage.InputTokens,
		OutputTokens: antResp.Usage.OutputTokens,
		Model:        antResp.Model,
		FinishReason: finishReason,
		ToolCalls:    toolCalls,
	}, nil
}

func (a *anthropicAdapter) Stream(ctx context.Context, req CompletionRequest) (<-chan StreamChunk, error) {
	antReq := a.buildRequest(req, true)
	httpReq, err := a.newRequest(ctx, "/v1/messages", antReq)
	if err != nil {
		return nil, err
	}

	resp, err := a.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("anthropic: stream request: %w", err)
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
		var inputTokens, outputTokens int

		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := strings.TrimPrefix(line, "data: ")
			var event anthropicStreamEvent
			if err := json.Unmarshal([]byte(data), &event); err != nil {
				continue
			}

			switch event.Type {
			case "content_block_delta":
				if event.Delta != nil && event.Delta.Type == "text_delta" {
					select {
					case ch <- StreamChunk{Content: event.Delta.Text}:
					case <-ctx.Done():
						return
					}
				}
			case "message_delta":
				if event.Usage != nil {
					outputTokens = event.Usage.OutputTokens
				}
			case "message_start":
				if event.Usage != nil {
					inputTokens = event.Usage.InputTokens
				}
			case "message_stop":
				select {
				case ch <- StreamChunk{Done: true, InputTokens: inputTokens, OutputTokens: outputTokens}:
				case <-ctx.Done():
				}
				return
			}
		}
	}()
	return ch, nil
}

func (a *anthropicAdapter) HealthCheck(ctx context.Context) error {
	// Anthropic has no free "list models" or "auth check" endpoint, so we
	// deliberately do NOT burn tokens on every /health probe. Instead,
	// verify that (a) an API key is configured and (b) the endpoint host
	// is reachable with a cheap GET — we accept any non-network-level
	// response (even 404/405) as "Anthropic is reachable". If the operator
	// needs an authenticated probe they can use the Complete path directly
	// via the existing callback machinery.
	if strings.TrimSpace(a.apiKey) == "" {
		return fmt.Errorf("anthropic: no API key configured")
	}
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(pingCtx, http.MethodGet, a.endpoint+"/v1/messages", nil)
	if err != nil {
		return fmt.Errorf("anthropic: health check request: %w", err)
	}
	// Anthropic returns 405 Method Not Allowed on GET /v1/messages; that's
	// "the host is up and speaking HTTP" which is all we need.
	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("anthropic: health check: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	return nil
}

// ---- helpers ----

func (a *anthropicAdapter) buildRequest(req CompletionRequest, stream bool) anthropicRequest {
	var system string
	var msgs []anthropicMessage

	for _, m := range req.Messages {
		if m.Role == RoleSystem {
			if system != "" {
				system += "\n"
			}
			system += m.Content
			continue
		}

		if m.Role == RoleTool {
			// Tool result messages: Anthropic expects role=user with a tool_result content block.
			// Group consecutive tool messages under a single user turn.
			block := anthropicContentBlock{
				Type:      "tool_result",
				ToolUseID: m.ToolCallID,
				Content:   m.Content,
			}
			blockBytes, _ := json.Marshal([]anthropicContentBlock{block})

			// If the last message is already a user turn containing tool_result blocks, append to it.
			if len(msgs) > 0 && msgs[len(msgs)-1].Role == "user" {
				var existing []anthropicContentBlock
				if err := json.Unmarshal(msgs[len(msgs)-1].Content, &existing); err == nil {
					// Only merge if existing blocks are tool_result blocks (not plain text)
					if len(existing) > 0 && existing[0].Type == "tool_result" {
						existing = append(existing, block)
						merged, _ := json.Marshal(existing)
						msgs[len(msgs)-1].Content = merged
						continue
					}
				}
			}

			msgs = append(msgs, anthropicMessage{
				Role:    "user",
				Content: blockBytes,
			})
			continue
		}

		// Regular user/assistant messages.
		// Assistant messages that invoked tools must include tool_use content
		// blocks so subsequent tool_result blocks can reference them by ID.
		if m.Role == RoleAssistant && len(m.ToolCalls) > 0 {
			var blocks []anthropicContentBlock
			if m.Content != "" {
				blocks = append(blocks, anthropicContentBlock{
					Type: "text",
					Text: m.Content,
				})
			}
			for _, tc := range m.ToolCalls {
				blocks = append(blocks, anthropicContentBlock{
					Type:  "tool_use",
					ID:    tc.ID,
					Name:  tc.Name,
					Input: json.RawMessage(tc.Arguments),
				})
			}
			blockBytes, _ := json.Marshal(blocks)
			msgs = append(msgs, anthropicMessage{
				Role:    "assistant",
				Content: blockBytes,
			})
		} else {
			contentBytes, _ := json.Marshal(m.Content)
			msgs = append(msgs, anthropicMessage{
				Role:    string(m.Role),
				Content: contentBytes,
			})
		}
	}

	// Append any ToolResults from the request field as a user turn
	if len(req.ToolResults) > 0 {
		blocks := make([]anthropicContentBlock, len(req.ToolResults))
		for i, tr := range req.ToolResults {
			blocks[i] = anthropicContentBlock{
				Type:      "tool_result",
				ToolUseID: tr.ToolCallID,
				Content:   tr.Content,
				IsError:   tr.IsError,
			}
		}
		blockBytes, _ := json.Marshal(blocks)
		msgs = append(msgs, anthropicMessage{
			Role:    "user",
			Content: blockBytes,
		})
	}

	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = 4096
	}

	antReq := anthropicRequest{
		Model:     a.model,
		Messages:  msgs,
		System:    system,
		MaxTokens: maxTokens,
		Stream:    stream,
		Stop:      req.Stop,
	}

	// Convert tools
	if len(req.Tools) > 0 {
		antReq.Tools = make([]anthropicTool, len(req.Tools))
		for i, t := range req.Tools {
			antReq.Tools[i] = anthropicTool{
				Name:        t.Name,
				Description: t.Description,
				InputSchema: t.Parameters,
			}
		}
	}

	return antReq
}

func (a *anthropicAdapter) newRequest(ctx context.Context, path string, body interface{}) (*http.Request, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("anthropic: marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint+path, bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("anthropic: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", a.apiKey)
	req.Header.Set("anthropic-version", anthropicVersion)
	return req, nil
}

func (a *anthropicAdapter) doPost(ctx context.Context, path string, body interface{}) ([]byte, int, error) {
	req, err := a.newRequest(ctx, path, body)
	if err != nil {
		return nil, 0, err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("anthropic: http: %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("anthropic: read body: %w", err)
	}
	return b, resp.StatusCode, nil
}

func (a *anthropicAdapter) parseError(statusCode int, body []byte) error {
	var antErr anthropicError
	if json.Unmarshal(body, &antErr) == nil && antErr.Error.Message != "" {
		return fmt.Errorf("anthropic: status %d: %s", statusCode, antErr.Error.Message)
	}
	return fmt.Errorf("anthropic: status %d: %s", statusCode, string(body))
}
