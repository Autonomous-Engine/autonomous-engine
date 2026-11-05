package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
)

// A round whose tasks never report is closed by the sweeper and its work
// item blocked with a reason, instead of showing "research" forever (F37).
func TestIntegration_SweepExpiredRoundsBlocksWorkItem(t *testing.T) {
	pool := integrationPool(t)
	wg := workgraph.New(pool)
	ctx := context.Background()
	workID, productID := newWorkItem(t, pool, models.WorkItemStateResearch)

	task := researchTaskMessage{TaskID: uuid.NewString(), WorkID: workID.String(), ProductID: productID.String(), ResearchType: "impact_assessment"}
	round := roundFor(workID, prdHash("the PRD"), []researchTaskMessage{task}, nil)
	if err := commitResearchDispatch(ctx, pool, wg, workID, workID, []researchTaskMessage{task}, round); err != nil {
		t.Fatalf("commitResearchDispatch: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE research_rounds SET created_at = now() - interval '31 minutes' WHERE id = $1`, round.ID); err != nil {
		t.Fatal(err)
	}

	n, err := sweepExpiredRounds(ctx, pool, 30*time.Minute)
	if err != nil {
		t.Fatalf("sweepExpiredRounds: %v", err)
	}
	if n != 1 {
		t.Fatalf("swept %d rounds, want 1", n)
	}

	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM research_rounds WHERE id = $1`, round.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != roundBlocked {
		t.Fatalf("round status = %q, want blocked", status)
	}
	var state string
	if err := pool.QueryRow(ctx, `SELECT state FROM work_items WHERE id = $1`, workID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != string(models.WorkItemStateBlocked) {
		t.Fatalf("work item state = %q, want blocked", state)
	}
	var reason string
	if err := pool.QueryRow(ctx, `SELECT reason FROM work_item_transitions WHERE work_item_id = $1 ORDER BY "timestamp" DESC LIMIT 1`, workID).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reason, "timed out after 30m0s: 1 of 1 tasks never reported") || !strings.Contains(reason, task.TaskID) {
		t.Fatalf("transition reason = %q", reason)
	}

	// A second sweep finds nothing: the round is closed.
	if n, err := sweepExpiredRounds(ctx, pool, 30*time.Minute); err != nil || n != 0 {
		t.Fatalf("second sweep: n=%d err=%v", n, err)
	}
}

// A round that is open but still inside its time budget is left alone.
func TestIntegration_SweepLeavesFreshRounds(t *testing.T) {
	pool := integrationPool(t)
	wg := workgraph.New(pool)
	ctx := context.Background()
	workID, productID := newWorkItem(t, pool, models.WorkItemStateResearch)
	task := researchTaskMessage{TaskID: uuid.NewString(), WorkID: workID.String(), ProductID: productID.String(), ResearchType: "code_trace"}
	round := roundFor(workID, prdHash("fresh"), []researchTaskMessage{task}, nil)
	if err := commitResearchDispatch(ctx, pool, wg, workID, workID, []researchTaskMessage{task}, round); err != nil {
		t.Fatal(err)
	}
	if n, err := sweepExpiredRounds(ctx, pool, 30*time.Minute); err != nil || n != 0 {
		t.Fatalf("fresh round swept: n=%d err=%v", n, err)
	}
}
