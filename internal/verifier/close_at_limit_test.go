package verifier

import (
	"context"
	"errors"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
	"github.com/autonomousengine/autonomous-engine/internal/llm/tools"
)

// readingThenVerdictAdapter reads a.go on every request that offers tools
// and gives the verdict to the one that does not.
type readingThenVerdictAdapter struct{ n int }

func (a *readingThenVerdictAdapter) Complete(_ context.Context, req adapter.CompletionRequest) (*adapter.CompletionResponse, error) {
	a.n++
	if len(req.Tools) == 0 {
		return &adapter.CompletionResponse{Content: approveAC1, FinishReason: "stop"}, nil
	}
	return &adapter.CompletionResponse{ToolCalls: []adapter.ToolCall{{ID: "c" + string(rune('a'+a.n)), Name: "read_file", Arguments: `{"path":"a.go"}`}}, FinishReason: "tool_calls"}, nil
}
func (a *readingThenVerdictAdapter) Stream(context.Context, adapter.CompletionRequest) (<-chan adapter.StreamChunk, error) {
	return nil, errors.New("not used")
}
func (a *readingThenVerdictAdapter) Capabilities() adapter.ModelCapabilities {
	return adapter.ModelCapabilities{MaxContextTokens: 100000, SupportsFunctions: true}
}
func (a *readingThenVerdictAdapter) HealthCheck(context.Context) error { return nil }
func (a *readingThenVerdictAdapter) Name() string                      { return "reading/test" }

// Live, the verifier built the branch, ran its tests and the CLI, then its
// 20-call cap turned the verdict into "not verifiable: verifier tool loop
// failed: tool loop reached safety limit" — an escalation. At the cap the
// verifier is asked for its verdict with no further tools.
func TestRunClosesWithAVerdictAtTheIterationCap(t *testing.T) {
	in := runInput(t)
	model := &readingThenVerdictAdapter{}
	v := Run(context.Background(), Runner{Adapter: model, Executor: tools.NewExecutor(in.RepoTree), MaxIterations: 2}, in)
	if v.Error != "" || v.Overall != OverallApprove {
		t.Fatalf("verdict = %+v, want the verdict given at the cap", v)
	}
	if model.n != 3 {
		t.Fatalf("calls = %d, want 2 tool turns and the closing call", model.n)
	}
}
