package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/evidence"
)

// Codex zones (migration 063): the PR body lists every zone the change
// touches with its policy; verified evidence flagged zone_review_required
// opens a human-review PR labelled and marked as a zone review, never as
// partially verified or security review.
func TestZoneReviewPRBodyListsZones(t *testing.T) {
	flaggedJSON := evidenceJSON(t, true, func(ev *evidence.ImplementationEvidence) {
		ev.Flags = []string{evidence.FlagZoneReviewRequired}
		ev.Zones = []evidence.ZoneTouch{
			{Name: "guarded", Policy: evidence.ZonePolicyHumanReview, Files: []string{"cmd/api/server.go"}},
			{Name: "safe", Policy: evidence.ZonePolicyAutonomous, Files: []string{"cmd/api/main.go", "internal/models/u.go"}},
		}
	})
	zone := decideSubtaskPR(payloadWith(flaggedJSON, true), notOptedIn(), evidence.CompatWindowStrict)
	if zone.Action != evidence.ActionHumanReview || zone.Label != outcomeLabelZoneReview || !zone.ZoneReview || zone.SecurityReview {
		t.Fatalf("flagged verified = %+v, want human_review labelled %q", zone, outcomeLabelZoneReview)
	}
	if len(zone.Zones) != 2 {
		t.Fatalf("zones = %+v", zone.Zones)
	}
	if got := prTitleFor("[repo] Pay", []subtaskOutcome{zone}); got != zoneReviewTitlePrefix+"[repo] Pay" {
		t.Fatalf("title = %q", got)
	}
	if again := prTitleFor(zoneReviewTitlePrefix+"[repo] Pay", []subtaskOutcome{zone}); again != zoneReviewTitlePrefix+"[repo] Pay" {
		t.Fatalf("re-prefixed title = %q", again)
	}
	body := renderPRBody(prBodyInput{Title: "t", Branch: "b", BaseBranch: "main", Outcomes: []subtaskOutcome{zone}})
	for _, want := range []string{
		zoneReviewBodyMarker,
		"verified; touches human_review codex zones: guarded",
		"## Codex zones touched",
		"| `guarded` | human_review | `cmd/api/server.go` |",
		"| `safe` | autonomous | `cmd/api/main.go`, `internal/models/u.go` |",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "PARTIALLY VERIFIED") || strings.Contains(body, "SECURITY REVIEW") {
		t.Fatalf("zone-only body uses another review's wording:\n%s", body)
	}

	// An autonomous-only change releases and still lists its zones.
	plainJSON := evidenceJSON(t, true, func(ev *evidence.ImplementationEvidence) {
		ev.Zones = []evidence.ZoneTouch{{Name: "safe", Policy: evidence.ZonePolicyAutonomous, Files: []string{"internal/models/u.go"}}}
	})
	plain := decideSubtaskPR(payloadWith(plainJSON, true), notOptedIn(), evidence.CompatWindowStrict)
	if plain.Action != evidence.ActionRelease {
		t.Fatalf("autonomous zone = %+v, want release", plain)
	}
	pb := renderPRBody(prBodyInput{Title: "t", Branch: "b", Outcomes: []subtaskOutcome{plain}})
	if !strings.Contains(pb, "| `safe` | autonomous |") || strings.Contains(pb, zoneReviewBodyMarker) {
		t.Fatalf("autonomous body:\n%s", pb)
	}
	// No zones: no section.
	none := decideSubtaskPR(payloadWith(evidenceJSON(t, true, nil), true), notOptedIn(), evidence.CompatWindowStrict)
	if nb := renderPRBody(prBodyInput{Title: "t", Branch: "b", Outcomes: []subtaskOutcome{none}}); strings.Contains(nb, "Codex zones") {
		t.Fatalf("zone-less body has a zones section:\n%s", nb)
	}

	// Mixed with a partial subtask: the partial title wins, both reasons named.
	partial := decideSubtaskPR(payloadWith(partiallyVerifiedJSON(t), true), optedIn(), evidence.CompatWindowStrict)
	mixed := []subtaskOutcome{zone, partial}
	if got := prTitleFor("x", mixed); !strings.HasPrefix(got, partialVerificationTitlePrefix) {
		t.Fatalf("mixed title = %q", got)
	}
	if mb := renderPRBody(prBodyInput{Title: "t", Branch: "b", Outcomes: mixed}); !strings.Contains(mb, partialVerificationBodyMarker) || !strings.Contains(mb, "human_review codex zones") {
		t.Fatalf("mixed body:\n%s", mb)
	}
}

// A partially verified run that also touches a human_review zone (product
// opted in): the PR keeps the partial marker and also names the zone
// review in its label, title and body.
func TestPartialZoneReviewNamesTheZone(t *testing.T) {
	raw := evidenceJSON(t, true, func(ev *evidence.ImplementationEvidence) {
		ev.Checks = append(ev.Checks, evidence.CheckResult{Kind: evidence.CheckKindLint, Command: "lint", Status: evidence.CheckSkipped, SkipReason: "no linter"})
		ev.Outcome = evidence.OutcomePartiallyVerified
		ev.Flags = []string{evidence.FlagZoneReviewRequired}
		ev.Zones = []evidence.ZoneTouch{{Name: "payments", Policy: evidence.ZonePolicyHumanReview, Files: []string{"payments/a.go"}}}
	})
	o := decideSubtaskPR(payloadWith(raw, true), optedIn(), evidence.CompatWindowStrict)
	if o.Action != evidence.ActionHumanReview || o.ZoneReview {
		t.Fatalf("outcome = %+v, want human_review that is not a pure zone review", o)
	}
	if !strings.Contains(o.Label, outcomeLabelPartiallyVerified) || !strings.Contains(o.Label, outcomeLabelZoneReview) {
		t.Fatalf("label = %q, want both partial and zone review", o.Label)
	}
	if !reflect.DeepEqual(o.ZoneReviewNames, []string{"payments"}) {
		t.Fatalf("zone names = %v", o.ZoneReviewNames)
	}
	title := prTitleFor("[repo] Pay", []subtaskOutcome{o})
	if title != partialVerificationTitlePrefix+zoneReviewTitlePrefix+"[repo] Pay" {
		t.Fatalf("title = %q", title)
	}
	if again := prTitleFor(title, []subtaskOutcome{o}); again != title {
		t.Fatalf("re-prefixed title = %q", again)
	}
	body := renderPRBody(prBodyInput{Title: "t", Branch: "b", Outcomes: []subtaskOutcome{o}})
	for _, want := range []string{partialVerificationBodyMarker, "touches human_review codex zones: payments"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body lacks %q:\n%s", want, body)
		}
	}
}
