package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/plan"
	"github.com/autonomousengine/autonomous-engine/internal/snapshot"
	"github.com/google/uuid"
)

func snapshotPin(sha string) snapshot.Pin { return snapshot.Pin{BaseRef: "main", BaseSHA: sha} }

func TestPlanValidationMetrics(t *testing.T) {
	for _, c := range plan.ViolationCodes {
		if _, ok := violationCounters[c]; !ok {
			t.Errorf("ae_plan_validation_violations_total{code=%q} is not registered at startup", c)
		}
	}
	before := violationCounter(plan.CodeOrphanCriterion).Value()
	countViolations([]plan.Violation{{Code: plan.CodeOrphanCriterion}, {Code: plan.CodeOrphanCriterion}})
	if got := violationCounter(plan.CodeOrphanCriterion).Value() - before; got != 2 {
		t.Fatalf("orphan_criterion counter moved by %d, want 2", got)
	}
}

// repairFixture is internal/plan/testdata/repair: a brief whose AC-3 is a
// derived duplicate of AC-1, a first plan that leaves AC-2 orphaned (task 2
// cites nothing), and the repaired plan.
func repairFixture(t *testing.T) (*plan.BriefCriteria, []byte, []byte) {
	t.Helper()
	dir := filepath.Join("..", "..", "internal", "plan", "testdata", "repair")
	read := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	brief, err := briefCriteriaFromFile(read("brief.json"))
	if err != nil {
		t.Fatalf("brief: %v", err)
	}
	return brief, read("first.json"), read("second.json")
}

func mustTasks(t *testing.T, data []byte) []decomposedTask {
	t.Helper()
	tasks, err := parseTaskArray(string(data))
	if err != nil {
		t.Fatal(err)
	}
	return tasks
}

// capturedBuilder is phase 1 as the handler runs it, minus the database:
// the brief is folded ONCE before the prompt (foldBrief, exactly as
// handleTechnicalBrief does), and every attempt rewrites citations with
// those folds and folds the plan's derived criteria (foldPlanCriteria, as
// planInputs.build does); then dropping, grouping, dependency resolution and
// validation as the -validate subcommand runs them.
func capturedBuilder(brief *plan.BriefCriteria) func([]decomposedTask) *planBuild {
	folded, briefFolds := foldBrief(uuid.Nil, brief)
	return func(raw []decomposedTask) *planBuild {
		numberSteps(raw)
		criteria, folds := foldPlanCriteria(uuid.Nil, folded, briefFolds, raw)
		b := capturedBuild(criteria, raw, "", []evidence.CheckKind{evidence.CheckKindTest}, false)
		b.folds = folds
		return b
	}
}

// The handler folds the brief before the prompt and hands the folds to
// phase 1 (Important 1 of the M2-05 review): a citation of a brief-folded id
// is rewritten, not reported as unknown.
func TestHandlerPassesBriefFoldsToPhase1(t *testing.T) {
	src := stripComments(plannerSources(t)["main.go"])
	for _, want := range []string{
		"structured.Criteria, briefFolds = foldBrief(workID, structured.Criteria)",
		"briefFolds:    briefFolds,",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("handleTechnicalBrief lacks %q", want)
		}
	}
	if !strings.Contains(stripComments(plannerSources(t)["validation.go"]), "foldPlanCriteria(in.workID, in.structured.Criteria, in.briefFolds, raw)") {
		t.Error("planInputs.build does not rewrite citations with the brief folds")
	}
}

