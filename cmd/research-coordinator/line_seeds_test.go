package main

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// attempt9Results are live run attempt 9's two research results (code_trace
// and impact_assessment; evidence only), rebuilt on the synthetic sample-app.
func attempt9Results(t *testing.T) []researchResultMessage {
	t.Helper()
	f, err := os.Open("testdata/attempt9_research_results.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []researchResultMessage
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<16), 1<<22)
	for sc.Scan() {
		var r researchResultMessage
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// sampleAppClone is the synthetic repository the research fixtures were
// rebuilt on (a bike-dock station finder whose map pages load Mapforge
// basemap tiles): the files and line numbers the recorded evidence names.
const sampleAppClone = "testdata/sample-app"

// R10 W5: the code_trace evidence's line references reach the brief as
// line seeds, verified against the clone: public/index.html 836-853 quotes
// the two Mapforge tile layers (840, 848), landing/index.html 855 its one.
// Whole-file ranges (README 1-81, docker-compose 1-329) are not seeds.
func TestLineSeedsFromAttempt9Evidence(t *testing.T) {
	seeds := extractLineSeeds(attempt9Results(t), []string{sampleAppClone})
	for _, s := range seeds {
		t.Logf("seed %s:%d-%d verified=%v %q", s.Path, s.LineStart, s.LineEnd, s.Verified, s.Snippet)
	}
	byPath := map[string]lineSeed{}
	for _, s := range seeds {
		byPath[s.Path] = s
		if n := len(strings.Split(s.Snippet, "\n")); n > seedWholeRangeLines {
			t.Errorf("%s snippet has %d lines", s.Path, n)
		}
	}
	if len(seeds) > maxLineSeeds {
		t.Fatalf("%d seeds", len(seeds))
	}
	pub, ok := byPath["public/index.html"]
	if !ok || pub.LineStart != 836 || pub.LineEnd != 853 || !pub.Verified {
		t.Fatalf("public seed = %+v", pub)
	}
	for _, want := range []string{"840:     L.tileLayer('https://{s}.tiles.mapforgecdn.example/night_base/{z}/{x}/{y}.png', {", "848:     const labelsLayer = L.tileLayer('https://{s}.tiles.mapforgecdn.example/night_labels/"} {
		if !strings.Contains(pub.Snippet, want) {
			t.Errorf("public snippet lacks %q:\n%s", want, pub.Snippet)
		}
	}
	land := byPath["landing/index.html"]
	if land.LineStart != 855 || !strings.Contains(land.Snippet, "855:     L.tileLayer('https://{s}.tiles.mapforgecdn.example/night_full/") {
		t.Fatalf("landing seed = %+v", land)
	}
	for _, p := range []string{"README.md", "docker-compose.yml", "docs/configuration.md", "tests/unit/server.test.ts", ".env.example", "src/config/env.ts"} {
		if s, ok := byPath[p]; ok {
			t.Errorf("whole-file range kept as a seed: %+v", s)
		}
	}
	// Without a clone the seeds keep only their path and lines (never an
	// unverified snippet).
	raw := extractLineSeeds(attempt9Results(t), nil)
	if len(raw) == 0 || raw[0].Verified || raw[0].Snippet != "" || raw[0].Path != "public/index.html" || raw[0].LineStart != 836 {
		t.Fatalf("unverified seeds = %+v", raw)
	}
	// The artifact carries them under line_seeds.
	b, _ := json.Marshal(technicalBrief{LineSeeds: seeds})
	if !strings.Contains(string(b), `"line_seeds":[{"path":"public/index.html","line_start":836,"line_end":853`) {
		t.Fatalf("artifact JSON: %s", b)
	}
	// The golden copy of these seeds (R10_WRITE_GOLDEN=1 rewrites it); the
	// implementation agent's "Where to edit" test reads the same seeds.
	golden := "testdata/attempt9_line_seeds.json"
	want, _ := json.MarshalIndent(seeds, "", " ")
	if os.Getenv("R10_WRITE_GOLDEN") == "1" {
		if err := os.WriteFile(golden, append(want, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(golden)
	if err != nil || strings.TrimSpace(strings.ReplaceAll(string(got), "\r\n", "\n")) != string(want) {
		t.Fatalf("%s is not the seeds extracted here (%v):\n%s", golden, err, want)
	}
}
