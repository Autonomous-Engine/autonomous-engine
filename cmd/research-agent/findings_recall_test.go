package main

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Earlier tasks' and rounds' findings are the work item's memory: a task
// starts from them, newest first, and never from its own re-delivered
// rows.
func TestIntegration_LoadKnownFindingsExcludesThisTask(t *testing.T) {
	pool := budgetIntegrationPool(t)
	ctx := context.Background()
	workID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO work_items (id, type, state, title) VALUES ($1, 'feature', 'research', 'recall')`, workID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM work_items WHERE id = $1`, workID) })
	base := time.Now().UTC().Add(-time.Hour)
	insert := func(task, claim string, at time.Time) {
		if _, err := pool.Exec(ctx, `INSERT INTO research_findings (work_item_id, task_id, research_type, claim, path, start_line, end_line, excerpt, created_at)
			VALUES ($1, $2, 'code_trace', $3, 'a.go', 1, 2, 'x', $4)`, workID, task, claim, at); err != nil {
			t.Fatal(err)
		}
	}
	insert("t1", "older", base)
	insert("t1", "newer", base.Add(time.Minute))
	insert("t2", "mine", base.Add(2*time.Minute))

	fs, err := loadKnownFindings(ctx, pool, workID, "t2")
	if err != nil {
		t.Fatal(err)
	}
	if len(fs) != 2 || fs[0].Claim != "newer" || fs[1].Claim != "older" {
		t.Fatalf("findings = %+v, want t1's two, newest first", fs)
	}
	if fs[0].Path != "a.go" || fs[0].StartLine != 1 || fs[0].EndLine != 2 || fs[0].Excerpt != "x" {
		t.Fatalf("citation = %+v", fs[0])
	}
}
