package guards

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/outbox"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// fakeTx is a pgx.Tx that records statements. QueryRow answers the
// transition's SELECT ... FOR UPDATE with state; anything else panics via the
// nil embedded interface (including Commit/Rollback, which BlockWorkItemTx
// must leave to the caller).
type fakeTx struct {
	pgx.Tx
	state    models.WorkItemState
	queryErr error
	sqls     []string
	args     [][]any
}

func (f *fakeTx) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	f.sqls = append(f.sqls, sql)
	f.args = append(f.args, args)
	return pgconn.CommandTag{}, nil
}

func (f *fakeTx) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	f.sqls = append(f.sqls, sql)
	f.args = append(f.args, args)
	return fakeRow{f}
}

type fakeRow struct{ f *fakeTx }

func (r fakeRow) Scan(dest ...any) error {
	if r.f.queryErr != nil {
		return r.f.queryErr
	}
	*(dest[0].(*models.WorkItemState)) = r.f.state
	return nil
}

func TestBlockWorkItemTx_TransitionsAndEnqueuesWorkBlocked(t *testing.T) {
	tx := &fakeTx{state: models.WorkItemStateImplementation}
	parent, workID := uuid.New(), uuid.New()
	if err := BlockWorkItemTx(context.Background(), tx, parent, workID, "agent", "tests failed"); err != nil {
		t.Fatalf("BlockWorkItemTx: %v", err)
	}
	if len(tx.sqls) != 4 {
		t.Fatalf("statements = %d (%q), want lock, update, transition insert, outbox insert", len(tx.sqls), tx.sqls)
	}
	if !strings.Contains(tx.sqls[0], "FOR UPDATE") ||
		!strings.Contains(tx.sqls[1], "UPDATE work_items SET state") ||
		!strings.Contains(tx.sqls[2], "INSERT INTO work_item_transitions") ||
		!strings.Contains(tx.sqls[3], "INSERT INTO outbox_events") {
		t.Errorf("statements out of order: %q", tx.sqls)
	}
	if tx.args[1][0] != models.WorkItemStateBlocked || tx.args[1][2] != workID {
		t.Errorf("state update args = %v, want blocked for %s", tx.args[1], workID)
	}
	if tx.args[2][5] != "tests failed" || tx.args[2][4] != "agent" {
		t.Errorf("transition args = %v", tx.args[2])
	}
	ev := tx.args[3]
	if ev[1] != outbox.AggregateTypeWorkItem || ev[2] != parent {
		t.Errorf("aggregate = %v/%v, want work_item/%s", ev[1], ev[2], parent)
	}
	if ev[3] != bus.TopicWorkBlocked || ev[4] != workID.String() {
		t.Errorf("topic/key = %v/%v, want %s/%s", ev[3], ev[4], bus.TopicWorkBlocked, workID)
	}
	var body map[string]any
	if err := json.Unmarshal(ev[5].([]byte), &body); err != nil {
		t.Fatal(err)
	}
	if body["work_id"] != workID.String() || body["reason"] != "tests failed" {
		t.Errorf("payload = %v", body)
	}
	delete(body, outbox.FieldEventID)
	if len(body) != 2 {
		t.Errorf("payload has fields beyond work_id, reason (+event_id): %v", body)
	}
}

// The existing work.blocked shape is {work_id, reason}, as published by
// change-reconciler and consumed by status-orchestrator.
func TestWorkBlockedPayload_JSONShape(t *testing.T) {
	b, err := json.Marshal(WorkBlockedPayload{WorkID: "w", Reason: "r"})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"work_id":"w","reason":"r"}` {
		t.Errorf("payload = %s", b)
	}
}

func TestBlockWorkItemTx_AlreadyBlockedIsNoOp(t *testing.T) {
	tx := &fakeTx{state: models.WorkItemStateBlocked}
	if err := BlockWorkItemTx(context.Background(), tx, uuid.New(), uuid.New(), "a", "r"); err != nil {
		t.Fatalf("already blocked: err = %v, want nil", err)
	}
	if len(tx.sqls) != 1 {
		t.Errorf("statements = %q, want only the locking select", tx.sqls)
	}
}

func TestBlockWorkItemTx_InvalidTransitionFromTerminalStateErrors(t *testing.T) {
	tx := &fakeTx{state: models.WorkItemStateReleased}
	err := BlockWorkItemTx(context.Background(), tx, uuid.New(), uuid.New(), "a", "r")
	if err == nil {
		t.Fatal("released → blocked accepted")
	}
	if len(tx.sqls) != 1 {
		t.Errorf("statements = %q, want nothing written", tx.sqls)
	}
}

func TestBlockWorkItemTx_NotFoundErrors(t *testing.T) {
	tx := &fakeTx{queryErr: pgx.ErrNoRows}
	if err := BlockWorkItemTx(context.Background(), tx, uuid.New(), uuid.New(), "a", "r"); err == nil {
		t.Fatal("missing work item accepted")
	}
}

func TestBlockWorkItemTx_RejectsBadArguments(t *testing.T) {
	ctx := context.Background()
	if err := BlockWorkItemTx(ctx, nil, uuid.New(), uuid.New(), "a", "r"); err == nil {
		t.Error("nil tx accepted")
	}
	tx := &fakeTx{state: models.WorkItemStateImplementation}
	if err := BlockWorkItemTx(ctx, tx, uuid.Nil, uuid.New(), "a", "r"); err == nil {
		t.Error("nil aggregate accepted")
	}
	if err := BlockWorkItemTx(ctx, tx, uuid.New(), uuid.Nil, "a", "r"); err == nil {
		t.Error("nil work id accepted")
	}
	if len(tx.sqls) != 0 {
		t.Errorf("rejected calls reached the database: %q", tx.sqls)
	}
}
