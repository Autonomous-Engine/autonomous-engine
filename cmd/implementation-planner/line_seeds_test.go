package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/plan"
)

// R10 W5: the brief's line_seeds are read and each subtask carries the
// seeds on its own files, under line_seeds.
func TestLineSeedsReachTheTaskMessage(t *testing.T) {
	artifact := `{"narrative":"n","line_seeds":[
	 {"path":"public/index.html","line_start":836,"line_end":853,"snippet":"840:     map.addTiles('https://tiles.example.com/night/base/{z}/{x}/{y}.png', {","verified":true},
	 {"path":"other-repo/app.ts","line_start":3,"line_end":3},
	 {"path":"./landing/index.html","line_start":855,"line_end":855,"snippet":"855:     map.addTiles(","verified":true}]}`
	var brief structuredBrief
	if err := json.Unmarshal([]byte(artifact), &brief); err != nil {
		t.Fatal(err)
	}
	spec := plan.TaskSpec{Files: []plan.FileRef{{Path: "landing/index.html"}}}
	got := seedsForTask(brief.LineSeeds, []string{"public/index.html", "src/server/index.ts"}, &spec)
	if len(got) != 2 || got[0].Path != "public/index.html" || got[1].LineStart != 855 {
		t.Fatalf("seeds = %+v", got)
	}
	b, _ := json.Marshal(implementationTaskMessage{LineSeeds: got})
	if !strings.Contains(string(b), `"line_seeds":[{"path":"public/index.html","line_start":836,"line_end":853,"snippet":"840:`) {
		t.Fatalf("task message JSON: %s", b)
	}
	if seedsForTask(nil, []string{"a"}, nil) != nil {
		t.Fatal("no seeds: nil")
	}
}
