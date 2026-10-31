package idempotency

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestConsumersAreWired pins the M1-05 consumer migration: every in-scope
// service builds a Ledger and wraps its handler under its own service name
// (the consumer key in processed_events).
func TestConsumersAreWired(t *testing.T) {
	for _, svc := range []string{
		"status-orchestrator", "implementation-planner", "pr-coordinator",
		"implementation-agent", "review-agent", "qa-agent", "product-agent",
		"research-coordinator", "research-agent", "knowledge-indexer",
		"change-reconciler", "dashboard-backend",
	} {
		dir := filepath.Join("..", "..", "cmd", svc)
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("%s: %v", svc, err)
		}
		var src strings.Builder
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			b, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			src.Write(b)
		}
		s := src.String()
		if !strings.Contains(s, "idempotency.NewLedger(") || !strings.Contains(s, ".Handler(") {
			t.Errorf("%s: no Ledger.Handler wiring", svc)
		}
		if !strings.Contains(s, `"`+svc+`"`) {
			t.Errorf("%s: the consumer name %q does not appear in the service", svc, svc)
		}
	}
}