func TestBriefFoldedIDCitationIsRewritten(t *testing.T) {
	brief, first, _ := repairFixture(t)
	folded, briefFolds := foldBrief(uuid.Nil, brief)
	if len(briefFolds) != 1 || briefFolds[0].From != "AC-3" || len(folded.Criteria) != 2 {
		t.Fatalf("brief fold: %+v (criteria %d)", briefFolds, len(folded.Criteria))
	}
	raw := mustTasks(t, first) // task 1 cites AC-3, folded away before the prompt
	numberSteps(raw)
	if _, folds := foldPlanCriteria(uuid.Nil, folded, briefFolds, raw); len(folds) != 1 {
		t.Fatalf("folds = %+v, want the brief's", folds)
	}
	if got := raw[0].CriterionIDs; len(got) != 1 || got[0] != "AC-1" {
		t.Fatalf("task 1 cites %v, want [AC-1]", got)
	}
	for _, v := range capturedBuilder(brief)(mustTasks(t, first)).validate() {
		if v.Code == plan.CodeUnknownCriterion {
			t.Fatalf("a brief-folded id is reported unknown: %+v", v)
		}
	}
}

// The -validate subcommand and the handler's repair loop see the same
// violations on the same captured plan.
func TestValidateCommandAgreesWithPlanWithRepair(t *testing.T) {
	brief, first, second := repairFixture(t)
	viaCmd, _, err := validateCaptured(brief, mustTasks(t, first), "", []evidence.CheckKind{evidence.CheckKindTest}, false)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	out := planWithRepair(uuid.Nil, func(string) ([]decomposedTask, error) {
		calls++
		if calls == 1 {
			return mustTasks(t, first), nil
		}
		return mustTasks(t, second), nil
	}, capturedBuilder(brief))
	if !reflect.DeepEqual(viaCmd, out.previous) {
		t.Fatalf("-validate and planWithRepair disagree:\n cmd  %+v\n loop %+v", viaCmd, out.previous)
	}
	if len(viaCmd) != 2 {
		t.Fatalf("want 2 violations on testdata/repair/first.json, got %+v", viaCmd)
	}
}

// TestPlanWithRepair_RepairRoundSucceeds is the planner-level repair case:
// the first plan is rejected, its violations reach the second prompt
// verbatim, and the repaired plan is accepted.
func TestPlanWithRepair_RepairRoundSucceeds(t *testing.T) {
	brief, first, second := repairFixture(t)

	// What the validator says about the first plan.
	firstViolations := capturedBuilder(brief)(mustTasks(t, first)).validate()
	if !plan.HasErrors(firstViolations) {
		t.Fatalf("the first plan must be rejected: %+v", firstViolations)
	}
	codes := map[plan.ViolationCode]bool{}
	for _, v := range firstViolations {
		codes[v.Code] = true
	}
	if !codes[plan.CodeOrphanCriterion] || !codes[plan.CodeNoCriteria] || len(firstViolations) != 2 {
		t.Fatalf("first plan: want orphan_criterion (AC-2) and no_criteria (task 2), got %+v", firstViolations)
	}

	var repairs []string
	decompose := func(repair string) ([]decomposedTask, error) {
		repairs = append(repairs, repair)
		if len(repairs) == 1 {
			return mustTasks(t, first), nil
		}
		return mustTasks(t, second), nil
	}
	out := planWithRepair(uuid.Nil, decompose, capturedBuilder(brief))
	if len(repairs) != 2 || repairs[0] != "" {
		t.Fatalf("decompose calls: %q — want the plain prompt, then one repair", repairs)
	}
	for _, v := range firstViolations {
		if !strings.Contains(repairs[1], "- "+v.Message+"\n") && !strings.HasSuffix(repairs[1], "- "+v.Message) {
			t.Errorf("repair prompt lacks the violation verbatim:\n%s\nwant line: - %s", repairs[1], v.Message)
		}
	}
	if out.rejected() || out.attempts != 2 || out.decomposeErr != nil {
		t.Fatalf("outcome: rejected=%t attempts=%d err=%v violations=%+v", out.rejected(), out.attempts, out.decomposeErr, out.violations)
	}
	if len(out.build.planned) != 1 || len(out.build.folds) != 1 || out.build.folds[0].From != "AC-3" {
		t.Errorf("build: %d subtask(s), folds %+v", len(out.build.planned), out.build.folds)
	}
}

