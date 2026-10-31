package main

import (
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/evidence"
)

// TestSecurityReviewLabelling (M3-03, D3): verified evidence carrying
// security_review_required opens a human-review PR labelled and marked as a
// security review, never as partially verified; a PR mixing it with a
// partially verified subtask keeps the partial title and names both.
func TestSecurityReviewLabelling(t *testing.T) {
	flaggedJSON := evidenceJSON(t, true, func(ev *evidence.ImplementationEvidence) {
		ev.Flags = []string{evidence.FlagSecurityReviewRequired}
		ev.Reason = "security review required: changed files match the repository's security_review_paths: internal/auth/login.go, migrations/052.up.sql"
	})
	security := decideSubtaskPR(payloadWith(flaggedJSON, true), notOptedIn(), evidence.CompatWindowStrict)
	if security.Action != evidence.ActionHumanReview || security.Label != outcomeLabelSecurityReview || !security.SecurityReview {
		t.Fatalf("flagged verified = %+v, want human_review labelled %q", security, outcomeLabelSecurityReview)
	}
	if security.SecurityFiles != "internal/auth/login.go, migrations/052.up.sql" {
		t.Fatalf("security files = %q", security.SecurityFiles)
	}

	title := prTitleFor("[repo] Login", []subtaskOutcome{security})
	if title != securityReviewTitlePrefix+"[repo] Login" {
		t.Fatalf("title = %q, want the security-review prefix", title)
	}
	if again := prTitleFor(title, []subtaskOutcome{security}); again != title {
		t.Fatalf("re-prefixed title = %q", again)
	}
	body := renderPRBody(prBodyInput{Title: "t", Branch: "b", BaseBranch: "main", Outcomes: []subtaskOutcome{security}})
	for _, want := range []string{securityReviewBodyMarker, "verified; touches security-review paths: internal/auth/login.go, migrations/052.up.sql"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "PARTIALLY VERIFIED") || strings.Contains(body, "partially verified evidence") {
		t.Fatalf("security-only body uses partial wording:\n%s", body)
	}
	if got := humanReviewBodyMarker([]subtaskOutcome{security}); got != securityReviewBodyMarker {
		t.Fatalf("marker = %q", got)
	}

	partial := decideSubtaskPR(payloadWith(partiallyVerifiedJSON(t), true), optedIn(), evidence.CompatWindowStrict)
	mixed := []subtaskOutcome{security, partial}
	if got := prTitleFor("x", mixed); !strings.HasPrefix(got, partialVerificationTitlePrefix) {
		t.Fatalf("mixed title = %q, want the partial prefix", got)
	}
	mb := renderPRBody(prBodyInput{Title: "t", Branch: "b", BaseBranch: "main", Outcomes: mixed})
	if !strings.Contains(mb, partialVerificationBodyMarker) || !strings.Contains(mb, "security-review paths") {
		t.Fatalf("mixed body must carry both reasons:\n%s", mb)
	}
}
