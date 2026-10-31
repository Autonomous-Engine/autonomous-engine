package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// insertColumns are the columns insertSQL writes, in order. They must match
// migration 047 and the order of insertArgs. created_at is written explicitly
// (clock_timestamp()) rather than left to the column default; see the SQL-text
// test below for why.
var insertColumns = []string{
	"id", "aggregate_type", "aggregate_id", "topic", "key", "payload",
	"headers", "created_at",
}

func sampleEvent(t *testing.T) Event {
	t.Helper()
	ev, err := NewEvent("work_item", uuid.New(), "implementation.tasks", "k", map[string]any{"work_id": "w1"})
	if err != nil {
		t.Fatalf("NewEvent: %v", err)
	}
	return ev
}

func TestInsertArgs_MapsColumnsInOrder(t *testing.T) {
	ev := sampleEvent(t)
	args, err := insertArgs(ev)
	if err != nil {
		t.Fatalf("insertArgs: %v", err)
	}
	// created_at is a SQL expression, not a bound argument.
	if len(args) != len(insertColumns)-1 {
		t.Fatalf("insertArgs returned %d args, insertSQL binds %d columns", len(args), len(insertColumns)-1)
	}
	if args[0].(uuid.UUID) != ev.ID {
		t.Errorf("id = %v, want %v", args[0], ev.ID)
	}
	if args[1].(string) != ev.AggregateType || args[2].(uuid.UUID) != ev.AggregateID {
		t.Errorf("aggregate columns = %v/%v", args[1], args[2])
	}
	if args[3].(string) != ev.Topic || args[4].(string) != ev.Key {
		t.Errorf("topic/key columns = %v/%v", args[3], args[4])
	}
	if !json.Valid(args[5].([]byte)) {
		t.Errorf("payload column is not valid JSON: %s", args[5])
	}
	var headers map[string]string
	if err := json.Unmarshal(args[6].([]byte), &headers); err != nil {
		t.Fatalf("headers column is not a JSON object: %v", err)
	}
	if headers[HeaderEventID] != ev.ID.String() {
		t.Errorf("headers column = %v, want the event id", headers)
	}
}

func TestInsertArgs_RejectsIncompleteEvents(t *testing.T) {
	cases := map[string]func(*Event){
		"aggregate_type": func(e *Event) { e.AggregateType = "" },
		"aggregate_id":   func(e *Event) { e.AggregateID = uuid.Nil },
		"topic":          func(e *Event) { e.Topic = "" },
		"id":             func(e *Event) { e.ID = uuid.Nil },
		"payload":        func(e *Event) { e.Payload = nil },
	}
	for want, mutate := range cases {
		t.Run(want, func(t *testing.T) {
			ev := sampleEvent(t)
			mutate(&ev)
			if _, err := insertArgs(ev); err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("error = %v, want it to mention %q", err, want)
			}
		})
	}
	t.Run("invalid payload json", func(t *testing.T) {
		ev := sampleEvent(t)
		ev.Payload = json.RawMessage(`{"a":`)
		if _, err := insertArgs(ev); err == nil {
			t.Error("accepted a payload that is not valid JSON")
		}
	})
}

func TestInsertArgs_NilHeadersBecomeAnEmptyObject(t *testing.T) {
	ev := sampleEvent(t)
	ev.Headers = nil
	args, err := insertArgs(ev)
	if err != nil {
		t.Fatalf("insertArgs: %v", err)
	}
	if got := string(args[6].([]byte)); got != "{}" {
		t.Errorf("headers column = %s, want {} (the column is NOT NULL)", got)
	}
}

func TestTruncateError(t *testing.T) {
	if got := truncateError(nil); got != "" {
		t.Errorf("truncateError(nil) = %q, want empty", got)
	}
	if got := truncateError(errors.New("boom")); got != "boom" {
		t.Errorf("truncateError = %q, want boom", got)
	}
	long := errors.New(strings.Repeat("x", maxLastErrorLen*2))
	got := truncateError(long)
	if len([]rune(got)) != maxLastErrorLen {
		t.Errorf("truncated length = %d runes, want %d", len([]rune(got)), maxLastErrorLen)
	}
	if !strings.HasSuffix(got, truncationMarker) {
		t.Errorf("a truncated error must be marked as truncated: %q", got[len(got)-8:])
	}
}

