package strategy

import (
	"context"
	"fmt"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
)

// StandardStrategy sends the initial prompt and, for quality-critical intents,
// follows up with a self-review pass asking the model to verify and correct its output.
type StandardStrategy struct{}

const (
	defaultStandardTemp      = 0.3
	defaultStandardMaxTokens = 4096
)

// qualityCriticalIntents is the set of intents that benefit from a self-review pass.
var qualityCriticalIntents = map[IntentType]bool{
	IntentImplementChange:  true,
	IntentWriteTests:       true,
	IntentReviewCode:       true,
	IntentSynthesizeBrief:  true,
	IntentGeneratePRD:      true,
}

// Execute sends the initial prompt and, if the intent is quality-critical,
// sends a self-review pass to verify and improve the output.
func (s *StandardStrategy) Execute(ctx context.Context, a adapter.ModelAdapter, intent Intent) (*Result, error) {
	systemPrompt := buildSystemPrompt(ctx, intent)
	userPrompt := buildUserPrompt(intent)

	temp := intent.Temperature
	if temp == 0 {
		temp = defaultStandardTemp
	}
	maxTokens := intent.MaxTokens
	if maxTokens == 0 {
		maxTokens = defaultStandardMaxTokens
	}

	// Initial request
	initialReq := adapter.CompletionRequest{
		Messages: []adapter.ChatMessage{
			{Role: adapter.RoleSystem, Content: systemPrompt},
			{Role: adapter.RoleUser, Content: userPrompt},
		},
		Temperature: temp,
		MaxTokens:   maxTokens,
	}

	initialResp, err := a.Complete(ctx, initialReq)
	if err != nil {
		return nil, fmt.Errorf("standard strategy (initial): %w", err)
	}

	totalInput := initialResp.InputTokens
	totalOutput := initialResp.OutputTokens

	// Self-review pass for quality-critical intents
	if qualityCriticalIntents[intent.Type] {
		reviewReq := adapter.CompletionRequest{
			Messages: []adapter.ChatMessage{
				{Role: adapter.RoleSystem, Content: systemPrompt},
				{Role: adapter.RoleUser, Content: userPrompt},
				{Role: adapter.RoleAssistant, Content: initialResp.Content},
				{Role: adapter.RoleUser, Content: selfReviewPrompt(intent.Type)},
			},
			Temperature: temp,
			MaxTokens:   maxTokens,
		}

		reviewResp, err := a.Complete(ctx, reviewReq)
		if err != nil {
			// If self-review fails, return the initial response rather than failing entirely.
			return &Result{
				Content:      initialResp.Content,
				InputTokens:  totalInput,
				OutputTokens: totalOutput,
			}, nil
		}

		totalInput += reviewResp.InputTokens
		totalOutput += reviewResp.OutputTokens

		return &Result{
			Content:      reviewResp.Content,
			InputTokens:  totalInput,
			OutputTokens: totalOutput,
			Messages:     reviewReq.Messages,
		}, nil
	}

	return &Result{
		Content:      initialResp.Content,
		InputTokens:  totalInput,
		OutputTokens: totalOutput,
		Messages:     initialReq.Messages,
	}, nil
}

// selfReviewPrompt returns an appropriate self-review instruction for the intent type.
func selfReviewPrompt(t IntentType) string {
	switch t {
	case IntentImplementChange:
		return `Review your implementation above. Check for:
1. Correctness — does it handle all cases including errors?
2. Style — does it follow the patterns shown in the context?
3. Completeness — are there any missing pieces?

If you find issues, provide the corrected implementation. If the implementation is correct, output it unchanged.`

	case IntentWriteTests:
		return `Review your tests above. Check for:
1. Coverage — are happy paths, edge cases, and error conditions covered?
2. Correctness — will these tests actually catch bugs?
3. Independence — are tests independent of each other?

If you find gaps, provide the improved test suite. If the tests are complete, output them unchanged.`

	case IntentReviewCode:
		return `Review your code review above. Check for:
1. Completeness — did you miss any important issues?
2. Accuracy — are your observations correct?
3. Priority — are the severity ratings appropriate?

If you find corrections or additions, provide the updated review. Otherwise output it unchanged.`

	case IntentSynthesizeBrief:
		return `Review your brief above. Check for:
1. Accuracy — does it correctly represent the source information?
2. Clarity — is it understandable to a technical audience?
3. Actionability — are the recommendations concrete and prioritized?

If you find improvements, provide the revised brief. Otherwise output it unchanged.`

	case IntentGeneratePRD:
		return `Review your PRD above. Check for:
1. Completeness — are all required sections present and substantive?
2. Clarity — are acceptance criteria testable and unambiguous?
3. Consistency — are there any contradictions between sections?

If you find issues, provide the corrected PRD. Otherwise output it unchanged.`

	default:
		return `Review your response above for accuracy, completeness, and clarity. If you find any issues, provide a corrected version. Otherwise output it unchanged.`
	}
}
