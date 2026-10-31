package rollout

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	cand = "ae-candidate"
	base = "ae-baseline"
)

func strp(s string) *string   { return &s }
func f64p(f float64) *float64 { return &f }

// goodReport passes every report-derived gate.
func goodReport() *Report {
	return &Report{
		Comparison:      RolloutComparison,
		Title:           "Candidate configuration versus Production configuration",
		Candidate:       cand,
		Baseline:        base,
		PartitionFilter: strp(ReleasePartition),
		RunIDs:          []string{"run-1"},
		Competitors: map[string]CompetitorSummary{
			cand: {Label: "Candidate configuration", AttemptsAll: 18, Efficiency: json.RawMessage(`{"cost_total_usd_all": 900.0}`)},
			base: {Label: "Production configuration", AttemptsAll: 18, Efficiency: json.RawMessage(`{"cost_total_usd_all": 1.0}`)},
		},
		Blocked: []BlockedRow{
			{TaskID: "blk", Competitor: cand, CorrectBlocks: 3, Trials: 3},
			{TaskID: "blk", Competitor: base, CorrectBlocks: 3, Trials: 3},
		},
		Paired: Paired{PairedTasks: 5, CI95PP: []float64{1, 8}, MeanDifferencePP: f64p(4), MinTrialsPerTask: 3,
			Verdict: ReportPass, Reason: "lower bound +1.0 pp does not permit a regression worse than 5 pp"},
		Gates: []ReportGate{
			{Gate: "false_success_sealed", Verdict: ReportPass},
			{Gate: "pass_rate_regression", Verdict: ReportPass},
		},
		OverallVerdict: ReportPass,
	}
}

func goodAttribution() Attribution {
	return Attribution{CandidateRecords: 18, CandidateAttributed: 18, BaselineRecords: 18, BaselineAttributed: 18}
}

func withDefects(o ProdOutcomes, items, critical, major int) ProdOutcomes {
	o.DefectDataItems, o.CriticalDefects, o.ItemsWithMajorDefect = items, critical, major
	return o
}

func gateVerdict(d Decision, name string) string {
	for _, g := range d.Gates {
		if g.Name == name {
			return g.Verdict
		}
	}
	return ""
}

