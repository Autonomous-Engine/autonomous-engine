package tools

import (
	"bytes"
	"context"
	"fmt"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
)

// countingOverride answers every call with a body derived from its
// arguments and counts executions.
func countingOverride(n *int64) ToolOverride {
	return func(_ context.Context, call adapter.ToolCall) (adapter.ToolResult, bool) {
		atomic.AddInt64(n, 1)
		runtime.Gosched()
		return adapter.ToolResult{Content: "body of " + call.Arguments}, true
	}
}

func identicalReadCalls(n int) []adapter.ToolCall {
	calls := make([]adapter.ToolCall, n)
	for i := range calls {
		calls[i] = adapter.ToolCall{
			ID:        fmt.Sprintf("call_%d", i),
			Name:      "codex_read_file",
			Arguments: `{"path":"src/server/index.ts"}`,
		}
	}
	return calls
}

// goroutineID parses the current goroutine's id from its stack header
// ("goroutine 123 [running]:"). Test-only.
func goroutineID() uint64 {
	buf := make([]byte, 64)
	buf = buf[:runtime.Stack(buf, false)]
	buf = bytes.TrimPrefix(buf, []byte("goroutine "))
	if i := bytes.IndexByte(buf, ' '); i > 0 {
		buf = buf[:i]
	}
	id, _ := strconv.ParseUint(string(buf), 10, 64)
	return id
}

// X1: a 32-call batch of identical cacheable reads used to write the
// shared dedup map from every goroutine ("fatal error: concurrent map
// writes", live-run attempt 5). Run it many times with real parallelism;
// the old code dies here, the fixed code never touches the map off the
// calling goroutine.
func TestParallelBatchWithIdenticalKeysDoesNotRaceTheCache(t *testing.T) {
	prev := runtime.GOMAXPROCS(0)
	if prev < 4 {
		runtime.GOMAXPROCS(4)
		t.Cleanup(func() { runtime.GOMAXPROCS(prev) })
	}
	for i := 0; i < 200; i++ {
		t.Run(fmt.Sprintf("run%03d", i), func(t *testing.T) {
			t.Parallel()
			var execs int64
			ex := NewExecutor(t.TempDir())
			ex.SetToolOverride(countingOverride(&execs))
			cache := map[string]string{}
			calls := identicalReadCalls(32)
			results := runToolCallBatches(context.Background(), ex, calls, cache)
			if len(results) != 32 {
				t.Fatalf("got %d results, want 32", len(results))
			}
			for k, r := range results {
				if r.ToolCallID != calls[k].ID {
					t.Fatalf("result %d has id %q, want %q", k, r.ToolCallID, calls[k].ID)
				}
				if r.IsError || r.Content != "body of "+calls[k].Arguments {
					t.Fatalf("result %d = %+v", k, r)
				}
			}
			if len(cache) != 1 {
				t.Fatalf("cache has %d entries, want 1", len(cache))
			}
		})
	}
}

// X1: deterministic form. Every cache accessor reports the goroutine it
// ran on; all must be the goroutine that called runToolCallBatches.
func TestParallelBatchTouchesTheCacheOnlyFromTheCallingGoroutine(t *testing.T) {
	var mu sync.Mutex
	var accesses []uint64
	toolCacheAccessHook = func() {
		id := goroutineID()
		mu.Lock()
		accesses = append(accesses, id)
		mu.Unlock()
	}
	t.Cleanup(func() { toolCacheAccessHook = nil })

	var execs int64
	ex := NewExecutor(t.TempDir())
	ex.SetToolOverride(countingOverride(&execs))
	calls := identicalReadCalls(16)
	// Two distinct keys plus a pre-cached one, so lookups, stores and
	// hits all happen.
	calls[3].Arguments = `{"path":"README.md"}`
	calls[5].Arguments = `{"path":"docs/configuration.md"}`
	cache := map[string]string{"codex_read_file:" + `{"path":"docs/configuration.md"}`: "cached docs"}

	self := goroutineID()
	results := runToolCallBatches(context.Background(), ex, calls, cache)

	mu.Lock()
	defer mu.Unlock()
	if len(accesses) == 0 {
		t.Fatal("no cache accessor was called; the hook is not wired")
	}
	for _, id := range accesses {
		if id != self {
			t.Fatalf("cache accessed from goroutine %d, want only the caller %d (%d accesses)", id, self, len(accesses))
		}
	}
	if results[5].Content != "cached docs" || results[5].ToolCallID != "call_5" {
		t.Fatalf("pre-cached call not served from the cache: %+v", results[5])
	}
	// Identical keys inside one batch run once: two distinct misses.
	if got := atomic.LoadInt64(&execs); got != 2 {
		t.Fatalf("executed %d calls, want 2 (one per distinct uncached key)", got)
	}
	if results[3].Content != "body of "+calls[3].Arguments || results[3].ToolCallID != "call_3" {
		t.Fatalf("result 3 = %+v", results[3])
	}
	if len(cache) != 3 {
		t.Fatalf("cache has %d entries, want 3", len(cache))
	}
}

