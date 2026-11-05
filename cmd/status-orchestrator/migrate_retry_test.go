package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// A migration that fails for a transient reason (a connection blip, a
// lock timeout) must not leave the service unhealthy until someone
// restarts it: every other service waits on this one's health. The
// migrations are retried until they apply, and /health reports the last
// failure meanwhile.
func TestMigrationsAreRetriedUntilTheyApply(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	migrate := func(_ context.Context, dir string) error {
		n := calls.Add(1)
		if n <= 3 { // the first attempt (two dirs) and one retry fail
			return errors.New("dial tcp: connection refused")
		}
		return nil
	}
	applied, lastErr := applyMigrations(ctx, migrate, []string{"/app/migrations", "migrations"}, 10*time.Millisecond)
	check := migrationCheckOf(applied, lastErr)
	if err := check(); err == nil || err.Error() != "migrations not applied: dial tcp: connection refused" {
		t.Fatalf("health after the first failure = %v, want the failure reported", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for check() != nil && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if err := check(); err != nil {
		t.Fatalf("migrations never applied: %v (calls %d)", err, calls.Load())
	}
	if n := calls.Load(); n < 4 {
		t.Fatalf("calls = %d, want the retries", n)
	}
}

// A migration that applies on the first try reports healthy at once and
// is not retried.
func TestMigrationsThatApplyAreNotRetried(t *testing.T) {
	var calls atomic.Int32
	applied, lastErr := applyMigrations(context.Background(), func(context.Context, string) error { calls.Add(1); return nil }, []string{"a", "b"}, time.Millisecond)
	if err := migrationCheckOf(applied, lastErr)(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1", calls.Load())
	}
}
