package plan

import (
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/evidence"
)

func TestTargetsPRText(t *testing.T) {
	for s, want := range map[string]bool{
		// Asks for something to be stated in the PR text.
		"Explicitly state in the pull request description that the owner must set TILES_API_KEY": true,
		"The pull request description MUST state that the owner must obtain a TileHost API key":  true,
		"The PR description lists the new environment variable":                                  true,
		"the PR's description names the migration":                                               true,
		"The pull-request description mentions the breaking change":                              true,
		"Each of the PR descriptions states which repository it changes":                         true,
		"State in the PR descriptions that the two PRs must merge together":                      true,
		"Add GET /api/client-config and say so in the PR description":                            true,
		"The commit message states the ticket id":                                                true,
		"Commit messages reference the ticket":                                                   true,
		"Mention the migration in the commit message":                                            true,
		"The release notes mention the new flag":                                                 true,
		"Explain the breaking change in the release notes":                                       true,
		// Names the PR text, or a PR, without asking for it to say anything.
		"The pre-commit hook rejects commit messages without a ticket id":  false,
		"The release notes generator script lists merged PRs":              false,
		"Use a conventional commit message":                                false,
		"Add a release note for the deprecation":                           false,
		"Fix the bug where the PR description is truncated":                false,
		"Update README.md to document TILES_API_KEY":                       false,
		"The server logs a warning at startup when TILES_API_KEY is empty": false,
		"Add a description field to the PR model":                          false,
		"Open a pull request":                                              false,
		"The commit is signed":                                             false,
		"Add pull request description for API key":                         false,
	} {
		if got := TargetsPRText(s); got != want {
			t.Errorf("TargetsPRText(%q) = %v, want %v", s, got, want)
		}
	}
}

// The two review negatives are code work: owned by no kept step they are
// orphans (a repair round is asked for), never a PR-text gap.
func TestCommitAndReleaseToolingCriteriaStayOrphans(t *testing.T) {
	for _, text := range []string{
		"The pre-commit hook rejects commit messages without a ticket id",
		"The release notes generator script lists merged PRs",
	} {
		brief := BriefCriteria{SchemaVersion: SchemaVersion, Criteria: []Criterion{
			{ID: "AC-1", Text: "code", Source: Source{Kind: SourceUser}, Risk: RiskLow},
			{ID: "AC-2", Text: text, Source: Source{Kind: SourceUser, Quote: "q"}, Risk: RiskLow},
		}}
		vs := ValidatePlan(brief, []TaskSpecWithFiles{codeTask(1, "AC-1")}, DependencyGraph{}, nil)
		var orphan bool
		for _, v := range Errors(vs) {
			orphan = orphan || (v.Code == CodeOrphanCriterion && v.CriterionID == "AC-2")
		}
		if !orphan {
			t.Errorf("%q: violations = %+v, want orphan_criterion AC-2", text, vs)
		}
		if marks := NonCodeCriteria(brief, nil); len(marks) != 0 {
			t.Errorf("%q: NonCodeCriteria = %v", text, marks)
		}
	}
}