// fakeScanner stands in for a pgx row: it copies the given values into the
// scan destinations in order.
type fakeScanner struct {
	values []any
	err    error
}

func (f fakeScanner) Scan(dest ...any) error {
	if f.err != nil {
		return f.err
	}
	if len(dest) != len(f.values) {
		return errors.New("fakeScanner: destination count mismatch")
	}
	for i, d := range dest {
		v := f.values[i]
		switch d := d.(type) {
		case *uuid.UUID:
			*d = v.(uuid.UUID)
		case *string:
			*d = v.(string)
		case *[]byte:
			b, _ := v.([]byte)
			*d = b
		case *time.Time:
			*d = v.(time.Time)
		case *int:
			*d = v.(int)
		case **time.Time:
			p, _ := v.(*time.Time)
			*d = p
		case **string:
			p, _ := v.(*string)
			*d = p
		default:
			return errors.New("fakeScanner: unsupported destination type")
		}
	}
	return nil
}

func rowValues(ev Event) []any {
	return []any{
		ev.ID, ev.AggregateType, ev.AggregateID, ev.Topic, ev.Key,
		[]byte(ev.Payload), []byte(`{"event_id":"` + ev.ID.String() + `"}`),
		ev.CreatedAt, ev.Attempts, ev.NextAttemptAt,
		ev.ClaimedAt, (*string)(nil), ev.PublishedAt, ev.DeadAt, (*string)(nil),
	}
}

func TestScanEvent_MapsRow(t *testing.T) {
	ev := sampleEvent(t)
	ev.CreatedAt = time.Date(2026, 9, 12, 9, 0, 0, 0, time.UTC)
	ev.NextAttemptAt = ev.CreatedAt
	ev.Attempts = 3
	claimed := ev.CreatedAt.Add(time.Minute)
	ev.ClaimedAt = &claimed

	values := rowValues(ev)
	who := "dispatcher-1"
	boom := "boom"
	values[11] = &who
	values[14] = &boom

	got, err := scanEvent(fakeScanner{values: values})
	if err != nil {
		t.Fatalf("scanEvent: %v", err)
	}
	if got.ID != ev.ID || got.AggregateID != ev.AggregateID || got.Topic != ev.Topic {
		t.Errorf("identity mismatch: %+v", got)
	}
	if got.Attempts != 3 || !got.CreatedAt.Equal(ev.CreatedAt) {
		t.Errorf("bookkeeping mismatch: %+v", got)
	}
	if got.ClaimedBy != who || got.LastError != boom {
		t.Errorf("nullable text columns = %q/%q, want %q/%q", got.ClaimedBy, got.LastError, who, boom)
	}
	if got.ClaimedAt == nil || !got.ClaimedAt.Equal(claimed) {
		t.Errorf("claimed_at = %v, want %v", got.ClaimedAt, claimed)
	}
	if got.PublishedAt != nil || got.DeadAt != nil {
		t.Errorf("published_at/dead_at = %v/%v, want nil", got.PublishedAt, got.DeadAt)
	}
	if got.Headers[HeaderEventID] != ev.ID.String() {
		t.Errorf("headers = %v, want the event id", got.Headers)
	}
}

func TestScanEvent_NullTextColumnsAreEmptyStrings(t *testing.T) {
	ev := sampleEvent(t)
	got, err := scanEvent(fakeScanner{values: rowValues(ev)})
	if err != nil {
		t.Fatalf("scanEvent: %v", err)
	}
	if got.ClaimedBy != "" || got.LastError != "" {
		t.Errorf("NULL text columns = %q/%q, want empty strings", got.ClaimedBy, got.LastError)
	}
}

func TestScanEvent_NoRowsIsErrNotFound(t *testing.T) {
	if _, err := scanEvent(fakeScanner{err: pgx.ErrNoRows}); !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}

func TestScanEvent_RejectsCorruptHeaders(t *testing.T) {
	ev := sampleEvent(t)
	values := rowValues(ev)
	values[6] = []byte(`{"a":`)
	if _, err := scanEvent(fakeScanner{values: values}); err == nil {
		t.Error("accepted corrupt headers JSON")
	}
}

