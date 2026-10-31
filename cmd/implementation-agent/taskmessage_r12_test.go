package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
	"github.com/autonomousengine/autonomous-engine/internal/llm/tools"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/plan"
)

// attempt12PlanTask is attempt 12's plan_tasks.message (subtask eba1a067,
// rewritten onto the demo-app fixture) with the seeds the coordinator now
// quotes for it (testdata/attempt12/line_seeds.json: the tile-layer and
// createApp lines of testdata/attempt9/demo-app). The seeds in the task
// message quote only public/index.html:837.
func attempt12PlanTask(t *testing.T) implementationTaskMessage {
	t.Helper()
	var task implementationTaskMessage
	b, err := os.ReadFile("testdata/attempt12/plan_task.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &task); err != nil {
		t.Fatal(err)
	}
	if len(task.LineSeeds) != 3 || !strings.HasPrefix(task.LineSeeds[0].Snippet, "837: ") {
		t.Fatalf("recorded seeds = %+v", task.LineSeeds)
	}
	s, err := os.ReadFile("testdata/attempt12/line_seeds.json")
	if err != nil {
		t.Fatal(err)
	}
	task.LineSeeds = nil
	if err := json.Unmarshal(s, &task.LineSeeds); err != nil {
		t.Fatal(err)
	}
	return task
}

// attempt12FirstRequest is the main loop's first request for attempt 12's
// task.
func attempt12FirstRequest(t *testing.T) adapter.CompletionRequest {
	return adapter.CompletionRequest{
		Messages: []adapter.ChatMessage{
			{Role: adapter.RoleSystem, Content: assembleImplementationSystemPrompt(models.OperatingModeStandard, "", "npm test", "", "")},
			{Role: adapter.RoleUser, Content: buildTaskMessage(attempt12PlanTask(t))},
		},
		Tools: withoutLSPUnlessReady(editTools(), false),
	}
}

// R12 X3: attempt 12's "## Where to edit" quotes the whole 836-853 range,
// tile URLs included, and the task stays within its ceilings.
func TestAttempt12WhereToEditQuotesTheWholeRange(t *testing.T) {
	req := attempt12FirstRequest(t)
	msg := req.Messages[1].Content
	where := msg[strings.Index(msg, "## Where to edit"):strings.Index(msg, "## Acceptance criteria")]
	t.Logf("attempt-12 task message: %d chars, Where to edit %d chars\n%s", len(msg), len(where), where)
	for _, want := range []string{
		"- public/index.html:836-853\n    836:     const map = L.map(",
		"    837:     L.control.zoom({ position: 'topleft' }).addTo(map);\n",
		"    840:     L.tileLayer('https://{s}.basemaps.example.com/dark_nolabels/{z}/{x}/{y}{r}.png', {\n",
		"    848:     const labelsLayer = L.tileLayer('https://{s}.basemaps.example.com/dark_only_labels/",
		"    852:       pane: 'overlayPane',\n",
		"- landing/index.html:855\n    855:     L.tileLayer('https://{s}.basemaps.example.com/dark_all/",
	} {
		if !strings.Contains(where, want) {
			t.Errorf("Where to edit lacks %q", want)
		}
	}
	if strings.Contains(where, "853:") {
		t.Error("the trailing closer 853 is quoted")
	}
	floor := tools.EstimatePromptTokens(req)
	t.Logf("implementation first-call floor (attempt-12 task): task %d chars = %d est. tokens", len(msg), floor)
	if len(msg) > taskMessageCeiling {
		t.Errorf("task message %d chars, ceiling %d", len(msg), taskMessageCeiling)
	}
	if floor > implementationFloorCeiling {
		t.Errorf("floor %d > %d", floor, implementationFloorCeiling)
	}
	// read_file centres on the first tile URL of the range.
	if seeds := seedLines(attempt12PlanTask(t).LineSeeds); seeds["public/index.html"] != 840 || seeds["landing/index.html"] != 855 {
		t.Fatalf("seed lines = %v", seeds)
	}
}

