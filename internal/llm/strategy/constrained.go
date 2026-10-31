package strategy

import (
	"context"
	"fmt"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
)

// ConstrainedStrategy uses a four-step chain to guide weaker models through
// complex tasks: understand → plan → execute → verify.
// Each step uses a smaller token budget to stay within model limits.
type ConstrainedStrategy struct{}

const defaultConstrainedMaxTokens = 2048

// Execute runs the four-step constrained chain:
// 1. Understanding — confirm task comprehension
// 2. Planning — outline the approach
// 3. Execution — produce the output
// 4. Verification — check the output
func (s *ConstrainedStrategy) Execute(ctx context.Context, a adapter.ModelAdapter, intent Intent) (*Result, error) {
	systemPrompt := buildSystemPrompt(ctx, intent)
	userPrompt := buildUserPrompt(intent)

	temp := intent.Temperature
	if temp == 0 {
		temp = defaultStandardTemp
	}
	maxTokens := defaultConstrainedMaxTokens

	totalInput := 0
	totalOutput := 0

	// Step 1: Understanding
	understandReq := adapter.CompletionRequest{
		Messages: []adapter.ChatMessage{
			{Role: adapter.RoleSystem, Content: systemPrompt},
			{Role: adapter.RoleUser, Content: fmt.Sprintf("%s\n\n---\nStep 1: Summarize this task in 2-3 sentences to confirm you understand what is being asked.", userPrompt)},
		},
		Temperature: temp,
		MaxTokens:   maxTokens / 4, // small budget for understanding
	}

	understandResp, err := a.Complete(ctx, understandReq)
	if err != nil {
		return nil, fmt.Errorf("constrained strategy (understand): %w", err)
	}
	totalInput += understandResp.InputTokens
	totalOutput += understandResp.OutputTokens

	// Step 2: Planning
	planReq := adapter.CompletionRequest{
		Messages: []adapter.ChatMessage{
			{Role: adapter.RoleSystem, Content: systemPrompt},
			{Role: adapter.RoleUser, Content: userPrompt},
			{Role: adapter.RoleAssistant, Content: understandResp.Content},
			{Role: adapter.RoleUser, Content: "Step 2: Outline your approach step by step. List each step you will take to complete the task."},
		},
		Temperature: temp,
		MaxTokens:   maxTokens / 2,
	}

	planResp, err := a.Complete(ctx, planReq)
	if err != nil {
		return nil, fmt.Errorf("constrained strategy (plan): %w", err)
	}
	totalInput += planResp.InputTokens
	totalOutput += planResp.OutputTokens

	// Step 3: Execution
	executeReq := adapter.CompletionRequest{
		Messages: []adapter.ChatMessage{
			{Role: adapter.RoleSystem, Content: systemPrompt},
			{Role: adapter.RoleUser, Content: userPrompt},
			{Role: adapter.RoleAssistant, Content: understandResp.Content},
			{Role: adapter.RoleUser, Content: "Step 2: Outline your approach step by step. List each step you will take to complete the task."},
			{Role: adapter.RoleAssistant, Content: planResp.Content},
			{Role: adapter.RoleUser, Content: "Step 3: Execute the plan. Produce the complete output now, following the steps you outlined."},
		},
		Temperature: temp,
		MaxTokens:   maxTokens,
	}

	executeResp, err := a.Complete(ctx, executeReq)
	if err != nil {
		return nil, fmt.Errorf("constrained strategy (execute): %w", err)
	}
	totalInput += executeResp.InputTokens
	totalOutput += executeResp.OutputTokens

	// Step 4: Verification
	verifyReq := adapter.CompletionRequest{
		Messages: []adapter.ChatMessage{
			{Role: adapter.RoleSystem, Content: systemPrompt},
			{Role: adapter.RoleUser, Content: userPrompt},
			{Role: adapter.RoleAssistant, Content: understandResp.Content},
			{Role: adapter.RoleUser, Content: "Step 2: Outline your approach step by step. List each step you will take to complete the task."},
			{Role: adapter.RoleAssistant, Content: planResp.Content},
			{Role: adapter.RoleUser, Content: "Step 3: Execute the plan. Produce the complete output now, following the steps you outlined."},
			{Role: adapter.RoleAssistant, Content: executeResp.Content},
			{Role: adapter.RoleUser, Content: "Step 4: Verify your output. Check it for correctness, completeness, and accuracy. If it is correct, output it exactly as-is. If you find errors, output the corrected version."},
		},
		Temperature: temp,
		MaxTokens:   maxTokens,
	}

	verifyResp, err := a.Complete(ctx, verifyReq)
	if err != nil {
		// If verification fails, return the execution result
		return &Result{
			Content:      executeResp.Content,
			InputTokens:  totalInput,
			OutputTokens: totalOutput,
		}, nil
	}
	totalInput += verifyResp.InputTokens
	totalOutput += verifyResp.OutputTokens

	return &Result{
		Content:      verifyResp.Content,
		InputTokens:  totalInput,
		OutputTokens: totalOutput,
	}, nil
}
