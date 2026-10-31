package outbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Dispatcher defaults (M1-01 delivery semantics).
const (
	// DefaultClaimTTL is how long a claim is honoured before another
	// dispatcher may take the row. It bounds how long a crashed dispatcher
	// stalls its aggregates.
	DefaultClaimTTL = 60 * time.Second
	// DefaultMaxAttempts is the number of failed publishes after which a row
	// is marked dead and left for an operator.
	DefaultMaxAttempts = 25
	// maxLastErrorLen caps what a failed publish writes to last_error, so a
	// pathological broker error cannot bloat the table.
	maxLastErrorLen = 2000
	// truncationMarker is appended to a truncated last_error.
	truncationMarker = "…"
)

// eventColumnList is the outbox_events column set, in the order scanEvent
// reads it. Every SELECT/RETURNING in this file is built from it, so the
// column list and the scan can never drift apart.
var eventColumnList = []string{
	"id", "aggregate_type", "aggregate_id", "topic", "key", "payload",
	"headers", "created_at", "attempts", "next_attempt_at", "claimed_at",
	"claimed_by", "published_at", "dead_at", "last_error",
}

// columns renders eventColumnList with an optional table alias prefix.
func columns(prefix string) string {
	out := make([]string, len(eventColumnList))
	for i, c := range eventColumnList {
		out[i] = prefix + c
	}
	return strings.Join(out, ", ")
}

// insertSQL writes the producer-owned columns only: attempts, next_attempt_at,
// claimed_*, published_at and dead_at belong to the dispatcher and keep their
// defaults.
//
// created_at is clock_timestamp() rather than the column default now(),
// because now() is the transaction start time: several events enqueued in one
// transaction would share a created_at and their relative order would fall to
// the random UUID tie-break. clock_timestamp() advances within the
// transaction, so dispatch order matches enqueue order.
//
// ON CONFLICT (id) DO NOTHING makes the insert idempotent for producers that
// supply a retry-stable event_id (see NewEvent): a producer whose own
// transaction is retried enqueues the same id a second time and gets a no-op
// rather than a unique-violation that aborts the caller's transaction — and
// the caller's transaction is the state change itself, so aborting it would
// turn a duplicate event into a lost state change. The event that is already
// in the table wins; the second insert's payload is discarded, so a *derived*
// event — a different message about the same thing — must be given a fresh id
// rather than reusing one.
const insertSQL = `
	INSERT INTO outbox_events (
		id, aggregate_type, aggregate_id, topic, key, payload, headers, created_at
	) VALUES (
		$1, $2, $3, $4, $5, $6::jsonb, $7::jsonb, clock_timestamp()
	)
	ON CONFLICT (id) DO NOTHING`

// claimSQL claims up to $4 rows for dispatcher $1 at time $2, treating a claim
// older than $3 as expired.
//
// The inner SELECT encodes the whole eligibility contract:
//   - the row is neither published nor dead;
//   - its backoff has elapsed (next_attempt_at <= now);
//   - it is unclaimed, or its claim has expired (a crashed dispatcher);
//   - no earlier live row exists for the same aggregate, which is what keeps
//     per-aggregate order — a row claimed by another dispatcher is still
//     "live" and therefore still blocks its successors, while published and
//     dead rows deliberately do not.
//
// FOR UPDATE SKIP LOCKED lets many dispatchers claim disjoint rows in
// parallel; a row another transaction already locked is skipped rather than
// waited on.
var claimSQL = `
	WITH claimable AS (
		SELECT e.id
		FROM outbox_events e
		WHERE e.published_at IS NULL
		  AND e.dead_at IS NULL
		  AND e.next_attempt_at <= $2
		  AND (e.claimed_at IS NULL OR e.claimed_at <= $3)
		  AND NOT EXISTS (
			SELECT 1
			FROM outbox_events earlier
			WHERE earlier.aggregate_id = e.aggregate_id
			  AND earlier.published_at IS NULL
			  AND earlier.dead_at IS NULL
			  AND (earlier.created_at, earlier.id) < (e.created_at, e.id)
		  )
		ORDER BY e.created_at, e.id
		LIMIT $4
		FOR UPDATE SKIP LOCKED
	)
	UPDATE outbox_events o
	SET claimed_at = $2, claimed_by = $1
	FROM claimable c
	WHERE o.id = c.id
	RETURNING ` + columns("o.")

// markPublishedSQL is idempotent: re-marking an already published row keeps
// the first publish time. dead_at is cleared because a row that reached the
// bus is delivered, whatever an earlier attempt concluded.
//
// last_error is deliberately NOT cleared. attempts and last_error are the only
// record that this event took 9 tries and why; an operator reading the table
// after an incident needs to see that a published row struggled, and clearing
// it would make a queue that barely limped through look identical to one that
// never faltered.
const markPublishedSQL = `
	UPDATE outbox_events
	SET published_at = COALESCE(published_at, $2),
	    claimed_at   = NULL,
	    claimed_by   = NULL,
	    dead_at      = NULL
	WHERE id = $1`

