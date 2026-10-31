package main

import (
	"os"
	"regexp"
	"testing"
)

// R16 G4: the breaker's limit is a named constant (3 fix rounds per parent,
// review and verifier together: the previous effective live limit, from
// MAX_RETRY_COUNT=3), overridable by MAX_CHANGE_CYCLES.
func TestMaxChangeCycles(t *testing.T) {
	if DefaultMaxChangeCycles != 3 {
		t.Fatalf("DefaultMaxChangeCycles = %d, want 3", DefaultMaxChangeCycles)
	}
	t.Setenv(MaxChangeCyclesEnv, "")
	if got := maxChangeCycles(); got != 3 {
		t.Errorf("unset: %d, want 3", got)
	}
	t.Setenv(MaxChangeCyclesEnv, "2")
	if got := maxChangeCycles(); got != 2 {
		t.Errorf("2: %d", got)
	}
	for _, bad := range []string{"0", "-1", "x"} {
		t.Setenv(MaxChangeCyclesEnv, bad)
		if got := maxChangeCycles(); got != DefaultMaxChangeCycles {
			t.Errorf("%q: %d, want the default", bad, got)
		}
	}
	// Both sources count toward the same breaker.
	if !changeCycleReasons[changesFromReview.reason] || !changeCycleReasons[changesFromVerification.reason] {
		t.Error("the breaker must count review and verification fix rounds together")
	}
}

// The breaker no longer takes MAX_RETRY_COUNT (the QA-bug breaker's limit):
// requestChanges reads its own limit.
func TestRequestChangesUsesItsOwnLimit(t *testing.T) {
	src, err := os.ReadFile("changes.go")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`limit := maxChangeCycles\(\)`).Match(src) {
		t.Error("requestChanges does not read maxChangeCycles")
	}
	if regexp.MustCompile(`func requestChanges\([^)]*maxRetries`).Match(src) {
		t.Error("requestChanges still takes maxRetries")
	}
}
