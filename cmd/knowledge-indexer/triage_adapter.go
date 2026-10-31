// cmd/knowledge-indexer/triage_adapter.go — glue between interactions.TriageLLM
// and the service's existing llm.Service. Extracts JSON from markdown fences
// since some model adapters wrap their output.
package main

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
)

// oneShotLLM is a narrow contract we inject for testing. The production
// implementation wraps llm.Service; the test injects a stub directly.
type oneShotLLM interface {
	oneShotPrompt(ctx context.Context, prompt string) (string, error)
}

type TriageAdapter struct {
	llm oneShotLLM
}

func newTriageAdapterForTest(llm oneShotLLM) *TriageAdapter {
	return &TriageAdapter{llm: llm}
}

// Classify implements interactions.TriageLLM by delegating to the oneShotLLM
// and extracting any JSON payload from markdown fences.
func (a *TriageAdapter) Classify(ctx context.Context, prompt string) (string, error) {
	raw, err := a.llm.oneShotPrompt(ctx, prompt)
	if err != nil {
		return "", err
	}
	return extractJSON(raw), nil
}

var fenceRE = regexp.MustCompile("(?s)```(?:json)?\\s*(.*?)\\s*```")

// extractJSON strips a ```json ... ``` fence if present, else returns input trimmed.
func extractJSON(s string) string {
	if m := fenceRE.FindStringSubmatch(s); len(m) >= 2 {
		return strings.TrimSpace(m[1])
	}
	return strings.TrimSpace(s)
}

// productionLLMShim wraps an llm.ModelAdapter and implements oneShotLLM for
// the TriageAdapter. It calls the adapter's Complete method with a simple
// system+user message pair, requesting JSON output.
type productionLLMShim struct {
	adapter adapter.ModelAdapter
}

// newTriageAdapter is the production constructor that wires the real
// llm.ModelAdapter into the TriageAdapter.
func newTriageAdapter(a adapter.ModelAdapter) *TriageAdapter {
	return &TriageAdapter{llm: &productionLLMShim{adapter: a}}
}

func (p *productionLLMShim) oneShotPrompt(ctx context.Context, prompt string) (string, error) {
	// Classify is the system prompt; user passes the full question+answer probe.
	// Set JSON mode if supported and force a JSON response.
	req := adapter.CompletionRequest{
		Messages: []adapter.ChatMessage{
			{
				Role:    adapter.RoleSystem,
				Content: "You are a classifier. Respond only with valid JSON objects as specified in the prompt.",
			},
			{
				Role:    adapter.RoleUser,
				Content: prompt,
			},
		},
		MaxTokens:   256,
		Temperature: 0.3,
		JSONMode:    p.adapter.Capabilities().SupportsJSON,
	}

	resp, err := p.adapter.Complete(ctx, req)
	if err != nil {
		return "", fmt.Errorf("adapter complete: %w", err)
	}

	return resp.Content, nil
}
