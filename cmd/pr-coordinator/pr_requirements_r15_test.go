package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/plan"
)

func readR15Fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/attempt13b/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}

// attempt13bPlanTaskWithIntake is attempt 13b's plan_tasks message
// (subtask 99a8e484, read-only export) with the pr_requirements research
// now extracts from its intake (work item description + PRD).
func attempt13bPlanTaskWithIntake(t *testing.T) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(readR15Fixture(t, "plan_task.json")), &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["pr_requirements"]; ok {
		t.Fatal("the recorded message already has pr_requirements")
	}
	m["pr_requirements"] = plan.PRTextRequirements(readR15Fixture(t, "intake.txt"), readR15Fixture(t, "prd.md"))
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// R15 T2: attempt 13b's intake says "the pull request description MUST
// state explicitly that the repository owner must obtain a TileHost API key
// and set TILES_API_KEY …", but no criterion carried it, so the PR body had
// no "Requirements stated for this change". The intake's PR-text sentences
// now reach the plan task message and the PR body lists them verbatim.
func TestAttempt13bPRBodyStatesTheIntakePRRequirement(t *testing.T) {
	recorded := prTextRequirements([]byte(readR15Fixture(t, "plan_task.json")))
	if len(recorded) != 0 {
		t.Fatalf("attempt 13b's criteria had PR-text requirements: %+v", recorded)
	}
	reqs := prTextRequirements(attempt13bPlanTaskWithIntake(t))
	verified := subtaskOutcome{TaskID: "t1", Summary: "Implement in sample-app", Label: "verified", Action: "release", HeadSHA: "19ddb70"}
	body := renderPRBody(prBodyInput{Title: "[sample-app] Product Requirements Document", Branch: "agent/63f733e0/99a8e484", BaseBranch: "main", Outcomes: []subtaskOutcome{verified}, Requirements: reqs})
	i := strings.Index(body, prRequirementsHeading+"\n")
	if i < 0 {
		t.Fatalf("no %q section:\n%s", prRequirementsHeading, body)
	}
	section := body[i:]
	if j := strings.Index(section[len(prRequirementsHeading):], "\n## "); j >= 0 {
		section = section[:len(prRequirementsHeading)+j]
	}
	t.Logf("\n%s", section)
	want := "- **Intake**: This change cannot fully resolve the error by itself: the pull request description MUST state explicitly that the repository owner must obtain a TileHost API key and set TILES_API_KEY in the deployment environment (.env / docker-compose) for the basemap error to go away.\n"
	if !strings.Contains(section, want) {
		t.Errorf("section lacks %q", want)
	}
	for _, s := range []string{"obtain a TileHost API key", "TILES_API_KEY"} {
		if !strings.Contains(section, s) {
			t.Errorf("section lacks %q", s)
		}
	}
	// The PRD restates the intake's sentence ("The pull request description
	// explicitly states that …"): listed once.
	if n := strings.Count(section, "\n- "); n != 1 {
		t.Errorf("%d items, want the intake's one", n)
	}
}

// Intake requirements follow the criteria; one that restates a listed
// criterion is not repeated; the section appears with either source.
func TestPRTextRequirementsMergeCriteriaAndIntake(t *testing.T) {
	var m map[string]any
	if err := json.Unmarshal(planTaskMessage13, &m); err != nil {
		t.Fatal(err)
	}
	m["pr_requirements"] = []string{
		"The pull request description MUST state explicitly that the repository owner must obtain a TileHost API key and set TILES_API_KEY in the deployment environment for the basemap error to go away.",
		"The PR description must list the new environment variables.",
		"  ",
	}
	raw, _ := json.Marshal(m)
	got := prTextRequirements(raw)
	var ids []string
	for _, c := range got {
		ids = append(ids, c.ID)
	}
	if strings.Join(ids, ",") != "AC-11,AC-12,AC-14," {
		t.Fatalf("requirements = %q, want the three criteria then one intake sentence (the other restates AC-11)", ids)
	}
	if got[3].Text != "The PR description must list the new environment variables." {
		t.Errorf("intake text = %q", got[3].Text)
	}
	section := renderPRRequirements(got)
	if !strings.Contains(section, "- **Intake**: The PR description must list the new environment variables.\n") || !strings.Contains(section, prRequirementsIntakeNote) {
		t.Errorf("section:\n%s", section)
	}

	only, _ := json.Marshal(map[string]any{"pr_requirements": []string{"Say in the PR description that X is needed."}})
	if s := renderPRRequirements(prTextRequirements(only)); !strings.Contains(s, prRequirementsHeading) || !strings.Contains(s, "- **Intake**: Say in the PR description that X is needed.\n") {
		t.Errorf("intake-only section:\n%s", s)
	}
	if s := renderPRRequirements(prTextRequirements(planTaskMessage13)); strings.Contains(s, prRequirementsIntakeNote) {
		t.Errorf("criteria-only section has the intake note:\n%s", s)
	}
}
