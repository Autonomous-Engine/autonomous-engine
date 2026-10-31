package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/checks"
	"github.com/autonomousengine/autonomous-engine/internal/controller"
	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/failures"
	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
	"github.com/autonomousengine/autonomous-engine/internal/llm/tools"
	"github.com/autonomousengine/autonomous-engine/internal/logging"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/verifypolicy"
	"github.com/google/uuid"
)

// Impact-selected fast checks (M3-04, internal/checks). The plan is
// computed from the worktree's diff against the pinned base after the
// model's first non-test write, shown to the model as a short note on that
// tool result (and again whenever a later write changes it), recomputed
// before every pre-commit test run, and its executable targets replace the
// full suite in the pre-commit loop (runRepairLoop, repair.go). The final gate is
// untouched: it still runs the policy's full suite. The fast checks the
// gate can tie to the verified tree are appended to the evidence after the
// gate's own checks (appendFastChecks).
//

// checkPlanArtifactKind marks the work-item artifact holding a task's plan;
// the dashboard shows it on the subtask.
const checkPlanArtifactKind = "check_plan"

// artifactAdder is the workgraph surface the plan store needs.
type artifactAdder interface {
	AddArtifact(ctx context.Context, a *models.WorkItemArtifact) error
}

// fastCheckDeps is what startFastChecks needs.
type fastCheckDeps struct {
	worktree     string
	baseSHA      string
	repositoryID string
	workID       uuid.UUID
	taskID       uuid.UUID
	// testCommand is the command the final gate will run (detected, after
	// the policy override). Empty disables the fast loop.
	testCommand string
	policies    verificationPolicySource
	edges       checks.EdgeQuerier
	artifacts   artifactAdder
	executor    *tools.Executor
	logger      *logging.Logger
}

// fastChecks holds one task's plan and the results of its last fast run.
// Every method is a no-op on a nil receiver.
type fastChecks struct {
	ctx      context.Context
	worktree string
	baseSHA  string
	workID   uuid.UUID
	taskID   uuid.UUID
	policy   checks.Policy
	edges    []checks.ContractEdge
	store    artifactAdder
	logger   *logging.Logger

	mu        sync.Mutex
	plan      checks.Plan
	havePlan  bool
	notedSig  string
	loggedSig string
	// results are the targets of the last fast run, executed against
	// resultsTree (the worktree's tree object at that time).
	results     []fastCheckResult
	resultsTree string
}

// fastCheckResult is one executed batch (one command).
type fastCheckResult struct {
	batch      checks.Batch
	exitCode   *int
	status     evidence.CheckStatus
	skipReason string
	started    time.Time
	finished   time.Time
	output     []byte
}

// startFastChecks resolves the selection policy and the repository's
// contract edges and registers the note annotator on the executor. It
// returns nil (fast loop disabled, pre-commit runs the full command as
// before) when no test command is configured.
func startFastChecks(ctx context.Context, d fastCheckDeps) *fastChecks {
	if strings.TrimSpace(d.testCommand) == "" {
		return nil
	}
	f := &fastChecks{
		ctx: ctx, worktree: d.worktree, baseSHA: d.baseSHA, workID: d.workID, taskID: d.taskID,
		store: d.artifacts, logger: d.logger,
	}
	vp := verifypolicy.Default(uuid.Nil)
	repoID, parseErr := uuid.Parse(strings.TrimSpace(d.repositoryID))
	if parseErr == nil && d.policies != nil {
		if p, _, err := d.policies.LoadVerificationPolicy(ctx, repoID); err == nil {
			vp = p
		} else {
			f.warn("fast checks: verification policy lookup failed — selecting with the default policy", map[string]interface{}{"err": err.Error()})
		}
	}
	// The required kinds come from the repository policy until the work
	// item's acceptance criteria carry RequiredChecks (M2-04).
	f.policy = checks.PolicyFor(vp, d.testCommand)
	if parseErr == nil && d.edges != nil {
		edges, err := checks.LoadContractEdges(ctx, d.edges, repoID)
		if err != nil {
			f.warn("fast checks: contract edges unavailable — no api_compat targets", map[string]interface{}{"err": err.Error()})
		}
		f.edges = edges
	}
	if d.executor != nil {
		d.executor.SetResultAnnotator(f.annotate)
	}
	return f
}

