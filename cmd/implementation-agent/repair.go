package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/controller"
	"github.com/autonomousengine/autonomous-engine/internal/failures"
	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
	"github.com/autonomousengine/autonomous-engine/internal/llm/tools"
	"github.com/autonomousengine/autonomous-engine/internal/logging"
	"github.com/autonomousengine/autonomous-engine/internal/metrics"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Fingerprint-bounded repair (M3-06). The pre-commit check run that
// generation does before its commit is a loop: run the checks (the
// impact-selected fast checks, M3-04, or the full test command), and on
// failure normalise the output into failure records (internal/failures),
// record them as a run event, and give the model one repair attempt with
// the records — grouped by file, with a pointer to the stored raw output —
// instead of the raw log. A fingerprint that fails again in the run right
// after a repair attempt stops the loop deterministically
// (repeated_failure); otherwise at most maxRepairAttempts repairs run
// (repairs_exhausted). A record that contradicts the plan (a missing
// dependency or environment, a missing planned file or symbol) first gets
// a one-turn, tool-less plan revision, recorded as a note linked to the
// record's fingerprint and the check run's call id. Both D4 budgets are
// checked before every check run and every repair.

const (
	// maxRepairAttempts bounds the repairs per pre-commit loop (unchanged
	// from the blind loop it replaces; now fingerprint-aware).
	maxRepairAttempts = 3
	// repairIterations is each repair's tool-loop iteration cap.
	repairIterations = 10
	// checkRunTimeout bounds one pre-commit check run.
	checkRunTimeout = 6 * time.Minute
	// preCommitTool names the agent's check runs in implementation_run_events.
	preCommitTool = "pre_commit_checks"
	// maxRevisionsPerLoop bounds plan revisions per pre-commit loop.
	maxRevisionsPerLoop = 3

	stopPassed           = "passed"
	stopRepeatedFailure  = "repeated_failure"
	stopRepairsExhausted = "repairs_exhausted"
	stopBudgetExhausted  = "budget_exhausted"
	// stopNotRepairable: every record of the failing run is owned by a
	// dependency or the environment (or is a tooling failure).
	stopNotRepairable = "not_repairable"
)

func anyRetryable(rs []failures.Record) bool {
	for _, r := range rs {
		if r.Retryable {
			return true
		}
	}
	return false
}

var (
	repairAttemptsFixed    = metrics.NewLabeledCounter("ae_repair_attempts_total", "Repair attempts in the pre-commit loop by what the next check run showed", "outcome", "fixed")
	repairAttemptsChanged  = metrics.NewLabeledCounter("ae_repair_attempts_total", "Repair attempts in the pre-commit loop by what the next check run showed", "outcome", "changed")
	repairAttemptsRepeated = metrics.NewLabeledCounter("ae_repair_attempts_total", "Repair attempts in the pre-commit loop by what the next check run showed", "outcome", "repeated_failure")
	repairAttemptsBudget   = metrics.NewLabeledCounter("ae_repair_attempts_total", "Repair attempts in the pre-commit loop by what the next check run showed", "outcome", "budget_exhausted")
	repeatedFailureStops   = metrics.NewCounter("ae_repeated_failure_stops_total", "Pre-commit repair loops stopped because the same failure fingerprint survived a repair attempt")
)

func countRepairAttempt(outcome string) {
	switch outcome {
	case "fixed":
		repairAttemptsFixed.Inc()
	case "changed":
		repairAttemptsChanged.Inc()
	case stopRepeatedFailure:
		repairAttemptsRepeated.Inc()
	case stopBudgetExhausted:
		repairAttemptsBudget.Inc()
	}
}

// outputStorer stores a raw output and returns its reference
// (completionSink satisfies it).
type outputStorer interface {
	StoreOutput(name string, content []byte) (string, error)
}

// repairInputs is what the repair loop needs.
type repairInputs struct {
	logger   *logging.Logger
	taskID   uuid.UUID
	workID   uuid.UUID
	worktree string
	// command describes the checks for records and events when results
	// is nil (the full test command).
	command string
	// runChecks runs the checks once; a non-nil error is a failing run.
	runChecks func(ctx context.Context) ([]byte, error)
	// results, when set, returns the last run's failing checks one per
	// command (fastChecks.failureResults) so each record carries its own
	// command and exit code; nil parses the combined output.
	results func() []failures.Result
	// lang hints the parser ("go", "typescript", "python"); "" infers it.
	lang         func() string
	llm          adapter.ModelAdapter
	executor     *tools.Executor
	pool         *pgxpool.Pool
	run          *controller.Run
	outputs      outputStorer
	budget       *attemptBudget
	plannedFiles []string
	// timeout bounds one check run (0: checkRunTimeout).
	timeout time.Duration
	// detailKey is the implementation_runs.detail key of the loop's state
	// ("" = "repair"; a continuation pass's gate uses its own, R11 S3).
	detailKey string
}

