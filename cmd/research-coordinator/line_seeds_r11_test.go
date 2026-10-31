package main

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// attempt11Results are live run attempt 11's two research results (work
// item 2bba384c, research_round_tasks.result, rebuilt on sample-app): the
// code_trace evidence holds the tile-layer lines only as a codex_read_file
// JSON result, the impact_assessment evidence only as read calls.
func attempt11Results(t *testing.T) []researchResultMessage {
	t.Helper()
	f, err := os.Open("testdata/attempt11_research_results.jsonl")
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

func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}

// attempt11Context is the coordinator's view of attempt 11's intake: the
// seed set it logged (`landing/index.html:[855] public/index.html:[836
// 853] src/server/index.ts:[33 60]`, validated against the clone) and the
// intake description and PRD as hints.
func attempt11Context(t *testing.T) seedContext {
	t.Helper()
	desc := readFixture(t, "testdata/attempt11_intake.txt")
	prd := readFixture(t, "testdata/attempt11_prd.md")
	seeds := validateSeeds(extractSeedCandidates(desc, prd), []string{sampleAppClone}, nil)
	if seeds.Unverified || seeds.Lines["public/index.html"] == nil || seeds.Lines["landing/index.html"] == nil {
		t.Fatalf("intake seeds = %+v", seeds)
	}
	return seedContext{intakeFiles: seeds.Files, intakeLines: seeds.Lines, hintText: desc + "\n\n" + prd}
}

func seedsByPath(seeds []lineSeed) map[string]lineSeed {
	out := map[string]lineSeed{}
	for _, s := range seeds {
		if _, ok := out[s.Path]; !ok {
			out[s.Path] = s
		}
	}
	return out
}

// requireTileSeeds: the three tile-URL lines are quoted from the clone.
func requireTileSeeds(t *testing.T, seeds []lineSeed) {
	t.Helper()
	by := seedsByPath(seeds)
	pub, land := by["public/index.html"], by["landing/index.html"]
	if !pub.Verified || pub.LineStart > 840 || pub.LineEnd < 848 {
		t.Fatalf("public seed = %+v", pub)
	}
	for _, want := range []string{
		"840:     L.tileLayer('https://{s}.tiles.mapforgecdn.example/night_base/{z}/{x}/{y}.png', {",
		"848:     const labelsLayer = L.tileLayer('https://{s}.tiles.mapforgecdn.example/night_labels/",
	} {
		if !strings.Contains(pub.Snippet, want) {
			t.Errorf("public snippet lacks %q:\n%s", want, pub.Snippet)
		}
	}
	if !land.Verified || land.LineStart != 855 || !strings.Contains(land.Snippet, "855:     L.tileLayer('https://{s}.tiles.mapforgecdn.example/night_full/") {
		t.Fatalf("landing seed = %+v", land)
	}
	// R12 X3: 836-853 is short, so it is quoted whole (from 836, its first
	// code line; read_file centres there).
	if !strings.HasPrefix(pub.Snippet, "836: ") {
		t.Errorf("public snippet does not quote the whole range:\n%s", pub.Snippet)
	}
}

// R11 S1a: attempt 11's evidence shape (codex_read_file JSON, read calls
// in a ```json fence) yields the three tile-URL seeds. On df414a9 it
// yielded none, and the task message had no "## Where to edit".
func TestLineSeedsFromAttempt11Evidence(t *testing.T) {
	seeds := extractLineSeeds(attempt11Results(t), []string{sampleAppClone})
	for _, s := range seeds {
		t.Logf("seed %s:%d-%d verified=%v %q", s.Path, s.LineStart, s.LineEnd, s.Verified, s.Snippet)
	}
	if len(seeds) == 0 || len(seeds) > maxLineSeeds {
		t.Fatalf("%d seeds", len(seeds))
	}
	requireTileSeeds(t, seeds)
	for _, s := range seeds {
		if n := len(strings.Split(s.Snippet, "\n")); n > seedWholeRangeLines {
			t.Errorf("%s snippet has %d lines", s.Path, n)
		}
	}
	// The JSON scan alone finds the read result and the read calls; a read
	// result's content never becomes the snippet.
	js := parseJSONSeeds(attempt11Results(t)[0].Evidence)
	if len(js) == 0 || js[0].Path != "public/index.html" || js[0].LineStart != 836 || js[0].LineEnd != 853 || js[0].Snippet != "" {
		t.Fatalf("JSON seeds = %+v", js)
	}
}

