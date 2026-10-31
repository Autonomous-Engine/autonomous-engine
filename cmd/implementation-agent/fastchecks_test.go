package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/checks"
	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
	"github.com/autonomousengine/autonomous-engine/internal/llm/tools"
	"github.com/autonomousengine/autonomous-engine/internal/logging"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/google/uuid"
)

const fastFixtureFullSuite = "go test ./... -count=1"

// fastFixture is a real-git Go module on a subtask branch with a bare
// origin. Every package's test appends the package name to a marker file
// in $FASTCHECK_MARKERS, so a test can see exactly which packages' tests
// ran. Packages: a (leaf), b (leaf), c, d (imports c).
type fastFixture struct {
	finalizeFixture
	markers     string
	testCommand string
	artifacts   *recordingArtifacts
}

// recordingArtifacts captures the check_plan artifacts.
type recordingArtifacts struct{ added []*models.WorkItemArtifact }

func (r *recordingArtifacts) AddArtifact(_ context.Context, a *models.WorkItemArtifact) error {
	r.added = append(r.added, a)
	return nil
}

func markerTest(pkg string) string {
	return "package " + pkg + "\n\nimport (\n\t\"os\"\n\t\"path/filepath\"\n\t\"testing\"\n)\n\n" +
		"func TestMark(t *testing.T) {\n" +
		"\tf, err := os.OpenFile(filepath.Join(os.Getenv(\"FASTCHECK_MARKERS\"), \"" + pkg + "\"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)\n" +
		"\tif err != nil {\n\t\tt.Fatal(err)\n\t}\n\tf.Close()\n}\n"
}

func newFastFixture(t *testing.T) *fastFixture {
	t.Helper()
	return newFastFixtureWith(t, nil, fastFixtureFullSuite)
}

// newFastFixtureWith adds extra files to the base commit and uses
// testCommand as the policy's full suite (fast and gate).
func newFastFixtureWith(t *testing.T, extra map[string]string, testCommand string) *fastFixture {
	t.Helper()
	requireSh(t)
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH; the fixture runs real go test")
	}
	repo := initTestRepository(t)
	files := map[string]string{
		"go.mod":      "module example.com/fx\n\ngo 1.22\n",
		"a/a.go":      "package a\n\nfunc A() int { return 1 }\n",
		"a/a_test.go": markerTest("a"),
		"b/b.go":      "package b\n\nfunc B() int { return 2 }\n",
		"b/b_test.go": markerTest("b"),
		"c/c.go":      "package c\n\nfunc C() int { return 3 }\n",
		"c/c_test.go": markerTest("c"),
		"d/d.go":      "package d\n\nimport \"example.com/fx/c\"\n\nfunc D() int { return c.C() + 1 }\n",
		"d/d_test.go": markerTest("d"),
	}
	for name, content := range extra {
		files[name] = content
	}
	for name, content := range files {
		p := filepath.Join(repo, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-m", "base: go module")
	remote := filepath.Join(t.TempDir(), "origin.git")
	gitRun(t, repo, "init", "--bare", remote)
	gitRun(t, repo, "remote", "add", "origin", remote)
	gitRun(t, repo, "push", "-u", "origin", "main")
	branch := "agent/work/task1"
	gitRun(t, repo, "checkout", "-b", branch)

	markers := t.TempDir()
	t.Setenv("FASTCHECK_MARKERS", markers)
	return &fastFixture{
		finalizeFixture: finalizeFixture{
			worktree: repo, remote: remote, branch: branch,
			baseSHA: gitOutput(t, repo, "rev-parse", "HEAD"),
			store:   &recordingEvidenceStore{},
		},
		markers:     markers,
		testCommand: testCommand,
		artifacts:   &recordingArtifacts{},
	}
}

// ran returns (and clears) the packages whose tests ran.
func (f *fastFixture) ran(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(f.markers)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
		if err := os.Remove(filepath.Join(f.markers, e.Name())); err != nil {
			t.Fatal(err)
		}
	}
	sort.Strings(out)
	return out
}

func (f *fastFixture) start(t *testing.T, executor *tools.Executor) *fastChecks {
	t.Helper()
	fast := startFastChecks(context.Background(), fastCheckDeps{
		worktree: f.worktree, baseSHA: f.baseSHA, taskID: uuid.New(), workID: uuid.New(),
		testCommand: f.testCommand, executor: executor, artifacts: f.artifacts, logger: logging.New("implementation-agent-test"),
	})
	if fast == nil {
		t.Fatal("startFastChecks returned nil with a test command configured")
	}
	return fast
}

