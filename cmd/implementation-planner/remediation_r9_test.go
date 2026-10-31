package main

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/idempotency"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/plan"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// R9 P1: the planner dispatches on the remediation policy
// (internal/plan/remediation.go): normalise codes are normalised before any
// re-prompt, reprompt codes get a targeted hint, gap codes never block, and
// every hard block raises an escalation prompt.

// An orphan the planner can re-home is the only problem: no re-prompt is
// spent on it, the plan is accepted on the first decomposition, and the
// re-homing is recorded.
func TestNormaliseCodesAreNormalisedBeforeAnyReprompt(t *testing.T) {
	brief := orphanBrief("A", "B", "The banner in `web/app.js` says hello")
	calls := 0
	out := planWithRepair(uuid.Nil, func(string) ([]decomposedTask, error) {
		calls++
		return []decomposedTask{
			orphanStep("A", []string{"AC-1"}, "web/app.js"),
			orphanStep("B", []string{"AC-2"}, "web/style.css"),
		}, nil
	}, capturedBuilder(brief))
	if out.rejected() || out.decomposeErr != nil {
		t.Fatalf("rejected: %+v (%v)", out.violations, out.decomposeErr)
	}
	if calls != 1 || out.attempts != 1 {
		t.Fatalf("calls=%d attempts=%d, want the plan accepted without a re-prompt", calls, out.attempts)
	}
	if len(out.build.rehomed) != 1 || out.build.rehomed[0].CriterionID != "AC-3" || out.build.rehomed[0].TaskIndex != 1 {
		t.Fatalf("rehomed = %+v, want AC-3 on task 1", out.build.rehomed)
	}
}

// Next to a reprompt code, a re-homeable orphan is still normalised first:
// the repair prompt does not ask the model about it, and the plan it quotes
// already cites it on the step that changes its file.
func TestOrphanNextToARepromptCodeIsRehomedBeforeTheRepair(t *testing.T) {
	brief := orphanBrief("A", "B", "The banner in `web/app.js` says hello")
	first := []decomposedTask{
		orphanStep("A", []string{"AC-1"}, "web/app.js"),
		orphanStep("B", []string{"AC-2"}, "web/style.css"),
	}
	first[1].Deliverable = ""
	var repairs []string
	out := planWithRepair(uuid.Nil, func(r string) ([]decomposedTask, error) {
		repairs = append(repairs, r)
		if len(repairs) == 1 {
			return copySteps(first), nil
		}
		fixed := copySteps(first)
		fixed[1].Deliverable = "B done"
		fixed[0].CriterionIDs = append(fixed[0].CriterionIDs, "AC-3")
		return fixed, nil
	}, capturedBuilder(brief))
	if out.rejected() || len(repairs) != 2 {
		t.Fatalf("rejected=%t calls=%d violations=%+v", out.rejected(), len(repairs), out.violations)
	}
	repair := repairs[1]
	if strings.Contains(repair, "AC-3 (") && strings.Contains(repair, "is owned by no task") {
		t.Errorf("the repair prompt asks about the orphan the planner re-homed:\n%s", repair)
	}
	if !strings.Contains(repair, `task 2 "B" states no deliverable`) {
		t.Errorf("the repair prompt lacks the reprompt violation:\n%s", repair)
	}
	i := strings.Index(repair, "Your previous plan")
	if i < 0 || !strings.Contains(repair[i:], `"AC-3"`) {
		t.Errorf("the quoted plan does not carry the re-homed AC-3:\n%s", repair)
	}
}

// Gap codes never block and never cost a re-prompt (R3/R8 plumbing); an
// orphan no step covers still gets the repair round before it becomes a gap.
func TestGapCodesNeverBlock(t *testing.T) {
	brief, _, second := repairFixture(t)
	tasks := mustTasks(t, second)
	tasks[1].FileClaims = []fileClaim{{Path: "tax/rates.go", EvidenceRef: "hypothesis"}}
	calls := 0
	out := planWithRepair(uuid.Nil, func(string) ([]decomposedTask, error) {
		calls++
		return copySteps(tasks), nil
	}, capturedBuilder(brief))
	if out.rejected() || calls != 1 {
		t.Fatalf("rejected=%t calls=%d", out.rejected(), calls)
	}
	for _, v := range out.violations {
		if p, _ := plan.RemediationFor(v.Code); p.Remediation != plan.RemediationGap {
			t.Errorf("violation %+v left on an accepted plan is not a gap", v)
		}
	}
}

// ─── hints ──────────────────────────────────────────────────────────────────

