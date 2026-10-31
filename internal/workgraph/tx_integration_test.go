package workgraph_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/db"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/outbox"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func migratedAwaitPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	p := awaitPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := db.Migrate(ctx, p, filepath.Join("..", "..", "migrations")); err != nil {
		p.Close()
		t.Fatalf("migrate: %v", err)
	}
	return p
}

type txCounts struct {
	state                                       models.WorkItemState
	transitions, artifacts, assignments, events int
}

func readCounts(t *testing.T, p *pgxpool.Pool, wiID uuid.UUID) txCounts {
	t.Helper()
	ctx := context.Background()
	var c txCounts
	if err := p.QueryRow(ctx, `SELECT state FROM work_items WHERE id = $1`, wiID).Scan(&c.state); err != nil {
		t.Fatalf("state: %v", err)
	}
	for _, q := range []struct {
		sql string
		dst *int
	}{
		{`SELECT COUNT(*) FROM work_item_transitions WHERE work_item_id = $1`, &c.transitions},
		{`SELECT COUNT(*) FROM work_item_artifacts WHERE work_item_id = $1`, &c.artifacts},
		{`SELECT COUNT(*) FROM agent_assignments WHERE work_item_id = $1`, &c.assignments},
		{`SELECT COUNT(*) FROM outbox_events WHERE aggregate_id = $1`, &c.events},
	} {
		if err := p.QueryRow(ctx, q.sql, wiID).Scan(q.dst); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}
	return c
}

// convertedPublish is the shape a service ticket writes: assignment,
// artifact, transition, assignment completion and the event, in one WithTx.
func convertedPublish(ctx context.Context, p *pgxpool.Pool, wg *workgraph.WorkGraph, wiID uuid.UUID, fail error) (models.WorkItemState, error) {
	var from models.WorkItemState
	err := workgraph.WithTx(ctx, p, func(tx pgx.Tx) error {
		a := &models.AgentAssignment{WorkItemID: wiID, AgentType: models.AgentTypeProduct, AgentInstanceID: "it", Status: models.AssignmentStatusActive}
		if err := wg.AssignAgentTx(ctx, tx, a); err != nil {
			return err
		}
		if err := wg.AddArtifactTx(ctx, tx, &models.WorkItemArtifact{WorkItemID: wiID, Type: models.ArtifactTypePRD, Content: []byte(`{"prd":"x"}`), AgentID: "it"}); err != nil {
			return err
		}
		var err error
		if from, err = wg.TransitionStateTx(ctx, tx, wiID, models.WorkItemStatePRD, "it", "integration"); err != nil {
			return err
		}
		if err := wg.CompleteAssignmentTx(ctx, tx, a.ID, models.AssignmentStatusCompleted); err != nil {
			return err
		}
		if err := outbox.EnqueueJSON(ctx, tx, wiID, "state.changed", wiID.String(), map[string]string{"work_id": wiID.String(), "from_state": string(from), "to_state": "prd"}); err != nil {
			return err
		}
		return fail
	})
	return from, err
}

func cleanupOutbox(p *pgxpool.Pool, wiID uuid.UUID) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _ = p.Exec(ctx, `DELETE FROM outbox_events WHERE aggregate_id = $1`, wiID)
	_, _ = p.Exec(ctx, `DELETE FROM work_item_transitions WHERE work_item_id = $1`, wiID)
	_, _ = p.Exec(ctx, `DELETE FROM work_item_artifacts WHERE work_item_id = $1`, wiID)
	_, _ = p.Exec(ctx, `DELETE FROM agent_assignments WHERE work_item_id = $1`, wiID)
}

func TestIntegration_WithTx_TxVariantsCommitWithEvent(t *testing.T) {
	p := migratedAwaitPool(t)
	defer p.Close()
	_, wiID := fixtureProductAndWI(t, p, "intake")
	defer cleanupOutbox(p, wiID)

	from, err := convertedPublish(context.Background(), p, workgraph.New(p), wiID, nil)
	if err != nil {
		t.Fatalf("convertedPublish: %v", err)
	}
	if from != models.WorkItemStateIntake {
		t.Errorf("from = %q, want intake", from)
	}
	got := readCounts(t, p, wiID)
	want := txCounts{state: models.WorkItemStatePRD, transitions: 1, artifacts: 1, assignments: 1, events: 1}
	if got != want {
		t.Fatalf("after commit: %+v, want %+v", got, want)
	}
	var status string
	if err := p.QueryRow(context.Background(), `SELECT status FROM agent_assignments WHERE work_item_id = $1`, wiID).Scan(&status); err != nil || status != "completed" {
		t.Errorf("assignment status = %q (%v), want completed", status, err)
	}
}

func TestIntegration_WithTx_RollbackLeavesNothing(t *testing.T) {
	p := migratedAwaitPool(t)
	defer p.Close()
	_, wiID := fixtureProductAndWI(t, p, "intake")
	defer cleanupOutbox(p, wiID)

	later := errors.New("later step failed")
	if _, err := convertedPublish(context.Background(), p, workgraph.New(p), wiID, later); !errors.Is(err, later) {
		t.Fatalf("err = %v, want the later failure", err)
	}
	got := readCounts(t, p, wiID)
	want := txCounts{state: models.WorkItemStateIntake}
	if got != want {
		t.Fatalf("after rollback: %+v, want %+v", got, want)
	}
}

// The pooled TransitionState now runs through WithTx; it must still commit.
func TestIntegration_TransitionState_PooledStillCommits(t *testing.T) {
	p := migratedAwaitPool(t)
	defer p.Close()
	_, wiID := fixtureProductAndWI(t, p, "intake")
	defer cleanupOutbox(p, wiID)

	wg := workgraph.New(p)
	if err := wg.TransitionState(context.Background(), wiID, models.WorkItemStatePRD, "it", "pooled"); err != nil {
		t.Fatalf("TransitionState: %v", err)
	}
	if err := wg.TransitionState(context.Background(), wiID, models.WorkItemStateReleased, "it", "invalid"); err == nil {
		t.Error("invalid transition accepted")
	}
	got := readCounts(t, p, wiID)
	if got.state != models.WorkItemStatePRD || got.transitions != 1 {
		t.Fatalf("after pooled transition: %+v", got)
	}
}
