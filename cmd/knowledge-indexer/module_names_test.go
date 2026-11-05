package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Modules were keyed by Go package name alone, so cmd/mime-dump and
// cmd/mime-extractor (both package main) collided and the second silently
// overwrote the first (F22).
func TestDisambiguateModuleNamesUsesTheDirectoryForCollisions(t *testing.T) {
	in := []moduleInfo{
		{Name: "main", Path: "cmd/a"},
		{Name: "main", Path: "cmd/b"},
		{Name: "x", Path: "internal/x"},
		{Name: "x", Path: "pkg/x"},
		{Name: "coding", Path: "internal/coding"},
	}
	got := disambiguateModuleNames(in)
	want := []string{"cmd/a", "cmd/b", "internal/x", "pkg/x", "coding"}
	for i := range want {
		if got[i].Name != want[i] {
			t.Errorf("module %d: name = %q, want %q", i, got[i].Name, want[i])
		}
	}
}

func TestAnalyzeModulesKeepsBothPackageMainDirectories(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("cmd/dump/main.go", "package main\n\nfunc main() {}\n\nfunc Dump() {}\n")
	write("cmd/extract/main.go", "package main\n\nfunc main() {}\n\nfunc Extract() {}\n")
	write("internal/coding/charsets.go", "package coding\n\nfunc NewCharsetReader() {}\n\nfunc helper() {}\n\ntype peekBreakReader struct{}\n\ntype Cleaner struct{}\n")
	write("internal/coding/charsets_test.go", "package coding\n\nimport \"testing\"\n\nfunc TestCharsets(t *testing.T) {}\n")
	files := []fileInfo{
		{Path: "cmd/dump/main.go", Category: "source", Language: "Go"},
		{Path: "cmd/extract/main.go", Category: "source", Language: "Go"},
		{Path: "internal/coding/charsets.go", Category: "source", Language: "Go"},
		{Path: "internal/coding/charsets_test.go", Category: "test", Language: "Go"},
	}
	modules, err := analyzeModules(root, files, &IndexProgress{})
	if err != nil {
		t.Fatal(err)
	}
	modules = disambiguateModuleNames(modules)
	names := map[string]moduleInfo{}
	for _, m := range modules {
		names[m.Name] = m
	}
	if _, ok := names["cmd/dump"]; !ok {
		t.Errorf("cmd/dump missing: %v", keys(names))
	}
	if _, ok := names["cmd/extract"]; !ok {
		t.Errorf("cmd/extract missing: %v", keys(names))
	}
	coding, ok := names["coding"]
	if !ok {
		t.Fatalf("coding missing: %v", keys(names))
	}
	// The public interface lists exported, non-test symbols only (F23).
	fns := publicFunctionNames(coding.Functions)
	if strings.Join(fns, ",") != "NewCharsetReader" {
		t.Errorf("public functions = %v, want [NewCharsetReader]", fns)
	}
	if strings.Join(coding.Types, ",") != "Cleaner" {
		t.Errorf("types = %v, want [Cleaner]", coding.Types)
	}
}

func keys(m map[string]moduleInfo) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// An incremental run analyses only the changed modules, so collisions must
// be counted over the whole repository, not over the run (review I2):
// internal/x changed alone must still be stored as internal/x when pkg/x
// exists.
func TestDisambiguateUsesRepositoryWideCollisions(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("internal/x/a.go", "package x\n\nfunc A() {}\n")
	write("pkg/x/b.go", "package x\n\nfunc B() {}\n")
	write("internal/coding/c.go", "package coding\n\nfunc C() {}\n")
	all := []fileInfo{
		{Path: "internal/x/a.go", Category: "source", Language: "Go"},
		{Path: "pkg/x/b.go", Category: "source", Language: "Go"},
		{Path: "internal/coding/c.go", Category: "source", Language: "Go"},
	}
	counts := packageNameCounts(root, all)
	if counts["x"] != 2 || counts["coding"] != 1 {
		t.Fatalf("counts = %v", counts)
	}
	// Only internal/x was analysed this run.
	only := []moduleInfo{{Name: "x", Path: "internal/x"}}
	got := disambiguateModuleNamesWith(only, counts)
	if got[0].Name != "internal/x" {
		t.Fatalf("incremental name = %q, want internal/x", got[0].Name)
	}
}
