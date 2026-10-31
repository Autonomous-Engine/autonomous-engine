package failures

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A missing package that is the repository's own — under the main Go
// module's path, a local Python package, a TS path alias — is the change's
// failure (changed_file, retryable), not a dependency's. The same messages
// for third-party packages stay dependency. Outputs captured from go 1.22,
// pytest 9 and tsc 5.
func TestClassifyInRepoMissingPackages(t *testing.T) {
	cases := []struct {
		file, lang string
		repo       RepoContext
		want       Ownership
		msg        string
	}{
		{"go/go_build_missing_inrepo_package.txt", "go", RepoContext{ModulePath: "github.com/x/app"}, OwnedChangedFile, "github.com/x/app/internal/foo"},
		{"go/go_build_missing_inrepo_package.txt", "go", RepoContext{ModulePath: "github.com/other/app"}, OwnedDependency, "github.com/x/app/internal/foo"},
		{"go/go_build_missing_module.txt", "go", RepoContext{ModulePath: "example.com/app"}, OwnedDependency, "github.com/nonexistent/zzzpkg"},
		{"python/pytest_missing_local_module.txt", "python", RepoContext{LocalNames: []string{"mypkg", "tests"}}, OwnedChangedFile, "No module named 'mypkg.core'"},
		{"python/pytest_missing_local_module.txt", "python", RepoContext{LocalNames: []string{"tests"}}, OwnedDependency, "No module named 'mypkg.core'"},
		{"python/pytest_collection_error.txt", "python", RepoContext{LocalNames: []string{"tests"}}, OwnedDependency, "missing_lib_zz"},
		{"ts/tsc_missing_alias_module.txt", "typescript", RepoContext{}, OwnedChangedFile, "Cannot find module '@/lib/helper'"},
		{"ts/jest.txt", "typescript", RepoContext{}, OwnedChangedFile, "Cannot find module './nope'"},
	}
	for _, c := range cases {
		recs := Parse(c.lang, Result{Output: load(t, c.file), Repo: c.repo})
		var got *Record
		for i := range recs {
			if strings.Contains(recs[i].Message, c.msg) {
				got = &recs[i]
			}
		}
		if got == nil {
			t.Fatalf("%s: no record mentions %q: %+v", c.file, c.msg, recs)
		}
		if got.Ownership != c.want || got.Retryable != (c.want == OwnedChangedFile) {
			t.Errorf("%s with %+v: ownership %s retryable %v, want %s", c.file, c.repo, got.Ownership, got.Retryable, c.want)
		}
	}
	// A third-party TS package is still a dependency.
	if own := classifyOwnership(Record{Message: "TS2307: Cannot find module 'left-pad'"}, RepoContext{LocalNames: []string{"src"}}); own != OwnedDependency {
		t.Fatalf("left-pad: %s", own)
	}
	// A bare TS import of a repository directory is local.
	if own := classifyOwnership(Record{Message: "Cannot find module 'components/Button'"}, RepoContext{LocalNames: []string{"components"}}); own != OwnedChangedFile {
		t.Fatalf("components/Button: %s", own)
	}
}

func TestRepoContextOf(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("// c\nmodule github.com/x/app\n\ngo 1.22\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"internal", "src/mypkg", ".git", "node_modules"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	rc := RepoContextOf(root, []string{"newpkg/core.py", "src/widgets/a.ts", "tool.py"})
	if rc.ModulePath != "github.com/x/app" {
		t.Fatalf("module path %q", rc.ModulePath)
	}
	for _, n := range []string{"internal", "src", "mypkg", "newpkg", "widgets", "tool"} {
		if !rc.hasLocal(n) {
			t.Errorf("missing local name %q in %v", n, rc.LocalNames)
		}
	}
	for _, n := range []string{".git", "node_modules"} {
		if rc.hasLocal(n) {
			t.Errorf("%q must not be local", n)
		}
	}
}
