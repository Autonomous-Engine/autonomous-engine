package idempotency

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/db"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Integration tests against a disposable PostgreSQL (TEST_POSTGRES_DSN; they
// migrate it). Run with the repo's docker command, e.g.
//
//	docker run ... -e TEST_POSTGRES_DSN=postgres://.../ae_m105_test?sslmode=disable \
//	    golang:1.22-alpine sh -c "go test ./internal/idempotency/ -run Integration -v"

func integrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set — skipping DB-integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	p, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	if err := db.Migrate(ctx, p, filepath.Join("..", "..", "migrations")); err != nil {
		p.Close()
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

func consumerName(t *testing.T) string {
	return "m105-test-" + strings.ReplaceAll(t.Name(), "/", "-") + "-" + uuid.NewString()[:8]
}

func cleanupLedger(t *testing.T, pool *pgxpool.Pool, consumer string) {
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM processed_events WHERE consumer = $1`, consumer)
	})
}

func seedWorkItem(t *testing.T, pool *pgxpool.Pool, state models.WorkItemState, parent *uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	typ := "feature"
	if parent != nil {
		typ = "subtask"
	}
	if _, err := pool.Exec(context.Background(), `INSERT INTO work_items (id, type, state, title, description, parent_id)
		VALUES ($1, $2, $3, 'M1-05 precondition item', 'd', $4)`, id, typ, string(state), parent); err != nil {
		t.Fatalf("seed work item: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, `DELETE FROM outbox_events WHERE aggregate_id = $1`, id)
		_, _ = pool.Exec(ctx, `DELETE FROM work_items WHERE parent_id = $1`, id)
		_, _ = pool.Exec(ctx, `DELETE FROM work_items WHERE id = $1`, id)
	})
	return id
}

func stateOf(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(context.Background(), `SELECT state::text FROM work_items WHERE id = $1`, id).Scan(&s); err != nil {
		t.Fatalf("read state: %v", err)
	}
	return s
}

func ledgerRows(t *testing.T, pool *pgxpool.Pool, consumer string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM processed_events WHERE consumer = $1`, consumer).Scan(&n); err != nil {
		t.Fatalf("count ledger rows: %v", err)
	}
	return n
}

func TestIntegration_Migration049Shape(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	var pk, idx string
	if err := pool.QueryRow(ctx, `
		SELECT pg_get_constraintdef(oid) FROM pg_constraint
		WHERE conrelid = 'processed_events'::regclass AND conname = 'processed_events_pkey'`).Scan(&pk); err != nil {
		t.Fatalf("read primary key: %v", err)
	}
	if pk != "PRIMARY KEY (consumer, event_id)" {
		t.Fatalf("primary key = %q", pk)
	}
	if err := pool.QueryRow(ctx, `SELECT indexdef FROM pg_indexes WHERE indexname = 'idx_processed_events_processed_at'`).Scan(&idx); err != nil {
		t.Fatalf("read processed_at index: %v", err)
	}
	if !strings.Contains(idx, "(processed_at)") {
		t.Fatalf("index = %q", idx)
	}
}

