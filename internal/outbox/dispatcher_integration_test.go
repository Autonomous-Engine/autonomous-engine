package outbox_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/metrics"
	"github.com/autonomousengine/autonomous-engine/internal/outbox"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/segmentio/kafka-go"
)

// These tests need both a disposable PostgreSQL database (TEST_POSTGRES_DSN)
// and the compose Kafka (TEST_KAFKA_BROKERS, e.g. kafka:9092). The
// store-only ones below need the database alone.

// integrationBrokers returns the broker list, skipping when unset.
func integrationBrokers(t *testing.T) []string {
	t.Helper()
	v := os.Getenv("TEST_KAFKA_BROKERS")
	if v == "" {
		t.Skip("TEST_KAFKA_BROKERS not set — skipping Kafka-integration test")
	}
	var out []string
	for _, b := range strings.Split(v, ",") {
		if b = strings.TrimSpace(b); b != "" {
			out = append(out, b)
		}
	}
	if len(out) == 0 {
		t.Skip("TEST_KAFKA_BROKERS is empty — skipping Kafka-integration test")
	}
	return out
}

// testTopic creates a fresh single-partition topic and returns its name.
//
// A dedicated topic keeps the assertions deterministic: no other producer
// writes to it, so every message on it came from the dispatcher under test,
// and one partition means "read from the first offset" is the complete,
// correctly ordered history rather than a merge across six readers.
func testTopic(t *testing.T, brokers []string) string {
	t.Helper()
	topic := "outbox.test." + strings.ReplaceAll(uuid.NewString(), "-", "")
	cc := dialController(t, brokers)
	defer cc.Close()
	if err := cc.CreateTopics(kafka.TopicConfig{
		Topic:             topic,
		NumPartitions:     1,
		ReplicationFactor: 1,
	}); err != nil {
		t.Fatalf("create topic %s: %v", topic, err)
	}
	t.Cleanup(func() {
		c, err := kafka.Dial("tcp", brokers[0])
		if err != nil {
			return
		}
		defer c.Close()
		// Best effort: delete.topic.enable may be off on this broker.
		if err := c.DeleteTopics(topic); err != nil {
			t.Logf("delete topic %s: %v", topic, err)
		}
	})
	return topic
}

func dialController(t *testing.T, brokers []string) *kafka.Conn {
	t.Helper()
	conn, err := kafka.Dial("tcp", brokers[0])
	if err != nil {
		t.Fatalf("dial %s: %v", brokers[0], err)
	}
	defer conn.Close()
	ctrl, err := conn.Controller()
	if err != nil {
		t.Fatalf("controller: %v", err)
	}
	addr := net.JoinHostPort(ctrl.Host, strconv.Itoa(ctrl.Port))
	cc, err := kafka.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial controller %s: %v", addr, err)
	}
	return cc
}

