package main

import (
	"context"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/models"
)

// M4-06: a replay item ends on its review.complete — whatever the decision —
// as cancelled ("replay: evidence recorded") with its outcome row, never
// reaching a fix cycle, human review or QA; replay.complete cancels one with
// nothing to review; replay.complete for an ordinary item is ignored.
// Redeliveries change nothing. Env-gated (TEST_POSTGRES_DSN).
func TestIntegration_ReplayItemsTerminate(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	h := guardedOrchestrator(t, pool)

	for _, decision := range []string{"changes_requested", "approved"} {
		item, _ := seedItem(t, pool, models.WorkItemStateReview)
		if _, err := pool.Exec(ctx, `UPDATE work_items SET replay = true WHERE id = $1`, item); err != nil {
			t.Fatal(err)
		}
		msg := eventMessage(t, bus.TopicReviewComplete, reviewCompletePayload{WorkID: item.String(), Decision: decision, Feedback: "x"})
		cleanupLedgerRow(t, pool, msg)
		replay(t, h, msg, 3)
		if n := count(t, pool, `SELECT count(*) FROM work_items WHERE id = $1 AND state = 'cancelled'`, item); n != 1 {
			t.Errorf("%s: replay item not cancelled", decision)
		}
		if n := count(t, pool, `SELECT count(*) FROM work_item_transitions WHERE work_item_id = $1 AND reason LIKE 'replay: evidence recorded%'`, item); n != 1 {
			t.Errorf("%s: cancel transitions = %d, want 1", decision, n)
		}
		if n := count(t, pool, `SELECT count(*) FROM work_items WHERE parent_id = $1`, item); n != 0 {
			t.Errorf("%s: a fix subtask was created for a replay item", decision)
		}
		if n := count(t, pool, `SELECT count(*) FROM work_item_outcomes WHERE work_item_id = $1`, item); n != 1 {
			t.Errorf("%s: outcome rows = %d, want 1", decision, n)
		}
	}

	// replay.complete: cancels a replay item, ignores an ordinary one.
	rep, _ := seedItem(t, pool, models.WorkItemStateImplementation)
	if _, err := pool.Exec(ctx, `UPDATE work_items SET replay = true WHERE id = $1`, rep); err != nil {
		t.Fatal(err)
	}
	msg := eventMessage(t, bus.TopicReplayComplete, replayCompletePayload{WorkID: rep.String(), Reason: "no verified branch to review"})
	cleanupLedgerRow(t, pool, msg)
	replay(t, h, msg, 2)
	if n := count(t, pool, `SELECT count(*) FROM work_items WHERE id = $1 AND state = 'cancelled'`, rep); n != 1 {
		t.Error("replay.complete did not cancel the replay item")
	}
	ordinary, _ := seedItem(t, pool, models.WorkItemStateImplementation)
	msg = eventMessage(t, bus.TopicReplayComplete, replayCompletePayload{WorkID: ordinary.String()})
	cleanupLedgerRow(t, pool, msg)
	replay(t, h, msg, 1)
	if n := count(t, pool, `SELECT count(*) FROM work_items WHERE id = $1 AND state = 'implementation'`, ordinary); n != 1 {
		t.Error("replay.complete moved an ordinary item")
	}
}
