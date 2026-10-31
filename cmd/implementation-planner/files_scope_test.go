package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/plan"
)

// TestFilesToChangeCoversEveryStepFile is live-run attempt 6: the steps cite
// env.ts, index.ts and both HTML pages, research's affected_files names only
// index.ts, and files_to_change came out as ["src/server/index.ts"] — so
// the implementer stopped after three files. files_to_change is now the
// union of the subtask's own list and every repository file its steps
// cite; a path that does not exist stays a flagged hypothesis outside it.
func TestFilesToChangeCoversEveryStepFile(t *testing.T) {
	clone := t.TempDir()
	for _, f := range []string{"src/server/index.ts", "src/config/env.ts", "public/map.html", "landing/index.html"} {
		p := filepath.Join(clone, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	steps := []decomposedTask{
		{Title: "Configure key", Files: []string{"src/config/env.ts"}},
		{Title: "Expose endpoint", Files: []string{"src/server/index.ts"}},
		{Title: "Map page notice", Files: []string{"public/map.html"}},
		{Title: "Landing claim", Files: []string{"landing/index.html", "docs/ghost.md"}},
	}
	grounded := groundTaskFilesAt(clone, clone, plan.BasisClone, groupFileClaims(steps), nil)
	spec := buildTaskSpec(steps, grounded)

	got := plannedFilesToChange(grounded, []string{"src/server/index.ts"}, "", &spec)
	want := []string{"src/server/index.ts", "src/config/env.ts", "public/map.html", "landing/index.html"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("files_to_change = %v, want %v", got, want)
	}
	// The missing path is still in the spec, flagged as before.
	var ghost *plan.FileRef
	for i := range spec.Files {
		if spec.Files[i].Path == "docs/ghost.md" {
			ghost = &spec.Files[i]
		}
	}
	if ghost == nil || ghost.Evidence.Kind != plan.EvidenceHypothesis {
		t.Fatalf("docs/ghost.md must stay a hypothesis in the spec: %+v", spec.Files)
	}
}

// With no grounded files and no research scope the description fallback
// still applies.
func TestFilesToChangeDescriptionFallback(t *testing.T) {
	got := plannedFilesToChange(nil, nil, "## Step 1: x\n\nFiles: [src/a.go]\n", &plan.TaskSpec{})
	if !reflect.DeepEqual(got, []string{"src/a.go"}) {
		t.Fatalf("files_to_change = %v, want [src/a.go]", got)
	}
}