func (in repairInputs) stateKey() string {
	if in.detailKey != "" {
		return in.detailKey
	}
	return "repair"
}

// checkRunRecord is one entry of detail.repair.check_runs.
type checkRunRecord struct {
	Run          int      `json:"run"`
	CallID       string   `json:"call_id"`
	Seq          int64    `json:"seq,omitempty"`
	Passed       bool     `json:"passed"`
	Fingerprints []string `json:"fingerprints,omitempty"`
	New          []string `json:"new,omitempty"`
	Repeated     []string `json:"repeated,omitempty"`
	RawRef       string   `json:"raw_ref,omitempty"`
}

// repairAttemptRecord is one entry of detail.repair.attempts.
type repairAttemptRecord struct {
	Attempt int `json:"attempt"`
	// AfterCallID is the failing check run the repair answered.
	AfterCallID  string   `json:"after_call_id"`
	Fingerprints []string `json:"fingerprints"`
	// Outcome is what the next check run showed: fixed, changed,
	// repeated_failure; or budget_exhausted / error when it never ran.
	Outcome string `json:"outcome,omitempty"`
	Error   string `json:"error,omitempty"`
}

// planRevision is one entry of detail.repair.revisions and the detail of
// its run event.
type planRevision struct {
	Type           string `json:"type"`
	Seq            int64  `json:"seq,omitempty"`
	EvidenceCallID string `json:"evidence_call_id"`
	EvidenceSeq    int64  `json:"evidence_seq,omitempty"`
	Fingerprint    string `json:"fingerprint"`
	Reason         string `json:"reason"`
	Revision       string `json:"revision"`
	Category       string `json:"category,omitempty"`
	Location       string `json:"location,omitempty"`
}

// failureSummary is one fingerprint in detail.repair.failures.
type failureSummary struct {
	failures.Record
	Count      int    `json:"count"`
	FirstRun   int    `json:"first_run"`
	LastRun    int    `json:"last_run"`
	LastCallID string `json:"last_call_id,omitempty"`
}

// repairState is implementation_runs.detail.repair.
type repairState struct {
	MaxRepairs int                   `json:"max_repairs"`
	Repairs    int                   `json:"repairs"`
	Runs       int                   `json:"runs"`
	StopReason string                `json:"stop_reason,omitempty"`
	StopDetail string                `json:"stop_detail,omitempty"`
	Failures   []failureSummary      `json:"failures"`
	Attempts   []repairAttemptRecord `json:"attempts"`
	CheckRuns  []checkRunRecord      `json:"check_runs"`
	Revisions  []planRevision        `json:"revisions"`
}

// repairResult is the loop's outcome.
type repairResult struct {
	passed bool
	output string
	// stop is stopPassed or why the loop gave up.
	stop       string
	stopDetail string
	budget     *budgetExhaustedError
	tracker    *failures.Tracker
	// last* describe the last failing run (nil/empty when it passed).
	lastRecords []failures.Record
	lastResults []failures.Result
	lastRawRef  string
	// lastRunCurrent: no repair has touched the tree since the last
	// failing run, so its checks describe the tree a stop commits.
	lastRunCurrent bool
	state          repairState
}

