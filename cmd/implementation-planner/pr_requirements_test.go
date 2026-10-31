package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// R15 T2: the brief's pr_requirements are read and every subtask's task
// message carries them under pr_requirements.
func TestPRRequirementsReachTheTaskMessage(t *testing.T) {
	const req = "This change cannot fully resolve the error by itself: the pull request description MUST state explicitly that the repository owner must obtain a TileHost API key and set TILES_API_KEY in the deployment environment (.env / docker-compose) for the basemap error to go away."
	var brief structuredBrief
	if err := json.Unmarshal([]byte(`{"narrative":"n","pr_requirements":[`+string(mustJSON(t, req))+`]}`), &brief); err != nil {
		t.Fatal(err)
	}
	if len(brief.PRRequirements) != 1 || brief.PRRequirements[0] != req {
		t.Fatalf("brief pr_requirements = %q", brief.PRRequirements)
	}
	b, _ := json.Marshal(implementationTaskMessage{PRRequirements: brief.PRRequirements})
	if !strings.Contains(string(b), `"pr_requirements":["This change cannot fully resolve`) {
		t.Fatalf("task message JSON: %s", b)
	}
	if b, _ := json.Marshal(implementationTaskMessage{}); strings.Contains(string(b), "pr_requirements") {
		t.Errorf("pr_requirements must be omitted when absent: %s", b)
	}
	src, err := os.ReadFile("validation.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "PRRequirements:       structured.PRRequirements,") {
		t.Error("the subtask task message does not copy the brief's pr_requirements")
	}
}
