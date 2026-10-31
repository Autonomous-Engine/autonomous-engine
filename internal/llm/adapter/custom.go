package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// customAdapter is a generic HTTP adapter that uses an OpenAI-compatible request
// format. It serves as a fallback for unusual or custom LLM setups.
type customAdapter struct {
	endpoint string
	apiKey   string
	model    string
	client   *http.Client
}

// NewCustom returns a ModelAdapter using a generic OpenAI-compatible HTTP endpoint.
func NewCustom(endpoint, apiKey, model string) ModelAdapter {
	return &customAdapter{
		endpoint: strings.TrimRight(endpoint, "/"),
		apiKey:   apiKey,
		model:    model,
		client:   &http.Client{Timeout: 120 * time.Second},
	}
}

func (a *customAdapter) Name() string {
	return "custom/" + a.model
}

func (a *customAdapter) Capabilities() ModelCapabilities {
	return ModelCapabilities{
		MaxContextTokens:  4096,
		SupportsJSON:      false,
		SupportsStreaming: false,
		SupportsFunctions: false,
	}
}

func (a *customAdapter) Complete(ctx context.Context, req CompletionRequest) (*CompletionResponse, error) {
	// Re-use OpenAI request shape as the generic format.
	msgs := make([]openAIMessage, len(req.Messages))
	for i, m := range req.Messages {
		msgs[i] = openAIMessage{Role: string(m.Role), Content: m.Content}
	}
	body := openAIRequest{
		Model:       a.model,
		Messages:    msgs,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stop:        req.Stop,
	}

	b, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("custom: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint+"/chat/completions", bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("custom: create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if a.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+a.apiKey)
	}

	resp, err := a.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("custom: http: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("custom: read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("custom: status %d: %s", resp.StatusCode, string(respBody))
	}

	var oaResp openAIResponse
	if err := json.Unmarshal(respBody, &oaResp); err != nil {
		return nil, fmt.Errorf("custom: decode response: %w", err)
	}
	if len(oaResp.Choices) == 0 {
		return nil, fmt.Errorf("custom: empty choices in response")
	}
	content := ""
	if oaResp.Choices[0].Message != nil {
		content = oaResp.Choices[0].Message.Content
	}
	return &CompletionResponse{
		Content:      content,
		InputTokens:  oaResp.Usage.PromptTokens,
		OutputTokens: oaResp.Usage.CompletionTokens,
		Model:        oaResp.Model,
		FinishReason: oaResp.Choices[0].FinishReason,
	}, nil
}

// Stream is not supported by the generic custom adapter.
func (a *customAdapter) Stream(_ context.Context, _ CompletionRequest) (<-chan StreamChunk, error) {
	return nil, fmt.Errorf("custom: streaming not supported")
}

func (a *customAdapter) HealthCheck(ctx context.Context) error {
	pingCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	_, err := a.Complete(pingCtx, CompletionRequest{
		Messages:  []ChatMessage{{Role: RoleUser, Content: "ping"}},
		MaxTokens: 5,
	})
	return err
}
