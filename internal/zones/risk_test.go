package zones

import (
	"reflect"
	"testing"
)

func TestRiskZoneTemplates(t *testing.T) {
	// The prefill's answer, unedited: modules after the colon, plus the
	// candidate's evidence modules.
	answer := "3 of 20 modules are high-risk (>5 dependents): internal/db, internal/auth, pkg/api. Changes to these modules affect the most downstream code."
	got := RiskZoneTemplates(answer, []string{"internal/db", "billing"})
	want := []Template{
		{Name: "billing", Description: riskDescription, Patterns: []string{"billing/**"}, Policy: PolicyHumanReview, Source: SourceOnboarding},
		{Name: "internal/auth", Description: riskDescription, Patterns: []string{"internal/auth/**"}, Policy: PolicyHumanReview, Source: SourceOnboarding},
		{Name: "internal/db", Description: riskDescription, Patterns: []string{"internal/db/**"}, Policy: PolicyHumanReview, Source: SourceOnboarding},
		{Name: "pkg/api", Description: riskDescription, Patterns: []string{"pkg/api/**"}, Policy: PolicyHumanReview, Source: SourceOnboarding},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}

	// A user's own words: a list of paths and globs; prose is ignored.
	got = RiskZoneTemplates("payments/**; src/security\nand anything touching the ledger, migrations/*.sql", nil)
	if names := templateNames(got); !reflect.DeepEqual(names, []string{"migrations/*.sql", "payments", "src/security"}) {
		t.Fatalf("names = %v", names)
	}
	if got[0].Patterns[0] != "migrations/*.sql" || got[1].Patterns[0] != "payments/**" {
		t.Fatalf("patterns = %+v", got)
	}

	for _, none := range []string{
		"", "(none identified)",
		"No high-risk modules detected across 12 modules. All modules have 5 or fewer dependents. Risk profile is flat.",
		"No modules detected — risk analysis requires at least one analyzed module.",
	} {
		if got := RiskZoneTemplates(none, nil); len(got) != 0 {
			t.Errorf("%q → %+v, want none", none, got)
		}
	}
}

func templateNames(ts []Template) []string {
	var out []string
	for _, t := range ts {
		out = append(out, t.Name)
	}
	return out
}
