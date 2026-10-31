package tools

import (
	"context"
	"errors"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
)

// listingAdapter calls list_files with a fresh argument every turn.
type listingAdapter struct{ n int }

func (a *listingAdapter) Complete(context.Context, adapter.CompletionRequest) (*adapter.CompletionResponse, error) {
	a.n++
	args := `{"path":"."}`
	if a.n%2 == 0 {
		args = `{"path":"./"}`
	}
	return &adapter.CompletionResponse{ToolCalls: []adapter.ToolCall{{ID: "c" + string(rune('a'+a.n)), Name: "list_files", Arguments: args}},
		FinishReason: "tool_calls", InputTokens: 100, OutputTokens: 100}, nil
}
func (a *listingAdapter) Stream(context.Context, adapter.CompletionRequest) (<-chan adapter.StreamChunk, error) {
	return nil, errors.New("not used")
}
func (a *listingAdapter) Capabilities() adapter.ModelCapabilities {
	return adapter.ModelCapabilities{MaxContextTokens: 100000, SupportsFunctions: true}
}
func (a *listingAdapter) HealthCheck(context.Context) error { return nil }
func (a *listingAdapter) Name() string                      { return "listing/test" }

type stopErr struct{}

func (stopErr) Error() string { return "budget says stop" }

// M3-06: the iteration hook runs before every LLM call with the loop's
// cumulative tokens, and its error stops the loop, wrapped.
func TestIterationHookStopsTheLoopBeforeTheNextCall(t *testing.T) {
	ex := NewExecutor(t.TempDir())
	var seen []int64
	ex.SetIterationHook(func(_ context.Context, cumulative, budget int64) error {
		seen = append(seen, cumulative)
		if budget != TokenBudget() {
			t.Errorf("budget %d, want %d", budget, TokenBudget())
		}
		if cumulative >= 400 {
			return stopErr{}
		}
		return nil
	})
	model := &listingAdapter{}
	req := adapter.CompletionRequest{Messages: []adapter.ChatMessage{{Role: adapter.RoleUser, Content: "go"}}, Tools: SafeImplementationTools()}
	_, err := RunToolLoop(context.Background(), model, req, ex, 50, nil, nil)
	var se stopErr
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want the hook's error wrapped", err)
	}
	if model.n != 2 || len(seen) != 3 || seen[0] != 0 || seen[1] != 200 || seen[2] != 400 {
		t.Fatalf("calls=%d hook saw %v, want 2 calls and [0 200 400]", model.n, seen)
	}
}

func TestTokenBudgetErrorIsTyped(t *testing.T) {
	t.Setenv("WORK_ITEM_TOKEN_BUDGET", "300")
	if TokenBudget() != 300 {
		t.Fatalf("TokenBudget = %d", TokenBudget())
	}
	ex := NewExecutor(t.TempDir())
	req := adapter.CompletionRequest{Messages: []adapter.ChatMessage{{Role: adapter.RoleUser, Content: "go"}}, Tools: SafeImplementationTools()}
	_, err := RunToolLoop(context.Background(), &listingAdapter{}, req, ex, 50, nil, nil)
	if !errors.Is(err, ErrTokenBudgetExceeded) {
		t.Fatalf("err = %v, want ErrTokenBudgetExceeded", err)
	}
}
