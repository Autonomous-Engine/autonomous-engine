package main

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// approve-codex used to upsert a module literally named "main", which
// overwrote the real Go `package main` module the indexer had written
// (F29). The approval writes sections and patterns only.
func TestApprovalSectionsNeverNamesAModule(t *testing.T) {
	got := approvalSections([]string{"risk_zones", "autonomous_scope", "risk_zones"}, true)
	want := "section:autonomous_scope,section:risk_zones,patterns"
	if strings.Join(got, ",") != want {
		t.Fatalf("sections = %v, want %s", got, want)
	}
	for _, s := range got {
		if strings.HasPrefix(s, "module:") {
			t.Fatalf("approval must not claim a module change: %v", got)
		}
	}
	if got := approvalSections([]string{"k"}, false); strings.Join(got, ",") != "section:k" {
		t.Fatalf("patterns not written = %v", got)
	}
}

// The onboarding session is per product, created on demand: approve-codex
// must not fail with "no active session" because the dashboard never
// called /start (F28, F31).
func TestSessionForIsPerProductAndCreatedOnDemand(t *testing.T) {
	s := &server{}
	p1, p2 := uuid.New(), uuid.New()
	a := s.sessionFor(p1)
	if a == nil || a.ProductID != p1 {
		t.Fatalf("sessionFor(p1) = %+v", a)
	}
	if b := s.sessionFor(p1); b != a {
		t.Fatal("sessionFor(p1) must return the same session twice")
	}
	if c := s.sessionFor(p2); c == a {
		t.Fatal("sessions must be distinct per product")
	}
}