func TestIntegration_GuardAndSeen(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	consumer := consumerName(t)
	cleanupLedger(t, pool, consumer)
	l := NewLedger(pool)
	id := uuid.New()

	if seen, err := l.Seen(ctx, consumer, id); err != nil || seen {
		t.Fatalf("Seen before = %v, %v", seen, err)
	}
	// A rolled-back guard leaves nothing.
	if err := workgraph.WithTx(ctx, pool, func(tx pgx.Tx) error {
		first, err := l.Guard(ctx, tx, consumer, id, "m105.test")
		if err != nil || !first {
			t.Fatalf("Guard = %v, %v", first, err)
		}
		return context.Canceled
	}); err != context.Canceled {
		t.Fatalf("WithTx err = %v", err)
	}
	if n := ledgerRows(t, pool, consumer); n != 0 {
		t.Fatalf("rows after rollback = %d", n)
	}
	for i, want := range []bool{true, false, false} {
		var first bool
		if err := workgraph.WithTx(ctx, pool, func(tx pgx.Tx) error {
			var err error
			first, err = l.Guard(ctx, tx, consumer, id, "m105.test")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if first != want {
			t.Fatalf("Guard #%d = %v, want %v", i, first, want)
		}
	}
	if seen, err := l.Seen(ctx, consumer, id); err != nil || !seen {
		t.Fatalf("Seen after = %v, %v", seen, err)
	}
}

// Two deliveries of one event racing: the second blocks on the first's
// uncommitted row and sees a duplicate once it commits.
func TestIntegration_GuardSerialisesRacingDeliveries(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	consumer := consumerName(t)
	cleanupLedger(t, pool, consumer)
	l := NewLedger(pool)
	id := uuid.New()

	const racers = 8
	var wg sync.WaitGroup
	results := make(chan bool, racers)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			var first bool
			err := workgraph.WithTx(ctx, pool, func(tx pgx.Tx) error {
				var err error
				first, err = l.Guard(ctx, tx, consumer, id, "m105.test")
				time.Sleep(50 * time.Millisecond) // hold the row while others wait
				return err
			})
			if err != nil {
				t.Errorf("racer: %v", err)
			}
			results <- first
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	firsts := 0
	for r := range results {
		if r {
			firsts++
		}
	}
	if firsts != 1 {
		t.Fatalf("%d racers saw the event for the first time, want exactly 1", firsts)
	}
}

// Ten deliveries of one event through Ledger.Handler and a Wrap'd
// transaction: exactly one state change, one ledger row.
func TestIntegration_HandlerReplayTenTimes(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	consumer := consumerName(t)
	cleanupLedger(t, pool, consumer)
	l := NewLedger(pool)
	item := seedWorkItem(t, pool, models.WorkItemStateImplementation, nil)

	h := l.Handler(consumer, func(ctx context.Context, msg *bus.Message) error {
		return workgraph.WithTx(ctx, Wrap(ctx, pool), func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE work_items SET title = title || '+' WHERE id = $1`, item)
			return err
		})
	})
	msg := &bus.Message{Topic: "m105.test", Headers: map[string]string{bus.HeaderEventID: uuid.NewString()}, Value: []byte(`{}`)}
	for i := 0; i < 10; i++ {
		if err := h(ctx, msg); err != nil {
			t.Fatalf("delivery %d: %v", i, err)
		}
	}
	var title string
	if err := pool.QueryRow(ctx, `SELECT title FROM work_items WHERE id = $1`, item).Scan(&title); err != nil {
		t.Fatal(err)
	}
	if strings.Count(title, "+") != 1 {
		t.Fatalf("title = %q: want exactly one application", title)
	}
	if n := ledgerRows(t, pool, consumer); n != 1 {
		t.Fatalf("ledger rows = %d, want 1", n)
	}
}

func precondition(work uuid.UUID, parent uuid.UUID, expected ...models.WorkItemState) Precondition {
	return Precondition{
		Action:      "create a worktree",
		AgentID:     "m105-test",
		AggregateID: work,
		WorkID:      work,
		Expected:    expected,
		ParentID:    parent,
		ParentNotIn: []models.WorkItemState{models.WorkItemStateCancelled, models.WorkItemStateBlocked},
	}
}

func deliveryCtx(l *Ledger, consumer string) (context.Context, *Delivery) {
	d := l.NewDelivery(consumer, &bus.Message{Topic: "m105.test", Headers: map[string]string{bus.HeaderEventID: uuid.NewString()}, Value: []byte(`{}`)})
	return WithDelivery(context.Background(), d), d
}

func TestIntegration_PreconditionMatchWritesNothing(t *testing.T) {
	pool := integrationPool(t)
	consumer := consumerName(t)
	cleanupLedger(t, pool, consumer)
	l := NewLedger(pool)
	parent := seedWorkItem(t, pool, models.WorkItemStatePlanning, nil)
	sub := seedWorkItem(t, pool, models.WorkItemStateImplementation, &parent)

	ctx, d := deliveryCtx(l, consumer)
	res, err := CheckPrecondition(ctx, Wrap(ctx, pool), precondition(sub, parent, models.WorkItemStateImplementation))
	if err != nil || !res.OK {
		t.Fatalf("CheckPrecondition = %+v, %v", res, err)
	}
	if n := ledgerRows(t, pool, consumer); n != 0 || d.Recorded() {
		t.Fatalf("a passing check must leave the guard pending (rows=%d recorded=%v)", n, d.Recorded())
	}
	// The guard then goes into the handler's own transaction.
	if err := workgraph.WithTx(ctx, Wrap(ctx, pool), func(tx pgx.Tx) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if n := ledgerRows(t, pool, consumer); n != 1 || !d.Recorded() {
		t.Fatalf("guard not recorded by the handler's transaction (rows=%d)", n)
	}
}

func TestIntegration_PreconditionMismatchBlocksWithGuard(t *testing.T) {
	pool := integrationPool(t)
	consumer := consumerName(t)
	cleanupLedger(t, pool, consumer)
	l := NewLedger(pool)
	parent := seedWorkItem(t, pool, models.WorkItemStateCancelled, nil)
	sub := seedWorkItem(t, pool, models.WorkItemStateImplementation, &parent)

	ctx, d := deliveryCtx(l, consumer)
	res, err := CheckPrecondition(ctx, Wrap(ctx, pool), precondition(sub, parent, models.WorkItemStateImplementation))
	if err != nil {
		t.Fatal(err)
	}
	if res.OK || !res.Blocked {
		t.Fatalf("result = %+v, want a block", res)
	}
	if !strings.Contains(res.Reason, "parent work item "+parent.String()+" is cancelled") ||
		!strings.Contains(res.Reason, "expected not one of cancelled, blocked") {
		t.Fatalf("reason does not name the observed and expected states: %q", res.Reason)
	}
	if s := stateOf(t, pool, sub); s != "blocked" {
		t.Fatalf("subtask state = %s, want blocked", s)
	}
	var events int
	_ = pool.QueryRow(context.Background(), `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND topic = 'work.blocked'`, sub).Scan(&events)
	if events != 1 {
		t.Fatalf("work.blocked events = %d, want 1", events)
	}
	if n := ledgerRows(t, pool, consumer); n != 1 || !d.Recorded() {
		t.Fatalf("the block must commit with the guard row (rows=%d)", n)
	}
	// A redelivery of the stale event is a duplicate.
	ctx2 := WithDelivery(context.Background(), &Delivery{ledger: l, consumer: consumer, eventID: d.EventID(), topic: "m105.test"})
	if _, err := CheckPrecondition(ctx2, Wrap(ctx2, pool), precondition(sub, parent, models.WorkItemStateImplementation)); !IsDuplicate(err) {
		t.Fatalf("redelivery err = %v, want ErrDuplicate", err)
	}
}

func TestIntegration_PreconditionTerminalItemIsNotBlocked(t *testing.T) {
	pool := integrationPool(t)
	consumer := consumerName(t)
	cleanupLedger(t, pool, consumer)
	l := NewLedger(pool)
	sub := seedWorkItem(t, pool, models.WorkItemStateCancelled, nil)

	ctx, _ := deliveryCtx(l, consumer)
	res, err := CheckPrecondition(ctx, Wrap(ctx, pool), precondition(sub, uuid.Nil, models.WorkItemStateImplementation))
	if err != nil {
		t.Fatal(err)
	}
	if res.OK || res.Blocked {
		t.Fatalf("result = %+v, want mismatch without block", res)
	}
	if !strings.Contains(res.Reason, "is cancelled, expected implementation") {
		t.Fatalf("reason = %q", res.Reason)
	}
	if s := stateOf(t, pool, sub); s != "cancelled" {
		t.Fatalf("state = %s, want cancelled (untouched)", s)
	}
	if n := ledgerRows(t, pool, consumer); n != 1 {
		t.Fatalf("the stale event must still be recorded (rows=%d)", n)
	}
}

func TestIntegration_PreconditionAwaitingUserUsesPreviousState(t *testing.T) {
	pool := integrationPool(t)
	consumer := consumerName(t)
	cleanupLedger(t, pool, consumer)
	l := NewLedger(pool)
	item := seedWorkItem(t, pool, models.WorkItemStateQA, nil)
	if _, err := pool.Exec(context.Background(), `UPDATE work_items SET previous_state = 'qa', state = 'awaiting_user' WHERE id = $1`, item); err != nil {
		t.Fatal(err)
	}
	ctx, _ := deliveryCtx(l, consumer)
	res, err := CheckPrecondition(ctx, Wrap(ctx, pool), precondition(item, uuid.Nil, models.WorkItemStateQA))
	if err != nil || !res.OK {
		t.Fatalf("awaiting_user over qa must satisfy expected qa: %+v, %v", res, err)
	}

	if _, err := pool.Exec(context.Background(), `UPDATE work_items SET previous_state = 'review' WHERE id = $1`, item); err != nil {
		t.Fatal(err)
	}
	res, err = CheckPrecondition(ctx, Wrap(ctx, pool), precondition(item, uuid.Nil, models.WorkItemStateQA))
	if err != nil || res.OK || res.Blocked {
		t.Fatalf("awaiting_user over review: %+v, %v; want mismatch without block", res, err)
	}
	if s := stateOf(t, pool, item); s != "awaiting_user" {
		t.Fatalf("state = %s: an item paused on a prompt must not be blocked", s)
	}
}
