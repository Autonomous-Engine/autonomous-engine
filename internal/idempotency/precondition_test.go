package idempotency

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// DSN-free tests for CheckPrecondition. The fake answers the state reads and
// records the ledger row; the paths that transition an item to blocked go
// through guards.BlockWorkItemTx (real SQL) and are covered by the
// integration tests in ledger_integration_test.go, not here.

type itemRow struct{ state, previous string }

type stateDB struct {
	*fakeDB
	states map[uuid.UUID]itemRow
}

func (s *stateDB) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := s.fakeDB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &stateTx{fakeTx: tx.(*fakeTx), states: s.states}, nil
}

type stateTx struct {
	*fakeTx
	states map[uuid.UUID]itemRow
}

func (t *stateTx) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	if !strings.Contains(sql, "FROM work_items") {
		return stateRow{err: errors.New("unexpected query: " + sql)}
	}
	r, ok := t.states[args[0].(uuid.UUID)]
	if !ok {
		return stateRow{err: pgx.ErrNoRows}
	}
	return stateRow{row: r}
}

type stateRow struct {
	row itemRow
	err error
}

func (r stateRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*(dest[0].(*string)) = r.row.state
	*(dest[1].(*string)) = r.row.previous
	return nil
}

func newStateDB(states map[uuid.UUID]itemRow) *stateDB {
	return &stateDB{fakeDB: newFakeDB(), states: states}
}

var refused = []models.WorkItemState{
	models.WorkItemStateCancelled, models.WorkItemStateBlocked,
	models.WorkItemStateFailed, models.WorkItemStateRejected,
}

func TestCheckPrecondition_RejectsUnsetStateRule(t *testing.T) {
	db := newStateDB(nil)
	_, err := CheckPrecondition(context.Background(), db, Precondition{Action: "x", WorkID: uuid.New()})
	if err == nil || !strings.Contains(err.Error(), "no state rule") {
		t.Fatalf("err = %v, want a 'no state rule' error", err)
	}
	if db.begins != 0 {
		t.Fatal("a misconfigured precondition must not open a transaction")
	}
}

func TestCheckPrecondition_RejectsBothRules(t *testing.T) {
	db := newStateDB(nil)
	_, err := CheckPrecondition(context.Background(), db, Precondition{
		Action:   "x",
		WorkID:   uuid.New(),
		Expected: []models.WorkItemState{models.WorkItemStateQA},
		NotIn:    refused,
	})
	if err == nil || !strings.Contains(err.Error(), "ambiguous state rule") {
		t.Fatalf("err = %v, want an 'ambiguous state rule' error", err)
	}
	if db.begins != 0 {
		t.Fatal("a misconfigured precondition must not open a transaction")
	}
}

func TestCheckPrecondition_RejectsMissingWorkID(t *testing.T) {
	_, err := CheckPrecondition(context.Background(), newStateDB(nil), Precondition{NotIn: refused})
	if err == nil {
		t.Fatal("a precondition without a work id must be an error")
	}
}

// guardedCtx attaches a delivery so the guard row's fate is observable.
func guardedCtx(db *stateDB) (context.Context, *Delivery, TxBeginner) {
	l := NewLedger(db.fakeDB)
	d := l.NewDelivery("svc", message("t", uuid.New()))
	ctx := WithDelivery(context.Background(), d)
	return ctx, d, Wrap(ctx, db)
}

func TestCheckPrecondition_DenyListAllowsUnlistedStates(t *testing.T) {
	for _, s := range []string{"planning", "implementation", "review", "qa", "passed", "released"} {
		id := uuid.New()
		db := newStateDB(map[uuid.UUID]itemRow{id: {state: s}})
		ctx, d, gdb := guardedCtx(db)
		res, err := CheckPrecondition(ctx, gdb, Precondition{Action: "open a pull request", WorkID: id, NotIn: refused})
		if err != nil || !res.OK {
			t.Fatalf("state %s: res=%+v err=%v, want OK", s, res, err)
		}
		if db.rows() != 0 || !d.pending() {
			t.Fatalf("state %s: a passing check must write nothing and leave the guard pending", s)
		}
	}
}

