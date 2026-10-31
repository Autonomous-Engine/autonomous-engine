package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/idempotency"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/plan"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// M2-05: a plan that is still invalid after the repair round blocks the
// parent, and the plan-validation artifact is written in the SAME
// transaction as the block (with work.blocked and the delivery's guard row).

// orphanPlanViolations runs the repair fixture's first plan (AC-2 owned by
// no task) through the validator twice, as the handler would, and returns
// the final violations.
func orphanPlanViolations(t *testing.T) ([]plan.Violation, int) {
	t.Helper()
	brief, first, _ := repairFixture(t)
	out := planWithRepair(uuid.Nil, func(string) ([]decomposedTask, error) { return mustTasks(t, first), nil }, capturedBuilder(brief))
	if !out.rejected() {
		t.Fatalf("fixture plan must be rejected: %+v", out.violations)
	}
	return out.violations, out.attempts
}

func guardedDelivery(t *testing.T, pool *pgxpool.Pool) (context.Context, uuid.UUID) {
	t.Helper()
	eventID := uuid.New()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM processed_events WHERE event_id = $1`, eventID)
	})
	msg := &bus.Message{Topic: bus.TopicTechnicalBriefReady, Value: []byte(`{}`), Headers: map[string]string{bus.HeaderEventID: eventID.String()}}
	return idempotency.WithDelivery(context.Background(), idempotency.NewLedger(pool).NewDelivery(plannerConsumer, msg)), eventID
}

func parentState(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(context.Background(), `SELECT state FROM work_items WHERE id = $1`, id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func planValidationArtifacts(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) []planValidationContent {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT content FROM work_item_artifacts
		WHERE work_item_id = $1 AND artifact_type = 'technical_brief' AND content->>'name' = 'plan-validation'`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []planValidationContent
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var c planValidationContent
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	return out
}

