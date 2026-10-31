package outbox

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"runtime/debug"
	"sync"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Dispatcher defaults (M1-02). ClaimTTL and MaxAttempts are delivery policy
// and live on the Store (DefaultClaimTTL, DefaultMaxAttempts); the values here
// are loop policy only.
const (
	// DefaultPollInterval is how long the loop sleeps after an empty claim.
	DefaultPollInterval = 1 * time.Second
	// DefaultBatchSize is how many rows one claim asks for. At most one row
	// per aggregate is eligible at a time, so a batch of 50 means "up to 50
	// different aggregates", not 50 events of one work item.
	DefaultBatchSize = 50
	// DefaultPublishTimeout bounds a single publish. It exists because a
	// broker that accepts the connection and then stalls would otherwise
	// block the loop, and with it every other aggregate's events, forever.
	//
	// It must be longer than the publisher's own worst case, or the publisher
	// never gets to finish: with *bus.KafkaBus the writer retries internally
	// up to MaxAttempts (3) times with a WriteTimeout (10s) each, so one
	// WriteMessages call can legitimately run for 30s plus kafka-go's
	// backoff. At the old 10s this loop abandoned the goroutine after the
	// writer's FIRST internal attempt, which meant the writer's retries were
	// dead code from the outbox's point of view and last_error recorded this
	// loop's generic "timed out after 10s" instead of the broker's actual
	// complaint — exactly the diagnostic an operator needs. 40s sits above
	// 3 x 10s with room for backoff.
	//
	// It must also stay below the claim TTL (see Options.validate): a publish
	// that outlives its claim is a publish whose outcome the dispatcher is no
	// longer allowed to record.
	DefaultPublishTimeout = 40 * time.Second

	// bookkeepingTimeout bounds MarkPublished / MarkFailed. These run on a
	// context detached from the caller's (see bookkeepingContext), so they
	// need a deadline of their own or a shutdown could hang on a wedged
	// database.
	bookkeepingTimeout = 10 * time.Second
	// maxLoggedStack caps the goroutine dump logged when a publisher panics.
	maxLoggedStack = 4096
)

// Publisher is the bus dependency of the dispatcher, satisfied by
// *bus.KafkaBus.
//
// It is PublishMessage rather than the simpler Publish(ctx, topic, key,
// payload) because *bus.KafkaBus.PublishMessage forwards Message.Headers to
// kafka.Message.Headers, and the outbox needs that: every event carries its id
// in the Kafka header HeaderEventID ("event_id") as well as in the payload, so
// a consumer can deduplicate (M1-05) without parsing — or trusting — the body,
// and so a non-JSON-object payload, which has nowhere to carry FieldEventID,
// is still identifiable. Publish() would silently drop the header.
type Publisher interface {
	PublishMessage(ctx context.Context, msg *bus.Message) error
}

// EventStore is the dispatcher's view of the outbox table, satisfied by
// *Store. It exists so the loop can be tested against an in-memory fake and so
// the crash-injection tests can wrap a real Store; production code should pass
// a *Store to NewDispatcher.
//
// It names MarkFailedBy, not MarkFailed: the dispatcher may only record a
// failure on a row it still holds. The unfenced MarkFailed is left off this
// interface on purpose, so no future change to the loop can reach it by
// accident.
type EventStore interface {
	Claim(ctx context.Context, claimedBy string, limit int, now time.Time) ([]Event, error)
	MarkPublished(ctx context.Context, id uuid.UUID, now time.Time) error
	MarkFailedBy(ctx context.Context, id uuid.UUID, claimedBy string, cause error, now time.Time) (bool, error)
	Pending(ctx context.Context) (int, *time.Time, error)
}

// compile-time proof that the real store satisfies the dispatcher's view.
var _ EventStore = (*Store)(nil)

// Options configures the dispatch loop. The zero value is valid: every field
// falls back to its default.
type Options struct {
	// PollInterval is how long to sleep after a claim came back empty.
	// A non-empty batch is followed immediately by another claim, so a
	// backlog drains at publish speed rather than BatchSize per interval.
	// Default DefaultPollInterval.
	PollInterval time.Duration
	// BatchSize is the claim limit. Default DefaultBatchSize.
	BatchSize int
	// ClaimedBy identifies this dispatcher in outbox_events.claimed_by, so an
	// expired claim can be attributed to the instance that dropped it.
	// Default hostname/pid.
	ClaimedBy string
	// ClaimTTL, when positive, overrides the Store's claim TTL
	// (DefaultClaimTTL). Delivery policy lives on the Store; this is a
	// convenience for Start, which builds the Store itself. Applied to the
	// Store by NewDispatcher, so set it before the Store is shared.
	ClaimTTL time.Duration
	// MaxAttempts, when positive, overrides the Store's dead-letter threshold
	// (DefaultMaxAttempts). Same caveat as ClaimTTL.
	MaxAttempts int
	// PublishTimeout bounds one publish. Default DefaultPublishTimeout.
	PublishTimeout time.Duration
	// Logger receives the failure, dead-letter and lifecycle lines. Default
	// log.Default().
	Logger *log.Logger
	// Now is the clock, for tests. Default time.Now.
	Now func() time.Time
}

