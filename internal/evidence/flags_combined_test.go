package evidence

import (
	"strings"
	"testing"
)

// Codex coherence Parts 2 and 3 together: any of security_review_required,
// zone_review_required and ungrounded on verified evidence routes to human
// review, never release, and the reason names every flag present.
func TestReleaseDecision_SecurityZoneUngroundedCombinations(t *testing.T) {
	cases := []struct {
		flags []string
		want  []string
	}{
		{[]string{FlagUngrounded}, []string{"ungrounded"}},
		{[]string{FlagZoneReviewRequired}, []string{"zone guarded"}},
		{[]string{FlagSecurityReviewRequired}, []string{"security-review paths"}},
		{[]string{FlagZoneReviewRequired, FlagUngrounded}, []string{"zone guarded", "ungrounded"}},
		{[]string{FlagSecurityReviewRequired, FlagUngrounded}, []string{"security-review paths", "ungrounded"}},
		{[]string{FlagSecurityReviewRequired, FlagZoneReviewRequired, FlagUngrounded}, []string{"security-review paths", "zone guarded", "ungrounded"}},
	}
	for _, c := range cases {
		ev := verifiedEvidence(t)
		ev.Flags = c.flags
		ev.Zones = []ZoneTouch{{Name: "guarded", Policy: ZonePolicyHumanReview, Files: []string{"db/migrations/001.sql"}}}
		if err := ev.Validate(); err != nil {
			t.Fatalf("%v: %v", c.flags, err)
		}
		d := ReleaseDecision(ev, ReleasePolicy{})
		if d.Action != ActionHumanReview {
			t.Errorf("%v: action = %s, want human_review", c.flags, d.Action)
		}
		for _, w := range c.want {
			if !strings.Contains(d.Reason, w) {
				t.Errorf("%v: reason %q does not name %q", c.flags, d.Reason, w)
			}
		}
	}
}