// A PR-text criterion that no kept step owns is non-code with PRTextReason,
// not an orphan: when no step cites it, when only a step dropped as non-code
// cites it, and when only a step dropped for another reason (test-only
// without a runner) cites it.
func TestPRTextCriterionIsNonCode(t *testing.T) {
	brief := BriefCriteria{SchemaVersion: SchemaVersion, Criteria: []Criterion{
		{ID: "AC-1", Text: "code", Source: Source{Kind: SourceUser}, Risk: RiskLow},
		{ID: "AC-2", Text: "The pull request description tells the owner to set TILES_API_KEY", Source: Source{Kind: SourceDerived, Reason: "r"}, Risk: RiskLow, RequiredChecks: nil},
	}}
	dropped := TaskSpecWithFiles{Index: 2, Title: "Add pull request description", Spec: TaskSpec{CriterionIDs: []string{"AC-2"}},
		Dropped: "not an implementation task", NonCode: true}
	testOnly := TaskSpecWithFiles{Index: 2, Title: "Test the description", Spec: TaskSpec{CriterionIDs: []string{"AC-2"}},
		Dropped: "test-only, and repository r has no test runner"}
	for name, tasks := range map[string][]TaskSpecWithFiles{
		"uncited":                          {codeTask(1, "AC-1")},
		"cited by a non-code drop":         {codeTask(1, "AC-1"), dropped},
		"cited by a step dropped as tests": {codeTask(1, "AC-1"), testOnly},
	} {
		t.Run(name, func(t *testing.T) {
			vs := ValidatePlan(brief, tasks, DependencyGraph{}, nil)
			if HasErrors(vs) {
				t.Fatalf("errors: %+v", Errors(vs))
			}
			ws := Warnings(vs)
			if len(ws) != 1 || ws[0].Code != CodeNonCodeCriterion || ws[0].CriterionID != "AC-2" {
				t.Fatalf("warnings = %+v, want one non_code_criterion for AC-2", ws)
			}
			if want := "criterion AC-2 is non-code: satisfied by the PR text; verify manually"; ws[0].Message != want {
				t.Errorf("gap line = %q, want %q", ws[0].Message, want)
			}
			if marks := NonCodeCriteria(brief, tasks); marks["AC-2"] != PRTextReason || len(marks) != 1 {
				t.Errorf("NonCodeCriteria = %v", marks)
			}
		})
	}
}

// The mixed case (R13 ruling): "implement X and say so in the PR
// description". Owned by a kept code step, it is an ordinary criterion the
// verifier judges — no non-code mark, no warning, its required checks
// enforced. Owned by no kept step, it is a non-code gap satisfied by the PR
// text.
func TestMixedPRTextCriterionOwnedOrNot(t *testing.T) {
	brief := BriefCriteria{SchemaVersion: SchemaVersion, Criteria: []Criterion{
		{ID: "AC-1", Text: "code", Source: Source{Kind: SourceUser}, Risk: RiskLow},
		{ID: "AC-2", Text: "Add GET /api/client-config and say so in the PR description", Source: Source{Kind: SourceUser, Quote: "q"}, Risk: RiskMedium, RequiredChecks: []evidence.CheckKind{"build"}},
	}}
	if !IsPRTextCriterion(brief.Criteria[1]) {
		t.Fatal("fixture: AC-2 must target the PR text")
	}

	t.Run("owned by a kept step: a verifier criterion", func(t *testing.T) {
		tasks := []TaskSpecWithFiles{codeTask(1, "AC-1", "AC-2")}
		if marks := NonCodeCriteria(brief, tasks); len(marks) != 0 {
			t.Fatalf("NonCodeCriteria = %v, want none", marks)
		}
		vs := ValidatePlan(brief, tasks, DependencyGraph{}, nil)
		for _, v := range vs {
			if v.Code == CodeNonCodeCriterion {
				t.Errorf("an owned criterion is a gap: %s", v.Message)
			}
		}
		// Judged as code: its required build check is enforced.
		var missing bool
		for _, v := range Errors(vs) {
			missing = missing || (v.Code == CodeVerifierMissing && v.CriterionID == "AC-2" && v.CheckKind == "build")
		}
		if !missing {
			t.Errorf("violations = %+v, want verifier_missing build for AC-2", vs)
		}
	})

	t.Run("owned by no kept step: a PR-text gap", func(t *testing.T) {
		tasks := []TaskSpecWithFiles{codeTask(1, "AC-1")}
		if marks := NonCodeCriteria(brief, tasks); marks["AC-2"] != PRTextReason {
			t.Fatalf("NonCodeCriteria = %v, want AC-2 %q", marks, PRTextReason)
		}
		vs := ValidatePlan(brief, tasks, DependencyGraph{}, nil)
		if HasErrors(vs) {
			t.Fatalf("errors: %+v", Errors(vs))
		}
		if ws := Warnings(vs); len(ws) != 1 || ws[0].Code != CodeNonCodeCriterion || ws[0].CriterionID != "AC-2" {
			t.Errorf("warnings = %+v", ws)
		}
	})
}
