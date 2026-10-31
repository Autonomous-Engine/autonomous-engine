package lsp

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeStarter stands in for spawning a language server: it counts spawns,
// records the readiness timeout each was given, and fails or succeeds.
type fakeStarter struct {
	mu       sync.Mutex
	spawns   int
	timeouts []time.Duration
	fail     bool
	block    chan struct{} // when set, a spawn waits on it
}

func (f *fakeStarter) start(ctx context.Context, repoRoot, language, command string, args []string, ready time.Duration) (*Client, error) {
	f.mu.Lock()
	f.spawns++
	f.timeouts = append(f.timeouts, ready)
	fail, block := f.fail, f.block
	f.mu.Unlock()
	if block != nil {
		<-block
	}
	if fail {
		return nil, errors.New("lsp: typescript workspace not ready: context deadline exceeded")
	}
	return &Client{}, nil
}

func (f *fakeStarter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.spawns
}

func newTestManager(f *fakeStarter, clock *time.Time) *Manager {
	m := NewManager()
	m.detector = func(string) (string, string, []string, error) {
		return "typescript", "typescript-language-server", []string{"--stdio"}, nil
	}
	m.start = f.start
	m.now = func() time.Time { return *clock }
	return m
}

func TestReadinessFailureIsCachedForTheCooldown(t *testing.T) {
	t.Setenv("LSP_READY_COOLDOWN", "")
	clock := time.Unix(1_000_000, 0)
	f := &fakeStarter{fail: true}
	m := newTestManager(f, &clock)

	_, _, err1 := m.ClientForRepo(context.Background(), "/repo")
	if err1 == nil || !strings.Contains(err1.Error(), "not ready") {
		t.Fatalf("first Get err = %v, want a not-ready error", err1)
	}
	start := time.Now()
	_, _, err2 := m.ClientForRepo(context.Background(), "/repo")
	if err2 == nil || !strings.Contains(err2.Error(), "not ready") {
		t.Fatalf("second Get err = %v, want the cached not-ready error", err2)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("a cached failure must return immediately")
	}
	if n := f.count(); n != 1 {
		t.Fatalf("spawns = %d within the cooldown, want 1", n)
	}
	if m.Ready("/repo") {
		t.Fatal("Ready = true after a failed probe")
	}

	// Still inside the default 10-minute cooldown: no spawn.
	clock = clock.Add(9 * time.Minute)
	m.ClientForRepo(context.Background(), "/repo") //nolint:errcheck
	if n := f.count(); n != 1 {
		t.Fatalf("spawns = %d at 9m, want 1", n)
	}
	// After the cooldown the server is probed again, with the regular timeout.
	clock = clock.Add(2 * time.Minute)
	m.ClientForRepo(context.Background(), "/repo") //nolint:errcheck
	if n := f.count(); n != 2 {
		t.Fatalf("spawns = %d after the cooldown, want 2", n)
	}
	if f.timeouts[0] != 30*time.Second || f.timeouts[1] != 15*time.Second {
		t.Fatalf("readiness timeouts = %v, want [30s 15s] (first-ever probe is longer)", f.timeouts)
	}
}

