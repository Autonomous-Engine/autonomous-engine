package verifier

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The verifier builds and tests the implementation's branch in the
// review-agent container (CommandGuard: "a verifier only reads, builds
// and tests"). That image shipped language servers but no Go at all, so
// `go test` in verification could only fail with "go: not found".
func TestVerifierImageCarriesAGoToolchain(t *testing.T) {
	b, err := os.ReadFile("../../docker/Dockerfile.agent-lsp")
	if err != nil {
		t.Fatal(err)
	}
	runtime := b[strings.LastIndex(string(b), "\nFROM "):]
	if !regexp.MustCompile(`golang:1\.(2[5-9]|[3-9]\d)-alpine`).Match(runtime) {
		t.Fatal("docker/Dockerfile.agent-lsp: the runtime stage has no Go 1.25+ toolchain")
	}
	if !regexp.MustCompile(`(?m)^ENV .*GOTOOLCHAIN=auto`).Match(runtime) || !regexp.MustCompile(`(?m)^ENV .*PATH=.*/usr/local/go/bin`).Match(runtime) {
		t.Fatal("docker/Dockerfile.agent-lsp: the runtime stage must put Go on PATH with GOTOOLCHAIN=auto")
	}
}