// A step body line its criteria already state is not repeated; one that
// adds something is kept.
func TestStepBodyThatRestatesItsCriteriaIsDropped(t *testing.T) {
	task := implementationTaskMessage{
		Title: "Add the key",
		Description: "## Step 1: Add the key (files: src/config/env.ts)\nFiles: [src/config/env.ts]\nCriteria: AC-1\n" +
			"Add `tilerApiKey: process.env.TILER_API_KEY || ''` to `src/config/env.ts`.\n" +
			"Keep the existing exports in the same order.\n",
		Criteria: []plan.Criterion{{ID: "AC-1", Text: "Add `tilerApiKey: process.env.TILER_API_KEY || ''` to src/config/env.ts with an empty default."}},
	}
	msg := buildTaskMessage(task)
	if strings.Count(msg, "process.env.TILER_API_KEY") != 1 {
		t.Errorf("the restated line is repeated:\n%s", msg)
	}
	if !strings.Contains(msg, "   Keep the existing exports in the same order.\n") {
		t.Errorf("the new line is dropped:\n%s", msg)
	}
	// Without its criterion the line stays.
	task.Criteria = nil
	if !strings.Contains(buildTaskMessage(task), "   Add `tilerApiKey") {
		t.Error("a line with no criterion to restate is dropped")
	}
}

// R12 review 5: a step line is dropped only when it restates a criterion:
// a line with a negation or limiter, or with a path, identifier or quoted
// term the criterion lacks, is kept; stop words are not counted.
func TestRestatesCriteriaKeepsWhatTheCriterionLacks(t *testing.T) {
	crit := []plan.Criterion{{ID: "AC-1", Text: "Add a GET /api/client-config route in createApp in src/server/index.ts that returns the Tiler tile host and `tilerApiKey`."}}
	for _, tc := range []struct {
		line string
		drop bool
	}{
		// The reviewer's probe: "only" limits what the criterion says.
		{"Allow the Tiler tile host in the CSP header in createApp only", false},
		{"Add a GET /api/client-config route in createApp in src/server/index.ts that returns the Tiler tile host.", true},
		{"Add a GET /api/client-config route in createApp, not in src/server/router.ts.", false},
		{"Add the route in createApp and return `basemapConfigured` too.", false},     // backticked term absent
		{"Add a GET /api/client-config route in createServer.", false},                // camelCase absent
		{"Return the Tiler tile host from the route in src/server/routes.ts.", false}, // path absent
		{"Add the GET route in createApp without a cache.", false},
		{"Don't add a GET /api/client-config route in createApp in 'src/server/index.ts'.", false},
	} {
		if got := restatesCriteria(tc.line, "AC-1", crit); got != tc.drop {
			t.Errorf("restatesCriteria(%q) = %v, want %v", tc.line, got, tc.drop)
		}
	}
	// A negation inside a quoted literal is the literal's text, not a
	// limiter (attempt 12's notice text "… is not configured …").
	notice := []plan.Criterion{{ID: "AC-4", Text: "Both pages display a notice 'Basemap unavailable: TILER_API_KEY is not configured.' when the key is missing."}}
	for _, tc := range []struct {
		line string
		drop bool
	}{
		{"Display a notice 'Basemap unavailable: TILER_API_KEY is not configured.' on both pages when the key is missing.", true},
		{"Display a notice 'Basemap unavailable: TILER_API_KEY is not configured.' only on the landing page.", false},
	} {
		if got := restatesCriteria(tc.line, "AC-4", notice); got != tc.drop {
			t.Errorf("restatesCriteria(%q) = %v, want %v", tc.line, got, tc.drop)
		}
	}
}

// R12 X4a: the "## Where to edit" header asks for the quoted lines to be
// changed in place (attempt 12 added a new keyed tile layer and left the
// three keyless URLs).
func TestWhereToEditAsksToChangeInPlace(t *testing.T) {
	msg := buildTaskMessage(attempt12PlanTask(t))
	want := "## Where to edit\nResearch located these lines (base commit). Change these existing lines in place (edit_file on the quoted line) unless the step says to add new code; do not add a parallel code path that leaves these lines unchanged.\n"
	if !strings.Contains(msg, want) {
		t.Fatalf("header:\n%s", msg[strings.Index(msg, "## Where to edit"):strings.Index(msg, "## Acceptance criteria")])
	}
}