func hintBuild(t *testing.T, steps []decomposedTask, brief *plan.BriefCriteria) (*planBuild, []plan.Violation) {
	t.Helper()
	b := capturedBuilder(brief)(copySteps(steps))
	return b, b.validate()
}

func TestVerifierMissingHintNamesTheOwnersAndAKnownCommand(t *testing.T) {
	brief := orphanBrief("A", "B")
	brief.Criteria[1].RequiredChecks = []evidence.CheckKind{evidence.CheckKindBuild}
	steps := []decomposedTask{
		orphanStep("A", []string{"AC-1"}, "src/a.ts"),
		orphanStep("B", []string{"AC-2"}, "src/b.ts"),
	}
	steps[0].Verification = append(steps[0].Verification, plan.VerificationStep{CheckKind: evidence.CheckKindBuild, Command: "npm run build"})
	b, vs := hintBuild(t, steps, brief)
	p := repairPrompt(vs, b.raw, repairHints(b, vs))
	want := `AC-2 needs a build step: add {"check_kind":"build","command":"npm run build"} to the verification of task 2, which cites AC-2 (the plan already runs "npm run build" as a build check)`
	if !strings.Contains(p, want) {
		t.Fatalf("repair prompt lacks %q:\n%s", want, p)
	}
	// Without a known command the hint asks for one.
	steps[0].Verification = steps[0].Verification[:1]
	b, vs = hintBuild(t, steps, brief)
	p = repairPrompt(vs, b.raw, repairHints(b, vs))
	want = `AC-2 needs a build step: add a verification step with "check_kind":"build" and the command that runs the build to task 2, which cites AC-2`
	if !strings.Contains(p, want) {
		t.Fatalf("repair prompt lacks %q:\n%s", want, p)
	}
}

func TestNoDeliverableHintOffersTheCitedCriterion(t *testing.T) {
	brief := orphanBrief("The banner renders on load", "B")
	steps := []decomposedTask{
		orphanStep("Render", []string{"AC-1"}, "src/a.ts"),
		orphanStep("B", []string{"AC-2"}, "src/b.ts"),
	}
	steps[0].Deliverable = ""
	b, vs := hintBuild(t, steps, brief)
	p := repairPrompt(vs, b.raw, repairHints(b, vs))
	want := `set "deliverable" of task 1 to the observable result it produces, for example the criterion it cites: "The banner renders on load" (AC-1)`
	if !strings.Contains(p, want) {
		t.Fatalf("repair prompt lacks %q:\n%s", want, p)
	}
}

// A hypothesis-only warning is mentioned (not as a rejection) when a repair
// round runs anyway, with the evidence the File Evidence table has.
func TestHypothesisOnlyHintIsAddedWhenARoundRuns(t *testing.T) {
	brief := orphanBrief("A", "B")
	brief.FileEvidence = map[string]plan.EvidenceRef{"src/a.ts": {Kind: plan.EvidenceRepo, Ref: "stat", Confidence: 1, Basis: plan.BasisSnapshot}}
	steps := []decomposedTask{
		orphanStep("A", []string{"AC-1"}, "src/a.ts"),
		orphanStep("B", []string{"AC-2"}, "src/b.ts"),
	}
	steps[1].Deliverable = ""
	b, vs := hintBuild(t, steps, brief)
	p := repairPrompt(vs, b.raw, repairHints(b, vs))
	for _, want := range []string{
		"Not rejected, but fix it if you can:",
		`task 1 "A": every file it names is a hypothesis (src/a.ts)`,
		`src/a.ts is in the File Evidence table: use evidence_ref "repo:stat" for it`,
	} {
		if !strings.Contains(p, want) {
			t.Errorf("repair prompt lacks %q:\n%s", want, p)
		}
	}
	if i, j := strings.Index(p, "Not rejected"), strings.Index(p, "Your previous plan (task 1"); i < 0 || j < i {
		t.Errorf("the warning section must come before the quoted plan:\n%s", p)
	}
}

// ─── hard blocks ────────────────────────────────────────────────────────────

