package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadRecords_JSONAndJSONLAgree(t *testing.T) {
	root := filepath.Join("..", "..", "scripts", "testdata")
	a, err := loadRecords([]string{filepath.Join(root, "sample-results.json")})
	if err != nil {
		t.Fatal(err)
	}
	b, err := loadRecords([]string{filepath.Join(root, "sample-results.jsonl")})
	if err != nil {
		t.Fatal(err)
	}
	both, err := loadRecords([]string{filepath.Join(root, "sample-results.json"), filepath.Join(root, "sample-results.jsonl")})
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 36 || len(b) != 36 || len(both) != 36 {
		t.Fatalf("records: json %d, jsonl %d, both (de-duplicated) %d; want 36", len(a), len(b), len(both))
	}
	refs := recordRefs(a, "autonomous-engine", "claude-cli")
	var cand, base int
	for _, r := range refs {
		switch r.Side {
		case "candidate":
			cand++
		case "baseline":
			base++
		}
		if r.SnapshotID != "" {
			t.Errorf("the sample fixture carries no snapshot ids, got %q", r.SnapshotID)
		}
	}
	if cand != 18 || base != 18 {
		t.Errorf("sides: %d candidate, %d baseline", cand, base)
	}
}

func TestRecordSnapshotID_FlattenedOrNested(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "r.jsonl")
	content := `{"run_id":"r","task_id":"a","competitor":"ae-candidate","trial":0,"role":"candidate","versions":{"config_snapshot_id":"aaa"}}
{"run_id":"r","task_id":"b","competitor":"ae-baseline","trial":0,"role":"baseline","versions":{},"agent_output":{"versions":{"config_snapshot_id":"bbb"}}}
{"run_id":"r","task_id":"c","competitor":"other","trial":0}
`
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	recs, err := loadRecords([]string{p})
	if err != nil {
		t.Fatal(err)
	}
	refs := recordRefs(recs, "ae-candidate", "ae-baseline")
	if len(refs) != 2 || refs[0].SnapshotID != "aaa" || refs[1].SnapshotID != "bbb" || refs[1].Side != "baseline" {
		t.Errorf("refs: %+v", refs)
	}
}

func TestShadowPlan_PassesEachSideItsSnapshot(t *testing.T) {
	p := shadowPlan{Manifest: "benchmarks/tasks.json", Trials: 3, Wrapper: "python benchmarks/wrappers/ae.py",
		Candidate: "cccc", Baseline: "bbbb", RolloutID: "rid", ResultsOut: "out/results.json", ReportOut: "out/report.json"}
	args := strings.Join(p.runnerArgs(), "\n")
	for _, want := range []string{
		"--trials\n3", "--comparison\nrollout", "--candidate-name\nae-candidate", "--baseline-name\nae-baseline",
		"--candidate-command\nAE_CONFIG_SNAPSHOT_ID='cccc' AE_ROLLOUT_ID='rid' python benchmarks/wrappers/ae.py",
		"--baseline-command\nAE_CONFIG_SNAPSHOT_ID='bbbb' AE_ROLLOUT_ID='rid' python benchmarks/wrappers/ae.py",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("runner args lack %q:\n%s", want, args)
		}
	}
	report := strings.Join(reportArgs("m.json", "out/report.json", []string{"out/results.json"}), " ")
	if !strings.Contains(report, "--partition sealed") || !strings.Contains(report, "--json out/report.json") {
		t.Errorf("report args: %s", report)
	}
	if got := shellQuote("it's"); got != `'it'"'"'s'` {
		t.Errorf("shellQuote: %s", got)
	}
}

// The observation path: evaluate the M4-03 sample report without a database.
func TestEvaluateOffline_SampleFixture(t *testing.T) {
	var out bytes.Buffer
	report := filepath.Join("..", "..", "internal", "rollout", "testdata", "sample-report-sealed.json")
	if err := cmdEvaluate(context.Background(), []string{"--offline", "--report", report}, &out); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{"Rollout decision: MORE_TRIALS", "pass_rate_regression", "MORE_TRIALS", "decision not written"} {
		if !strings.Contains(text, want) {
			t.Errorf("output lacks %q:\n%s", want, text)
		}
	}
}
