package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/codex"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/google/uuid"
)

// A full index records the architecture section and the modules whose rows
// changed, typed (migration 060).
func TestFullIndexVersion(t *testing.T) {
	pid := uuid.New()
	v := fullIndexVersion(pid, "develop", []string{"b", "a"})
	if v.ProductID != pid || v.Branch != "develop" || v.Trigger != codex.TriggerFullIndex || v.Source != models.CodexChangeSourceAutomatedReanalysis {
		t.Fatalf("version = %+v", v)
	}
	want := []string{"section:codebase_architecture", "module:a", "module:b"}
	if !reflect.DeepEqual(v.SectionsChanged, want) {
		t.Fatalf("sections = %v, want %v", v.SectionsChanged, want)
	}
}

// An incremental index writes a version only for a product's clone and only
// when a module row changed.
func TestIncrementalIndexVersion(t *testing.T) {
	pid, rid := uuid.New(), uuid.New()
	st := indexStep{path: "/clones/a", productID: pid, repoID: rid}
	v, ok := incrementalIndexVersion(st, "main", []string{"api"})
	if !ok || v.ProductID != pid || v.Branch != "main" || v.Trigger != codex.TriggerIncrementalIndex ||
		!reflect.DeepEqual(v.SectionsChanged, []string{"module:api"}) {
		t.Fatalf("version = %+v, %v", v, ok)
	}
	if _, ok := incrementalIndexVersion(st, "main", nil); ok {
		t.Error("a run that changed nothing writes a version")
	}
	if _, ok := incrementalIndexVersion(indexStep{path: "/repo-cache"}, "main", []string{"api"}); ok {
		t.Error("the legacy path (no product) writes a version")
	}
}

// Incremental runs see only the changed modules, so the dependents they
// compute are partial: they are added to the stored ones, never replace them.
func TestMergeDependents(t *testing.T) {
	existing := map[string][]string{"api": {"web", "cli"}}
	if got := mergeDependents(existing, "api", []string{"worker", "web"}); !reflect.DeepEqual(got, []string{"web", "cli", "worker"}) {
		t.Errorf("merged = %v", got)
	}
	if got := mergeDependents(nil, "api", []string{"worker"}); !reflect.DeepEqual(got, []string{"worker"}) {
		t.Errorf("full index = %v", got)
	}
	if got := mergeDependents(existing, "new", nil); got == nil || len(got) != 0 {
		t.Errorf("new module = %#v, want empty non-nil", got)
	}
}

// A module is a directory: an incremental run analyses every file of each
// directory a changed file is in, so the module row it rewrites is the whole
// module, not the changed files alone. A deleted file is simply absent.
func TestModuleFilesFor(t *testing.T) {
	all := []fileInfo{
		{Path: "pkg/a/a.go"}, {Path: "pkg/a/b.go"}, {Path: "pkg/a/sub/c.go"},
		{Path: "pkg/z/z.go"}, {Path: "main.go"},
	}
	got := moduleFilesFor(all, []string{"pkg/a/b.go", "pkg/a/deleted.go", "main.go"})
	var paths []string
	for _, f := range got {
		paths = append(paths, f.Path)
	}
	if want := []string{"pkg/a/a.go", "pkg/a/b.go", "main.go"}; !reflect.DeepEqual(paths, want) {
		t.Fatalf("module files = %v, want %v", paths, want)
	}
}

// A changed module's imports of modules not in the run resolve against the
// stored module names, and every dependency list is sorted (a stable array,
// so the change comparison never sees a reordering).
func TestBuildDependencyGraphWithStoredModules(t *testing.T) {
	mods := []moduleInfo{{Name: "a", Path: "pkg/a", Imports: []string{"example.com/p/pkg/z", "example.com/p/pkg/c", "example.com/p/pkg/b"}}}
	got := buildDependencyGraphWith(mods, []string{"b", "c", "z", "a"})
	if strings.Join(got["a"], ",") != "b,c,z" {
		t.Fatalf("a depends on %v, want [b c z]", got["a"])
	}
	if only := buildDependencyGraph(mods); len(only["a"]) != 0 {
		t.Fatalf("without stored modules a depends on %v", only["a"])
	}
}

// The indexer announces finished runs on codex.indexed (written with each
// version) and no longer on codex.updated, which is the index request.
func TestKnowledgeIndexer_DoesNotEnqueueCodexUpdated(t *testing.T) {
	for name, code := range sources(t) {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		if strings.Contains(code, "bus.TopicCodexUpdated, serviceName") || strings.Contains(code, "enqueueCodexUpdated(") {
			t.Errorf("%s still enqueues codex.updated as a completion notice", name)
		}
	}
}
