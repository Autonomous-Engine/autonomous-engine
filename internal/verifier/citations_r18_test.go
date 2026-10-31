package verifier

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/llm/tools"
	"github.com/autonomousengine/autonomous-engine/internal/plan"
	"github.com/google/uuid"
)

// R18 E2: live-run attempt 15's verifier (version 5) answered twice. The
// first answer cited the tool name "git_diff" on AC-1, AC-2, AC-3 and AC-5;
// the re-prompt fixed those, but the second cited the bare path
// "tests/unit/server.test.ts" on AC-5, and that one citation voided the
// whole verdict: all seven criteria not_verifiable, no defects, escalate.
// Read-only export in testdata/attempt15/verifier_calls.json.

type attempt15Verifier struct {
	CriterionIDs []string          `json:"criterion_ids"`
	CriteriaText map[string]string `json:"criteria_text"`
	FirstAnswer  string            `json:"first_answer"`
	Reprompt     string            `json:"reprompt"`
	SecondAnswer string            `json:"second_answer"`
}

func loadAttempt15Verifier(t *testing.T) attempt15Verifier {
	t.Helper()
	b, err := os.ReadFile("testdata/attempt15/verifier_calls.json")
	if err != nil {
		t.Fatal(err)
	}
	var f attempt15Verifier
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.CriterionIDs) != 7 || f.FirstAnswer == "" || f.SecondAnswer == "" {
		t.Fatalf("fixture: %+v", f.CriterionIDs)
	}
	return f
}

