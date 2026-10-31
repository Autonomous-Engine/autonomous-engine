package tools

import (
	"context"
	"errors"
	"testing"
)

// A failed workspace-root lookup is retried; only a success is kept.
func TestLSPRootRetriesAfterAFailedLookup(t *testing.T) {
	ex := NewExecutor(t.TempDir())
	calls := 0
	ex.lspRootResolver = func(context.Context) (string, error) {
		calls++
		if calls == 1 {
			return "", errors.New("list repos: connection reset")
		}
		return "/repo-cache/p/app", nil
	}
	if _, ok := ex.lspRoot(context.Background()); ok {
		t.Fatal("first lookup failed but lspRoot reported a root")
	}
	root, ok := ex.lspRoot(context.Background())
	if !ok || root != "/repo-cache/p/app" {
		t.Fatalf("second lookup = %q, %v; want the root after a retry", root, ok)
	}
	ex.lspRoot(context.Background())
	if calls != 2 {
		t.Fatalf("resolver calls = %d, want 2 (a success is kept)", calls)
	}
}