// selectAttemptsSQL reads the attempt count and the current claim holder under
// a row lock, so two dispatchers holding stale claims cannot both increment
// from the same value and so MarkFailedBy can fence on the claim.
const selectAttemptsSQL = `
	SELECT attempts, published_at, claimed_by
	FROM outbox_events
	WHERE id = $1
	FOR UPDATE`

const markFailedRetrySQL = `
	UPDATE outbox_events
	SET attempts        = $2,
	    next_attempt_at = $3,
	    last_error      = $4,
	    claimed_at      = NULL,
	    claimed_by      = NULL
	WHERE id = $1`

const markFailedDeadSQL = `
	UPDATE outbox_events
	SET attempts        = $2,
	    dead_at         = $3,
	    last_error      = $4,
	    claimed_at      = NULL,
	    claimed_by      = NULL
	WHERE id = $1`

const pendingSQL = `
	SELECT COUNT(*), MIN(created_at)
	FROM outbox_events
	WHERE published_at IS NULL AND dead_at IS NULL`

var listDeadSQL = `
	SELECT ` + columns("") + `
	FROM outbox_events
	WHERE dead_at IS NOT NULL
	ORDER BY dead_at DESC, id
	LIMIT $1`

// requeueSQL puts a dead (or merely stuck) row back at the front of the
// queue. A published row is never resurrected.
const requeueSQL = `
	UPDATE outbox_events
	SET dead_at         = NULL,
	    attempts        = 0,
	    next_attempt_at = now(),
	    claimed_at      = NULL,
	    claimed_by      = NULL,
	    last_error      = NULL
	WHERE id = $1 AND published_at IS NULL`

// Enqueue writes ev inside the caller's transaction.
//
// It takes a pgx.Tx, never a pool, and there is deliberately no pool-based
// variant: the point of the outbox is that the event and the state change it
// describes commit together or not at all. A caller that only has a pool must
// open a transaction first.
//
// Enqueueing an id that is already in the table is a silent no-op rather than
// an error (see insertSQL): a producer using a retry-stable event_id stays
// idempotent, and the existing row's payload wins.
func Enqueue(ctx context.Context, tx pgx.Tx, ev Event) error {
	if tx == nil {
		return errors.New("outbox: Enqueue requires the caller's transaction")
	}
	args, err := insertArgs(ev)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, insertSQL, args...); err != nil {
		return fmt.Errorf("insert outbox event %s: %w", ev.ID, err)
	}
	return nil
}

// EnqueueAll writes several events in the caller's transaction, in the order
// given; that order is preserved on dispatch because created_at is
// clock_timestamp(). It stops at the first failure, which rolls back with the
// caller's transaction.
func EnqueueAll(ctx context.Context, tx pgx.Tx, evs ...Event) error {
	if len(evs) == 0 {
		return nil
	}
	for i, ev := range evs {
		if err := Enqueue(ctx, tx, ev); err != nil {
			return fmt.Errorf("event %d of %d: %w", i+1, len(evs), err)
		}
	}
	return nil
}

// Store owns the outbox_events table on behalf of a dispatcher (M1-02) and
// of operator tooling.
type Store struct {
	db          *pgxpool.Pool
	claimTTL    time.Duration
	maxAttempts int
	rand        func() float64
}

// Option customises a Store. The defaults are the approved delivery
// semantics; the options exist so tests can compress time.
type Option func(*Store)

// WithClaimTTL sets how long a claim is honoured. Non-positive values keep
// the default.
func WithClaimTTL(d time.Duration) Option {
	return func(s *Store) {
		if d > 0 {
			s.claimTTL = d
		}
	}
}

// WithMaxAttempts sets how many failures mark a row dead. Non-positive values
// keep the default.
func WithMaxAttempts(n int) Option {
	return func(s *Store) {
		if n > 0 {
			s.maxAttempts = n
		}
	}
}

// WithRand replaces the jitter source used when scheduling retries.
func WithRand(f func() float64) Option {
	return func(s *Store) {
		if f != nil {
			s.rand = f
		}
	}
}

