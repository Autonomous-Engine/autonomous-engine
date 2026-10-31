package evidence

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestFlagsAreAdditiveWithinSchemaV1: a record without flags encodes
// exactly as before (no "flags" key), a flagged record round-trips through
// Parse, and an unknown flag fails Validate so no consumer can ignore a
// risk marker it does not understand.
func TestFlagsAreAdditiveWithinSchemaV1(t *testing.T) {
	ev := sampleEvidence(t)
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"flags"`) {
		t.Fatalf("unflagged record encodes a flags key: %s", raw)
	}

	ev.Flags = []string{FlagSecurityReviewRequired}
	raw, err = json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"flags":["security_review_required"]`) {
		t.Fatalf("flagged record = %s, want flags array", raw)
	}
	parsed, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse flagged record: %v", err)
	}
	if !parsed.HasFlag(FlagSecurityReviewRequired) || parsed.SchemaVersion != SchemaVersion {
		t.Fatalf("parsed = %+v, want the flag at schema %d", parsed, SchemaVersion)
	}

	ev.Flags = []string{FlagSecurityReviewRequired, ""}
	if err := ev.Validate(); err == nil || !strings.Contains(err.Error(), "unknown flag") {
		t.Fatalf("Validate with an empty flag = %v, want unknown flag", err)
	}
	ev.Flags = []string{"skip_review"}
	if err := ev.Validate(); err == nil || !strings.Contains(err.Error(), "flags[0]") {
		t.Fatalf("Validate with an unknown flag = %v, want flags[0] error", err)
	}
	var nilEv *ImplementationEvidence
	if nilEv.HasFlag(FlagSecurityReviewRequired) {
		t.Fatal("nil record reports a flag")
	}
}
