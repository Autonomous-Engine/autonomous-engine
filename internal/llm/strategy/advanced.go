package strategy

import (
	"context"
	"fmt"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
)

// AdvancedStrategy sends a single comprehensive prompt containing both the
// system prompt and the fully assembled user prompt. It is suited for capable
// models that can handle rich context in one shot.
type AdvancedStrategy struct{}

const (
	defaultAdvancedTemp      = 0.3
	defaultAdvancedMaxTokens = 4096
)

// Execute builds a single system+user prompt and sends it to the adapter.
func (s *AdvancedStrategy) Execute(ctx context.Context, a adapter.ModelAdapter, intent Intent) (*Result, error) {
	systemPrompt := buildSystemPrompt(ctx, intent)
	userPrompt := buildUserPrompt(intent)

	temp := intent.Temperature
	if temp == 0 {
		temp = defaultAdvancedTemp
	}
	maxTokens := intent.MaxTokens
	if maxTokens == 0 {
		maxTokens = defaultAdvancedMaxTokens
	}

	req := adapter.CompletionRequest{
		Messages: []adapter.ChatMessage{
			{Role: adapter.RoleSystem, Content: systemPrompt},
			{Role: adapter.RoleUser, Content: userPrompt},
		},
		Temperature: temp,
		MaxTokens:   maxTokens,
	}

	resp, err := a.Complete(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("advanced strategy: %w", err)
	}

	return &Result{
		Content:      resp.Content,
		InputTokens:  resp.InputTokens,
		OutputTokens: resp.OutputTokens,
	}, nil
}
