package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/plan"
	"github.com/google/uuid"
)

// R3 P1: a task that edits repository files is an implementation task,
// whatever its wording; only tasks that name no repository file and read as
// process, deployment or documentation work are dropped.
func TestFilterInvalidTasksKeepsTasksThatNameRepositoryFiles(t *testing.T) {
	tasks := []decomposedTask{
		{Title: "Implement the handler", Files: []string{"src/server/index.ts"}},
		{Title: "Update Documentation for TileHost API Key", Description: "Document the key in the README.",
			FileClaims: []fileClaim{{Path: "README.md", EvidenceRef: "repo:r2"}, {Path: ".env.example"}}},
		{Title: "Write the configuration guide", Description: "documentation for operators", Files: []string{"docs/configuration.md"}},
		{Title: "Update the changelog documentation", Files: []string{"CHANGELOG"}},
		{Title: "Deploy to production", Description: "deploy to prod after review"},
		{Title: "Update documentation", Description: "Refresh the user guide on the wiki."},
		{Title: "Notify team about the new key", Description: "notify team in chat", Files: []string{"https://wiki.example.com/tilehost"}},
		{Title: "Deploy to staging", Description: "deploy to staging", Files: []string{"/etc/nginx/nginx.conf"}},
		{Title: "Update the deployment compose file", Description: "deploy to staging needs the key", Files: []string{"docker-compose.yml"}},
	}
	numberSteps(tasks)
	kept := map[string]bool{}
	for _, task := range filterInvalidTasks(tasks) {
		kept[task.Title] = true
	}
	for title, want := range map[string]bool{
		"Implement the handler":                     true,
		"Update Documentation for TileHost API Key": true,
		"Write the configuration guide":             true,
		"Update the changelog documentation":        true,
		"Deploy to production":                      false,
		"Update documentation":                      false,
		"Notify team about the new key":             false,
		"Deploy to staging":                         false,
		"Update the deployment compose file":        true,
	} {
		if kept[title] != want {
			t.Errorf("%q kept=%t, want %t", title, kept[title], want)
		}
	}
}

func TestNamesRepositoryFile(t *testing.T) {
	for p, want := range map[string]bool{
		"README.md":                true,
		"./README.md":              true,
		"docs/setup/tilehost.md":   true,
		".env.example":             true,
		"CHANGELOG":                true,
		"LICENSE":                  true,
		"Makefile":                 true,
		"src/config/env.ts":        true,
		"src/config":               false, // a directory, not a file
		"":                         false,
		"/etc/hosts":               false,
		"C:\\Users\\x\\notes.txt":  false,
		"../outside/file.go":       false,
		"https://example.com/a.md": false,
		// R3 review: Windows separators are repository paths; host-like
		// first segments are not; leading-dot directories are.
		"docs\\setup.md":                 true,
		"docs/a.md":                      true,
		".github/workflows/ci.yml":       true,
		"example.com/a.md":               false,
		"www.example.org/docs/readme.md": false,
		"the README":                     false,
	} {
		if got := isRepositoryFilePath(p); got != want {
			t.Errorf("isRepositoryFilePath(%q) = %t, want %t", p, got, want)
		}
	}
}

// documentationFixture is internal/plan/testdata/documentation: a PRD (brief)
// with a code criterion, a documentation criterion (live-run attempt 5's
// AC-7) and a deployment criterion, and the plan the model returned.
func documentationFixture(t *testing.T) (*plan.BriefCriteria, []byte) {
	t.Helper()
	dir := filepath.Join("..", "..", "internal", "plan", "testdata", "documentation")
	briefRaw, err := os.ReadFile(filepath.Join(dir, "brief.json"))
	if err != nil {
		t.Fatal(err)
	}
	planRaw, err := os.ReadFile(filepath.Join(dir, "plan.json"))
	if err != nil {
		t.Fatal(err)
	}
	brief, err := briefCriteriaFromFile(briefRaw)
	if err != nil {
		t.Fatalf("brief: %v", err)
	}
	return brief, planRaw
}

