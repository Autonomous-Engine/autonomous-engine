package outbox_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/db"
	"github.com/autonomousengine/autonomous-engine/internal/outbox"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// integrationDSN returns the test database DSN: TEST_POSTGRES_DSN (the repo
// convention, see internal/evidence) first, then TEST_DATABASE_URL.
func integrationDSN() string {
	if dsn := os.Getenv("TEST_POSTGRES_DSN"); dsn != "" {
		return dsn
	}
	return os.Getenv("TEST_DATABASE_URL")
}

// integrationPool opens the test database and applies the repository's
// migrations so 047 is guaranteed to be present. Gated on TEST_POSTGRES_DSN /
// TEST_DATABASE_URL; skips cleanly when both are unset.
func integrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := integrationDSN()
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN / TEST_DATABASE_URL not set — skipping DB-integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	p, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	if err := db.Migrate(ctx, p, filepath.Join("..", "..", "migrations")); err != nil {
		p.Close()
		t.Fatalf("migrate: %v", err)
	}
	// Registered first, so it runs last: the per-aggregate row cleanups
	// registered later still have a live pool to delete through.
	t.Cleanup(p.Close)
	return p
}

// newAggregate returns a fresh aggregate id whose rows are deleted when the
// test ends. outbox_events has no foreign key, so nothing else needs seeding.
func newAggregate(t *testing.T, p *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := p.Exec(ctx, `DELETE FROM outbox_events WHERE aggregate_id = $1`, id); err != nil {
			t.Logf("cleanup aggregate %s: %v", id, err)
		}
	})
	return id
}

func newEvent(t *testing.T, agg uuid.UUID, seq int) outbox.Event {
	t.Helper()
	ev, err := outbox.NewEvent("work_item", agg, "implementation.tasks", agg.String(), map[string]any{
		"work_id": agg.String(),
		"seq":     seq,
	})
	if err != nil {
		t.Fatalf("NewEvent: %v", err)
	}
	return ev
}

