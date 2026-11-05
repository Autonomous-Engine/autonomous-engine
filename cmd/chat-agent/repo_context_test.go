package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Chat answered "this is jordan-wright/enmime" — an invented owner — while
// go.mod names github.com/jhillyerd/enmime/v2 (F32). The context block
// carries each repository's remote and module identity.
func TestModuleIdentityReadsGoModAndPackageJSON(t *testing.T) {
	goDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(goDir, "go.mod"), []byte("module github.com/jhillyerd/enmime/v2\n\ngo 1.25.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := moduleIdentity(goDir); got != "github.com/jhillyerd/enmime/v2" {
		t.Fatalf("go.mod identity = %q", got)
	}
	jsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(jsDir, "package.json"), []byte(`{"name":"@acme/widget","version":"1.0.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := moduleIdentity(jsDir); got != "@acme/widget" {
		t.Fatalf("package.json identity = %q", got)
	}
	if got := moduleIdentity(t.TempDir()); got != "" {
		t.Fatalf("empty dir identity = %q, want empty", got)
	}
}

func TestFormatRepoContextNamesRemoteAndModule(t *testing.T) {
	got := formatRepoContext([]repoContext{{Name: "enmime", URL: "/repo-cache/mirrors/enmime.git", Kind: "local_path", Module: "github.com/jhillyerd/enmime/v2"}})
	for _, want := range []string{"enmime", "/repo-cache/mirrors/enmime.git", "local_path", "github.com/jhillyerd/enmime/v2"} {
		if !strings.Contains(got, want) {
			t.Errorf("context %q lacks %q", got, want)
		}
	}
}