// A caller whose own deadline (validateAfterWrite 10s, a tool call 15s) is
// shorter than the probe must not turn LSP off for the cooldown: its
// timeout says nothing about the server. The next caller probes again,
// still with the full first-probe timeout.
func TestCallerDeadlineIsNotCachedAsAServerFailure(t *testing.T) {
	clock := time.Unix(1_000_000, 0)
	f := &fakeStarter{}
	m := newTestManager(f, &clock)
	m.start = func(ctx context.Context, repoRoot, language, command string, args []string, ready time.Duration) (*Client, error) {
		f.mu.Lock()
		f.spawns++
		f.timeouts = append(f.timeouts, ready)
		n := f.spawns
		f.mu.Unlock()
		if n == 1 {
			<-ctx.Done() // the probe outlives the caller's deadline
			return nil, errors.New("lsp: typescript workspace not ready: " + ctx.Err().Error())
		}
		return &Client{}, nil
	}
	short, cancelShort := context.WithTimeout(context.Background(), 10*time.Millisecond)
	if _, _, err := m.ClientForRepo(short, "/repo"); err == nil {
		t.Fatal("first Get succeeded, want the deadline error")
	}
	cancelShort()
	if _, _, err := m.ClientForRepo(context.Background(), "/repo"); err != nil {
		t.Fatalf("second Get err = %v, want a fresh probe that succeeds", err)
	}
	if n := f.count(); n != 2 {
		t.Fatalf("spawns = %d, want 2 (the deadline was not cached)", n)
	}
	if f.timeouts[1] != 30*time.Second {
		t.Fatalf("second probe timeout = %v, want the full first-probe 30s", f.timeouts[1])
	}

	// A cancelled caller is not cached either.
	f2 := &fakeStarter{}
	m2 := newTestManager(f2, &clock)
	m2.start = func(ctx context.Context, repoRoot, language, command string, args []string, ready time.Duration) (*Client, error) {
		f2.mu.Lock()
		f2.spawns++
		f2.mu.Unlock()
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m2.ClientForRepo(ctx, "/repo")                  //nolint:errcheck
	m2.ClientForRepo(context.Background(), "/repo") //nolint:errcheck
	if n := f2.count(); n != 2 {
		t.Fatalf("spawns = %d after a cancelled caller, want 2", n)
	}
}

func TestReadinessCooldownAndTimeoutFromEnv(t *testing.T) {
	t.Setenv("LSP_READY_COOLDOWN", "1m")
	t.Setenv("LSP_READY_TIMEOUT", "45s")
	clock := time.Unix(1_000_000, 0)
	f := &fakeStarter{fail: true}
	m := newTestManager(f, &clock)
	m.ClientForRepo(context.Background(), "/repo") //nolint:errcheck
	clock = clock.Add(61 * time.Second)
	m.ClientForRepo(context.Background(), "/repo") //nolint:errcheck
	if n := f.count(); n != 2 {
		t.Fatalf("spawns = %d, want 2 with a 1m cooldown", n)
	}
	if f.timeouts[0] != 45*time.Second {
		t.Fatalf("first probe timeout = %v, want 45s from LSP_READY_TIMEOUT", f.timeouts[0])
	}
}

func TestReadyIsNonBlockingAndWarmMakesItTrue(t *testing.T) {
	clock := time.Unix(1_000_000, 0)
	f := &fakeStarter{block: make(chan struct{})}
	m := newTestManager(f, &clock)

	if m.Ready("/repo") {
		t.Fatal("Ready before any probe")
	}
	done := make(chan struct{})
	go func() { m.Warm(context.Background(), "/repo"); close(done) }()

	// While the probe is in flight Ready answers at once, and a second Warm
	// does not spawn again.
	deadline := time.Now().Add(2 * time.Second)
	for f.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if m.Ready("/repo") {
		t.Fatal("Ready = true while the probe is still running")
	}
	m.Warm(context.Background(), "/repo")
	close(f.block)
	<-done
	if !m.Ready("/repo") {
		t.Fatal("Ready = false after a successful warm")
	}
	if n := f.count(); n != 1 {
		t.Fatalf("spawns = %d, want 1", n)
	}
}

// Concurrent Gets during an in-flight probe share it: one spawn.
func TestConcurrentGetsShareOneProbe(t *testing.T) {
	clock := time.Unix(1_000_000, 0)
	f := &fakeStarter{fail: true, block: make(chan struct{})}
	m := newTestManager(f, &clock)
	var wg sync.WaitGroup
	var errs atomic.Int32
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := m.ClientForRepo(context.Background(), "/repo"); err != nil {
				errs.Add(1)
			}
		}()
	}
	deadline := time.Now().Add(2 * time.Second)
	for f.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	close(f.block)
	wg.Wait()
	if n := f.count(); n != 1 {
		t.Fatalf("spawns = %d for 5 concurrent Gets, want 1", n)
	}
	if errs.Load() != 5 {
		t.Fatalf("errors = %d, want 5", errs.Load())
	}
}