// A Store with a nil pool panics on any query, so these prove the guards
// return before touching the database.
func TestStoreGuards_ReturnBeforeQuerying(t *testing.T) {
	s := NewStore(nil)
	ctx := context.Background()

	if _, err := s.Claim(ctx, "", 10, time.Now()); err == nil {
		t.Error("Claim accepted an empty claimedBy")
	}
	if _, err := s.Claim(ctx, "d1", 0, time.Now()); err == nil {
		t.Error("Claim accepted a non-positive limit")
	}
	if _, err := s.ListDead(ctx, -1); err == nil {
		t.Error("ListDead accepted a non-positive limit")
	}
	if err := s.MarkPublished(ctx, uuid.Nil, time.Now()); !errors.Is(err, ErrNotFound) {
		t.Errorf("MarkPublished(nil uuid) = %v, want ErrNotFound", err)
	}
	if _, err := s.MarkFailed(ctx, uuid.Nil, errors.New("boom"), time.Now()); !errors.Is(err, ErrNotFound) {
		t.Errorf("MarkFailed(nil uuid) = %v, want ErrNotFound", err)
	}
	if _, err := s.MarkFailedBy(ctx, uuid.New(), "", errors.New("boom"), time.Now()); err == nil {
		t.Error("MarkFailedBy accepted an empty claim holder; the fence would be a no-op")
	}
	if _, err := s.MarkFailedBy(ctx, uuid.Nil, "d1", errors.New("boom"), time.Now()); !errors.Is(err, ErrNotFound) {
		t.Errorf("MarkFailedBy(nil uuid) = %v, want ErrNotFound", err)
	}
	if err := s.Requeue(ctx, uuid.Nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("Requeue(nil uuid) = %v, want ErrNotFound", err)
	}
}

func TestNewStore_Defaults(t *testing.T) {
	s := NewStore(nil)
	if s.claimTTL != DefaultClaimTTL {
		t.Errorf("claimTTL = %v, want %v", s.claimTTL, DefaultClaimTTL)
	}
	if s.maxAttempts != DefaultMaxAttempts {
		t.Errorf("maxAttempts = %d, want %d", s.maxAttempts, DefaultMaxAttempts)
	}
	s2 := NewStore(nil, WithClaimTTL(5*time.Second), WithMaxAttempts(3), WithRand(noJitter))
	if s2.claimTTL != 5*time.Second || s2.maxAttempts != 3 || s2.rand == nil {
		t.Errorf("options not applied: %+v", s2)
	}
	if NewStore(nil, WithClaimTTL(0)).claimTTL != DefaultClaimTTL {
		t.Error("a non-positive claim TTL must fall back to the default")
	}
	if NewStore(nil, WithMaxAttempts(-1)).maxAttempts != DefaultMaxAttempts {
		t.Error("a non-positive max attempts must fall back to the default")
	}
}

func TestEnqueue_RequiresATransaction(t *testing.T) {
	if err := Enqueue(context.Background(), nil, sampleEvent(t)); err == nil ||
		!strings.Contains(err.Error(), "transaction") {
		t.Errorf("Enqueue(nil tx) = %v, want a complaint about the transaction", err)
	}
	if err := EnqueueAll(context.Background(), nil, sampleEvent(t)); err == nil {
		t.Error("EnqueueAll(nil tx) accepted a nil transaction")
	}
	if err := EnqueueAll(context.Background(), nil); err != nil {
		t.Errorf("EnqueueAll with no events = %v, want nil (nothing to do)", err)
	}
}

// Enqueue must only ever be reachable with a caller-owned transaction: there
// is deliberately no pool-based variant, so an event cannot be written outside
// the transaction that made the state change it describes.
func TestNoPoolBasedEnqueueExists(t *testing.T) {
	src, err := os.ReadFile("store.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	for _, forbidden := range []string{
		"func (s *Store) Enqueue",
		"func EnqueueWithPool",
		"s.db.Exec(ctx, insertSQL",
	} {
		if strings.Contains(text, forbidden) {
			t.Errorf("store.go declares %q; Enqueue must take the caller's pgx.Tx only", forbidden)
		}
	}
	if !strings.Contains(text, "func Enqueue(ctx context.Context, tx pgx.Tx, ev Event) error") {
		t.Error("Enqueue must keep the signature Enqueue(ctx, tx pgx.Tx, ev Event) error")
	}
}

var placeholderRe = regexp.MustCompile(`\$(\d+)`)

