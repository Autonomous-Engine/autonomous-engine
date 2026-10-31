package sharedstate

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestOpenEmptyURLDisablesRedis(t *testing.T) {
	c, err := Open("")
	if err != nil {
		t.Fatalf("Open(\"\") error: %v", err)
	}
	if c != nil {
		t.Fatalf("Open(\"\") = %v, want nil client", c)
	}
}

func TestOpenRejectsMalformedURL(t *testing.T) {
	if _, err := Open("not a url"); err == nil {
		t.Fatal("Open(malformed) returned no error")
	}
}

func TestOpenParsesURL(t *testing.T) {
	mr := miniredis.RunT(t)
	c, err := Open("redis://" + mr.Addr() + "/0")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer c.Close()
	if err := c.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("ping: %v", err)
	}
}

// A server that accepts and never replies must be cut off at the caller's
// context deadline, not at the client's 1 s read timeout.
func TestOpenHonoursContextDeadlineOnStalledServer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close() // hold the connection open, never reply
		}
	}()

	c, err := Open("redis://" + ln.Addr().String() + "/0")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	const timeout = 150 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	start := time.Now()
	err = c.Ping(ctx).Err()
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("ping to a stalled server succeeded")
	}
	if elapsed > 2*timeout+100*time.Millisecond {
		t.Fatalf("ping returned after %v, want close to the %v context deadline", elapsed, timeout)
	}
}

func TestFallbackCoolsDownAfterFailureAndCountsIt(t *testing.T) {
	f := NewFallback("test-cooldown", 50*time.Millisecond)
	if !f.Available() {
		t.Fatal("fresh fallback should allow Redis")
	}
	before := f.Total()
	f.Fail(errors.New("boom"))
	if f.Available() {
		t.Fatal("Redis should be skipped during the cooldown")
	}
	if got := f.Total() - before; got != 1 {
		t.Fatalf("fallback counter delta = %d, want 1", got)
	}
	f.Skipped()
	if got := f.Total() - before; got != 2 {
		t.Fatalf("fallback counter delta after a skipped call = %d, want 2", got)
	}
	time.Sleep(60 * time.Millisecond)
	if !f.Available() {
		t.Fatal("Redis should be retried after the cooldown")
	}
	f.Recover()
	if !f.Available() {
		t.Fatal("recovered fallback should allow Redis")
	}
}

func TestRelayDeliversToEverySubscriberOnce(t *testing.T) {
	mr := miniredis.RunT(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type got struct{ channel, payload string }
	var mu sync.Mutex
	recv := map[string][]got{}
	deliver := func(name string) func(string, []byte) {
		return func(ch string, p []byte) {
			mu.Lock()
			recv[name] = append(recv[name], got{ch, string(p)})
			mu.Unlock()
		}
	}

	a := NewRelay(redis.NewClient(&redis.Options{Addr: mr.Addr()}))
	b := NewRelay(redis.NewClient(&redis.Options{Addr: mr.Addr()}))
	if err := a.Subscribe(ctx, deliver("a")); err != nil {
		t.Fatalf("subscribe a: %v", err)
	}
	if err := b.Subscribe(ctx, deliver("b")); err != nil {
		t.Fatalf("subscribe b: %v", err)
	}

	if n, err := a.Publish(ctx, "pipeline", []byte(`{"x":1}`)); err != nil || n != 2 {
		t.Fatalf("publish: receivers=%d err=%v, want 2 receivers", n, err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		done := len(recv["a"]) >= 1 && len(recv["b"]) >= 1
		mu.Unlock()
		if done || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond) // a duplicate would have arrived by now

	mu.Lock()
	defer mu.Unlock()
	for _, name := range []string{"a", "b"} {
		if len(recv[name]) != 1 {
			t.Fatalf("subscriber %s got %d messages, want 1: %v", name, len(recv[name]), recv[name])
		}
		if recv[name][0] != (got{"pipeline", `{"x":1}`}) {
			t.Fatalf("subscriber %s got %+v", name, recv[name][0])
		}
	}
}