// annotate is the executor's result annotator: after a successful write
// to a non-test file it recomputes the plan and, when the plan differs from
// the one the model last saw, appends its note to the tool result.
func (f *fastChecks) annotate(toolName, arguments string, result adapter.ToolResult) string {
	if f == nil || result.IsError || !tools.IsFileEditTool(toolName) {
		return ""
	}
	ev := controller.ToolEvent("", toolName, arguments, false, "", nil)
	if ev.Path == "" || controller.IsTestFile(controller.LanguageOf(ev.Path), ev.Path) {
		return ""
	}
	plan := f.recompute(f.ctx, "after write to "+ev.Path, false)
	f.mu.Lock()
	defer f.mu.Unlock()
	sig := plan.Signature()
	if sig == f.notedSig {
		return ""
	}
	f.notedSig = sig
	return plan.Note()
}

// recompute selects the plan from the current diff against the pinned
// base and logs it when it changed. store records it for the dashboard:
// only the plan a pre-commit run executes is stored, not every
// intermediate plan the model's writes produce.
func (f *fastChecks) recompute(ctx context.Context, why string, store bool) checks.Plan {
	changed, err := worktreeChangedFiles(ctx, f.worktree, f.baseSHA)
	if err != nil {
		f.warn("fast checks: cannot list changed files — the plan falls back to the full suite", map[string]interface{}{"err": err.Error()})
	}
	var plan checks.Plan
	if err != nil {
		// Without the diff the impact is unknown: run the full suite.
		plan = checks.FallbackPlan(f.policy, "changed files unavailable: "+err.Error())
	} else {
		plan = checks.Select(ctx, f.worktree, changed, f.policy, f.edges)
	}
	f.mu.Lock()
	f.plan, f.havePlan = plan, true
	sig := plan.Signature()
	changedPlan := sig != f.loggedSig
	f.loggedSig = sig
	f.mu.Unlock()
	if changedPlan {
		f.info("fast checks: plan "+why, planLogFields(plan))
	}
	if store {
		f.storePlan(ctx, plan)
	}
	return plan
}

// run is the pre-commit runner (runRepairLoop): recompute the plan,
// then run every batch in the worktree — the test targets as one go test
// invocation — all of them even after a failure so the fix prompt sees
// every failing check. It returns the combined output and a non-nil error
// when a non-advisory batch failed. An advisory batch (go vet / go build
// the policy does not require) is reported but never fails the run.
func (f *fastChecks) run(ctx context.Context) ([]byte, error) {
	// The plan is stored once per pre-commit run, after it, so the
	// dashboard can show targets that could not run (an all-skipped
	// migration run).
	plan := f.recompute(ctx, "before the pre-commit run", false)
	defer func() { f.storePlan(ctx, plan) }()
	tree, treeErr := worktreeTree(ctx, f.worktree)
	if treeErr != nil {
		f.warn("fast checks: cannot hash the worktree — results will not be recorded as evidence", map[string]interface{}{"err": treeErr.Error()})
	}
	var out bytes.Buffer
	var results []fastCheckResult
	failed, advisoryFailed := 0, 0
	for _, b := range plan.Batches() {
		started := time.Now().UTC()
		o, err := runTests(ctx, f.worktree, b.Command)
		finished := time.Now().UTC()
		r := fastCheckResult{batch: b, started: started, finished: finished, output: o, status: evidence.CheckPassed}
		code := 0
		if err != nil {
			r.status = evidence.CheckFailed
			if ee, ok := err.(*exec.ExitError); ok {
				code = ee.ExitCode()
			} else {
				code = -1
			}
			if b.Advisory {
				advisoryFailed++
			} else {
				failed++
			}
		} else if b.Kind == evidence.CheckKindMigration {
			// An all-skipped run is a target that could not run: shown in
			// the run output and the plan, never evidence.
			if why := migrationSkipped(o); why != "" {
				r.status, r.skipReason = evidence.CheckSkipped, "no runnable migration tests ("+why+")"
				plan.MarkSkipped(b, r.skipReason)
			}
		}
		if code >= 0 {
			c := code
			r.exitCode = &c
		}
		results = append(results, r)
		var reasons []string
		for _, t := range b.Targets {
			reasons = append(reasons, t.Reason)
		}
		label := string(b.Kind)
		if b.Advisory {
			label += ", advisory"
		}
		fmt.Fprintf(&out, "$ %s   # %s: %s\n%s", b.Command, label, strings.Join(reasons, "; "), o)
		if len(o) > 0 && o[len(o)-1] != '\n' {
			out.WriteByte('\n')
		}
		switch {
		case r.status == evidence.CheckFailed && b.Advisory:
			fmt.Fprintf(&out, "[advisory %s exit %d: reported only, does not fail this run]\n\n", r.status, code)
		case r.status == evidence.CheckSkipped:
			fmt.Fprintf(&out, "[skipped: %s]\n\n", r.skipReason)
		default:
			fmt.Fprintf(&out, "[%s exit %d]\n\n", r.status, code)
		}
		if ctx.Err() != nil {
			break
		}
	}
	f.mu.Lock()
	f.results = results
	f.resultsTree = ""
	if treeErr == nil {
		f.resultsTree = tree
	}
	f.mu.Unlock()
	f.info("fast checks: pre-commit run finished", map[string]interface{}{
		"commands": len(results), "failed": failed, "advisory_failed": advisoryFailed, "fallback": plan.Fallback,
	})
	if failed > 0 {
		return out.Bytes(), fmt.Errorf("%d of %d fast checks failed", failed, len(results))
	}
	return out.Bytes(), nil
}