// withDefaults returns opts with the loop defaults filled in. ClaimTTL and
// MaxAttempts are deliberately left at zero when unset: zero means "whatever
// the Store was built with", which is already DefaultClaimTTL /
// DefaultMaxAttempts.
func (o Options) withDefaults() Options {
	if o.PollInterval <= 0 {
		o.PollInterval = DefaultPollInterval
	}
	if o.BatchSize <= 0 {
		o.BatchSize = DefaultBatchSize
	}
	if o.PublishTimeout <= 0 {
		o.PublishTimeout = DefaultPublishTimeout
	}
	if o.ClaimedBy == "" {
		o.ClaimedBy = defaultClaimedBy()
	}
	if o.Logger == nil {
		o.Logger = log.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return o
}

// effectiveClaimTTL is the claim TTL these options imply: the override when
// one is set, otherwise the Store default.
//
// A Store built directly with WithClaimTTL and handed to
// NewDispatcherWithStore is invisible here — EventStore does not expose the
// TTL — so validate() checks the TTL a caller *configured through Options*.
// That covers Start/StartWithOptions, which build the Store themselves, and
// the ordinary NewDispatcher path.
func (o Options) effectiveClaimTTL() time.Duration {
	if o.ClaimTTL > 0 {
		return o.ClaimTTL
	}
	return DefaultClaimTTL
}

// validate rejects option combinations that can never behave correctly.
//
// The one invariant worth enforcing is PublishTimeout < ClaimTTL. A publish
// bounded by a timeout at least as long as the claim can still be in flight
// when the claim expires, at which point another dispatcher may take the row
// and publish it too, while this one comes back to a fenced MarkFailedBy that
// refuses the write (ErrClaimLost). The attempt is then invisible: not
// counted on the row, not backed off, not reflected in next_attempt_at — and
// the duplicate delivery it caused is unexplained in the table. Better to
// refuse the configuration than to produce a queue whose bookkeeping silently
// disagrees with what reached the bus.
func (o Options) validate() error {
	if ttl := o.effectiveClaimTTL(); o.PublishTimeout >= ttl {
		return fmt.Errorf("outbox: PublishTimeout (%s) must be shorter than the claim TTL (%s), or a publish can outlive the claim it was made under and its outcome cannot be recorded", o.PublishTimeout, ttl)
	}
	return nil
}

// defaultClaimedBy names this process. Hostname alone is not enough: two
// services on one host, or two replicas in one pod, would collide and each
// would treat the other's fresh claim as its own.
func defaultClaimedBy() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown-host"
	}
	return fmt.Sprintf("%s/%d", host, os.Getpid())
}

// Dispatcher publishes committed outbox rows to the bus. It owns no delivery
// policy of its own: ordering, claim expiry, backoff and death all live in the
// Store, and the dispatcher only decides when to ask and what to do with the
// answer.
type Dispatcher struct {
	store     EventStore
	publisher Publisher
	opts      Options

	// done is closed when a loop started by Start exits; nil otherwise.
	done     chan struct{}
	doneOnce sync.Once
}

// NewDispatcher builds a dispatcher over a real Store.
//
// opts.ClaimTTL and opts.MaxAttempts, when positive, are applied to the store
// here, so a caller configures delivery policy in one place. Do this before
// the store is used by anything else.
func NewDispatcher(store *Store, publisher Publisher, opts Options) *Dispatcher {
	if store != nil {
		WithClaimTTL(opts.ClaimTTL)(store)
		WithMaxAttempts(opts.MaxAttempts)(store)
		return NewDispatcherWithStore(store, publisher, opts)
	}
	// A nil *Store must stay a nil EventStore, not a non-nil interface
	// wrapping a nil pointer, or Run's guard would not catch it.
	return NewDispatcherWithStore(nil, publisher, opts)
}

