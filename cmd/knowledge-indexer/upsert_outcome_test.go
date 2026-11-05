package main

import (
	"strings"
	"testing"
)

// An index run whose modules are all test-only (a helper-only directory
// in an incremental run, a tests-only repository) attempts nothing and
// succeeds with nothing to upsert; a run that attempted modules and
// landed none is the failure.
func TestUpsertOutcomeCountsAttemptedModulesOnly(t *testing.T) {
	if err := upsertOutcome(0, 0, nil); err != nil {
		t.Fatalf("all test-only: %v, want success", err)
	}
	if err := upsertOutcome(3, 3, nil); err != nil {
		t.Fatalf("all upserted: %v", err)
	}
	if err := upsertOutcome(0, 3, []string{"a: boom", "b: boom", "c: boom"}); err == nil || !strings.Contains(err.Error(), "no modules successfully upserted out of 3") {
		t.Fatalf("none landed: %v", err)
	}
	if err := upsertOutcome(2, 3, []string{"c: boom"}); err == nil || !strings.Contains(err.Error(), "1 of 3 modules failed") {
		t.Fatalf("partial: %v", err)
	}
}
