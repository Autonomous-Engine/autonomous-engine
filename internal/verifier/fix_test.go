package verifier

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

// fixtureFixVerdict is a live-run verdict's shape: AC-8 not met at index.html:840,
// a major defect in src/map.js, a minor nit, and the AC-11 PR-text gap.
func fixtureFixVerdict() Verdict {
	gapLine := "criterion AC-11 is non-code: satisfied by the PR text; verify manually"
	return Verdict{
		SchemaVersion: SchemaVersion,
		PerCriterion: []CriterionResult{
			{ID: "AC-1", Status: StatusMet, Evidence: []Ref{"index.html:10"}},
			{ID: "AC-8", Status: StatusNotMet, Evidence: []Ref{"call_3", "index.html:840", "src/map.js:12-14"}, Note: "the tile layer still requests the keyless endpoint"},
			{ID: "AC-11", Status: StatusNotVerifiable, Evidence: []Ref{}, Note: gapLine},
		},
		Defects: []Defect{
			{Severity: SeverityMinor, File: "README.md", Line: 3, Category: "docs", Description: "typo"},
			{Severity: SeverityMajor, File: "src/map.js", Line: 12, Category: "logic", Description: "AC-8: the URL template ignores TILES_API_KEY", Reproduction: "load the map with the key set"},
		},
		Overall: OverallRequestChanges,
		Gaps:    []string{gapLine},
	}
}

func TestRenderFixBriefCarriesTheVerdictsFindings(t *testing.T) {
	v := fixtureFixVerdict()
	if g := Gate(v); g.Action != ActionRequestChanges {
		t.Fatalf("fixture gates to %+v, want request_changes", g)
	}
	src := FixSource{
		TaskID:   "11111111-2222-3333-4444-555555555555",
		Verdict:  v,
		Criteria: map[string]string{"AC-1": "the page loads", "AC-8": "Map tiles are requested with the TileHost API key from TILES_API_KEY"},
		Seeds: []LineSeed{
			{Path: "index.html", LineStart: 836, LineEnd: 853, Snippet: "map.addTiles('https://tiles.example.com/{z}/{x}/{y}.png')\n  .addTo(map);", Verified: true},
			{Path: "src/other.js", LineStart: 5, LineEnd: 5, Snippet: "x", Verified: true}, // not referenced: dropped
			{Path: "./src/map.js", LineStart: 10, LineEnd: 14, Snippet: "paraphrase", Verified: false},
		},
	}
	brief, seeds := RenderFixBrief([]FixSource{src})
	for _, want := range []string{
		"INDEPENDENT VERIFIER REQUESTED CHANGES",
		`- AC-8 not met (subtask 11111111): "Map tiles are requested with the TileHost API key from TILES_API_KEY"`,
		"Why: the tile layer still requests the keyless endpoint",
		"At: index.html:840, src/map.js:12-14",
		"- [major] src/map.js:12 — logic: AC-8: the URL template ignores TILES_API_KEY",
		"Reproduction: load the map with the key set",
		`Criterion AC-8: "Map tiles are requested`,
		"- [minor, optional] README.md:3 — docs: typo",
		"Recorded gaps (not code work",
		"- criterion AC-11 is non-code: satisfied by the PR text; verify manually",
		`Where to edit: the task's "## Where to edit" lists the original subtask's line seeds on these files (2).`,
	} {
		if !strings.Contains(brief, want) {
			t.Errorf("brief lacks %q:\n%s", want, brief)
		}
	}
	// Review minor 4: the seeds render once, from line_seeds — the brief
	// carries no copy of them.
	for _, lack := range []string{"call_3", "src/other.js", "paraphrase", "AC-1 ", "index.html:836", "map.addTiles"} {
		if strings.Contains(brief, lack) {
			t.Errorf("brief has %q:\n%s", lack, brief)
		}
	}
	// Order: the unmet criterion, then the major defect, then the minor.
	if i, j, k := strings.Index(brief, "AC-8 not met"), strings.Index(brief, "[major]"), strings.Index(brief, "[minor"); !(i < j && j < k) {
		t.Errorf("order: criterion %d, major %d, minor %d", i, j, k)
	}
	if len(seeds) != 2 || seeds[0].Path != "index.html" || seeds[1].Path != "./src/map.js" {
		t.Errorf("seeds = %+v, want index.html and src/map.js only", seeds)
	}
	if len(brief) > FixBriefMaxChars {
		t.Errorf("brief is %d bytes, over %d", len(brief), FixBriefMaxChars)
	}
}