// readMessages reads from the start of a single-partition topic until it has
// want messages or the deadline passes. It uses a plain partition reader with
// no consumer group, so it commits nothing and competes with nobody.
func readMessages(t *testing.T, brokers []string, topic string, want int, timeout time.Duration) []kafka.Message {
	t.Helper()
	r := kafka.NewReader(kafka.ReaderConfig{
		Brokers:   brokers,
		Topic:     topic,
		Partition: 0,
		MinBytes:  1,
		MaxBytes:  10e6,
	})
	defer r.Close()
	if err := r.SetOffset(kafka.FirstOffset); err != nil {
		t.Fatalf("set offset: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var out []kafka.Message
	for len(out) < want {
		m, err := r.ReadMessage(ctx)
		if err != nil {
			break
		}
		out = append(out, m)
	}
	return out
}

func headerValue(m kafka.Message, key string) string {
	for _, h := range m.Headers {
		if h.Key == key {
			return string(h.Value)
		}
	}
	return ""
}

func payloadEventID(t *testing.T, value []byte) string {
	t.Helper()
	var fields map[string]any
	if err := json.Unmarshal(value, &fields); err != nil {
		t.Fatalf("decode payload %s: %v", value, err)
	}
	s, _ := fields[outbox.FieldEventID].(string)
	return s
}

// kafkaBus builds the real publisher against the compose cluster.
func kafkaBus(t *testing.T, brokers []string) *bus.KafkaBus {
	t.Helper()
	kb, err := bus.NewKafkaBus(brokers)
	if err != nil {
		t.Fatalf("NewKafkaBus(%v): %v", brokers, err)
	}
	t.Cleanup(func() {
		if err := kb.Close(); err != nil {
			t.Logf("close bus: %v", err)
		}
	})
	return kb
}

// rowState is the dispatcher-owned half of a row, read straight from SQL so
// the assertions do not depend on the store's own scan.
type rowState struct {
	Attempts    int
	NextAttempt time.Time
	ClaimedBy   string
	ClaimedAt   *time.Time
	PublishedAt *time.Time
	DeadAt      *time.Time
	LastError   string
}

func readRow(t *testing.T, p *pgxpool.Pool, id uuid.UUID) rowState {
	t.Helper()
	var st rowState
	var claimedBy, lastError *string
	err := p.QueryRow(context.Background(), `
		SELECT attempts, next_attempt_at, claimed_by, claimed_at, published_at, dead_at, last_error
		FROM outbox_events WHERE id = $1`, id).
		Scan(&st.Attempts, &st.NextAttempt, &claimedBy, &st.ClaimedAt, &st.PublishedAt, &st.DeadAt, &lastError)
	if err != nil {
		t.Fatalf("read row %s: %v", id, err)
	}
	if claimedBy != nil {
		st.ClaimedBy = *claimedBy
	}
	if lastError != nil {
		st.LastError = *lastError
	}
	return st
}

func topicEvent(t *testing.T, agg uuid.UUID, topic string, seq int) outbox.Event {
	t.Helper()
	ev, err := outbox.NewEvent("work_item", agg, topic, agg.String(), map[string]any{
		"work_id": agg.String(),
		"seq":     seq,
	})
	if err != nil {
		t.Fatalf("NewEvent: %v", err)
	}
	return ev
}

func testDispatcherOptions(t *testing.T, name string) outbox.Options {
	t.Helper()
	return outbox.Options{
		BatchSize:      10,
		ClaimedBy:      name,
		PublishTimeout: 20 * time.Second,
		Logger:         log.New(testWriter{t}, "", log.Lmicroseconds),
	}
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// scrapeMetric reads one metric out of the process-wide registry's exposition
// output; the registry has no read API.
func scrapeMetric(t *testing.T, name string) int64 {
	t.Helper()
	rec := httptest.NewRecorder()
	metrics.Handler()(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body, err := io.ReadAll(rec.Result().Body)
	if err != nil {
		t.Fatalf("read /metrics: %v", err)
	}
	m := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + ` (-?\d+)$`).FindStringSubmatch(string(body))
	if m == nil {
		t.Fatalf("metric %q not exported:\n%s", name, body)
	}
	v, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		t.Fatalf("metric %q = %q: %v", name, m[1], err)
	}
	return v
}

// ---------------------------------------------------------------------------
// Kafka-backed
// ---------------------------------------------------------------------------

// The end-to-end path: a producer commits events in its own transaction, the
// dispatcher publishes them to Kafka in order, every message carries its
// event_id in a header as well as in the payload, and the rows end up marked
// published.
func TestIntegration_DispatcherPublishesCommittedEventsToKafka(t *testing.T) {
	p := integrationPool(t)
	brokers := integrationBrokers(t)
	kb := kafkaBus(t, brokers)
	topic := testTopic(t, brokers)
	agg := newAggregate(t, p)

	evs := []outbox.Event{
		topicEvent(t, agg, topic, 1),
		topicEvent(t, agg, topic, 2),
		topicEvent(t, agg, topic, 3),
	}
	commitEvents(t, p, evs...)

	store := outbox.NewStore(p)
	d := outbox.NewDispatcher(store, kb, testDispatcherOptions(t, "it-publish/1"))

	before := scrapeMetric(t, "ae_outbox_published_total")
	// One event per aggregate is claimable at a time, so draining three
	// events of one aggregate takes three passes — and that is exactly the
	// ordering guarantee being exercised.
	for i := range evs {
		n, err := d.RunOnce(context.Background())
		if err != nil {
			t.Fatalf("RunOnce %d: %v", i, err)
		}
		if n != 1 {
			t.Fatalf("pass %d dispatched %d events, want 1 (per-aggregate ordering)", i, n)
		}
	}
	if delta := scrapeMetric(t, "ae_outbox_published_total") - before; delta != 3 {
		t.Errorf("ae_outbox_published_total moved %d, want 3", delta)
	}

	msgs := readMessages(t, brokers, topic, 3, 30*time.Second)
	if len(msgs) != 3 {
		t.Fatalf("read %d messages from %s, want 3", len(msgs), topic)
	}
	for i, ev := range evs {
		if got := headerValue(msgs[i], outbox.HeaderEventID); got != ev.ID.String() {
			t.Errorf("message %d header %s = %q, want %s (and the order must match the enqueue order)",
				i, outbox.HeaderEventID, got, ev.ID)
		}
		if got := payloadEventID(t, msgs[i].Value); got != ev.ID.String() {
			t.Errorf("message %d payload %s = %q, want %s", i, outbox.FieldEventID, got, ev.ID)
		}
		if got := string(msgs[i].Key); got != agg.String() {
			t.Errorf("message %d key = %q, want %s", i, got, agg)
		}
		st := readRow(t, p, ev.ID)
		if st.PublishedAt == nil {
			t.Errorf("event %d was published to Kafka but the row is not marked published", i)
		}
		if st.ClaimedBy != "" || st.ClaimedAt != nil {
			t.Errorf("event %d kept its claim after publishing: %+v", i, st)
		}
		if st.DeadAt != nil {
			t.Errorf("event %d is marked dead: %+v", i, st)
		}
	}

	// The backlog gauges were refreshed from the real Pending query.
	if got := scrapeMetric(t, "ae_outbox_pending"); got != 0 {
		t.Errorf("ae_outbox_pending = %d after draining, want 0", got)
	}
}

// The crash-injection case from the roadmap: the publish reaches the broker
// but the process dies before MarkPublished. Once the claim expires the event
// is delivered a second time, with the same event_id — which is exactly why
// consumers deduplicate on it (M1-05).
func TestIntegration_CrashAfterPublishRedeliversTheSameEventID(t *testing.T) {
	p := integrationPool(t)
	brokers := integrationBrokers(t)
	kb := kafkaBus(t, brokers)
	topic := testTopic(t, brokers)
	agg := newAggregate(t, p)

	ev := topicEvent(t, agg, topic, 1)
	commitEvents(t, p, ev)

	// The default 60s TTL, so the "still fresh" half cannot flake on a slow
	// broker; expiry is then forced by backdating claimed_at rather than by
	// sleeping. That the TTL really does elapse on the clock is the store's
	// own TestIntegration_ExpiredClaimIsReclaimable.
	store := outbox.NewStore(p)

	// "Dies" after the broker acks: the publish happened, the bookkeeping
	// never did.
	dying := outbox.NewDispatcherWithStore(
		crashingStore{Store: store},
		kb,
		testDispatcherOptions(t, "it-crash-victim/1"),
	)
	if n, err := dying.RunOnce(context.Background()); err != nil || n != 1 {
		t.Fatalf("first pass dispatched %d (err %v), want 1", n, err)
	}

	st := readRow(t, p, ev.ID)
	if st.PublishedAt != nil {
		t.Fatal("the crash simulation did not skip MarkPublished")
	}
	if st.ClaimedBy != "it-crash-victim/1" {
		t.Fatalf("the dead dispatcher's claim is %q, want it-crash-victim/1", st.ClaimedBy)
	}

	// A survivor must NOT steal the row while the claim is still fresh, or
	// every publish slower than a poll would be duplicated on purpose. This
	// runs before the Kafka read below, which can take seconds on a
	// freshly created topic.
	survivor := outbox.NewDispatcher(store, kb, testDispatcherOptions(t, "it-crash-survivor/2"))
	if n, err := survivor.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	} else if n != 0 {
		t.Fatalf("a live claim was stolen (dispatched %d)", n)
	}

	first := readMessages(t, brokers, topic, 1, 30*time.Second)
	if len(first) != 1 {
		t.Fatalf("the publish did not reach Kafka (%d messages)", len(first))
	}

	// Once the claim expires, it is re-delivered.
	expireClaim(t, p, ev.ID)
	if n, err := survivor.RunOnce(context.Background()); err != nil || n != 1 {
		t.Fatalf("after the claim expired the survivor dispatched %d (err %v), want 1", n, err)
	}

	both := readMessages(t, brokers, topic, 2, 30*time.Second)
	if len(both) != 2 {
		t.Fatalf("read %d messages, want 2 (the original and the redelivery)", len(both))
	}
	for i, m := range both {
		if got := headerValue(m, outbox.HeaderEventID); got != ev.ID.String() {
			t.Errorf("delivery %d carries event_id %q, want %s — a redelivery MUST reuse the id",
				i, got, ev.ID)
		}
	}
	if string(both[0].Value) != string(both[1].Value) {
		t.Errorf("the redelivered payload differs:\n%s\n%s", both[0].Value, both[1].Value)
	}
	st = readRow(t, p, ev.ID)
	if st.PublishedAt == nil {
		t.Error("the surviving dispatcher did not mark the row published")
	}
	if st.Attempts != 0 {
		t.Errorf("attempts = %d; a redelivery after a crash is not a failed attempt", st.Attempts)
	}
}

// expireClaim backdates a row's claim past any plausible TTL, which is what a
// crashed dispatcher's row looks like once claim_ttl has elapsed.
func expireClaim(t *testing.T, p *pgxpool.Pool, id uuid.UUID) {
	t.Helper()
	tag, err := p.Exec(context.Background(),
		`UPDATE outbox_events SET claimed_at = claimed_at - INTERVAL '1 hour' WHERE id = $1`, id)
	if err != nil {
		t.Fatalf("expire claim %s: %v", id, err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("expire claim %s affected %d rows", id, tag.RowsAffected())
	}
}

// crashingStore is a real Store whose MarkPublished never happens: the
// process died between the broker's ack and the bookkeeping.
type crashingStore struct {
	*outbox.Store
}

func (crashingStore) MarkPublished(context.Context, uuid.UUID, time.Time) error { return nil }

// A broker that is not there must produce a recorded, backed-off retry rather
// than a lost event or a wedged loop.
func TestIntegration_UnreachableBrokerBacksOffAndKeepsTheEvent(t *testing.T) {
	p := integrationPool(t)
	agg := newAggregate(t, p)

	ev := topicEvent(t, agg, "outbox.test.unreachable", 1)
	commitEvents(t, p, ev)

	store := outbox.NewStore(p)
	opts := testDispatcherOptions(t, "it-broken-broker/1")
	opts.PublishTimeout = 2 * time.Second
	d := outbox.NewDispatcherWithStore(store, unreachablePublisher{}, opts)

	before := scrapeMetric(t, "ae_outbox_failed_total")
	start := time.Now()
	if n, err := d.RunOnce(context.Background()); err != nil || n != 1 {
		t.Fatalf("dispatched %d (err %v), want 1", n, err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("a dead broker held the loop for %s; PublishTimeout was %s", elapsed, opts.PublishTimeout)
	}
	if delta := scrapeMetric(t, "ae_outbox_failed_total") - before; delta != 1 {
		t.Errorf("ae_outbox_failed_total moved %d, want 1", delta)
	}

	st := readRow(t, p, ev.ID)
	if st.PublishedAt != nil {
		t.Fatal("an event that never reached the broker was marked published")
	}
	if st.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", st.Attempts)
	}
	if st.LastError == "" {
		t.Error("last_error is empty; the operator has nothing to read")
	}
	if st.ClaimedBy != "" {
		t.Errorf("a failed publish must release the claim, held by %q", st.ClaimedBy)
	}
	if !st.NextAttempt.After(time.Now()) {
		t.Errorf("next_attempt_at = %s, want a backoff into the future", st.NextAttempt)
	}
}

// unreachablePublisher stands in for a bus whose broker is gone. Dialling a
// closed port from inside the test would work too, but kafka-go's retry
// schedule makes the timing unpredictable; this keeps the assertion about the
// dispatcher.
type unreachablePublisher struct{}

func (unreachablePublisher) PublishMessage(ctx context.Context, _ *bus.Message) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(30 * time.Second):
		return errors.New("unreachable")
	}
}

