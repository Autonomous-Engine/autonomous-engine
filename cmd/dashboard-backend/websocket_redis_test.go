package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/gorilla/websocket"
	"github.com/redis/go-redis/v9"
)

// capturingBus records Subscribe handlers so a test can play the role of
// Kafka delivering one event to one replica's consumer group member.
type capturingBus struct {
	mu       sync.Mutex
	handlers map[string]bus.Handler
}

func newCapturingBus() *capturingBus { return &capturingBus{handlers: map[string]bus.Handler{}} }

func (b *capturingBus) Publish(context.Context, string, string, []byte) error { return nil }
func (b *capturingBus) PublishMessage(context.Context, *bus.Message) error    { return nil }
func (b *capturingBus) SubscribeMultiple([]string, string, bus.Handler) error { return nil }
func (b *capturingBus) Close() error                                          { return nil }
func (b *capturingBus) Subscribe(topic, _ string, h bus.Handler) error {
	b.mu.Lock()
	b.handlers[topic] = h
	b.mu.Unlock()
	return nil
}

func (b *capturingBus) deliver(t *testing.T, topic string, value []byte) {
	t.Helper()
	b.mu.Lock()
	h := b.handlers[topic]
	b.mu.Unlock()
	if h == nil {
		t.Fatalf("no handler for %s", topic)
	}
	if err := h(context.Background(), &bus.Message{Topic: topic, Value: value}); err != nil {
		t.Fatalf("handler: %v", err)
	}
}

func (h *WSHub) clientCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

// replica is one dashboard-backend instance: a hub, its HTTP server and its
// Kafka consumer.
type replica struct {
	hub *WSHub
	srv *httptest.Server
	bus *capturingBus
}