// NewStore constructs a Store bound to the given pool.
func NewStore(db *pgxpool.Pool, opts ...Option) *Store {
	s := &Store{db: db, claimTTL: DefaultClaimTTL, maxAttempts: DefaultMaxAttempts}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Claim locks and claims up to limit dispatchable events for claimedBy and
// returns them in dispatch order (created_at, id). Claiming is a short
// transaction of its own: the rows are marked claimed before this returns, so
// a second dispatcher running concurrently sees a disjoint set.
//
// The returned slice may be shorter than limit, including empty, because at
// most one row per aggregate is ever eligible at a time.
func (s *Store) Claim(ctx context.Context, claimedBy string, limit int, now time.Time) ([]Event, error) {
	if claimedBy == "" {
		return nil, errors.New("outbox: claimedBy is required so an expired claim can be attributed")
	}
	if limit <= 0 {
		return nil, fmt.Errorf("outbox: claim limit must be positive, got %d", limit)
	}
	expiredBefore := now.Add(-s.claimTTL)
	rows, err := s.db.Query(ctx, claimSQL, claimedBy, now, expiredBefore, limit)
	if err != nil {
		return nil, fmt.Errorf("claim outbox events: %w", err)
	}
	defer rows.Close()

	out := []Event{}
	for rows.Next() {
		ev, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("claim outbox events: %w", err)
	}
	// UPDATE ... RETURNING does not promise the CTE's order.
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		// Same byte order PostgreSQL uses for uuid, so the Go sort and the
		// SQL ORDER BY agree on the tie-break.
		return bytes.Compare(out[i].ID[:], out[j].ID[:]) < 0
	})
	return out, nil
}

