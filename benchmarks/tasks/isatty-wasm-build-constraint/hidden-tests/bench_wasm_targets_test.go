package isatty_test

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// Held-out acceptance test: the package (and code using its API) compiles for every
// GOARCH=wasm target. `go vet` type-checks the package together with its tests, including
// the example that calls IsTerminal and IsCygwinTerminal.

func benchGoForTarget(t *testing.T, goos, goarch string, args ...string) {
	t.Helper()
	cmd := exec.Command("go", args...)
	cmd.Env = append(os.Environ(),
		"GOOS="+goos,
		"GOARCH="+goarch,
		"CGO_ENABLED=0",
		"GOFLAGS=",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("GOOS=%s GOARCH=%s go %s failed: %v\n%s", goos, goarch, strings.Join(args, " "), err, out)
	}
}

func TestBenchBuildsForWasmTargets(t *testing.T) {
	if testing.Short() {
		t.Skip("cross-compiles for wasm targets")
	}
	for _, target := range [][2]string{{"js", "wasm"}, {"wasip1", "wasm"}} {
		benchGoForTarget(t, target[0], target[1], "vet", ".")
	}
}

func TestBenchHostAndOtherTargetsStillBuild(t *testing.T) {
	if testing.Short() {
		t.Skip("cross-compiles for other targets")
	}
	for _, target := range [][2]string{{"linux", "amd64"}, {"windows", "amd64"}, {"darwin", "arm64"}} {
		benchGoForTarget(t, target[0], target[1], "build", ".")
	}
}
