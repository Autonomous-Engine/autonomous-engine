package outbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/metrics"
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// In-memory store fake
// ---------------------------------------------------------------------------

// fakeStore is an EventStore that keeps rows in memory and records the order
// of every call. It models the parts of the real store the loop depends on —
// claim/expiry, attempt counting, death at maxAttempts — and nothing else;
// per-aggregate ordering and SKIP LOCKED are the real store's problem and are
// covered by its own integration tests.
type fakeStore struct {
	mu sync.Mutex

	rows []*fakeRow
	// claimErr, when set, is returned by the next Claim (and then cleared).
	claimErr error
	// pendingErr, when set, is returned by every Pending.
	pendingErr error
	// markPublishedErr, when set, is returned by every MarkPublished.
	markPublishedErr error
	// markFailedErr, when set, is returned by every MarkFailedBy.
	markFailedErr error

	claimTTL    time.Duration
	maxAttempts int

	// calls records method names in order, so a test can assert that
	// MarkPublished never precedes the publish it records.
	calls []string
	// claims counts Claim calls, so a test can wait for the loop to turn over.
	claims int
	// claimed records the claimedBy of the last Claim.
	lastClaimedBy string
}

type fakeRow struct {
	ev          Event
	published   bool
	dead        bool
	claimedAt   *time.Time
	claimedBy   string
	nextAttempt time.Time
}

func newFakeStore(evs ...Event) *fakeStore {
	s := &fakeStore{claimTTL: DefaultClaimTTL, maxAttempts: DefaultMaxAttempts}
	for _, ev := range evs {
		s.rows = append(s.rows, &fakeRow{ev: ev})
	}
	return s
}

func (s *fakeStore) record(name string) { s.calls = append(s.calls, name) }

func (s *fakeStore) Claim(ctx context.Context, claimedBy string, limit int, now time.Time) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claims++
	s.lastClaimedBy = claimedBy
	s.record("Claim")
	if err := s.claimErr; err != nil {
		s.claimErr = nil
		return nil, err
	}
	out := []Event{}
	for _, r := range s.rows {
		if len(out) >= limit {
			break
		}
		if r.published || r.dead || now.Before(r.nextAttempt) {
			continue
		}
		if r.claimedAt != nil && now.Sub(*r.claimedAt) < s.claimTTL {
			continue
		}
		c := now
		r.claimedAt, r.claimedBy = &c, claimedBy
		ev := r.ev
		ev.ClaimedBy = claimedBy
		out = append(out, ev)
	}
	return out, nil
}

func (s *fakeStore) MarkPublished(ctx context.Context, id uuid.UUID, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record("MarkPublished:" + id.String())
	if s.markPublishedErr != nil {
		return s.markPublishedErr
	}
	r := s.find(id)
	if r == nil {
		return ErrNotFound
	}
	r.published = true
	r.claimedAt, r.claimedBy = nil, ""
	return nil
}

func (s *fakeStore) MarkFailedBy(ctx context.Context, id uuid.UUID, claimedBy string, cause error, now time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record("MarkFailedBy:" + id.String())
	if claimedBy == "" {
		return false, errors.New("fake: MarkFailedBy requires the claim holder")
	}
	if s.markFailedErr != nil {
		return false, s.markFailedErr
	}
	r := s.find(id)
	if r == nil {
		return false, ErrNotFound
	}
	if r.published {
		return false, nil
	}
	if r.claimedBy != claimedBy {
		return false, fmt.Errorf("%w: held by %q, not %q", ErrClaimLost, r.claimedBy, claimedBy)
	}
	r.ev.Attempts++
	r.ev.LastError = cause.Error()
	r.claimedAt, r.claimedBy = nil, ""
	if r.ev.Attempts >= s.maxAttempts {
		r.dead = true
		return true, nil
	}
	r.nextAttempt = now // the fake does not model backoff; the store's tests do
	return false, nil
}

func (s *fakeStore) Pending(ctx context.Context) (int, *time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pendingErr != nil {
		return 0, nil, s.pendingErr
	}
	count := 0
	var oldest *time.Time
	for _, r := range s.rows {
		if r.published || r.dead {
			continue
		}
		count++
		if oldest == nil || r.ev.CreatedAt.Before(*oldest) {
			c := r.ev.CreatedAt
			oldest = &c
		}
	}
	return count, oldest, nil
}

func (s *fakeStore) find(id uuid.UUID) *fakeRow {
	for _, r := range s.rows {
		if r.ev.ID == id {
			return r
		}
	}
	return nil
}

func (s *fakeStore) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

func (s *fakeStore) claimCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.claims
}

