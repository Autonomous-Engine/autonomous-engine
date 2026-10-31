package guards

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestRunAllPassesWhenAllValid verifies that RunAll returns Valid=true only
// when every input check passes.
func TestRunAllPassesWhenAllValid(t *testing.T) {
	got := RunAll(
		ValidationResult{Valid: true},
		ValidationResult{Valid: true},
	)
	if !got.Valid {
		t.Fatalf("RunAll(all valid) = %+v, want Valid=true", got)
	}
}

// TestRunAllFailsWhenAnyInvalid verifies that a single failure short-circuits
// the combined result to Valid=false with the failing message preserved.
func TestRunAllFailsWhenAnyInvalid(t *testing.T) {
	got := RunAll(
		ValidationResult{Valid: true},
		ValidationResult{
			Valid:   false,
			Missing: []string{"codex_grounding"},
			Message: "Codex has no grounding data",
		},
		ValidationResult{Valid: true},
	)
	if got.Valid {
		t.Fatalf("RunAll(with 1 invalid) = %+v, want Valid=false", got)
	}
	if !strings.Contains(got.Message, "Codex has no grounding data") {
		t.Errorf("RunAll.Message = %q, expected it to include the failing message", got.Message)
	}
}

// TestRunAllMergesMissingFromAllFailures verifies that multiple failing
// checks have their Missing slices merged so operators see every blocker.
func TestRunAllMergesMissingFromAllFailures(t *testing.T) {
	got := RunAll(
		ValidationResult{Valid: false, Missing: []string{"a"}, Message: "first failed"},
		ValidationResult{Valid: false, Missing: []string{"b"}, Message: "second failed"},
	)
	if got.Valid {
		t.Fatal("RunAll with multiple failures must be invalid")
	}
	if len(got.Missing) != 2 {
		t.Fatalf("Missing = %v, want 2 items", got.Missing)
	}
	if !strings.Contains(got.Message, "first failed") || !strings.Contains(got.Message, "second failed") {
		t.Errorf("Message %q should include both failure messages", got.Message)
	}
}

// The ungrounded message names the product and branch, says patterns are
// not enough, and points at Re-index and onboarding. Missing carries the
// stable machine-readable tag codex_grounding.
func TestUngroundedMessage(t *testing.T) {
	productID := uuid.New()
	msg := ungroundedMessage(productID, "feature/x", "master")
	for _, want := range []string{productID.String(), "feature/x", "default branch master", "patterns and anti-patterns alone are not enough", "Re-index", "onboard"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q lacks %q", msg, want)
		}
	}
	if MissingCodexGrounding != "codex_grounding" {
		t.Errorf("MissingCodexGrounding = %q; keep it stable", MissingCodexGrounding)
	}
}
