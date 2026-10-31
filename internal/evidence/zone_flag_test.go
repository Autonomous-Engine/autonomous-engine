package evidence

import (
	"encoding/json"
	"strings"
	"testing"
)

// Codex zones (migration 063): a change in a human_review zone carries
// FlagZoneReviewRequired and the zones it touched. The release gate sends
// verified work with the flag to human review exactly like the security
// flag, never to release.
func TestZoneReviewFlag(t *testing.T) {
	ev := verifiedEvidence(t)
	ev.Flags = []string{FlagZoneReviewRequired}
	ev.Zones = []ZoneTouch{
		{Name: "guarded", Policy: "human_review", Files: []string{"cmd/api/server.go"}},
		{Name: "safe", Policy: "autonomous", Files: []string{"cmd/api/main.go"}},
	}
	if err := ev.Validate(); err != nil {
		t.Fatalf("flagged record must validate: %v", err)
	}
	for _, optIn := range []bool{false, true} {
		d := ReleaseDecision(ev, ReleasePolicy{PartialVerificationOptIn: optIn})
		if d.Action != ActionHumanReview {
			t.Fatalf("opt-in %v: action = %s, want human_review", optIn, d.Action)
		}
		if !strings.Contains(d.Reason, "zone guarded") || strings.Contains(d.Reason, "safe") {
			t.Fatalf("reason = %q, want the human_review zone named (and only it)", d.Reason)
		}
	}
	if got := ev.ZoneReviewNames(); len(got) != 1 || got[0] != "guarded" {
		t.Fatalf("ZoneReviewNames = %v", got)
	}

	// Both flags: still human review, both reasons named.
	ev.Flags = []string{FlagSecurityReviewRequired, FlagZoneReviewRequired}
	d := ReleaseDecision(ev, ReleasePolicy{})
	if d.Action != ActionHumanReview || !strings.Contains(d.Reason, "security-review paths") || !strings.Contains(d.Reason, "zone guarded") {
		t.Fatalf("both flags = %+v", d)
	}

	// Partially verified with the zone flag: the opt-in still decides, and
	// the reason names the zone.
	p := partiallyVerifiedEvidence(t)
	p.Flags = []string{FlagZoneReviewRequired}
	p.Zones = []ZoneTouch{{Name: "guarded", Policy: "human_review"}}
	if d := ReleaseDecision(p, ReleasePolicy{}); d.Action != ActionBlock || !strings.Contains(d.Reason, "zone guarded") {
		t.Fatalf("partial, not opted in = %+v", d)
	}
	if d := ReleaseDecision(p, ReleasePolicy{PartialVerificationOptIn: true}); d.Action != ActionHumanReview {
		t.Fatalf("partial, opted in = %+v", d)
	}

	// The flag without a human_review zone is inconsistent; a zone without
	// a name or with an unknown policy is invalid.
	bad := verifiedEvidence(t)
	bad.Flags = []string{FlagZoneReviewRequired}
	if err := bad.Validate(); err == nil {
		t.Fatal("zone flag without a human_review zone must not validate")
	}
	bad.Zones = []ZoneTouch{{Name: "", Policy: "human_review"}}
	if err := bad.Validate(); err == nil {
		t.Fatal("unnamed zone must not validate")
	}
	bad.Zones = []ZoneTouch{{Name: "x", Policy: "maybe"}}
	if err := bad.Validate(); err == nil {
		t.Fatal("unknown zone policy must not validate")
	}

	// Additive: no zones key on a record without zones.
	plain := verifiedEvidence(t)
	raw, _ := json.Marshal(plain)
	if strings.Contains(string(raw), `"zones"`) {
		t.Fatalf("record without zones encodes a zones key: %s", raw)
	}
}
