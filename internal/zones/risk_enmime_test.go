package zones

import (
	"sort"
	"strings"
	"testing"
)

// The audit's risk_zones answer. The wizard proposed boundary.go,
// header.go, parser.go and internal/stringutil (from the *lower-risk*
// clause) and dropped part.go, envelope.go, internal/coding, charsets.go
// and internal/textproto (F27).
const enmimeRiskAnswer = "Highest risk: the root enmime package parser (part.go, boundary.go, parser.go, header.go, envelope.go) since every consumer depends on its output; internal/coding (charsets.go, Base64/QP cleaners) because charset mistakes silently corrupt text; internal/textproto header reading. Lower risk: cmd/ tools, internal/stringutil, tests and fixtures."

func TestRiskZoneTemplatesEnmimeAnswer(t *testing.T) {
	got := RiskZoneTemplates(enmimeRiskAnswer, nil)
	names := make([]string, 0, len(got))
	for _, z := range got {
		names = append(names, z.Name)
	}
	sort.Strings(names)
	want := []string{"boundary.go", "charsets.go", "envelope.go", "header.go", "internal/coding", "internal/textproto", "parser.go", "part.go"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("zones = %v\nwant    %v", names, want)
	}
	for _, z := range got {
		if z.Name == "internal/coding" && z.Patterns[0] != "internal/coding/**" {
			t.Errorf("internal/coding pattern = %v", z.Patterns)
		}
	}
}

// The prefill's own phrasing and a bare module list still work.
func TestRiskZoneTemplatesPrefillListStillParses(t *testing.T) {
	got := RiskZoneTemplates("2 of 10 modules are high-risk (>5 dependents): coding, textproto. Changes to these modules affect the most downstream code.", []string{"enmime"})
	names := make([]string, 0, len(got))
	for _, z := range got {
		names = append(names, z.Name)
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "coding,enmime,textproto" {
		t.Fatalf("zones = %v", names)
	}
}
