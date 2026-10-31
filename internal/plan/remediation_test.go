package plan

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// R9 P1: every violation code declares how the planner remedies it. A code
// added without a remediation fails here instead of defaulting to "block".

// declaredViolationCodes are the ViolationCode constants declared in this
// package's non-test sources, read from the syntax tree, so a new constant
// that is missing from ViolationCodes is caught as well.
func declaredViolationCodes(t *testing.T) map[ViolationCode]string {
	t.Helper()
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	out := map[ViolationCode]string{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			g, ok := d.(*ast.GenDecl)
			if !ok || g.Tok != token.CONST {
				continue
			}
			for _, s := range g.Specs {
				vs := s.(*ast.ValueSpec)
				id, ok := vs.Type.(*ast.Ident)
				if !ok || id.Name != "ViolationCode" {
					continue
				}
				for i, n := range vs.Names {
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok {
						t.Fatalf("%s: %s is not a string literal", name, n.Name)
					}
					out[ViolationCode(strings.Trim(lit.Value, `"`))] = n.Name
				}
			}
		}
	}
	return out
}

func TestEveryViolationCodeHasARemediation(t *testing.T) {
	declared := declaredViolationCodes(t)
	if len(declared) == 0 {
		t.Fatal("no ViolationCode constants found")
	}
	listed := map[ViolationCode]bool{}
	for _, c := range ViolationCodes {
		listed[c] = true
	}
	for c, name := range declared {
		if !listed[c] {
			t.Errorf("%s (%s) is declared but not in ViolationCodes", name, c)
		}
	}
	for _, c := range ViolationCodes {
		if _, ok := declared[c]; !ok {
			t.Errorf("ViolationCodes lists %s, which is not declared", c)
		}
		p, ok := RemediationFor(c)
		if !ok {
			t.Errorf("%s has no remediation", c)
			continue
		}
		switch p.Remediation {
		case RemediationNormalise, RemediationReprompt, RemediationGap, RemediationBlock:
		default:
			t.Errorf("%s: remediation %q is not one of normalise, reprompt, gap, block", c, p.Remediation)
		}
		if strings.TrimSpace(p.Rationale) == "" || strings.Contains(p.Rationale, "\n") {
			t.Errorf("%s: the rationale must be one line, got %q", c, p.Rationale)
		}
		// A normaliser fixes only the unambiguous cases; what it leaves goes
		// to the model or becomes a gap, and the table says which.
		if p.Remediation == RemediationNormalise && p.Fallback != RemediationReprompt && p.Fallback != RemediationGap {
			t.Errorf("%s: a normalise code needs a fallback of reprompt or gap, got %q", c, p.Fallback)
		}
		if p.Remediation != RemediationNormalise && p.Fallback != "" {
			t.Errorf("%s: only a normalise code has a fallback", c)
		}
	}
	for c := range RemediationPolicy {
		if !listed[c] {
			t.Errorf("the policy names %s, which is not a violation code", c)
		}
	}
}

// An unknown code is not silently given a remediation.
func TestRemediationForUnknownCode(t *testing.T) {
	if _, ok := RemediationFor("no_such_code"); ok {
		t.Fatal("an unknown code has a remediation")
	}
}

// The table's classes agree with the validator: a gap code is only ever a
// warning (it never rejects), and every code that rejects a plan is
// normalised or re-prompted (and blocks only once the rounds are spent).
func TestRemediationAgreesWithSeverity(t *testing.T) {
	for _, c := range ViolationCodes {
		p, _ := RemediationFor(c)
		if p.Remediation == RemediationGap && !WarningCode(c) {
			t.Errorf("%s is a gap but the validator reports it as an error", c)
		}
		if p.Remediation != RemediationGap && WarningCode(c) {
			t.Errorf("%s is a warning but its remediation is %s", c, p.Remediation)
		}
	}
}

