package verifier

import (
	"context"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/llm/tools"
	"github.com/autonomousengine/autonomous-engine/internal/plan"
)

const nonCodeReason = `cited only by task 3 "Deploy to staging", which the planner dropped (not an implementation task)`

const wantGap = "criterion AC-3 is non-code: " + nonCodeReason + "; verify manually"

// R3 P1: the planner records non-code criteria on every subtask's message;
// the requirements carry them marked, ahead of the brief's unmarked copy.
func TestLoadRequirementsReadsNonCodeCriteria(t *testing.T) {
	rb := requirementsBuilder{workID: "w", seen: map[string]bool{}}
	rb.addPlanTask("11111111-1111-1111-1111-111111111111", []byte(`{
		"task_spec": {"schema_version":1,"criterion_ids":["AC-1"],"deliverable":"d","verification":[{"check_kind":"test","command":"go test ./..."}]},
		"criteria": [{"id":"AC-1","text":"code","source":{"kind":"derived","reason":"r"},"risk":"low"}],
		"non_code_criteria": [{"id":"AC-3","text":"deployed to staging","source":{"kind":"derived","reason":"r"},"risk":"low","non_code":true,"non_code_reason":`+jsonString(nonCodeReason)+`}]}`))
	rb.addBrief([]byte(`{"criteria":{"schema_version":1,"criteria":[
		{"id":"AC-1","text":"code","source":{"kind":"derived","reason":"r"},"risk":"low"},
		{"id":"AC-3","text":"deployed to staging","source":{"kind":"derived","reason":"r"},"risk":"low"}]}}`))
	if len(rb.req.Criteria) != 2 {
		t.Fatalf("criteria = %+v", rb.req.Criteria)
	}
	c := rb.req.Criteria[1]
	if c.ID != "AC-3" || !c.NonCode || c.NonCodeReason != nonCodeReason {
		t.Fatalf("AC-3 = %+v, want it marked non-code with the planner's reason", c)
	}
}

func jsonString(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}

func nonCodeInput(t *testing.T) Input {
	in := runInput(t)
	in.Criteria = append(in.Criteria, plan.Criterion{
		ID: "AC-3", Text: "deployed to staging", Source: plan.Source{Kind: plan.SourceDerived, Reason: "r"}, Risk: plan.RiskLow,
		NonCode: true, NonCodeReason: nonCodeReason,
	})
	return in
}

// A non-code criterion is not given to the model to judge; the verdict
// records it as not_verifiable with the gap line, so the gate escalates to
// human review and the comment shows the gap.
func TestRunRecordsNonCodeCriteriaAsGaps(t *testing.T) {
	loop, reqs := scripted(approveAC1) // judges AC-1 only: AC-3 must not be asked for
	v := Run(context.Background(), Runner{Adapter: nopAdapter{}, Executor: tools.NewExecutor(t.TempDir()), loop: loop}, nonCodeInput(t))
	if v.Error != "" {
		t.Fatalf("the model's answer was rejected: %+v", v)
	}
	if len(*reqs) != 1 || strings.Contains((*reqs)[0].Messages[1].Content, "deployed to staging") {
		t.Fatalf("the non-code criterion reached the model's prompt (%d calls)", len(*reqs))
	}
	if len(v.PerCriterion) != 2 || v.PerCriterion[1].ID != "AC-3" || v.PerCriterion[1].Status != StatusNotVerifiable || v.PerCriterion[1].Note != wantGap {
		t.Fatalf("per_criterion = %+v", v.PerCriterion)
	}
	if len(v.Gaps) != 1 || v.Gaps[0] != wantGap {
		t.Fatalf("gaps = %q", v.Gaps)
	}
	d := Gate(v)
	if d.Action != ActionEscalate || !strings.Contains(d.Reason, wantGap) {
		t.Fatalf("gate = %+v, want escalate naming the gap", d)
	}
	comment := RenderComment(v, d)
	if !strings.Contains(comment, "### Recorded gaps") || !strings.Contains(comment, "- "+wantGap) {
		t.Errorf("comment lacks the gap:\n%s", comment)
	}
	if fb := RenderFeedback(v, d); !strings.Contains(fb, wantGap) {
		t.Errorf("feedback lacks the gap:\n%s", fb)
	}
}

// Without a tree (or a model) the escalation carries the gap too.
func TestEscalationWithoutATreeKeepsTheGap(t *testing.T) {
	in := nonCodeInput(t)
	in.RepoTree = ""
	in.Unavailable = "repository not available on disk"
	v := Run(context.Background(), Runner{}, in)
	if len(v.Gaps) != 1 || v.Gaps[0] != wantGap || len(v.PerCriterion) != 2 || v.PerCriterion[1].Note != wantGap {
		t.Fatalf("verdict = %+v", v)
	}
}