func (f *fastFixture) inputs(fast *fastChecks) finalizeInputs {
	in := f.finalizeFixture.inputs()
	in.testCommand = f.testCommand
	in.fast = fast
	return in
}

// TestFastLoopRunsOwningPackageGateRunsFullSuite is the M3-04 proof on a
// real git fixture: after a write to package a, the model sees a note
// naming a's tests and the gate's full suite; the pre-commit loop
// (runRepairLoop, the generation phase's runner) runs a's tests only;
// the final gate still runs every package's tests; and the evidence shows
// the gate's full-suite check first, then the fast checks with their kinds
// and commands, outcome verified.
func TestFastLoopRunsOwningPackageGateRunsFullSuite(t *testing.T) {
	f := newFastFixture(t)
	executor := tools.NewExecutor(f.worktree)
	fast := f.start(t, executor)
	ctx := context.Background()

	// The model's first non-test write, through the real executor (which
	// insists on a read first).
	for i, p := range []string{"a/a.go", "a/a_test.go"} {
		ra, _ := json.Marshal(map[string]string{"path": p})
		if res := executor.Execute(ctx, adapter.ToolCall{ID: "read-" + string(rune('0'+i)), Name: "read_file", Arguments: string(ra)}); res.IsError || strings.Contains(res.Content, "[system note]") {
			t.Fatalf("read_file %s: %s", p, res.Content)
		}
	}
	args, _ := json.Marshal(map[string]string{"path": "a/a.go", "content": "package a\n\nfunc A() int { return 10 }\n"})
	res := executor.Execute(ctx, adapter.ToolCall{ID: "call-1", Name: "write_file", Arguments: string(args)})
	if res.IsError {
		t.Fatalf("write_file: %s", res.Content)
	}
	for _, s := range []string{"Fast checks for your change", "`go test -count=1 ./a` (owner of changed file a/a.go", "`" + fastFixtureFullSuite + "`) runs at the final gate"} {
		if !strings.Contains(res.Content, s) {
			t.Fatalf("write_file result lacks %q:\n%s", s, res.Content)
		}
	}
	if strings.Contains(res.Content, "./b") || strings.Contains(res.Content, "./d") {
		t.Fatalf("note selects packages the change does not impact:\n%s", res.Content)
	}
	// A test-file write and an unchanged plan add no note.
	args, _ = json.Marshal(map[string]string{"path": "a/a_test.go", "content": markerTest("a")})
	if res := executor.Execute(ctx, adapter.ToolCall{ID: "call-2", Name: "write_file", Arguments: string(args)}); strings.Contains(res.Content, "[system note]") {
		t.Fatalf("test-file write got a note: %s", res.Content)
	}

	// Pre-commit loop, as generation runs it (generation stages first).
	gitRun(t, f.worktree, "add", "-A")
	rr := runRepairLoop(ctx, repairInputs{logger: logging.New("implementation-agent-test"), runChecks: fast.run, results: fast.failureResults})
	passed, out := rr.passed, rr.output
	if !passed {
		t.Fatalf("fast loop failed:\n%s", out)
	}
	if got := f.ran(t); !reflect.DeepEqual(got, []string{"a"}) {
		t.Fatalf("fast loop ran the tests of %v, want only the owning package [a]\n%s", got, out)
	}
	var cmds []string
	for _, r := range fast.results {
		cmds = append(cmds, string(r.batch.Kind)+": "+r.batch.Command)
	}
	if want := []string{"test: go test -count=1 ./a", "build: go build ./...", "lint: go vet ./a"}; !reflect.DeepEqual(cmds, want) {
		t.Fatalf("fast checks = %v, want %v", cmds, want)
	}

	// Commit as generation does, then finalize: the gate runs the full suite.
	gitRun(t, f.worktree, "commit", "-m", "feat: change a")
	head := gitOutput(t, f.worktree, "rev-parse", "HEAD")
	sink := &recordingSink{}
	if !finalizeImplementation(ctx, f.inputs(fast), sink) {
		t.Fatalf("finalize failed: %+v", sink.published)
	}
	if got := f.ran(t); !reflect.DeepEqual(got, []string{"a", "b", "c", "d"}) {
		t.Fatalf("final gate ran the tests of %v, want the full suite [a b c d]", got)
	}
	ev := publishedEvidence(t, &f.finalizeFixture, sink)
	if ev.HeadSHA != head || ev.Outcome != evidence.OutcomeVerified {
		t.Fatalf("evidence head=%s outcome=%s, want %s verified", ev.HeadSHA, ev.Outcome, head)
	}
	if len(ev.Checks) != 4 {
		t.Fatalf("checks = %+v, want the gate's test check and three fast checks", ev.Checks)
	}
	if c := ev.Checks[0]; c.Kind != evidence.CheckKindTest || c.Command != fastFixtureFullSuite || c.Status != evidence.CheckPassed {
		t.Fatalf("checks[0] = %+v, want the gate's full-suite test check", c)
	}
	for i, want := range []struct {
		kind evidence.CheckKind
		cmd  string
	}{{evidence.CheckKindTest, "go test -count=1 ./a"}, {evidence.CheckKindBuild, "go build ./..."}, {evidence.CheckKindLint, "go vet ./a"}} {
		c := ev.Checks[i+1]
		if c.Kind != want.kind || c.Command != want.cmd || c.Status != evidence.CheckPassed || c.ExitCode == nil || *c.ExitCode != 0 || c.StartedAt == nil || c.FinishedAt == nil {
			t.Fatalf("checks[%d] = %+v, want passed %s %q with exit 0 and timestamps", i+1, c, want.kind, want.cmd)
		}
		if c.OutputRef == "" {
			t.Fatalf("checks[%d] has no output_ref", i+1)
		}
	}
	if err := ev.Validate(); err != nil {
		t.Fatalf("evidence invalid: %v", err)
	}
}

