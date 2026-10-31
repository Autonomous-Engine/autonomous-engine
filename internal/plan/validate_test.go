package plan

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/evidence"
)

// validateFixture is one file under testdata/validate: a plan, the paths
// that exist at the snapshot per repository, and the violations expected.
type validateFixture struct {
	Brief    BriefCriteria       `json:"brief"`
	Tasks    []TaskSpecWithFiles `json:"tasks"`
	Deps     DependencyGraph     `json:"deps"`
	Existing map[string][]string `json:"existing"`
	Want     []struct {
		Code        ViolationCode `json:"code"`
		Severity    Severity      `json:"severity"`
		TaskIndex   int           `json:"task_index"`
		CriterionID string        `json:"criterion_id"`
		Contains    string        `json:"contains"`
	} `json:"want"`
}

func (f validateFixture) stat(repoID, path string) bool {
	for _, p := range f.Existing[repoID] {
		if p == path {
			return true
		}
	}
	return false
}

func loadValidateFixtures(t *testing.T) map[string]validateFixture {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("testdata", "validate", "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no fixtures under testdata/validate (%v)", err)
	}
	out := map[string]validateFixture{}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var f validateFixture
		dec := json.NewDecoder(strings.NewReader(string(data)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&f); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		if err := f.Brief.Validate(); err != nil {
			t.Fatalf("%s: the fixture's brief is invalid: %v", file, err)
		}
		out[strings.TrimSuffix(filepath.Base(file), ".json")] = f
	}
	return out
}

// TestValidatePlanFixtures: the valid plan has no violation, and each
// single-violation fixture produces exactly the expected violation with a
// message specific enough to re-prompt.
func TestValidatePlanFixtures(t *testing.T) {
	fixtures := loadValidateFixtures(t)
	if _, ok := fixtures["valid"]; !ok {
		t.Fatal("no valid.json fixture")
	}
	names := make([]string, 0, len(fixtures))
	for n := range fixtures {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		f := fixtures[name]
		t.Run(name, func(t *testing.T) {
			got := ValidatePlan(f.Brief, f.Tasks, f.Deps, f.stat)
			if len(got) != len(f.Want) {
				t.Fatalf("got %d violations, want %d:\n%s", len(got), len(f.Want), dump(got))
			}
			for i, w := range f.Want {
				g := got[i]
				wantSev := w.Severity
				if wantSev == "" {
					wantSev = SeverityError
				}
				if g.Code != w.Code || g.Severity != wantSev || g.TaskIndex != w.TaskIndex || g.CriterionID != w.CriterionID {
					t.Errorf("violation %d = %+v, want code=%s severity=%s task=%d criterion=%q", i, g, w.Code, wantSev, w.TaskIndex, w.CriterionID)
				}
				if !strings.Contains(g.Message, w.Contains) {
					t.Errorf("violation %d message\n  %s\ndoes not contain\n  %s", i, g.Message, w.Contains)
				}
			}
		})
	}
}

// TestValidatePlanCoversEveryCode: every rule has a fixture that triggers it.
func TestValidatePlanCoversEveryCode(t *testing.T) {
	seen := map[ViolationCode]bool{}
	for _, f := range loadValidateFixtures(t) {
		for _, w := range f.Want {
			seen[w.Code] = true
		}
	}
	for _, c := range ViolationCodes {
		if !seen[c] {
			t.Errorf("no fixture under testdata/validate triggers %s", c)
		}
	}
}

func dump(vs []Violation) string {
	b, _ := json.MarshalIndent(vs, "", "  ")
	return string(b)
}

func TestValidatePlanUnknownCriterionMessage(t *testing.T) {
	brief := BriefCriteria{SchemaVersion: SchemaVersion}
	for i := 1; i <= 6; i++ {
		brief.Criteria = append(brief.Criteria, Criterion{ID: "AC-" + string(rune('0'+i)), Text: "x", Source: Source{Kind: SourceDerived, Reason: "r"}, Risk: RiskLow})
	}
	tasks := []TaskSpecWithFiles{
		{Index: 1, Title: "Own all", Spec: TaskSpec{CriterionIDs: []string{"AC-1", "AC-2", "AC-3", "AC-4", "AC-5", "AC-6"}, Deliverable: "d", Verification: []VerificationStep{{CheckKind: evidence.CheckKindOther, Description: "x"}}}},
		{Index: 2, Title: "Add handler", Spec: TaskSpec{CriterionIDs: []string{"AC-7"}, Deliverable: "d", Verification: []VerificationStep{{CheckKind: evidence.CheckKindOther, Description: "x"}}}},
	}
	got := ValidatePlan(brief, tasks, DependencyGraph{}, nil)
	want := `task 2 "Add handler" cites AC-7, which does not exist; known ids: AC-1..AC-6`
	if len(got) != 1 || got[0].Message != want {
		t.Fatalf("got %s\nwant one violation %q", dump(got), want)
	}
}