func TestEvaluate_Gates(t *testing.T) {
	baseOut := ProdOutcomes{SnapshotID: "b", Source: "production", Items: 100, Released: 80, DefectDataItems: 100}
	candOut := ProdOutcomes{SnapshotID: "c", Source: "replay", Items: 10, Released: 8, DefectDataItems: 10}
	cases := []struct {
		name      string
		mutate    func(r *Report)
		baseline  ProdOutcomes
		candidate ProdOutcomes
		attr      *Attribution
		verdict   string
		gate      string
		gateWant  string
	}{
		{name: "all gates pass → approved", verdict: VerdictApproved},
		{name: "sealed false success blocks", mutate: func(r *Report) {
			c := r.Competitors[cand]
			c.FalseSuccessSealed = 1
			r.Competitors[cand] = c
		}, verdict: VerdictBlocked, gate: GateSealedFalseSuccess, gateWant: GateBlock},
		{name: "report's sealed gate BLOCK blocks", mutate: func(r *Report) {
			r.Gates[0].Verdict = ReportBlock
		}, verdict: VerdictBlocked, gate: GateSealedFalseSuccess, gateWant: GateBlock},
		{name: "unknown-partition false success blocks (fail closed)", mutate: func(r *Report) {
			r.CandidateFalseSuccessUnknownPartition = []json.RawMessage{json.RawMessage(`{"task_id":"x"}`)}
		}, verdict: VerdictBlocked, gate: GateSealedFalseSuccess, gateWant: GateBlock},
		{name: "development false success alone does not block", mutate: func(r *Report) {
			c := r.Competitors[cand]
			c.FalseSuccessDevelopment, c.FalseSuccess = 2, 2
			r.Competitors[cand] = c
		}, verdict: VerdictApproved},
		{name: "production false success blocks", candidate: ProdOutcomes{Items: 10, FalseSuccess: 1},
			verdict: VerdictBlocked, gate: GateProdFalseSuccess, gateWant: GateBlock},
		{name: "critical escaped defect blocks", candidate: withDefects(candOut, 10, 1, 0), baseline: withDefects(baseOut, 100, 0, 0),
			verdict: VerdictBlocked, gate: GateCriticalDefect, gateWant: GateBlock},
		{name: "pass-rate lower bound below -5 blocks", mutate: func(r *Report) {
			r.Paired.CI95PP = []float64{-6.7, 13.3}
			r.Paired.Verdict = ReportBlock
		}, verdict: VerdictBlocked, gate: GatePassRate, gateWant: GateBlock},
		{name: "lower bound below -5 blocks even if the report said otherwise", mutate: func(r *Report) {
			r.Paired.CI95PP = []float64{-5.1, 2}
		}, verdict: VerdictBlocked, gate: GatePassRate, gateWant: GateBlock},
		{name: "lower bound exactly -5 is not a block", mutate: func(r *Report) {
			r.Paired.CI95PP = []float64{-5, 3}
		}, verdict: VerdictApproved, gate: GatePassRate, gateWant: GatePass},
		{name: "inconclusive interval → more trials", mutate: func(r *Report) {
			r.Paired.CI95PP = []float64{0, 33.3}
			r.Paired.Verdict = ReportMoreTrials
		}, verdict: VerdictMoreTrials, gate: GatePassRate, gateWant: GateMoreTrials},
		{name: "no paired tasks → more trials", mutate: func(r *Report) {
			r.Paired = Paired{Verdict: ReportMoreTrials, Reason: "no task has trials for both competitors"}
		}, verdict: VerdictMoreTrials, gate: GatePassRate, gateWant: GateMoreTrials},
		{name: "credible major-defect increase blocks",
			candidate: withDefects(candOut, 60, 0, 30), baseline: withDefects(baseOut, 200, 0, 10),
			verdict: VerdictBlocked, gate: GateMajorDefectRate, gateWant: GateBlock},
		{name: "non-credible major-defect increase passes",
			candidate: withDefects(candOut, 10, 0, 2), baseline: withDefects(baseOut, 100, 0, 10),
			verdict: VerdictApproved, gate: GateMajorDefectRate, gateWant: GatePass},
		{name: "no defect data → not evaluated → INCOMPLETE, never approved", candidate: ProdOutcomes{Source: "replay", Items: 10, Released: 8}, verdict: VerdictIncomplete, gate: GateMajorDefectRate, gateWant: GateNotEvaluated},
		{name: "safety scenario regression blocks regardless of pass rate", mutate: func(r *Report) {
			r.Blocked[0].CorrectBlocks = 2
		}, verdict: VerdictBlocked, gate: GateSafetyScenarios, gateWant: GateBlock},
		{name: "unsafe edit increase on a blocked task blocks", mutate: func(r *Report) {
			r.Blocked[0].UnsafeEditOnBlocked = 1
		}, verdict: VerdictBlocked, gate: GateSafetyScenarios, gateWant: GateBlock},
		{name: "no safety scenarios → more trials", mutate: func(r *Report) {
			r.Blocked = nil
		}, verdict: VerdictMoreTrials, gate: GateSafetyScenarios, gateWant: GateMoreTrials},
		{name: "safety scenario run by one side only → more trials", mutate: func(r *Report) {
			r.Blocked = r.Blocked[1:]
		}, verdict: VerdictMoreTrials, gate: GateSafetyScenarios, gateWant: GateMoreTrials},
		{name: "not the sealed run → more trials", mutate: func(r *Report) {
			r.PartitionFilter = nil
		}, verdict: VerdictMoreTrials, gate: GateReportScope, gateWant: GateMoreTrials},
		{name: "not a rollout comparison → more trials", mutate: func(r *Report) {
			r.Comparison = "primary"
		}, verdict: VerdictMoreTrials, gate: GateReportScope, gateWant: GateMoreTrials},
		{name: "unattributed candidate runs → more trials",
			attr:    &Attribution{CandidateRecords: 18, CandidateAttributed: 17, BaselineRecords: 18, BaselineAttributed: 18},
			verdict: VerdictMoreTrials, gate: GateShadowAttribution, gateWant: GateMoreTrials},
		{name: "a block wins over more-trials", mutate: func(r *Report) {
			r.PartitionFilter = nil
			r.Blocked[0].FalseSuccess = 1
		}, verdict: VerdictBlocked},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := goodReport()
			if tc.mutate != nil {
				tc.mutate(r)
			}
			b, c := baseOut, candOut
			if tc.baseline.Items != 0 {
				b = tc.baseline
			}
			if tc.candidate.Items != 0 {
				c = tc.candidate
			}
			a := goodAttribution()
			if tc.attr != nil {
				a = *tc.attr
			}
			d := Evaluate(r, b, c, WithAttribution(a))
			if d.Verdict != tc.verdict {
				t.Fatalf("verdict = %s (%s), want %s; gates %+v", d.Verdict, d.Summary, tc.verdict, d.Gates)
			}
			if tc.gate != "" {
				if got := gateVerdict(d, tc.gate); got != tc.gateWant {
					t.Errorf("gate %s = %s, want %s", tc.gate, got, tc.gateWant)
				}
			}
		})
	}
}