func TestRenderFixBriefIsBounded(t *testing.T) {
	v := Verdict{Overall: OverallRequestChanges}
	crit := map[string]string{}
	var seeds []LineSeed
	for i := 1; i <= 40; i++ {
		id := fmt.Sprintf("AC-%d", i)
		crit[id] = strings.Repeat("é criterion text ", 30)
		v.PerCriterion = append(v.PerCriterion, CriterionResult{ID: id, Status: StatusNotMet, Evidence: []Ref{Ref(fmt.Sprintf("f%d.go:%d", i, i))}, Note: strings.Repeat("why ", 100)})
		v.Defects = append(v.Defects, Defect{Severity: SeverityMajor, File: fmt.Sprintf("f%d.go", i), Line: i, Category: "logic", Description: strings.Repeat("d ", 200)})
		seeds = append(seeds, LineSeed{Path: fmt.Sprintf("f%d.go", i), LineStart: i, LineEnd: i + 3, Snippet: strings.Repeat("x", 500), Verified: true})
	}
	brief, got := RenderFixBrief([]FixSource{{TaskID: "t", Verdict: v, Criteria: crit, Seeds: seeds}})
	if len(brief) > FixBriefMaxChars {
		t.Fatalf("brief is %d bytes, over %d", len(brief), FixBriefMaxChars)
	}
	if !utf8.ValidString(brief) {
		t.Fatal("brief was cut inside a rune")
	}
	if !strings.Contains(brief, "more item(s) not shown; see the verdict artifact") {
		t.Errorf("brief does not count the items it dropped:\n%s", brief)
	}
	if len(got) != fixBriefMaxSeeds {
		t.Errorf("seeds = %d, want the cap %d", len(got), fixBriefMaxSeeds)
	}
	// A smaller budget (a caller reserving room) is honoured too.
	small, _ := RenderFixBriefWithin([]FixSource{{TaskID: "t", Verdict: v, Criteria: crit, Seeds: seeds}}, 1200)
	if len(small) > 1200 || !strings.Contains(small, "more item(s) not shown") {
		t.Errorf("RenderFixBriefWithin(1200) = %d bytes:\n%s", len(small), small)
	}
}

// Gaps that do not fit are counted, not cut silently.
func TestRenderFixBriefCountsGapsThatDoNotFit(t *testing.T) {
	v := Verdict{PerCriterion: []CriterionResult{{ID: "AC-1", Status: StatusNotMet, Evidence: []Ref{"a.go:1"}, Note: "n"}}}
	for i := 2; i < 60; i++ {
		v.Gaps = append(v.Gaps, fmt.Sprintf("criterion AC-%d is non-code: %s; verify manually", i, strings.Repeat("g", 120)))
	}
	brief, _ := RenderFixBrief([]FixSource{{TaskID: "t", Verdict: v}})
	if len(brief) > FixBriefMaxChars || !strings.Contains(brief, "more recorded gap(s) not shown") {
		t.Errorf("brief (%d bytes) does not count the gaps it dropped:\n%s", len(brief), brief)
	}
}

func TestRenderFixBriefSeveralSubtasks(t *testing.T) {
	a := FixSource{TaskID: "aaaaaaaa-0000", Verdict: Verdict{PerCriterion: []CriterionResult{{ID: "AC-1", Status: StatusNotMet, Evidence: []Ref{"a.go:1"}, Note: "n"}}}}
	b := FixSource{TaskID: "bbbbbbbb-0000", Verdict: Verdict{Defects: []Defect{{Severity: SeverityCritical, File: "b.go", Line: 2, Category: "nil_dereference", Description: "panics"}}}}
	brief, _ := RenderFixBrief([]FixSource{a, b})
	for _, want := range []string{"AC-1 not met (subtask aaaaaaaa)", "[critical] b.go:2 — nil_dereference: panics"} {
		if !strings.Contains(brief, want) {
			t.Errorf("brief lacks %q:\n%s", want, brief)
		}
	}
}
