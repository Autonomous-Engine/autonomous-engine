package snapshot

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

// TestIntegration_ReferencedSHAsIgnoresTerminalItems: a pin keeps its
// snapshot alive only while its work item is not finished (passed, failed,
// released, cancelled, rejected) AND its parent, if any, is not finished.
// blocked items keep theirs.
func TestIntegration_ReferencedSHAsIgnoresTerminalItems(t *testing.T) {
	pool := integrationPool(t)
	f := newFixture(t, pool)
	s := NewStore(pool)
	ctx := context.Background()
	// Parent pinned at shaA, its subtask at shaB (distinct so each pin's
	// effect is visible).
	if err := inTx(t, pool, func(tx pgx.Tx) error {
		return s.UpsertTx(ctx, tx,
			Pin{WorkItemID: f.parent, RepositoryID: f.repo, BaseRef: "main", BaseSHA: shaA},
			Pin{WorkItemID: f.child, RepositoryID: f.repo, BaseRef: "main", BaseSHA: shaB})
	}); err != nil {
		t.Fatal(err)
	}
	set := func(id any, state string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE work_items SET state = $2 WHERE id = $1`, id, state); err != nil {
			t.Fatal(err)
		}
	}
	ref := func() map[string]bool {
		t.Helper()
		r, err := ReferencedSHAs(ctx, pool, f.repo)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := ref(); !r[shaA] || !r[shaB] {
		t.Fatalf("both pins of live items must be referenced: %v", r)
	}
	set(f.child, "blocked")
	if r := ref(); !r[shaB] {
		t.Fatalf("a blocked subtask's pin must stay referenced: %v", r)
	}
	for _, st := range []string{"passed", "failed"} {
		set(f.child, st)
		if r := ref(); r[shaB] || !r[shaA] {
			t.Fatalf("a %s subtask releases its pin; the live parent keeps its own: %v", st, r)
		}
	}
	// A live subtask of a finished parent does not hold the snapshot.
	set(f.child, "implementation")
	set(f.parent, "released")
	if r := ref(); len(r) != 0 {
		t.Fatalf("released parent: nothing may stay referenced, got %v", r)
	}
	set(f.parent, "cancelled")
	if r := ref(); len(r) != 0 {
		t.Fatalf("cancelled parent: nothing may stay referenced, got %v", r)
	}
	if other, _ := ReferencedSHAs(ctx, pool, f.repo2); len(other) != 0 {
		t.Fatalf("repo2 has no pins, got %v", other)
	}
}