func TestPlanWithRepair_StillInvalidAfterOneRepair(t *testing.T) {
	brief, first, _ := repairFixture(t)
	calls := 0
	out := planWithRepair(uuid.Nil, func(string) ([]decomposedTask, error) {
		calls++
		return mustTasks(t, first), nil
	}, capturedBuilder(brief))
	if calls != maxPlanAttempts || !out.rejected() || out.attempts != 2 {
		t.Fatalf("calls=%d rejected=%t attempts=%d — exactly one repair round, then rejected", calls, out.rejected(), out.attempts)
	}
}

func TestPlanWithRepair_ValidFirstTimeAndDecomposeFailure(t *testing.T) {
	brief, _, second := repairFixture(t)
	calls := 0
	out := planWithRepair(uuid.Nil, func(string) ([]decomposedTask, error) {
		calls++
		return mustTasks(t, second), nil
	}, capturedBuilder(brief))
	if calls != 1 || out.rejected() || out.attempts != 1 {
		t.Fatalf("a valid plan is not re-prompted: calls=%d out=%+v", calls, out)
	}

	boom := errors.New("provider down")
	out = planWithRepair(uuid.Nil, func(string) ([]decomposedTask, error) { return nil, boom }, capturedBuilder(brief))
	if !errors.Is(out.decomposeErr, boom) || out.build != nil {
		t.Fatalf("a decomposition failure is returned as such: %+v", out)
	}
}

func TestPlanWithRepair_WarningsDoNotTriggerARepair(t *testing.T) {
	brief, _, second := repairFixture(t)
	tasks := mustTasks(t, second)
	calls := 0
	out := planWithRepair(uuid.Nil, func(string) ([]decomposedTask, error) {
		calls++
		cp := append([]decomposedTask(nil), tasks...)
		cp[1].FileClaims = []fileClaim{{Path: "tax/rates.go", EvidenceRef: "hypothesis"}}
		cp[1].Files = []string{"tax/rates.go"}
		return cp, nil
	}, capturedBuilder(brief))
	if calls != 1 || out.rejected() {
		t.Fatalf("a warning must not reject or repair: calls=%d violations=%+v", calls, out.violations)
	}
	if w := plan.Warnings(out.violations); len(w) != 1 || w[0].Code != plan.CodeHypothesisOnlyTask || w[0].TaskIndex != 2 {
		t.Fatalf("warnings: %+v", out.violations)
	}

	// The warning is shown in the subtask's description and message.
	planned := out.build.planned
	planned[0].item = &models.WorkItem{Description: "desc"}
	attachPlanWarnings(planned, out.violations)
	for _, d := range []string{planned[0].msg.Description, planned[0].item.Description} {
		if !strings.Contains(d, "## Plan validation warnings") || !strings.Contains(d, "every file it names is a hypothesis (tax/rates.go)") {
			t.Errorf("description lacks the warning: %q", d)
		}
	}
}

func TestRepairPromptCarriesOnlyErrorsVerbatim(t *testing.T) {
	vs := []plan.Violation{
		{Code: plan.CodeOrphanCriterion, Severity: plan.SeverityError, Message: `AC-2 ("Tax") is owned by no task`},
		{Code: plan.CodeHypothesisOnlyTask, Severity: plan.SeverityWarning, Message: "a warning"},
	}
	prev := []decomposedTask{{Title: "Fix discount order", CriterionIDs: []string{"AC-1"}}, {Title: "Compute tax"}}
	got := repairPrompt(vs, prev, nil)
	if !strings.Contains(got, "\n- AC-2 (\"Tax\") is owned by no task") || strings.Contains(got, "a warning") {
		t.Fatalf("repair prompt: %q", got)
	}
	// The previous plan is quoted so "task 2" is resolvable.
	if !strings.Contains(got, "Your previous plan (task 1 is the first element):") ||
		strings.Index(got, `"Fix discount order"`) > strings.Index(got, `"Compute tax"`) || strings.Index(got, `"Fix discount order"`) < 0 {
		t.Fatalf("repair prompt does not quote the previous plan in order: %s", got)
	}
}