func TestValidatePlanLegacyBriefRaisesNoCriteriaRule(t *testing.T) {
	tasks := []TaskSpecWithFiles{{Index: 1, Title: "Do it", Spec: TaskSpec{Deliverable: "d", Verification: []VerificationStep{{CheckKind: evidence.CheckKindTest, Command: "go test ./..."}}}}}
	if got := ValidatePlan(BriefCriteria{}, tasks, DependencyGraph{}, nil); len(got) != 0 {
		t.Fatalf("a brief without criteria must not raise criteria rules: %s", dump(got))
	}
}

func TestValidatePlanNilStatSkipsStatRules(t *testing.T) {
	f := loadValidateFixtures(t)["stale_evidence"]
	if got := ValidatePlan(f.Brief, f.Tasks, f.Deps, nil); len(got) != 0 {
		t.Fatalf("nil snapshotStat must skip stat rules: %s", dump(got))
	}
}

func TestValidatePlanBriefEvidenceCoversABarePath(t *testing.T) {
	f := loadValidateFixtures(t)["valid"]
	// calculate.go claimed bare: the brief's repo evidence covers it.
	f.Tasks[0].Spec.Files[0].Evidence = EvidenceRef{}
	if got := ValidatePlan(f.Brief, f.Tasks, f.Deps, f.stat); len(got) != 0 {
		t.Fatalf("a bare path the brief evidences is not unevidenced: %s", dump(got))
	}
	// ... and when that brief evidence is stale, it says so.
	f.Existing = map[string][]string{"repo-1": {"handler.go"}}
	got := ValidatePlan(f.Brief, f.Tasks, f.Deps, f.stat)
	if len(got) != 1 || got[0].Code != CodeStaleEvidence || !strings.Contains(got[0].Message, "relies on the brief's repo evidence") {
		t.Fatalf("got %s", dump(got))
	}
}

func TestValidatePlanVerifierKindsCompareNormalised(t *testing.T) {
	f := loadValidateFixtures(t)["valid"]
	f.Brief.Criteria[0].RequiredChecks = []evidence.CheckKind{" Test "}
	f.Tasks[0].PolicyChecks = []evidence.CheckKind{"TEST"}
	if got := ValidatePlan(f.Brief, f.Tasks, f.Deps, f.stat); len(got) != 0 {
		t.Fatalf("check kinds must compare normalised: %s", dump(got))
	}
}

func TestValidatePlanAmbiguousDependency(t *testing.T) {
	f := loadValidateFixtures(t)["valid"]
	f.Deps.Unresolved = []UnresolvedDependency{{Subtask: "Implement in web", Dependency: "Setup", Through: "Prep", Ambiguous: []string{"Implement in api", "Implement in web"}}}
	got := ValidatePlan(f.Brief, f.Tasks, f.Deps, f.stat)
	want := `subtask "Implement in web" depends on "Setup" (through dropped step "Prep"), which names steps in 2 different subtasks ("Implement in api", "Implement in web")`
	if len(got) != 1 || got[0].Code != CodeUnknownDependency || !strings.HasPrefix(got[0].Message, want) {
		t.Fatalf("got %s", dump(got))
	}
}

func TestHasErrorsIgnoresWarnings(t *testing.T) {
	vs := []Violation{{Code: CodeHypothesisOnlyTask, Severity: SeverityWarning}}
	if HasErrors(vs) || len(Warnings(vs)) != 1 || len(Errors(vs)) != 0 {
		t.Fatal("a warning must not count as an error")
	}
	vs = append(vs, Violation{Code: CodeNoDeliverable, Severity: SeverityError})
	if !HasErrors(vs) || len(Errors(vs)) != 1 {
		t.Fatal("an error must count")
	}
}

