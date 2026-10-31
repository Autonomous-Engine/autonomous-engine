package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/snapshot"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// M2-03 drift, end to end through the guarded handler against a real
// PostgreSQL and real git: the work item is pinned at the base, the agent
// branch is pushed, then origin's main moves. A conflicting move blocks the
// parent (reason names the path, drift_detected row, escalation prompt, no
// PR); a clean move records the drift and gates the PR with base_drift.

// seedPinnedGatedParent is seedGatedParent with the repository pointed at the
// fixture clone and the parent pinned at r.pin.
func seedPinnedGatedParent(t *testing.T, pool *pgxpool.Pool, r *driftRepo) (workID, taskID, repoID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	workID, taskID = seedGatedParent(t, pool, "implementation")
	if err := pool.QueryRow(ctx, `SELECT repository_id FROM work_items WHERE id = $1`, taskID).Scan(&repoID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE repositories SET clone_path = $2 WHERE id = $1`, repoID, r.clone); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO work_item_base_shas (work_item_id, repository_id, base_ref, base_sha) VALUES ($1, $2, 'main', $3)`, workID, repoID, r.pin); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM base_sha_events WHERE work_item_id = $1`, workID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM work_item_base_shas WHERE work_item_id = $1`, workID)
	})
	return workID, taskID, repoID
}

func agentBranch(workID, taskID uuid.UUID) string {
	return "agent/" + workID.String() + "/" + taskID.String()[:8]
}

func driftEvents(t *testing.T, pool *pgxpool.Pool, workID uuid.UUID) []snapshot.Event {
	t.Helper()
	evs, err := snapshot.NewStore(pool).Events(context.Background(), workID)
	if err != nil {
		t.Fatal(err)
	}
	var out []snapshot.Event
	for _, ev := range evs {
		if ev.Kind == snapshot.EventDriftDetected {
			out = append(out, ev)
		}
	}
	return out
}

