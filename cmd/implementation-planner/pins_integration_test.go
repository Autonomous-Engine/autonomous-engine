package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/snapshot"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const planPinSHA = "7af0d7084cd8b7c8a96b50da4311b560bebcb8e8"

func pinParent(t *testing.T, f fixture, pool interface {
	Begin(context.Context) (pgx.Tx, error)
}) {
	t.Helper()
	ctx := context.Background()
	err := workgraph.WithTx(ctx, pool, func(tx pgx.Tx) error {
		_, err := snapshot.NewStore(nil).PinTx(ctx, tx, snapshot.Pin{
			WorkItemID: f.parentWorkID, RepositoryID: f.repositoryID, BaseRef: "main", BaseSHA: planPinSHA,
		}, "intake: test")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// M2-02: the parent's pin reaches the subtask's own row (with a resolved
// event naming the parent), the implementation.tasks payload and the stored
// plan_tasks.message, all in the plan's transaction.
func TestIntegration_Pins_CopiedToSubtaskAndMessage(t *testing.T) {
	pool := integrationPool(t)
	f := newFixture(t, pool)
	pinParent(t, f, pool)
	ctx := context.Background()

	pins, err := loadParentPins(ctx, pool, f.parentWorkID)
	if err != nil {
		t.Fatal(err)
	}
	pin, ok := pins[f.repositoryID]
	if !ok || pin.BaseSHA != planPinSHA {
		t.Fatalf("loadParentPins = %+v", pins)
	}
	taskID := uuid.New()
	msg := f.taskMessageFor(taskID)
	msg.BaseSHA, msg.BaseRef = pin.BaseSHA, pin.BaseRef
	plan := []plannedSubtask{{repoName: "m103-repo", steps: 1, item: f.subtaskFor(taskID), msg: msg}}
	if err := commitDecomposition(ctx, pool, workgraph.New(pool), f.parentWorkID, &f.agentDefID, plan); err != nil {
		t.Fatalf("commitDecomposition: %v", err)
	}

	store := snapshot.NewStore(pool)
	child, found, err := store.Get(ctx, taskID, f.repositoryID)
	if err != nil || !found || child.BaseSHA != planPinSHA || child.BaseRef != "main" {
		t.Fatalf("subtask pin = %+v found=%v err=%v", child, found, err)
	}
	evs, _ := store.Events(ctx, taskID)
	if len(evs) != 1 || evs[0].Kind != snapshot.EventResolved || !strings.Contains(evs[0].Detail, f.parentWorkID.String()) {
		t.Fatalf("subtask events = %+v", evs)
	}

	var payload, stored []byte
	if err := pool.QueryRow(ctx, `SELECT payload FROM outbox_events WHERE aggregate_id = $1 AND topic = 'implementation.tasks'`, f.parentWorkID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT message FROM plan_tasks WHERE id = $1`, taskID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string][]byte{"outbox payload": payload, "plan_tasks.message": stored} {
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if m["base_sha"] != planPinSHA || m["base_ref"] != "main" {
			t.Errorf("%s base_sha=%v base_ref=%v", name, m["base_sha"], m["base_ref"])
		}
	}
}

// A legacy-unpinned parent: no pin row for the subtask and no base_sha on the
// message; the plan still commits.
func TestIntegration_Pins_UnpinnedParentPlansUnpinned(t *testing.T) {
	pool := integrationPool(t)
	f := newFixture(t, pool)
	ctx := context.Background()
	if pins, err := loadParentPins(ctx, pool, f.parentWorkID); err != nil || len(pins) != 0 {
		t.Fatalf("unpinned parent has pins %+v", pins)
	}
	taskID := uuid.New()
	plan := []plannedSubtask{{repoName: "m103-repo", steps: 1, item: f.subtaskFor(taskID), msg: f.taskMessageFor(taskID)}}
	if err := commitDecomposition(ctx, pool, workgraph.New(pool), f.parentWorkID, nil, plan); err != nil {
		t.Fatalf("commitDecomposition: %v", err)
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM work_item_base_shas WHERE work_item_id = $1`, taskID).Scan(&n)
	if n != 0 {
		t.Fatalf("unpinned parent produced %d subtask pins", n)
	}
	var payload []byte
	_ = pool.QueryRow(ctx, `SELECT payload FROM outbox_events WHERE aggregate_id = $1 AND topic = 'implementation.tasks'`, f.parentWorkID).Scan(&payload)
	if strings.Contains(string(payload), "base_sha") {
		t.Fatalf("unpinned message carries base_sha: %s", payload)
	}
}

// A message that claims a pin the parent does not have rolls the whole plan
// back: no subtask is dispatched with a SHA its row does not record.
func TestIntegration_Pins_MissingParentPinRollsBackPlan(t *testing.T) {
	pool := integrationPool(t)
	f := newFixture(t, pool)
	ctx := context.Background()
	taskID := uuid.New()
	msg := f.taskMessageFor(taskID)
	msg.BaseSHA, msg.BaseRef = planPinSHA, "main"
	plan := []plannedSubtask{{repoName: "m103-repo", steps: 1, item: f.subtaskFor(taskID), msg: msg}}
	err := commitDecomposition(ctx, pool, workgraph.New(pool), f.parentWorkID, nil, plan)
	if err == nil || !strings.Contains(err.Error(), "no base pin") {
		t.Fatalf("err = %v, want a missing-pin error", err)
	}
	if workItemExists(t, pool, taskID) {
		t.Fatal("subtask committed despite the failed pin copy")
	}
}
