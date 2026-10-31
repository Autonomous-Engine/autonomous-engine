package main

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/plan"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// attempt13AC11 is attempt 13's PR-description criterion, verbatim.
const attempt13AC11 = "Explicitly state in the pull request description that the repository owner must obtain a TileHost API key and set `TILES_API_KEY` in the deployment environment for the basemap error to go away."

// planTaskMessage13 is a plan_tasks message as the planner writes it for
// attempt 13 after R13: AC-11 non-code with the PR-text reason, next to a
// deployment criterion that is non-code for another reason, and AC-14, a
// mixed criterion this subtask owns (an ordinary criterion the verifier
// judges, not a gap) whose text still targets the PR description.
var planTaskMessage13 = []byte(`{
	"task_id": "11111111-1111-1111-1111-111111111111",
	"task_spec": {"schema_version":1,"criterion_ids":["AC-1"],"deliverable":"d","verification":[{"check_kind":"test","command":"npm test"}]},
	"criteria": [
		{"id":"AC-1","text":"Add tilesApiKey to src/config/env.ts","source":{"kind":"user","quote":"q"},"risk":"medium"},
		{"id":"AC-12","text":"The release notes mention TILES_API_KEY","source":{"kind":"derived","reason":"r"},"risk":"low"},
		{"id":"AC-14","text":"Add GET /api/client-config and say so in the PR description","source":{"kind":"user","quote":"q"},"risk":"medium"}
	],
	"non_code_criteria": [
		{"id":"AC-11","text":` + jsonQuote(attempt13AC11) + `,"source":{"kind":"derived","reason":"Provides clear instructions for deployment."},"risk":"low","non_code":true,"non_code_reason":"satisfied by the PR text"},
		{"id":"AC-13","text":"Deployed to staging","source":{"kind":"derived","reason":"r"},"risk":"low","non_code":true,"non_code_reason":"cited only by task 4"},
		{"id":"AC-12","text":"The release notes mention TILES_API_KEY","source":{"kind":"derived","reason":"r"},"risk":"low","non_code":true,"non_code_reason":"satisfied by the PR text"}
	]
}`)

func jsonQuote(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}

func TestPRTextRequirementsFromThePlanTaskMessage(t *testing.T) {
	got := prTextRequirements(planTaskMessage13)
	var ids []string
	for _, c := range got {
		ids = append(ids, c.ID)
	}
	if strings.Join(ids, ",") != "AC-11,AC-12,AC-14" {
		t.Fatalf("PR-text requirements = %v, want AC-11,AC-12,AC-14 (the deployment criterion is not PR text; AC-12 once; the owned AC-14 too)", ids)
	}
	if got[0].Text != attempt13AC11 {
		t.Errorf("AC-11 text = %q, want it verbatim", got[0].Text)
	}
	if prTextRequirements([]byte(`not json`)) != nil {
		t.Error("an unreadable message yields requirements")
	}
}

// The generated PR description lists the PR-text criteria verbatim under
// "Requirements stated for this change"; with none, the section is absent.
func TestPRDescriptionCarriesThePRTextRequirements(t *testing.T) {
	reqs := prTextRequirements(planTaskMessage13)
	verified := subtaskOutcome{TaskID: "t1", Summary: "Implement in sample-app", Label: "verified", Action: "release", HeadSHA: "abc123"}
	body := renderPRBody(prBodyInput{Title: "[sample-app] TileHost key", Branch: "agent/x/t1", BaseBranch: "main", Outcomes: []subtaskOutcome{verified}, Requirements: reqs})
	i := strings.Index(body, prRequirementsHeading+"\n")
	if i < 0 {
		t.Fatalf("no %q section:\n%s", prRequirementsHeading, body)
	}
	section := body[i:]
	if j := strings.Index(section[len(prRequirementsHeading):], "\n## "); j >= 0 {
		section = section[:len(prRequirementsHeading)+j]
	}
	for _, want := range []string{"- **AC-11**: " + attempt13AC11 + "\n", "- **AC-12**: The release notes mention TILES_API_KEY\n", "- **AC-14**: Add GET /api/client-config and say so in the PR description\n"} {
		if !strings.Contains(section, want) {
			t.Errorf("section lacks %q:\n%s", want, section)
		}
	}
	if strings.Contains(section, "Deployed to staging") || strings.Contains(section, "tilesApiKey to src/config") {
		t.Errorf("section lists a criterion that is not PR text:\n%s", section)
	}
	if s := strings.Index(body, "## Summary"); s < 0 || s > i {
		t.Errorf("the section should follow the summary:\n%s", body)
	}

	if plain := renderPRBody(prBodyInput{Title: "t", Branch: "b", BaseBranch: "main", Outcomes: []subtaskOutcome{verified}}); strings.Contains(plain, prRequirementsHeading) {
		t.Errorf("a PR with no PR-text criterion has the section:\n%s", plain)
	}
}

