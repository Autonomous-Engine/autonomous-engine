package main

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/idempotency"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// M1-05: implementation.complete replayed ten times into the planner's
// guarded handler completes the task and releases its dependent once; a
// technical-brief.ready delivery that reaches the plan's transaction a second
// time commits nothing.

const plannerConsumer = "implementation-planner"

func guardedCompletion(pool *pgxpool.Pool) bus.Handler {
	wg := workgraph.New(pool)
	return idempotency.NewLedger(pool).Handler(plannerConsumer, func(ctx context.Context, msg *bus.Message) error {
		return handleImplementationComplete(ctx, idempotency.Wrap(ctx, pool), wg, nil, evidence.CompatWindowLegacy, msg)
	})
}

func TestIntegration_Idempotency_ImplementationCompleteReplayedTenTimes(t *testing.T) {
	pool := integrationPool(t)
	f := newFixture(t, pool)
	plan, ids := f.chainPlan(t, 3)
	if err := commitDecomposition(context.Background(), pool, workgraph.New(pool), f.parentWorkID, nil, plan); err != nil {
		t.Fatalf("commitDecomposition: %v", err)
	}
	value, err := json.Marshal(implementationCompletePayload{TaskID: ids[0].String(), WorkID: f.parentWorkID.String(), Success: true, Reason: "m105"})
	if err != nil {
		t.Fatal(err)
	}
	eventID := uuid.New()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM processed_events WHERE event_id = $1`, eventID)
	})
	msg := &bus.Message{Topic: bus.TopicImplementationComplete, Key: ids[0].String(), Value: value,
		Headers: map[string]string{bus.HeaderEventID: eventID.String()}}

	h := guardedCompletion(pool)
	for i := 0; i < 10; i++ {
		if err := h(context.Background(), msg); err != nil {
			t.Fatalf("delivery %d: %v", i+1, err)
		}
	}

	assertStatuses(t, pool, ids, planTaskCompleted, planTaskReleased, planTaskHeld)
	if got := eventKeys(taskEvents(t, pool, f.parentWorkID)); fmt.Sprint(got) != fmt.Sprint([]string{ids[0].String(), ids[1].String()}) {
		t.Fatalf("events = %v, want [A B]: B released exactly once", got)
	}
	var rows int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM processed_events WHERE consumer = $1 AND event_id = $2`, plannerConsumer, eventID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("ledger rows = %d, want 1", rows)
	}
}

func TestIntegration_Idempotency_DecompositionCommitsOncePerEvent(t *testing.T) {
	pool := integrationPool(t)
	f := newFixture(t, pool)
	wg := workgraph.New(pool)
	ledger := idempotency.NewLedger(pool)
	eventID := uuid.New()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM processed_events WHERE event_id = $1`, eventID)
	})
	msg := &bus.Message{Topic: bus.TopicTechnicalBriefReady, Value: []byte(`{}`), Headers: map[string]string{bus.HeaderEventID: eventID.String()}}

	// Two deliveries of one technical-brief.ready that both got as far as
	// the commit (the second raced past the up-front Seen check). Each plans
	// fresh subtask ids, as a real re-decomposition would.
	var errs [2]error
	for i := range errs {
		ctx := idempotency.WithDelivery(context.Background(), ledger.NewDelivery(plannerConsumer, msg))
		plan, _ := f.chainPlan(t, 2)
		errs[i] = commitDecomposition(ctx, idempotency.Wrap(ctx, pool), wg, f.parentWorkID, nil, plan)
	}
	if errs[0] != nil {
		t.Fatalf("first commit: %v", errs[0])
	}
	if !idempotency.IsDuplicate(errs[1]) {
		t.Fatalf("second commit err = %v, want ErrDuplicate", errs[1])
	}
	var subtasks int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM plan_tasks WHERE parent_work_id = $1`, f.parentWorkID).Scan(&subtasks); err != nil {
		t.Fatal(err)
	}
	if subtasks != 2 {
		t.Fatalf("plan_tasks rows = %d, want 2 (one plan)", subtasks)
	}
}