func TestSQLText_Insert(t *testing.T) {
	if !strings.Contains(insertSQL, "INSERT INTO outbox_events") {
		t.Fatalf("insertSQL does not target outbox_events: %s", insertSQL)
	}
	open := strings.Index(insertSQL, "(")
	closeIdx := strings.Index(insertSQL, ")")
	var cols []string
	for _, c := range strings.Split(insertSQL[open+1:closeIdx], ",") {
		cols = append(cols, strings.TrimSpace(c))
	}
	if !reflect.DeepEqual(cols, insertColumns) {
		t.Errorf("insert columns = %v, want %v", cols, insertColumns)
	}
	matches := placeholderRe.FindAllStringSubmatch(insertSQL, -1)
	if len(matches) != len(insertColumns)-1 {
		t.Errorf("insertSQL has %d placeholders, want %d", len(matches), len(insertColumns)-1)
	}
	for i, m := range matches {
		if want := i + 1; m[1] != strconv.Itoa(want) {
			t.Errorf("placeholder %d is $%s, want $%d", i, m[1], want)
		}
	}
	for _, frag := range []string{"$6::jsonb", "$7::jsonb", "clock_timestamp()"} {
		if !strings.Contains(insertSQL, frag) {
			t.Errorf("insertSQL missing %q: %s", frag, insertSQL)
		}
	}
	// A producer that re-enqueues a retry-stable event_id must get a no-op,
	// not a unique violation that aborts its state-change transaction.
	if !strings.Contains(insertSQL, "ON CONFLICT (id) DO NOTHING") {
		t.Errorf("insertSQL must be idempotent on the event id:\n%s", insertSQL)
	}
	if strings.Contains(insertSQL, "DO UPDATE") {
		t.Errorf("a duplicate id must not overwrite the stored event:\n%s", insertSQL)
	}
	// The dispatcher owns these; a producer must never set them.
	for _, forbidden := range []string{"published_at", "dead_at", "attempts", "claimed_"} {
		if strings.Contains(insertSQL, forbidden) {
			t.Errorf("insertSQL writes dispatcher-owned column %q", forbidden)
		}
	}
}

func TestSQLText_Claim(t *testing.T) {
	for _, frag := range []string{
		// eligibility
		"e.published_at IS NULL",
		"e.dead_at IS NULL",
		"e.next_attempt_at <= $2",
		"(e.claimed_at IS NULL OR e.claimed_at <= $3)",
		// per-aggregate ordering: no earlier live row for this aggregate
		"NOT EXISTS",
		"earlier.aggregate_id = e.aggregate_id",
		"earlier.published_at IS NULL",
		"earlier.dead_at IS NULL",
		"(earlier.created_at, earlier.id) < (e.created_at, e.id)",
		// ordering, batching, concurrency
		"ORDER BY e.created_at, e.id",
		"LIMIT $4",
		"FOR UPDATE SKIP LOCKED",
		// the claim itself
		"SET claimed_at = $2, claimed_by = $1",
	} {
		if !strings.Contains(claimSQL, frag) {
			t.Errorf("claimSQL missing %q:\n%s", frag, claimSQL)
		}
	}
	if n := strings.Count(claimSQL, "FOR UPDATE"); n != 1 {
		t.Errorf("claimSQL has %d FOR UPDATE clauses, want exactly 1", n)
	}
	if strings.Count(claimSQL, "SKIP LOCKED") != 1 {
		t.Errorf("every FOR UPDATE in claimSQL must SKIP LOCKED:\n%s", claimSQL)
	}
	if !strings.Contains(claimSQL, "RETURNING "+columns("o.")) {
		t.Errorf("claimSQL must return exactly the columns scanEvent reads:\n%s", claimSQL)
	}
}

