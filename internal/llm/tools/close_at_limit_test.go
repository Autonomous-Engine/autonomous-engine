package tools

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
)

// toolsThenAnswerAdapter calls list_files with a fresh argument while tools
// are offered and answers "VERDICT" to a request without tools.
type toolsThenAnswerAdapter struct {
	listingAdapter
	last adapter.CompletionRequest
}

func (a *toolsThenAnswerAdapter) Complete(ctx context.Context, req adapter.CompletionRequest) (*adapter.CompletionResponse, error) {
	a.last = req
	if len(req.Tools) == 0 {
		a.n++
		return &adapter.CompletionResponse{Content: "VERDICT", FinishReason: "stop", InputTokens: 10, OutputTokens: 5}, nil
	}
	return a.listingAdapter.Complete(ctx, req)
}

// The verifier ran its build, its tests and the CLI, then hit its 20-call
// cap and the verdict it was about to give became "not verifiable: tool
// loop reached safety limit". With CloseAtLimit the cap closes the loop
// with one call that offers no tools and asks for the answer.
func TestCloseAtLimitAnswersAtTheIterationCap(t *testing.T) {
	req := adapter.CompletionRequest{Messages: []adapter.ChatMessage{{Role: adapter.RoleUser, Content: "go"}}, Tools: SafeImplementationTools()}
	model := &toolsThenAnswerAdapter{}
	resp, err := RunToolLoopWithOptions(context.Background(), model, req, NewExecutor(t.TempDir()), 3, nil, nil, LoopOptions{CloseAtLimit: true})
	if err != nil || resp == nil || resp.Content != "VERDICT" || resp.FinishReason != "closed_at_limit" {
		t.Fatalf("resp=%+v err=%v, want the closing answer", resp, err)
	}
	if model.n != 4 {
		t.Fatalf("calls = %d, want 3 tool turns and the closing call", model.n)
	}
	last := model.last.Messages[len(model.last.Messages)-1]
	if len(model.last.Tools) != 0 || last.Role != adapter.RoleUser || !strings.Contains(last.Content, "no more tool calls") {
		t.Fatalf("closing request: tools=%d last=%+v", len(model.last.Tools), last)
	}
	// Without the option the cap is still the error it was.
	_, err = RunToolLoopWithOptions(context.Background(), &toolsThenAnswerAdapter{}, req, NewExecutor(t.TempDir()), 3, nil, nil, LoopOptions{})
	if !errors.Is(err, ErrSafetyLimit) {
		t.Fatalf("err = %v, want ErrSafetyLimit", err)
	}
}