// NewDispatcherWithStore builds a dispatcher over any EventStore. Tests use it
// with an in-memory fake or with a wrapper around a real Store; production
// code uses NewDispatcher.
func NewDispatcherWithStore(store EventStore, publisher Publisher, opts Options) *Dispatcher {
	return &Dispatcher{store: store, publisher: publisher, opts: opts.withDefaults()}
}

// Name returns the value this dispatcher writes to claimed_by.
func (d *Dispatcher) Name() string { return d.opts.ClaimedBy }

// Run drives the loop until ctx is cancelled, and returns nil when it is. It
// returns an error only for a misconfiguration that can never succeed (no
// store, no publisher, a PublishTimeout that outlives the claim); every
// runtime failure is retried rather than returned, because a dispatcher that
// exits on a broker blip would strand the queue.
//
// One iteration: claim a batch, publish each event in claim order, mark it
// published or failed, refresh the backlog gauges, then either claim again
// (the batch had work) or sleep PollInterval (it did not).
func (d *Dispatcher) Run(ctx context.Context) error {
	defer d.markDone()
	if d.store == nil {
		return errors.New("outbox: dispatcher needs a store")
	}
	if d.publisher == nil {
		return errors.New("outbox: dispatcher needs a publisher")
	}
	if err := d.opts.validate(); err != nil {
		return err
	}

	d.opts.Logger.Printf("outbox: dispatcher started claimed_by=%q batch_size=%d poll_interval=%s publish_timeout=%s",
		d.opts.ClaimedBy, d.opts.BatchSize, d.opts.PollInterval, d.opts.PublishTimeout)
	defer d.opts.Logger.Printf("outbox: dispatcher stopped claimed_by=%q", d.opts.ClaimedBy)

	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}

		dispatched, err := d.RunOnce(ctx)
		if ctx.Err() != nil {
			return nil
		}

		// Sleep on an empty batch and on a claim failure; go straight round
		// again while there is work, so a backlog is not metered out at
		// BatchSize per PollInterval.
		wait := d.opts.PollInterval
		if err == nil && dispatched > 0 {
			wait = 0
		}
		timer.Reset(wait)
	}
}

// RunOnce claims one batch, dispatches it, refreshes the backlog gauges, and
// returns how many events it handled. It is what Run calls; it is exported so
// a service can drain the queue on demand (right after a commit, or in a
// shutdown hook) and so tests can step the loop deterministically.
//
// A claim failure is returned; a publish failure is not, because it is
// recorded on the row and retried.
func (d *Dispatcher) RunOnce(ctx context.Context) (int, error) {
	events, err := d.store.Claim(ctx, d.opts.ClaimedBy, d.opts.BatchSize, d.opts.Now())
	if err != nil {
		if ctx.Err() == nil {
			d.opts.Logger.Printf("outbox: claim failed claimed_by=%q batch_size=%d error=%q",
				d.opts.ClaimedBy, d.opts.BatchSize, err)
		}
		return 0, err
	}

	dispatched := 0
	for i := range events {
		if ctx.Err() != nil {
			// Shutting down. The rows we did not reach stay claimed until the
			// TTL expires and are then re-delivered; that is the
			// at-least-once contract, and it is better than publishing
			// during a teardown.
			break
		}
		d.dispatch(ctx, events[i])
		dispatched++
	}

	d.refreshBacklog(ctx)
	return dispatched, nil
}