// failureResults returns the last fast run's failing checks, one per
// command, as inputs to the failure parser (the repair loop, M3-06). An
// advisory failure is left out: it never fails the run, so it is not a
// failure to repair.
func (f *fastChecks) failureResults() []failures.Result {
	if f == nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []failures.Result
	for _, r := range f.results {
		if r.status != evidence.CheckFailed || r.batch.Advisory {
			continue
		}
		out = append(out, failures.Result{Tool: preCommitTool, Command: r.batch.Command, ExitCode: r.exitCode, Output: string(r.output), Root: f.worktree})
	}
	return out
}

// failedChecks returns the last fast run's failing, non-advisory checks as
// evidence checks (finalizeStopped records them on a stopped attempt).
func (f *fastChecks) failedChecks(outputRef string) []evidence.CheckResult {
	if f == nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []evidence.CheckResult
	for _, r := range f.results {
		if r.status != evidence.CheckFailed || r.batch.Advisory {
			continue
		}
		started, finished := r.started, r.finished
		out = append(out, evidence.CheckResult{Kind: r.batch.Kind, Command: r.batch.Command, ExitCode: r.exitCode,
			Status: evidence.CheckFailed, StartedAt: &started, FinishedAt: &finished, OutputRef: outputRef})
	}
	return out
}

