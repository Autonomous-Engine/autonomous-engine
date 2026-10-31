package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/failures"
)

// R11 review: a reverted continuation reaches the evidence as a structured
// check — a test check not_run on the verified head, naming the pass, the
// stop, the fingerprints and the files — and caps the outcome at
// partially_verified. finalize gets them from generation (main.go).
func TestRevertedContinuationIsAnEvidenceCheck(t *testing.T) {
	fp := strings.Repeat("ab", 32)
	in := finalizeInputs{testCommand: "npm test", continuationFailures: []continuationFailure{{
		Pass: 1, Stop: stopRepeatedFailure, Repairs: 1, Reverted: []string{"tests/unit/server.test.ts"}, Fingerprints: []string{fp},
	}}}
	ev := &evidence.ImplementationEvidence{Checks: []evidence.CheckResult{{Kind: evidence.CheckKindTest, Command: "npm test", Status: evidence.CheckPassed}}}
	addRevertedContinuationChecks(in, ev)
	if len(ev.Checks) != 2 {
		t.Fatalf("checks = %+v", ev.Checks)
	}
	c := ev.Checks[1]
	if c.Kind != evidence.CheckKindTest || c.Status != evidence.CheckNotRun || c.Command != "npm test" ||
		!strings.Contains(c.SkipReason, "continuation 1 reverted, not committed: pre-commit checks repeated_failure after 1 repair attempt(s)") ||
		!strings.Contains(c.SkipReason, failures.Short(fp)) || !strings.Contains(c.SkipReason, "files not changed: tests/unit/server.test.ts") {
		t.Fatalf("check = %+v", c)
	}
	if got := evidence.DeriveOutcome(ev.Checks); got != evidence.OutcomePartiallyVerified {
		t.Fatalf("outcome = %s, want partially_verified", got)
	}
	// Nothing reverted: no check.
	ev2 := &evidence.ImplementationEvidence{}
	addRevertedContinuationChecks(finalizeInputs{}, ev2)
	if len(ev2.Checks) != 0 {
		t.Fatalf("checks without a failure = %+v", ev2.Checks)
	}
	src, _ := os.ReadFile("main.go")
	if !strings.Contains(strings.ReplaceAll(string(src), " ", ""), "continuationFailures:gen.continuationFailures") {
		t.Fatal("main.go does not hand the continuation failures to finalize")
	}
	fin, _ := os.ReadFile("finalize.go")
	if !strings.Contains(string(fin), "addRevertedContinuationChecks(in, ev)") {
		t.Fatal("finalizeImplementation does not record them")
	}
}

// R11 review: the revert touches only what the pass changed; a file that
// was already modified before the pass is left as it was.
func TestRevertLeavesPreexistingChangesAlone(t *testing.T) {
	ctx := context.Background()
	dir := initRepoAt(t, filepath.Join(t.TempDir(), "repo"))
	writeFile(t, dir, "a.txt", "a\n")
	writeFile(t, dir, "b.txt", "b\n")
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-m", "base")
	writeFile(t, dir, "a.txt", "a changed before the pass\n")
	writeFile(t, dir, "keep.txt", "untracked before the pass\n")
	before := uncommittedSet(ctx, dir)

	// The pass: edits b.txt, adds c.txt, deletes README.md; the gate stages.
	writeFile(t, dir, "b.txt", "b broken\n")
	writeFile(t, dir, "c.txt", "new\n")
	if err := os.Remove(filepath.Join(dir, "README.md")); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "-A")
	if got := strings.Join(passPaths(ctx, dir, before), ","); got != "README.md,b.txt,c.txt" {
		t.Fatalf("pass paths = %s", got)
	}
	if err := revertUncommitted(ctx, dir, before); err != nil {
		t.Fatal(err)
	}
	// Checked-out files may have CRLF endings (core.autocrlf).
	read := func(p string) string {
		b, _ := os.ReadFile(filepath.Join(dir, p))
		return strings.ReplaceAll(string(b), "\r\n", "\n")
	}
	if read("a.txt") != "a changed before the pass\n" || read("keep.txt") != "untracked before the pass\n" {
		t.Fatalf("pre-existing changes lost: a=%q keep=%q", read("a.txt"), read("keep.txt"))
	}
	if read("b.txt") != "b\n" || read("README.md") != "test\n" {
		t.Fatalf("pass not reverted: b=%q README=%q", read("b.txt"), read("README.md"))
	}
	if _, err := os.Stat(filepath.Join(dir, "c.txt")); !os.IsNotExist(err) {
		t.Fatal("c.txt survived")
	}
	if st := gitOutput(t, dir, "status", "--porcelain"); st != "M a.txt\n?? keep.txt" {
		t.Fatalf("status = %q", st)
	}
}