func TestRepairPromptBoundsThePreviousPlan(t *testing.T) {
	big := []decomposedTask{{Title: "big", Description: strings.Repeat("x", 3*repairPlanMaxBytes)}}
	got := repairPrompt([]plan.Violation{{Code: plan.CodeNoDeliverable, Message: "m"}}, big, nil)
	if len(got) > repairPlanMaxBytes+1024 || !strings.Contains(got, "(truncated: 16384 of ") {
		t.Fatalf("repair prompt is %d bytes; want <= %d plus the note", len(got), repairPlanMaxBytes+1024)
	}
}

// When the repair round ends for another reason, the reason says what the
// first plan was rejected for.
func TestRepairRoundEndingOtherwiseMentionsTheFirstViolations(t *testing.T) {
	brief, first, _ := repairFixture(t)
	calls := 0
	boom := errors.New("provider down")
	out := planWithRepair(uuid.Nil, func(string) ([]decomposedTask, error) {
		calls++
		if calls == 1 {
			return mustTasks(t, first), nil
		}
		return nil, boom
	}, capturedBuilder(brief))
	if !errors.Is(out.decomposeErr, boom) || out.attempts != 2 {
		t.Fatalf("outcome: %+v", out)
	}
	note := out.previousNote()
	for _, want := range []string{"plan-validation repair round", "2 violation(s)", "[no_criteria] task 2", "[orphan_criterion] AC-2"} {
		if !strings.Contains(note, want) {
			t.Errorf("note lacks %q: %s", want, note)
		}
	}
	if (planOutcome{}).previousNote() != "" {
		t.Error("no repair round, no note")
	}
	src := stripComments(plannerSources(t)["main.go"])
	if n := strings.Count(src, "outcome.previousNote()"); n != 4 {
		t.Errorf("previousNote is used %d time(s) in the handler, want 4 (decompose failure, no repository, all non-code (R3), nothing to implement)", n)
	}
}

// Criterion checks share the no-runner exemption with the policy's kinds.
func TestNoTestRunnerExemptsCriterionAndPolicyTestChecks(t *testing.T) {
	brief, _, second := repairFixture(t)
	raw := mustTasks(t, second)
	for i := range raw {
		raw[i].Verification = []plan.VerificationStep{{CheckKind: evidence.CheckKindBuild, Command: "go build ./..."}}
	}
	vs, _, err := validateCaptured(brief, raw, "", []evidence.CheckKind{evidence.CheckKindTest}, true)
	if err != nil || len(vs) != 0 {
		t.Fatalf("no runner: want no violation (criteria and policy both require test), got %+v (%v)", vs, err)
	}
	vs, _, _ = validateCaptured(brief, mustTasksWithBuildOnly(t, second), "", []evidence.CheckKind{evidence.CheckKindTest}, false)
	var criterion, policy int
	for _, v := range vs {
		if v.Code == plan.CodeVerifierMissing && v.CriterionID != "" {
			criterion++
		}
		if v.Code == plan.CodeVerifierMissing && v.CriterionID == "" {
			policy++
		}
	}
	if criterion != 2 || policy != 1 {
		t.Fatalf("with a runner: want 2 criterion and 1 policy verifier_missing, got %+v", vs)
	}
	src := stripComments(plannerSources(t)["validation.go"])
	if !strings.Contains(src, "s.NoTestRunner = !hasTestInfra && !userAskedForTests") {
		t.Error("the handler's phase 1 does not pass the no-runner condition to the validator")
	}
}