func TestIntegration_Drift_ConflictingBaseMoveBlocksTheParent(t *testing.T) {
	pool := integrationPool(t)
	r := newDriftRepo(t)
	workID, taskID, repoID := seedPinnedGatedParent(t, pool, r)
	r.pushAgentBranch(agentBranch(workID, taskID), "a.txt", "line 1\nagent\nline 3\n")
	tip := r.moveMain("a.txt", "line 1\nupstream\nline 3\n")

	if err := guardedCoordinator(pool)(context.Background(), completionMessage(t, pool, workID, taskID)); err != nil {
		t.Fatal(err)
	}

	var state, reason string
	if err := pool.QueryRow(context.Background(), `SELECT state FROM work_items WHERE id = $1`, workID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "blocked" {
		t.Fatalf("parent state = %s, want blocked", state)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT reason FROM work_item_transitions WHERE work_item_id = $1 AND to_state = 'blocked' ORDER BY "timestamp" DESC LIMIT 1`, workID).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"conflicts in: a.txt", r.pin[:12], tip[:12]} {
		if !strings.Contains(reason, want) {
			t.Errorf("block reason %q lacks %q", reason, want)
		}
	}
	evs := driftEvents(t, pool, workID)
	if len(evs) != 1 || evs[0].OldSHA != r.pin || evs[0].NewSHA != tip || evs[0].RepositoryID != repoID || !strings.Contains(evs[0].Detail, "a.txt") {
		t.Fatalf("drift events = %+v", evs)
	}
	if n := count(t, pool, `SELECT count(*) FROM user_interactions WHERE work_item_id = $1 AND trigger = 'blocked' AND state = 'pending' AND asking_agent = 'pr-coordinator'`, workID); n != 1 {
		t.Errorf("escalation prompts = %d, want 1", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM pending_pr_changes WHERE work_item_id = $1`, workID); n != 0 {
		t.Errorf("pending PR rows = %d, want none for a blocked parent", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND topic = 'work.blocked'`, workID); n != 1 {
		t.Errorf("work.blocked events = %d, want 1", n)
	}
}

func TestIntegration_Drift_CleanBaseMoveGatesThePRWithBaseDrift(t *testing.T) {
	pool := integrationPool(t)
	r := newDriftRepo(t)
	workID, taskID, _ := seedPinnedGatedParent(t, pool, r)
	r.pushAgentBranch(agentBranch(workID, taskID), "a.txt", "line 1\nagent\nline 3\n")
	tip := r.moveMain("b.txt", "moved\n")

	if err := guardedCoordinator(pool)(context.Background(), completionMessage(t, pool, workID, taskID)); err != nil {
		t.Fatal(err)
	}

	var desc string
	if err := pool.QueryRow(context.Background(), `SELECT pr_description FROM pending_pr_changes WHERE work_item_id = $1`, workID).Scan(&desc); err != nil {
		t.Fatalf("pending PR row: %v", err)
	}
	if want := "base moved from " + r.pin[:12] + " to " + tip[:12] + "; merges cleanly"; !strings.Contains(desc, want) {
		t.Errorf("PR description lacks %q:\n%s", want, desc)
	}
	var payload []byte
	if err := pool.QueryRow(context.Background(), `SELECT payload FROM outbox_events WHERE aggregate_id = $1 AND topic = 'pr.gated'`, workID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var msg prReadyMessage
	if err := json.Unmarshal(payload, &msg); err != nil {
		t.Fatal(err)
	}
	if msg.BaseSHA != r.pin || msg.BaseDrift == nil || !msg.BaseDrift.Clean || msg.BaseDrift.Old != r.pin || msg.BaseDrift.New != tip {
		t.Fatalf("pr.gated payload base = %s drift = %+v", msg.BaseSHA, msg.BaseDrift)
	}
	if evs := driftEvents(t, pool, workID); len(evs) != 1 || evs[0].NewSHA != tip {
		t.Fatalf("drift events = %+v", evs)
	}
	var state string
	_ = pool.QueryRow(context.Background(), `SELECT state FROM work_items WHERE id = $1`, workID).Scan(&state)
	if state == "blocked" {
		t.Fatal("a clean drift must not block")
	}
}

// The drift check cannot run (origin unreachable): fail closed — blocked with
// the cause, one pending escalation prompt, no PR, no drift event.
func TestIntegration_Drift_UnevaluableBaseBlocksWithoutAPR(t *testing.T) {
	pool := integrationPool(t)
	r := newDriftRepo(t)
	workID, taskID, _ := seedPinnedGatedParent(t, pool, r)
	r.pushAgentBranch(agentBranch(workID, taskID), "a.txt", "x\n")
	r.git(r.clone, "remote", "set-url", "origin", r.root+"/gone.git")

	if err := guardedCoordinator(pool)(context.Background(), completionMessage(t, pool, workID, taskID)); err != nil {
		t.Fatal(err)
	}
	var state, reason string
	if err := pool.QueryRow(context.Background(), `SELECT state FROM work_items WHERE id = $1`, workID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT reason FROM work_item_transitions WHERE work_item_id = $1 AND to_state = 'blocked' ORDER BY "timestamp" DESC LIMIT 1`, workID).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if state != "blocked" || !strings.HasPrefix(reason, "base drift could not be evaluated for ") {
		t.Fatalf("state=%s reason=%q", state, reason)
	}
	if n := count(t, pool, `SELECT count(*) FROM user_interactions WHERE work_item_id = $1 AND trigger = 'blocked' AND state = 'pending'`, workID); n != 1 {
		t.Errorf("pending escalation prompts = %d, want 1", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM pending_pr_changes WHERE work_item_id = $1`, workID); n != 0 {
		t.Errorf("pending PR rows = %d, want none", n)
	}
	if evs := driftEvents(t, pool, workID); len(evs) != 0 {
		t.Errorf("drift events = %+v, want none (nothing was observed)", evs)
	}
}

func TestIntegration_Drift_UnmovedBaseRecordsNothing(t *testing.T) {
	pool := integrationPool(t)
	r := newDriftRepo(t)
	workID, taskID, _ := seedPinnedGatedParent(t, pool, r)
	r.pushAgentBranch(agentBranch(workID, taskID), "a.txt", "line 1\nagent\nline 3\n")

	if err := guardedCoordinator(pool)(context.Background(), completionMessage(t, pool, workID, taskID)); err != nil {
		t.Fatal(err)
	}
	if n := count(t, pool, `SELECT count(*) FROM pending_pr_changes WHERE work_item_id = $1`, workID); n != 1 {
		t.Fatalf("pending PR rows = %d, want 1", n)
	}
	if evs := driftEvents(t, pool, workID); len(evs) != 0 {
		t.Fatalf("drift events = %+v, want none", evs)
	}
}