// End to end through the planner's repair loop: the documentation task is
// kept and owns AC-2, the deployment task is dropped and its sole criterion
// AC-3 becomes non-code (a warning, not a block), and the plan is accepted
// on the first attempt. The committed subtask carries AC-3 as a non-code
// criterion so review sees the gap.
func TestDocumentationCriterionProducesAValidPlan(t *testing.T) {
	brief, raw := documentationFixture(t)
	calls := 0
	out := planWithRepair(uuid.Nil, func(string) ([]decomposedTask, error) {
		calls++
		return mustTasks(t, raw), nil
	}, capturedBuilder(brief))
	if calls != 1 || out.rejected() || out.attempts != 1 {
		t.Fatalf("calls=%d rejected=%t violations=%+v", calls, out.rejected(), out.violations)
	}
	b := out.build
	if len(b.planned) != 1 || b.planned[0].steps != 2 {
		t.Fatalf("planned = %+v, want one subtask with the code and documentation steps", b.planned)
	}
	for _, s := range b.specs {
		switch s.Index {
		case 1, 2:
			if s.Dropped != "" {
				t.Fatalf("step %d (%s) was dropped: %s", s.Index, s.Title, s.Dropped)
			}
		case 3:
			if s.Dropped != nonImplementationDropReason || !s.NonCode {
				t.Fatalf("the deployment step: dropped=%q non_code=%t", s.Dropped, s.NonCode)
			}
		}
	}
	ws := plan.Warnings(out.violations)
	if len(ws) != 1 || ws[0].Code != plan.CodeNonCodeCriterion || ws[0].CriterionID != "AC-3" {
		t.Fatalf("warnings = %+v, want one non_code_criterion for AC-3", ws)
	}

	attachNonCodeCriteria(b)
	nc := b.planned[0].msg.NonCodeCriteria
	if len(nc) != 1 || nc[0].ID != "AC-3" || !nc[0].NonCode || !strings.Contains(nc[0].NonCodeReason, `task 3 "Deploy to staging and notify team"`) {
		t.Fatalf("non-code criteria on the subtask = %+v", nc)
	}
	for _, c := range b.criteria.Criteria {
		if c.NonCode != (c.ID == "AC-3") {
			t.Errorf("criterion %s non_code=%t", c.ID, c.NonCode)
		}
	}
	if !strings.Contains(b.planned[0].msg.Description, "criterion AC-3 is non-code: ") {
		t.Errorf("the subtask description does not record the gap:\n%s", b.planned[0].msg.Description)
	}
}

// Ownership failing for another reason keeps the old behaviour: an
// uncited criterion is an orphan and the repair prompt names it.
func TestOrphanCriterionStillRepairs(t *testing.T) {
	brief, raw := documentationFixture(t)
	tasks := mustTasks(t, raw)
	tasks[1].CriterionIDs = []string{"AC-1"} // the documentation step no longer cites AC-2
	vs := capturedBuilder(brief)(tasks).validate()
	errs := plan.Errors(vs)
	if len(errs) != 1 || errs[0].Code != plan.CodeOrphanCriterion || errs[0].CriterionID != "AC-2" {
		t.Fatalf("errors = %+v, want the orphan_criterion for AC-2", errs)
	}
	if p := repairPrompt(vs, tasks, nil); !strings.Contains(p, "AC-2 (") || !strings.Contains(p, "is owned by no task") {
		t.Fatalf("repair prompt: %s", p)
	}
}

