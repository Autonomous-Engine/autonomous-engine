package ratelimit

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newShared(t *testing.T, mr *miniredis.Miniredis, max int, window time.Duration) *IPRateLimiter {
	t.Helper()
	c := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = c.Close() })
	return New(max, window).UseRedis(c, "portal")
}

// Two portal replicas share one budget per IP: three requests are allowed in
// total, whichever replica serves them.
func TestRedisLimiterIsSharedAcrossInstances(t *testing.T) {
	mr := miniredis.RunT(t)
	a := newShared(t, mr, 3, time.Minute)
	b := newShared(t, mr, 3, time.Minute)

	got := []bool{a.Allow("1.2.3.4"), b.Allow("1.2.3.4"), a.Allow("1.2.3.4"), b.Allow("1.2.3.4"), a.Allow("1.2.3.4")}
	want := []bool{true, true, true, false, false}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("request %d: allowed=%v, want %v (all: %v)", i+1, got[i], want[i], got)
		}
	}
	if !b.Allow("5.6.7.8") {
		t.Fatal("another IP must have its own budget")
	}
}

func TestRedisLimiterWindowExpires(t *testing.T) {
	mr := miniredis.RunT(t)
	a := newShared(t, mr, 1, time.Minute)
	b := newShared(t, mr, 1, time.Minute)
	if !a.Allow("ip") {
		t.Fatal("first request denied")
	}
	if b.Allow("ip") {
		t.Fatal("second request in the window allowed")
	}
	mr.FastForward(time.Minute + time.Second)
	if !b.Allow("ip") {
		t.Fatal("request after the window denied")
	}
}

// With Redis down the limiter keeps limiting, from its own in-process
// counters, and counts the fallback.
func TestRedisLimiterFallsBackToLocalWhenRedisIsDown(t *testing.T) {
	mr := miniredis.RunT(t)
	a := newShared(t, mr, 2, time.Minute)
	mr.Close()

	before := a.shared.fallback.Total()
	got := []bool{a.Allow("ip"), a.Allow("ip"), a.Allow("ip")}
	want := []bool{true, true, false}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("request %d: allowed=%v, want %v", i+1, got[i], want[i])
		}
	}
	if d := a.shared.fallback.Total() - before; d != 3 {
		t.Fatalf("fallback counter delta = %d, want 3", d)
	}
}

// Without UseRedis the limiter is the in-process one, unchanged.
func TestLocalLimiterUnchanged(t *testing.T) {
	l := New(2, time.Minute)
	if l.shared != nil {
		t.Fatal("New must not enable Redis")
	}
	if !l.Allow("ip") || !l.Allow("ip") || l.Allow("ip") {
		t.Fatal("local limiter: want allow, allow, deny")
	}
}

// TestRedisLimiterLiveRedis repeats the shared-budget check against a real
// Redis when TEST_REDIS_URL is set (e.g. redis://localhost:56379/0).
func TestRedisLimiterLiveRedis(t *testing.T) {
	url := os.Getenv("TEST_REDIS_URL")
	if url == "" {
		t.Skip("TEST_REDIS_URL not set")
	}
	opts, err := redis.ParseURL(url)
	if err != nil {
		t.Fatal(err)
	}
	ca, cb := redis.NewClient(opts), redis.NewClient(opts)
	defer ca.Close()
	defer cb.Close()
	a := New(2, time.Minute).UseRedis(ca, "live-test")
	b := New(2, time.Minute).UseRedis(cb, "live-test")
	ip := "live-" + time.Now().Format("150405.000000000")
	if !a.Allow(ip) || !b.Allow(ip) || a.Allow(ip) || b.Allow(ip) {
		t.Fatal("want allow, allow, deny, deny across two instances")
	}
	if ttl := ca.PTTL(context.Background(), "ae:rl:ip:live-test:"+ip).Val(); ttl <= 0 || ttl > time.Minute {
		t.Fatalf("window key TTL = %v, want (0, 1m]", ttl)
	}
}

// UseRedis with a nil client (REDIS_URL unset) keeps the local limiter.
func TestUseRedisNilClientKeepsLocal(t *testing.T) {
	l := New(1, time.Minute).UseRedis(nil, "portal")
	if l.shared != nil {
		t.Fatal("nil client must leave Redis disabled")
	}
}