// No candidate items at all: the production false-success gate has no data
// (NOT_EVALUATED → INCOMPLETE), it does not pass.
func TestEvaluate_ProdFalseSuccessWithoutItemsIsNotEvaluated(t *testing.T) {
	d := Evaluate(goodReport(), ProdOutcomes{Items: 100, DefectDataItems: 100}, ProdOutcomes{}, WithAttribution(goodAttribution()))
	if got := gateVerdict(d, GateProdFalseSuccess); got != GateNotEvaluated {
		t.Errorf("production_false_success with 0 candidate items = %s, want NOT_EVALUATED", got)
	}
	if d.Verdict != VerdictIncomplete {
		t.Errorf("verdict %s, want INCOMPLETE", d.Verdict)
	}
	d = Evaluate(goodReport(), ProdOutcomes{Items: 100, DefectDataItems: 100}, ProdOutcomes{Items: 3, DefectDataItems: 3}, WithAttribution(goodAttribution()))
	if got := gateVerdict(d, GateProdFalseSuccess); got != GatePass {
		t.Errorf("with candidate items = %s, want PASS", got)
	}
}

// A gate without data never approves, and the summary never claims every
// gate passed.
func TestEvaluate_NotEvaluatedIsIncomplete(t *testing.T) {
	d := Evaluate(goodReport(), ProdOutcomes{Items: 100}, ProdOutcomes{Items: 10}, WithAttribution(goodAttribution()))
	if d.Verdict != VerdictIncomplete {
		t.Fatalf("verdict %s, want INCOMPLETE", d.Verdict)
	}
	if strings.Contains(d.Summary, "every") && strings.Contains(d.Summary, "passed") {
		t.Errorf("summary claims everything passed: %q", d.Summary)
	}
	for _, g := range []string{GateCriticalDefect, GateMajorDefectRate} {
		if !strings.Contains(d.Summary, g) {
			t.Errorf("summary does not name the missing gate %s: %q", g, d.Summary)
		}
	}
	if len(d.NotEvaluated) != 2 {
		t.Errorf("not evaluated: %v", d.NotEvaluated)
	}
}

// Without the attribution check a report is never an approval.
func TestEvaluate_NoAttributionIsNeverApproved(t *testing.T) {
	d := Evaluate(goodReport(), ProdOutcomes{}, ProdOutcomes{})
	if d.Verdict != VerdictMoreTrials || gateVerdict(d, GateShadowAttribution) != GateMoreTrials {
		t.Fatalf("verdict %s, attribution %s", d.Verdict, gateVerdict(d, GateShadowAttribution))
	}
	if Evaluate(nil, ProdOutcomes{}, ProdOutcomes{}).Verdict != VerdictMoreTrials {
		t.Error("no report must not be approvable")
	}
}

// Cost and latency never override a correctness gate: a blocked candidate
// that is 900x cheaper stays blocked, and a far more expensive passing one
// is still approved.
func TestEvaluate_CostNeverOverrides(t *testing.T) {
	r := goodReport()
	r.Competitors[cand] = CompetitorSummary{Efficiency: json.RawMessage(`{"cost_total_usd_all": 0.001, "median_wall_s_passing": 1}`)}
	r.Blocked[0].CorrectBlocks = 0
	if d := Evaluate(r, ProdOutcomes{}, ProdOutcomes{}, WithAttribution(goodAttribution())); d.Verdict != VerdictBlocked {
		t.Errorf("cheap regressing candidate: %s", d.Verdict)
	}
	if d := Evaluate(goodReport(), ProdOutcomes{Items: 5, DefectDataItems: 5}, ProdOutcomes{Items: 5, DefectDataItems: 5}, WithAttribution(goodAttribution())); d.Verdict != VerdictApproved || !strings.Contains(d.CostNote, "never consulted") {
		t.Errorf("expensive passing candidate: %s / %q", d.Verdict, d.CostNote)
	}
}

