package main

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/codex"
	"github.com/autonomousengine/autonomous-engine/internal/vector"
	"github.com/google/uuid"
)

// An incremental run sees only the changed modules, but a changed module's
// imports of unchanged modules are dependencies all the same: they are
// resolved against the product's stored modules of the repository, so a
// comment-only change keeps them (and writes no version), while a real
// import change is a module change (TEST_POSTGRES_DSN; git on PATH).
func TestIntegration_IncrementalIndexKeepsDependenciesOnUnchangedModules(t *testing.T) {
	p := codexTestPool(t)
	defer p.Close()
	ctx := context.Background()
	productID := mustInsertFixtureProductForCodex(t, p)
	var repoID uuid.UUID
	if err := p.QueryRow(ctx, `SELECT id FROM repositories WHERE product_id = $1`, productID).Scan(&repoID); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	gitIn(t, dir, "init", "-q", "-b", "main")
	writeFile(t, dir, "go.mod", "module example.com/p\n\ngo 1.22\n")
	writeFile(t, dir, "pkg/a/a.go", "package a\n\nimport \"example.com/p/pkg/b\"\n\nfunc A() int { return b.B() }\n")
	writeFile(t, dir, "pkg/b/b.go", "package b\n\nfunc B() int { return 1 }\n")
	gitIn(t, dir, "add", ".")
	gitIn(t, dir, "commit", "-q", "-m", "empty start")
	// Both modules change: the first run writes both rows.
	writeFile(t, dir, "pkg/a/a.go", "package a\n\nimport \"example.com/p/pkg/b\"\n\nfunc A() int { return b.B() }\n\nfunc A2() int { return 2 }\n")
	writeFile(t, dir, "pkg/b/b.go", "package b\n\nfunc B() int { return 1 }\n\nfunc B2() int { return 2 }\n")
	gitIn(t, dir, "commit", "-q", "-am", "both")

	prog := newProgress()
	s := &server{pool: p, cx: codex.New(p), vs: vector.New("http://127.0.0.1:1", "p0-test"), progress: prog}
	st := indexStep{path: dir, productID: productID, repoID: repoID}
	deps := func(name string) string {
		t.Helper()
		var d []string
		if err := p.QueryRow(ctx, `SELECT dependencies FROM codex_modules WHERE product_id = $1 AND branch = 'main' AND name = $2`, productID, name).Scan(&d); err != nil {
			t.Fatalf("module %s: %v", name, err)
		}
		sort.Strings(d)
		return strings.Join(d, ",")
	}
	latest := func() (int, string) {
		var pv int
		var sections []string
		_ = p.QueryRow(ctx, `SELECT product_version, sections_changed FROM codex_versions WHERE product_id = $1 ORDER BY product_version DESC LIMIT 1`, productID).Scan(&pv, &sections)
		return pv, strings.Join(sections, ",")
	}

	if err := s.runIncrementalIndex(ctx, prog, st, "HEAD~1"); err != nil {
		t.Fatal(err)
	}
	if got := deps("a"); got != "b" {
		t.Fatalf("after the first run a depends on %q, want b", got)
	}
	if pv, _ := latest(); pv != 1 {
		t.Fatalf("first run: v%d, want v1", pv)
	}

	// Comment-only change in pkg/a: pkg/b is not in this run.
	writeFile(t, dir, "pkg/a/a.go", "package a\n\nimport \"example.com/p/pkg/b\"\n\n// A calls B.\nfunc A() int { return b.B() }\n\nfunc A2() int { return 2 }\n")
	gitIn(t, dir, "commit", "-q", "-am", "comment")
	if err := s.runIncrementalIndex(ctx, prog, st, "HEAD~1"); err != nil {
		t.Fatal(err)
	}
	if got := deps("a"); got != "b" {
		t.Fatalf("a comment-only change dropped a's dependency on the unchanged b: %q", got)
	}
	if pv, sections := latest(); pv != 1 {
		t.Fatalf("a comment-only change wrote v%d %s, want no new version", pv, sections)
	}

	// A real import change: a now also imports c.
	writeFile(t, dir, "pkg/c/c.go", "package c\n\nfunc C() int { return 3 }\n")
	writeFile(t, dir, "pkg/a/a.go", "package a\n\nimport (\n\t\"example.com/p/pkg/b\"\n\t\"example.com/p/pkg/c\"\n)\n\n// A calls B.\nfunc A() int { return b.B() + c.C() }\n\nfunc A2() int { return 2 }\n")
	gitIn(t, dir, "add", ".")
	gitIn(t, dir, "commit", "-q", "-m", "import c")
	if err := s.runIncrementalIndex(ctx, prog, st, "HEAD~1"); err != nil {
		t.Fatal(err)
	}
	if got := deps("a"); got != "b,c" {
		t.Fatalf("after importing c a depends on %q, want b,c", got)
	}
	if pv, sections := latest(); pv != 2 || !strings.Contains(sections, "module:a") {
		t.Fatalf("import change: v%d %s, want v2 with module:a", pv, sections)
	}
}