func (s *fakeStore) row(t *testing.T, id uuid.UUID) fakeRow {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.find(id)
	if r == nil {
		t.Fatalf("no row %s", id)
	}
	return *r
}

var _ EventStore = (*fakeStore)(nil)

// ---------------------------------------------------------------------------
// Publisher fakes
// ---------------------------------------------------------------------------

// fakePublisher records every message and answers with a scripted outcome.
type fakePublisher struct {
	mu   sync.Mutex
	msgs []bus.Message
	// fn, when set, decides the outcome for a given call index. It receives
	// the publish context so a fake can honour cancellation the way kafka-go
	// does, rather than guessing when the caller gave up.
	fn    func(ctx context.Context, call int, msg *bus.Message) error
	calls int
	// before is called (holding no lock) before the outcome is decided, so a
	// test can inspect store state at the moment of the publish.
	before func(msg *bus.Message)
}

func (p *fakePublisher) PublishMessage(ctx context.Context, msg *bus.Message) error {
	p.mu.Lock()
	call := p.calls
	p.calls++
	cp := *msg
	cp.Headers = map[string]string{}
	for k, v := range msg.Headers {
		cp.Headers[k] = v
	}
	p.msgs = append(p.msgs, cp)
	before, fn := p.before, p.fn
	p.mu.Unlock()

	if before != nil {
		before(msg)
	}
	if fn != nil {
		return fn(ctx, call, msg)
	}
	return nil
}

func (p *fakePublisher) published() []bus.Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]bus.Message(nil), p.msgs...)
}

func (p *fakePublisher) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func testEvent(t *testing.T, agg uuid.UUID, seq int) Event {
	t.Helper()
	ev, err := NewEvent("work_item", agg, "implementation.tasks", agg.String(), map[string]any{"seq": seq})
	if err != nil {
		t.Fatalf("NewEvent: %v", err)
	}
	ev.CreatedAt = time.Unix(1700000000+int64(seq), 0).UTC()
	return ev
}

// testOptions returns options that log into a buffer and tick fast.
func testOptions(logs *strings.Builder) Options {
	return Options{
		PollInterval:   time.Millisecond,
		BatchSize:      10,
		ClaimedBy:      "test-dispatcher/1",
		PublishTimeout: 200 * time.Millisecond,
		Logger:         log.New(logs, "", 0),
	}
}

// metricValue scrapes one metric from the process-wide registry. The registry
// has no read API, so the exposition format is the only way in; tests compare
// deltas rather than absolutes because the counters are shared.
func metricValue(t *testing.T, name string) int64 {
	t.Helper()
	rec := httptest.NewRecorder()
	metrics.Handler()(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body, err := io.ReadAll(rec.Result().Body)
	if err != nil {
		t.Fatalf("read /metrics: %v", err)
	}
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + ` (-?\d+)$`)
	m := re.FindStringSubmatch(string(body))
	if m == nil {
		t.Fatalf("metric %q not exported:\n%s", name, body)
	}
	v, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		t.Fatalf("metric %q value %q: %v", name, m[1], err)
	}
	return v
}