// runRepairLoop runs the checks, repairing between failing runs, until
// they pass or a stop rule fires.
func runRepairLoop(ctx context.Context, in repairInputs) repairResult {
	timeout := in.timeout
	if timeout <= 0 {
		timeout = checkRunTimeout
	}
	tracker := failures.NewTracker()
	res := repairResult{tracker: tracker, state: repairState{MaxRepairs: maxRepairAttempts, Failures: []failureSummary{}, Attempts: []repairAttemptRecord{}, CheckRuns: []checkRunRecord{}, Revisions: []planRevision{}}}
	revised := map[string]bool{}
	afterRepair := false
	finish := func(stop, detail string) repairResult {
		res.stop, res.stopDetail = stop, detail
		res.state.StopReason, res.state.StopDetail = stop, detail
		res.state.Failures = summarize(tracker)
		in.run.MergeDetail(context.Background(), in.stateKey(), res.state)
		in.budget.persist(context.Background(), true)
		fields := map[string]interface{}{"task_id": in.taskID.String(), "stop": stop, "repairs": res.state.Repairs, "runs": res.state.Runs}
		if detail != "" {
			fields["detail"] = detail
		}
		if stop == stopPassed {
			in.info(ctx, "pre-commit checks passed", fields)
		} else {
			in.warn(ctx, "pre-commit repair loop stopped", fields)
		}
		return res
	}
	budgetStop := func() (repairResult, bool) {
		if e := in.budget.check(); e != nil {
			res.budget = e
			if afterRepair {
				res.state.Attempts[len(res.state.Attempts)-1].Outcome = stopBudgetExhausted
				countRepairAttempt(stopBudgetExhausted)
			}
			return finish(stopBudgetExhausted, e.Error()), true
		}
		return repairResult{}, false
	}

	for runN := 1; ; runN++ {
		if r, stop := budgetStop(); stop {
			return r
		}
		in.budget.activity()
		runCtx, cancel := context.WithTimeout(ctx, timeout)
		out, err := in.runChecks(runCtx)
		timedOut := errors.Is(runCtx.Err(), context.DeadlineExceeded)
		cancel()
		res.output = string(out)
		res.state.Runs = runN
		callID := fmt.Sprintf("precommit-%s-%d", shortID(in.taskID), runN)

		if err == nil {
			seq := in.run.Append(ctx, controller.EventRow{Kind: controller.KindTool, ToolCallID: callID, Tool: preCommitTool,
				Command: in.describeCommand(nil), ExitCode: intPtr(0), Note: "pre-commit checks passed",
				Detail: mustJSON(map[string]any{"call_id": callID, "run": runN, "passed": true})})
			res.state.CheckRuns = append(res.state.CheckRuns, checkRunRecord{Run: runN, CallID: callID, Seq: seq, Passed: true})
			if afterRepair {
				res.state.Attempts[len(res.state.Attempts)-1].Outcome = "fixed"
				countRepairAttempt("fixed")
			}
			res.passed = true
			res.lastRecords, res.lastResults, res.lastRawRef = nil, nil, ""
			return finish(stopPassed, "")
		}

		rawRef := ""
		if in.outputs != nil {
			if ref, serr := in.outputs.StoreOutput(fmt.Sprintf("pre-commit-checks-run-%d-output", runN), capOutput(out)); serr == nil {
				rawRef = ref
			} else {
				in.warn(ctx, "cannot store the pre-commit check output — records carry no raw ref", map[string]interface{}{"task_id": in.taskID.String(), "err": serr.Error()})
			}
		}
		results := in.failingResults(out, err, timedOut)
		lang := ""
		if in.lang != nil {
			lang = in.lang()
		}
		var records []failures.Record
		for _, r := range results {
			r.RawRef = rawRef
			records = append(records, failures.Parse(lang, r)...)
		}
		obs := tracker.Observe(records, callID, afterRepair)
		res.lastRecords, res.lastResults, res.lastRawRef, res.lastRunCurrent = records, results, rawRef, true

		fps := fingerprintsOf(records)
		seq := in.run.Append(ctx, controller.EventRow{Kind: controller.KindTool, ToolCallID: callID, Tool: preCommitTool,
			Command: in.describeCommand(results), ExitCode: firstExit(results),
			Note: fmt.Sprintf("pre-commit checks failed: %d failure record(s), %d new, %d repeated after a repair", len(records), len(obs.New), len(obs.Repeated)),
			Detail: mustJSON(map[string]any{"call_id": callID, "run": runN, "passed": false, "raw_ref": rawRef, "records": records,
				"new": obs.New, "repeated": obs.Repeated, "after_repair": afterRepair})})
		res.state.CheckRuns = append(res.state.CheckRuns, checkRunRecord{Run: runN, CallID: callID, Seq: seq, Fingerprints: fps, New: obs.New, Repeated: obs.Repeated, RawRef: rawRef})
		in.info(ctx, "pre-commit checks failed", map[string]interface{}{"task_id": in.taskID.String(), "run": runN, "records": len(records), "new": len(obs.New), "repeated": len(obs.Repeated), "call_id": callID})

		if afterRepair {
			outcome := "changed"
			if len(obs.Repeated) > 0 {
				outcome = stopRepeatedFailure
			}
			res.state.Attempts[len(res.state.Attempts)-1].Outcome = outcome
			countRepairAttempt(outcome)
		}
		if len(obs.Repeated) > 0 {
			repeatedFailureStops.Inc()
			e, _ := tracker.Entry(obs.Repeated[0])
			return finish(stopRepeatedFailure, fmt.Sprintf("%s failed again after repair attempt %d (%s)", failures.Short(e.Record.Fingerprint), res.state.Repairs, describeRecord(e.Record)))
		}
		if res.state.Repairs >= maxRepairAttempts || in.llm == nil {
			return finish(stopRepairsExhausted, fmt.Sprintf("checks still failing after %d repair attempt(s)", res.state.Repairs))
		}
		if r, stop := budgetStop(); stop {
			return r
		}

		// Plan revisions for records that contradict the plan.
		var revisionNotes []string
		for _, rec := range records {
			if len(res.state.Revisions) >= maxRevisionsPerLoop {
				break
			}
			if revised[rec.Fingerprint] {
				continue
			}
			reason := failures.Contradiction(rec, in.plannedFiles)
			if reason == "" {
				continue
			}
			revised[rec.Fingerprint] = true
			rev, ok := in.revisePlan(ctx, rec, reason, callID, seq)
			if r, stop := budgetStop(); stop {
				return r
			}
			if ok {
				res.state.Revisions = append(res.state.Revisions, rev)
				revisionNotes = append(revisionNotes, rev.Revision)
			}
		}

		// Nothing a code repair can fix (every record is a dependency,
		// environment or tooling failure): stop now, after the plan
		// revision, instead of spending a repair on it.
		if !anyRetryable(records) {
			return finish(stopNotRepairable, fmt.Sprintf("none of the %d failure record(s) is fixable by editing the change (%s)", len(records), describeRecord(records[0])))
		}

		// One repair attempt with the structured records.
		res.state.Repairs++
		res.lastRunCurrent = false
		attempt := repairAttemptRecord{Attempt: res.state.Repairs, AfterCallID: callID, Fingerprints: fps}
		counts := map[string]int{}
		for _, e := range tracker.Entries() {
			counts[e.Record.Fingerprint] = e.Count
		}
		fixReq := adapter.CompletionRequest{
			Messages: []adapter.ChatMessage{
				{Role: adapter.RoleSystem, Content: repairSystemPrompt},
				{Role: adapter.RoleUser, Content: repairPrompt(records, counts, rawRef, revisionNotes, res.state.Repairs)},
			},
			Tools:      verifyTools(),
			ToolChoice: "auto",
			MaxTokens:  4096,
		}
		in.info(ctx, fmt.Sprintf("repair attempt %d/%d with %d failure record(s)", res.state.Repairs, maxRepairAttempts, len(records)), map[string]interface{}{"task_id": in.taskID.String(), "call_id": callID})
		_, fixErr := runImplementationLoop(ctx, in.llm, fixReq, in.executor, repairIterations, in.pool, &in.workID, in.plannedFiles)
		in.budget.activity() // the loop returned: nothing waits in-process for a pending prompt
		res.state.Attempts = append(res.state.Attempts, attempt)
		afterRepair = true
		if fixErr != nil {
			res.state.Attempts[len(res.state.Attempts)-1].Error = truncate(fixErr.Error(), 300)
			if e := in.budget.fromLoopError(fixErr); e != nil {
				res.budget = e
				res.state.Attempts[len(res.state.Attempts)-1].Outcome = stopBudgetExhausted
				countRepairAttempt(stopBudgetExhausted)
				return finish(stopBudgetExhausted, e.Error())
			}
			in.warn(ctx, "repair attempt's tool loop ended with an error — re-running the checks", map[string]interface{}{"task_id": in.taskID.String(), "err": fixErr.Error()})
		}
		res.state.Failures = summarize(tracker)
		in.run.MergeDetail(ctx, in.stateKey(), res.state)
	}
}

