package main

import (
	"context"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/interactions"
)

// Live (ciblame item 0f565594): two verification.complete events for the
// same parent — one per approved subtask of the round — each ran the PR
// loop over every passed child, and the manual gate recorded the same
// branch at the same verified head twice: two pending changes and two
// "Create PR" prompts for one PR. A branch already pending at that head is
// gated once; a later event for it records nothing more.
func TestIntegration_ABranchPendingAtTheSameHeadIsGatedOnce(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	workID, taskID := seedGatedParent(t, pool, "implementation")
	h := guardedCoordinator(pool)

	for i := 0; i < 2; i++ {
		msg := completionMessage(t, pool, workID, taskID) // a new event id each time
		msg.Value = verifiedCompletion(t, pool, workID, taskID)
		if err := h(ctx, msg); err != nil {
			t.Fatalf("delivery %d: %v", i+1, err)
		}
	}

	if n := count(t, pool, `SELECT count(*) FROM pending_pr_changes WHERE work_item_id = $1 AND status = 'pending'`, workID); n != 1 {
		t.Errorf("pending PR rows = %d, want 1 (the branch is already pending at this head)", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM user_interactions WHERE work_item_id = $1 AND trigger = $2 AND state = 'pending'`, workID, string(interactions.TriggerApprovalPRGate)); n != 1 {
		t.Errorf("pending pr-gate prompts = %d, want 1", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND topic = 'pr.gated'`, workID); n != 1 {
		t.Errorf("pr.gated events = %d, want 1", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM work_items WHERE id = $1 AND state = 'blocked'`, workID); n != 0 {
		t.Error("a repeated event must not block the parent")
	}
}