// TestFastChecksFromAnEarlierTreeAreNotEvidence: a continuation commit
// after the fast run changes the verified tree, so the fast results
// describe other code and are left out; the gate's own outcome stands.
func TestFastChecksFromAnEarlierTreeAreNotEvidence(t *testing.T) {
	f := newFastFixture(t)
	fast := f.start(t, nil)
	ctx := context.Background()
	writeFile(t, f.worktree, "a/a.go", "package a\n\nfunc A() int { return 10 }\n")
	if _, err := fast.run(ctx); err != nil {
		t.Fatal(err)
	}
	gitRun(t, f.worktree, "add", "-A")
	gitRun(t, f.worktree, "commit", "-m", "feat: change a")
	f.commit(t, "b/b.go", "package b\n\nfunc B() int { return 20 }\n", "feat: continuation")
	f.ran(t)

	sink := &recordingSink{}
	if !finalizeImplementation(ctx, f.inputs(fast), sink) {
		t.Fatalf("finalize failed: %+v", sink.published)
	}
	ev := publishedEvidence(t, &f.finalizeFixture, sink)
	if len(ev.Checks) != 1 || ev.Checks[0].Command != fastFixtureFullSuite || ev.Outcome != evidence.OutcomeVerified {
		t.Fatalf("checks = %+v outcome %s, want only the gate's verified full-suite check", ev.Checks, ev.Outcome)
	}
}

// TestFastChecksNeverLiftAnOutcomeTheGateDidNotEarn: when the gate did not
// run the full suite (no runner), passed fast checks are not appended, so
// they cannot turn an unverified run into a partially verified one.
func TestFastChecksNeverLiftAnOutcomeTheGateDidNotEarn(t *testing.T) {
	f := newFastFixture(t)
	fast := f.start(t, nil)
	ctx := context.Background()
	writeFile(t, f.worktree, "a/a.go", "package a\n\nfunc A() int { return 10 }\n")
	if _, err := fast.run(ctx); err != nil {
		t.Fatal(err)
	}
	gitRun(t, f.worktree, "add", "-A")
	gitRun(t, f.worktree, "commit", "-m", "feat: change a")
	head := gitOutput(t, f.worktree, "rev-parse", "HEAD")

	ev := &evidence.ImplementationEvidence{Checks: []evidence.CheckResult{{Kind: evidence.CheckKindTest, Status: evidence.CheckSkipped, SkipReason: "no test runner detected"}}}
	gate := gateResult{headSHA: head, testCheck: ev.Checks[0]}
	appendFastChecks(ctx, f.inputs(fast), &recordingSink{}, gate, ev)
	if len(ev.Checks) != 1 || evidence.DeriveOutcome(ev.Checks) != evidence.OutcomeUnverified {
		t.Fatalf("checks = %+v, want the skipped gate check alone (unverified)", ev.Checks)
	}
	// The same results are appended when the gate ran the suite.
	gate.testCheck = evidence.CheckResult{Kind: evidence.CheckKindTest, Command: fastFixtureFullSuite, Status: evidence.CheckPassed}
	ev.Checks = []evidence.CheckResult{gate.testCheck}
	appendFastChecks(ctx, f.inputs(fast), &recordingSink{}, gate, ev)
	if len(ev.Checks) != 4 || evidence.DeriveOutcome(ev.Checks) != evidence.OutcomeVerified {
		t.Fatalf("checks = %+v, want the gate check plus three passed fast checks (verified)", ev.Checks)
	}
}