const repairSystemPrompt = "You are fixing code that failed its checks. The failures below are structured records parsed from the check output, grouped by file. " +
	"Read the named files, fix the cause, and re-run the failing command with run_command to confirm. Do not weaken or delete tests to make them pass."

// repairPrompt is the repair attempt's user message: the records grouped
// by file, the raw output's reference, and any plan revision.
func repairPrompt(records []failures.Record, counts map[string]int, rawRef string, revisions []string, attempt int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Repair attempt %d of %d. The pre-commit checks failed with %d failure record(s):\n\n", attempt, maxRepairAttempts, len(records))
	b.WriteString(failures.FormatGrouped(records, counts))
	if rawRef != "" {
		fmt.Fprintf(&b, "\nThe full raw output is stored as %s (not shown here).\n", rawRef)
	}
	if len(revisions) > 0 {
		b.WriteString("\nThe evidence contradicted the plan. Your revised plan:\n")
		for _, r := range revisions {
			fmt.Fprintf(&b, "- %s\n", r)
		}
	}
	b.WriteString("\nA failure whose fingerprint appears again after this repair stops the task, so fix the cause rather than the symptom. " +
		"Records marked \"not fixable by editing the change\" are dependency or environment problems: do not paper over them in code.")
	return b.String()
}

const planRevisionSystemPrompt = "You maintain an implementation plan. A check failure contradicts an assumption of the plan. " +
	"Reply with exactly one short line that starts with \"revised plan:\" and states how the plan changes. No tool calls, no other text."

