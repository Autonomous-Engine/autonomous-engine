package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/plan"
)

// R15 T2: the brief carries the intake's PR-text sentences (from the
// seed context's text: description + PRD) under pr_requirements.
func TestTechnicalBriefCarriesTheIntakePRRequirements(t *testing.T) {
	sc := seedContext{hintText: readFixture(t, "testdata/attempt12_intake.txt") + "\n\n" + readFixture(t, "testdata/attempt12_prd.md")}
	reqs := plan.PRTextRequirements(sc.hintText)
	if len(reqs) != 1 || !strings.Contains(reqs[0], "obtain a Mapforge API key") || !strings.Contains(reqs[0], "MAPFORGE_API_KEY") {
		t.Fatalf("pr_requirements = %q", reqs)
	}
	art, _ := json.Marshal(technicalBrief{Brief: "b", Narrative: "b", PRRequirements: reqs})
	if !strings.Contains(string(art), `"pr_requirements":["This change cannot fully resolve the error by itself: the pull request description MUST state explicitly`) {
		t.Errorf("pr_requirements missing from %s", art)
	}
	if legacy, _ := json.Marshal(technicalBrief{Brief: "b"}); strings.Contains(string(legacy), "pr_requirements") {
		t.Errorf("pr_requirements must be omitted when absent: %s", legacy)
	}
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "PRRequirements:       plan.PRTextRequirements(seedCtx.hintText),") {
		t.Error("the brief does not carry the intake's PR-text sentences")
	}
}