// R11 S1b: research with no seed falls back to the intake's verified line
// map; a path research did seed is not seeded again from the intake.
func TestLineSeedsFallBackToIntakeLines(t *testing.T) {
	sc := attempt11Context(t)
	none := []researchResultMessage{{Status: ResearchStatusOK, Evidence: "Read the specified files and summarised the impact."}}
	seeds := extractLineSeedsWith(none, []string{sampleAppClone}, sc)
	for _, s := range seeds {
		t.Logf("seed %s:%d-%d verified=%v %q", s.Path, s.LineStart, s.LineEnd, s.Verified, s.Snippet)
	}
	requireTileSeeds(t, seeds)
	by := seedsByPath(seeds)
	if p := by["public/index.html"]; p.LineStart != 836 || p.LineEnd != 853 {
		t.Fatalf("intake range 836-853 not kept: %+v", p)
	}
	// The intake's ranges come first; a research seed inside one adds
	// nothing, one elsewhere in the file is kept after them.
	some := []researchResultMessage{{Status: ResearchStatusOK, Evidence: "The map opens at public/index.html:836-838 and the legend at public/index.html:1000-1002."}}
	seeds = extractLineSeedsWith(some, []string{sampleAppClone}, sc)
	var pubs []lineSeed
	for _, s := range seeds {
		if s.Path == "public/index.html" {
			pubs = append(pubs, s)
		}
	}
	if len(pubs) != 2 || pubs[0].LineStart != 836 || pubs[0].LineEnd != 853 || pubs[1].LineStart != 1000 {
		t.Fatalf("public seeds = %+v", pubs)
	}
	if seedsByPath(seeds)["landing/index.html"].LineStart != 855 {
		t.Fatalf("landing fallback missing: %+v", seeds)
	}
	// Unverified intake seeds are not a fallback.
	sc.intakeFiles, sc.intakeLines = nil, nil
	if got := extractLineSeedsWith(none, []string{sampleAppClone}, sc); len(got) != 0 {
		t.Fatalf("seeds without research or intake: %+v", got)
	}
	// The intake ranges: lines less than 80 apart share one.
	got := intakeLineSeeds("a.ts", []int{853, 836, 990})
	if len(got) != 2 || got[0].LineStart != 836 || got[0].LineEnd != 853 || got[1].LineStart != 990 {
		t.Fatalf("intake ranges = %+v", got)
	}
}

