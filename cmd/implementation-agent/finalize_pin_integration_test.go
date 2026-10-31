package main

import (
	"context"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/snapshot"
	"github.com/google/uuid"
)

// M2-02 against PostgreSQL: a drift_detected event commits in the outcome's
// transaction, alongside the evidence row that records the pinned base.
func TestIntegration_FinalizeDriftEventCommitsWithTheOutcome(t *testing.T) {
	requireSh(t)
	pool := integrationPool(t)
	dbf := newDBFixture(t, pool)
	ctx := context.Background()

	g := newFinalizeFixture(t)
	gitRun(t, g.worktree, "checkout", "-q", "main")
	moved := g.commit(t, "upstream.txt", "x\n", "upstream: moved base")
	gitRun(t, g.worktree, "checkout", "-q", g.branch)
	head := g.commit(t, "feature.txt", "value=2\n", "feat: initial")

	repoID := uuid.New()
	in := g.inputs()
	in.task.WorkID = dbf.workID.String()
	in.task.RepositoryID = repoID.String()
	in.task.BaseSHA, in.task.BaseRef = moved, "main"
	in.evidenceStore = evidence.NewStore(pool)
	if _, err := pool.Exec(ctx, `INSERT INTO repositories (id, product_id, name, github_url, base_branch) VALUES ($1, $2, 'm202-impl', $3, 'main')`,
		repoID, dbf.productID, "o/m202-"+repoID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO work_items (id, type, state, title, product_id, parent_id, repository_id) VALUES ($1, 'subtask', 'implementation', 'M2-02 subtask', $2, $3, $4)`,
		in.taskID, dbf.productID, dbf.workID, repoID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM work_items WHERE id = $1`, in.taskID) })

	if !finalizeImplementation(ctx, in, dbf.sink(pool, in.task.TaskID)) {
		t.Fatal("finalize failed")
	}
	evs, err := snapshot.NewStore(pool).Events(ctx, in.taskID)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Kind != snapshot.EventDriftDetected || evs[0].OldSHA != moved || evs[0].NewSHA != g.baseSHA || evs[0].RepositoryID != repoID {
		t.Fatalf("events = %+v, want old=pin %s new=merge-base %s", evs, moved, g.baseSHA)
	}
	if !strings.Contains(evs[0].Detail, head) || !strings.Contains(evs[0].Detail, driftReason) {
		t.Fatalf("detail = %q, want head %s and the reason", evs[0].Detail, head)
	}
	var base string
	if err := pool.QueryRow(ctx, `SELECT base_sha FROM implementation_verifications WHERE work_item_id = $1`, dbf.workID).Scan(&base); err != nil {
		t.Fatal(err)
	}
	if base != g.baseSHA {
		t.Fatalf("evidence base_sha = %s, want the diffed merge-base %s (D1)", base, g.baseSHA)
	}
}
