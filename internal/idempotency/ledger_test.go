package idempotency

import (
	"context"
	"errors"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/metrics"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ── fakes ────────────────────────────────────────────────────────────────

type ledgerKey struct {
	consumer string
	eventID  uuid.UUID
}

// fakeDB models processed_events with commit/rollback semantics: a row
// inserted in a transaction is visible to later inserts only once committed
// (the real table serialises racing inserts on the uncommitted row; tests
// that need that use the integration test).
type fakeDB struct {
	mu        sync.Mutex
	committed map[ledgerKey]string // -> topic
	// otherWrites counts non-ledger statements committed (the "effects").
	otherWrites int
	begins      int
	seenErr     error
}

func newFakeDB() *fakeDB { return &fakeDB{committed: map[ledgerKey]string{}} }

func (f *fakeDB) Begin(context.Context) (pgx.Tx, error) {
	f.mu.Lock()
	f.begins++
	f.mu.Unlock()
	return &fakeTx{db: f, pending: map[ledgerKey]string{}}, nil
}

func (f *fakeDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	if f.seenErr != nil {
		return fakeRow{err: f.seenErr}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.committed[ledgerKey{args[0].(string), args[1].(uuid.UUID)}]
	return fakeRow{val: ok}
}

func (f *fakeDB) rows() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.committed)
}

func (f *fakeDB) writes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.otherWrites
}

type fakeRow struct {
	val bool
	err error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*(dest[0].(*bool)) = r.val
	return nil
}

type fakeTx struct {
	pgx.Tx
	db      *fakeDB
	pending map[ledgerKey]string
	writes  int
	ledger  int // ledger inserts attempted in this tx
	done    bool
}

func (t *fakeTx) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if strings.Contains(sql, "processed_events") {
		t.ledger++
		k := ledgerKey{args[0].(string), args[1].(uuid.UUID)}
		t.db.mu.Lock()
		_, dup := t.db.committed[k]
		t.db.mu.Unlock()
		if _, again := t.pending[k]; dup || again {
			return pgconn.NewCommandTag("INSERT 0 0"), nil
		}
		t.pending[k] = args[2].(string)
		return pgconn.NewCommandTag("INSERT 0 1"), nil
	}
	t.writes++
	return pgconn.NewCommandTag("UPDATE 1"), nil
}

func (t *fakeTx) Commit(context.Context) error {
	if t.done {
		return pgx.ErrTxClosed
	}
	t.done = true
	t.db.mu.Lock()
	defer t.db.mu.Unlock()
	for k, v := range t.pending {
		t.db.committed[k] = v
	}
	t.db.otherWrites += t.writes
	return nil
}

func (t *fakeTx) Rollback(context.Context) error {
	if t.done {
		return pgx.ErrTxClosed
	}
	t.done = true
	return nil
}

func counter(t *testing.T, name string) int64 {
	t.Helper()
	rec := httptest.NewRecorder()
	metrics.Handler()(rec, httptest.NewRequest("GET", "/metrics", nil))
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if strings.HasPrefix(line, name+" ") {
			v, err := strconv.ParseInt(strings.TrimPrefix(line, name+" "), 10, 64)
			if err != nil {
				t.Fatalf("parse %s: %v", line, err)
			}
			return v
		}
	}
	t.Fatalf("counter %s not exported", name)
	return 0
}

func message(topic string, id uuid.UUID) *bus.Message {
	return &bus.Message{
		Topic:   topic,
		Key:     "k",
		Value:   []byte(`{"work_id":"w"}`),
		Headers: map[string]string{bus.HeaderEventID: id.String()},
	}
}

// effect is a state change written in a transaction opened on db.
func effect(ctx context.Context, db TxBeginner) error {
	return workgraph.WithTx(ctx, db, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "UPDATE work_items SET state = 'x'")
		return err
	})
}

// ── Guard / Seen ─────────────────────────────────────────────────────────