// R11 S1c: every seed is re-verified, and with the file present its quoted
// lines come from the file: a paraphrased snippet is replaced, prose refs
// count, and nothing is quoted without a clone.
func TestLineSeedsSnippetFromClone(t *testing.T) {
	ev := "The basemap is created in `public/index.html` (lines 836-853):\n" +
		"File: public/index.html\nLines: 836-853\n```js\nL.tileLayer(tileUrl, opts).addTo(map); // paraphrase\n```\n" +
		"and the landing page at landing/index.html line 855."
	res := []researchResultMessage{{Status: ResearchStatusOK, Evidence: ev}}
	pick := attempt11Context(t)
	pick.intakeFiles, pick.intakeLines = nil, nil
	seeds := extractLineSeedsWith(res, []string{sampleAppClone}, pick)
	for _, s := range seeds {
		t.Logf("seed %s:%d-%d verified=%v %q", s.Path, s.LineStart, s.LineEnd, s.Verified, s.Snippet)
		if strings.Contains(s.Snippet, "paraphrase") {
			t.Fatalf("research text quoted: %+v", s)
		}
	}
	requireTileSeeds(t, seeds)
	// Without a clone: the place only.
	for _, s := range extractLineSeedsWith(res, nil, pick) {
		if s.Verified || s.Snippet != "" {
			t.Fatalf("unverified seed quoted: %+v", s)
		}
	}
	// No snippet, no hints: a criterion's symbol line inside the range is
	// the anchor (attempt 11's AC-3 names L.tileLayer at 840).
	syms := attempt11Results(t)
	for i := range syms {
		syms[i].Evidence = strings.ReplaceAll(syms[i].Evidence, `"start_line": 836`, `"start_line": 836, "x": 0`)
	}
	seeds = extractLineSeedsWith(syms, []string{sampleAppClone}, seedContext{})
	requireTileSeeds(t, seeds)
	// Without a symbol or hint the first lines of the range are quoted.
	plain := extractLineSeedsWith([]researchResultMessage{{Status: ResearchStatusOK, Evidence: `{"path": "public/index.html", "start_line": 836, "end_line": 853}`}}, []string{sampleAppClone}, seedContext{})
	if len(plain) != 1 || !plain[0].Verified || !strings.HasPrefix(plain[0].Snippet, "835: ") && !strings.HasPrefix(plain[0].Snippet, "836: ") {
		t.Fatalf("plain seed = %+v", plain)
	}
}

// R11 S1: the brief carries the seeds the implementation agent's
// "## Where to edit" test reads (golden copy testdata/attempt11_line_seeds.json;
// R11_WRITE_GOLDEN=1 rewrites it). The fixture is the production path:
// attempt 11's results and the intake context.
func TestLineSeedsAttempt11Golden(t *testing.T) {
	seeds := extractLineSeedsWith(attempt11Results(t), []string{sampleAppClone}, attempt11Context(t))
	requireTileSeeds(t, seeds)
	golden := "testdata/attempt11_line_seeds.json"
	want, _ := json.MarshalIndent(seeds, "", " ")
	if os.Getenv("R11_WRITE_GOLDEN") == "1" {
		if err := os.WriteFile(golden, append(want, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(golden)
	if err != nil || strings.TrimSpace(strings.ReplaceAll(string(got), "\r\n", "\n")) != string(want) {
		t.Fatalf("%s is not the seeds extracted here (%v):\n%s", golden, err, want)
	}
}

// After a restart the coordinator has no session: the seed context comes
// from the persisted round (its PRD and its clones).
func TestSeedContextFromRoundAfterRestart(t *testing.T) {
	round := &researchRound{WorkID: uuid.New(), PRDContent: readFixture(t, "testdata/attempt11_intake.txt"), RepoList: []repoRef{{ClonePath: sampleAppClone}}}
	var c *coordinator
	sc := c.seedContext(round)
	if sc.intakeLines["public/index.html"] == nil || !strings.Contains(sc.hintText, "night_base") {
		t.Fatalf("seed context = %+v", sc)
	}
	if got := c.roundClones(round); len(got) != 1 || got[0] != sampleAppClone {
		t.Fatalf("round clones = %v", got)
	}
	co := newCoordinator()
	s := co.getOrCreate(round.WorkID)
	s.mu.Lock()
	s.dispatched, s.intakeText, s.prdContent = true, "intake words", "prd words"
	s.seeds = seedSet{Files: []string{"landing/index.html"}, Lines: map[string][]int{"landing/index.html": {855}}}
	s.mu.Unlock()
	sc = co.seedContext(round)
	if len(sc.intakeFiles) != 1 || sc.intakeLines["landing/index.html"][0] != 855 || !strings.Contains(sc.hintText, "intake words") {
		t.Fatalf("session seed context = %+v", sc)
	}
}