// language is the language of the current plan ("go"; "" for a fallback
// plan), the failure parser's hint.
func (f *fastChecks) language() string {
	if f == nil {
		return ""
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.plan.Language
}

// migrationSkipped returns a skip reason when a passing `go test -v` run
// of the migration tests ran none of them: every one skipped, or no test
// matched. "" when at least one passed.
func migrationSkipped(out []byte) string {
	s := string(out)
	if strings.Contains(s, "--- PASS") {
		return ""
	}
	switch {
	case strings.Contains(s, "--- SKIP"):
		return "every migration test skipped (e.g. no integration database configured)"
	case strings.Contains(s, "no tests to run"):
		return "no migration test matched -run Migrat"
	case strings.Contains(s, "[no test files]"):
		return "no test files in the migration test packages"
	}
	return ""
}

// appendFastChecks appends the fast checks of the last pre-commit run to
// the evidence, after the gate's checks, with their raw outputs stored:
// one check per executed command that actually ran a test (an all-skipped
// migration run is left out, like a target with no command). They are appended only when the gate
// itself ran the full test suite (passed or failed) and the fast run
// executed against exactly the tree the gate verified, so they neither
// lift an outcome the gate did not earn nor describe code other than the
// verified head. A failed advisory check is never recorded: the gate does
// not run it, so it must not fail the evidence either.
func appendFastChecks(ctx context.Context, in finalizeInputs, sink completionSink, gate gateResult, ev *evidence.ImplementationEvidence) {
	f := in.fast
	if f == nil {
		return
	}
	f.mu.Lock()
	results := append([]fastCheckResult(nil), f.results...)
	tree := f.resultsTree
	f.mu.Unlock()
	if len(results) == 0 {
		return
	}
	skip := ""
	switch st := gate.testCheck.Status; {
	case st != evidence.CheckPassed && st != evidence.CheckFailed:
		skip = "the gate did not run the full suite"
	case gate.headSHA == "":
		skip = "no verified head"
	case tree == "":
		skip = "the fast run's tree is unknown"
	default:
		headTree, err := gitRead(ctx, in.worktreePath, "rev-parse", gate.headSHA+"^{tree}")
		if err != nil {
			skip = "cannot resolve the verified head's tree: " + err.Error()
		} else if headTree != tree {
			skip = "the fast checks ran against an earlier tree (later commits changed the code)"
		}
	}
	if skip != "" {
		f.info("fast checks not recorded as evidence: "+skip, map[string]interface{}{"head_sha": gate.headSHA, "fast_checks": len(results)})
		return
	}
	for i, r := range results {
		if r.batch.Advisory && r.status == evidence.CheckFailed {
			f.info("advisory fast check failed — reported to the model, not recorded as evidence", map[string]interface{}{"command": r.batch.Command})
			continue
		}
		if r.status == evidence.CheckSkipped {
			// Equivalent to a target that could not run: never evidence,
			// never caps the outcome.
			f.info("fast check ran no tests — not recorded as evidence", map[string]interface{}{"command": r.batch.Command, "reason": r.skipReason})
			continue
		}
		started, finished := r.started, r.finished
		c := evidence.CheckResult{
			Kind: r.batch.Kind, Command: r.batch.Command, ExitCode: r.exitCode, Status: r.status,
			SkipReason: r.skipReason, StartedAt: &started, FinishedAt: &finished,
		}
		if ref := storeRawOutput(ctx, in, sink, fmt.Sprintf("fast-check-%d-%s-output", i+1, r.batch.Kind), r.output); ref != "" {
			c.OutputRef = ref
			ev.ArtifactRefs = append(ev.ArtifactRefs, ref)
		}
		ev.Checks = append(ev.Checks, c)
	}
}

// storePlan records the plan as a work-item artifact for the dashboard.
func (f *fastChecks) storePlan(ctx context.Context, plan checks.Plan) {
	if f.store == nil {
		return
	}
	content, err := json.Marshal(map[string]interface{}{
		"type":        checkPlanArtifactKind,
		"task_id":     f.taskID.String(),
		"plan":        plan,
		"computed_at": time.Now().UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		return
	}
	a := &models.WorkItemArtifact{WorkItemID: f.workID, Type: models.ArtifactTypeResearchFinding, Content: content, AgentID: agentID}
	if err := f.store.AddArtifact(ctx, a); err != nil {
		f.warn("fast checks: cannot store the plan artifact", map[string]interface{}{"err": err.Error()})
	}
}

// worktreeChangedFiles lists the files that differ from base in the
// working tree (tracked changes, staged or not, both sides of renames)
// plus untracked, non-ignored files.
func worktreeChangedFiles(ctx context.Context, dir, base string) ([]string, error) {
	if strings.TrimSpace(base) == "" {
		base = "HEAD"
	}
	diff, err := gitRead(ctx, dir, "diff", "--name-only", "--no-renames", base)
	if err != nil {
		return nil, err
	}
	untracked, err := gitRead(ctx, dir, "ls-files", "--others", "--exclude-standard")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, l := range strings.Split(diff+"\n"+untracked, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out, nil
}

// worktreeTree returns the tree object the working tree would commit as
// (git add -A semantics), computed in a throwaway index so the real index
// is untouched.
func worktreeTree(ctx context.Context, dir string) (string, error) {
	tmp, err := os.CreateTemp("", "ae-fastcheck-index-*")
	if err != nil {
		return "", err
	}
	idx := tmp.Name()
	tmp.Close()
	os.Remove(idx) // git creates it; an empty file is not a valid index
	defer os.Remove(idx)
	run := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_INDEX_FILE="+filepath.Clean(idx))
		out, err := cmd.Output()
		if err != nil {
			return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
		}
		return strings.TrimSpace(string(out)), nil
	}
	if _, err := run("read-tree", "HEAD"); err != nil {
		return "", err
	}
	if _, err := run("add", "-A"); err != nil {
		return "", err
	}
	return run("write-tree")
}

func planLogFields(p checks.Plan) map[string]interface{} {
	var targets []string
	for _, t := range p.Targets {
		cmd := t.Command
		if cmd == "" {
			cmd = "not_run: " + t.NotRunReason
		}
		targets = append(targets, fmt.Sprintf("%s: %s (%s)", t.Kind, cmd, t.Reason))
	}
	return map[string]interface{}{
		"changed_files": strings.Join(p.ChangedFiles, ","),
		"targets":       strings.Join(targets, " | "),
		"fallback":      p.Fallback,
		"language":      p.Language,
	}
}

func (f *fastChecks) info(msg string, fields map[string]interface{}) {
	if f.logger == nil {
		return
	}
	if fields == nil {
		fields = map[string]interface{}{}
	}
	fields["task_id"] = f.taskID.String()
	f.logger.Info(f.ctx, msg, fields)
}

func (f *fastChecks) warn(msg string, fields map[string]interface{}) {
	if f.logger == nil {
		return
	}
	if fields == nil {
		fields = map[string]interface{}{}
	}
	fields["task_id"] = f.taskID.String()
	f.logger.Warn(f.ctx, msg, fields)
}