// A write tool still invalidates the cache, and a later read in the same
// turn re-executes.
func TestWriteToolStillInvalidatesTheCache(t *testing.T) {
	var execs int64
	ex := NewExecutor(t.TempDir())
	ex.SetToolOverride(countingOverride(&execs))
	cache := map[string]string{"codex_read_file:{}": "stale"}
	calls := []adapter.ToolCall{
		{ID: "a", Name: "codex_read_file", Arguments: `{"path":"a.go"}`},
		{ID: "b", Name: "codex_read_file", Arguments: `{"path":"b.go"}`},
		{ID: "w", Name: "write_file", Arguments: `{"path":"a.go","content":"x"}`},
		{ID: "c", Name: "codex_read_file", Arguments: `{"path":"a.go"}`},
	}
	results := runToolCallBatches(context.Background(), ex, calls, cache)
	if got := atomic.LoadInt64(&execs); got != 4 {
		t.Fatalf("executed %d calls, want 4 (the read after the write re-runs)", got)
	}
	if results[3].ToolCallID != "c" {
		t.Fatalf("result 3 id = %q", results[3].ToolCallID)
	}
	if _, ok := cache["codex_read_file:{}"]; ok {
		t.Fatal("write did not invalidate the cache")
	}
	if len(cache) != 1 {
		t.Fatalf("cache after the write has %d entries, want 1 (the re-read)", len(cache))
	}
}

// A panicking tool inside a parallel batch still yields an error result
// for its own index and does not take the process down.
func TestParallelBatchRecoversAPanickingTool(t *testing.T) {
	ex := NewExecutor(t.TempDir())
	ex.SetToolOverride(func(_ context.Context, call adapter.ToolCall) (adapter.ToolResult, bool) {
		if call.ID == "boom" {
			panic("kaboom")
		}
		return adapter.ToolResult{Content: "ok"}, true
	})
	calls := []adapter.ToolCall{
		{ID: "x", Name: "codex_grep", Arguments: `{"q":"1"}`},
		{ID: "boom", Name: "codex_grep", Arguments: `{"q":"2"}`},
	}
	cache := map[string]string{}
	results := runToolCallBatches(context.Background(), ex, calls, cache)
	if !results[1].IsError || results[1].ToolCallID != "boom" {
		t.Fatalf("panicking call result = %+v", results[1])
	}
	if results[0].Content != "ok" {
		t.Fatalf("sibling result = %+v", results[0])
	}
	if len(cache) != 1 {
		t.Fatalf("cache has %d entries, want 1 (errors are not cached)", len(cache))
	}
}

// observedIDs registers a tool callback that records every call id it sees.
func observedIDs(ex *Executor) func() map[string]int {
	var mu sync.Mutex
	seen := map[string]int{}
	ex.SetCallback(func(callID, _, _, _ string, isError bool, _ ResultMeta) {
		mu.Lock()
		defer mu.Unlock()
		if !isError {
			seen[callID]++
		}
	})
	return func() map[string]int {
		mu.Lock()
		defer mu.Unlock()
		out := make(map[string]int, len(seen))
		for k, v := range seen {
			out[k] = v
		}
		return out
	}
}

// R3 review: an in-batch duplicate and a cache hit are answered without
// executing, but the tool callback still sees each call id exactly once
// (the verifier's grounding set accepts only observed ids).
func TestReusedResultsAreReportedToTheCallback(t *testing.T) {
	var execs int64
	ex := NewExecutor(t.TempDir())
	ex.SetToolOverride(countingOverride(&execs))
	seen := observedIDs(ex)
	calls := identicalReadCalls(8)
	calls[7].Arguments = `{"path":"README.md"}`
	cache := map[string]string{"codex_read_file:" + `{"path":"README.md"}`: "cached readme"}
	runToolCallBatches(context.Background(), ex, calls, cache)
	if got := atomic.LoadInt64(&execs); got != 1 {
		t.Fatalf("executed %d calls, want 1", got)
	}
	got := seen()
	for _, c := range calls {
		if got[c.ID] != 1 {
			t.Errorf("callback saw %s %d time(s), want 1 (all: %v)", c.ID, got[c.ID], got)
		}
	}
}

// R3 review: a duplicate of a failed call does not inherit the error; it
// executes (and a later duplicate reuses its success).
func TestDuplicateOfAFailedCallExecutes(t *testing.T) {
	var execs int64
	ex := NewExecutor(t.TempDir())
	ex.SetToolOverride(func(_ context.Context, call adapter.ToolCall) (adapter.ToolResult, bool) {
		if atomic.AddInt64(&execs, 1) == 1 {
			return adapter.ToolResult{Content: "transient failure", IsError: true}, true
		}
		return adapter.ToolResult{Content: "body of " + call.Arguments}, true
	})
	seen := observedIDs(ex)
	calls := identicalReadCalls(4)
	cache := map[string]string{}
	results := runToolCallBatches(context.Background(), ex, calls, cache)
	if !results[0].IsError {
		t.Fatalf("first call should carry its own failure: %+v", results[0])
	}
	for k := 1; k < 4; k++ {
		if results[k].IsError || results[k].ToolCallID != calls[k].ID || results[k].Content != "body of "+calls[k].Arguments {
			t.Fatalf("duplicate %d = %+v, want its own success", k, results[k])
		}
	}
	if got := atomic.LoadInt64(&execs); got != 2 {
		t.Fatalf("executed %d calls, want 2 (the failure, then one retry the rest reuse)", got)
	}
	got := seen()
	for k := 1; k < 4; k++ {
		if got[calls[k].ID] != 1 {
			t.Errorf("callback saw %s %d time(s), want 1", calls[k].ID, got[calls[k].ID])
		}
	}
}
