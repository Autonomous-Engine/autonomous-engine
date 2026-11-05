package main

import (
	"strings"
	"testing"
)

// "Request changes" sends the item back through intake with the reviewer's
// feedback in metadata.user_guidance; the PRD prompt must carry it (F34).
func TestGuidanceFromIntakeReadsMetadata(t *testing.T) {
	msg := intakeMessage{Metadata: map[string]interface{}{"user_guidance": "  Name the option type Option.  "}}
	if got := guidanceFromIntake(msg); got != "Name the option type Option." {
		t.Fatalf("guidance = %q", got)
	}
	if got := guidanceFromIntake(intakeMessage{}); got != "" {
		t.Fatalf("no metadata: %q, want empty", got)
	}
	if got := guidanceFromIntake(intakeMessage{Metadata: map[string]interface{}{"user_guidance": 42}}); got != "" {
		t.Fatalf("non-string guidance: %q, want empty", got)
	}
}

func TestPRDContextCarriesGuidanceOnlyWhenPresent(t *testing.T) {
	with := prdContext("t", "p", "feature", "cx", "dashboard", "w1", "Keep the Option interface.")
	if !strings.Contains(with["user_guidance"], "Keep the Option interface.") || with["title"] != "t" || with["pain_point"] != "p" {
		t.Fatalf("context = %v", with)
	}
	without := prdContext("t", "p", "feature", "cx", "dashboard", "w1", "")
	if _, ok := without["user_guidance"]; ok {
		t.Fatalf("empty guidance must not add a key: %v", without)
	}
}
