package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// attempt12Context is attempt 12's intake (work item d402ace4, read-only
// export of work_items.description and research_rounds.prd_content) as the
// coordinator sees it: the intake's verified seed lines and the hint text.
func attempt12Context(t *testing.T) seedContext {
	t.Helper()
	desc := readFixture(t, "testdata/attempt12_intake.txt")
	prd := readFixture(t, "testdata/attempt12_prd.md")
	seeds := validateSeeds(extractSeedCandidates(desc, prd), []string{sampleAppClone}, nil)
	if seeds.Unverified || seeds.Lines["public/index.html"] == nil {
		t.Fatalf("intake seeds = %+v", seeds)
	}
	return seedContext{intakeFiles: seeds.Files, intakeLines: seeds.Lines, hintText: desc + "\n\n" + prd}
}

// R12 X3: attempt 12's places to edit (its plan task carried
// public/index.html:836-853 quoting only 837), quoted as X3 quotes them.
// The implementation agent's attempt-12 task test reads these seeds
// (golden copy testdata/attempt12_line_seeds.json; R12_WRITE_GOLDEN=1
// rewrites it).
func TestLineSeedsAttempt12Golden(t *testing.T) {
	seeds := extractLineSeedsWith(nil, []string{sampleAppClone}, attempt12Context(t))
	for _, s := range seeds {
		t.Logf("seed %s:%d-%d %q", s.Path, s.LineStart, s.LineEnd, s.Snippet)
	}
	by := seedsByPath(seeds)
	if pub := by["public/index.html"]; pub.LineStart != 836 || pub.LineEnd != 853 || !strings.Contains(pub.Snippet, "\n840: ") || !strings.Contains(pub.Snippet, "\n848: ") {
		t.Fatalf("public seed %+v", pub)
	}
	if land := by["landing/index.html"]; land.LineStart != 855 || !strings.HasPrefix(land.Snippet, "855: ") {
		t.Fatalf("landing seed %+v", land)
	}
	golden := "testdata/attempt12_line_seeds.json"
	want, _ := json.MarshalIndent(seeds, "", " ")
	if os.Getenv("R12_WRITE_GOLDEN") == "1" {
		if err := os.WriteFile(golden, append(want, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(golden)
	if err != nil || strings.TrimSpace(strings.ReplaceAll(string(got), "\r\n", "\n")) != string(want) {
		t.Fatalf("%s is not the seeds extracted here (%v):\n%s", golden, err, want)
	}
}

// snippetLineNumbers are the "N: " numbers of a verified snippet.
func snippetLineNumbers(snippet string) []string {
	var out []string
	for _, l := range strings.Split(snippet, "\n") {
		if n, _, ok := strings.Cut(l, ": "); ok {
			out = append(out, n)
		}
	}
	return out
}

// R12 X3: a range of at most 20 lines is quoted whole, without its leading
// and trailing non-code lines and without blank and comment lines. Live run attempt 12
// quoted only public/index.html:837 (`L.control.zoom`) of 836-853.
func TestShortRangeIsQuotedWhole(t *testing.T) {
	ev := `{"path": "public/index.html", "start_line": 836, "end_line": 853}` + "\n" +
		`{"path": "landing/index.html", "start_line": 855, "end_line": 855}`
	seeds := extractLineSeedsWith([]researchResultMessage{{Status: ResearchStatusOK, Evidence: ev}}, []string{sampleAppClone}, seedContext{hintText: "Mapforge API key missing"})
	by := seedsByPath(seeds)
	pub := by["public/index.html"]
	got := strings.Join(snippetLineNumbers(pub.Snippet), ",")
	// 838 and 845 are blank, 839, 846 and 847 comments; 853 is the lone
	// closer `});`.
	if want := "836,837,840,841,842,843,844,848,849,850,851,852"; got != want || !pub.Verified || pub.LineStart != 836 || pub.LineEnd != 853 {
		t.Fatalf("public seed %d-%d lines %s, want %s:\n%s", pub.LineStart, pub.LineEnd, got, want, pub.Snippet)
	}
	for _, want := range []string{
		"840:     L.tileLayer('https://{s}.tiles.mapforgecdn.example/night_base/{z}/{x}/{y}.png', {",
		"848:     const labelsLayer = L.tileLayer('https://{s}.tiles.mapforgecdn.example/night_labels/",
		"837:     L.control.zoom({ position: 'topright' }).addTo(map);",
	} {
		if !strings.Contains(pub.Snippet, want) {
			t.Errorf("public snippet lacks %q", want)
		}
	}
	if land := by["landing/index.html"]; land.Snippet != "855:     L.tileLayer('https://{s}.tiles.mapforgecdn.example/night_full/{z}/{x}/{y}.png', {" {
		t.Fatalf("landing snippet %q", land.Snippet)
	}
}

// R12 review 3: comments are told by the file's language. A CSS `#id {`
// selector, a C `#include` and a Markdown heading are kept; `#` comments
// in Python, `*` lines inside a JS block comment and `//` lines are
// dropped; a `*`-prefixed line outside a block comment is kept.
func TestWholeRangeCommentsByLanguage(t *testing.T) {
	quote := func(path string, lines ...string) string {
		var out []string
		for _, i := range wholeRangeLines(path, lines, 0, len(lines)-1) {
			out = append(out, strings.TrimSpace(lines[i]))
		}
		return strings.Join(out, " | ")
	}
	for _, tc := range []struct {
		path  string
		lines []string
		want  string
	}{
		{"public/style.css", []string{"#map { height: 100%; }", "/* the legend */", "#legend { color: red; }"},
			"#map { height: 100%; } | #legend { color: red; }"},
		{"src/main.c", []string{"#include <stdio.h>", "// entry point", "int main(void) {"},
			"#include <stdio.h> | int main(void) {"},
		{"docs/configuration.md", []string{"## Configuration", "<!-- generated -->", "Set MAPFORGE_API_KEY first."},
			"## Configuration | Set MAPFORGE_API_KEY first."},
		{"scripts/run.py", []string{"import os, sys", "# the key", "key = os.environ['KEY']"},
			"import os, sys | key = os.environ['KEY']"},
		{"src/a.js", []string{"const total = a", "  * factor;", "/**", " * Docs line.", " */", "export default total;"},
			"const total = a | * factor; | export default total;"},
		{"public/index.html", []string{"const map = L.map('map');", "// Dark base layer", "L.tileLayer('https://x/{z}.png');"},
			"const map = L.map('map'); | L.tileLayer('https://x/{z}.png');"},
	} {
		if got := quote(tc.path, tc.lines...); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.path, got, tc.want)
		}
	}
}

// A longer range quotes at most 3 lines: those whose URL or string literal
// shares a hint word first — the tile URLs, not `L.control.zoom`.
func TestLongRangeQuotesHintedLiterals(t *testing.T) {
	ev := `{"path": "public/index.html", "start_line": 800, "end_line": 870}`
	seeds := extractLineSeedsWith([]researchResultMessage{{Status: ResearchStatusOK, Evidence: ev}}, []string{sampleAppClone}, seedContext{hintText: "Mapforge API key missing"})
	pub := seedsByPath(seeds)["public/index.html"]
	nums := snippetLineNumbers(pub.Snippet)
	got := "," + strings.Join(nums, ",") + ","
	if len(nums) > seedLongRangeLines || !strings.Contains(got, ",840,") || !strings.Contains(got, ",848,") || strings.Contains(got, ",837,") {
		t.Fatalf("long range quotes %v:\n%s", nums, pub.Snippet)
	}
	// Without a hint the anchor heuristic still quotes something.
	seeds = extractLineSeedsWith([]researchResultMessage{{Status: ResearchStatusOK, Evidence: ev}}, []string{sampleAppClone}, seedContext{})
	if pub := seedsByPath(seeds)["public/index.html"]; !pub.Verified || len(snippetLineNumbers(pub.Snippet)) == 0 || len(snippetLineNumbers(pub.Snippet)) > seedLongRangeLines {
		t.Fatalf("no-hint long range: %+v", pub)
	}
}
