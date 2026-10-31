package main

import (
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/failures"
)

// A missing package of a different module that merely shares the
// worktree module's path as a string prefix (github.com/x/app-utils vs
// github.com/x/app) is a dependency failure, not the change's: only the
// module path itself or a path under it ("github.com/x/app/...") is local.
func TestRepairOwnershipPrefixModuleStaysDependency(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "go.mod", "module github.com/x/app\n\ngo 1.22\n")
	repo := failures.RepoContextOf(root, nil)
	if repo.ModulePath != "github.com/x/app" {
		t.Fatalf("module path = %q", repo.ModulePath)
	}
	cases := []struct {
		pkg  string
		want failures.Ownership
	}{
		{"github.com/x/app-utils/foo", failures.OwnedDependency},
		{"github.com/x/app-utils", failures.OwnedDependency},
		{"github.com/x/appx", failures.OwnedDependency},
		{"github.com/x/app/internal/foo", failures.OwnedChangedFile},
		{"github.com/x/app", failures.OwnedChangedFile},
	}
	for _, c := range cases {
		t.Run(c.pkg, func(t *testing.T) {
			out := "# github.com/x/app/cmd/app\ncmd/app/main.go:3:8: no required module provides package " + c.pkg + "; to add it:\n\tgo get " + c.pkg + "\n"
			recs := failures.Parse("go", failures.Result{Tool: preCommitTool, Command: "go build ./...", Output: out, Root: root, Repo: repo})
			var got *failures.Record
			for i := range recs {
				if strings.Contains(recs[i].Message, c.pkg) {
					got = &recs[i]
				}
			}
			if got == nil {
				t.Fatalf("no record mentions %s: %+v", c.pkg, recs)
			}
			if got.Ownership != c.want || got.Retryable != (c.want == failures.OwnedChangedFile) {
				t.Fatalf("ownership %s retryable %v, want %s", got.Ownership, got.Retryable, c.want)
			}
		})
	}
}