// The section's intro covers owned and unowned criteria; every item is one
// line (whitespace and newlines collapsed), at most 500 characters, and at
// most 20 items are listed.
func TestPRRequirementsSectionIsBounded(t *testing.T) {
	section := renderPRRequirements([]plan.Criterion{{ID: "AC-1", Text: "State in the PR description\n\n  that   the key\tis needed."}})
	if !strings.Contains(section, prRequirementsIntro) ||
		!strings.Contains(prRequirementsIntro, "owned by a code step are also verified against the code") ||
		!strings.Contains(prRequirementsIntro, "the rest are recorded as gaps for review") {
		t.Errorf("intro = %q", prRequirementsIntro)
	}
	if !strings.Contains(section, "- **AC-1**: State in the PR description that the key is needed.\n") {
		t.Errorf("whitespace not collapsed:\n%s", section)
	}

	long := strings.Repeat("é", 600)
	section = renderPRRequirements([]plan.Criterion{{ID: "AC-2", Text: "Say in the PR description " + long}})
	item := section[strings.Index(section, "- **AC-2**: ")+len("- **AC-2**: "):]
	item = item[:strings.Index(item, "\n")]
	if n := len([]rune(item)); n != prRequirementsMaxChars+1 || !strings.HasSuffix(item, "…") {
		t.Errorf("item is %d runes (want %d + the ellipsis): %q…", n, prRequirementsMaxChars, string([]rune(item)[:40]))
	}

	var many []plan.Criterion
	for i := 1; i <= 23; i++ {
		many = append(many, plan.Criterion{ID: "AC-" + strconv.Itoa(i), Text: "State it in the PR description"})
	}
	section = renderPRRequirements(many)
	if got := strings.Count(section, "\n- **AC-"); got != prRequirementsMaxItems {
		t.Errorf("%d items listed, want %d", got, prRequirementsMaxItems)
	}
	if !strings.Contains(section, "- **AC-20**:") || strings.Contains(section, "- **AC-21**:") || !strings.Contains(section, "and 3 more") {
		t.Errorf("cap not applied:\n%s", section)
	}
}

type fakePlanTaskRow struct {
	raw []byte
	err error
}

func (r fakePlanTaskRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*(dest[0].(*[]byte)) = r.raw
	return nil
}

type fakePlanTaskQuerier struct {
	row  fakePlanTaskRow
	sql  string
	args []any
}

func (q *fakePlanTaskQuerier) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	q.sql, q.args = sql, args
	return q.row
}

func TestLoadPRTextRequirements(t *testing.T) {
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	q := &fakePlanTaskQuerier{row: fakePlanTaskRow{raw: planTaskMessage13}}
	got := loadPRTextRequirements(context.Background(), q, id)
	if len(got) != 3 || got[0].ID != "AC-11" {
		t.Fatalf("got %+v", got)
	}
	if !strings.Contains(q.sql, "FROM plan_tasks WHERE id = $1") || len(q.args) != 1 || q.args[0] != id {
		t.Errorf("query %q %v, want the subtask's plan_tasks row", q.sql, q.args)
	}
	for _, err := range []error{pgx.ErrNoRows, errors.New("connection reset")} {
		if got := loadPRTextRequirements(context.Background(), &fakePlanTaskQuerier{row: fakePlanTaskRow{err: err}}, id); got != nil {
			t.Errorf("%v: got %+v, want none", err, got)
		}
	}
}

// The handler loads the subtask's PR-text requirements and hands them to
// the PR description.
func TestHandlerWritesPRTextRequirementsIntoTheDescription(t *testing.T) {
	src := stripComments(coordinatorSources(t)["main.go"])
	for _, want := range []string{
		"prRequirements = loadPRTextRequirements(msgCtx, pool, c.ID)",
		"buildPRDescription(msgCtx, repoClonePath, baseBranch, branch, prTitle, outcomes, prRequirements)",
		"Requirements: requirements,",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("main.go lacks %q", want)
		}
	}
	if plan.PRTextReason != "satisfied by the PR text" {
		t.Errorf("PRTextReason = %q", plan.PRTextReason)
	}
}