// MarkPublished records that the bus acknowledged the event and releases the
// claim. It is safe to call twice: the first publish time wins.
func (s *Store) MarkPublished(ctx context.Context, id uuid.UUID, now time.Time) error {
	if id == uuid.Nil {
		return ErrNotFound
	}
	tag, err := s.db.Exec(ctx, markPublishedSQL, id, now)
	if err != nil {
		return fmt.Errorf("mark outbox event %s published: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkFailed records a failed publish without checking who holds the claim.
// It is the operator-facing form; a dispatcher must use MarkFailedBy so it
// cannot act on a row it no longer owns.
//
// It increments attempts, stores the error, releases the claim, and either
// schedules the next attempt with NextAttempt's backoff or, once maxAttempts
// is reached, marks the row dead and returns dead=true. A dead row stops
// blocking later events for the same aggregate, so callers should log loudly
// when this returns true.
//
// The read and the write happen in one transaction with the row locked, so
// concurrent callers cannot lose an increment.
func (s *Store) MarkFailed(ctx context.Context, id uuid.UUID, cause error, now time.Time) (bool, error) {
	return s.markFailed(ctx, id, "", cause, now)
}

// MarkFailedBy is MarkFailed fenced on the claim: it applies only if the row
// is still claimed by claimedBy, and returns ErrClaimLost otherwise, having
// changed nothing.
//
// This is what a dispatcher calls. Without the fence, a dispatcher that was
// paused past the claim TTL — a long GC pause, a frozen VM, a publish that
// took longer than ClaimTTL — would come back, find its publish had failed,
// and write that failure onto a row a second dispatcher has since claimed and
// may already be publishing: it would burn an attempt that is not its own
// (bringing an innocent event closer to the dead letter), clear the other
// dispatcher's claim mid-flight, and overwrite next_attempt_at. The fence
// turns all of that into a single logged no-op, which is correct — the row now
// belongs to someone else, and whatever happens to it is their attempt to
// record.
func (s *Store) MarkFailedBy(ctx context.Context, id uuid.UUID, claimedBy string, cause error, now time.Time) (bool, error) {
	if claimedBy == "" {
		return false, errors.New("outbox: MarkFailedBy requires the claim holder; use MarkFailed to override the fence")
	}
	return s.markFailed(ctx, id, claimedBy, cause, now)
}

// markFailed is the shared body. owner == "" skips the claim fence.
func (s *Store) markFailed(ctx context.Context, id uuid.UUID, owner string, cause error, now time.Time) (bool, error) {
	if id == uuid.Nil {
		return false, ErrNotFound
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("mark outbox event %s failed: %w", id, err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var attempts int
	var publishedAt *time.Time
	var claimedBy *string
	if err := tx.QueryRow(ctx, selectAttemptsSQL, id).Scan(&attempts, &publishedAt, &claimedBy); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, ErrNotFound
		}
		return false, fmt.Errorf("read attempts for outbox event %s: %w", id, err)
	}
	if publishedAt != nil {
		// The publish did land (or another dispatcher recorded it); there is
		// nothing to retry and nothing to kill.
		return false, nil
	}
	if owner != "" {
		held := ""
		if claimedBy != nil {
			held = *claimedBy
		}
		if held != owner {
			// The row was re-claimed (or released) while this attempt ran.
			return false, fmt.Errorf("%w: outbox event %s is claimed by %q, not %q", ErrClaimLost, id, held, owner)
		}
	}

	attempts++
	lastErr := truncateError(cause)
	dead := attempts >= s.maxAttempts
	stmt, when := markFailedRetrySQL, NextAttempt(attempts, now, s.rand)
	if dead {
		stmt, when = markFailedDeadSQL, now
	}
	if _, err := tx.Exec(ctx, stmt, id, attempts, when, lastErr); err != nil {
		return false, fmt.Errorf("mark outbox event %s failed: %w", id, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("mark outbox event %s failed: %w", id, err)
	}
	return dead, nil
}

// Pending reports the dispatch backlog for metrics: how many events are
// neither published nor dead, and the creation time of the oldest of them
// (nil when there are none). A growing oldest age is the signal that the
// dispatcher is stuck or that one aggregate is blocked.
func (s *Store) Pending(ctx context.Context) (int, *time.Time, error) {
	var count int
	var oldest *time.Time
	if err := s.db.QueryRow(ctx, pendingSQL).Scan(&count, &oldest); err != nil {
		return 0, nil, fmt.Errorf("count pending outbox events: %w", err)
	}
	return count, oldest, nil
}

// ListDead returns rows that exhausted their attempts, newest first, for the
// operator runbook.
func (s *Store) ListDead(ctx context.Context, limit int) ([]Event, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("outbox: list limit must be positive, got %d", limit)
	}
	rows, err := s.db.Query(ctx, listDeadSQL, limit)
	if err != nil {
		return nil, fmt.Errorf("list dead outbox events: %w", err)
	}
	defer rows.Close()

	out := []Event{}
	for rows.Next() {
		ev, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list dead outbox events: %w", err)
	}
	return out, nil
}

// Requeue clears the dead marker and the retry state so the dispatcher picks
// the row up again. Unknown ids, and rows that have already been published,
// return ErrNotFound.
func (s *Store) Requeue(ctx context.Context, id uuid.UUID) error {
	if id == uuid.Nil {
		return ErrNotFound
	}
	tag, err := s.db.Exec(ctx, requeueSQL, id)
	if err != nil {
		return fmt.Errorf("requeue outbox event %s: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// insertArgs validates ev and produces the positional arguments for insertSQL,
// in column order. Kept separate from Enqueue so the mapping can be
// unit-tested without a database.
func insertArgs(ev Event) ([]any, error) {
	if ev.ID == uuid.Nil {
		return nil, errors.New("event id is required; construct events with NewEvent")
	}
	if ev.AggregateType == "" {
		return nil, errors.New("aggregate_type is required")
	}
	if ev.AggregateID == uuid.Nil {
		return nil, errors.New("aggregate_id is required")
	}
	if ev.Topic == "" {
		return nil, errors.New("topic is required")
	}
	if len(ev.Payload) == 0 {
		return nil, errors.New("payload is required")
	}
	if !json.Valid(ev.Payload) {
		return nil, errors.New("payload is not valid JSON")
	}
	headers := ev.Headers
	if headers == nil {
		headers = map[string]string{}
	}
	encodedHeaders, err := json.Marshal(headers)
	if err != nil {
		return nil, fmt.Errorf("encode headers: %w", err)
	}
	return []any{
		ev.ID,
		ev.AggregateType,
		ev.AggregateID,
		ev.Topic,
		ev.Key,
		[]byte(ev.Payload),
		encodedHeaders,
	}, nil
}

type scannerLike interface {
	Scan(dest ...any) error
}

// scanEvent maps one row of eventColumnList.
func scanEvent(r scannerLike) (*Event, error) {
	var (
		ev        Event
		payload   []byte
		headers   []byte
		claimedBy *string
		lastError *string
	)
	if err := r.Scan(
		&ev.ID, &ev.AggregateType, &ev.AggregateID, &ev.Topic, &ev.Key,
		&payload, &headers, &ev.CreatedAt, &ev.Attempts, &ev.NextAttemptAt,
		&ev.ClaimedAt, &claimedBy, &ev.PublishedAt, &ev.DeadAt, &lastError,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("scan outbox event: %w", err)
	}
	ev.Payload = append(json.RawMessage(nil), payload...)
	if len(headers) > 0 {
		if err := json.Unmarshal(headers, &ev.Headers); err != nil {
			return nil, fmt.Errorf("outbox event %s: decode headers: %w", ev.ID, err)
		}
	}
	if ev.Headers == nil {
		ev.Headers = map[string]string{}
	}
	if claimedBy != nil {
		ev.ClaimedBy = *claimedBy
	}
	if lastError != nil {
		ev.LastError = *lastError
	}
	return &ev, nil
}

// truncateError renders cause for the last_error column, bounded in length so
// one pathological broker error cannot bloat the row.
func truncateError(cause error) string {
	if cause == nil {
		return ""
	}
	runes := []rune(cause.Error())
	if len(runes) <= maxLastErrorLen {
		return string(runes)
	}
	marker := []rune(truncationMarker)
	return string(runes[:maxLastErrorLen-len(marker)]) + truncationMarker
}