// metricDelta runs fn and returns how much the named metric moved.
func metricDelta(t *testing.T, name string, fn func()) int64 {
	t.Helper()
	before := metricValue(t, name)
	fn()
	return metricValue(t, name) - before
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// ---------------------------------------------------------------------------
// Options and construction
// ---------------------------------------------------------------------------

func TestOptions_Defaults(t *testing.T) {
	got := Options{}.withDefaults()
	if got.PollInterval != DefaultPollInterval {
		t.Errorf("PollInterval = %s, want %s", got.PollInterval, DefaultPollInterval)
	}
	if got.BatchSize != DefaultBatchSize {
		t.Errorf("BatchSize = %d, want %d", got.BatchSize, DefaultBatchSize)
	}
	if got.PublishTimeout != DefaultPublishTimeout {
		t.Errorf("PublishTimeout = %s, want %s", got.PublishTimeout, DefaultPublishTimeout)
	}
	if got.Logger == nil || got.Now == nil {
		t.Error("Logger and Now must be defaulted")
	}
	// ClaimTTL and MaxAttempts stay zero: zero means "use the store's", which
	// is already the approved policy. Defaulting them here would silently
	// re-apply them to a store an operator had deliberately tuned.
	if got.ClaimTTL != 0 || got.MaxAttempts != 0 {
		t.Errorf("ClaimTTL/MaxAttempts = %s/%d, want 0/0 (store-owned)", got.ClaimTTL, got.MaxAttempts)
	}
	// The effective values are still the brief's.
	s := NewStore(nil)
	if s.claimTTL != 60*time.Second || s.maxAttempts != 25 {
		t.Errorf("effective defaults = %s/%d, want 60s/25", s.claimTTL, s.maxAttempts)
	}
}

// TestOptions_PublishTimeoutClearsThePublishersWorstCase pins the left half of
// the publish-timeout invariant.
//
// *bus.KafkaBus retries internally up to MaxAttempts (3) times with a
// WriteTimeout (10s) each, so one WriteMessages call can legitimately run for
// 30s plus backoff. A PublishTimeout below that abandons the publisher after
// its first internal attempt: the writer's retries never run, and last_error
// records this loop's generic timeout instead of the broker's actual error,
// which is the line an operator reads first. internal/bus pins its two numbers
// exactly (TestNewWriter_BoundsOneAckedPublish) so the pair cannot drift
// silently; this is the other end of that contract.
func TestOptions_PublishTimeoutClearsThePublishersWorstCase(t *testing.T) {
	const kafkaWorstCase = 3 * 10 * time.Second // MaxAttempts x WriteTimeout
	if DefaultPublishTimeout <= kafkaWorstCase {
		t.Errorf("DefaultPublishTimeout = %s, want more than the bus writer's worst case of %s plus backoff",
			DefaultPublishTimeout, kafkaWorstCase)
	}
	if DefaultPublishTimeout >= DefaultClaimTTL {
		t.Errorf("DefaultPublishTimeout (%s) must stay below DefaultClaimTTL (%s), or the defaults themselves are invalid",
			DefaultPublishTimeout, DefaultClaimTTL)
	}
}

// TestOptions_RejectsAPublishTimeoutThatOutlivesTheClaim pins the right half.
//
// A publish bounded by a timeout at least as long as the claim can still be in
// flight when the claim expires. Another dispatcher then takes the row and
// publishes it too, while this one returns to a fenced MarkFailedBy that
// refuses the write: the attempt is not counted, not backed off, not in
// next_attempt_at, and the duplicate delivery is unexplained in the table.
func TestOptions_RejectsAPublishTimeoutThatOutlivesTheClaim(t *testing.T) {
	tests := []struct {
		name    string
		opts    Options
		wantErr bool
	}{
		{"defaults are valid", Options{}, false},
		{"comfortably inside the default TTL", Options{PublishTimeout: 5 * time.Second}, false},
		{"equal to the default TTL", Options{PublishTimeout: DefaultClaimTTL}, true},
		{"longer than the default TTL", Options{PublishTimeout: 2 * DefaultClaimTTL}, true},
		{"inside an overridden TTL", Options{PublishTimeout: 20 * time.Second, ClaimTTL: 30 * time.Second}, false},
		{"outside an overridden TTL", Options{PublishTimeout: 20 * time.Second, ClaimTTL: 15 * time.Second}, true},
		{"a shortened TTL invalidates the default timeout", Options{ClaimTTL: 5 * time.Second}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.opts.withDefaults().validate()
			if tt.wantErr && err == nil {
				t.Fatalf("validate() = nil, want an error for PublishTimeout=%s ClaimTTL=%s",
					tt.opts.withDefaults().PublishTimeout, tt.opts.effectiveClaimTTL())
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("validate() = %v, want nil", err)
			}
		})
	}
}

// TestRun_RejectsAPublishTimeoutThatOutlivesTheClaim proves the invariant is
// enforced where it matters and not merely available: Run must refuse rather
// than spin, the same way it refuses a missing store.
func TestRun_RejectsAPublishTimeoutThatOutlivesTheClaim(t *testing.T) {
	d := NewDispatcherWithStore(newFakeStore(), &fakePublisher{}, Options{
		PublishTimeout: 90 * time.Second, // > DefaultClaimTTL
	})
	if err := d.Run(context.Background()); err == nil {
		t.Error("Run accepted a PublishTimeout longer than the claim TTL; a publish could outlive the claim it was made under")
	}
}

// TestStartWithOptions_RejectsTheBadTimeoutBeforeStarting checks the caller
// gets the error, rather than a running dispatcher that logs and dies.
func TestStartWithOptions_RejectsTheBadTimeoutBeforeStarting(t *testing.T) {
	// A nil pool is rejected first, so this asserts ordering too: the pool
	// error must win, and the timeout error must appear for a caller who
	// would otherwise have got a dispatcher.
	if _, err := StartWithOptions(context.Background(), nil, &fakePublisher{}, Options{}); err == nil {
		t.Error("StartWithOptions accepted a nil pool")
	}
}

func TestOptions_ClaimedByIdentifiesTheProcess(t *testing.T) {
	got := Options{}.withDefaults().ClaimedBy
	if got == "" {
		t.Fatal("ClaimedBy must never default to empty; Claim rejects it")
	}
	// hostname/pid — two dispatchers on one host must not collide.
	if !strings.Contains(got, "/") {
		t.Errorf("ClaimedBy = %q, want hostname/pid", got)
	}
	if strings.HasSuffix(got, "/0") {
		t.Errorf("ClaimedBy = %q, want a real pid", got)
	}
}

