package tools

import (
	"context"
	"errors"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The work item's token total is shared by every agent working on it. A
// loop that reads it once at start and then only counts its own calls lets
// parallel loops overshoot together (F36): the total must be re-read from
// the database before each paid call.
func TestToolLoopRefreshesBudgetFromDB(t *testing.T) {
	t.Setenv("WORK_ITEM_TOKEN_BUDGET", "500000")
	orig := sumWorkItemTokens
	t.Cleanup(func() { sumWorkItemTokens = orig })
	reads := 0
	sumWorkItemTokens = func(context.Context, *pgxpool.Pool, uuid.UUID) (int64, bool) {
		reads++
		if reads <= 2 { // loop start and the read before call 1
			return 100, true
		}
		return 600_000, true // another agent spent the budget meanwhile
	}

	ex := NewExecutor(t.TempDir())
	model := &listingAdapter{}
	id := uuid.New()
	req := adapter.CompletionRequest{Messages: []adapter.ChatMessage{{Role: adapter.RoleUser, Content: "go"}}, Tools: SafeImplementationTools()}
	_, err := RunToolLoop(context.Background(), model, req, ex, 50, nil, &id)
	if !errors.Is(err, ErrTokenBudgetExceeded) {
		t.Fatalf("err = %v, want ErrTokenBudgetExceeded", err)
	}
	if model.n != 1 {
		t.Fatalf("model called %d times, want 1: the second call must not be paid for once the shared total is over budget", model.n)
	}
	if reads < 2 {
		t.Fatalf("total read %d times, want one read per iteration", reads)
	}
}

// Without a database the loop still counts its own calls.
func TestToolLoopCountsOwnTokensWithoutDB(t *testing.T) {
	t.Setenv("WORK_ITEM_TOKEN_BUDGET", "300")
	ex := NewExecutor(t.TempDir())
	req := adapter.CompletionRequest{Messages: []adapter.ChatMessage{{Role: adapter.RoleUser, Content: "go"}}, Tools: SafeImplementationTools()}
	_, err := RunToolLoop(context.Background(), &listingAdapter{}, req, ex, 50, nil, nil)
	if !errors.Is(err, ErrTokenBudgetExceeded) {
		t.Fatalf("err = %v, want ErrTokenBudgetExceeded", err)
	}
}