func TestSummarizeViolations(t *testing.T) {
	var vs []Violation
	for i := 0; i < 7; i++ {
		vs = append(vs, Violation{Code: CodeNoDeliverable, Message: "m" + string(rune('0'+i))})
	}
	got := SummarizeViolations(vs, 5)
	want := "[no_deliverable] m0; [no_deliverable] m1; [no_deliverable] m2; [no_deliverable] m3; [no_deliverable] m4 (and 2 more)"
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	if got := SummarizeViolations(vs[:2], 5); strings.Contains(got, "more") {
		t.Fatalf("no count when nothing is cut: %q", got)
	}
}

func TestCriterionIDList(t *testing.T) {
	cases := map[string][]string{
		"none":                    nil,
		"AC-1..AC-6":              {"AC-1", "AC-2", "AC-3", "AC-4", "AC-5", "AC-6"},
		"AC-1..AC-3, AC-5":        {"AC-5", "AC-1", "AC-2", "AC-3"},
		"AC-1, AC-2, AC-9":        {"AC-1", "AC-2", "AC-9"},
		"AC-4, AC-10..AC-12, x-1": {"AC-4", "AC-10", "AC-11", "AC-12", "x-1"},
	}
	for want, ids := range cases {
		if got := CriterionIDList(ids); got != want {
			t.Errorf("CriterionIDList(%v) = %q, want %q", ids, got, want)
		}
	}
}

func TestFindDependencyCycle(t *testing.T) {
	cases := []struct {
		name string
		deps [][]int
		want bool
	}{
		{"empty", nil, false},
		{"chain", [][]int{nil, {0}, {1}}, false},
		{"diamond", [][]int{nil, {0}, {0}, {1, 2}}, false},
		{"self", [][]int{{0}}, true},
		{"back edge", [][]int{{2}, {0}, {1}}, true},
		{"out of range edge is ignored", [][]int{{5}}, false},
	}
	for _, tc := range cases {
		if got := FindDependencyCycle(tc.deps) != nil; got != tc.want {
			t.Errorf("%s: cycle=%t, want %t", tc.name, got, tc.want)
		}
	}
}

// --- folding derived duplicates (pre-validation normalisation) ---

