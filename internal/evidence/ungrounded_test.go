package evidence

import (
	"encoding/json"
	"strings"
	"testing"
)

// Codex coherence Part 3: "ungrounded" (too many empty codex lookups in the
// run) is a known flag; it never releases automatically but keeps the
// human paths.
func TestUngroundedFlag_NoAutomaticRelease(t *testing.T) {
	ev := sampleEvidence(t)
	ev.Checks = ev.Checks[:1]
	ev.Outcome = OutcomeVerified
	if d := ReleaseDecision(&ev, ReleasePolicy{}); d.Action != ActionRelease {
		t.Fatalf("fixture: verified without flags = %s, want release", d.Action)
	}

	ev.Flags = []string{FlagUngrounded}
	if err := ev.Validate(); err != nil {
		t.Fatalf("ungrounded must be a known flag: %v", err)
	}
	d := ReleaseDecision(&ev, ReleasePolicy{})
	if d.Action != ActionHumanReview {
		t.Fatalf("verified + ungrounded = %s, want human_review (no automatic release)", d.Action)
	}
	if !strings.Contains(d.Reason, "ungrounded") {
		t.Errorf("reason %q does not say why", d.Reason)
	}

	// Partially verified keeps its own rule (blocked without the opt-in,
	// human review with it) and names the flag.
	partial := sampleEvidence(t)
	partial.Flags = []string{FlagUngrounded}
	if d := ReleaseDecision(&partial, ReleasePolicy{}); d.Action != ActionBlock || !strings.Contains(d.Reason, "ungrounded") {
		t.Errorf("partial + ungrounded, no opt-in = %+v", d)
	}
	if d := ReleaseDecision(&partial, ReleasePolicy{PartialVerificationOptIn: true}); d.Action != ActionHumanReview {
		t.Errorf("partial + ungrounded, opt-in = %+v", d)
	}
}

// WithFlag adds a flag to evidence still on the wire, keeping every other
// field, and is idempotent.
func TestWithFlag(t *testing.T) {
	ev := sampleEvidence(t)
	raw, _ := json.Marshal(ev)
	var withExtra map[string]any
	_ = json.Unmarshal(raw, &withExtra)
	withExtra["future_field"] = "kept"
	raw, _ = json.Marshal(withExtra)

	out, err := WithFlag(raw, FlagUngrounded)
	if err != nil {
		t.Fatal(err)
	}
	again, err := WithFlag(out, FlagUngrounded)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := Parse(again)
	if err != nil {
		t.Fatal(err)
	}
	if !parsed.HasFlag(FlagUngrounded) || len(parsed.Flags) != 1 {
		t.Errorf("flags = %v, want [ungrounded]", parsed.Flags)
	}
	if !strings.Contains(string(again), `"future_field":"kept"`) {
		t.Errorf("WithFlag dropped an unknown field: %s", again)
	}
	if _, err := WithFlag([]byte(`null`), FlagUngrounded); err == nil {
		t.Error("WithFlag on no record must fail")
	}
}