func TestCheckPrecondition_DenyListRefusesListedStateWithoutBlockingIt(t *testing.T) {
	id := uuid.New()
	db := newStateDB(map[uuid.UUID]itemRow{id: {state: "cancelled"}})
	ctx, d, gdb := guardedCtx(db)
	res, err := CheckPrecondition(ctx, gdb, Precondition{Action: "open a pull request", WorkID: id, NotIn: refused})
	if err != nil {
		t.Fatal(err)
	}
	if res.OK || res.Blocked {
		t.Fatalf("res = %+v, want a mismatch without a block (cancelled has no edge to blocked)", res)
	}
	if !strings.Contains(res.Reason, "is cancelled, expected not one of cancelled, blocked, failed, rejected") {
		t.Fatalf("reason = %q", res.Reason)
	}
	if db.rows() != 1 || !d.Recorded() || db.writes() != 0 {
		t.Fatalf("the stale event must be recorded with no other write (rows=%d writes=%d)", db.rows(), db.writes())
	}
}

func TestCheckPrecondition_DenyListJudgesAwaitingUserByPreviousState(t *testing.T) {
	refusedID, allowedID := uuid.New(), uuid.New()
	db := newStateDB(map[uuid.UUID]itemRow{
		refusedID: {state: "awaiting_user", previous: "blocked"},
		allowedID: {state: "awaiting_user", previous: "review"},
	})
	ctx, _, gdb := guardedCtx(db)
	res, err := CheckPrecondition(ctx, gdb, Precondition{Action: "a", WorkID: allowedID, NotIn: refused})
	if err != nil || !res.OK {
		t.Fatalf("awaiting_user over review: res=%+v err=%v, want OK", res, err)
	}
	res, err = CheckPrecondition(ctx, gdb, Precondition{Action: "a", WorkID: refusedID, NotIn: refused})
	if err != nil || res.OK || res.Blocked {
		t.Fatalf("awaiting_user over blocked: res=%+v err=%v, want a mismatch without a block", res, err)
	}
	if !strings.Contains(res.Reason, "is blocked (awaiting_user)") {
		t.Fatalf("reason = %q", res.Reason)
	}
}

func TestCheckPrecondition_DenyListRefusesMissingItem(t *testing.T) {
	db := newStateDB(map[uuid.UUID]itemRow{})
	ctx, _, gdb := guardedCtx(db)
	id := uuid.New()
	res, err := CheckPrecondition(ctx, gdb, Precondition{Action: "a", WorkID: id, NotIn: refused})
	if err != nil || res.OK || res.Blocked {
		t.Fatalf("res=%+v err=%v, want a mismatch without a block", res, err)
	}
	if !strings.Contains(res.Reason, "is missing") {
		t.Fatalf("reason = %q", res.Reason)
	}
}

func TestCheckPrecondition_AllowListStillApplies(t *testing.T) {
	ok, terminal := uuid.New(), uuid.New()
	db := newStateDB(map[uuid.UUID]itemRow{ok: {state: "qa"}, terminal: {state: "passed"}})
	ctx, _, gdb := guardedCtx(db)
	expect := []models.WorkItemState{models.WorkItemStateQA}
	if res, err := CheckPrecondition(ctx, gdb, Precondition{Action: "a", WorkID: ok, Expected: expect}); err != nil || !res.OK {
		t.Fatalf("qa: res=%+v err=%v, want OK", res, err)
	}
	res, err := CheckPrecondition(ctx, gdb, Precondition{Action: "a", WorkID: terminal, Expected: expect})
	if err != nil || res.OK || res.Blocked || !strings.Contains(res.Reason, "is passed, expected qa") {
		t.Fatalf("passed: res=%+v err=%v, want a mismatch without a block", res, err)
	}
}
