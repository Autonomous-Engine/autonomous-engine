package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/codexgap"
	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/verifier/stage"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Codex coherence Part 3, end to end through the database: more than
// CODEX_GAP_THRESHOLD empty codex lookups in the parent's implementation run
// mark it ungrounded, and the verification stage then does not approve
// automatically (approve → escalate, evidence flagged ungrounded). A parent
// whose run stayed under the threshold keeps the approval.
func TestIntegration_Verification_UngroundedRunIsNotApproved(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	wg := workgraph.New(pool)

	newParent := func() uuid.UUID {
		id := uuid.New()
		if _, err := pool.Exec(ctx, `INSERT INTO work_items (id, type, state, title, description) VALUES ($1, 'feature', 'blocked', 'p3 gap', 'd')`, id); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM work_items WHERE id = $1`, id) })
		if err := wg.TransitionState(ctx, id, models.WorkItemStateImplementation, "test", "t"); err != nil {
			t.Fatal(err)
		}
		return id
	}
	gaps := func(parent uuid.UUID, n int) {
		for i := 0; i < n; i++ {
			raw, _ := json.Marshal(codexgap.Event{GapID: uuid.NewString(), WorkItemID: parent.String(), CodexTool: "codex_find_module", Query: "q", FallbackTool: "search_code"})
			if err := workgraph.WithTx(ctx, pool, func(tx pgx.Tx) error {
				_, err := codexgap.RecordTx(ctx, tx, raw, codexgap.DefaultThreshold)
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	outcome := func(parent uuid.UUID) *stage.Outcome {
		impl, _ := json.Marshal(map[string]any{"task_id": uuid.NewString(), "work_id": parent.String(), "success": true,
			"evidence": map[string]any{"schema_version": 1, "outcome": "verified"}})
		return &stage.Outcome{
			Message: stage.Message{Decision: stage.DecisionApprove, Reason: "all criteria met", Implementation: impl},
			Result:  stage.Result{Decision: stage.DecisionApprove, Reason: "all criteria met"},
		}
	}

	ungrounded := newParent()
	gaps(ungrounded, codexgap.DefaultThreshold+1)
	// The verification stage runs once the parent is in verification.
	if err := wg.TransitionState(ctx, ungrounded, models.WorkItemStateVerification, "test", "t"); err != nil {
		t.Fatal(err)
	}
	o := outcome(ungrounded)
	applyCodexGrounding(ctx, pool, o, ungrounded, uuid.New())
	if o.Message.Decision != stage.DecisionEscalate || o.Result.Decision != stage.DecisionEscalate {
		t.Fatalf("ungrounded run: decision %s / %s, want escalate", o.Message.Decision, o.Result.Decision)
	}
	var m struct {
		Evidence evidence.ImplementationEvidence `json:"evidence"`
	}
	_ = json.Unmarshal(o.Message.Implementation, &m)
	if !m.Evidence.HasFlag(evidence.FlagUngrounded) {
		t.Errorf("evidence not flagged: %s", o.Message.Implementation)
	}

	grounded := newParent()
	gaps(grounded, codexgap.DefaultThreshold)
	o = outcome(grounded)
	applyCodexGrounding(ctx, pool, o, grounded, uuid.New())
	if o.Message.Decision != stage.DecisionApprove {
		t.Errorf("run within the threshold: decision %s, want approve", o.Message.Decision)
	}
}