func startReplica(t *testing.T, ctx context.Context, rc *redis.Client) *replica {
	t.Helper()
	hub := NewWSHub(nil)
	if rc != nil {
		if err := hub.EnableRedisFanout(ctx, rc); err != nil {
			t.Fatalf("EnableRedisFanout: %v", err)
		}
	}
	b := newCapturingBus()
	if err := hub.StartKafkaFanout(b); err != nil {
		t.Fatalf("StartKafkaFanout: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(hub.HandleConnect))
	t.Cleanup(srv.Close)
	return &replica{hub: hub, srv: srv, bus: b}
}

// wsClient reads frames on a goroutine: gorilla/websocket connections are
// unusable after a read deadline expires, so tests wait on the channel.
type wsClient struct {
	frames chan broadcastPayload
}

func (r *replica) connect(t *testing.T) *wsClient {
	t.Helper()
	before := r.hub.clientCount()
	url := "ws" + strings.TrimPrefix(r.srv.URL, "http")
	c, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	deadline := time.Now().Add(2 * time.Second)
	for r.hub.clientCount() <= before {
		if time.Now().After(deadline) {
			t.Fatal("client never registered")
		}
		time.Sleep(5 * time.Millisecond)
	}
	wc := &wsClient{frames: make(chan broadcastPayload, 16)}
	go func() {
		for {
			_, raw, err := c.ReadMessage()
			if err != nil {
				return
			}
			var p broadcastPayload
			if json.Unmarshal(raw, &p) == nil {
				wc.frames <- p
			}
		}
	}()
	return wc
}

// readAll collects every frame that arrives within first, then within quiet
// of the previous one.
func readAll(t *testing.T, c *wsClient, first, quiet time.Duration) []broadcastPayload {
	t.Helper()
	var out []broadcastPayload
	wait := first
	for {
		select {
		case p := <-c.frames:
			out = append(out, p)
			wait = quiet
		case <-time.After(wait):
			return out
		}
	}
}

func redisClient(t *testing.T, addr string) *redis.Client {
	t.Helper()
	c := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func expectOnce(t *testing.T, name string, frames []broadcastPayload, channel, data string) {
	t.Helper()
	if len(frames) != 1 {
		t.Fatalf("%s: got %d frames, want exactly 1: %+v", name, len(frames), frames)
	}
	if frames[0].Channel != channel || string(frames[0].Data) != data {
		t.Fatalf("%s: got %s %s, want %s %s", name, frames[0].Channel, frames[0].Data, channel, data)
	}
}

// Two replicas share one Redis. Kafka hands a state change to replica A's
// consumer only; every client on both replicas gets it exactly once.
func TestRedisFanoutReachesClientsOfEveryReplicaOnce(t *testing.T) {
	mr := miniredis.RunT(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	a := startReplica(t, ctx, redisClient(t, mr.Addr()))
	b := startReplica(t, ctx, redisClient(t, mr.Addr()))
	a1, a2 := a.connect(t), a.connect(t)
	b1 := b.connect(t)

	a.bus.deliver(t, bus.TopicStateChanged, []byte(`{"work_item_id":"w1"}`))

	for name, c := range map[string]*wsClient{"a1": a1, "a2": a2, "b1": b1} {
		expectOnce(t, name, readAll(t, c, 2*time.Second, 300*time.Millisecond), "pipeline", `{"work_item_id":"w1"}`)
	}
}

// A request-driven broadcast (a REST handler on one replica) also reaches
// every replica once; a replica-local broadcast (the health monitor, which
// every replica runs) stays on its own replica.
func TestRedisFanoutBroadcastIsClusterWideAndLocalStaysLocal(t *testing.T) {
	mr := miniredis.RunT(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	a := startReplica(t, ctx, redisClient(t, mr.Addr()))
	b := startReplica(t, ctx, redisClient(t, mr.Addr()))
	ca, cb := a.connect(t), b.connect(t)

	a.hub.PushPromptEvent("prompt_answered", map[string]any{"id": "p1"})
	expectOnce(t, "a", readAll(t, ca, 2*time.Second, 300*time.Millisecond), "prompts", `{"id":"p1","type":"prompt_answered"}`)
	expectOnce(t, "b", readAll(t, cb, 2*time.Second, 300*time.Millisecond), "prompts", `{"id":"p1","type":"prompt_answered"}`)

	a.hub.BroadcastLocal("service_health", map[string]any{"ok": true})
	expectOnce(t, "a local", readAll(t, ca, 2*time.Second, 300*time.Millisecond), "service_health", `{"ok":true}`)
	if got := readAll(t, cb, 300*time.Millisecond, 100*time.Millisecond); len(got) != 0 {
		t.Fatalf("replica-local broadcast reached the other replica: %+v", got)
	}
}

// Without Redis the hub delivers directly, once, as before.
func TestFanoutWithoutRedisIsDirect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := startReplica(t, ctx, nil)
	c := a.connect(t)
	a.bus.deliver(t, bus.TopicStateChanged, []byte(`{"x":1}`))
	expectOnce(t, "direct", readAll(t, c, 2*time.Second, 300*time.Millisecond), "pipeline", `{"x":1}`)
}

// When Redis goes away after start-up, the replica that consumed the event
// still delivers it to its own clients (once) and counts the fallback.
func TestRedisFanoutFallsBackToLocalWhenRedisIsDown(t *testing.T) {
	mr := miniredis.RunT(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	a := startReplica(t, ctx, redisClient(t, mr.Addr()))
	c := a.connect(t)
	mr.Close()

	before := a.hub.fallback.Total()
	a.bus.deliver(t, bus.TopicStateChanged, []byte(`{"x":2}`))
	expectOnce(t, "fallback", readAll(t, c, 3*time.Second, 300*time.Millisecond), "pipeline", `{"x":2}`)
	if a.hub.fallback.Total() <= before {
		t.Fatal("fallback not counted")
	}
}

// waitPatternSubscribers waits until miniredis reports n pattern
// subscriptions.
func waitPatternSubscribers(t *testing.T, mr *miniredis.Miniredis, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for mr.PubSubNumPat() != n {
		if time.Now().After(deadline) {
			t.Fatalf("pattern subscribers = %d, want %d", mr.PubSubNumPat(), n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// When nobody is subscribed (this replica's own subscription has not come
// back yet, e.g. just after a Redis restart) a publish reaches no one, so
// the hub delivers locally instead of losing the event everywhere.
func TestRedisFanoutDeliversLocallyWhenNoSubscriberReceived(t *testing.T) {
	mr := miniredis.RunT(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	subCtx, stopSub := context.WithCancel(ctx)

	a := startReplica(t, subCtx, redisClient(t, mr.Addr()))
	c := a.connect(t)
	waitPatternSubscribers(t, mr, 1)
	stopSub() // the subscription goes away; the relay stays enabled
	waitPatternSubscribers(t, mr, 0)

	a.bus.deliver(t, bus.TopicStateChanged, []byte(`{"x":3}`))
	expectOnce(t, "no receivers", readAll(t, c, 2*time.Second, 300*time.Millisecond), "pipeline", `{"x":3}`)
}

// After Redis restarts, the relay resubscribes and delivery between
// replicas resumes.
func TestRedisFanoutResumesAfterRedisRestart(t *testing.T) {
	mr := miniredis.RunT(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	a := startReplica(t, ctx, redisClient(t, mr.Addr()))
	b := startReplica(t, ctx, redisClient(t, mr.Addr()))
	ca, cb := a.connect(t), b.connect(t)
	waitPatternSubscribers(t, mr, 2)

	mr.Close()
	if err := mr.Restart(); err != nil {
		t.Fatalf("restart: %v", err)
	}
	waitPatternSubscribers(t, mr, 2) // both relays resubscribed
	// The fallback may still be cooling down from a failed probe; wait for
	// it so the event goes through Redis rather than the local path.
	for _, r := range []*replica{a, b} {
		deadline := time.Now().Add(6 * time.Second)
		for !r.hub.fallback.Available() {
			if time.Now().After(deadline) {
				t.Fatal("fallback never recovered")
			}
			time.Sleep(50 * time.Millisecond)
		}
	}

	a.bus.deliver(t, bus.TopicStateChanged, []byte(`{"after":"restart"}`))
	expectOnce(t, "a", readAll(t, ca, 2*time.Second, 300*time.Millisecond), "pipeline", `{"after":"restart"}`)
	expectOnce(t, "b", readAll(t, cb, 2*time.Second, 300*time.Millisecond), "pipeline", `{"after":"restart"}`)
}

// TestRedisFanoutLiveRedis repeats the two-replica check against a real
// Redis when TEST_REDIS_URL is set (e.g. redis://localhost:56379/0).
func TestRedisFanoutLiveRedis(t *testing.T) {
	url := os.Getenv("TEST_REDIS_URL")
	if url == "" {
		t.Skip("TEST_REDIS_URL not set")
	}
	opts, err := redis.ParseURL(url)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	a := startReplica(t, ctx, redis.NewClient(opts))
	b := startReplica(t, ctx, redis.NewClient(opts))
	ca, cb := a.connect(t), b.connect(t)
	b.bus.deliver(t, bus.TopicIndexingProgress, []byte(`{"phase":"live"}`))
	expectOnce(t, "a", readAll(t, ca, 2*time.Second, 300*time.Millisecond), "indexing", `{"phase":"live"}`)
	expectOnce(t, "b", readAll(t, cb, 2*time.Second, 300*time.Millisecond), "indexing", `{"phase":"live"}`)
}
