package failures

import (
	"strings"
	"testing"
)

// A check that could not run — the toolchain missing from the image, a
// module cache it cannot write, a script it cannot execute, a container
// exec that failed — is an environment failure: one record owned by the
// environment, not repairable, whatever the rest of the output says. Live,
// such output was classified "unknown" and repaired like a failing test.
func TestEnvironmentErrorsAreRecognisedInAnyOutput(t *testing.T) {
	code := func(n int) *int { return &n }
	cases := []struct {
		name   string
		result Result
	}{
		{"docker exec without the toolchain", Result{Command: "go test ./...", ExitCode: code(126),
			Output: `OCI runtime exec failed: exec failed: unable to start container process: exec: "go": executable file not found in $PATH: unknown` + "\n"}},
		{"no main module", Result{Command: "go test ./...", ExitCode: code(1),
			Output: "go: cannot find main module, but found .git/config in /src\n\tto create a module there, run:\n\tgo mod init\n"}},
		{"module cache not writable", Result{Command: "go build ./...", ExitCode: code(1),
			Output: "go: could not create module cache: mkdir /go/pkg/mod: permission denied\n"}},
		{"script not executable", Result{Command: "./scripts/test.sh", ExitCode: code(126),
			Output: "bash: ./scripts/test.sh: Permission denied\n"}},
		{"npm cannot spawn", Result{Command: "npm test", ExitCode: code(1),
			Output: "npm ERR! code ENOENT\nnpm ERR! syscall spawn sh\nnpm ERR! path /src\nnpm ERR! errno -2\nnpm ERR! enoent spawn sh ENOENT\n"}},
		{"exit 127 with no output", Result{Command: "make check", ExitCode: code(127), Output: ""}},
		{"failed to start", Result{Command: "pytest", ExitCode: code(-1), Output: "fork/exec /usr/bin/pytest: no such file or directory\n"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			recs := Parse("", c.result)
			if len(recs) != 1 {
				t.Fatalf("records = %+v, want one", recs)
			}
			r := recs[0]
			if r.Category != CategoryTooling || r.Ownership != OwnedEnvironment || r.Retryable {
				t.Fatalf("record = %+v, want tooling/environment, not retryable", r)
			}
			if r.Message == "" || r.Message == "(no output)" && c.result.Output != "" {
				t.Fatalf("message = %q", r.Message)
			}
		})
	}
}

// An environment line next to a parsed failure is still an environment
// record: the failing test may be a symptom of the environment, and the
// loop must see it.
func TestEnvironmentErrorNextToAParsedFailureIsRecorded(t *testing.T) {
	out := "--- FAIL: TestAlpha (0.00s)\n    alpha_test.go:5: wrong\nFAIL\n" +
		`OCI runtime exec failed: exec failed: unable to start container process: exec: "golangci-lint": executable file not found in $PATH: unknown` + "\n"
	recs := Parse("go", Result{Command: "make check", Output: out})
	var env, test bool
	for _, r := range recs {
		switch {
		case r.Ownership == OwnedEnvironment && r.Category == CategoryTooling && strings.Contains(r.Message, "golangci-lint"):
			env = true
		case r.Category == CategoryTestAssert && r.Test == "TestAlpha":
			test = true
		}
	}
	if !env || !test {
		t.Fatalf("records = %+v, want the test failure and the environment record", recs)
	}
}