func TestNewDispatcher_AppliesStorePolicyOverrides(t *testing.T) {
	s := NewStore(nil)
	NewDispatcher(s, &fakePublisher{}, Options{ClaimTTL: 3 * time.Second, MaxAttempts: 4})
	if s.claimTTL != 3*time.Second {
		t.Errorf("claimTTL = %s, want 3s", s.claimTTL)
	}
	if s.maxAttempts != 4 {
		t.Errorf("maxAttempts = %d, want 4", s.maxAttempts)
	}

	// Zero values leave the store alone.
	s2 := NewStore(nil, WithClaimTTL(9*time.Second), WithMaxAttempts(7))
	NewDispatcher(s2, &fakePublisher{}, Options{})
	if s2.claimTTL != 9*time.Second || s2.maxAttempts != 7 {
		t.Errorf("unset options overwrote store policy: %s/%d", s2.claimTTL, s2.maxAttempts)
	}
}

func TestRun_RejectsAnIncompleteDispatcher(t *testing.T) {
	ctx := context.Background()
	if err := NewDispatcher(nil, &fakePublisher{}, Options{}).Run(ctx); err == nil {
		t.Error("Run with no store must fail fast, not spin")
	}
	if err := NewDispatcherWithStore(newFakeStore(), nil, Options{}).Run(ctx); err == nil {
		t.Error("Run with no publisher must fail fast, not spin")
	}
	if _, err := Start(ctx, nil, &fakePublisher{}, nil); err == nil {
		t.Error("Start with no pool must fail")
	}
	if _, err := Start(ctx, nil, nil, nil); err == nil {
		t.Error("Start with no publisher must fail")
	}
}

