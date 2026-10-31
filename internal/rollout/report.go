package rollout

import (
	"encoding/json"
	"fmt"
)

// Report is the subset of the scripts/benchmark-report.py --json summary
// (M4-03) the evaluator reads. The statistics are the report's: Evaluate
// never recomputes a pass rate, interval or bootstrap; it reads the numbers
// and verdicts the report wrote, so the Go gate and the script cannot
// disagree about them.
type Report struct {
	Comparison       string                       `json:"comparison"`
	Title            string                       `json:"title"`
	Candidate        string                       `json:"candidate"`
	Baseline         string                       `json:"baseline"`
	PartitionFilter  *string                      `json:"partition_filter"`
	RunIDs           []string                     `json:"run_ids"`
	TrialsPerTaskMax int                          `json:"trials_per_task_max"`
	Attempts         int                          `json:"attempts"`
	Competitors      map[string]CompetitorSummary `json:"competitors"`
	// Strata: field -> value -> competitor -> rate.
	Strata         map[string]map[string]map[string]RateSummary `json:"strata"`
	Blocked        []BlockedRow                                 `json:"blocked"`
	Paired         Paired                                       `json:"paired"`
	Gates          []ReportGate                                 `json:"gates"`
	OverallVerdict string                                       `json:"overall_verdict"`
	// CandidateFalseSuccessUnknownPartition lists candidate false successes
	// whose partition is missing or unrecognised; the report gates them as
	// sealed (fail closed).
	CandidateFalseSuccessUnknownPartition []json.RawMessage `json:"candidate_false_success_unknown_partition"`
	NotEvaluated                          []string          `json:"not_evaluated"`
}

// CompetitorSummary is one competitor's block of the report.
type CompetitorSummary struct {
	Label                      string          `json:"label"`
	PassRate                   RateSummary     `json:"pass_rate"`
	FalseSuccess               int             `json:"false_success"`
	FalseSuccessSealed         int             `json:"false_success_sealed"`
	FalseSuccessDevelopment    int             `json:"false_success_development"`
	FalseSuccessOtherPartition int             `json:"false_success_other_partition"`
	UnsafeEditOnBlocked        int             `json:"unsafe_edit_on_blocked"`
	AttemptsAll                int             `json:"attempts_all"`
	Efficiency                 json.RawMessage `json:"efficiency,omitempty"`
}

// RateSummary is a pass rate with its Wilson interval (nil when no attempts).
type RateSummary struct {
	Passes   int       `json:"passes"`
	Attempts int       `json:"attempts"`
	Tasks    int       `json:"tasks"`
	Rate     *float64  `json:"rate"`
	Wilson95 []float64 `json:"wilson95"`
}

// BlockedRow is one row of the blocked/underspecified (safety/policy)
// table: one task, one competitor.
type BlockedRow struct {
	TaskID              string   `json:"task_id"`
	Competitor          string   `json:"competitor"`
	CorrectBlocks       int      `json:"correct_blocks"`
	Trials              int      `json:"trials"`
	FalseSuccess        int      `json:"false_success"`
	UnsafeEditOnBlocked int      `json:"unsafe_edit_on_blocked"`
	WrapperOutcomes     []string `json:"wrapper_outcomes"`
}

// Paired is the paired comparison candidate minus baseline.
type Paired struct {
	PairedTasks      int          `json:"paired_tasks"`
	Seed             int64        `json:"seed"`
	Resamples        int          `json:"resamples"`
	Tasks            []PairedTask `json:"tasks"`
	UnpairedTasks    []string     `json:"unpaired_tasks"`
	MeanDifferencePP *float64     `json:"mean_difference_pp"`
	CI95PP           []float64    `json:"ci95_pp"`
	MinTrialsPerTask int          `json:"min_trials_per_task"`
	Verdict          string       `json:"verdict"`
	Note             *string      `json:"note"`
	Reason           string       `json:"reason"`
}

// PairedTask is one task's paired row.
type PairedTask struct {
	TaskID          string  `json:"task_id"`
	CandidatePasses int     `json:"candidate_passes"`
	CandidateTrials int     `json:"candidate_trials"`
	BaselinePasses  int     `json:"baseline_passes"`
	BaselineTrials  int     `json:"baseline_trials"`
	Difference      float64 `json:"difference"`
}

// ReportGate is one gate verdict the report evaluated.
type ReportGate struct {
	Gate    string  `json:"gate"`
	Verdict string  `json:"verdict"`
	Reason  string  `json:"reason"`
	Note    *string `json:"note,omitempty"`
}

// Report verdict strings (scripts/benchmark-report.py VERDICT_*).
const (
	ReportPass       = "PASS"
	ReportBlock      = "BLOCK"
	ReportMoreTrials = "MORE TRIALS NEEDED"
)

// ParseReport decodes a --json summary and checks that the fields the gate
// depends on are present.
func ParseReport(data []byte) (*Report, error) {
	var r Report
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("rollout: parse benchmark report: %w", err)
	}
	if r.Candidate == "" || r.Baseline == "" {
		return nil, fmt.Errorf("rollout: benchmark report names no candidate/baseline (not a benchmark-report.py --json summary?)")
	}
	if _, ok := r.Competitors[r.Candidate]; !ok {
		return nil, fmt.Errorf("rollout: benchmark report has no summary for the candidate %q", r.Candidate)
	}
	if _, ok := r.Competitors[r.Baseline]; !ok {
		return nil, fmt.Errorf("rollout: benchmark report has no summary for the baseline %q", r.Baseline)
	}
	if r.Paired.Verdict == "" {
		return nil, fmt.Errorf("rollout: benchmark report has no paired verdict")
	}
	return &r, nil
}

// gate returns the report's verdict for name.
func (r *Report) gate(name string) (ReportGate, bool) {
	for _, g := range r.Gates {
		if g.Gate == name {
			return g, true
		}
	}
	return ReportGate{}, false
}
