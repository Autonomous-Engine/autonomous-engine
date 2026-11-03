package tools

import (
	"context"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The hook's cumulative total is the work item's, refreshed from the
// database before every call; a per-task cap compared against it tripped
// at 0 iterations live ("this research task used 220914 of its 66666-token
// slice"). The hook also gets this loop's own spend.
func TestIterationHookGetsTheLoopsOwnSpend(t *testing.T) {
	orig := sumWorkItemTokens
	sumWorkItemTokens = func(context.Context, *pgxpool.Pool, uuid.UUID) (int64, bool) { return 400_000, true }
	t.Cleanup(func() { sumWorkItemTokens = orig })

	ex := NewExecutor(t.TempDir())
	var totals, own []int64
	ex.SetIterationHook(func(_ context.Context, cumulative, _, loopSpend int64) error {
		totals, own = append(totals, cumulative), append(own, loopSpend)
		return nil
	})
	model := &listingAdapter{}
	req := adapter.CompletionRequest{Messages: []adapter.ChatMessage{{Role: adapter.RoleUser, Content: "go"}}, Tools: SafeImplementationTools()}
	workID := uuid.New()
	_, _ = RunToolLoopWithOptions(context.Background(), model, req, ex, 3, nil, &workID, LoopOptions{})
	// Work tokens: the first call's input (capped at the 100 reported) plus
	// output, then new input plus output — not the resent prompt.
	if len(own) < 3 || own[0] != 0 || own[1] != 200 || own[2] < 300 || own[2] > 400 {
		t.Fatalf("loop spend seen by the hook = %v, want [0 200 300..400]", own)
	}
	// The total is the work item's refreshed sum (plus what this loop added
	// since the refresh), never the loop's own count.
	if totals[0] != 400_000 || totals[1] < 400_000 || totals[2] < 400_000 {
		t.Fatalf("work item total seen by the hook = %v, want the refreshed 400000 and up", totals)
	}
}