func TestGuard_FirstTimeThenDuplicate(t *testing.T) {
	ctx := context.Background()
	db := newFakeDB()
	l := NewLedger(db)
	id := uuid.New()

	tx, _ := db.Begin(ctx)
	first, err := l.Guard(ctx, tx, "svc", id, "topic")
	if err != nil || !first {
		t.Fatalf("first Guard = %v, %v; want true, nil", first, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	tx2, _ := db.Begin(ctx)
	first, err = l.Guard(ctx, tx2, "svc", id, "topic")
	if err != nil || first {
		t.Fatalf("second Guard = %v, %v; want false, nil", first, err)
	}
	_ = tx2.Rollback(ctx)

	// Another consumer processes the same event independently.
	tx3, _ := db.Begin(ctx)
	if first, _ := l.Guard(ctx, tx3, "other-svc", id, "topic"); !first {
		t.Fatal("a different consumer must see the event for the first time")
	}
}

func TestGuard_RolledBackRowIsNotRecorded(t *testing.T) {
	ctx := context.Background()
	db := newFakeDB()
	l := NewLedger(db)
	id := uuid.New()
	tx, _ := db.Begin(ctx)
	if first, _ := l.Guard(ctx, tx, "svc", id, "t"); !first {
		t.Fatal("want first")
	}
	_ = tx.Rollback(ctx)
	if seen, _ := l.Seen(ctx, "svc", id); seen {
		t.Fatal("a rolled-back guard must not be seen")
	}
	tx2, _ := db.Begin(ctx)
	if first, _ := l.Guard(ctx, tx2, "svc", id, "t"); !first {
		t.Fatal("after a rollback the event must be first-time again")
	}
}

func TestGuard_NilEventIDIsUnguarded(t *testing.T) {
	ctx := context.Background()
	db := newFakeDB()
	before := counter(t, "ae_idempotency_unguarded_total")
	tx, _ := db.Begin(ctx)
	first, err := NewLedger(db).Guard(ctx, tx, "svc", uuid.Nil, "t")
	if err != nil || !first {
		t.Fatalf("Guard(Nil) = %v, %v; want true, nil", first, err)
	}
	if tx.(*fakeTx).ledger != 0 {
		t.Fatal("Guard(Nil) must not write")
	}
	if got := counter(t, "ae_idempotency_unguarded_total"); got != before+1 {
		t.Fatalf("unguarded counter = %d, want %d", got, before+1)
	}
}

func TestGuard_RejectsBadArguments(t *testing.T) {
	ctx := context.Background()
	db := newFakeDB()
	l := NewLedger(db)
	if _, err := l.Guard(ctx, nil, "svc", uuid.New(), "t"); err == nil {
		t.Error("nil tx must be an error")
	}
	tx, _ := db.Begin(ctx)
	if _, err := l.Guard(ctx, tx, "", uuid.New(), "t"); err == nil {
		t.Error("empty consumer must be an error")
	}
}

func TestSeen(t *testing.T) {
	ctx := context.Background()
	db := newFakeDB()
	l := NewLedger(db)
	id := uuid.New()
	if seen, err := l.Seen(ctx, "svc", id); err != nil || seen {
		t.Fatalf("Seen before = %v, %v", seen, err)
	}
	if _, err := l.record(ctx, "svc", id, "t"); err != nil {
		t.Fatal(err)
	}
	if seen, err := l.Seen(ctx, "svc", id); err != nil || !seen {
		t.Fatalf("Seen after = %v, %v", seen, err)
	}
	if seen, _ := l.Seen(ctx, "other", id); seen {
		t.Fatal("Seen must be per consumer")
	}
	if seen, _ := l.Seen(ctx, "svc", uuid.Nil); seen {
		t.Fatal("Nil is never seen")
	}
	var nilLedger *Ledger
	if seen, err := nilLedger.Seen(ctx, "svc", id); seen || err != nil {
		t.Fatal("nil ledger has seen nothing")
	}
}

// ── Wrap / Delivery ──────────────────────────────────────────────────────

func TestWrap_WithoutDeliveryReturnsDB(t *testing.T) {
	db := newFakeDB()
	if got := Wrap(context.Background(), db); got != TxBeginner(db) {
		t.Fatal("Wrap without a delivery must return db unchanged")
	}
	l := NewLedger(db)
	d := l.NewDelivery("svc", &bus.Message{Topic: "t", Value: []byte(`{}`)})
	if got := Wrap(WithDelivery(context.Background(), d), db); got != TxBeginner(db) {
		t.Fatal("Wrap for a message without an event id must return db unchanged")
	}
}

func TestWrap_GuardGoesIntoFirstCommittedTransactionOnly(t *testing.T) {
	ctx := context.Background()
	db := newFakeDB()
	l := NewLedger(db)
	d := l.NewDelivery("svc", message("t", uuid.New()))
	gdb := Wrap(WithDelivery(ctx, d), db)
	if Wrap(WithDelivery(ctx, d), gdb) != gdb {
		t.Fatal("re-wrapping must be a no-op")
	}

	// First transaction rolls back: its guard row goes with it.
	errBoom := errors.New("boom")
	err := workgraph.WithTx(ctx, gdb, func(tx pgx.Tx) error { return errBoom })
	if !errors.Is(err, errBoom) || db.rows() != 0 || d.Recorded() {
		t.Fatalf("rolled-back tx: err=%v rows=%d recorded=%v", err, db.rows(), d.Recorded())
	}
	// Second commits: guard row and effect together.
	if err := effect(ctx, gdb); err != nil {
		t.Fatal(err)
	}
	if db.rows() != 1 || db.writes() != 1 || !d.Recorded() {
		t.Fatalf("committed tx: rows=%d writes=%d recorded=%v", db.rows(), db.writes(), d.Recorded())
	}
	// Third runs unguarded (no second ledger insert).
	var ledgerInserts int
	if err := workgraph.WithTx(ctx, gdb, func(tx pgx.Tx) error {
		ledgerInserts = tx.(*fakeTx).ledger
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if ledgerInserts != 0 {
		t.Fatal("a later transaction of the same delivery must not guard again")
	}
}

func TestWrap_DuplicateIsStickyAndWritesNothing(t *testing.T) {
	ctx := context.Background()
	db := newFakeDB()
	l := NewLedger(db)
	id := uuid.New()
	if _, err := l.record(ctx, "svc", id, "t"); err != nil {
		t.Fatal(err)
	}
	d := l.NewDelivery("svc", message("t", id))
	gdb := Wrap(WithDelivery(ctx, d), db)
	for i := 0; i < 3; i++ {
		if err := effect(ctx, gdb); !IsDuplicate(err) {
			t.Fatalf("attempt %d: err = %v, want ErrDuplicate", i, err)
		}
	}
	if !d.Duplicate() || db.writes() != 0 {
		t.Fatalf("duplicate=%v writes=%d", d.Duplicate(), db.writes())
	}
}

// ── Handler ──────────────────────────────────────────────────────────────

func TestHandler_ReplayTenTimesAppliesOnce(t *testing.T) {
	ctx := context.Background()
	db := newFakeDB()
	l := NewLedger(db)
	calls := 0
	h := l.Handler("svc", func(ctx context.Context, msg *bus.Message) error {
		calls++
		return effect(ctx, Wrap(ctx, db))
	})
	before := counter(t, "ae_idempotency_duplicates_total")
	msg := message("t", uuid.New())
	for i := 0; i < 10; i++ {
		if err := h(ctx, msg); err != nil {
			t.Fatalf("delivery %d: %v", i, err)
		}
	}
	if calls != 1 || db.writes() != 1 || db.rows() != 1 {
		t.Fatalf("calls=%d writes=%d rows=%d; want 1,1,1", calls, db.writes(), db.rows())
	}
	if got := counter(t, "ae_idempotency_duplicates_total"); got != before+9 {
		t.Fatalf("duplicates counter moved by %d, want 9", got-before)
	}
}

// The restart-between-receipt-and-ack case: the state change committed, then
// the handler failed (or the process died) before the offset was committed.
// The error is returned so the bus redelivers; the redelivery is a no-op.
func TestHandler_ErrorAfterCommitRedeliveryIsDeduplicated(t *testing.T) {
	ctx := context.Background()
	db := newFakeDB()
	l := NewLedger(db)
	errCrash := errors.New("crash after commit")
	calls := 0
	h := l.Handler("svc", func(ctx context.Context, msg *bus.Message) error {
		calls++
		if err := effect(ctx, Wrap(ctx, db)); err != nil {
			return err
		}
		return errCrash
	})
	msg := message("t", uuid.New())
	if err := h(ctx, msg); !errors.Is(err, errCrash) {
		t.Fatalf("first delivery err = %v, want the handler's error", err)
	}
	if err := h(ctx, msg); err != nil {
		t.Fatalf("redelivery err = %v, want nil (ack)", err)
	}
	if calls != 1 || db.writes() != 1 {
		t.Fatalf("calls=%d writes=%d; want 1,1", calls, db.writes())
	}
}

func TestHandler_ErrorBeforeCommitIsRetried(t *testing.T) {
	ctx := context.Background()
	db := newFakeDB()
	l := NewLedger(db)
	fail := true
	h := l.Handler("svc", func(ctx context.Context, msg *bus.Message) error {
		return workgraph.WithTx(ctx, Wrap(ctx, db), func(tx pgx.Tx) error {
			if fail {
				return errors.New("transient")
			}
			_, err := tx.Exec(ctx, "UPDATE x")
			return err
		})
	})
	msg := message("t", uuid.New())
	if err := h(ctx, msg); err == nil {
		t.Fatal("want the transient error")
	}
	if db.rows() != 0 {
		t.Fatal("a failed transaction must not record the event")
	}
	fail = false
	if err := h(ctx, msg); err != nil {
		t.Fatal(err)
	}
	if db.rows() != 1 || db.writes() != 1 {
		t.Fatalf("rows=%d writes=%d", db.rows(), db.writes())
	}
}

func TestHandler_RecordsAfterHandlerWithoutGuardedTx(t *testing.T) {
	ctx := context.Background()
	db := newFakeDB()
	l := NewLedger(db)
	calls := 0
	h := l.Handler("svc", func(ctx context.Context, msg *bus.Message) error {
		calls++
		return nil // e.g. "waiting for siblings", or pooled writes only
	})
	before := counter(t, "ae_idempotency_recorded_after_handler_total")
	msg := message("t", uuid.New())
	for i := 0; i < 3; i++ {
		if err := h(ctx, msg); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 || db.rows() != 1 {
		t.Fatalf("calls=%d rows=%d", calls, db.rows())
	}
	if got := counter(t, "ae_idempotency_recorded_after_handler_total"); got != before+1 {
		t.Fatalf("recorded-after counter moved by %d, want 1", got-before)
	}
}

func TestHandler_DuplicateFoundInsideTransactionIsAcked(t *testing.T) {
	ctx := context.Background()
	db := newFakeDB()
	l := NewLedger(db)
	id := uuid.New()
	// The Seen check fails, so the duplicate reaches the handler and is
	// caught by the guard inside its transaction.
	if _, err := l.record(ctx, "svc", id, "t"); err != nil {
		t.Fatal(err)
	}
	db.seenErr = errors.New("seen unavailable")
	h := l.Handler("svc", func(ctx context.Context, msg *bus.Message) error {
		if err := effect(ctx, Wrap(ctx, db)); err != nil {
			return err // a handler that propagates the duplicate as an error
		}
		return nil
	})
	if err := h(ctx, message("t", id)); err != nil {
		t.Fatalf("err = %v, want nil (ack)", err)
	}
	if db.writes() != 0 {
		t.Fatal("a duplicate must write nothing")
	}
}

func TestHandler_NoEventIDRunsUnguarded(t *testing.T) {
	ctx := context.Background()
	db := newFakeDB()
	l := NewLedger(db)
	calls := 0
	h := l.Handler("svc", func(ctx context.Context, msg *bus.Message) error {
		calls++
		if FromContext(ctx) != nil {
			t.Error("no delivery is attached for a message without an id")
		}
		return effect(ctx, Wrap(ctx, db))
	})
	msg := &bus.Message{Topic: "t", Value: []byte(`{"work_id":"w"}`)}
	for i := 0; i < 2; i++ {
		if err := h(ctx, msg); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 || db.writes() != 2 || db.rows() != 0 {
		t.Fatalf("calls=%d writes=%d rows=%d; legacy messages are applied every time", calls, db.writes(), db.rows())
	}
}

func TestHandler_NilLedgerRunsUnguarded(t *testing.T) {
	var l *Ledger
	calls := 0
	h := l.Handler("svc", func(context.Context, *bus.Message) error { calls++; return nil })
	msg := message("t", uuid.New())
	_ = h(context.Background(), msg)
	_ = h(context.Background(), msg)
	if calls != 2 {
		t.Fatalf("calls = %d", calls)
	}
}

func TestHandler_PayloadEventIDIsUsed(t *testing.T) {
	ctx := context.Background()
	db := newFakeDB()
	l := NewLedger(db)
	calls := 0
	h := l.Handler("svc", func(ctx context.Context, msg *bus.Message) error { calls++; return nil })
	id := uuid.New()
	msg := &bus.Message{Topic: "t", Value: []byte(`{"event_id":"` + id.String() + `"}`)}
	_ = h(ctx, msg)
	_ = h(ctx, msg)
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 (event id from the payload)", calls)
	}
}

func TestUnguarded_TransactionCarriesNoGuard(t *testing.T) {
	ctx := context.Background()
	db := newFakeDB()
	l := NewLedger(db)
	d := l.NewDelivery("svc", message("t", uuid.New()))
	ctx = WithDelivery(ctx, d)
	if Wrap(Unguarded(ctx), db) != TxBeginner(db) {
		t.Fatal("Wrap on an Unguarded context must return db unchanged")
	}
	if err := effect(Unguarded(ctx), Wrap(Unguarded(ctx), db)); err != nil {
		t.Fatal(err)
	}
	if db.rows() != 0 || !d.pending() {
		t.Fatal("an unguarded write must leave the guard pending")
	}
	if err := effect(ctx, Wrap(ctx, db)); err != nil {
		t.Fatal(err)
	}
	if db.rows() != 1 || !d.Recorded() {
		t.Fatal("the next guarded write carries the guard")
	}
}