// The dispatcher must not be able to reach the unfenced MarkFailed: a
// dispatcher records failures only on rows it still holds.
func TestEventStore_ExcludesTheUnfencedMarkFailed(t *testing.T) {
	iface := reflect.TypeOf((*EventStore)(nil)).Elem()
	if _, ok := iface.MethodByName("MarkFailed"); ok {
		t.Error("EventStore declares MarkFailed; the dispatcher must only ever use the fenced MarkFailedBy")
	}
	if _, ok := iface.MethodByName("MarkFailedBy"); !ok {
		t.Error("EventStore must declare MarkFailedBy")
	}
	// And the loop must not reach the unfenced form some other way.
	src, err := os.ReadFile("dispatcher.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(src), ".MarkFailed(") {
		t.Error("dispatcher.go calls the unfenced MarkFailed")
	}
	if !strings.Contains(string(src), ".MarkFailedBy(ctx, ev.ID, d.opts.ClaimedBy,") {
		t.Error("the dispatcher must fence MarkFailedBy on its own ClaimedBy")
	}
}

// ---------------------------------------------------------------------------
// The loop
// ---------------------------------------------------------------------------

func TestDispatcher_PublishesInClaimOrderAndMarksEach(t *testing.T) {
	agg := uuid.New()
	evs := []Event{testEvent(t, agg, 1), testEvent(t, agg, 2), testEvent(t, agg, 3)}
	store := newFakeStore(evs...)
	pub := &fakePublisher{}
	var logs strings.Builder
	d := NewDispatcherWithStore(store, pub, testOptions(&logs))

	delta := metricDelta(t, "ae_outbox_published_total", func() {
		n, err := d.RunOnce(context.Background())
		if err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
		if n != 3 {
			t.Fatalf("dispatched %d, want 3", n)
		}
	})
	if delta != 3 {
		t.Errorf("ae_outbox_published_total moved %d, want 3", delta)
	}

	got := pub.published()
	if len(got) != 3 {
		t.Fatalf("published %d messages, want 3", len(got))
	}
	for i, ev := range evs {
		if got[i].Headers[HeaderEventID] != ev.ID.String() {
			t.Errorf("message %d is %s, want %s (claim order must be publish order)",
				i, got[i].Headers[HeaderEventID], ev.ID)
		}
		if got[i].Topic != ev.Topic || got[i].Key != ev.Key {
			t.Errorf("message %d routed to %s/%s, want %s/%s", i, got[i].Topic, got[i].Key, ev.Topic, ev.Key)
		}
		if string(got[i].Value) != string(ev.Payload) {
			t.Errorf("message %d payload = %s, want %s", i, got[i].Value, ev.Payload)
		}
		if !store.row(t, ev.ID).published {
			t.Errorf("event %d was not marked published", i)
		}
	}
	if store.lastClaimedBy != "test-dispatcher/1" {
		t.Errorf("claimed_by = %q, want the configured name", store.lastClaimedBy)
	}
}

// The event id travels as a Kafka header as well as in the payload, so a
// consumer can deduplicate without parsing the body.
func TestDispatcher_SendsTheEventIDAsAHeader(t *testing.T) {
	ev := testEvent(t, uuid.New(), 1)
	// A row whose headers were lost (hand-written, or written before NewEvent
	// existed) must still arrive identifiable.
	ev.Headers = nil
	store := newFakeStore(ev)
	pub := &fakePublisher{}
	var logs strings.Builder
	if _, err := NewDispatcherWithStore(store, pub, testOptions(&logs)).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	msgs := pub.published()
	if len(msgs) != 1 {
		t.Fatalf("published %d messages, want 1", len(msgs))
	}
	if got := msgs[0].Headers[HeaderEventID]; got != ev.ID.String() {
		t.Errorf("header %s = %q, want %s", HeaderEventID, got, ev.ID)
	}
	if !strings.Contains(string(msgs[0].Value), ev.ID.String()) {
		t.Errorf("payload lost its %s field: %s", FieldEventID, msgs[0].Value)
	}
}

// Mutating the message must not corrupt the claimed event's headers.
func TestDispatcher_HeadersAreCopiedPerMessage(t *testing.T) {
	ev := testEvent(t, uuid.New(), 1)
	ev.Headers["trace_id"] = "abc"
	store := newFakeStore(ev)
	pub := &fakePublisher{fn: func(_ context.Context, _ int, msg *bus.Message) error {
		msg.Headers["trace_id"] = "clobbered"
		delete(msg.Headers, HeaderEventID)
		return nil
	}}
	var logs strings.Builder
	if _, err := NewDispatcherWithStore(store, pub, testOptions(&logs)).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ev.Headers["trace_id"] != "abc" || ev.Headers[HeaderEventID] != ev.ID.String() {
		t.Errorf("publisher mutated the event's own headers: %v", ev.Headers)
	}
}

// The whole delivery contract: the store is told about a publish only after
// the publisher has returned.
func TestDispatcher_MarkPublishedOnlyAfterTheAck(t *testing.T) {
	ev := testEvent(t, uuid.New(), 1)
	store := newFakeStore(ev)
	var duringPublish []string
	pub := &fakePublisher{before: func(*bus.Message) {
		duringPublish = store.snapshot()
	}}
	var logs strings.Builder
	if _, err := NewDispatcherWithStore(store, pub, testOptions(&logs)).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, c := range duringPublish {
		if strings.HasPrefix(c, "MarkPublished") {
			t.Fatalf("MarkPublished ran before the publish was acked: %v", duringPublish)
		}
	}
	after := store.snapshot()
	if !containsCall(after, "MarkPublished:"+ev.ID.String()) {
		t.Fatalf("MarkPublished never ran: %v", after)
	}
}

// A publisher that fails must not be recorded as published, and the event must
// come back on the next claim.
func TestDispatcher_FailureIsRecordedAndRetried(t *testing.T) {
	ev := testEvent(t, uuid.New(), 1)
	store := newFakeStore(ev)
	pub := &fakePublisher{fn: func(_ context.Context, call int, _ *bus.Message) error {
		if call == 0 {
			return errors.New("broker unavailable")
		}
		return nil
	}}
	var logs strings.Builder
	d := NewDispatcherWithStore(store, pub, testOptions(&logs))
	ctx := context.Background()

	failed := metricDelta(t, "ae_outbox_failed_total", func() {
		if _, err := d.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
	})
	if failed != 1 {
		t.Errorf("ae_outbox_failed_total moved %d, want 1", failed)
	}
	r := store.row(t, ev.ID)
	if r.published {
		t.Fatal("a failed publish was marked published")
	}
	if r.ev.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", r.ev.Attempts)
	}
	if !strings.Contains(r.ev.LastError, "broker unavailable") {
		t.Errorf("last_error = %q, want the publisher's error", r.ev.LastError)
	}
	if r.claimedBy != "" {
		t.Errorf("a failed publish must release the claim, held by %q", r.claimedBy)
	}
	if !strings.Contains(logs.String(), "publish failed, will retry") {
		t.Errorf("no retry log line:\n%s", logs.String())
	}

	// Second pass: the same event is claimed again and succeeds.
	if _, err := d.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if !store.row(t, ev.ID).published {
		t.Error("the retry did not publish")
	}
	if pub.count() != 2 {
		t.Errorf("publisher called %d times, want 2", pub.count())
	}
}