func mustTasksWithBuildOnly(t *testing.T, data []byte) []decomposedTask {
	raw := mustTasks(t, data)
	for i := range raw {
		raw[i].Verification = []plan.VerificationStep{{CheckKind: evidence.CheckKindBuild, Command: "go build ./..."}}
	}
	return raw
}

func TestPlanValidationReasonListsFiveAndTheCount(t *testing.T) {
	var vs []plan.Violation
	for i := 1; i <= 7; i++ {
		vs = append(vs, plan.Violation{Code: plan.CodeNoDeliverable, Severity: plan.SeverityError, TaskIndex: i, Message: plan.TaskLabel(i, "t") + " states no deliverable"})
	}
	got := planValidationReason(vs, 2)
	for _, want := range []string{"after 2 attempt(s)", "7 violation(s)", `[no_deliverable] task 5 "t" states no deliverable (and 2 more)`, "plan-validation artifact"} {
		if !strings.Contains(got, want) {
			t.Errorf("reason lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, `task 6 "t"`) {
		t.Errorf("reason lists more than five: %s", got)
	}
}

// The plan-validation artifact reuses the technical_brief type, so the
// brief reader must skip it (a retry would otherwise plan against it).
func TestLoadStructuredBriefSkipsThePlanValidationArtifact(t *testing.T) {
	src := stripComments(plannerSources(t)["main.go"])
	i := strings.Index(src, "func loadStructuredBrief(")
	if i < 0 {
		t.Fatal("loadStructuredBrief not found")
	}
	if !strings.Contains(src[i:], `COALESCE(content->>'name', '') <> 'plan-validation'`) {
		t.Error("loadStructuredBrief does not skip the plan-validation artifact")
	}
	if planValidationArtifactName != "plan-validation" {
		t.Error("artifact name changed without the reader")
	}
}

func TestPlanDependencyGraphAttributesUnknownTitlesToTheirStep(t *testing.T) {
	steps := []decomposedTask{
		{Title: "Add endpoint", index: 1},
		{Title: "Add page", DependsOn: []string{"Add endpoint", "Write migration"}, index: 2},
	}
	planned := []plannedSubtask{
		{msg: implementationTaskMessage{Title: "api"}, titles: []string{"Add endpoint"}, stepTasks: steps[:1]},
		{msg: implementationTaskMessage{Title: "web"}, titles: []string{"Add page"}, dependsOnTitles: steps[1].DependsOn, stepTasks: steps[1:]},
	}
	g := planDependencyGraph(planned, planSteps(steps))
	if len(g.Unresolved) != 1 || g.Unresolved[0].TaskIndex != 2 || g.Unresolved[0].Dependency != "Write migration" {
		t.Fatalf("unresolved: %+v", g.Unresolved)
	}
	if len(g.Edges) != 2 || len(g.Edges[1]) != 1 || g.Edges[1][0] != 0 {
		t.Fatalf("edges: %+v", g.Edges)
	}
	vs := plan.ValidatePlan(plan.BriefCriteria{}, nil, g, nil)
	if len(vs) != 1 || vs[0].Code != plan.CodeUnknownDependency ||
		vs[0].Message != `task 2 "Add page" depends on "Write migration", which is not a task in this plan; known titles: "Add endpoint", "Add page"` {
		t.Fatalf("violations: %+v", vs)
	}
}

func TestStepSpecKeepsTheClaims(t *testing.T) {
	step := decomposedTask{Title: "x", index: 3, CriterionIDs: []string{"AC-1"}, FileClaims: []fileClaim{
		{Path: "./a.go"}, {Path: "b.go", EvidenceRef: "hypothesis"}, {Path: "c.go", EvidenceRef: "repo:c2"}, {Path: "d.go", EvidenceRef: "new"},
	}}
	s := stepSpec(step, "r", "repo", nil)
	want := []plan.EvidenceKind{"", plan.EvidenceHypothesis, plan.EvidenceRepo, plan.EvidenceNew}
	if s.Index != 3 || len(s.Spec.Files) != 4 || s.Spec.Files[0].Path != "a.go" {
		t.Fatalf("spec: %+v", s)
	}
	for i, k := range want {
		if s.Spec.Files[i].Evidence.Kind != k {
			t.Errorf("file %d kind %q, want %q", i, s.Spec.Files[i].Evidence.Kind, k)
		}
	}
	if s.Spec.Files[2].Evidence.Ref != "c2" {
		t.Errorf("repo ref lost: %+v", s.Spec.Files[2])
	}
}

func TestStatRootForPrefersTheSnapshotWorktree(t *testing.T) {
	clone := t.TempDir()
	sha := strings.Repeat("ab", 20)
	if root, basis := statRootFor(clone, snapshotPin(sha)); root != clone || basis != plan.BasisClone {
		t.Fatalf("no snapshot: got %s %s", root, basis)
	}
	wt := filepath.Join(clone, ".git", "ae-snapshots", sha[:12])
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if root, basis := statRootFor(clone, snapshotPin(sha)); root != wt || basis != plan.BasisSnapshot {
		t.Fatalf("snapshot present: got %s %s", root, basis)
	}
	if root, basis := statRootFor(clone, snapshotPin("")); root != clone || basis != plan.BasisClone {
		t.Fatalf("unpinned: got %s %s", root, basis)
	}
	if root, _ := statRootFor("", snapshotPin(sha)); root != "" {
		t.Fatalf("no clone path: got %q", root)
	}
}

func TestGroundTaskFilesAtRecordsTheSnapshotBasis(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.go"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	got := groundTaskFilesAt("/clone", root, plan.BasisSnapshot, []fileClaim{{Path: "a.go"}, {Path: "gone.go"}}, nil)
	if got[0].ref.Evidence.Kind != plan.EvidenceRepo || got[0].ref.Evidence.Basis != plan.BasisSnapshot {
		t.Errorf("a.go: %+v", got[0].ref)
	}
	if got[1].ref.Evidence.Kind != plan.EvidenceHypothesis || got[1].ref.Evidence.Note != "grounding: not found in the pinned snapshot" {
		t.Errorf("gone.go: %+v", got[1].ref)
	}
}

// --- the -validate subcommand ---

func TestValidateCommand(t *testing.T) {
	dir := filepath.Join("..", "..", "internal", "plan", "testdata", "repair")
	var out, errOut bytes.Buffer
	code := runValidateCommand([]string{"-policy", "test", filepath.Join(dir, "brief.json"), filepath.Join(dir, "first.json")}, &out, &errOut)
	if code != 1 {
		t.Fatalf("exit %d for a rejected plan, want 1\n%s%s", code, out.String(), errOut.String())
	}
	for _, want := range []string{"fold     AC-3 folded into AC-1", "[no_criteria] task 2:", "[orphan_criterion] AC-2:", "plan REJECTED"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}

	out.Reset()
	if code := runValidateCommand([]string{filepath.Join(dir, "brief.json"), filepath.Join(dir, "second.json")}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d for an accepted plan\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "0 error(s), 0 warning(s) — plan accepted") {
		t.Errorf("output: %s", out.String())
	}

	// -root turns the stat rules on: calculate.go does not exist under an
	// empty root, so its repo evidence is stale.
	out.Reset()
	if code := runValidateCommand([]string{"-root", t.TempDir(), filepath.Join(dir, "brief.json"), filepath.Join(dir, "second.json")}, &out, &errOut); code != 1 ||
		!strings.Contains(out.String(), "[stale_evidence]") {
		t.Fatalf("exit %d, output: %s", code, out.String())
	}

	if code := runValidateCommand([]string{"only-one.json"}, &out, &errOut); code != 2 {
		t.Fatalf("usage error exit %d, want 2", code)
	}
}
