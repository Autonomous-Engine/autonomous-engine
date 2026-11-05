package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The implementation agent runs the repository's own tests in its
// container. Its image shipped Go 1.22 with GOTOOLCHAIN=local, so a
// repository whose go.mod says `go 1.23` failed every test run with
// "go.mod requires go >= 1.23" — an environment error the agent then
// tried to repair as if it were a failing test. The runtime stage carries
// a current Go and lets a newer requirement download its toolchain.
func TestImplementationImageRunsCurrentGoRepositories(t *testing.T) {
	assertRuntimeGo(t, "../../docker/Dockerfile.implementation-agent")
}

func assertRuntimeGo(t *testing.T, dockerfile string) {
	t.Helper()
	b, err := os.ReadFile(dockerfile)
	if err != nil {
		t.Fatal(err)
	}
	runtime := b[strings.LastIndex(string(b), "\nFROM "):]
	if !regexp.MustCompile(`golang:1\.(2[5-9]|[3-9]\d)-alpine`).Match(runtime) {
		t.Fatalf("%s: the runtime stage has no Go 1.25+ toolchain", dockerfile)
	}
	if !regexp.MustCompile(`(?m)^ENV .*GOTOOLCHAIN=auto`).Match(runtime) || !regexp.MustCompile(`(?m)^ENV .*PATH=.*/usr/local/go/bin`).Match(runtime) {
		t.Fatalf("%s: the runtime stage must put Go on PATH with GOTOOLCHAIN=auto so a newer go.mod requirement downloads its toolchain", dockerfile)
	}
}