// ---------------------------------------------------------------------------
// Store additions (database only)
// ---------------------------------------------------------------------------

// A producer that re-enqueues a retry-stable event_id must get a no-op, not a
// unique violation that aborts the transaction carrying its state change.
func TestIntegration_EnqueueIsIdempotentOnTheEventID(t *testing.T) {
	p := integrationPool(t)
	ctx := context.Background()
	agg := newAggregate(t, p)

	first := topicEvent(t, agg, "outbox.test.idempotent", 1)
	commitEvents(t, p, first)

	// The producer's transaction is retried: the same id comes round again,
	// this time alongside a genuinely new event.
	second := topicEvent(t, agg, "outbox.test.idempotent", 2)
	tx, err := p.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := outbox.EnqueueAll(ctx, tx, first, second); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("re-enqueueing a known id must be a no-op, got: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("the producer's transaction was aborted by the duplicate: %v", err)
	}

	if n := countRows(t, p, agg); n != 2 {
		t.Errorf("rows = %d, want 2 (the duplicate must not have been inserted twice)", n)
	}

	// The stored event is the original, not the second attempt's copy.
	// (Compare the decoded field: jsonb re-renders its own whitespace.)
	var payload []byte
	if err := p.QueryRow(ctx, `SELECT payload FROM outbox_events WHERE id = $1`, first.ID).Scan(&payload); err != nil {
		t.Fatalf("read payload: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(payload, &fields); err != nil {
		t.Fatalf("decode payload %s: %v", payload, err)
	}
	if seq, _ := fields["seq"].(float64); seq != 1 {
		t.Errorf("payload = %s, want the first insert's (seq 1)", payload)
	}
	if got, _ := fields[outbox.FieldEventID].(string); got != first.ID.String() {
		t.Errorf("payload event_id = %q, want %s", got, first.ID)
	}
}

// Publishing must not erase the evidence that an event struggled first.
func TestIntegration_MarkPublishedPreservesTheFailureHistory(t *testing.T) {
	p := integrationPool(t)
	ctx := context.Background()
	agg := newAggregate(t, p)

	ev := topicEvent(t, agg, "outbox.test.history", 1)
	commitEvents(t, p, ev)

	s := outbox.NewStore(p)
	if ids := claimIDs(t, s, "d1", 50, time.Now(), agg); len(ids) != 1 {
		t.Fatalf("claimed %d rows, want 1", len(ids))
	}
	if _, err := s.MarkFailedBy(ctx, ev.ID, "d1", errors.New("broker refused the batch"), time.Now()); err != nil {
		t.Fatalf("MarkFailedBy: %v", err)
	}
	if err := s.MarkPublished(ctx, ev.ID, time.Now()); err != nil {
		t.Fatalf("MarkPublished: %v", err)
	}

	st := readRow(t, p, ev.ID)
	if st.PublishedAt == nil {
		t.Fatal("the row was not marked published")
	}
	if st.Attempts != 1 {
		t.Errorf("attempts = %d, want 1 — publishing must not reset the attempt count", st.Attempts)
	}
	if !strings.Contains(st.LastError, "broker refused the batch") {
		t.Errorf("last_error = %q, want the earlier failure preserved", st.LastError)
	}
	if st.DeadAt != nil {
		t.Error("a published row must not stay dead")
	}
	if st.ClaimedBy != "" || st.ClaimedAt != nil {
		t.Errorf("publishing must release the claim: %+v", st)
	}
}

// A dispatcher whose claim expired while it was publishing must not be able to
// burn an attempt on, or unclaim, the row its successor now owns.
func TestIntegration_MarkFailedByIsFencedOnTheClaim(t *testing.T) {
	p := integrationPool(t)
	ctx := context.Background()
	agg := newAggregate(t, p)

	ev := topicEvent(t, agg, "outbox.test.fence", 1)
	commitEvents(t, p, ev)

	s := outbox.NewStore(p)
	if ids := claimIDs(t, s, "d1", 50, time.Now(), agg); len(ids) != 1 {
		t.Fatalf("claimed %d rows, want 1", len(ids))
	}

	// The stale dispatcher tries to record its failure.
	dead, err := s.MarkFailedBy(ctx, ev.ID, "d2", errors.New("stale failure"), time.Now())
	if !errors.Is(err, outbox.ErrClaimLost) {
		t.Fatalf("MarkFailedBy from a non-owner = (%v, %v), want ErrClaimLost", dead, err)
	}
	st := readRow(t, p, ev.ID)
	if st.Attempts != 0 {
		t.Errorf("attempts = %d; a non-owner must not burn an attempt", st.Attempts)
	}
	if st.ClaimedBy != "d1" {
		t.Errorf("claimed_by = %q; a non-owner must not clear the owner's claim", st.ClaimedBy)
	}
	if st.LastError != "" {
		t.Errorf("last_error = %q; a non-owner must not overwrite it", st.LastError)
	}

	// The owner can.
	if _, err := s.MarkFailedBy(ctx, ev.ID, "d1", errors.New("real failure"), time.Now()); err != nil {
		t.Fatalf("MarkFailedBy from the owner: %v", err)
	}
	st = readRow(t, p, ev.ID)
	if st.Attempts != 1 || !strings.Contains(st.LastError, "real failure") {
		t.Errorf("the owner's failure was not recorded: %+v", st)
	}
	if st.ClaimedBy != "" {
		t.Errorf("the owner's failure must release the claim, held by %q", st.ClaimedBy)
	}

	// And now that the claim is released, even the former owner is fenced
	// out: the row is free for whoever claims it next.
	if _, err := s.MarkFailedBy(ctx, ev.ID, "d1", errors.New("second stale failure"), time.Now()); !errors.Is(err, outbox.ErrClaimLost) {
		t.Errorf("MarkFailedBy on an unclaimed row = %v, want ErrClaimLost", err)
	}
	if got := readRow(t, p, ev.ID).Attempts; got != 1 {
		t.Errorf("attempts = %d, want 1", got)
	}

	// The unfenced operator form still works, which is what it is for.
	if _, err := s.MarkFailed(ctx, ev.ID, errors.New("operator override"), time.Now()); err != nil {
		t.Fatalf("MarkFailed (operator): %v", err)
	}
	if got := readRow(t, p, ev.ID).Attempts; got != 2 {
		t.Errorf("attempts = %d after the operator override, want 2", got)
	}
}
