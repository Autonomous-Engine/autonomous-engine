// Package sharedstate holds the optional Redis client and the helpers built
// on it: the WebSocket relay used by the dashboard fan-out and the fallback
// tracker the shared rate limiters use when Redis is unreachable.
//
// Redis is optional. REDIS_URL empty means every caller keeps its
// in-process behaviour. Only short-lived state that is safe to lose on a
// Redis restart belongs here; durable timers and single-flight flags live in
// Postgres.
package sharedstate

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/config"
	"github.com/autonomousengine/autonomous-engine/internal/metrics"
	"github.com/redis/go-redis/v9"
)

// Open returns a client for rawURL, or nil (and no error) when rawURL is
// empty. It does not contact Redis; the first command does.
func Open(rawURL string) (*redis.Client, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil, nil
	}
	opts, err := redis.ParseURL(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse REDIS_URL: %w", err)
	}
	// Short timeouts: every caller has a local fallback, so a slow Redis
	// must cost little before the fallback takes over.
	opts.DialTimeout = 2 * time.Second
	opts.ReadTimeout = time.Second
	opts.WriteTimeout = time.Second
	// go-redis ignores context deadlines on socket I/O unless this is set;
	// callers pass short deadlines (500 ms) that must actually cut a stalled
	// Redis off.
	opts.ContextTimeoutEnabled = true
	return redis.NewClient(opts), nil
}

var (
	envOnce   sync.Once
	envClient *redis.Client
)

// FromEnv returns the process-wide client for REDIS_URL, or nil when it is
// unset or malformed (a malformed URL is logged once and treated as unset,
// so callers keep their in-process behaviour; see config.RedisConfig for why
// dashboard-backend and portal use Open and fail instead).
func FromEnv() *redis.Client {
	envOnce.Do(func() {
		c, err := Open(config.RedisURL())
		if err != nil {
			log.Printf("sharedstate: %v; Redis disabled", err)
			return
		}
		envClient = c
	})
	return envClient
}

// ── Fallback tracking ───────────────────────────────────────────────────────

var (
	fallbackMu       sync.Mutex
	fallbackCounters = map[string]*metrics.Counter{}
)

// fallbackCounter returns the ae_redis_fallback_total series for component,
// registering it once.
func fallbackCounter(component string) *metrics.Counter {
	fallbackMu.Lock()
	defer fallbackMu.Unlock()
	if c, ok := fallbackCounters[component]; ok {
		return c
	}
	c := metrics.NewLabeledCounter(
		"ae_redis_fallback_total",
		"Operations that used the in-process fallback because Redis was unreachable",
		"component", component,
	)
	fallbackCounters[component] = c
	return c
}

// Fallback tracks whether a component should use Redis or its in-process
// fallback. After a failure Redis is skipped for the cooldown so an outage
// does not add a timeout to every call; the first call after the cooldown
// tries Redis again. Transitions are logged once each way; every operation
// served by the fallback increments ae_redis_fallback_total{component}.
type Fallback struct {
	component string
	cooldown  time.Duration
	counter   *metrics.Counter

	mu    sync.Mutex
	down  bool
	until time.Time
}

// NewFallback returns a tracker for component. cooldown <= 0 means 5s.
func NewFallback(component string, cooldown time.Duration) *Fallback {
	if cooldown <= 0 {
		cooldown = 5 * time.Second
	}
	return &Fallback{component: component, cooldown: cooldown, counter: fallbackCounter(component)}
}

var (
	processMu        sync.Mutex
	processFallbacks = map[string]*Fallback{}
)

// ProcessFallback returns the one tracker for component in this process,
// creating it (5 s cooldown) on first use. Use it where the owning object is
// short-lived (an LLM router per Kafka message): the cooldown and the
// "logged once" transition then hold for the whole process.
func ProcessFallback(component string) *Fallback {
	processMu.Lock()
	defer processMu.Unlock()
	if f, ok := processFallbacks[component]; ok {
		return f
	}
	f := NewFallback(component, 0)
	processFallbacks[component] = f
	return f
}

// Available reports whether the caller should try Redis now.
func (f *Fallback) Available() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.down || !time.Now().Before(f.until)
}

// Fail records a Redis error for an operation the caller is about to serve
// from its fallback.
func (f *Fallback) Fail(err error) {
	f.counter.Inc()
	f.mu.Lock()
	wasDown := f.down
	f.down = true
	f.until = time.Now().Add(f.cooldown)
	f.mu.Unlock()
	if !wasDown {
		log.Printf("sharedstate: %s: Redis unreachable (%v); using the in-process fallback", f.component, err)
	}
}

// Skipped records an operation served from the fallback without trying Redis
// (during the cooldown).
func (f *Fallback) Skipped() { f.counter.Inc() }

// Recover records a successful Redis operation.
func (f *Fallback) Recover() {
	f.mu.Lock()
	wasDown := f.down
	f.down = false
	f.mu.Unlock()
	if wasDown {
		log.Printf("sharedstate: %s: Redis reachable again", f.component)
	}
}

// Total returns the component's fallback count (for tests and diagnostics).
func (f *Fallback) Total() int64 { return f.counter.Value() }

// ── WebSocket relay ─────────────────────────────────────────────────────────

// WSChannelPrefix prefixes the Redis channel of each dashboard WebSocket
// channel: events for "pipeline" travel on "ae:ws:pipeline".
const WSChannelPrefix = "ae:ws:"

// Relay carries dashboard WebSocket events between replicas over Redis
// pub/sub. Pub/sub is at-most-once: a replica that is disconnected from Redis
// misses the events published meanwhile, which matches the dashboard's
// existing semantics (a client that is not connected misses live events and
// reloads state over REST).
type Relay struct {
	c *redis.Client
}

// NewRelay returns a relay over c.
func NewRelay(c *redis.Client) *Relay { return &Relay{c: c} }

// Publish sends payload to every replica subscribed to channel and returns
// how many subscriptions received it (0 when no replica, not even the
// publisher, is subscribed at that moment).
func (r *Relay) Publish(ctx context.Context, channel string, payload []byte) (int64, error) {
	return r.c.Publish(ctx, WSChannelPrefix+channel, payload).Result()
}

// Subscribe subscribes to every WebSocket channel and calls deliver for each
// event, with the WebSocket channel name (prefix removed). It returns once
// Redis has confirmed the subscription, so events published afterwards are
// not missed; delivery runs on a goroutine until ctx is cancelled.
func (r *Relay) Subscribe(ctx context.Context, deliver func(channel string, payload []byte)) error {
	ps := r.c.PSubscribe(ctx, WSChannelPrefix+"*")
	confirmCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := ps.Receive(confirmCtx); err != nil {
		_ = ps.Close()
		return fmt.Errorf("subscribe %s*: %w", WSChannelPrefix, err)
	}
	ch := ps.Channel()
	go func() {
		defer ps.Close() //nolint:errcheck
		for {
			select {
			case <-ctx.Done():
				return
			case m, ok := <-ch:
				if !ok {
					return
				}
				deliver(strings.TrimPrefix(m.Channel, WSChannelPrefix), []byte(m.Payload))
			}
		}
	}()
	return nil
}