// R12 X4b: a step whose criterion appends to / replaces / changes a quoted
// literal of one of its files gets one Target line naming those lines.
func TestStepTargetsTheQuotedLiterals(t *testing.T) {
	for _, tc := range []struct {
		name string
		task implementationTaskMessage
		step string // the step heading the line follows
	}{
		{"attempt 12", attempt12PlanTask(t), "3. Modify HTML Pages to Fetch API Key"},
		{"attempt 11", attempt11PlanTask(t), "3. Update Tile URLs in HTML Pages"},
	} {
		msg := buildTaskMessage(tc.task)
		target := "   Target: modify the quoted literal(s) at public/index.html:840,848 and landing/index.html:855 — not a new layer/block.\n"
		if n := strings.Count(msg, "Target:"); n != 1 {
			t.Errorf("%s: %d Target lines:\n%s", tc.name, n, msg)
			continue
		}
		at := strings.Index(msg, tc.step)
		next := at + strings.Index(msg[at:], "\n4. ") + 1 // through the line break
		if at < 0 || !strings.Contains(msg[at:next], target) {
			t.Errorf("%s: step 3 lacks %q:\n%s", tc.name, target, msg)
		}
		t.Logf("%s: task message %d chars", tc.name, len(msg))
		if len(msg) > taskMessageCeiling {
			t.Errorf("%s: task message %d chars > %d", tc.name, len(msg), taskMessageCeiling)
		}
	}

	seeds := []lineSeed{
		{Path: "src/a.ts", LineStart: 10, LineEnd: 12, Verified: true, Snippet: "10: const url = 'https://api.example.com/v1';\n11: const name = 'widget';"},
		{Path: "src/b.ts", LineStart: 3, LineEnd: 3, Verified: true, Snippet: "3: export function run() {"},
	}
	step := func(crit string) string {
		return buildTaskMessage(implementationTaskMessage{
			Title:       "t",
			Description: "## Step 1: Do it\nFiles: [src/a.ts src/b.ts]\nCriteria: AC-1\nDo it.\n",
			Criteria:    []plan.Criterion{{ID: "AC-1", Text: crit}},
			LineSeeds:   seeds,
		})
	}
	if msg := step("Append `?v=2` to the API URL in src/a.ts."); !strings.Contains(msg, "   Target: modify the quoted literal(s) at src/a.ts:10 — not a new layer/block.\n") {
		t.Errorf("URL literal:\n%s", msg)
	}
	if msg := step("Replace the widget name in src/a.ts and src/b.ts."); !strings.Contains(msg, "Target: modify the quoted literal(s) at src/a.ts:11 — not") {
		t.Errorf("a quoted literal sharing a word (b.ts has none):\n%s", msg)
	}
	// R15 T1: a criterion that names no step file falls back to the step
	// files' quoted lines (it was a negative under R12).
	if msg := step("Change the API URL."); !strings.Contains(msg, "   Target: modify the quoted literal(s) at src/a.ts:10 — not a new layer/block.\n") {
		t.Errorf("names no path:\n%s", msg)
	}
	if msg := step("Change the widget name."); !strings.Contains(msg, "Target: modify the quoted literal(s) at src/a.ts:11 — not") {
		t.Errorf("names no path, shared word:\n%s", msg)
	}
	if msg := step("Change the widget name in src/b.ts."); strings.Contains(msg, "Target:") {
		t.Errorf("a criterion naming a step file keeps the path-named rule:\n%s", msg)
	}
	for _, crit := range []string{
		"Log a warning in src/a.ts when the key is empty.", // no verb
		// R12 review 4: a negated verb is no Target.
		"The URL in src/a.ts must not change.",
		"Never replace the widget name in src/a.ts.",
		"Don't append anything to the API URL in src/a.ts.",
		"Do not change src/a.ts.",
		"Change the startup in src/b.ts.", // no literal quoted
	} {
		if msg := step(crit); strings.Contains(msg, "Target:") {
			t.Errorf("%q gave a Target line:\n%s", crit, msg)
		}
	}
}