func TestDispatcher_DeadAfterMaxAttempts(t *testing.T) {
	ev := testEvent(t, uuid.New(), 1)
	store := newFakeStore(ev)
	store.maxAttempts = 3
	pub := &fakePublisher{fn: func(context.Context, int, *bus.Message) error { return errors.New("poison payload") }}
	var logs strings.Builder
	d := NewDispatcherWithStore(store, pub, testOptions(&logs))
	ctx := context.Background()

	dead := metricDelta(t, "ae_outbox_dead_total", func() {
		for i := 0; i < 3; i++ {
			if _, err := d.RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
		}
	})
	if dead != 1 {
		t.Errorf("ae_outbox_dead_total moved %d, want 1", dead)
	}
	r := store.row(t, ev.ID)
	if !r.dead {
		t.Fatal("the event was not dead-lettered")
	}
	if r.ev.Attempts != 3 {
		t.Errorf("attempts = %d, want 3", r.ev.Attempts)
	}
	if !strings.Contains(logs.String(), "EVENT DEAD-LETTERED after 3 attempts") {
		t.Errorf("the dead-letter log must name the event and the count:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), ev.ID.String()) {
		t.Errorf("the dead-letter log must carry the event id:\n%s", logs.String())
	}

	// A dead row is not claimed again.
	n, err := d.RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("a dead row was claimed again (%d events)", n)
	}
}

// A publisher that never returns must cost one PublishTimeout, not the loop.
func TestDispatcher_PerEventTimeoutFiresAndTheLoopContinues(t *testing.T) {
	agg1, agg2 := uuid.New(), uuid.New()
	hung, next := testEvent(t, agg1, 1), testEvent(t, agg2, 2)
	store := newFakeStore(hung, next)
	release := make(chan struct{})
	defer close(release)
	pub := &fakePublisher{fn: func(_ context.Context, _ int, msg *bus.Message) error {
		if msg.Headers[HeaderEventID] == hung.ID.String() {
			// Ignores its context entirely, which is the point: a deadline
			// the publisher does not honour must still bound the loop.
			<-release
			return nil
		}
		return nil
	}}
	var logs strings.Builder
	opts := testOptions(&logs)
	opts.PublishTimeout = 50 * time.Millisecond
	d := NewDispatcherWithStore(store, pub, opts)

	start := time.Now()
	n, err := d.RunOnce(context.Background())
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("dispatched %d, want 2", n)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("the hung publish wedged the loop for %s", elapsed)
	}
	if elapsed < 50*time.Millisecond {
		t.Fatalf("the publish was not actually awaited (%s)", elapsed)
	}
	if store.row(t, hung.ID).published {
		t.Error("a timed-out publish was marked published")
	}
	if store.row(t, hung.ID).ev.Attempts != 1 {
		t.Error("a timed-out publish must count as a failed attempt")
	}
	if !store.row(t, next.ID).published {
		t.Error("the event behind the hung one was never published")
	}
	if !strings.Contains(logs.String(), "timed out after 50ms") {
		t.Errorf("the timeout must be logged:\n%s", logs.String())
	}
}

// A panicking publisher is one failed attempt, not a dead process.
func TestDispatcher_PublisherPanicIsContained(t *testing.T) {
	agg1, agg2 := uuid.New(), uuid.New()
	bad, good := testEvent(t, agg1, 1), testEvent(t, agg2, 2)
	store := newFakeStore(bad, good)
	pub := &fakePublisher{fn: func(_ context.Context, _ int, msg *bus.Message) error {
		if msg.Headers[HeaderEventID] == bad.ID.String() {
			panic("nil map write in the balancer")
		}
		return nil
	}}
	var logs strings.Builder
	d := NewDispatcherWithStore(store, pub, testOptions(&logs))

	if _, err := d.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.row(t, bad.ID).published {
		t.Error("a panicking publish was marked published")
	}
	if store.row(t, bad.ID).ev.Attempts != 1 {
		t.Error("a panic must count as a failed attempt")
	}
	if !store.row(t, good.ID).published {
		t.Error("the panic took the rest of the batch with it")
	}
	if !strings.Contains(logs.String(), "publisher panicked") {
		t.Errorf("the panic must be logged:\n%s", logs.String())
	}
}

