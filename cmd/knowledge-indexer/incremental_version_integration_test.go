package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/codex"
	"github.com/autonomousengine/autonomous-engine/internal/vector"
	"github.com/google/uuid"
)

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.email=t@example.invalid", "-c", "user.name=t", "-c", "commit.gpgsign=false"}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func writeFile(t *testing.T, dir, rel, body string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Migration 060: an incremental index of a product's repository writes the
// product's module rows for the whole changed module and one codex version
// naming it, with codex.indexed; a run that changes no module row writes no
// version (TEST_POSTGRES_DSN; git on PATH).
func TestIntegration_IncrementalIndexWritesAVersionOnlyForChangedModules(t *testing.T) {
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
	writeFile(t, dir, "pkg/a/a.go", "package a\n\nfunc Alpha() int { return 1 }\n")
	writeFile(t, dir, "pkg/a/b.go", "package a\n\nfunc Beta() int { return 2 }\n")
	gitIn(t, dir, "add", ".")
	gitIn(t, dir, "commit", "-q", "-m", "one")
	writeFile(t, dir, "pkg/a/b.go", "package a\n\nfunc Beta() int { return 2 }\n\nfunc Gamma() int { return 3 }\n")
	gitIn(t, dir, "commit", "-q", "-am", "two")

	prog := newProgress()
	s := &server{pool: p, cx: codex.New(p), vs: vector.New("http://127.0.0.1:1", "p0-test"), progress: prog}
	st := indexStep{path: dir, productID: productID, repoID: repoID}

	versions := func() int {
		var n int
		_ = p.QueryRow(ctx, `SELECT count(*) FROM codex_versions WHERE product_id = $1`, productID).Scan(&n)
		return n
	}

	if err := s.runIncrementalIndex(ctx, prog, st, "HEAD~1"); err != nil {
		t.Fatalf("first run: %v", err)
	}
	var sections []string
	var pv int
	var branch string
	if err := p.QueryRow(ctx, `SELECT sections_changed, product_version, branch FROM codex_versions WHERE product_id = $1 ORDER BY product_version DESC LIMIT 1`, productID).Scan(&sections, &pv, &branch); err != nil {
		t.Fatalf("no codex version after an incremental index: %v", err)
	}
	if pv != 1 || branch != "main" || strings.Join(sections, ",") != "module:a" {
		t.Fatalf("version = v%d %s %v, want v1 main [module:a] (modules are named by package)", pv, branch, sections)
	}
	// The product's row describes the whole module (Alpha is in an
	// unchanged file of it).
	var iface []byte
	if err := p.QueryRow(ctx, `SELECT public_interface FROM codex_modules WHERE product_id = $1 AND branch = 'main' AND name = 'a'`, productID).Scan(&iface); err != nil {
		t.Fatalf("product module row: %v", err)
	}
	for _, fn := range []string{"Alpha", "Beta", "Gamma"} {
		if !strings.Contains(string(iface), fn) {
			t.Errorf("public interface %s lacks %s", iface, fn)
		}
	}
	var payload []byte
	if err := p.QueryRow(ctx, `SELECT payload FROM outbox_events WHERE topic = $1 AND aggregate_id = $2`, bus.TopicCodexIndexed, productID).Scan(&payload); err != nil {
		t.Fatalf("codex.indexed: %v", err)
	}
	ev, err := codex.DecodeIndexedEvent(payload)
	if err != nil || ev.Trigger != codex.TriggerIncrementalIndex || ev.ProductVersion != 1 || strings.Join(ev.Modules(), ",") != "a" {
		t.Fatalf("codex.indexed = %+v, %v", ev, err)
	}

	// A comment-only change: the module's structure is the same.
	writeFile(t, dir, "pkg/a/b.go", "package a\n\n// Beta is two.\nfunc Beta() int { return 2 }\n\nfunc Gamma() int { return 3 }\n")
	gitIn(t, dir, "commit", "-q", "-am", "three")
	if err := s.runIncrementalIndex(ctx, prog, st, "HEAD~1"); err != nil {
		t.Fatalf("comment-only run: %v", err)
	}
	if n := versions(); n != 1 {
		t.Fatalf("%d versions after a run that changed no module row, want 1", n)
	}
	// Nothing changed at all.
	if err := s.runIncrementalIndex(ctx, prog, st, "HEAD"); err != nil {
		t.Fatalf("empty run: %v", err)
	}
	if n := versions(); n != 1 {
		t.Fatalf("%d versions after an empty run, want 1", n)
	}

	// The legacy path (no product) writes no version.
	writeFile(t, dir, "pkg/b/c.go", "package b\n\nfunc Delta() int { return 4 }\n")
	gitIn(t, dir, "add", ".")
	gitIn(t, dir, "commit", "-q", "-m", "four")
	if err := s.runIncrementalIndex(ctx, prog, indexStep{path: dir}, "HEAD~1"); err != nil {
		t.Fatalf("legacy run: %v", err)
	}
	if n := versions(); n != 1 {
		t.Fatalf("%d versions after a productless run, want 1", n)
	}
	_, _ = p.Exec(ctx, `DELETE FROM codex_modules WHERE product_id IS NULL AND name = 'b'`)
}