// R3 review: a plan whose every step is non-code work is neither promoted
// (the old "keep the first task" fallback made a file-less step own every
// criterion) nor silently completed: nothing is planned, every criterion
// is marked non-code, and the handler blocks with the reason.
func TestDeployOnlyPlanBlocksWithNothingToImplement(t *testing.T) {
	brief, _ := documentationFixture(t)
	deployOnly := []byte(`[
	  {"title": "Deploy to staging", "description": "deploy to staging", "criterion_ids": ["AC-3"],
	   "deliverable": "staging runs it", "verification": [{"check_kind": "other", "description": "staging loads"}]},
	  {"title": "Notify team about TILES_API_KEY", "description": "notify team in chat", "criterion_ids": ["AC-2"],
	   "deliverable": "the team knows", "verification": [{"check_kind": "other", "description": "message sent"}]}
	]`)
	if got := filterInvalidTasks(mustTasks(t, deployOnly)); len(got) != 0 {
		t.Fatalf("filterInvalidTasks kept %d non-code step(s): the fallback is back", len(got))
	}
	calls := 0
	out := planWithRepair(uuid.Nil, func(string) ([]decomposedTask, error) {
		calls++
		return mustTasks(t, deployOnly), nil
	}, capturedBuilder(brief))
	b := out.build
	if calls != 1 || b == nil || !b.allNonCode || len(b.planned) != 0 || b.noRepository {
		t.Fatalf("calls=%d build=%+v", calls, b)
	}
	for _, c := range b.criteria.Criteria {
		if !c.NonCode || c.NonCodeReason == "" {
			t.Errorf("criterion %s not marked non-code: %+v", c.ID, c)
		}
	}
	reason := b.allNonCodeReason()
	for _, want := range []string{
		"Planning found nothing to implement: all 2 decomposed step(s) are non-code work",
		`criterion AC-3 is non-code: cited only by task 1 "Deploy to staging"`,
		`criterion AC-2 is non-code: cited only by task 2 "Notify team about TILES_API_KEY"`,
		"criterion AC-1 is non-code: no step of the plan cites it",
		"verify manually",
	} {
		if !strings.Contains(reason, want) {
			t.Errorf("block reason lacks %q:\n%s", want, reason)
		}
	}
	src := stripComments(plannerSources(t)["main.go"])
	i := strings.Index(src, "if built.allNonCode {")
	if i < 0 || !strings.Contains(src[i:i+600], "built.allNonCodeReason()") || !strings.Contains(src[i:i+600], "escalateBlock(msgCtx, idempotency.Wrap(msgCtx, pool), workID, blockAllNonCode, reason, directBlock)") {
		t.Error("handleTechnicalBrief does not block an all-non-code plan with allNonCodeReason")
	}
	if j := strings.Index(src, "if len(planned) == 0 {"); j >= 0 && j < i {
		t.Error("the all-non-code block must come before the 'nothing to implement' completion")
	}
}

// R3 review: a brief that arrives with a criterion already marked non-code
// (research output, a stored artifact) does not exempt it: the planner's
// brief load strips the mark, and the uncited criterion is an orphan.
func TestBriefNonCodeMarkIsStrippedOnLoad(t *testing.T) {
	brief, raw := documentationFixture(t)
	brief.Criteria[1].NonCode = true
	brief.Criteria[1].NonCodeReason = "the research model says docs are not code"
	tasks := mustTasks(t, raw)
	tasks[1].CriterionIDs = []string{"AC-1"} // nothing cites AC-2
	vs := capturedBuilder(brief)(tasks).validate()
	errs := plan.Errors(vs)
	if len(errs) != 1 || errs[0].Code != plan.CodeOrphanCriterion || errs[0].CriterionID != "AC-2" {
		t.Fatalf("errors = %+v, want the orphan_criterion for AC-2", vs)
	}
	folded, _ := foldBrief(uuid.Nil, brief)
	if folded.Criteria[1].NonCode || folded.Criteria[1].NonCodeReason != "" {
		t.Fatalf("foldBrief kept the mark: %+v", folded.Criteria[1])
	}
	if !brief.Criteria[1].NonCode {
		t.Fatal("foldBrief modified the caller's brief")
	}
	if !strings.Contains(stripComments(plannerSources(t)["main.go"]), "plan.StripNonCode(brief.Criteria)") {
		t.Error("loadStructuredBrief does not strip non-code marks from the stored brief")
	}
}