func TestNeedsModel(t *testing.T) {
	vs := []Violation{
		{Code: CodeOrphanCriterion, Severity: SeverityError, CriterionID: "AC-1"},
		{Code: CodeHypothesisOnlyTask, Severity: SeverityWarning, TaskIndex: 2},
	}
	if NeedsModel(vs) {
		t.Error("an orphan (normalise) and a warning (gap) need no re-prompt")
	}
	vs = append(vs, Violation{Code: CodeNoDeliverable, Severity: SeverityError, TaskIndex: 1})
	if !NeedsModel(vs) {
		t.Error("no_deliverable is re-prompted")
	}
	// R9 ruling: verifier_missing is normalised when one whole-suite
	// command is known; one left after the normaliser goes to the model.
	if p, _ := RemediationFor(CodeVerifierMissing); p.Remediation != RemediationNormalise || p.Fallback != RemediationReprompt {
		t.Errorf("verifier_missing policy = %+v, want normalise with a reprompt fallback", p)
	}
	if !NeedsModel([]Violation{{Code: CodeVerifierMissing, Severity: SeverityError, CriterionID: "AC-1", CheckKind: "test"}}) {
		t.Error("a verifier_missing the normaliser left needs the model")
	}
	if NeedsModel(nil) {
		t.Error("nothing to fix needs no model")
	}
}

func TestViolationKeys(t *testing.T) {
	a := Violation{Code: CodeVerifierMissing, Severity: SeverityError, CriterionID: "AC-2", CheckKind: "build", Message: "m1"}
	b := a
	b.Message = "m2 (different wording)"
	if a.Key() != b.Key() {
		t.Errorf("the key ignores the wording: %q vs %q", a.Key(), b.Key())
	}
	c := a
	c.CheckKind = "lint"
	if a.Key() == c.Key() {
		t.Error("two missing check kinds of one criterion are two violations")
	}
	prev := []Violation{a, {Code: CodeNoDeliverable, Severity: SeverityError, TaskIndex: 3}}
	if !StrictlyFewer(prev, []Violation{b}) {
		t.Error("one of two fixed and nothing new: strictly fewer")
	}
	if StrictlyFewer(prev, prev) {
		t.Error("the same set is no progress")
	}
	if StrictlyFewer(prev, []Violation{b, {Code: CodeNoCriteria, Severity: SeverityError, TaskIndex: 4}}) {
		t.Error("a new violation is not progress, even if another was fixed")
	}
	warn := Violation{Code: CodeHypothesisOnlyTask, Severity: SeverityWarning, TaskIndex: 1}
	if !StrictlyFewer(prev, []Violation{b, warn}) {
		t.Error("warnings do not count")
	}
}

// Two different cycles are two violations: a repair that trades one cycle
// for another is not progress.
func TestDependencyCycleKeyNamesTheCycle(t *testing.T) {
	cycle := func(edges [][]int) Violation {
		t.Helper()
		vs := ValidatePlan(BriefCriteria{}, nil, DependencyGraph{Labels: []string{"a", "b", "c", "d"}, Edges: edges}, nil)
		for _, v := range vs {
			if v.Code == CodeDependencyCycle {
				return v
			}
		}
		t.Fatalf("no cycle in %v", edges)
		return Violation{}
	}
	ab := cycle([][]int{{1}, {0}, nil, nil})
	abAgain := cycle([][]int{{1}, {0}, {3}, nil})
	cd := cycle([][]int{nil, nil, {3}, {2}})
	if ab.Dependency == "" || ab.Key() != abAgain.Key() {
		t.Errorf("the same cycle must have the same key: %q vs %q", ab.Key(), abAgain.Key())
	}
	if ab.Key() == cd.Key() {
		t.Errorf("two different cycles share the key %q", ab.Key())
	}
	if StrictlyFewer([]Violation{ab}, []Violation{cd}) {
		t.Error("trading one cycle for another is not progress")
	}
}