// TestFastLoopFailureReportsTheFailingCheck: a failing impacted package fails
// the fast run with its output, so the repair loop reports the failure.
func TestFastLoopFailureReportsTheFailingCheck(t *testing.T) {
	f := newFastFixture(t)
	fast := f.start(t, nil)
	writeFile(t, f.worktree, "c/c.go", "package c\n\nfunc C() int { return 3 }\n\nvar _ = undefinedName\n")
	out, err := fast.run(context.Background())
	if err == nil {
		t.Fatalf("fast run passed with a compile error:\n%s", out)
	}
	// Owner and dependent run in one go test invocation.
	if !strings.Contains(string(out), "$ go test -count=1 ./c ./d   # test:") || !strings.Contains(string(out), "undefinedName") {
		t.Fatalf("output does not show the failing owner and dependent:\n%s", out)
	}
	if strings.Contains(string(out), "./a ") || strings.Contains(string(out), "./b ") {
		t.Fatalf("unimpacted packages ran:\n%s", out)
	}
}

// TestFastLoopPreExistingVetFindingIsAdvisory: the owner package carries a
// vet finding go test's vet subset does not report (unreachable code). The
// gate would pass, so the fast loop must too: go vet is advisory (reported
// in the output, not a run failure, not recorded as failed evidence).
func TestFastLoopPreExistingVetFindingIsAdvisory(t *testing.T) {
	f := newFastFixtureWith(t, map[string]string{
		"a/vet.go": "package a\n\n// U has unreachable code: a go vet finding outside go test's vet subset.\nfunc U() int {\n\treturn 1\n\tprintln(\"never\")\n\treturn 2\n}\n",
	}, fastFixtureFullSuite)
	fast := f.start(t, nil)
	ctx := context.Background()
	writeFile(t, f.worktree, "a/a.go", "package a\n\nfunc A() int { return 10 }\n")
	gitRun(t, f.worktree, "add", "-A")
	rr := runRepairLoop(ctx, repairInputs{logger: logging.New("implementation-agent-test"), runChecks: fast.run, results: fast.failureResults})
	passed, out := rr.passed, rr.output
	if !passed {
		t.Fatalf("a pre-existing vet finding failed the fast loop:\n%s", out)
	}
	if !strings.Contains(out, "unreachable code") || !strings.Contains(out, "advisory failed") {
		t.Fatalf("vet finding not reported as advisory:\n%s", out)
	}
	gitRun(t, f.worktree, "commit", "-m", "feat: change a")
	sink := &recordingSink{}
	if !finalizeImplementation(ctx, f.inputs(fast), sink) {
		t.Fatalf("finalize failed: %+v", sink.published)
	}
	ev := publishedEvidence(t, &f.finalizeFixture, sink)
	if ev.Outcome != evidence.OutcomeVerified {
		t.Fatalf("outcome = %s, want verified (checks %+v)", ev.Outcome, ev.Checks)
	}
	for _, c := range ev.Checks {
		if c.Kind == evidence.CheckKindLint {
			t.Fatalf("failed advisory vet recorded as evidence: %+v", c)
		}
	}
}