// R8: a criterion the planner recorded as unowned (no task owns it, no
// task's files cover it) is kept from the model and recorded as
// not_verifiable with its own gap line, so the gate escalates.
func TestRunRecordsUnownedCriteriaAsGaps(t *testing.T) {
	const gap = "criterion AC-3 is unowned after planning: the claim in landing.html is corrected; verify manually"
	rb := requirementsBuilder{workID: "w", seen: map[string]bool{}}
	rb.addPlanTask("11111111-1111-1111-1111-111111111111", []byte(`{
		"criteria": [{"id":"AC-1","text":"a","source":{"kind":"derived","reason":"r"},"risk":"low"}],
		"non_code_criteria": [{"id":"AC-3","text":"the claim in landing.html is corrected","source":{"kind":"user","quote":"q"},"risk":"low","non_code":true,"non_code_reason":"owned by no task after planning","unowned":true}]}`))
	in := runInput(t)
	in.Criteria = rb.req.Criteria
	loop, reqs := scripted(approveAC1)
	v := Run(context.Background(), Runner{Adapter: nopAdapter{}, Executor: tools.NewExecutor(t.TempDir()), loop: loop}, in)
	if v.Error != "" {
		t.Fatalf("the model's answer was rejected: %+v", v)
	}
	if len(*reqs) != 1 || strings.Contains((*reqs)[0].Messages[1].Content, "landing.html is corrected") {
		t.Fatalf("the unowned criterion reached the model's prompt")
	}
	if len(v.PerCriterion) != 2 || v.PerCriterion[1].ID != "AC-3" || v.PerCriterion[1].Status != StatusNotVerifiable || v.PerCriterion[1].Note != gap {
		t.Fatalf("per_criterion = %+v", v.PerCriterion)
	}
	if len(v.Gaps) != 1 || v.Gaps[0] != gap {
		t.Fatalf("gaps = %q", v.Gaps)
	}
	if d := Gate(v); d.Action != ActionEscalate || !strings.Contains(d.Reason, gap) {
		t.Fatalf("gate = %+v", d)
	}
}

// R13 D3: a PR-text criterion (the planner marks it non-code with
// plan.PRTextReason) is read from the plan task's non_code_criteria, kept
// from the model and recorded as not_verifiable with its gap line, so
// review sees it next to the PR description that carries its text.
func TestRunRecordsPRTextCriteriaAsGaps(t *testing.T) {
	const text = "Explicitly state in the pull request description that the repository owner must obtain a TileHost API key and set `TILES_API_KEY`."
	const gap = "criterion AC-11 is non-code: satisfied by the PR text; verify manually"
	rb := requirementsBuilder{workID: "w", seen: map[string]bool{}}
	rb.addPlanTask("11111111-1111-1111-1111-111111111111", []byte(`{
		"criteria": [{"id":"AC-1","text":"a","source":{"kind":"derived","reason":"r"},"risk":"low"}],
		"non_code_criteria": [{"id":"AC-11","text":`+jsonString(text)+`,"source":{"kind":"derived","reason":"deployment"},"risk":"low","non_code":true,"non_code_reason":`+jsonString(plan.PRTextReason)+`}]}`))
	in := runInput(t)
	in.Criteria = rb.req.Criteria
	loop, reqs := scripted(approveAC1)
	v := Run(context.Background(), Runner{Adapter: nopAdapter{}, Executor: tools.NewExecutor(t.TempDir()), loop: loop}, in)
	if v.Error != "" {
		t.Fatalf("the model's answer was rejected: %+v", v)
	}
	if len(*reqs) != 1 || strings.Contains((*reqs)[0].Messages[1].Content, "TileHost API key") {
		t.Fatalf("the PR-text criterion reached the model's prompt")
	}
	if len(v.PerCriterion) != 2 || v.PerCriterion[1].ID != "AC-11" || v.PerCriterion[1].Status != StatusNotVerifiable || v.PerCriterion[1].Note != gap {
		t.Fatalf("per_criterion = %+v", v.PerCriterion)
	}
	if len(v.Gaps) != 1 || v.Gaps[0] != gap {
		t.Fatalf("gaps = %q", v.Gaps)
	}
	if comment := RenderComment(v, Gate(v)); !strings.Contains(comment, "- "+gap) {
		t.Errorf("comment lacks the gap:\n%s", comment)
	}
}

// The model cannot write gaps: they come from the requirements only.
func TestParseVerdictDropsModelGaps(t *testing.T) {
	v, err := ParseVerdict(`{"schema_version":1,"per_criterion":[{"id":"AC-1","status":"met","evidence":["a.go:1"],"note":""}],"defects":[],"overall":"approve","gaps":["none"]}`,
		[]string{"AC-1"}, Grounding{PathExists: func(string) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Gaps) != 0 {
		t.Fatalf("gaps = %q", v.Gaps)
	}
}

// R3 review: only the planner's non_code_criteria carry a trusted mark; a
// mark on a cited criterion or in the brief is stripped, so the model
// judges that criterion as usual.
func TestRequirementsStripUntrustedNonCodeMarks(t *testing.T) {
	rb := requirementsBuilder{workID: "w", seen: map[string]bool{}}
	rb.addPlanTask("11111111-1111-1111-1111-111111111111", []byte(`{
		"criteria": [{"id":"AC-1","text":"code","source":{"kind":"derived","reason":"r"},"risk":"low","non_code":true,"non_code_reason":"x"}]}`))
	rb.addBrief([]byte(`{"criteria":{"schema_version":1,"criteria":[
		{"id":"AC-2","text":"docs","source":{"kind":"derived","reason":"r"},"risk":"low","non_code":true,"non_code_reason":"y"}]}}`))
	if len(rb.req.Criteria) != 2 {
		t.Fatalf("criteria = %+v", rb.req.Criteria)
	}
	for _, c := range rb.req.Criteria {
		if c.NonCode || c.NonCodeReason != "" {
			t.Errorf("%s kept an untrusted non-code mark", c.ID)
		}
	}
}