func guardRows(t *testing.T, pool *pgxpool.Pool, eventID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM processed_events WHERE consumer = $1 AND event_id = $2`, plannerConsumer, eventID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestIntegration_PlanValidation_OrphanCriterionBlocksWithArtifactInOneTx(t *testing.T) {
	pool := integrationPool(t)
	f := newFixture(t, pool)
	violations, attempts := orphanPlanViolations(t)

	// The real brief, so the reader can be shown to skip the artifact.
	brief := []byte(`{"narrative":"the real brief","work_id":"` + f.parentWorkID.String() + `"}`)
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO work_item_artifacts (id, work_item_id, artifact_type, content, created_at, agent_id)
		 VALUES ($1, $2, 'technical_brief', $3, now() - interval '1 minute', 'research-coordinator')`,
		uuid.New(), f.parentWorkID, brief); err != nil {
		t.Fatal(err)
	}

	ctx, eventID := guardedDelivery(t, pool)
	if err := blockForPlanValidation(ctx, idempotency.Wrap(ctx, pool), f.parentWorkID, violations, attempts, ""); err != nil {
		t.Fatalf("blockForPlanValidation: %v", err)
	}

	if s := parentState(t, pool, f.parentWorkID); s != string(models.WorkItemStateBlocked) {
		t.Fatalf("parent state %s, want blocked", s)
	}
	arts := planValidationArtifacts(t, pool, f.parentWorkID)
	if len(arts) != 1 || arts[0].Attempt != 2 || arts[0].Name != "plan-validation" {
		t.Fatalf("plan-validation artifacts: %+v", arts)
	}
	var orphan bool
	for _, v := range arts[0].Violations {
		if v.Code == plan.CodeOrphanCriterion && v.CriterionID == "AC-2" {
			orphan = true
		}
	}
	if !orphan {
		t.Fatalf("artifact lacks the orphan violation: %+v", arts[0].Violations)
	}

	// The reason names the violations; work.blocked carries it.
	var reason string
	for _, r := range rowsForAggregate(t, pool, f.parentWorkID) {
		if r.Topic == bus.TopicWorkBlocked {
			var p struct {
				Reason string `json:"reason"`
			}
			_ = json.Unmarshal(r.Payload, &p)
			reason = p.Reason
		}
	}
	if !strings.Contains(reason, "[orphan_criterion] AC-2") || !strings.Contains(reason, "Plan validation failed after 2 attempt(s)") {
		t.Fatalf("work.blocked reason: %q", reason)
	}
	var transitionReason string
	if err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(reason, '') FROM work_item_transitions WHERE work_item_id = $1 AND to_state = 'blocked' ORDER BY timestamp DESC LIMIT 1`,
		f.parentWorkID).Scan(&transitionReason); err != nil {
		t.Fatalf("read the block transition: %v", err)
	} else if transitionReason != reason {
		t.Errorf("transition reason %q differs from work.blocked %q", transitionReason, reason)
	}
	if n := guardRows(t, pool, eventID); n != 1 {
		t.Fatalf("guard rows %d, want 1 (committed with the block)", n)
	}

	// The brief reader still finds the real brief, not the artifact.
	if got := loadStructuredBrief(context.Background(), pool, f.parentWorkID, "fallback"); got.Narrative != "the real brief" {
		t.Fatalf("loadStructuredBrief read %q — the plan-validation artifact must be skipped", got.Narrative)
	}

	// A redelivery of the same event commits nothing.
	ctx2 := idempotency.WithDelivery(context.Background(), idempotency.NewLedger(pool).NewDelivery(plannerConsumer,
		&bus.Message{Topic: bus.TopicTechnicalBriefReady, Value: []byte(`{}`), Headers: map[string]string{bus.HeaderEventID: eventID.String()}}))
	if err := blockForPlanValidation(ctx2, idempotency.Wrap(ctx2, pool), f.parentWorkID, violations, attempts, ""); !idempotency.IsDuplicate(err) {
		t.Fatalf("redelivery: err = %v, want ErrDuplicate", err)
	}
	if arts := planValidationArtifacts(t, pool, f.parentWorkID); len(arts) != 1 {
		t.Fatalf("redelivery wrote another artifact: %d", len(arts))
	}
}

// R7: a decomposition failure blocks the parent and enqueues work.blocked
// (status-orchestrator raises the escalation prompt from it) in one commit
// with the delivery's guard row; a redelivery commits nothing.
func TestIntegration_DecompositionFailureBlocksWithWorkBlocked(t *testing.T) {
	pool := integrationPool(t)
	f := newFixture(t, pool)
	reason := decompositionBlockReason(&decompositionOutputError{err: errors.New(`LLM decomposition rejected: task 4 ("x"): derived criterion id AC-10 is used twice`)})

	ctx, eventID := guardedDelivery(t, pool)
	if err := blockForDecompositionFailure(ctx, idempotency.Wrap(ctx, pool), f.parentWorkID, reason); err != nil {
		t.Fatalf("blockForDecompositionFailure: %v", err)
	}
	if s := parentState(t, pool, f.parentWorkID); s != string(models.WorkItemStateBlocked) {
		t.Fatalf("parent state %s, want blocked", s)
	}
	var got string
	for _, r := range rowsForAggregate(t, pool, f.parentWorkID) {
		if r.Topic == bus.TopicWorkBlocked {
			var p struct {
				Reason string `json:"reason"`
			}
			_ = json.Unmarshal(r.Payload, &p)
			got = p.Reason
		}
	}
	if got != reason {
		t.Fatalf("work.blocked reason %q, want %q", got, reason)
	}
	if n := guardRows(t, pool, eventID); n != 1 {
		t.Fatalf("guard rows %d, want 1", n)
	}
	ctx2 := idempotency.WithDelivery(context.Background(), idempotency.NewLedger(pool).NewDelivery(plannerConsumer,
		&bus.Message{Topic: bus.TopicTechnicalBriefReady, Value: []byte(`{}`), Headers: map[string]string{bus.HeaderEventID: eventID.String()}}))
	if err := blockForDecompositionFailure(ctx2, idempotency.Wrap(ctx2, pool), f.parentWorkID, reason); !idempotency.IsDuplicate(err) {
		t.Fatalf("redelivery: err = %v, want ErrDuplicate", err)
	}
}

func TestIntegration_PlanValidation_ArtifactFailureRollsBackTheBlock(t *testing.T) {
	pool := integrationPool(t)
	f := newFixture(t, pool)
	violations, attempts := orphanPlanViolations(t)

	boom := errors.New("artifact insert failed")
	orig := addArtifactTx
	addArtifactTx = func(context.Context, pgx.Tx, *models.WorkItemArtifact) error { return boom }
	t.Cleanup(func() { addArtifactTx = orig })

	ctx, eventID := guardedDelivery(t, pool)
	before := planValidationBlocks.Value()
	if err := blockForPlanValidation(ctx, idempotency.Wrap(ctx, pool), f.parentWorkID, violations, attempts, ""); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the artifact failure", err)
	}
	if s := parentState(t, pool, f.parentWorkID); s != string(models.WorkItemStatePlanning) {
		t.Fatalf("parent state %s, want planning (the block rolled back with the artifact)", s)
	}
	for _, r := range rowsForAggregate(t, pool, f.parentWorkID) {
		if r.Topic == bus.TopicWorkBlocked {
			t.Fatal("work.blocked was enqueued although the transaction rolled back")
		}
	}
	if n := guardRows(t, pool, eventID); n != 0 {
		t.Fatalf("guard rows %d, want 0 (rolled back)", n)
	}
	if planValidationBlocks.Value() != before {
		t.Fatal("a rolled-back block was counted")
	}
}