// TestFastLoopKeepsPolicyFlagsAndPatterns: the policy runs `go test -short`
// over ./a/... and ./d only. Package a has an integration test that fails
// without -short, and c is outside the policy's packages. A change to c
// selects only its in-policy dependent d, with -short; the gate runs a and d.
func TestFastLoopKeepsPolicyFlagsAndPatterns(t *testing.T) {
	policy := "go test -short -count=1 ./a/... ./d"
	f := newFastFixtureWith(t, map[string]string{
		"a/integration_test.go": "package a\n\nimport \"testing\"\n\nfunc TestIntegration(t *testing.T) {\n\tif !testing.Short() {\n\t\tt.Fatal(\"integration test needs a database\")\n\t}\n}\n",
		"d/integration_test.go": "package d\n\nimport \"testing\"\n\nfunc TestIntegration(t *testing.T) {\n\tif !testing.Short() {\n\t\tt.Fatal(\"integration test needs a database\")\n\t}\n}\n",
	}, policy)
	fast := f.start(t, nil)
	ctx := context.Background()
	writeFile(t, f.worktree, "c/c.go", "package c\n\nfunc C() int { return 30 }\n")
	gitRun(t, f.worktree, "add", "-A")
	rr := runRepairLoop(ctx, repairInputs{logger: logging.New("implementation-agent-test"), runChecks: fast.run, results: fast.failureResults})
	passed, out := rr.passed, rr.output
	if !passed {
		t.Fatalf("fast loop failed (policy flags dropped?):\n%s", out)
	}
	if got := f.ran(t); !reflect.DeepEqual(got, []string{"d"}) {
		t.Fatalf("fast loop ran %v, want only the in-policy dependent [d]\n%s", got, out)
	}
	if !strings.Contains(out, "$ go test -short -count=1 ./d ") {
		t.Fatalf("fast test command lost the policy flags:\n%s", out)
	}
	gitRun(t, f.worktree, "commit", "-m", "feat: change c")
	sink := &recordingSink{}
	if !finalizeImplementation(ctx, f.inputs(fast), sink) {
		t.Fatalf("finalize failed: %+v", sink.published)
	}
	if got := f.ran(t); !reflect.DeepEqual(got, []string{"a", "d"}) {
		t.Fatalf("gate ran %v, want the policy's packages [a d]", got)
	}
	// Only the plans a pre-commit run executed are stored.
	if n := len(f.artifacts.added); n != 1 {
		t.Fatalf("stored %d plan artifacts, want 1 (one pre-commit run)", n)
	}
}