// attempt15Input is the verification input with the attempt's criteria
// and a tree at head holding files (repository-relative).
func attempt15Input(t *testing.T, f attempt15Verifier, files ...string) Input {
	t.Helper()
	tree := t.TempDir()
	for _, p := range files {
		full := filepath.Join(tree, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("// "+p+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var crit []plan.Criterion
	for _, id := range f.CriterionIDs {
		crit = append(crit, plan.Criterion{ID: id, Text: f.CriteriaText[id], Source: plan.Source{Kind: plan.SourceDerived, Reason: "r"}, Risk: plan.RiskMedium})
	}
	return Build(Sources{WorkID: uuid.New(), Requirements: Requirements{Criteria: crit}, RepoTree: tree})
}

var attempt15Head = []string{"src/config/env.ts", "src/server/index.ts", "public/index.html", "landing/index.html"}

func statusesOf(v Verdict) map[string]Status {
	out := map[string]Status{}
	for _, r := range v.PerCriterion {
		out[r.ID] = r.Status
	}
	return out
}

func resultOf(v Verdict, id string) CriterionResult {
	for _, r := range v.PerCriterion {
		if r.ID == id {
			return r
		}
	}
	return CriterionResult{}
}

// The recorded answers replayed through Run: when AC-5's bare path is not
// a file at head, AC-5 alone becomes not_verifiable ("evidence ref
// invalid: …"); the other six keep the statuses the model gave and the
// three defects stay. The re-prompt names only the offending citations.
func TestAttempt15BadCitationVoidsOnlyItsCriterion(t *testing.T) {
	f := loadAttempt15Verifier(t)
	loop, reqs := scripted(f.FirstAnswer, f.SecondAnswer)
	v := Run(context.Background(), Runner{Adapter: nopAdapter{}, Executor: tools.NewExecutor(t.TempDir()), loop: loop}, attempt15Input(t, f, attempt15Head...))
	if len(*reqs) != 2 {
		t.Fatalf("calls = %d, want 2", len(*reqs))
	}
	if v.Error != "" {
		t.Fatalf("the whole verdict was voided: %s", v.Error)
	}
	want := map[string]Status{"AC-1": StatusNotMet, "AC-2": StatusNotMet, "AC-3": StatusNotMet, "AC-4": StatusNotVerifiable, "AC-7": StatusNotVerifiable, "AC-5": StatusNotVerifiable, "AC-6": StatusNotVerifiable}
	got := statusesOf(v)
	for id, s := range want {
		if got[id] != s {
			t.Errorf("%s = %q, want %q", id, got[id], s)
		}
	}
	ac5 := resultOf(v, "AC-5")
	if !strings.HasPrefix(ac5.Note, "evidence ref invalid: ") || !strings.Contains(ac5.Note, `"tests/unit/server.test.ts"`) {
		t.Errorf("AC-5 note = %q", ac5.Note)
	}
	for _, id := range []string{"AC-1", "AC-2", "AC-3", "AC-4", "AC-6", "AC-7"} {
		if strings.Contains(resultOf(v, id).Note, "evidence ref invalid") {
			t.Errorf("%s was invalidated: %q", id, resultOf(v, id).Note)
		}
	}
	if len(v.Defects) != 3 || v.Overall != OverallRequestChanges {
		t.Errorf("defects = %d, overall = %q", len(v.Defects), v.Overall)
	}
	reprompt := (*reqs)[1].Messages[len((*reqs)[1].Messages)-1].Content
	for _, s := range []string{`AC-1: "git_diff"`, `AC-2: "git_diff"`, `AC-3: "git_diff"`, `AC-5: "git_diff"`} {
		if !strings.Contains(reprompt, s) {
			t.Errorf("re-prompt does not name %s:\n%s", s, reprompt)
		}
	}
	for _, s := range []string{"AC-4", "AC-6", "AC-7"} {
		if strings.Contains(reprompt, s) {
			t.Errorf("re-prompt names %s, which had no bad citation:\n%s", s, reprompt)
		}
	}
	// The gate now reads the real findings. Since verifier 7 the model's
	// "unvalidated_security_path" (the undefined apiKey) is an ordinary
	// defect, not a security escalation. The verdict still escalates: the
	// model left AC-4/AC-6/AC-7 not_verifiable itself (gate step 3), and
	// past those its 3 not_met + 3 major items exceed the fix-round bound.
	d := Gate(v)
	t.Logf("attempt-15 gate on the R18 verdict: %s (%s)", d.Action, d.Reason)
	if d.Action != ActionEscalate || strings.Contains(d.Reason, "security") || strings.Contains(d.Reason, "unparseable") ||
		!strings.Contains(d.Reason, "could not verify: AC-4, AC-6, AC-7") {
		t.Errorf("gate = %+v", d)
	}
	t.Setenv(FixMaxItemsEnv, "")
	fr := fixRound(v, "attempt 15")
	t.Logf("attempt-15 fix round past step 3: %s (%s)", fr.Action, fr.Reason)
	if fr.Action != ActionEscalate || !strings.Contains(fr.Reason, "too many problems for an automatic fix round (6 not-met criteria and critical/major defects, limit 5") {
		t.Errorf("fix round = %+v", fr)
	}
}

// R18 review M2: only the seven security categories escalate a critical or
// major defect; anything else (legacy unvalidated_security_path, an
// undefined variable) goes through the fix round.
func TestOnlySecurityCategoriesEscalate(t *testing.T) {
	notMet := CriterionResult{ID: "AC-1", Status: StatusNotMet, Evidence: []Ref{"public/index.html:837"}, Note: "apiKey is undefined"}
	for _, cat := range []string{"undefined_variable", "unvalidated_security_path", "security", "missing_error_path"} {
		v := Verdict{PerCriterion: []CriterionResult{notMet}, Defects: []Defect{{Severity: SeverityMajor, File: "public/index.html", Line: 837, Category: cat, Description: "apiKey is not defined"}}, Overall: OverallRequestChanges}
		if d := Gate(v); d.Action != ActionRequestChanges {
			t.Errorf("%s major → %+v, want request_changes", cat, d)
		}
	}
	for _, cat := range []string{"injection", "secrets", "authz", "ssrf", "path_traversal", "deserialization", "xss", "Path-Traversal", "path traversal", " XSS "} {
		v := Verdict{PerCriterion: []CriterionResult{notMet}, Defects: []Defect{{Severity: SeverityMajor, File: "src/db.ts", Line: 9, Category: cat, Description: "d"}}, Overall: OverallRequestChanges}
		if d := Gate(v); d.Action != ActionEscalate || !strings.Contains(d.Reason, "security-sensitive defect") {
			t.Errorf("%q major → %+v, want escalate", cat, d)
		}
	}
	minor := Verdict{PerCriterion: []CriterionResult{notMet}, Defects: []Defect{{Severity: SeverityMinor, File: "src/db.ts", Line: 9, Category: "injection", Description: "d"}}, Overall: OverallRequestChanges}
	if d := Gate(minor); d.Action != ActionRequestChanges {
		t.Errorf("minor injection → %+v, want request_changes", d)
	}
	if SecurityCategory("injection_like") || SecurityCategory("") || !SecurityCategory("AuthZ") {
		t.Error("SecurityCategory must be an exact match")
	}
}

// With AC-5's file present at head the bare path is file-level evidence:
// all seven criteria keep the model's statuses.
func TestAttempt15BarePathAtHeadIsFileLevelEvidence(t *testing.T) {
	f := loadAttempt15Verifier(t)
	loop, _ := scripted(f.FirstAnswer, f.SecondAnswer)
	v := Run(context.Background(), Runner{Adapter: nopAdapter{}, Executor: tools.NewExecutor(t.TempDir()), loop: loop},
		attempt15Input(t, f, append(attempt15Head, "tests/unit/server.test.ts")...))
	if v.Error != "" {
		t.Fatalf("verdict voided: %s", v.Error)
	}
	if ac5 := resultOf(v, "AC-5"); ac5.Status != StatusNotMet || len(ac5.Evidence) != 1 || ac5.Evidence[0] != "tests/unit/server.test.ts" {
		t.Fatalf("AC-5 = %+v", ac5)
	}
	if len(v.InvalidEvidence) != 0 {
		t.Fatalf("InvalidEvidence = %v", v.InvalidEvidence)
	}
}

// twoCriteria is a verdict on AC-1 (not_met, located) and AC-2 (met, citing
// ref); overall request_changes.
func twoCriteria(ref string) string {
	return `{"schema_version":1,"per_criterion":[` +
		`{"id":"AC-1","status":"not_met","evidence":["a.go:1"],"note":"Page returns the wrong slice"},` +
		`{"id":"AC-2","status":"met","evidence":["` + ref + `"],"note":""}],` +
		`"defects":[],"overall":"request_changes"}`
}

func twoCriteriaInput(t *testing.T) Input {
	t.Helper()
	in := runInput(t)
	in.Criteria = append(in.Criteria, plan.Criterion{ID: "AC-2", Text: "b", Source: plan.Source{Kind: plan.SourceDerived, Reason: "r"}, Risk: plan.RiskLow})
	return in
}

// A not_met criterion next to a bad citation elsewhere requests changes:
// the bad citation voids AC-2 alone, after a re-prompt naming only it.
func TestNotMetNextToABadCitationRequestsChanges(t *testing.T) {
	loop, reqs := scripted(twoCriteria("git_diff"), twoCriteria("git_diff"))
	v := Run(context.Background(), Runner{Adapter: nopAdapter{}, Executor: tools.NewExecutor(t.TempDir()), loop: loop}, twoCriteriaInput(t))
	if v.Error != "" || resultOf(v, "AC-1").Status != StatusNotMet || resultOf(v, "AC-2").Status != StatusNotVerifiable {
		t.Fatalf("verdict = %+v", v)
	}
	if len(v.InvalidEvidence) != 1 || v.InvalidEvidence[0] != "AC-2" || !strings.Contains(resultOf(v, "AC-2").Note, "(the verifier said met") {
		t.Fatalf("InvalidEvidence = %v, AC-2 = %+v", v.InvalidEvidence, resultOf(v, "AC-2"))
	}
	reprompt := (*reqs)[1].Messages[len((*reqs)[1].Messages)-1].Content
	if !strings.Contains(reprompt, `- AC-2: "git_diff" is neither`) || strings.Contains(reprompt, "AC-1") {
		t.Fatalf("re-prompt = %q", reprompt)
	}
	d := Gate(v)
	if d.Action != ActionRequestChanges || !strings.Contains(d.Reason, "criteria not met: AC-1") || !strings.Contains(d.Reason, "evidence ref invalid (a human checks): AC-2") {
		t.Fatalf("gate = %+v", d)
	}
	// The verdict survives its artifact round trip with the same decision.
	b, _ := json.Marshal(v)
	var back Verdict
	if err := json.Unmarshal(b, &back); err != nil || Gate(back).Action != ActionRequestChanges {
		t.Fatalf("round trip: %v %+v", err, Gate(back))
	}
}

// Only a voided criterion left open (everything else met): escalate, never
// approve. A re-prompt answer that does not parse leaves the first answer
// standing, its bad citation voiding its own criterion.
func TestVoidedCriterionAloneEscalates(t *testing.T) {
	approve := strings.Replace(strings.Replace(twoCriteria("git_diff"), `"not_met","evidence":["a.go:1"]`, `"met","evidence":["a.go:1"]`, 1), `"request_changes"`, `"approve"`, 1)
	loop, reqs := scripted(approve, "I cannot answer in JSON.")
	v := Run(context.Background(), Runner{Adapter: nopAdapter{}, Executor: tools.NewExecutor(t.TempDir()), loop: loop}, twoCriteriaInput(t))
	if len(*reqs) != 2 || v.Error != "" || resultOf(v, "AC-1").Status != StatusMet || resultOf(v, "AC-2").Status != StatusNotVerifiable {
		t.Fatalf("calls = %d, verdict = %+v", len(*reqs), v)
	}
	d := Gate(v)
	if d.Action != ActionEscalate || !strings.Contains(d.Reason, "criteria whose evidence was invalid need human review (criteria AC-2)") {
		t.Fatalf("gate = %+v", d)
	}
	// A structurally broken first answer still escalates as before.
	loop, _ = scripted("APPROVE", "still not JSON")
	if v := Run(context.Background(), Runner{Adapter: nopAdapter{}, Executor: tools.NewExecutor(t.TempDir()), loop: loop}, twoCriteriaInput(t)); v.Error == "" || Gate(v).Action != ActionEscalate {
		t.Fatalf("verdict = %+v", v)
	}
}

// The model cannot mark a criterion as voided itself: invalid_evidence in
// its answer is cleared, and a not_verifiable it wrote with the same note
// is a real unverifiable one.
func TestModelCannotSetInvalidEvidence(t *testing.T) {
	in := `{"schema_version":1,"per_criterion":[{"id":"AC-1","status":"not_met","evidence":["a.go:1"],"note":"x"},` +
		`{"id":"AC-2","status":"not_verifiable","evidence":[],"note":"evidence ref invalid: \"x\""}],"defects":[],"overall":"request_changes","invalid_evidence":["AC-2"]}`
	v, err := ParseVerdict(in, []string{"AC-1", "AC-2"}, Grounding{})
	if err != nil {
		t.Fatal(err)
	}
	if len(v.InvalidEvidence) != 0 {
		t.Fatalf("model-set InvalidEvidence survived: %v", v.InvalidEvidence)
	}
	if d := Gate(v); d.Action != ActionEscalate || !strings.Contains(d.Reason, "could not verify: AC-2") {
		t.Fatalf("gate = %+v", d)
	}
}

// ParseVerdict: a bare path is valid evidence only for a file at head.
func TestBarePathEvidence(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "tests", "unit"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "tests", "unit", "server.test.ts"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g := Grounding{CallIDs: map[string]bool{"call_1": true}, PathExists: TreePathExists(root)}
	ids := []string{"AC-1", "AC-2"}
	for _, ok := range []string{"tests/unit/server.test.ts", "./tests/unit/server.test.ts", "call_1", "tests/unit/server.test.ts:1"} {
		if _, err := ParseVerdict(twoCriteria(ok), ids, g); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"tests/unit/ghost.test.ts", "git_diff", "npm test", "../etc/passwd", "-rf"} {
		if _, err := ParseVerdict(twoCriteria(bad), ids, g); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	// Without a tree a bare path cannot be confirmed.
	if _, err := ParseVerdict(twoCriteria("tests/unit/server.test.ts"), ids, Grounding{}); err == nil {
		t.Error("bare path accepted without a tree")
	}
	// R18 review m2: a directory, ".", ".." or "./" is not file-level
	// evidence, bare or with a line.
	for _, dir := range []string{"tests", "tests/unit", "tests/unit/", ".", "..", "./", "tests/unit:3"} {
		if _, err := ParseVerdict(twoCriteria(dir), ids, g); err == nil {
			t.Errorf("directory ref %q accepted", dir)
		}
	}
	exists := TreePathExists(root)
	if exists("tests/unit") || exists(".") || exists("") || !exists("tests/unit/server.test.ts") {
		t.Error("TreePathExists must accept regular files only")
	}
}

// G2 (R16) actionability: a bare-path citation is a lower grade than
// path:line — it makes a not_met criterion actionable only when the note
// also names a line or a check.
func TestBarePathIsALowerGradeReference(t *testing.T) {
	r := CriterionResult{ID: "AC-1", Status: StatusNotMet, Evidence: []Ref{"server.test.ts"}, Note: "the test is missing"}
	if criterionActionable(r, Verdict{}) {
		t.Error("a bare path alone made the criterion actionable")
	}
	r.Note = "the test is missing after line 40"
	if !criterionActionable(r, Verdict{}) {
		t.Error("bare path + a line is not actionable")
	}
	r.Note = "no `test` covers it"
	if !criterionActionable(r, Verdict{}) {
		t.Error("bare path + a check is not actionable")
	}
	r = CriterionResult{ID: "AC-1", Status: StatusNotMet, Evidence: []Ref{"server.test.ts:40"}, Note: "x"}
	if !criterionActionable(r, Verdict{}) {
		t.Error("path:line is not actionable")
	}
}