func TestFoldDerivedDuplicatesIntoUserCriterion(t *testing.T) {
	user := Criterion{ID: "AC-1", Text: "Apply the fixed discount before the percentage discount",
		Source: Source{Kind: SourceUser, Quote: "fixed before percentage"}, Risk: RiskMedium,
		Files:          []FileRef{{Path: "calculate.go", Evidence: EvidenceRef{Kind: EvidenceHypothesis}}},
		RequiredChecks: []evidence.CheckKind{evidence.CheckKindTest}}
	paraphrase := Criterion{ID: "AC-2", Text: "The fixed discount is applied before the percentage discount",
		Source: Source{Kind: SourceDerived, Reason: "restates the request"}, Risk: RiskHigh,
		Files:          []FileRef{{Path: "calculate.go", Evidence: EvidenceRef{Kind: EvidenceRepo, Ref: "c3", Confidence: 1}}, {Path: "calculate_test.go", Evidence: EvidenceRef{Kind: EvidenceNew}}},
		Contracts:      []ContractRef{{Type: "rest_openapi", Name: "POST /api/calculate", Evidence: EvidenceRef{Kind: EvidenceCodex, Ref: "c1", Confidence: 0.9}}},
		RequiredChecks: []evidence.CheckKind{evidence.CheckKindTest, evidence.CheckKindBuild}}
	distinct := Criterion{ID: "AC-3", Text: "Existing tax calculation is unchanged",
		Source: Source{Kind: SourceDerived, Reason: "shared code path"}, Risk: RiskLow}
	in := []Criterion{user, paraphrase, distinct}

	kept, folds := FoldDerivedDuplicates(in)
	if len(folds) != 1 || folds[0].From != "AC-2" || folds[0].Into != "AC-1" || folds[0].Similarity < FoldJaccard {
		t.Fatalf("folds = %+v", folds)
	}
	if len(kept) != 2 || kept[0].ID != "AC-1" || kept[1].ID != "AC-3" {
		t.Fatalf("kept = %+v", kept)
	}
	got := kept[0]
	if got.Risk != RiskHigh || got.Source.Kind != SourceUser || got.Text != user.Text {
		t.Errorf("merged criterion keeps the user's text and source and takes the higher risk: %+v", got)
	}
	if len(got.Files) != 2 || got.Files[0].Evidence.Kind != EvidenceRepo {
		t.Errorf("files must union keeping the stronger evidence: %+v", got.Files)
	}
	if len(got.Contracts) != 1 || !reflect.DeepEqual(got.RequiredChecks, []evidence.CheckKind{evidence.CheckKindTest, evidence.CheckKindBuild}) {
		t.Errorf("contracts/checks not merged: %+v", got)
	}
	if in[0].Risk != RiskMedium || len(in[0].Files) != 1 || len(in) != 3 {
		t.Error("the input must not be modified")
	}

	// The folded id is rewritten in citations and never orphaned on its own.
	if ids := RewriteCriterionIDs([]string{"AC-2", "AC-1", "AC-3"}, folds); !reflect.DeepEqual(ids, []string{"AC-1", "AC-3"}) {
		t.Errorf("RewriteCriterionIDs = %v", ids)
	}
	brief := BriefCriteria{SchemaVersion: SchemaVersion, Criteria: kept}
	tasks := []TaskSpecWithFiles{{Index: 1, Title: "Fix", Spec: TaskSpec{
		CriterionIDs: RewriteCriterionIDs([]string{"AC-2", "AC-3"}, folds), Deliverable: "d",
		Verification: []VerificationStep{{CheckKind: evidence.CheckKindTest, Command: "go test ./..."}, {CheckKind: evidence.CheckKindBuild, Command: "go build ./..."}}}}}
	if vs := ValidatePlan(brief, tasks, DependencyGraph{}, nil); len(vs) != 0 {
		t.Fatalf("after folding the plan is valid, got %s", dump(vs))
	}
}

func TestFoldDerivedDuplicatesLeavesDistinctAndUserCriteria(t *testing.T) {
	in := []Criterion{
		{ID: "AC-1", Text: "Apply the fixed discount first", Source: Source{Kind: SourceUser, Quote: "q"}, Risk: RiskLow},
		{ID: "AC-2", Text: "Apply the fixed discount first", Source: Source{Kind: SourceUser, Quote: "q2"}, Risk: RiskLow},
		{ID: "AC-3", Text: "Log every calculation request", Source: Source{Kind: SourceDerived, Reason: "r"}, Risk: RiskLow},
	}
	kept, folds := FoldDerivedDuplicates(in)
	if len(folds) != 0 || len(kept) != 3 {
		t.Fatalf("nothing to fold: kept=%d folds=%+v", len(kept), folds)
	}
	if _, folds := FoldDerivedDuplicates([]Criterion{{ID: "AC-1", Text: "x", Source: Source{Kind: SourceDerived, Reason: "r"}}}); folds != nil {
		t.Fatal("no user criterion, nothing folds")
	}
}

func TestFoldPicksTheMostSimilarUserCriterion(t *testing.T) {
	in := []Criterion{
		{ID: "AC-1", Text: "fixed discount applied before percentage discount on orders", Source: Source{Kind: SourceUser, Quote: "a"}, Risk: RiskLow},
		{ID: "AC-2", Text: "fixed discount applied before percentage discount", Source: Source{Kind: SourceUser, Quote: "b"}, Risk: RiskLow},
		{ID: "AC-3", Text: "the fixed discount applied before percentage discount", Source: Source{Kind: SourceDerived, Reason: "r"}, Risk: RiskLow},
	}
	_, folds := FoldDerivedDuplicates(in)
	if len(folds) != 1 || folds[0].Into != "AC-2" {
		t.Fatalf("folds = %+v, want AC-3 into AC-2", folds)
	}
}

func TestTokenJaccard(t *testing.T) {
	if TokenJaccard("", "") != 1 || TokenJaccard("a b", "a b") != 1 || TokenJaccard("a", "b") != 0 {
		t.Fatal("jaccard basics")
	}
	if got := TokenJaccard("a b c", "a b d"); got != 0.5 {
		t.Fatalf("got %v", got)
	}
}
