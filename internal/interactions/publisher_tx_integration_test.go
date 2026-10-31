package interactions_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/db"
	"github.com/autonomousengine/autonomous-engine/internal/interactions"
	"github.com/autonomousengine/autonomous-engine/internal/outbox"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// migratedPool is pool() plus the repository's migrations, so the test runs
// against a fresh database (it needs 047 outbox_events).
func migratedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	p := pool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := db.Migrate(ctx, p, filepath.Join("..", "..", "migrations")); err != nil {
		p.Close()
		t.Fatalf("migrate: %v", err)
	}
	// outbox_events has no foreign key, so the product cleanup does not reach
	// it; each test deletes its own rows with deleteOutboxRows.
	return p
}

func countPublishTxRows(t *testing.T, p *pgxpool.Pool, promptID, workItemID uuid.UUID) (prompts, events int) {
	t.Helper()
	ctx := context.Background()
	if err := p.QueryRow(ctx, `SELECT COUNT(*) FROM user_interactions WHERE id = $1`, promptID).Scan(&prompts); err != nil {
		t.Fatalf("count prompts: %v", err)
	}
	if err := p.QueryRow(ctx, `SELECT COUNT(*) FROM outbox_events WHERE aggregate_id = $1`, workItemID).Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	return prompts, events
}

func deleteOutboxRows(p *pgxpool.Pool, workItemID uuid.UUID) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _ = p.Exec(ctx, `DELETE FROM outbox_events WHERE aggregate_id = $1`, workItemID)
}

func TestIntegration_PublishTx_RowAndEventCommitTogether(t *testing.T) {
	p := migratedPool(t)
	defer p.Close()
	ctx := context.Background()

	productID := mustInsertFixtureProduct(t, p)
	workItemID := mustInsertFixtureWorkItem(t, p, productID)
	defer deleteOutboxRows(p, workItemID)
	prompt := newPendingPrompt(workItemID, productID)

	pub := interactions.NewPublisher(nil)
	if err := workgraph.WithTx(ctx, p, func(tx pgx.Tx) error {
		return pub.PublishTx(ctx, tx, workItemID, prompt)
	}); err != nil {
		t.Fatalf("WithTx(PublishTx): %v", err)
	}

	prompts, events := countPublishTxRows(t, p, prompt.ID, workItemID)
	if prompts != 1 || events != 1 {
		t.Fatalf("after commit: prompts=%d events=%d, want 1/1", prompts, events)
	}

	got, err := interactions.NewStore(p).GetByID(ctx, prompt.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Headline != prompt.Headline || got.State != interactions.StatePending {
		t.Errorf("row = %+v", got)
	}

	var (
		aggType, topic, key string
		payload             []byte
		eventID             uuid.UUID
	)
	if err := p.QueryRow(ctx, `
		SELECT id, aggregate_type, topic, key, payload
		FROM outbox_events WHERE aggregate_id = $1`, workItemID,
	).Scan(&eventID, &aggType, &topic, &key, &payload); err != nil {
		t.Fatalf("read outbox row: %v", err)
	}
	if aggType != outbox.AggregateTypeWorkItem || topic != bus.TopicUserPromptRequested || key != workItemID.String() {
		t.Errorf("outbox row = %s/%s/%s", aggType, topic, key)
	}
	var body map[string]any
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if body["id"] != prompt.ID.String() || body[outbox.FieldEventID] != eventID.String() {
		t.Errorf("payload id=%v event_id=%v, want %s / %s", body["id"], body[outbox.FieldEventID], prompt.ID, eventID)
	}
}

func TestIntegration_PublishTx_RollbackLeavesNeither(t *testing.T) {
	p := migratedPool(t)
	defer p.Close()
	ctx := context.Background()

	productID := mustInsertFixtureProduct(t, p)
	workItemID := mustInsertFixtureWorkItem(t, p, productID)
	defer deleteOutboxRows(p, workItemID)
	prompt := newPendingPrompt(workItemID, productID)

	// A later step in the same unit of work fails after PublishTx succeeded:
	// both writes must disappear with the transaction.
	later := errors.New("later step failed")
	err := workgraph.WithTx(ctx, p, func(tx pgx.Tx) error {
		if err := interactions.NewPublisher(nil).PublishTx(ctx, tx, workItemID, prompt); err != nil {
			return err
		}
		return later
	})
	if !errors.Is(err, later) {
		t.Fatalf("WithTx err = %v, want the later failure", err)
	}
	if prompts, events := countPublishTxRows(t, p, prompt.ID, workItemID); prompts != 0 || events != 0 {
		t.Fatalf("after rollback: prompts=%d events=%d, want 0/0", prompts, events)
	}
}

func TestIntegration_PublishTx_FailedRowInsertEnqueuesNothing(t *testing.T) {
	p := migratedPool(t)
	defer p.Close()
	ctx := context.Background()

	productID := mustInsertFixtureProduct(t, p)
	workItemID := mustInsertFixtureWorkItem(t, p, productID)
	defer deleteOutboxRows(p, workItemID)
	prompt := newPendingPrompt(workItemID, uuid.New()) // unknown product: FK violation

	err := workgraph.WithTx(ctx, p, func(tx pgx.Tx) error {
		return interactions.NewPublisher(nil).PublishTx(ctx, tx, workItemID, prompt)
	})
	if err == nil {
		t.Fatal("PublishTx with an unknown product committed")
	}
	if prompts, events := countPublishTxRows(t, p, prompt.ID, workItemID); prompts != 0 || events != 0 {
		t.Fatalf("after failure: prompts=%d events=%d, want 0/0", prompts, events)
	}
}