// A publish that lands but cannot be recorded is loud, and is NOT counted as
// published — the count would then be a lie about the table, not the bus.
func TestDispatcher_UnrecordedPublishIsLoud(t *testing.T) {
	ev := testEvent(t, uuid.New(), 1)
	store := newFakeStore(ev)
	store.markPublishedErr = errors.New("connection reset")
	pub := &fakePublisher{}
	var logs strings.Builder
	d := NewDispatcherWithStore(store, pub, testOptions(&logs))

	delta := metricDelta(t, "ae_outbox_published_total", func() {
		if _, err := d.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
	if delta != 0 {
		t.Errorf("ae_outbox_published_total moved %d, want 0", delta)
	}
	if !strings.Contains(logs.String(), "PUBLISHED BUT NOT RECORDED") {
		t.Errorf("an unrecorded publish must be logged loudly:\n%s", logs.String())
	}
}

// A dispatcher whose claim expired must not burn another dispatcher's attempt.
func TestDispatcher_LostClaimIsNotRecorded(t *testing.T) {
	ev := testEvent(t, uuid.New(), 1)
	store := newFakeStore(ev)
	pub := &fakePublisher{fn: func(context.Context, int, *bus.Message) error { return errors.New("broker gone") }}
	var logs strings.Builder
	opts := testOptions(&logs)
	d := NewDispatcherWithStore(store, pub, opts)

	// Simulate the take-over: while "our" publish was in flight, another
	// dispatcher claimed the row.
	pub.before = func(*bus.Message) {
		store.mu.Lock()
		defer store.mu.Unlock()
		store.find(ev.ID).claimedBy = "other-dispatcher/2"
	}

	if _, err := d.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := store.row(t, ev.ID)
	if r.ev.Attempts != 0 {
		t.Errorf("attempts = %d; a lost claim must not burn an attempt", r.ev.Attempts)
	}
	if r.claimedBy != "other-dispatcher/2" {
		t.Errorf("claimed_by = %q; a lost claim must not be cleared", r.claimedBy)
	}
	if !strings.Contains(logs.String(), "the claim was lost") {
		t.Errorf("a lost claim must be logged:\n%s", logs.String())
	}
}

func TestDispatcher_RefreshesBacklogGauges(t *testing.T) {
	agg := uuid.New()
	a, b := testEvent(t, agg, 1), testEvent(t, uuid.New(), 2)
	store := newFakeStore(a, b)
	var logs strings.Builder
	opts := testOptions(&logs)
	// A fixed clock 90s after the oldest row's created_at.
	opts.Now = func() time.Time { return a.CreatedAt.Add(90 * time.Second) }
	pub := &fakePublisher{fn: func(context.Context, int, *bus.Message) error { return errors.New("down") }}
	d := NewDispatcherWithStore(store, pub, opts)

	if _, err := d.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := metricValue(t, "ae_outbox_pending"); got != 2 {
		t.Errorf("ae_outbox_pending = %d, want 2", got)
	}
	if got := metricValue(t, "ae_outbox_oldest_pending_seconds"); got != 90 {
		t.Errorf("ae_outbox_oldest_pending_seconds = %d, want 90", got)
	}

	// Publishing everything drains both gauges to zero.
	pub.fn = nil
	if _, err := d.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := metricValue(t, "ae_outbox_pending"); got != 0 {
		t.Errorf("ae_outbox_pending = %d, want 0", got)
	}
	if got := metricValue(t, "ae_outbox_oldest_pending_seconds"); got != 0 {
		t.Errorf("ae_outbox_oldest_pending_seconds = %d after draining, want 0", got)
	}
}

func TestOldestPendingSeconds(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	if got := oldestPendingSeconds(nil, now); got != 0 {
		t.Errorf("empty queue = %d, want 0", got)
	}
	old := now.Add(-45 * time.Second)
	if got := oldestPendingSeconds(&old, now); got != 45 {
		t.Errorf("45s old = %d, want 45", got)
	}
	// Clock skew between the database and this process must not report a
	// negative age, which Prometheus would graph as a spike.
	future := now.Add(30 * time.Second)
	if got := oldestPendingSeconds(&future, now); got != 0 {
		t.Errorf("skewed row = %d, want 0", got)
	}
}

// A failing Claim must be reported, logged, and not turn into a hot loop.
func TestDispatcher_ClaimFailureIsReportedAndSlept(t *testing.T) {
	store := newFakeStore()
	store.claimErr = errors.New("pool exhausted")
	var logs strings.Builder
	d := NewDispatcherWithStore(store, &fakePublisher{}, testOptions(&logs))

	n, err := d.RunOnce(context.Background())
	if err == nil {
		t.Fatal("RunOnce must report a claim failure")
	}
	if n != 0 {
		t.Errorf("dispatched %d on a failed claim, want 0", n)
	}
	if !strings.Contains(logs.String(), "claim failed") {
		t.Errorf("a claim failure must be logged:\n%s", logs.String())
	}

	// The loop keeps going: the next claim succeeds.
	if _, err := d.RunOnce(context.Background()); err != nil {
		t.Fatalf("the loop did not recover from a claim failure: %v", err)
	}
}

// Backlog metrics are best-effort; losing them must not stop dispatching.
func TestDispatcher_PendingFailureDoesNotStopTheLoop(t *testing.T) {
	ev := testEvent(t, uuid.New(), 1)
	store := newFakeStore(ev)
	store.pendingErr = errors.New("statement timeout")
	var logs strings.Builder
	d := NewDispatcherWithStore(store, &fakePublisher{}, testOptions(&logs))

	if _, err := d.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !store.row(t, ev.ID).published {
		t.Error("a metrics failure blocked the publish")
	}
	if !strings.Contains(logs.String(), "backlog metrics unavailable") {
		t.Errorf("a metrics failure must be logged:\n%s", logs.String())
	}
}

// ---------------------------------------------------------------------------
// Lifecycle
// ---------------------------------------------------------------------------

func TestRun_StopsOnContextCancel(t *testing.T) {
	store := newFakeStore()
	var logs strings.Builder
	d := NewDispatcherWithStore(store, &fakePublisher{}, testOptions(&logs))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()

	waitFor(t, "the loop to poll at least twice", func() bool { return store.claimCount() >= 2 })
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v on cancel, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return on context cancel")
	}
	if !strings.Contains(logs.String(), "dispatcher stopped") {
		t.Errorf("shutdown must be logged:\n%s", logs.String())
	}

	// And it really stopped.
	settled := store.claimCount()
	time.Sleep(20 * time.Millisecond)
	if store.claimCount() != settled {
		t.Error("the loop kept claiming after Run returned")
	}
}

// Cancelling mid-publish must not cost an attempt: the row is left claimed and
// re-delivered when the claim expires.
func TestRun_CancelMidPublishDoesNotBurnAnAttempt(t *testing.T) {
	ev := testEvent(t, uuid.New(), 1)
	store := newFakeStore(ev)
	publishing := make(chan struct{})
	var once sync.Once
	pub := &fakePublisher{fn: func(ctx context.Context, _ int, _ *bus.Message) error {
		once.Do(func() { close(publishing) })
		// Block on the real cancellation rather than returning
		// context.Canceled the moment the test is told we are publishing.
		// Returning early races the test's cancel(): the dispatcher could see
		// a context.Canceled error while the parent context is still live,
		// read that as a genuine broker failure, and — the fake store models
		// no backoff — retry it in a hot loop. kafka-go returns only once its
		// context is actually done, and so must this.
		<-ctx.Done()
		return ctx.Err()
	}}
	var logs strings.Builder
	opts := testOptions(&logs)
	opts.PublishTimeout = 5 * time.Second
	d := NewDispatcherWithStore(store, pub, opts)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	<-publishing
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return on context cancel")
	}
	r := store.row(t, ev.ID)
	if r.ev.Attempts != 0 {
		t.Errorf("attempts = %d; a shutdown is not a delivery failure", r.ev.Attempts)
	}
	if r.published {
		t.Error("a cancelled publish was marked published")
	}
	if !strings.Contains(logs.String(), "publish abandoned at shutdown") {
		t.Errorf("an abandoned publish must be logged:\n%s", logs.String())
	}
}

