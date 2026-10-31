package expirable_test

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
)

// Held-out acceptance tests: the expirable LRU's eviction callback may re-enter the cache
// (Len, Get, Peek, Contains, Keys, Add, Remove) without deadlocking, for every operation that
// evicts, and for background expiry. Explicit removals still run the callback before the
// method returns.

const benchDeadline = 3 * time.Second

// benchWithin runs fn on another goroutine and fails the test if it has not returned in
// time (a deadlock would otherwise hang the whole test binary).
func benchWithin(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(benchDeadline):
		t.Fatalf("%s did not return within %v (eviction callback re-entering the cache deadlocked)", what, benchDeadline)
	}
}

type benchCache = expirable.LRU[string, int]

func benchNew(size int, ttl time.Duration, onEvict func(c *benchCache, k string, v int)) *benchCache {
	var c *benchCache
	c = expirable.NewLRU[string, int](size, func(k string, v int) { onEvict(c, k, v) }, ttl)
	return c
}

func TestBenchCallbackReentersOnAddEviction(t *testing.T) {
	var calls, seenLen, stillPresent int32
	c := benchNew(2, 0, func(c *benchCache, k string, v int) {
		atomic.AddInt32(&calls, 1)
		atomic.StoreInt32(&seenLen, int32(c.Len()))
		_, _ = c.Peek("b")
		if c.Contains(k) {
			atomic.StoreInt32(&stillPresent, 1)
		}
	})
	benchWithin(t, "Add with eviction", func() {
		c.Add("a", 1)
		c.Add("b", 2)
		if !c.Add("c", 3) {
			t.Errorf("Add(c) reported no eviction")
		}
	})
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("callback ran %d times before Add returned, want 1", got)
	}
	if got := atomic.LoadInt32(&seenLen); got != 2 {
		t.Errorf("Len() inside callback = %d, want 2", got)
	}
	if atomic.LoadInt32(&stillPresent) != 0 {
		t.Errorf("evicted key was still in the cache when the callback ran")
	}
}

func TestBenchCallbackReentersOnRemoveAndRemoveOldest(t *testing.T) {
	var calls int32
	c := benchNew(10, 0, func(c *benchCache, k string, v int) {
		atomic.AddInt32(&calls, 1)
		_, _ = c.Peek("keep")
		_ = c.Keys()
	})
	c.Add("keep", 0)
	c.Add("x", 1)
	c.Add("y", 2)
	benchWithin(t, "Remove", func() {
		if !c.Remove("x") {
			t.Errorf("Remove(x) = false")
		}
	})
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("callback ran %d times before Remove returned, want 1", got)
	}
	benchWithin(t, "RemoveOldest", func() {
		if k, _, ok := c.RemoveOldest(); !ok || k != "keep" {
			t.Errorf("RemoveOldest() = %q, %v; want \"keep\", true", k, ok)
		}
	})
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("callback ran %d times after RemoveOldest returned, want 2", got)
	}
}

func TestBenchCallbackReentersOnPurgeAndResize(t *testing.T) {
	var calls int32
	c := benchNew(10, 0, func(c *benchCache, k string, v int) {
		atomic.AddInt32(&calls, 1)
		_ = c.Len()
	})
	for i, k := range []string{"a", "b", "c", "d"} {
		c.Add(k, i)
	}
	benchWithin(t, "Resize", func() {
		if n := c.Resize(2); n != 2 {
			t.Errorf("Resize(2) evicted %d, want 2", n)
		}
	})
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("callback ran %d times before Resize returned, want 2", got)
	}
	benchWithin(t, "Purge", func() { c.Purge() })
	if got := atomic.LoadInt32(&calls); got != 4 {
		t.Fatalf("callback ran %d times after Purge returned, want 4", got)
	}
	benchWithin(t, "Len after Purge", func() {
		if n := c.Len(); n != 0 {
			t.Errorf("Len() after Purge = %d, want 0", n)
		}
	})
}

func TestBenchCallbackMayMutateCache(t *testing.T) {
	c := benchNew(10, 0, func(c *benchCache, k string, v int) {
		if k == "a" {
			c.Add("replacement", v+100)
		}
	})
	c.Add("a", 1)
	benchWithin(t, "Remove with mutating callback", func() { c.Remove("a") })
	benchWithin(t, "Get after mutating callback", func() {
		if v, ok := c.Get("replacement"); !ok || v != 101 {
			t.Errorf("Get(replacement) = %d, %v; want 101, true", v, ok)
		}
	})
}

func TestBenchCallbackReentersOnExpiry(t *testing.T) {
	expired := make(chan string, 4)
	c := benchNew(10, 30*time.Millisecond, func(c *benchCache, k string, v int) {
		_ = c.Len()
		_, _ = c.Peek(k)
		expired <- k
	})
	c.Add("short", 1)
	select {
	case k := <-expired:
		if k != "short" {
			t.Fatalf("expired key = %q, want \"short\"", k)
		}
	case <-time.After(benchDeadline):
		t.Fatalf("no expiry callback within %v (background expiry deadlocked or never ran)", benchDeadline)
	}
	benchWithin(t, "Len after expiry", func() {
		if n := c.Len(); n != 0 {
			t.Errorf("Len() after expiry = %d, want 0", n)
		}
	})
}