// commitEvents enqueues events inside one transaction and commits it, the way
// a producer would alongside its own state change.
func commitEvents(t *testing.T, p *pgxpool.Pool, evs ...outbox.Event) {
	t.Helper()
	ctx := context.Background()
	tx, err := p.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := outbox.EnqueueAll(ctx, tx, evs...); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("EnqueueAll: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func countRows(t *testing.T, p *pgxpool.Pool, agg uuid.UUID) int {
	t.Helper()
	var n int
	if err := p.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM outbox_events WHERE aggregate_id = $1`, agg).Scan(&n); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	return n
}

// claimIDs claims and returns the ids belonging to the given aggregates, in
// claim order. Filtering by aggregate keeps each test independent of rows any
// other test (or a previous run) left in the shared table.
func claimIDs(t *testing.T, s *outbox.Store, who string, limit int, now time.Time, aggs ...uuid.UUID) []uuid.UUID {
	t.Helper()
	evs, err := s.Claim(context.Background(), who, limit, now)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	var ids []uuid.UUID
	for _, ev := range evs {
		for _, agg := range aggs {
			if ev.AggregateID == agg {
				ids = append(ids, ev.ID)
			}
		}
	}
	return ids
}

// A rolled-back producer transaction must leave no event behind: that is the
// whole point of the outbox — the event and the state change share a fate.
func TestIntegration_EnqueueRollsBackWithTheCaller(t *testing.T) {
	p := integrationPool(t)
	ctx := context.Background()
	agg := newAggregate(t, p)

	tx, err := p.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := outbox.Enqueue(ctx, tx, newEvent(t, agg, 1)); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if n := countRows(t, p, agg); n != 0 {
		t.Fatalf("%d rows survived the rollback, want 0", n)
	}

	// And the committed path does leave one, claimable immediately.
	ev := newEvent(t, agg, 2)
	commitEvents(t, p, ev)
	if n := countRows(t, p, agg); n != 1 {
		t.Fatalf("%d rows after commit, want 1", n)
	}
	s := outbox.NewStore(p)
	claimed, err := s.Claim(ctx, "dispatcher-1", 100, time.Now())
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	var found *outbox.Event
	for i := range claimed {
		if claimed[i].ID == ev.ID {
			found = &claimed[i]
		}
	}
	if found == nil {
		t.Fatalf("committed event %s was not claimed (claimed %d rows)", ev.ID, len(claimed))
	}
	if found.Topic != ev.Topic || found.Key != ev.Key || found.AggregateID != agg {
		t.Errorf("claimed event lost its routing: %+v", found)
	}
	if found.Headers[outbox.HeaderEventID] != ev.ID.String() {
		t.Errorf("headers = %v, want the event id", found.Headers)
	}
	if found.ClaimedBy != "dispatcher-1" || found.ClaimedAt == nil {
		t.Errorf("claim not recorded on the row: by=%q at=%v", found.ClaimedBy, found.ClaimedAt)
	}
	if found.Attempts != 0 || found.PublishedAt != nil || found.DeadAt != nil {
		t.Errorf("fresh row has dispatch state: %+v", found)
	}
}

// Per-aggregate ordering: the second event of an aggregate is invisible to the
// dispatcher until the first is published (or dead), and events enqueued in
// one transaction come back in enqueue order.
func TestIntegration_PerAggregateOrdering(t *testing.T) {
	p := integrationPool(t)
	ctx := context.Background()
	s := outbox.NewStore(p)
	agg := newAggregate(t, p)
	other := newAggregate(t, p)

	first, second, third := newEvent(t, agg, 1), newEvent(t, agg, 2), newEvent(t, agg, 3)
	commitEvents(t, p, first, second, third)
	otherEvent := newEvent(t, other, 1)
	commitEvents(t, p, otherEvent)

	same := func(label string, got []uuid.UUID, want ...uuid.UUID) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s: claimed %v, want exactly %v", label, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%s: claim order = %v, want %v", label, got, want)
			}
		}
	}

	now := time.Now()
	claimed, err := s.Claim(ctx, "dispatcher-1", 50, now)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	same("head of the aggregate", idsFor(claimed, agg), first.ID)
	// A different aggregate is never blocked by this one.
	same("unrelated aggregate", idsFor(claimed, other), otherEvent.ID)

	// While the first row is claimed but unpublished, nothing else for the
	// aggregate becomes eligible — not even for a second dispatcher.
	if got := claimIDs(t, s, "dispatcher-2", 50, now, agg, other); len(got) != 0 {
		t.Fatalf("claimed %v while every aggregate's head was in flight, want none", got)
	}

	// Publishing the head releases exactly the next row.
	if err := s.MarkPublished(ctx, first.ID, time.Now()); err != nil {
		t.Fatalf("MarkPublished: %v", err)
	}
	same("after publishing the head", claimIDs(t, s, "dispatcher-1", 50, time.Now(), agg), second.ID)

	// A dead head must not block the tail forever.
	dead := outbox.NewStore(p, outbox.WithMaxAttempts(1))
	isDead, err := dead.MarkFailed(ctx, second.ID, errors.New("broker down"), time.Now())
	if err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	if !isDead {
		t.Fatal("MarkFailed with maxAttempts=1 did not kill the row")
	}
	same("after the head died", claimIDs(t, s, "dispatcher-1", 50, time.Now(), agg), third.ID)
}

// idsFor returns the ids of the claimed events belonging to one aggregate, in
// claim order.
func idsFor(evs []outbox.Event, agg uuid.UUID) []uuid.UUID {
	var ids []uuid.UUID
	for _, ev := range evs {
		if ev.AggregateID == agg {
			ids = append(ids, ev.ID)
		}
	}
	return ids
}

// Many dispatchers may run at once; SKIP LOCKED must hand each row to exactly
// one of them.
func TestIntegration_ConcurrentClaimersNeverOverlap(t *testing.T) {
	p := integrationPool(t)
	s := outbox.NewStore(p)

	const aggregates = 12
	want := make(map[uuid.UUID]bool, aggregates)
	for i := 0; i < aggregates; i++ {
		agg := newAggregate(t, p)
		ev := newEvent(t, agg, 1)
		commitEvents(t, p, ev, newEvent(t, agg, 2))
		want[ev.ID] = true
	}

	now := time.Now()
	const claimers = 4
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		seen = map[uuid.UUID]string{}
		mine = map[uuid.UUID]string{}
		errs []error
	)
	for i := 0; i < claimers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			who := fmt.Sprintf("dispatcher-%d", n)
			evs, err := s.Claim(context.Background(), who, aggregates, now)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			for _, ev := range evs {
				if prev, dup := seen[ev.ID]; dup {
					errs = append(errs, fmt.Errorf("event %s claimed by both %s and %s", ev.ID, prev, who))
					continue
				}
				if ev.ClaimedBy != who {
					errs = append(errs, fmt.Errorf("event %s returned to %s but claimed_by=%s", ev.ID, who, ev.ClaimedBy))
				}
				if !want[ev.ID] {
					// A row from some other test's aggregate; it still must
					// not be double-claimed, but it does not count here.
					seen[ev.ID] = who
					continue
				}
				seen[ev.ID] = who
				mine[ev.ID] = who
			}
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		t.Error(err)
	}
	for id := range want {
		if _, ok := mine[id]; !ok {
			t.Errorf("head event %s was never claimed", id)
		}
	}
	if len(mine) != aggregates {
		t.Errorf("claimed %d of this test's rows, want %d (one head per aggregate, never the tail)", len(mine), aggregates)
	}
}

// A dispatcher that dies holding a claim must not strand its aggregate: the
// claim expires after claim_ttl and the row becomes claimable again.
func TestIntegration_ExpiredClaimIsReclaimable(t *testing.T) {
	p := integrationPool(t)
	s := outbox.NewStore(p) // DefaultClaimTTL
	agg := newAggregate(t, p)
	ev := newEvent(t, agg, 1)
	commitEvents(t, p, ev)

	now := time.Now()
	if got := claimIDs(t, s, "crashy", 100, now, agg); len(got) != 1 || got[0] != ev.ID {
		t.Fatalf("first claim = %v, want [%s]", got, ev.ID)
	}
	// Still inside the TTL: nobody else may take it.
	if got := claimIDs(t, s, "healthy", 100, now.Add(outbox.DefaultClaimTTL/2), agg); len(got) != 0 {
		t.Fatalf("claimed %v inside the claim TTL, want none", got)
	}
	// Past the TTL: the crashed dispatcher's row is re-claimable.
	later := now.Add(outbox.DefaultClaimTTL + time.Second)
	claimed, err := s.Claim(context.Background(), "healthy", 100, later)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	var reclaimed *outbox.Event
	for i := range claimed {
		if claimed[i].ID == ev.ID {
			reclaimed = &claimed[i]
		}
	}
	if reclaimed == nil {
		t.Fatalf("claim after the TTL = %v, want the expired row %s", idsFor(claimed, agg), ev.ID)
	}
	if reclaimed.ClaimedBy != "healthy" {
		t.Errorf("claimed_by = %q, want the new owner", reclaimed.ClaimedBy)
	}
}

// Retry then death: attempts accumulate, the row is rescheduled with backoff,
// and after max_attempts it is left for an operator — who can requeue it.
func TestIntegration_MarkFailedBackoffDeathAndRequeue(t *testing.T) {
	p := integrationPool(t)
	ctx := context.Background()
	const maxAttempts = 3
	s := outbox.NewStore(p, outbox.WithMaxAttempts(maxAttempts))
	agg := newAggregate(t, p)
	ev := newEvent(t, agg, 1)
	commitEvents(t, p, ev)

	readRow := func() (attempts int, nextAttempt time.Time, deadAt *time.Time, lastErr *string, claimedBy *string) {
		t.Helper()
		if err := p.QueryRow(ctx,
			`SELECT attempts, next_attempt_at, dead_at, last_error, claimed_by FROM outbox_events WHERE id = $1`,
			ev.ID).Scan(&attempts, &nextAttempt, &deadAt, &lastErr, &claimedBy); err != nil {
			t.Fatalf("read row: %v", err)
		}
		return
	}

	now := time.Now()
	if got := claimIDs(t, s, "dispatcher-1", 100, now, agg); len(got) != 1 {
		t.Fatalf("claim = %v, want the only row", got)
	}

	for attempt := 1; attempt < maxAttempts; attempt++ {
		dead, err := s.MarkFailed(ctx, ev.ID, fmt.Errorf("publish failed on attempt %d", attempt), now)
		if err != nil {
			t.Fatalf("MarkFailed: %v", err)
		}
		if dead {
			t.Fatalf("row died on attempt %d of %d", attempt, maxAttempts)
		}
		attempts, next, deadAt, lastErr, claimedBy := readRow()
		if attempts != attempt {
			t.Errorf("attempts = %d, want %d", attempts, attempt)
		}
		if deadAt != nil {
			t.Errorf("dead_at set on attempt %d", attempt)
		}
		if lastErr == nil || *lastErr == "" {
			t.Errorf("last_error not recorded on attempt %d", attempt)
		}
		if claimedBy != nil {
			t.Errorf("claim not released on failure: %q", *claimedBy)
		}
		if !next.After(now) {
			t.Errorf("next_attempt_at = %v, want a backoff after %v", next, now)
		}
		// Backed off: not eligible now, eligible once the backoff elapses.
		if got := claimIDs(t, s, "dispatcher-1", 100, now, agg); len(got) != 0 {
			t.Errorf("claimed %v before the backoff elapsed", got)
		}
		if got := claimIDs(t, s, "dispatcher-1", 100, next.Add(time.Second), agg); len(got) != 1 {
			t.Errorf("row not claimable after the backoff elapsed: %v", got)
		}
	}

	dead, err := s.MarkFailed(ctx, ev.ID, errors.New("final failure"), now)
	if err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	if !dead {
		t.Fatalf("row did not die after %d attempts", maxAttempts)
	}
	attempts, _, deadAt, _, _ := readRow()
	if attempts != maxAttempts || deadAt == nil {
		t.Fatalf("attempts=%d dead_at=%v, want %d and a tombstone", attempts, deadAt, maxAttempts)
	}
	if got := claimIDs(t, s, "dispatcher-1", 100, now.Add(24*time.Hour), agg); len(got) != 0 {
		t.Errorf("dead row is still being claimed: %v", got)
	}

	listed, err := s.ListDead(ctx, 50)
	if err != nil {
		t.Fatalf("ListDead: %v", err)
	}
	var listedIDs []uuid.UUID
	for _, d := range listed {
		listedIDs = append(listedIDs, d.ID)
	}
	if !contains(listedIDs, ev.ID) {
		t.Errorf("ListDead = %v, want it to include %s", listedIDs, ev.ID)
	}

	if err := s.Requeue(ctx, ev.ID); err != nil {
		t.Fatalf("Requeue: %v", err)
	}
	attempts, _, deadAt, lastErr, _ := readRow()
	if deadAt != nil || attempts != 0 || (lastErr != nil && *lastErr != "") {
		t.Errorf("after Requeue: attempts=%d dead_at=%v last_error=%v, want a clean row", attempts, deadAt, lastErr)
	}
	if got := claimIDs(t, s, "dispatcher-1", 100, time.Now(), agg); len(got) != 1 || got[0] != ev.ID {
		t.Errorf("requeued row not claimable: %v", got)
	}

	// A published row is never resurrected, and publishing is idempotent.
	if err := s.MarkPublished(ctx, ev.ID, time.Now()); err != nil {
		t.Fatalf("MarkPublished: %v", err)
	}
	if err := s.MarkPublished(ctx, ev.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("second MarkPublished: %v", err)
	}
	if err := s.Requeue(ctx, ev.ID); !errors.Is(err, outbox.ErrNotFound) {
		t.Errorf("Requeue of a published row = %v, want ErrNotFound", err)
	}
	if err := s.MarkPublished(ctx, uuid.New(), time.Now()); !errors.Is(err, outbox.ErrNotFound) {
		t.Errorf("MarkPublished(unknown) = %v, want ErrNotFound", err)
	}
	if _, err := s.MarkFailed(ctx, uuid.New(), errors.New("x"), time.Now()); !errors.Is(err, outbox.ErrNotFound) {
		t.Errorf("MarkFailed(unknown) = %v, want ErrNotFound", err)
	}
}

// Pending is the dispatcher's backlog metric: live rows only.
func TestIntegration_PendingCountsLiveRows(t *testing.T) {
	p := integrationPool(t)
	ctx := context.Background()
	s := outbox.NewStore(p)
	agg := newAggregate(t, p)

	before, _, err := s.Pending(ctx)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}

	first, second := newEvent(t, agg, 1), newEvent(t, agg, 2)
	commitEvents(t, p, first, second)

	count, oldest, err := s.Pending(ctx)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if count != before+2 {
		t.Errorf("pending = %d, want %d", count, before+2)
	}
	if oldest == nil {
		t.Fatal("pending rows but no oldest timestamp")
	}

	if err := s.MarkPublished(ctx, first.ID, time.Now()); err != nil {
		t.Fatalf("MarkPublished: %v", err)
	}
	if count, _, err = s.Pending(ctx); err != nil || count != before+1 {
		t.Errorf("pending after publish = %d (%v), want %d", count, err, before+1)
	}
	dead := outbox.NewStore(p, outbox.WithMaxAttempts(1))
	if _, err := dead.MarkFailed(ctx, second.ID, errors.New("dead"), time.Now()); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	if count, _, err = s.Pending(ctx); err != nil || count != before {
		t.Errorf("pending after death = %d (%v), want %d", count, err, before)
	}
}

func contains(ids []uuid.UUID, want uuid.UUID) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}
