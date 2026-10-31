package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
	"github.com/autonomousengine/autonomous-engine/internal/llm/tools"
	"github.com/autonomousengine/autonomous-engine/internal/models"
)

// attempt15PlanTask is attempt 15's plan_tasks.message (subtask
// a997d3d4-7f18-4be7-8eeb-33b1d61335cf), rewritten onto the demo-app fixture.
func attempt15PlanTask(t *testing.T) implementationTaskMessage {
	t.Helper()
	var task implementationTaskMessage
	b, err := os.ReadFile("testdata/attempt15/plan_task.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &task); err != nil {
		t.Fatal(err)
	}
	if len(task.LineSeeds) != 3 {
		t.Fatalf("recorded seeds = %+v", task.LineSeeds)
	}
	return task
}

// R18 E3: attempt 15's step 3 ("Update HTML Pages to Fetch and Append API
// Key", public/index.html + landing/index.html, AC-1) got no Target line:
// AC-1 reads "… and appended to the tile URLs only if it is configured",
// and the modify verbs matched only "append", not "appended".
func TestAttempt15StepTargetOnAppended(t *testing.T) {
	task := attempt15PlanTask(t)
	req := adapter.CompletionRequest{
		Messages: []adapter.ChatMessage{
			{Role: adapter.RoleSystem, Content: assembleImplementationSystemPrompt(models.OperatingModeStandard, "", "npm test", "", "")},
			{Role: adapter.RoleUser, Content: buildTaskMessage(task)},
		},
		Tools: withoutLSPUnlessReady(editTools(), false),
	}
	msg := req.Messages[1].Content
	target := "   Target: modify the quoted literal(s) at public/index.html:840,848 and landing/index.html:855 — not a new layer/block.\n"
	if n := strings.Count(msg, "Target:"); n != 1 {
		t.Fatalf("%d Target lines:\n%s", n, msg)
	}
	at := strings.Index(msg, "3. Update HTML Pages to Fetch and Append API Key")
	next := at + strings.Index(msg[at:], "\n4. ") + 1
	if at < 0 || !strings.Contains(msg[at:next], target) {
		t.Fatalf("step 3 lacks %q:\n%s", target, msg)
	}
	floor := tools.EstimatePromptTokens(req)
	t.Logf("implementation first-call floor (attempt-15 task): task %d chars = %d est. tokens", len(msg), floor)
	if len(msg) > taskMessageCeiling {
		t.Errorf("task message %d chars, ceiling %d", len(msg), taskMessageCeiling)
	}
	if floor > implementationFloorCeiling {
		t.Errorf("floor %d > %d", floor, implementationFloorCeiling)
	}
}

// The modify verbs in their inflections, and the phrasings the brief
// names; negated ones and unrelated text are not.
func TestAsksToModifyForms(t *testing.T) {
	for _, s := range []string{
		"append ?api_key=<key> to the Tiler tile URLs",
		"The key is appended to the tile URLs only if configured.",
		"appends the key to each URL",
		"Add `?api_key=<key>` to the Tiler tile URLs.",
		"adds the key to the tile URL",
		"Prefix the tile URL with the proxy path.",
		"the URLs are suffixed with the key",
		"Rewrite the tile URLs to the keyed form.",
		"Update the tile URLs to carry the key.",
		"the tile URLs are updated",
		"replaced by the keyed URL",
		"Change the API URL.",
	} {
		if !asksToModify(s) {
			t.Errorf("asksToModify(%q) = false", s)
		}
	}
	for _, s := range []string{
		"must not change the URL",
		"never append anything to the tile URLs",
		"makes no changes to the tile URLs",
		"Both HTML pages display a visible notice when the API key is missing.",
		"The server logs a warning at startup.",
		"Add a GET /api/client-config route.",
		"`npm test` passes without any errors.",
		// update / rewrite only about a URL (attempt 13b's AC-2, AC-4).
		"Both public/index.html and landing/index.html display a visible, non-blocking notice when the key is missing; update the meta description in landing/index.html to remove the incorrect claim 'Open map, no API keys.'",
		"Update README.md, .env.example, docker-compose.yml, and docs/configuration.md to document the need for TILER_API_KEY.",
		"All relevant documentation files are updated to reflect the requirement.",
	} {
		if asksToModify(s) {
			t.Errorf("asksToModify(%q) = true", s)
		}
	}
}
