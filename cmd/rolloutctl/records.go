package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/rollout"
)

// resultRecord is the part of a code-implementation-benchmark.py record
// attribution reads.
type resultRecord struct {
	RunID       string         `json:"run_id"`
	TaskID      string         `json:"task_id"`
	Competitor  string         `json:"competitor"`
	Trial       int            `json:"trial"`
	Role        string         `json:"role"`
	Versions    map[string]any `json:"versions"`
	AgentOutput *struct {
		Versions map[string]any `json:"versions"`
	} `json:"agent_output"`
}

// snapshotID is the work item's recorded snapshot as the AE wrapper
// reported it (versions.config_snapshot_id; flattened by the runner, else
// inside agent_output).
func (r resultRecord) snapshotID() string {
	for _, v := range []map[string]any{r.Versions, agentVersions(r)} {
		if s, ok := v["config_snapshot_id"].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func agentVersions(r resultRecord) map[string]any {
	if r.AgentOutput == nil {
		return nil
	}
	return r.AgentOutput.Versions
}

// loadRecords reads runner output: the final JSON document ({"results":
// [...]}) or the incremental JSONL file. Exact duplicates (run, task,
// competitor, trial) are dropped, as the report does.
func loadRecords(paths []string) ([]resultRecord, error) {
	var out []resultRecord
	seen := map[string]bool{}
	add := func(r resultRecord) {
		key := fmt.Sprintf("%s|%s|%s|%d", r.RunID, r.TaskID, r.Competitor, r.Trial)
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, r)
	}
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		trimmed := bytes.TrimSpace(data)
		if len(trimmed) > 0 && trimmed[0] == '{' && !strings.HasSuffix(p, ".jsonl") {
			var doc struct {
				Results []resultRecord `json:"results"`
			}
			if err := json.Unmarshal(trimmed, &doc); err != nil {
				return nil, fmt.Errorf("%s: %w", p, err)
			}
			for _, r := range doc.Results {
				add(r)
			}
			continue
		}
		sc := bufio.NewScanner(bytes.NewReader(data))
		sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
		line := 0
		for sc.Scan() {
			line++
			text := strings.TrimSpace(sc.Text())
			if text == "" {
				continue
			}
			var r resultRecord
			if err := json.Unmarshal([]byte(text), &r); err != nil {
				return nil, fmt.Errorf("%s:%d: %w", p, line, err)
			}
			add(r)
		}
		if err := sc.Err(); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
	}
	return out, nil
}

// recordRefs maps records to attribution sides: the runner's role field,
// else the report's competitor ids. Records of other competitors are
// dropped.
func recordRefs(records []resultRecord, candidate, baseline string) []rollout.RecordRef {
	var refs []rollout.RecordRef
	for _, r := range records {
		side := r.Role
		if side != "candidate" && side != "baseline" {
			switch r.Competitor {
			case candidate:
				side = "candidate"
			case baseline:
				side = "baseline"
			default:
				continue
			}
		}
		refs = append(refs, rollout.RecordRef{TaskID: r.TaskID, Trial: r.Trial, Side: side, SnapshotID: r.snapshotID()})
	}
	return refs
}

// shellQuote quotes s for the runner's `bash -lc` command line.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// shadowPlan is the benchmark run `rolloutctl shadow` performs.
type shadowPlan struct {
	Python    string
	Manifest  string
	Trials    int
	OutputDir string
	Wrapper   string
	Candidate string
	Baseline  string
	RolloutID string
	// ScratchProduct is the benchmark scratch product (AE_PRODUCT_ID) the
	// shadow intakes target; "" leaves AE_PRODUCT_ID to the environment.
	ScratchProduct string
	Timeout        int
	ResultsOut     string
	ReportOut      string
}

const (
	candidateName = "ae-candidate"
	baselineName  = "ae-baseline"
)

// competitorCommand is one side's wrapper command: the wrapper with
// AE_CONFIG_SNAPSHOT_ID (and the rollout id) in its environment.
func (p shadowPlan) competitorCommand(snapshotID string) string {
	cmd := fmt.Sprintf("AE_CONFIG_SNAPSHOT_ID=%s AE_ROLLOUT_ID=%s %s", shellQuote(snapshotID), shellQuote(p.RolloutID), p.Wrapper)
	if p.ScratchProduct != "" {
		// The gateway honours a snapshot only on a benchmark scratch product.
		cmd = "AE_PRODUCT_ID=" + shellQuote(p.ScratchProduct) + " " + cmd
	}
	return cmd
}

// runnerArgs are the scripts/code-implementation-benchmark.py arguments.
func (p shadowPlan) runnerArgs() []string {
	args := []string{"scripts/code-implementation-benchmark.py",
		"--manifest", p.Manifest,
		"--trials", fmt.Sprint(p.Trials),
		"--comparison", rollout.RolloutComparison,
		"--candidate-name", candidateName,
		"--baseline-name", baselineName,
		"--candidate-command", p.competitorCommand(p.Candidate),
		"--baseline-command", p.competitorCommand(p.Baseline),
		"--output", p.ResultsOut,
	}
	if p.Timeout > 0 {
		args = append(args, "--timeout", fmt.Sprint(p.Timeout))
	}
	return args
}

// reportArgs are the scripts/benchmark-report.py arguments of the
// release-gating report (--partition sealed).
func reportArgs(manifest, reportOut string, results []string) []string {
	args := []string{"scripts/benchmark-report.py", "--manifest", manifest,
		"--partition", rollout.ReleasePartition, "--json", reportOut}
	return append(args, results...)
}
