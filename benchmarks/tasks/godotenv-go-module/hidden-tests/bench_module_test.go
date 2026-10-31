package godotenv

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Held-out acceptance test: the checkout is a Go module named github.com/joho/godotenv
// and every package in it builds in module mode. The fixture's test command first runs this
// package in GOPATH mode (GO111MODULE=off), where it compiles without a go.mod, so the base
// revision fails on these assertions rather than on a build error; the child `go` commands
// below force module mode. The second step (`go test ./...`) then runs in module mode.

func benchGo(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("go", args...)
	cmd.Env = append(os.Environ(), "GOFLAGS=", "GO111MODULE=on")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestBenchRepositoryIsAGoModule(t *testing.T) {
	if got := benchGo(t, "list", "-m"); got != "github.com/joho/godotenv" {
		t.Fatalf("go list -m = %q, want %q", got, "github.com/joho/godotenv")
	}

	gomod := benchGo(t, "env", "GOMOD")
	if gomod == "" || gomod == os.DevNull || filepath.Base(gomod) != "go.mod" {
		t.Fatalf("go env GOMOD = %q, want a go.mod path", gomod)
	}
	if _, err := os.Stat(gomod); err != nil {
		t.Fatalf("go.mod reported at %q but not readable: %v", gomod, err)
	}

	if _, err := os.Stat(filepath.Join(filepath.Dir(gomod), "go.sum")); err == nil {
		t.Fatalf("go.sum exists but the module has no dependencies")
	}

	pkgs := benchGo(t, "list", "./...")
	for _, want := range []string{"github.com/joho/godotenv", "github.com/joho/godotenv/cmd/godotenv", "github.com/joho/godotenv/autoload"} {
		if !strings.Contains(pkgs, want) {
			t.Errorf("go list ./... does not include %s:\n%s", want, pkgs)
		}
	}
	benchGo(t, "build", "./...")
}