// dispatch publishes one event and records the outcome.
//
// The order here is the whole delivery contract: the bus is asked first, and
// the row is marked published only after the publish returned without error.
// Marking first would turn a broker failure into silent data loss; marking
// after means a crash in between re-delivers the event, which is why every
// event carries a stable id.
//
// How strong "published" is depends entirely on the Publisher, and the outbox
// cannot be more reliable than the ack it waits for. *bus.KafkaBus now sets
// RequiredAcks: kafka.RequireAll, so PublishMessage returns only once the
// in-sync replicas have written the record and a broker-side failure surfaces
// as an error. (It previously left the field at kafka-go's zero value,
// RequireNone, under which the broker sends no response at all and a publish
// "succeeded" as soon as the request hit the socket — rows would have been
// retired for messages the cluster never stored. M1-02 flagged it; it was
// fixed in internal/bus before M1-03 wired the first producer.)
//
// Any other Publisher must offer the same guarantee. One that acknowledges
// before the record is durable turns this loop's at-least-once delivery into
// silent loss, and nothing here can detect that.
func (d *Dispatcher) dispatch(ctx context.Context, ev Event) {
	err := d.publish(ctx, ev)

	// Bookkeeping runs on a detached context: if the process is shutting down
	// we still must record a publish that already reached the broker, or the
	// event is re-delivered for no reason.
	bctx, cancel := d.bookkeepingContext()
	defer cancel()

	if err != nil {
		if ctx.Err() != nil && errors.Is(err, context.Canceled) {
			// The publish was cut short by shutdown, not by the broker. Do
			// not burn an attempt: leave the claim to expire.
			d.opts.Logger.Printf("outbox: publish abandoned at shutdown event_id=%s topic=%s aggregate_type=%s aggregate_id=%s",
				ev.ID, ev.Topic, ev.AggregateType, ev.AggregateID)
			return
		}
		d.recordFailure(bctx, ev, err)
		return
	}

	if err := d.store.MarkPublished(bctx, ev.ID, d.opts.Now()); err != nil {
		// The event IS on the bus; only the record of it is missing. It will
		// be claimed again once the claim expires and published a second
		// time, with the same event_id — consumers deduplicate.
		d.opts.Logger.Printf("outbox: PUBLISHED BUT NOT RECORDED — will be re-delivered event_id=%s topic=%s aggregate_type=%s aggregate_id=%s error=%q",
			ev.ID, ev.Topic, ev.AggregateType, ev.AggregateID, err)
		return
	}
	MetricPublished.Inc()
}

// recordFailure writes the failed attempt to the row and logs it, loudly when
// the event is now dead.
func (d *Dispatcher) recordFailure(ctx context.Context, ev Event, cause error) {
	MetricFailed.Inc()
	dead, err := d.store.MarkFailedBy(ctx, ev.ID, d.opts.ClaimedBy, cause, d.opts.Now())
	if errors.Is(err, ErrClaimLost) {
		// This attempt took longer than the claim TTL and another dispatcher
		// owns the row now. Recording the failure would burn an attempt that
		// is not ours and clear their claim mid-flight, so we only say so.
		d.opts.Logger.Printf("outbox: publish failed but the claim was lost — not recorded event_id=%s topic=%s aggregate_type=%s aggregate_id=%s error=%q claim_error=%q",
			ev.ID, ev.Topic, ev.AggregateType, ev.AggregateID, cause, err)
		return
	}
	if err != nil {
		d.opts.Logger.Printf("outbox: publish failed AND could not be recorded event_id=%s topic=%s aggregate_type=%s aggregate_id=%s attempts=%d error=%q record_error=%q",
			ev.ID, ev.Topic, ev.AggregateType, ev.AggregateID, ev.Attempts, cause, err)
		return
	}
	if dead {
		MetricDead.Inc()
		// A dead row stops blocking later events for its aggregate, so the
		// aggregate carries on with a hole in it. This line is the operator's
		// entry point into the runbook.
		d.opts.Logger.Printf("outbox: EVENT DEAD-LETTERED after %d attempts — downstream state for this aggregate is incomplete until it is requeued event_id=%s topic=%s aggregate_type=%s aggregate_id=%s error=%q",
			ev.Attempts+1, ev.ID, ev.Topic, ev.AggregateType, ev.AggregateID, cause)
		return
	}
	d.opts.Logger.Printf("outbox: publish failed, will retry event_id=%s topic=%s aggregate_type=%s aggregate_id=%s attempt=%d error=%q",
		ev.ID, ev.Topic, ev.AggregateType, ev.AggregateID, ev.Attempts+1, cause)
}

// publish sends one event to the bus, bounded by PublishTimeout and isolated
// from a misbehaving publisher.
//
// The call runs on its own goroutine and the result is selected against the
// deadline. A context deadline alone would not be enough: a publisher that
// ignores its context — a blocked write to a broker socket, a mutex held by
// another stuck publish, a library bug — would otherwise hold the loop, and
// with it every other aggregate, indefinitely. When the deadline wins, the
// goroutine is abandoned rather than waited for; it holds nothing the loop
// needs (the buffered channel lets it finish and exit whenever it does), and
// the alternative is the wedge this is here to prevent.
//
// A panic in the publisher is turned into an ordinary failed attempt for the
// same reason: one poison event must not take the process down.
func (d *Dispatcher) publish(ctx context.Context, ev Event) error {
	msg := messageFor(ev)

	pctx, cancel := context.WithTimeout(ctx, d.opts.PublishTimeout)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- fmt.Errorf("publisher panicked: %v\n%s", r, truncateStack(debug.Stack()))
			}
		}()
		done <- d.publisher.PublishMessage(pctx, msg)
	}()

	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("publish to %s: %w", ev.Topic, err)
		}
		return nil
	case <-pctx.Done():
		if ctx.Err() != nil {
			// Parent cancelled: shutdown, not a broker fault.
			return fmt.Errorf("publish to %s abandoned: %w", ev.Topic, ctx.Err())
		}
		return fmt.Errorf("publish to %s timed out after %s: %w", ev.Topic, d.opts.PublishTimeout, pctx.Err())
	}
}