// revisePlan asks the model for one short, tool-less plan revision for rec
// and records it as a note event linked to the evidence (the record's
// fingerprint and the check run's call id and seq).
func (in repairInputs) revisePlan(ctx context.Context, rec failures.Record, reason, callID string, evidenceSeq int64) (planRevision, bool) {
	if in.llm == nil {
		return planRevision{}, false
	}
	var u strings.Builder
	fmt.Fprintf(&u, "Planned files: %s\n\nContradicting evidence (%s, fingerprint %s, check run %s): %s\n", strings.Join(in.plannedFiles, ", "), reason, failures.Short(rec.Fingerprint), callID, describeRecord(rec))
	fmt.Fprintf(&u, "message: %s\n", rec.Message)
	req := adapter.CompletionRequest{
		Messages: []adapter.ChatMessage{
			{Role: adapter.RoleSystem, Content: planRevisionSystemPrompt},
			{Role: adapter.RoleUser, Content: u.String()},
		},
		MaxTokens:   300,
		Temperature: 0.2,
	}
	resp, err := tools.RunToolLoop(ctx, in.llm, req, in.executor, 1, in.pool, &in.workID)
	in.budget.activity()
	if err != nil || resp == nil {
		if err != nil {
			in.budget.fromLoopError(err)
			in.warn(ctx, "plan revision turn failed", map[string]interface{}{"task_id": in.taskID.String(), "err": err.Error()})
		}
		return planRevision{}, false
	}
	text := revisionLine(resp.Content)
	rev := planRevision{Type: "plan_revision", EvidenceCallID: callID, EvidenceSeq: evidenceSeq, Fingerprint: rec.Fingerprint,
		Reason: reason, Revision: text, Category: string(rec.Category), Location: rec.Location()}
	rev.Seq = in.run.Append(ctx, controller.EventRow{Kind: controller.KindNote, ToolCallID: callID,
		Note: "plan_revision: " + text, Detail: mustJSON(rev)})
	in.info(ctx, "plan revised on contradicting evidence", map[string]interface{}{"task_id": in.taskID.String(), "fingerprint": failures.Short(rec.Fingerprint), "call_id": callID, "revision": text})
	return rev, true
}

// revisionLine normalises the model's answer to one "revised plan: …" line.
func revisionLine(s string) string {
	s = strings.TrimSpace(s)
	for _, l := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(l); strings.HasPrefix(strings.ToLower(t), "revised plan:") {
			s = t
			break
		}
	}
	s = strings.Join(strings.Fields(s), " ")
	if !strings.HasPrefix(strings.ToLower(s), "revised plan:") {
		s = "revised plan: " + s
	}
	return truncate(s, 500)
}

// failingResults are the last run's failing checks as parser inputs.
func (in repairInputs) failingResults(out []byte, err error, timedOut bool) []failures.Result {
	var rs []failures.Result
	if in.results != nil {
		rs = in.results()
	}
	if len(rs) == 0 {
		rs = []failures.Result{{Tool: preCommitTool, Command: in.command, ExitCode: exitCodeOf(err), Output: string(out), Root: in.worktree}}
	}
	// The repository's own module path and package names, so a missing
	// in-repo package is the change's failure, not a dependency's.
	repo := failures.RepoContextOf(in.worktree, in.plannedFiles)
	for i := range rs {
		rs[i].Repo = repo
		rs[i].TimedOut = rs[i].TimedOut || timedOut
		if rs[i].Root == "" {
			rs[i].Root = in.worktree
		}
	}
	return rs
}

