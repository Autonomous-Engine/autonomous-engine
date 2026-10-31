package main

import (
	"fmt"
	"strings"
	"testing"
)

// R11 review: a codex_grep hit list ({file, line, text}) gives at most one
// seed per file and cannot crowd out the intake's verified lines, whose
// slots are reserved.
func TestLineSeedsGrepHitsDoNotCrowdOutTheIntake(t *testing.T) {
	var hits []string
	for i, f := range []string{"src/a.ts", "src/b.ts", "src/c.ts", "src/d.ts", "src/e.ts", "src/f.ts", "src/g.ts", "src/h.ts", "src/i.ts"} {
		for k := 0; k < 3; k++ {
			hits = append(hits, fmt.Sprintf(`{"file": %q, "line": %d, "text": "mapforge"}`, f, 10*i+k+5))
		}
	}
	ev := `{"name": "codex_grep", "result": {"hits": [` + strings.Join(hits, ", ") + `]}}`
	if js := parseJSONSeeds(ev); len(js) != 9 {
		t.Fatalf("JSON seeds from 27 grep hits in 9 files = %d, want 9 (one per file)", len(js))
	}
	seeds := extractLineSeedsWith([]researchResultMessage{{Status: ResearchStatusOK, Evidence: ev}}, []string{sampleAppClone}, attempt11Context(t))
	if len(seeds) != maxLineSeeds {
		t.Fatalf("%d seeds", len(seeds))
	}
	requireTileSeeds(t, seeds)
}

// R11 review: with a short hint the URL line wins over the Mapforge
// attribution next to it ("mapforgecdn" holds "mapforge"; a URL breaks the
// tie).
// R12 X3: short ranges are now quoted whole, so this runs on ranges over
// 20 lines, which quote at most 3 lines, URL lines first.
func TestSeedPickerPrefersTheURLLine(t *testing.T) {
	ev := `{"path": "public/index.html", "start_line": 800, "end_line": 870}` + "\n" +
		`{"path": "landing/index.html", "start_line": 830, "end_line": 880}`
	seeds := extractLineSeedsWith([]researchResultMessage{{Status: ResearchStatusOK, Evidence: ev}}, []string{sampleAppClone}, seedContext{hintText: "Mapforge API key missing"})
	for _, s := range seeds {
		t.Logf("seed %s:%d-%d %q", s.Path, s.LineStart, s.LineEnd, s.Snippet)
	}
	by := seedsByPath(seeds)
	pub, land := by["public/index.html"], by["landing/index.html"]
	if !strings.HasPrefix(pub.Snippet, "840: ") || !strings.Contains(pub.Snippet, "\n848: ") || strings.Contains(pub.Snippet, "837:") {
		t.Fatalf("public snippet:\n%s", pub.Snippet)
	}
	if !strings.HasPrefix(land.Snippet, "855: ") || strings.Contains(land.Snippet, "853:") {
		t.Fatalf("landing snippet:\n%s", land.Snippet)
	}
}