// messageFor renders an event as a bus message. Headers are copied rather than
// aliased so a publisher that mutates them cannot corrupt the claimed event,
// and HeaderEventID is (re)asserted: a row written before NewEvent existed, or
// by hand, must still arrive identifiable.
func messageFor(ev Event) *bus.Message {
	headers := make(map[string]string, len(ev.Headers)+1)
	for k, v := range ev.Headers {
		headers[k] = v
	}
	headers[HeaderEventID] = ev.ID.String()
	return &bus.Message{
		Topic:   ev.Topic,
		Key:     ev.Key,
		Value:   []byte(ev.Payload),
		Headers: headers,
	}
}

// refreshBacklog updates the two gauges from Store.Pending, once per poll.
func (d *Dispatcher) refreshBacklog(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	count, oldest, err := d.store.Pending(ctx)
	if err != nil {
		if ctx.Err() == nil {
			d.opts.Logger.Printf("outbox: backlog metrics unavailable claimed_by=%q error=%q", d.opts.ClaimedBy, err)
		}
		return
	}
	MetricPending.Set(int64(count))
	MetricOldestPendingSeconds.Set(oldestPendingSeconds(oldest, d.opts.Now()))
}

// oldestPendingSeconds is the age of the oldest live row, floored at zero: an
// empty queue, and a row whose created_at is in this process's future (clock
// skew against the database), both report 0 rather than a negative age.
func oldestPendingSeconds(oldest *time.Time, now time.Time) int64 {
	if oldest == nil {
		return 0
	}
	age := now.Sub(*oldest)
	if age < 0 {
		return 0
	}
	return int64(age.Seconds())
}

// bookkeepingContext returns a context for MarkPublished / MarkFailed that
// outlives the caller's cancellation but not the process.
func (d *Dispatcher) bookkeepingContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), bookkeepingTimeout)
}

// truncateStack bounds a panic dump so one poison publisher cannot flood the
// log.
func truncateStack(stack []byte) []byte {
	if len(stack) <= maxLoggedStack {
		return stack
	}
	return stack[:maxLoggedStack]
}

// Wait blocks until a loop started by Start has exited. It returns
// immediately for a dispatcher built with NewDispatcher, which has no loop of
// its own to wait for.
func (d *Dispatcher) Wait() {
	if d.done == nil {
		return
	}
	<-d.done
}

func (d *Dispatcher) markDone() {
	if d.done == nil {
		return
	}
	d.doneOnce.Do(func() { close(d.done) })
}

// Start is the one-line wiring a service main needs: it builds a Store over
// pool, a Dispatcher over publisher, and runs the loop in the background until
// ctx is cancelled. Call Wait on the returned dispatcher during shutdown to
// let an in-flight publish finish being recorded.
//
//	disp, err := outbox.Start(ctx, pool, kafkaBus, nil)
//
// M1-03 wired the first producer: cmd/implementation-planner.
func Start(ctx context.Context, pool *pgxpool.Pool, publisher Publisher, logger *log.Logger) (*Dispatcher, error) {
	return StartWithOptions(ctx, pool, publisher, Options{Logger: logger})
}

// StartWithOptions is Start with the loop tuned.
//
// Unlike Run, which discovers a bad configuration only once the goroutine is
// running, this validates up front and returns the error to the caller — a
// service main can then refuse to start rather than log a line nobody reads.
func StartWithOptions(ctx context.Context, pool *pgxpool.Pool, publisher Publisher, opts Options) (*Dispatcher, error) {
	if pool == nil {
		return nil, errors.New("outbox: Start requires a database pool")
	}
	if publisher == nil {
		return nil, errors.New("outbox: Start requires a publisher")
	}
	if err := opts.withDefaults().validate(); err != nil {
		return nil, err
	}
	d := NewDispatcher(NewStore(pool), publisher, opts)
	d.done = make(chan struct{})
	go func() {
		if err := d.Run(ctx); err != nil {
			// Only a misconfiguration reaches here; Run retries everything
			// else. markDone has already run in Run's defer.
			d.opts.Logger.Printf("outbox: dispatcher did not start: %v", err)
		}
	}()
	return d, nil
}