func TestSQLText_MarkAndOperatorStatements(t *testing.T) {
	t.Run("mark published releases the claim and is idempotent", func(t *testing.T) {
		for _, frag := range []string{
			"UPDATE outbox_events",
			"published_at = COALESCE(published_at, $2)",
			"claimed_at   = NULL",
			"claimed_by   = NULL",
			"WHERE id = $1",
		} {
			if !strings.Contains(markPublishedSQL, frag) {
				t.Errorf("markPublishedSQL missing %q:\n%s", frag, markPublishedSQL)
			}
		}
		// attempts and last_error are the only record that this event
		// struggled before it landed; publishing must not erase that history.
		if strings.Contains(markPublishedSQL, "last_error") {
			t.Errorf("markPublishedSQL must preserve last_error:\n%s", markPublishedSQL)
		}
		if strings.Contains(markPublishedSQL, "attempts") {
			t.Errorf("markPublishedSQL must preserve attempts:\n%s", markPublishedSQL)
		}
	})
	t.Run("mark failed retry schedules and releases the claim", func(t *testing.T) {
		for _, frag := range []string{
			"attempts        = $2",
			"next_attempt_at = $3",
			"last_error      = $4",
			"claimed_at      = NULL",
			"claimed_by      = NULL",
			"WHERE id = $1",
		} {
			if !strings.Contains(markFailedRetrySQL, frag) {
				t.Errorf("markFailedRetrySQL missing %q:\n%s", frag, markFailedRetrySQL)
			}
		}
		if strings.Contains(markFailedRetrySQL, "dead_at") {
			t.Error("a retry must not touch dead_at")
		}
	})
	t.Run("mark failed dead records the tombstone", func(t *testing.T) {
		for _, frag := range []string{"dead_at         = $3", "attempts        = $2", "last_error      = $4"} {
			if !strings.Contains(markFailedDeadSQL, frag) {
				t.Errorf("markFailedDeadSQL missing %q:\n%s", frag, markFailedDeadSQL)
			}
		}
		if strings.Contains(markFailedDeadSQL, "next_attempt_at") {
			t.Error("a dead row must not be rescheduled")
		}
	})
	t.Run("attempts are read FOR UPDATE so two dispatchers cannot both increment", func(t *testing.T) {
		if !strings.Contains(selectAttemptsSQL, "FOR UPDATE") {
			t.Errorf("selectAttemptsSQL must lock the row: %s", selectAttemptsSQL)
		}
		// MarkFailedBy fences on the claim holder, which must be read under
		// the same lock as attempts or the check would race the re-claim.
		if !strings.Contains(selectAttemptsSQL, "claimed_by") {
			t.Errorf("selectAttemptsSQL must read claimed_by for the MarkFailedBy fence: %s", selectAttemptsSQL)
		}
	})
	t.Run("pending counts only live rows", func(t *testing.T) {
		for _, frag := range []string{"COUNT(*)", "MIN(created_at)", "published_at IS NULL", "dead_at IS NULL"} {
			if !strings.Contains(pendingSQL, frag) {
				t.Errorf("pendingSQL missing %q: %s", frag, pendingSQL)
			}
		}
	})
	t.Run("list dead selects only dead rows", func(t *testing.T) {
		if !strings.Contains(listDeadSQL, "WHERE dead_at IS NOT NULL") || !strings.Contains(listDeadSQL, "LIMIT $1") {
			t.Errorf("listDeadSQL = %s", listDeadSQL)
		}
		if !strings.Contains(listDeadSQL, "SELECT "+columns("")) {
			t.Errorf("listDeadSQL must select exactly the columns scanEvent reads:\n%s", listDeadSQL)
		}
	})
	t.Run("requeue resets the retry state but never resurrects a published row", func(t *testing.T) {
		for _, frag := range []string{
			"dead_at         = NULL",
			"attempts        = 0",
			"next_attempt_at = now()",
			"last_error      = NULL",
			"WHERE id = $1 AND published_at IS NULL",
		} {
			if !strings.Contains(requeueSQL, frag) {
				t.Errorf("requeueSQL missing %q:\n%s", frag, requeueSQL)
			}
		}
	})
}

const migration047Version = "047_outbox_events"

func migrationsDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func readMigration(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(migrationsDir(t), name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}

// The migration runner (internal/db/migrate.go) picks up every *.up.sql in
// lexical order and records the filename minus ".up.sql" as the version.
// These checks pin the properties 047 relies on, plus the CLAUDE.md rules.
func TestMigration047_FollowsRunnerAndCLAUDEmdRules(t *testing.T) {
	up := readMigration(t, migration047Version+".up.sql")
	down := readMigration(t, migration047Version+".down.sql")

	t.Run("runner ordering", func(t *testing.T) {
		entries, err := os.ReadDir(migrationsDir(t))
		if err != nil {
			t.Fatal(err)
		}
		var ups []string
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".up.sql") {
				ups = append(ups, e.Name())
			}
		}
		sort.Strings(ups)
		idx046, idx047 := -1, -1
		for i, name := range ups {
			switch {
			case strings.HasPrefix(name, "046_"):
				idx046 = i
			case name == migration047Version+".up.sql":
				idx047 = i
			}
		}
		if idx046 < 0 || idx047 < 0 {
			t.Fatalf("046 (%d) or 047 (%d) not found among %v", idx046, idx047, ups)
		}
		if idx047 <= idx046 {
			t.Errorf("047 must sort after 046 in the runner's lexical order; got 046@%d 047@%d", idx046, idx047)
		}
		// 047 was the highest migration when it landed; later ones (048,
		// M1-06) legitimately sort after it. What must hold is that no other
		// migration claims the 047 number.
		for i, name := range ups {
			if i != idx047 && strings.HasPrefix(name, "047_") {
				t.Errorf("another migration claims number 047: %s", name)
			}
		}
		if got := strings.TrimSuffix(filepath.Base(ups[idx047]), ".up.sql"); got != migration047Version {
			t.Errorf("runner would derive version %q, want %q", got, migration047Version)
		}
		if strings.Contains(up, "ALTER TYPE") || strings.Contains(down, "ALTER TYPE") {
			t.Error("047 must not need the runner's ALTER TYPE extraction")
		}
	})

	t.Run("up is guarded and re-runnable", func(t *testing.T) {
		for _, frag := range []string{
			"CREATE TABLE IF NOT EXISTS outbox_events",
			"CREATE INDEX IF NOT EXISTS idx_outbox_events_ready",
			"CREATE INDEX IF NOT EXISTS idx_outbox_events_aggregate",
		} {
			if !strings.Contains(up, frag) {
				t.Errorf("up migration missing %q", frag)
			}
		}
	})

	t.Run("indexes match the claim query", func(t *testing.T) {
		if !regexp.MustCompile(`(?s)idx_outbox_events_ready\s+ON outbox_events \(next_attempt_at, created_at\)\s+WHERE published_at IS NULL AND dead_at IS NULL`).MatchString(up) {
			t.Errorf("the ready index must be partial on (next_attempt_at, created_at):\n%s", up)
		}
		if !regexp.MustCompile(`(?s)idx_outbox_events_aggregate\s+ON outbox_events \(aggregate_id, created_at\)`).MatchString(up) {
			t.Errorf("the per-aggregate ordering index is missing:\n%s", up)
		}
	})

	t.Run("no foreign keys", func(t *testing.T) {
		// Events must be able to outlive (or predate) the rows they describe,
		// so the table carries no FK; CLAUDE.md rule 3 therefore cannot be
		// violated here, and this test keeps it that way.
		if strings.Contains(up, "REFERENCES") {
			t.Errorf("outbox_events must not reference another table:\n%s", up)
		}
	})

	t.Run("up declares every column the store reads", func(t *testing.T) {
		for _, col := range eventColumnList {
			if !regexp.MustCompile(`(?m)^\s+` + col + `\s`).MatchString(up) {
				t.Errorf("up migration does not declare column %q", col)
			}
		}
		for _, frag := range []string{
			"id              UUID        PRIMARY KEY DEFAULT gen_random_uuid()",
			"aggregate_type  TEXT        NOT NULL",
			"aggregate_id    UUID        NOT NULL",
			"topic           TEXT        NOT NULL",
			"key             TEXT        NOT NULL DEFAULT ''",
			"payload         JSONB       NOT NULL",
			"headers         JSONB       NOT NULL DEFAULT '{}'",
			"created_at      TIMESTAMPTZ NOT NULL DEFAULT now()",
			"attempts        INT         NOT NULL DEFAULT 0",
			"next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now()",
		} {
			if !strings.Contains(up, frag) {
				t.Errorf("column declaration missing: %q", frag)
			}
		}
	})

	t.Run("down is guarded and complete", func(t *testing.T) {
		for _, frag := range []string{
			"DROP INDEX IF EXISTS idx_outbox_events_aggregate",
			"DROP INDEX IF EXISTS idx_outbox_events_ready",
			"DROP TABLE IF EXISTS outbox_events",
		} {
			if !strings.Contains(down, frag) {
				t.Errorf("down migration missing %q", frag)
			}
		}
	})
}
