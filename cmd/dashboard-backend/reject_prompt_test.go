package main

import (
	"context"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
)

// While a prompt is pending the orchestrator parks the item in
// awaiting_user, so a Reject answered from the prompt sheet failed with
// "invalid transition from awaiting_user to rejected" and left the item in
// prd_approval with its prompt answered (rerun NEW-10). Reject goes
// through the awaiting_user-aware path like approve and revise.
func TestIntegration_PromptAnswerRejectFromAwaitingUser(t *testing.T) {
	pool := runsIntegrationPool(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		handle func(h *Handlers, id uuid.UUID) error
	}{
		{"prd", func(h *Handlers, id uuid.UUID) error { return h.HandlePRDPromptAnswer(ctx, id, "reject", "not now", "u1") }},
		{"human_review", func(h *Handlers, id uuid.UUID) error { return h.HandleHumanReviewPromptAnswer(ctx, id, "reject", "", "u1") }},
	} {
		workID := uuid.New()
		if _, err := pool.Exec(ctx, `INSERT INTO work_items (id, type, state, title, description) VALUES ($1, 'feature', 'awaiting_user', 'reject me', 'd')`, workID); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = pool.Exec(ctx, `DELETE FROM outbox_events WHERE aggregate_id = $1`, workID)
			_, _ = pool.Exec(ctx, `DELETE FROM work_items WHERE id = $1`, workID)
		})
		h := NewHandlers(workgraph.New(pool), nil, nil, nil, nil, nil)
		if err := tc.handle(h, workID); err != nil {
			t.Fatalf("%s: reject via prompt answer: %v", tc.name, err)
		}
		var state string
		_ = pool.QueryRow(ctx, `SELECT state FROM work_items WHERE id = $1`, workID).Scan(&state)
		if state != string(models.WorkItemStateRejected) {
			t.Fatalf("%s: state = %q, want rejected", tc.name, state)
		}
	}
}