// TestFastLoopAllSkippedMigrationIsNotEvidence: when every migration test
// skips (no integration database) the run executed no test, which is
// equivalent to a target that could not run: the run output and the stored
// plan say "skipped: no runnable migration tests", but nothing enters the
// evidence and the outcome is not capped. A migration run that executed a
// test is evidence.
func TestFastLoopAllSkippedMigrationIsNotEvidence(t *testing.T) {
	f := newFastFixtureWith(t, map[string]string{
		"db/db.go":           "package db\n\nfunc Migrate() error { return nil }\n",
		"db/migrate_test.go": "package db\n\nimport \"testing\"\n\nfunc TestMigrationsApply(t *testing.T) {\n\tt.Skip(\"TEST_POSTGRES_DSN not set\")\n}\n",
	}, fastFixtureFullSuite)
	fast := f.start(t, nil)
	ctx := context.Background()
	if err := os.MkdirAll(filepath.Join(f.worktree, "migrations"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, f.worktree, "migrations/001_init.up.sql", "CREATE TABLE IF NOT EXISTS t (id INT);\n")
	gitRun(t, f.worktree, "add", "-A")
	out, err := fast.run(ctx)
	if err != nil {
		t.Fatalf("fast run failed:\n%s", out)
	}
	if !strings.Contains(string(out), "$ go test -count=1 -v -run Migrat ./db") || !strings.Contains(string(out), "[skipped: no runnable migration tests") {
		t.Fatalf("run output does not show the skipped migration run:\n%s", out)
	}
	// The stored plan marks the target skipped.
	if len(f.artifacts.added) != 1 {
		t.Fatalf("stored %d plan artifacts, want 1", len(f.artifacts.added))
	}
	var stored struct {
		Plan checks.Plan `json:"plan"`
	}
	if err := json.Unmarshal(f.artifacts.added[0].Content, &stored); err != nil {
		t.Fatal(err)
	}
	marked := false
	for _, tg := range stored.Plan.Targets {
		if tg.Kind == evidence.CheckKindMigration && strings.HasPrefix(tg.Skipped, "no runnable migration tests") {
			marked = true
		}
	}
	if !marked {
		t.Fatalf("stored plan does not mark the migration target skipped: %+v", stored.Plan.Targets)
	}

	gitRun(t, f.worktree, "commit", "-m", "feat: migration")
	sink := &recordingSink{}
	if !finalizeImplementation(ctx, f.inputs(fast), sink) {
		t.Fatalf("finalize failed: %+v", sink.published)
	}
	ev := publishedEvidence(t, &f.finalizeFixture, sink)
	for _, c := range ev.Checks {
		if c.Kind == evidence.CheckKindMigration || c.Status == evidence.CheckSkipped {
			t.Fatalf("all-skipped migration run recorded as evidence: %+v", c)
		}
	}
	if ev.Outcome != evidence.OutcomeVerified {
		t.Fatalf("outcome = %s, want verified (an all-skipped migration run must not cap it); checks %+v", ev.Outcome, ev.Checks)
	}

	if migrationSkipped([]byte("=== RUN   TestM\n--- PASS: TestM (0.00s)\n")) != "" ||
		migrationSkipped([]byte("testing: warning: no tests to run\nPASS\n")) == "" ||
		migrationSkipped([]byte("--- SKIP: TestM (0.00s)\nPASS\n")) == "" {
		t.Fatal("migrationSkipped misclassifies")
	}
}

// TestFastLoopExecutedMigrationIsEvidence: a migration run that executed a
// test enters the evidence as passed.
func TestFastLoopExecutedMigrationIsEvidence(t *testing.T) {
	f := newFastFixtureWith(t, map[string]string{
		"db/db.go":           "package db\n\nfunc Migrate() error { return nil }\n",
		"db/migrate_test.go": "package db\n\nimport \"testing\"\n\nfunc TestMigrationsApply(t *testing.T) {\n\tif err := Migrate(); err != nil {\n\t\tt.Fatal(err)\n\t}\n}\n",
	}, fastFixtureFullSuite)
	fast := f.start(t, nil)
	ctx := context.Background()
	if err := os.MkdirAll(filepath.Join(f.worktree, "migrations"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, f.worktree, "migrations/001_init.up.sql", "CREATE TABLE IF NOT EXISTS t (id INT);\n")
	gitRun(t, f.worktree, "add", "-A")
	if out, err := fast.run(ctx); err != nil {
		t.Fatalf("fast run failed:\n%s", out)
	}
	gitRun(t, f.worktree, "commit", "-m", "feat: migration")
	sink := &recordingSink{}
	if !finalizeImplementation(ctx, f.inputs(fast), sink) {
		t.Fatalf("finalize failed: %+v", sink.published)
	}
	ev := publishedEvidence(t, &f.finalizeFixture, sink)
	found := false
	for _, c := range ev.Checks {
		if c.Kind == evidence.CheckKindMigration {
			found = c.Status == evidence.CheckPassed && c.Command == "go test -count=1 -v -run Migrat ./db"
		}
	}
	if !found || ev.Outcome != evidence.OutcomeVerified {
		t.Fatalf("checks = %+v outcome %s, want a passed migration check and verified", ev.Checks, ev.Outcome)
	}
}

// TestCommittedChangedFilesUsesTheResolvedBase: the continuation passes
// diff against the resolved base SHA, so they see the same base as the
// evidence even when the base branch name does not resolve in the worktree.
func TestCommittedChangedFilesUsesTheResolvedBase(t *testing.T) {
	f := newFinalizeFixture(t)
	f.commit(t, "feature.txt", "value=2\n", "feat")
	got := committedChangedFiles(context.Background(), f.worktree, f.baseSHA, "no-such-branch")
	if !reflect.DeepEqual(got, map[string]bool{"feature.txt": true}) {
		t.Fatalf("changed files = %v, want feature.txt from baseSHA...HEAD", got)
	}
	if got := committedChangedFiles(context.Background(), f.worktree, "", "main"); !got["feature.txt"] {
		t.Fatalf("branch fallback = %v", got)
	}
}

func TestStartFastChecksDisabledWithoutTestCommand(t *testing.T) {
	if fast := startFastChecks(context.Background(), fastCheckDeps{worktree: t.TempDir()}); fast != nil {
		t.Fatal("fast loop enabled without a test command")
	}
	var nilFast *fastChecks
	if note := nilFast.annotate("write_file", `{"path":"a.go"}`, adapter.ToolResult{}); note != "" {
		t.Fatalf("nil fast checks annotated: %q", note)
	}
}