// Every hard block of the planner is classified, has a next step, and goes
// through escalateBlock in the handler (the outbox path that raises the
// escalation prompt). The only direct block left is escalateBlock's own
// fallback for when the outbox transaction fails.
func TestEveryPlanningBlockRaisesAnEscalationPrompt(t *testing.T) {
	// A new kind must be added here and to the handler.
	names := map[planningBlock]string{
		blockContextValidation:   "blockContextValidation",
		blockProvider:            "blockProvider",
		blockEmptyBrief:          "blockEmptyBrief",
		blockDecompositionOutput: "blockDecompositionOutput",
		blockNoRepository:        "blockNoRepository",
		blockAllNonCode:          "blockAllNonCode",
		blockDependencyGraph:     "blockDependencyGraph",
		blockCommitFailed:        "blockCommitFailed",
		blockFrozenZone:          "blockFrozenZone",
	}
	if len(names) != len(planningBlocks) {
		t.Fatalf("%d planning blocks, %d named in this test", len(planningBlocks), len(names))
	}
	src := stripComments(plannerSources(t)["main.go"])
	for kind, p := range planningBlocks {
		if strings.TrimSpace(p.Rationale) == "" || !strings.Contains(strings.ToLower(p.Rationale), "retry") {
			t.Errorf("%s: the rationale must say what Retry does: %q", kind, p.Rationale)
		}
		if p.Remediation != plan.RemediationBlock {
			t.Errorf("%s: remediation %s, want block", kind, p.Remediation)
		}
		name, ok := names[kind]
		if !ok {
			t.Errorf("%s is not named in this test", kind)
			continue
		}
		if kind == blockProvider || kind == blockEmptyBrief || kind == blockDecompositionOutput {
			// Classified from the decomposition error.
			name = `decompositionBlockKind\(outcome\.decomposeErr`
		}
		if !regexpMustMatch(src, `escalateBlock\(msgCtx, idempotency\.Wrap\(msgCtx, pool\), workID, `+name+`\b`) {
			t.Errorf("%s (%s): no escalateBlock call in the handler", kind, name)
		}
	}
	var all []string
	for _, s := range plannerSources(t) {
		all = append(all, stripComments(s))
	}
	if n := strings.Count(strings.Join(all, " "), "guards.BlockWorkItem("); n != 1 {
		t.Errorf("guards.BlockWorkItem is called %d time(s) in the planner, want 1 (escalateBlock's fallback)", n)
	}
}

func TestDecompositionBlockKind(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want planningBlock
	}{
		{&decompositionOutputError{err: errors.New("bad json")}, blockDecompositionOutput},
		{&emptyBriefError{err: errors.New("no prose")}, blockEmptyBrief},
		{errors.New("provider down"), blockProvider},
	} {
		if got := decompositionBlockKind(tc.err); got != tc.want {
			t.Errorf("%v: %s, want %s", tc.err, got, tc.want)
		}
	}
}

func regexpMustMatch(s, expr string) bool { return regexp.MustCompile(expr).MatchString(s) }

func TestEscalateBlockGoesThroughTheOutbox(t *testing.T) {
	tx := &decompTx{state: models.WorkItemStatePlanning}
	workID := uuid.New()
	direct := 0
	dup := escalateBlock(context.Background(), decompDB{tx}, workID, blockNoRepository, "no repo", func(string) error { direct++; return nil })
	if dup || direct != 0 || !tx.committed {
		t.Fatalf("dup=%t direct=%d committed=%t", dup, direct, tx.committed)
	}
	var enqueued bool
	for i, sql := range tx.sqls {
		if strings.Contains(sql, "INSERT INTO outbox_events") && tx.args[i][3] == bus.TopicWorkBlocked {
			var body map[string]any
			if err := json.Unmarshal(tx.args[i][5].([]byte), &body); err != nil {
				t.Fatal(err)
			}
			enqueued = body["reason"] == "no repo"
		}
	}
	if !enqueued {
		t.Fatalf("no work.blocked with the reason: %q", tx.sqls)
	}

	// The outbox transaction fails: the block is made directly instead.
	var got string
	dup = escalateBlock(context.Background(), failingDB{}, workID, blockAllNonCode, "all non-code", func(r string) error { got = r; return nil })
	if dup || got != "all non-code" {
		t.Fatalf("fallback: dup=%t reason=%q", dup, got)
	}
}

// A redelivered technical-brief.ready: the guarded transaction reports the
// idempotency duplicate, escalateBlock says so, and nothing is blocked
// directly.
func TestEscalateBlockOnADuplicateDelivery(t *testing.T) {
	direct := 0
	dup := escalateBlock(context.Background(), duplicateDB{}, uuid.New(), blockDependencyGraph, "cycle", func(string) error { direct++; return nil })
	if !dup || direct != 0 {
		t.Fatalf("dup=%t direct=%d, want a duplicate and no direct block", dup, direct)
	}
}

type duplicateDB struct{}

func (duplicateDB) Begin(context.Context) (pgx.Tx, error) { return nil, idempotency.ErrDuplicate }

type failingDB struct{}

func (failingDB) Begin(context.Context) (pgx.Tx, error) { return nil, errors.New("database down") }
