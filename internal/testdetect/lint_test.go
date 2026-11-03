package testdetect

import (
	"os"
	"path/filepath"
	"testing"
)

// A plan whose criteria require a lint check could not be repaired when no
// step ran a lint command (live: three godoc criteria, "no task … has a
// lint verification step", blocked after the repair round). The
// repository's own toolchain names one.
func TestDetectLintCommand(t *testing.T) {
	goRepo := t.TempDir()
	if err := os.WriteFile(filepath.Join(goRepo, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := DetectLintCommand(goRepo); got != "go vet ./..." {
		t.Fatalf("go repo lint = %q, want go vet ./...", got)
	}
	node := t.TempDir()
	if err := os.WriteFile(filepath.Join(node, "package.json"), []byte(`{"scripts":{"lint":"eslint ."}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := DetectLintCommand(node); got != "npm run lint" {
		t.Fatalf("node repo with a lint script = %q, want npm run lint", got)
	}
	noScript := t.TempDir()
	if err := os.WriteFile(filepath.Join(noScript, "package.json"), []byte(`{"scripts":{"test":"jest"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := DetectLintCommand(noScript); got != "" {
		t.Fatalf("node repo without a lint script = %q, want none", got)
	}
	if got := DetectLintCommand(t.TempDir()); got != "" {
		t.Fatalf("empty repo = %q, want none", got)
	}
}