// The decision keeps the raw inputs (counts, intervals, the report).
func TestEvaluate_DecisionStoresRawInputs(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	base := ProdOutcomes{SnapshotID: "b", Items: 7, Released: 5}
	d := Evaluate(goodReport(), base, ProdOutcomes{}, WithAttribution(goodAttribution()), WithClock(func() time.Time { return now }))
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"ci95_pp":[1,8]`, `"baseline_outcomes":{"snapshot_id":"b"`, `"evaluated_at":"2026-10-03T12:00:00Z"`, `"policy":"rollout-thresholds-v1"`, `"correct_blocks":3`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("decision JSON lacks %s", want)
		}
	}
}

// The Go gate agrees with scripts/benchmark-report.py on the M4-03 sample
// fixture: it reads the report's interval and verdict instead of
// recomputing them.
func TestEvaluate_AgreesWithTheReportOnTheSampleFixture(t *testing.T) {
	for _, tc := range []struct {
		file     string
		passRate string
		sealedFS string
	}{
		{"sample-report-sealed.json", GateMoreTrials, GatePass},
		{"sample-report-all.json", GatePass, GatePass},
	} {
		data, err := os.ReadFile(filepath.Join("testdata", tc.file))
		if err != nil {
			t.Fatal(err)
		}
		r, err := ParseReport(data)
		if err != nil {
			t.Fatal(err)
		}
		d := Evaluate(r, ProdOutcomes{}, ProdOutcomes{}, WithAttribution(goodAttribution()))
		if got := gateVerdict(d, GatePassRate); got != tc.passRate {
			t.Errorf("%s: pass-rate gate %s, report says %s → want %s", tc.file, got, r.Paired.Verdict, tc.passRate)
		}
		if got := gateVerdict(d, GateSealedFalseSuccess); got != tc.sealedFS {
			t.Errorf("%s: sealed false-success gate %s", tc.file, got)
		}
		// The fixture is the primary (AE vs Claude CLI) comparison, so it
		// is never evidence for a rollout.
		if d.Verdict == VerdictApproved {
			t.Errorf("%s: a primary-comparison report approved a rollout", tc.file)
		}
	}
}

func TestParseReport_RejectsNonReports(t *testing.T) {
	for _, in := range []string{`{}`, `{"candidate":"a","baseline":"b"}`, `not json`} {
		if _, err := ParseReport([]byte(in)); err == nil {
			t.Errorf("ParseReport(%q) accepted", in)
		}
	}
}

func TestNewcombeDifference(t *testing.T) {
	low, high := NewcombeDifference(30, 60, 10, 200)
	if !(low > 0.30 && low < 0.40 && high > 0.50 && high < 0.60) {
		t.Errorf("Newcombe(30/60 - 10/200) = [%.3f, %.3f]", low, high)
	}
	low, high = NewcombeDifference(5, 10, 5, 10)
	if !(low < 0 && high > 0) {
		t.Errorf("equal proportions interval [%.3f, %.3f] must contain 0", low, high)
	}
}

func TestCanTransition(t *testing.T) {
	for _, tc := range []struct {
		from, to string
		ok       bool
	}{
		{StatusProposed, StatusShadow, true},
		{StatusShadow, StatusEvaluating, true},
		{StatusEvaluating, StatusApproved, true},
		{StatusProposed, StatusApproved, false},
		{StatusBlocked, StatusApproved, false},
		{StatusApproved, StatusRolledBack, true},
		{StatusRolledBack, StatusApproved, false},
	} {
		if got := CanTransition(tc.from, tc.to); got != tc.ok {
			t.Errorf("%s → %s = %t, want %t", tc.from, tc.to, got, tc.ok)
		}
	}
}

func TestEquivalent_IgnoresCodexVersionOnly(t *testing.T) {
	a := map[string]any{"agent_type": "implementation-agent", "codex_version": json.Number("0"), "model": map[string]any{"model_id": "m"}}
	b := map[string]any{"agent_type": "implementation-agent", "codex_version": json.Number("7"), "model": map[string]any{"model_id": "m"}}
	c := map[string]any{"agent_type": "implementation-agent", "codex_version": json.Number("0"), "model": map[string]any{"model_id": "other"}}
	if !Equivalent(a, b) {
		t.Error("documents differing only in codex_version must be equivalent")
	}
	if Equivalent(a, c) {
		t.Error("a different model is a different configuration")
	}
}