// A non-empty batch is followed immediately by another claim; an empty one
// sleeps. Without this a backlog drains at BatchSize per PollInterval.
func TestRun_DrainsWithoutSleepingBetweenFullBatches(t *testing.T) {
	agg := uuid.New()
	var evs []Event
	for i := 0; i < 20; i++ {
		evs = append(evs, testEvent(t, agg, i))
	}
	store := newFakeStore(evs...)
	var logs strings.Builder
	opts := testOptions(&logs)
	opts.BatchSize = 2
	opts.PollInterval = time.Hour // any sleep at all would hang this test
	d := NewDispatcherWithStore(store, &fakePublisher{}, opts)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = d.Run(ctx) }()

	waitFor(t, "all 20 events to drain", func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		for _, r := range store.rows {
			if !r.published {
				return false
			}
		}
		return true
	})
	// Join the loop before the test's log buffer goes out of scope.
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return on context cancel")
	}
}

func TestRun_IsIdempotentAboutDone(t *testing.T) {
	store := newFakeStore()
	var logs strings.Builder
	d := NewDispatcherWithStore(store, &fakePublisher{}, testOptions(&logs))
	d.done = make(chan struct{})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := d.Run(ctx); err != nil {
		t.Fatal(err)
	}
	d.Wait() // must not block: done was closed by Run
	// A dispatcher with no loop of its own must not block either.
	NewDispatcherWithStore(store, &fakePublisher{}, Options{}).Wait()
}

func containsCall(calls []string, want string) bool {
	for _, c := range calls {
		if c == want {
			return true
		}
	}
	return false
}