func (in repairInputs) describeCommand(rs []failures.Result) string {
	var cmds []string
	for _, r := range rs {
		if r.Command != "" {
			cmds = append(cmds, r.Command)
		}
	}
	if len(cmds) == 0 {
		return in.command
	}
	return strings.Join(cmds, " ; ")
}

func (in repairInputs) info(ctx context.Context, msg string, fields map[string]interface{}) {
	if in.logger != nil {
		in.logger.Info(ctx, msg, fields)
	}
}

func (in repairInputs) warn(ctx context.Context, msg string, fields map[string]interface{}) {
	if in.logger != nil {
		in.logger.Warn(ctx, msg, fields)
	}
}

// repairSummaryFields are the repair loop's fields of the implementation
// summary artifact: the stop reason, the repair and check-run counts and
// the top failure fingerprints ("" when no check ran).
func repairSummaryFields(rr *repairResult, stopped *stopInfo) map[string]string {
	out := map[string]string{"repair_stop_reason": "", "repair_attempts": "0", "repair_check_runs": "0", "failure_fingerprints": ""}
	if stopped != nil {
		out["stopped_reason"] = stopped.text()
	}
	if rr == nil {
		return out
	}
	out["repair_stop_reason"] = rr.stop
	out["repair_attempts"] = fmt.Sprintf("%d", rr.state.Repairs)
	out["repair_check_runs"] = fmt.Sprintf("%d", rr.state.Runs)
	if rr.tracker != nil {
		out["failure_fingerprints"] = strings.TrimSpace(formatTopFailures(rr.tracker.Top(escalationTopFingerprints)))
	}
	if len(rr.state.Revisions) > 0 {
		var revs []string
		for _, r := range rr.state.Revisions {
			revs = append(revs, fmt.Sprintf("%s (evidence %s, call %s)", r.Revision, failures.Short(r.Fingerprint), r.EvidenceCallID))
		}
		out["plan_revisions"] = strings.Join(revs, "\n")
	}
	return out
}

// saveImplementationSummary stores the implementation summary artifact:
// the model's summary, the controller fields (M3-02), the task contract
// (M2-04) and the repair loop's fields (M3-06).
func saveImplementationSummary(ctx context.Context, wg artifactAdder, run *controller.Run, task implementationTaskMessage, taskID, workID uuid.UUID, clonePath string, gen generationResult) {
	fields := runSummaryFields(run)
	for k, v := range map[string]string{
		"task_id":    taskID.String(),
		"summary":    gen.summary,
		"type":       "implementation",
		"clone_path": clonePath,
	} {
		fields[k] = v
	}
	for k, v := range criteriaSummaryFields(task) {
		fields[k] = v
	}
	for k, v := range repairSummaryFields(gen.repair, gen.stopped) {
		fields[k] = v
	}
	artifact := &models.WorkItemArtifact{
		WorkItemID: workID,
		Type:       models.ArtifactTypeResearchFinding,
		Content:    mustMarshal(fields),
		AgentID:    agentID,
	}
	if err := wg.AddArtifact(ctx, artifact); err != nil {
		log.Printf("implementation-agent: save artifact: %v", err)
	}
}

// summarize is the tracker's fingerprints for detail.repair.failures.
func summarize(t *failures.Tracker) []failureSummary {
	out := []failureSummary{}
	for _, e := range t.Entries() {
		rec := e.Record
		rec.Span = truncate(rec.Span, 600)
		out = append(out, failureSummary{Record: rec, Count: e.Count, FirstRun: e.FirstRun, LastRun: e.LastRun, LastCallID: e.LastCallID})
	}
	return out
}

func describeRecord(r failures.Record) string {
	s := string(r.Category)
	if loc := r.Location(); loc != "" {
		s += " at " + loc
	}
	if r.Test != "" {
		s += " in " + r.Test
	}
	return s + ": " + truncate(firstLine(r.Message), 160)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func fingerprintsOf(rs []failures.Record) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.Fingerprint)
	}
	return out
}

func firstExit(rs []failures.Result) *int {
	for _, r := range rs {
		if r.ExitCode != nil {
			return r.ExitCode
		}
	}
	return nil
}

func intPtr(i int) *int { return &i }

func shortID(id uuid.UUID) string { return id.String()[:8] }

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}
