package stage

import (
	"context"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
)

// Codex coherence Part 1 review: a stage failure on a subtask a codex
// re-plan superseded does not block the parent that is being planned again;
// the result and the event are still recorded. A failure on a live subtask
// still blocks it.
func TestIntegration_StageFailureOnASupersededSubtaskDoesNotBlockTheParent(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	run := func(t *testing.T, superseded bool) string {
		t.Helper()
		parent, task := uuid.New(), uuid.New()
		reason := "tests failed"
		if superseded {
			reason = "codex_changed: superseded by the re-plan of the parent (modules a (v1 → v2))"
		}
		for _, q := range []struct {
			sql  string
			args []any
		}{
			{`INSERT INTO work_items (id, type, state, title, description) VALUES ($1, 'feature', 'planning', 'stage parent', 'd')`, []any{parent}},
			{`INSERT INTO work_items (id, type, state, title, description, parent_id) VALUES ($1, 'subtask', 'blocked', 'stage subtask', 'd', $2)`, []any{task, parent}},
			{`INSERT INTO work_item_transitions (id, work_item_id, from_state, to_state, agent_id, reason) VALUES ($1, $2, 'implementation', 'blocked', 'test', $3)`, []any{uuid.New(), task, reason}},
		} {
			if _, err := pool.Exec(ctx, q.sql, q.args...); err != nil {
				t.Fatal(err)
			}
		}
		t.Cleanup(func() {
			c := context.Background()
			_, _ = pool.Exec(c, `DELETE FROM outbox_events WHERE aggregate_id = $1`, parent)
			_, _ = pool.Exec(c, `DELETE FROM work_items WHERE parent_id = $1`, parent)
			_, _ = pool.Exec(c, `DELETE FROM work_items WHERE id = $1`, parent)
		})
		o := &Outcome{
			Message: Message{WorkID: parent.String(), TaskID: task.String(), Decision: DecisionError, Reason: "clone missing"},
			Result:  Result{WorkID: parent, TaskID: task, Decision: DecisionError, Reason: "clone missing"},
			Block:   "verification stage failed: clone missing",
		}
		if err := Commit(ctx, pool, workgraph.New(pool), "review-agent", o); err != nil {
			t.Fatal(err)
		}
		var state string
		if err := pool.QueryRow(ctx, `SELECT state::text FROM work_items WHERE id = $1`, parent).Scan(&state); err != nil {
			t.Fatal(err)
		}
		var results int
		_ = pool.QueryRow(ctx, `SELECT count(*) FROM verification_results WHERE work_item_id = $1`, parent).Scan(&results)
		if results != 1 {
			t.Errorf("results = %d, want 1", results)
		}
		return state
	}
	if s := run(t, true); s != "planning" {
		t.Errorf("superseded subtask's failure moved the re-planning parent to %s", s)
	}
	if s := run(t, false); s != "blocked" {
		t.Errorf("live subtask's failure left the parent %s, want blocked", s)
	}
}
