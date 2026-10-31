package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/interactions"
	"github.com/autonomousengine/autonomous-engine/internal/logging"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/snapshot"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// recordingSink captures everything the phases report so tests can assert
// on outcomes without Kafka or Postgres.
type recordingSink struct {
	published []implementationCompleteMessage
	blocked   []string
	statuses  []models.AssignmentStatus
	// artifacts holds every raw output stored through StoreOutput, keyed
	// by the reference handed back to the caller.
	artifacts map[string]storedOutput
	// storeErr, when set, makes StoreOutput fail so the file fallback runs.
	storeErr error
	prompts  []*interactions.Prompt
	attempts []completionOutcome
	// failNext makes the next n Reports fail without recording anything.
	failNext int
	// baseEvents holds the committed base_sha_events (M2-02 drift).
	baseEvents []snapshot.Event
}

type storedOutput struct {
	name    string
	content string
}

// Report behaves like one transaction: the evidence insert runs first
// (through InsertTx, as production does) and, when it or failNext fails,
// nothing of the outcome is recorded. published, blocked and statuses hold
// only committed outcomes; attempts holds every outcome offered.
func (s *recordingSink) Report(o completionOutcome) error {
	s.attempts = append(s.attempts, o)
	if s.failNext > 0 {
		s.failNext--
		return errors.New("injected commit failure")
	}
	if o.evidence != nil && o.evidenceStore != nil {
		if _, err := o.evidenceStore.InsertTx(context.Background(), nil, o.evidence); err != nil {
			return err
		}
	}
	s.baseEvents = append(s.baseEvents, o.baseEvents...)
	if o.prompt != nil {
		s.prompts = append(s.prompts, o.prompt)
	}
	if o.blockReason != "" {
		s.blocked = append(s.blocked, o.blockReason)
	}
	if o.msg != nil {
		s.published = append(s.published, *o.msg)
	}
	if o.assignment != "" {
		s.statuses = append(s.statuses, o.assignment)
	}
	return nil
}

func (s *recordingSink) StoreOutput(name string, content []byte) (string, error) {
	if s.storeErr != nil {
		return "", s.storeErr
	}
	if s.artifacts == nil {
		s.artifacts = map[string]storedOutput{}
	}
	ref := fmt.Sprintf("test-artifact:%s:%d", name, len(s.artifacts))
	s.artifacts[ref] = storedOutput{name: name, content: string(content)}
	return ref, nil
}

// recordingEvidenceStore stands in for *evidence.Store: it captures every
// insert (after the same Validate the real store runs) or fails on demand.
type recordingEvidenceStore struct {
	inserted []*evidence.ImplementationEvidence
	err      error
	txs      []pgx.Tx
}

func (s *recordingEvidenceStore) Insert(_ context.Context, ev *evidence.ImplementationEvidence) (uuid.UUID, error) {
	if s.err != nil {
		return uuid.Nil, s.err
	}
	if err := ev.Validate(); err != nil {
		return uuid.Nil, err
	}
	s.inserted = append(s.inserted, ev)
	return uuid.New(), nil
}

// InsertTx records like Insert; the recordingSink passes a nil tx, the
// production sink its transaction.
func (s *recordingEvidenceStore) InsertTx(ctx context.Context, tx pgx.Tx, ev *evidence.ImplementationEvidence) (uuid.UUID, error) {
	s.txs = append(s.txs, tx)
	return s.Insert(ctx, ev)
}

// finalizeFixture is a subtask branch checked out in a temp repo with a
// bare `origin`, mirroring the worktree the agent finalizes from. The
// base commit ships run_tests.sh, a stand-in test suite that passes only
// when feature.txt contains "value=2".
type finalizeFixture struct {
	worktree string
	remote   string
	branch   string
	baseSHA  string
	store    *recordingEvidenceStore
}

const fixtureTestCommand = "sh run_tests.sh"

func requireSh(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not on PATH; the agent runs test commands through sh")
	}
}

func newFinalizeFixture(t *testing.T) *finalizeFixture {
	t.Helper()
	repo := initTestRepository(t)
	writeFile(t, repo, "run_tests.sh", "#!/bin/sh\ngrep -q 'value=2' feature.txt\n")
	writeFile(t, repo, "feature.txt", "value=1\n")
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-m", "base: add test suite")

	remote := filepath.Join(t.TempDir(), "origin.git")
	gitRun(t, repo, "init", "--bare", remote)
	gitRun(t, repo, "remote", "add", "origin", remote)
	gitRun(t, repo, "push", "-u", "origin", "main")

	branch := "agent/work/task1"
	gitRun(t, repo, "checkout", "-b", branch)
	return &finalizeFixture{
		worktree: repo,
		remote:   remote,
		branch:   branch,
		baseSHA:  gitOutput(t, repo, "rev-parse", "HEAD"),
		store:    &recordingEvidenceStore{},
	}
}

// commit writes one file and commits it on the subtask branch, returning
// the new HEAD SHA.
func (f *finalizeFixture) commit(t *testing.T, name, content, message string) string {
	t.Helper()
	writeFile(t, f.worktree, name, content)
	gitRun(t, f.worktree, "add", "-A")
	gitRun(t, f.worktree, "commit", "-m", message)
	return gitOutput(t, f.worktree, "rev-parse", "HEAD")
}

func (f *finalizeFixture) inputs() finalizeInputs {
	return finalizeInputs{
		task:          implementationTaskMessage{TaskID: uuid.NewString(), WorkID: uuid.NewString(), FilesToChange: []string{"planned/by/planner.go"}},
		taskID:        uuid.New(),
		clonePath:     f.worktree,
		worktreePath:  f.worktree,
		subtaskBranch: f.branch,
		baseBranch:    "main",
		baseSHA:       f.baseSHA,
		testCommand:   fixtureTestCommand,
		summary:       "implemented",
		evidenceStore: f.store,
		logger:        logging.New("implementation-agent-test"),
	}
}

// remoteSHA returns the SHA origin holds for the branch, or "" if the
// branch was never pushed.
func (f *finalizeFixture) remoteSHA(t *testing.T) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "--verify", "--quiet", "refs/heads/"+f.branch)
	cmd.Dir = f.remote
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return string(out[:len(out)-1])
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestFinalizeRejectsContinuationThatBreaksTests is the M0-01 acceptance
// test: the initial commit passes the suite, a continuation commit breaks
// it, and finalization must neither publish success nor push that HEAD.
func TestFinalizeRejectsContinuationThatBreaksTests(t *testing.T) {
	requireSh(t)
	f := newFinalizeFixture(t)
	f.commit(t, "feature.txt", "value=2\n", "feat: initial (tests pass)")
	broken := f.commit(t, "feature.txt", "value=3\n", "feat: continuation (tests fail)")

	sink := &recordingSink{}
	finalizeImplementation(context.Background(), f.inputs(), sink)

	if len(sink.published) != 1 {
		t.Fatalf("published %d completion messages, want exactly 1: %+v", len(sink.published), sink.published)
	}
	if msg := sink.published[0]; msg.Success {
		t.Fatalf("published success although tests fail on HEAD %s: %+v", broken, msg)
	}
	if pushed := f.remoteSHA(t); pushed != "" {
		t.Fatalf("branch %s was pushed at %s although tests fail on that HEAD", f.branch, pushed)
	}
	for _, st := range sink.statuses {
		if st == models.AssignmentStatusCompleted {
			t.Fatalf("assignment marked completed although tests fail on HEAD %s", broken)
		}
	}

	// The failure evidence must name the commit that failed and how.
	msg := sink.published[0]
	if msg.HeadSHA != broken {
		t.Fatalf("head_sha = %q, want the verified HEAD %q", msg.HeadSHA, broken)
	}
	if msg.Verified {
		t.Fatal("verified = true on a failed gate")
	}
	if want := "failed: " + fixtureTestCommand; msg.TestResults != want {
		t.Fatalf("test_results = %q, want %q", msg.TestResults, want)
	}
	if !strings.Contains(msg.Reason, f.branch) || !strings.Contains(msg.Reason, broken) {
		t.Fatalf("reason %q must include branch %s and commit %s for recovery", msg.Reason, f.branch, broken)
	}
	if len(sink.blocked) != 1 || !strings.Contains(sink.blocked[0], broken) {
		t.Fatalf("work item block reasons = %q, want one naming commit %s", sink.blocked, broken)
	}
	if len(sink.statuses) != 1 || sink.statuses[0] != models.AssignmentStatusFailed {
		t.Fatalf("assignment statuses = %v, want [failed]", sink.statuses)
	}
}

// TestFinalizePublishesEvidenceBoundToPushedHead covers the success path:
// the SHA recorded as verified is the SHA origin holds, and every evidence
// field comes from git rather than the planner.
func TestFinalizePublishesEvidenceBoundToPushedHead(t *testing.T) {
	requireSh(t)
	f := newFinalizeFixture(t)
	f.commit(t, "feature.txt", "value=2\n", "feat: initial")
	head := f.commit(t, "notes.txt", "continuation\n", "feat: continuation")

	sink := &recordingSink{}
	finalizeImplementation(context.Background(), f.inputs(), sink)

	if len(sink.published) != 1 {
		t.Fatalf("published %d completion messages, want 1: %+v", len(sink.published), sink.published)
	}
	msg := sink.published[0]
	if !msg.Success || !msg.Verified {
		t.Fatalf("success=%v verified=%v, want both true: %+v", msg.Success, msg.Verified, msg)
	}
	pushed := f.remoteSHA(t)
	if pushed == "" {
		t.Fatalf("branch %s was not pushed to origin", f.branch)
	}
	if msg.HeadSHA != head || pushed != head {
		t.Fatalf("head_sha=%s pushed=%s local HEAD=%s, want all equal", msg.HeadSHA, pushed, head)
	}
	if msg.BaseSHA != f.baseSHA {
		t.Fatalf("base_sha = %q, want %q", msg.BaseSHA, f.baseSHA)
	}
	if msg.Branch != f.branch {
		t.Fatalf("branch = %q, want %q", msg.Branch, f.branch)
	}
	if want := "passed: " + fixtureTestCommand; msg.TestResults != want {
		t.Fatalf("test_results = %q, want %q", msg.TestResults, want)
	}
	if want := []string{"feature.txt", "notes.txt"}; !reflect.DeepEqual(msg.FilesChanged, want) {
		t.Fatalf("files_changed = %v, want %v (from git, not the planner's %v)", msg.FilesChanged, want, f.inputs().task.FilesToChange)
	}
	if len(sink.blocked) != 0 {
		t.Fatalf("work item was blocked on success: %q", sink.blocked)
	}
	if len(sink.statuses) != 1 || sink.statuses[0] != models.AssignmentStatusCompleted {
		t.Fatalf("assignment statuses = %v, want [completed]", sink.statuses)
	}
}

// TestFinalizeReportsSkippedTestsAsUnverified: with no test runner and a
// partial_with_human_review repository policy the task can still complete,
// but the evidence must say the check was skipped and must not claim
// verification. (Under the default policy the run blocks; see
// TestFinalizePolicyBlocksWhenNoChecks.)
func TestFinalizeReportsSkippedTestsAsUnverified(t *testing.T) {
	f := newFinalizeFixture(t)
	head := f.commit(t, "feature.txt", "value=3\n", "feat: no runner, tests would fail if run")

	in := f.inputs()
	in.testCommand = ""
	in.policies = partialPolicySource(t, &in)
	sink := &recordingSink{}
	finalizeImplementation(context.Background(), in, sink)

	if len(sink.published) != 1 {
		t.Fatalf("published %d completion messages, want 1", len(sink.published))
	}
	msg := sink.published[0]
	if !msg.Success {
		t.Fatalf("success = false with no test runner: %+v", msg)
	}
	if msg.Verified {
		t.Fatal("verified = true although no tests ran")
	}
	if msg.TestResults != "skipped: no test runner detected" {
		t.Fatalf("test_results = %q, want skipped", msg.TestResults)
	}
	if msg.HeadSHA != head || f.remoteSHA(t) != head {
		t.Fatalf("head_sha=%s pushed=%s, want %s", msg.HeadSHA, f.remoteSHA(t), head)
	}
}

// installPreReceiveHook makes the bare origin run script before accepting
// a push. Git for Windows and POSIX both execute it through sh.
func (f *finalizeFixture) installPreReceiveHook(t *testing.T, script string) {
	t.Helper()
	hooks := filepath.Join(f.remote, "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hooks, "pre-receive"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// assertPushFailure checks the M0-04 contract: no success, no completed
// assignment, nothing on origin, and a reason that names the category,
// branch, and commit so an operator can recover the work.
func assertPushFailure(t *testing.T, f *finalizeFixture, sink *recordingSink, head, category string) {
	t.Helper()
	if len(sink.published) != 1 {
		t.Fatalf("published %d completion messages, want 1: %+v", len(sink.published), sink.published)
	}
	msg := sink.published[0]
	if msg.Success || msg.Verified {
		t.Fatalf("success=%v verified=%v after a failed push: %+v", msg.Success, msg.Verified, msg)
	}
	if msg.HeadSHA != head || msg.Branch != f.branch {
		t.Fatalf("head_sha=%q branch=%q, want %q %q", msg.HeadSHA, msg.Branch, head, f.branch)
	}
	for _, want := range []string{"push failed (" + category + ")", f.branch, head, f.worktree} {
		if !strings.Contains(msg.Reason, want) {
			t.Fatalf("reason %q must contain %q", msg.Reason, want)
		}
	}
	if len(sink.blocked) != 1 || !strings.Contains(sink.blocked[0], f.branch) || !strings.Contains(sink.blocked[0], head) {
		t.Fatalf("block reasons = %q, want one naming branch %s and commit %s", sink.blocked, f.branch, head)
	}
	if len(sink.statuses) != 1 || sink.statuses[0] != models.AssignmentStatusFailed {
		t.Fatalf("assignment statuses = %v, want [failed]", sink.statuses)
	}
	if pushed := f.remoteSHA(t); pushed != "" {
		t.Fatalf("origin holds %s although the push was reported failed", pushed)
	}
}

func TestFinalizeFailsWhenRemoteRejectsPush(t *testing.T) {
	requireSh(t)
	f := newFinalizeFixture(t)
	f.installPreReceiveHook(t, "#!/bin/sh\necho 'rejected by policy' >&2\nexit 1\n")
	head := f.commit(t, "feature.txt", "value=2\n", "feat: initial")

	sink := &recordingSink{}
	finalizeImplementation(context.Background(), f.inputs(), sink)

	assertPushFailure(t, f, sink, head, "rejected")
	if !strings.Contains(sink.published[0].Reason, "rejected by policy") {
		t.Fatalf("reason %q should carry the remote's message", sink.published[0].Reason)
	}
}

func TestFinalizeFailsWhenRemoteIsMissing(t *testing.T) {
	requireSh(t)
	f := newFinalizeFixture(t)
	gitRun(t, f.worktree, "remote", "remove", "origin")
	head := f.commit(t, "feature.txt", "value=2\n", "feat: initial")

	sink := &recordingSink{}
	finalizeImplementation(context.Background(), f.inputs(), sink)

	assertPushFailure(t, f, sink, head, "remote unavailable")
}

func TestFinalizeFailsWhenRemoteHangs(t *testing.T) {
	requireSh(t)
	f := newFinalizeFixture(t)
	// The hook outlives the killed push on Windows, so the remote lives
	// outside t.TempDir and is removed best-effort once the hook exits.
	slow, err := os.MkdirTemp("", "slow-origin-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for i := 0; i < 30; i++ {
			if os.RemoveAll(slow) == nil {
				return
			}
			time.Sleep(500 * time.Millisecond)
		}
	})
	gitRun(t, f.worktree, "init", "--bare", slow)
	gitRun(t, f.worktree, "remote", "set-url", "origin", slow)
	f.remote = slow
	f.installPreReceiveHook(t, "#!/bin/sh\nsleep 6\nexit 1\n")
	head := f.commit(t, "feature.txt", "value=2\n", "feat: initial")

	in := f.inputs()
	in.pushTimeout = time.Second
	sink := &recordingSink{}
	start := time.Now()
	finalizeImplementation(context.Background(), in, sink)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("finalize took %s; the 1s push timeout did not bound the hung remote", elapsed)
	}

	assertPushFailure(t, f, sink, head, "timeout")
}

// TestFinalizeFailsWhenTestRunMutatesTrackedFiles: a test command that
// rewrites a tracked file dirties the tree after the run, so the evidence
// is invalidated; the rerun finds the dirty tree and reports instability
// (naming the file) rather than blaming generation.
func TestFinalizeFailsWhenTestRunMutatesTrackedFiles(t *testing.T) {
	requireSh(t)
	f := newFinalizeFixture(t)
	head := f.commit(t, "feature.txt", "value=2\n", "feat: initial")

	in := f.inputs()
	in.testCommand = "echo mutated >> feature.txt"
	sink := &recordingSink{}
	if finalizeImplementation(context.Background(), in, sink) {
		t.Fatal("finalize returned success although the test run mutated tracked files")
	}

	if len(sink.published) != 1 || sink.published[0].Success || sink.published[0].Verified {
		t.Fatalf("want one unverified failure message, got %+v", sink.published)
	}
	msg := sink.published[0]
	if !strings.Contains(msg.Reason, "verification unstable") || !strings.Contains(msg.Reason, "feature.txt") {
		t.Fatalf("reason %q must report verification unstable and name feature.txt", msg.Reason)
	}
	if strings.Contains(msg.Reason, "uncommitted changes after generation") {
		t.Fatalf("reason %q blames generation for a mutation made by the test run", msg.Reason)
	}
	if msg.TestResults != "skipped: verification unstable" {
		t.Fatalf("test_results = %q, want skipped: verification unstable", msg.TestResults)
	}
	if msg.HeadSHA != head {
		t.Fatalf("head_sha = %q, want %q", msg.HeadSHA, head)
	}
	if pushed := f.remoteSHA(t); pushed != "" {
		t.Fatalf("pushed %s although verification was unstable", pushed)
	}
	if len(sink.statuses) != 1 || sink.statuses[0] != models.AssignmentStatusFailed {
		t.Fatalf("assignment statuses = %v, want [failed]", sink.statuses)
	}
}

// TestResolveBaseSHAUsesIntegrationBase: a review-fix task starts from
// the parent agent branch, not from main. base_sha must be the merge-base
// with main (where the PR diverged), not the worktree's starting HEAD and
// not main's tip, so files_changed covers the whole PR delta.
func TestResolveBaseSHAUsesIntegrationBase(t *testing.T) {
	requireSh(t)
	f := newFinalizeFixture(t)
	mergeBase := f.baseSHA // main's tip when the parent branch was cut

	// Parent implementation branch with one commit.
	gitRun(t, f.worktree, "checkout", "-b", "agent/work/parent")
	parentHead := f.commit(t, "parent.txt", "from parent\n", "feat: parent implementation")

	// main moves on independently, so its tip is not the merge-base.
	gitRun(t, f.worktree, "checkout", "main")
	f.commit(t, "unrelated.txt", "main moved\n", "chore: unrelated main commit")
	mainTip := gitOutput(t, f.worktree, "rev-parse", "HEAD")

	// Review-fix worktree branch starts from the parent branch (the
	// fixture already created it off main, so reset it).
	gitRun(t, f.worktree, "checkout", "-B", f.branch, "agent/work/parent")

	got := resolveBaseSHA(context.Background(), logging.New("implementation-agent-test"), f.worktree, "main", "task")
	if got != mergeBase {
		t.Fatalf("resolveBaseSHA = %s, want merge-base %s (start HEAD %s, main tip %s)", got, mergeBase, parentHead, mainTip)
	}

	// The gate's files_changed then spans the whole PR delta.
	head := f.commit(t, "feature.txt", "value=2\n", "feat: review fix")
	in := f.inputs()
	in.baseSHA = got
	sink := &recordingSink{}
	if !finalizeImplementation(context.Background(), in, sink) {
		t.Fatalf("finalize failed: %+v", sink.published)
	}
	msg := sink.published[0]
	if msg.BaseSHA != mergeBase || msg.HeadSHA != head {
		t.Fatalf("base_sha=%s head_sha=%s, want %s %s", msg.BaseSHA, msg.HeadSHA, mergeBase, head)
	}
	if want := []string{"feature.txt", "parent.txt"}; !reflect.DeepEqual(msg.FilesChanged, want) {
		t.Fatalf("files_changed = %v, want the full PR delta %v", msg.FilesChanged, want)
	}
}

// TestFinalizeRefusesUncommittedChanges: anything generation left
// uncommitted is not in HEAD, so verifying the tree would not verify the
// commit being pushed.
func TestFinalizeRefusesUncommittedChanges(t *testing.T) {
	requireSh(t)
	f := newFinalizeFixture(t)
	head := f.commit(t, "feature.txt", "value=2\n", "feat: initial")
	writeFile(t, f.worktree, "feature.txt", "value=2\nuncommitted\n")

	sink := &recordingSink{}
	finalizeImplementation(context.Background(), f.inputs(), sink)

	if len(sink.published) != 1 || sink.published[0].Success {
		t.Fatalf("want one failure message, got %+v", sink.published)
	}
	if !strings.Contains(sink.published[0].Reason, "uncommitted") {
		t.Fatalf("reason %q should explain the uncommitted changes", sink.published[0].Reason)
	}
	if got := sink.published[0].TestResults; got != "skipped: worktree has uncommitted changes" {
		t.Fatalf("test_results = %q, want a skipped reason", got)
	}
	if pushed := f.remoteSHA(t); pushed != "" {
		t.Fatalf("pushed %s despite uncommitted changes (HEAD %s)", pushed, head)
	}
}

// ---------------------------------------------------------------------------
// M0-07 Task B: the v1 evidence record attached to every finalization path.
// ---------------------------------------------------------------------------

// publishedEvidence returns the evidence carried by the single published
// message, after asserting it is present, valid, consistent with the legacy
// fields, decodable through the consumer entry point, and identical to what
// was handed to the evidence store (when the store accepted it).
func publishedEvidence(t *testing.T, f *finalizeFixture, sink *recordingSink) *evidence.ImplementationEvidence {
	t.Helper()
	if len(sink.published) != 1 {
		t.Fatalf("published %d completion messages, want 1: %+v", len(sink.published), sink.published)
	}
	msg := sink.published[0]
	ev := msg.Evidence
	if ev == nil {
		t.Fatalf("published message carries no evidence: %+v", msg)
	}
	if err := ev.Validate(); err != nil {
		t.Fatalf("emitted evidence fails Validate: %v\n%+v", err, ev)
	}
	if ev.SchemaVersion != evidence.SchemaVersion {
		t.Fatalf("schema_version = %d, want %d", ev.SchemaVersion, evidence.SchemaVersion)
	}
	if ev.WorkID != msg.WorkID || ev.TaskID != msg.TaskID {
		t.Fatalf("evidence ids (%s, %s) differ from message ids (%s, %s)", ev.WorkID, ev.TaskID, msg.WorkID, msg.TaskID)
	}
	if ev.HeadSHA != msg.HeadSHA || ev.BaseSHA != msg.BaseSHA || ev.Branch != msg.Branch {
		t.Fatalf("evidence sha/branch (%s, %s, %s) differ from message (%s, %s, %s)", ev.HeadSHA, ev.BaseSHA, ev.Branch, msg.HeadSHA, msg.BaseSHA, msg.Branch)
	}
	if ev.StartedAt.IsZero() || ev.FinishedAt.IsZero() || ev.FinishedAt.Before(ev.StartedAt) {
		t.Fatalf("started_at=%s finished_at=%s, want real, ordered timestamps", ev.StartedAt, ev.FinishedAt)
	}
	if ev.ChangedFiles == nil || ev.ArtifactRefs == nil {
		t.Fatalf("changed_files=%v artifact_refs=%v must be non-nil slices so they encode as [] not null", ev.ChangedFiles, ev.ArtifactRefs)
	}
	// The wire form must decode and validate through the consumer entry point
	// and the additive field must be spelled "evidence".
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	field, ok := wire["evidence"]
	if !ok {
		t.Fatalf("wire message has no evidence field: %s", raw)
	}
	parsed, err := evidence.Parse(field)
	if err != nil {
		t.Fatalf("evidence.Parse on the wire field: %v", err)
	}
	if parsed.Outcome != ev.Outcome || parsed.HeadSHA != ev.HeadSHA {
		t.Fatalf("wire evidence (%s, %s) differs from in-memory (%s, %s)", parsed.Outcome, parsed.HeadSHA, ev.Outcome, ev.HeadSHA)
	}
	if f.store.err == nil {
		if len(f.store.inserted) != 1 {
			t.Fatalf("evidence store received %d inserts, want 1", len(f.store.inserted))
		}
		if !reflect.DeepEqual(f.store.inserted[0], ev) {
			t.Fatalf("persisted evidence differs from published evidence:\n%+v\n%+v", f.store.inserted[0], ev)
		}
	}
	return ev
}

// checkOfKind returns the single check of the given kind, failing when there
// is not exactly one.
func checkOfKind(t *testing.T, ev *evidence.ImplementationEvidence, kind evidence.CheckKind) evidence.CheckResult {
	t.Helper()
	var found []evidence.CheckResult
	for _, c := range ev.Checks {
		if c.Kind == kind {
			found = append(found, c)
		}
	}
	if len(found) != 1 {
		t.Fatalf("checks of kind %q = %d, want exactly 1: %+v", kind, len(found), ev.Checks)
	}
	return found[0]
}

func assertNoCheckOfKind(t *testing.T, ev *evidence.ImplementationEvidence, kind evidence.CheckKind) {
	t.Helper()
	for _, c := range ev.Checks {
		if c.Kind == kind {
			t.Fatalf("unexpected check of kind %q: %+v", kind, c)
		}
	}
}

// assertStoredRef checks that ref names an output the sink stored under
// name, that the ref appears in artifact_refs, and (optionally) that the
// stored content contains want.
func assertStoredRef(t *testing.T, sink *recordingSink, ev *evidence.ImplementationEvidence, ref, name, want string) {
	t.Helper()
	if ref == "" {
		t.Fatalf("no reference recorded for %s output", name)
	}
	stored, ok := sink.artifacts[ref]
	if !ok {
		t.Fatalf("ref %q was not stored through the sink (stored: %v)", ref, sink.artifacts)
	}
	if stored.name != name {
		t.Fatalf("ref %q stored under name %q, want %q", ref, stored.name, name)
	}
	if want != "" && !strings.Contains(stored.content, want) {
		t.Fatalf("stored %s output %q does not contain %q", name, stored.content, want)
	}
	for _, r := range ev.ArtifactRefs {
		if r == ref {
			return
		}
	}
	t.Fatalf("artifact_refs %v does not list %q", ev.ArtifactRefs, ref)
}

// TestFinalizeEvidenceVerifiedOnPushedHead: passed gate + successful push
// yields outcome verified, bound to the SHA origin holds, with the test
// check passed and its raw output stored by reference.
func TestFinalizeEvidenceVerifiedOnPushedHead(t *testing.T) {
	requireSh(t)
	f := newFinalizeFixture(t)
	f.commit(t, "feature.txt", "value=2\n", "feat: initial")
	head := f.commit(t, "notes.txt", "continuation\n", "feat: continuation")

	in := f.inputs()
	repoID := uuid.NewString()
	in.task.RepositoryID = repoID
	sink := &recordingSink{}
	if !finalizeImplementation(context.Background(), in, sink) {
		t.Fatalf("finalize failed: %+v", sink.published)
	}

	ev := publishedEvidence(t, f, sink)
	if ev.Outcome != evidence.OutcomeVerified {
		t.Fatalf("outcome = %q, want verified", ev.Outcome)
	}
	if pushed := f.remoteSHA(t); ev.HeadSHA != pushed || pushed != head {
		t.Fatalf("head_sha=%s pushed=%s local HEAD=%s, want all equal", ev.HeadSHA, pushed, head)
	}
	if ev.BaseSHA != f.baseSHA || ev.Branch != f.branch {
		t.Fatalf("base_sha=%s branch=%s, want %s %s", ev.BaseSHA, ev.Branch, f.baseSHA, f.branch)
	}
	if ev.RepositoryID == nil || *ev.RepositoryID != repoID {
		t.Fatalf("repository_id = %v, want %s from the task", ev.RepositoryID, repoID)
	}
	if want := splitLines(gitOutput(t, f.worktree, "diff", "--name-only", f.baseSHA+"...HEAD")); !reflect.DeepEqual(ev.ChangedFiles, want) {
		t.Fatalf("changed_files = %v, want the git diff %v", ev.ChangedFiles, want)
	}
	if !reflect.DeepEqual(ev.ChangedFiles, []string{"feature.txt", "notes.txt"}) {
		t.Fatalf("changed_files = %v, want [feature.txt notes.txt] (never the planner's %v)", ev.ChangedFiles, in.task.FilesToChange)
	}
	// The changed files are not JS/TS, so the import verifier inspected
	// nothing: no diagnostics check is recorded (it neither ran nor was
	// deliberately skipped) and the passed test check alone is verified.
	if len(ev.Checks) != 1 {
		t.Fatalf("checks = %+v, want exactly the test check (no applicable diagnostics; a passed push is not a check)", ev.Checks)
	}
	test := checkOfKind(t, ev, evidence.CheckKindTest)
	if test.Status != evidence.CheckPassed || test.Command != fixtureTestCommand {
		t.Fatalf("test check = %+v, want passed %q", test, fixtureTestCommand)
	}
	assertNoCheckOfKind(t, ev, evidence.CheckKindDiagnostic)
	assertNoCheckOfKind(t, ev, evidence.CheckKindOther)
	if test.ExitCode == nil || *test.ExitCode != 0 {
		t.Fatalf("test exit_code = %v, want 0", test.ExitCode)
	}
	if test.StartedAt == nil || test.FinishedAt == nil || test.FinishedAt.Before(*test.StartedAt) {
		t.Fatalf("test check timestamps started=%v finished=%v", test.StartedAt, test.FinishedAt)
	}
	assertStoredRef(t, sink, ev, test.OutputRef, rawOutputTest, "")
	if len(ev.ArtifactRefs) != 2 {
		t.Fatalf("artifact_refs = %v, want the test output and the push output", ev.ArtifactRefs)
	}
	var pushRef string
	for _, r := range ev.ArtifactRefs {
		if r != test.OutputRef {
			pushRef = r
		}
	}
	assertStoredRef(t, sink, ev, pushRef, rawOutputPush, f.branch)
	if ev.Reason != "" {
		t.Fatalf("reason = %q on a verified run, want empty", ev.Reason)
	}
	if d := evidence.ReleaseDecision(ev, evidence.ReleasePolicy{}); d.Action != evidence.ActionRelease {
		t.Fatalf("release decision = %+v, want release", d)
	}
	// Legacy fields keep their meaning.
	msg := sink.published[0]
	if !msg.Success || !msg.Verified || msg.TestResults != "passed: "+fixtureTestCommand {
		t.Fatalf("legacy fields changed: %+v", msg)
	}
}

// TestFinalizeEvidenceSkippedRunnerIsPartiallyVerified is the D2 rule at
// the producer: with no test runner the test check is skipped (with the
// reason) while the import-diagnostics check actually inspected a JS file
// and passed, so the outcome is partially_verified, never verified. The
// push is not a check. Downstream, the product's partial_verification_opt_in
// decides between human review and block.
func TestFinalizeEvidenceSkippedRunnerIsPartiallyVerified(t *testing.T) {
	f := newFinalizeFixture(t)
	f.commit(t, "feature.txt", "value=3\n", "feat: no runner")
	head := f.commitJS(t, jsClean)

	in := f.inputs()
	in.testCommand = ""
	in.policies = partialPolicySource(t, &in)
	sink := &recordingSink{}
	if !finalizeImplementation(context.Background(), in, sink) {
		t.Fatalf("finalize failed: %+v", sink.published)
	}

	ev := publishedEvidence(t, f, sink)
	if ev.Outcome != evidence.OutcomePartiallyVerified {
		t.Fatalf("outcome = %q, want partially_verified (test skipped, diagnostics passed)", ev.Outcome)
	}
	if ev.HeadSHA != head || f.remoteSHA(t) != head {
		t.Fatalf("head_sha=%s pushed=%s, want %s", ev.HeadSHA, f.remoteSHA(t), head)
	}
	if len(ev.Checks) != 2 {
		t.Fatalf("checks = %+v, want exactly the skipped test check and the passed diagnostics check", ev.Checks)
	}
	test := checkOfKind(t, ev, evidence.CheckKindTest)
	if test.Status != evidence.CheckSkipped || test.SkipReason != "no test runner detected" {
		t.Fatalf("test check = %+v, want skipped with reason no test runner detected", test)
	}
	if test.ExitCode != nil || test.OutputRef != "" {
		t.Fatalf("skipped check must carry no exit code or output: %+v", test)
	}
	diag := checkOfKind(t, ev, evidence.CheckKindDiagnostic)
	if diag.Status != evidence.CheckPassed || diag.Command != diagnosticCheckCommand {
		t.Fatalf("diagnostics check = %+v, want passed %q", diag, diagnosticCheckCommand)
	}
	assertNoCheckOfKind(t, ev, evidence.CheckKindOther)
	if len(ev.ArtifactRefs) != 1 {
		t.Fatalf("artifact_refs = %v, want only the push output", ev.ArtifactRefs)
	}
	assertStoredRef(t, sink, ev, ev.ArtifactRefs[0], rawOutputPush, f.branch)
	if ev.Reason == "" {
		t.Fatal("reason must explain a partially verified run")
	}
	if d := evidence.ReleaseDecision(ev, evidence.ReleasePolicy{}); d.Action != evidence.ActionBlock {
		t.Fatalf("release decision without opt-in = %+v, want block (D2 default)", d)
	}
	if d := evidence.ReleaseDecision(ev, evidence.ReleasePolicy{PartialVerificationOptIn: true}); d.Action != evidence.ActionHumanReview {
		t.Fatalf("release decision with opt-in = %+v, want human_review (D2 opt-in is reachable from the live producer)", d)
	}
	msg := sink.published[0]
	if !msg.Success || msg.Verified || msg.TestResults != "skipped: no test runner detected" {
		t.Fatalf("legacy fields changed: %+v", msg)
	}
}

// jsFixture selects what JS the outcome-table fixture commits: nothing (the
// import verifier has no applicable file), a clean file whose relative
// import resolves, or a file whose relative import resolves to nothing.
type jsFixture int

const (
	jsNone jsFixture = iota
	jsClean
	jsBroken
)

// jsImportFile is the JS file the fixture commits under src/.
const jsImportFile = "src/app.js"

// commitJS commits src/app.js (and, for jsClean, the src/util.js it
// imports) on the subtask branch and returns the new HEAD. jsNone commits
// nothing and returns the current HEAD.
func (f *finalizeFixture) commitJS(t *testing.T, js jsFixture) string {
	t.Helper()
	if js == jsNone {
		return gitOutput(t, f.worktree, "rev-parse", "HEAD")
	}
	if err := os.MkdirAll(filepath.Join(f.worktree, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if js == jsClean {
		writeFile(t, f.worktree, "src/util.js", "export function helper() { return 2; }\n")
		return f.commit(t, jsImportFile, "import { helper } from './util';\nexport const x = helper();\n", "feat: clean import")
	}
	return f.commit(t, jsImportFile, "import { helper } from './missing';\nexport const x = helper();\n", "feat: broken import")
}

// TestFinalizeEvidenceOutcomeTable pins the producer's outcome for every
// combination of test runner and import diagnostics. The diagnostics check
// (kind diagnostic, codex_verify_imports) is recorded only when the verifier
// inspected at least one applicable (JS/TS) file: passed when clean, failed
// when unresolved imports remain. When nothing was applicable no diagnostics
// check is recorded at all — it neither ran nor was deliberately skipped —
// so it can neither lift a skipped-runner run to partially_verified (the
// D2 hole) nor drag a passed-runner run below verified. Unresolved imports
// are blocking at the final gate: broken dependencies are failures under
// D2, never partial verification.
func TestFinalizeEvidenceOutcomeTable(t *testing.T) {
	requireSh(t)
	cases := []struct {
		name        string
		testCommand string
		js          jsFixture
		wantOutcome evidence.Outcome
		wantSuccess bool
		wantTest    evidence.CheckStatus
		// wantDiag is "" when no diagnostics check must be recorded.
		wantDiag  evidence.CheckStatus
		wantOptIn evidence.ReleaseAction
	}{
		{
			name:        "runner passed, diagnostics not applicable",
			testCommand: fixtureTestCommand,
			js:          jsNone,
			wantOutcome: evidence.OutcomeVerified,
			wantSuccess: true,
			wantTest:    evidence.CheckPassed,
			wantDiag:    "",
			wantOptIn:   evidence.ActionRelease,
		},
		{
			name:        "runner passed, diagnostics applicable and clean",
			testCommand: fixtureTestCommand,
			js:          jsClean,
			wantOutcome: evidence.OutcomeVerified,
			wantSuccess: true,
			wantTest:    evidence.CheckPassed,
			wantDiag:    evidence.CheckPassed,
			wantOptIn:   evidence.ActionRelease,
		},
		{
			name:        "no runner, diagnostics applicable and clean",
			testCommand: "",
			js:          jsClean,
			wantOutcome: evidence.OutcomePartiallyVerified,
			wantSuccess: true,
			wantTest:    evidence.CheckSkipped,
			wantDiag:    evidence.CheckPassed,
			wantOptIn:   evidence.ActionHumanReview,
		},
		{
			name:        "no runner, diagnostics not applicable",
			testCommand: "",
			js:          jsNone,
			wantOutcome: evidence.OutcomeUnverified,
			wantSuccess: true,
			wantTest:    evidence.CheckSkipped,
			wantDiag:    "",
			wantOptIn:   evidence.ActionBlock,
		},
		{
			name:        "no runner, unresolved imports",
			testCommand: "",
			js:          jsBroken,
			wantOutcome: evidence.OutcomeFailed,
			wantSuccess: false,
			wantTest:    evidence.CheckSkipped,
			wantDiag:    evidence.CheckFailed,
			wantOptIn:   evidence.ActionBlock,
		},
		{
			name:        "runner present, unresolved imports",
			testCommand: fixtureTestCommand,
			js:          jsBroken,
			wantOutcome: evidence.OutcomeFailed,
			wantSuccess: false,
			wantTest:    evidence.CheckSkipped,
			wantDiag:    evidence.CheckFailed,
			wantOptIn:   evidence.ActionBlock,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFinalizeFixture(t)
			f.commit(t, "feature.txt", "value=2\n", "feat: initial")
			head := f.commitJS(t, tc.js)

			in := f.inputs()
			in.testCommand = tc.testCommand
			// The table pins the D2 partial path; it needs the explicit
			// partial_with_human_review repository policy (M3-03). The
			// default-policy block is TestFinalizePolicyBlocksWhenNoChecks.
			in.policies = partialPolicySource(t, &in)
			sink := &recordingSink{}
			if got := finalizeImplementation(context.Background(), in, sink); got != tc.wantSuccess {
				t.Fatalf("finalize returned %v, want %v: %+v", got, tc.wantSuccess, sink.published)
			}

			ev := publishedEvidence(t, f, sink)
			if ev.Outcome != tc.wantOutcome {
				t.Fatalf("outcome = %q, want %q (checks %+v)", ev.Outcome, tc.wantOutcome, ev.Checks)
			}
			if ev.HeadSHA != head {
				t.Fatalf("head_sha = %s, want %s", ev.HeadSHA, head)
			}
			test := checkOfKind(t, ev, evidence.CheckKindTest)
			if test.Status != tc.wantTest {
				t.Fatalf("test check = %+v, want %s", test, tc.wantTest)
			}
			if tc.wantDiag == "" {
				assertNoCheckOfKind(t, ev, evidence.CheckKindDiagnostic)
				if len(ev.Checks) != 1 {
					t.Fatalf("checks = %+v, want only the test check when no file was applicable to the verifier", ev.Checks)
				}
			} else {
				diag := checkOfKind(t, ev, evidence.CheckKindDiagnostic)
				if diag.Status != tc.wantDiag || diag.Command != diagnosticCheckCommand {
					t.Fatalf("diagnostics check = %+v, want %s %q", diag, tc.wantDiag, diagnosticCheckCommand)
				}
				if diag.StartedAt == nil || diag.FinishedAt == nil || diag.FinishedAt.Before(*diag.StartedAt) {
					t.Fatalf("diagnostics check timestamps started=%v finished=%v", diag.StartedAt, diag.FinishedAt)
				}
			}
			// The diagnostic failure is the failing check; no synthetic
			// run-level failure is added on top of it.
			assertNoCheckOfKind(t, ev, evidence.CheckKindOther)
			if d := evidence.ReleaseDecision(ev, evidence.ReleasePolicy{PartialVerificationOptIn: true}); d.Action != tc.wantOptIn {
				t.Fatalf("release decision with opt-in = %+v, want %s", d, tc.wantOptIn)
			}
			if d := evidence.ReleaseDecision(ev, evidence.ReleasePolicy{}); tc.wantOutcome != evidence.OutcomeVerified && d.Action != evidence.ActionBlock {
				t.Fatalf("release decision without opt-in = %+v, want block", d)
			}

			msg := sink.published[0]
			pushed := f.remoteSHA(t)
			if tc.wantSuccess {
				if pushed != head || !msg.Success {
					t.Fatalf("pushed=%s success=%v, want %s pushed and success", pushed, msg.Success, head)
				}
				if msg.Verified != (tc.wantOutcome == evidence.OutcomeVerified) {
					t.Fatalf("verified = %v for outcome %s", msg.Verified, tc.wantOutcome)
				}
				return
			}
			if pushed != "" || msg.Success || msg.Verified {
				t.Fatalf("pushed=%q success=%v verified=%v after a failed gate", pushed, msg.Success, msg.Verified)
			}
			if test.SkipReason != "unresolved imports remain at HEAD" {
				t.Fatalf("test skip_reason = %q, want the diagnostics abort reason", test.SkipReason)
			}
			for _, want := range []string{jsImportFile, "./missing", head} {
				if !strings.Contains(msg.Reason, want) {
					t.Fatalf("reason %q must name %q so the operator can fix the import", msg.Reason, want)
				}
			}
			if len(sink.blocked) != 1 || len(sink.statuses) != 1 || sink.statuses[0] != models.AssignmentStatusFailed {
				t.Fatalf("blocked=%v statuses=%v, want one block and [failed]", sink.blocked, sink.statuses)
			}
		})
	}
}

// TestFinalizeEvidenceFailedTests: a failing suite yields outcome failed
// through the test check itself (exit code, stored output); no push happens
// and no run-level failure check is needed.
func TestFinalizeEvidenceFailedTests(t *testing.T) {
	requireSh(t)
	f := newFinalizeFixture(t)
	broken := f.commit(t, "feature.txt", "value=3\n", "feat: tests fail")

	sink := &recordingSink{}
	if finalizeImplementation(context.Background(), f.inputs(), sink) {
		t.Fatal("finalize returned success although tests fail")
	}

	ev := publishedEvidence(t, f, sink)
	if ev.Outcome != evidence.OutcomeFailed {
		t.Fatalf("outcome = %q, want failed", ev.Outcome)
	}
	if ev.HeadSHA != broken {
		t.Fatalf("head_sha = %s, want the failing commit %s", ev.HeadSHA, broken)
	}
	if !reflect.DeepEqual(ev.ChangedFiles, []string{"feature.txt"}) {
		t.Fatalf("changed_files = %v, want [feature.txt]", ev.ChangedFiles)
	}
	test := checkOfKind(t, ev, evidence.CheckKindTest)
	if test.Status != evidence.CheckFailed || test.Command != fixtureTestCommand {
		t.Fatalf("test check = %+v, want failed %q", test, fixtureTestCommand)
	}
	if test.ExitCode == nil || *test.ExitCode != 1 {
		t.Fatalf("test exit_code = %v, want 1 (grep -q miss)", test.ExitCode)
	}
	assertStoredRef(t, sink, ev, test.OutputRef, rawOutputTest, "")
	assertNoCheckOfKind(t, ev, evidence.CheckKindOther)
	if len(ev.ArtifactRefs) != 1 {
		t.Fatalf("artifact_refs = %v, want only the test output (nothing was pushed)", ev.ArtifactRefs)
	}
	if ev.Reason != sink.published[0].Reason || !strings.Contains(ev.Reason, broken) {
		t.Fatalf("reason %q must equal the operator text and name %s", ev.Reason, broken)
	}
}

// TestFinalizeEvidencePushFailure: tests passed, the push was rejected. The
// evidence keeps the passed test check as observed and adds the run-level
// failure as a failed check of kind other naming the push command, so the
// outcome is failed (D1.1) and the reason is the operator text.
func TestFinalizeEvidencePushFailure(t *testing.T) {
	requireSh(t)
	f := newFinalizeFixture(t)
	f.installPreReceiveHook(t, "#!/bin/sh\necho 'rejected by policy' >&2\nexit 1\n")
	head := f.commit(t, "feature.txt", "value=2\n", "feat: initial")

	sink := &recordingSink{}
	if finalizeImplementation(context.Background(), f.inputs(), sink) {
		t.Fatal("finalize returned success although the push was rejected")
	}

	ev := publishedEvidence(t, f, sink)
	if ev.Outcome != evidence.OutcomeFailed {
		t.Fatalf("outcome = %q, want failed", ev.Outcome)
	}
	if ev.HeadSHA != head {
		t.Fatalf("head_sha = %s, want %s", ev.HeadSHA, head)
	}
	test := checkOfKind(t, ev, evidence.CheckKindTest)
	if test.Status != evidence.CheckPassed {
		t.Fatalf("test check = %+v, want passed as observed", test)
	}
	push := checkOfKind(t, ev, evidence.CheckKindOther)
	if push.Status != evidence.CheckFailed {
		t.Fatalf("run-level check = %+v, want failed", push)
	}
	if !strings.Contains(push.Command, "git push") || !strings.Contains(push.Command, head+":refs/heads/"+f.branch) {
		t.Fatalf("run-level check command = %q, want the push refspec", push.Command)
	}
	if push.ExitCode == nil || *push.ExitCode == 0 {
		t.Fatalf("push exit_code = %v, want non-zero", push.ExitCode)
	}
	assertStoredRef(t, sink, ev, push.OutputRef, rawOutputPush, "rejected by policy")
	assertStoredRef(t, sink, ev, test.OutputRef, rawOutputTest, "")
	if ev.Reason != sink.published[0].Reason || !strings.Contains(ev.Reason, "push failed (rejected)") {
		t.Fatalf("reason %q must be the operator text naming the push failure", ev.Reason)
	}
	if d := evidence.ReleaseDecision(ev, evidence.ReleasePolicy{PartialVerificationOptIn: true}); d.Action != evidence.ActionBlock {
		t.Fatalf("release decision = %+v, want block", d)
	}
}

// TestFinalizeEvidenceGateAbort: a gate abort (here: uncommitted changes)
// records the test check as skipped with the gate's reason plus a failed
// run-level check, so the outcome is failed even though no test failed.
func TestFinalizeEvidenceGateAbort(t *testing.T) {
	requireSh(t)
	f := newFinalizeFixture(t)
	head := f.commit(t, "feature.txt", "value=2\n", "feat: initial")
	writeFile(t, f.worktree, "feature.txt", "value=2\nuncommitted\n")

	sink := &recordingSink{}
	if finalizeImplementation(context.Background(), f.inputs(), sink) {
		t.Fatal("finalize returned success with uncommitted changes")
	}

	ev := publishedEvidence(t, f, sink)
	if ev.Outcome != evidence.OutcomeFailed {
		t.Fatalf("outcome = %q, want failed", ev.Outcome)
	}
	if ev.HeadSHA != head {
		t.Fatalf("head_sha = %s, want %s", ev.HeadSHA, head)
	}
	test := checkOfKind(t, ev, evidence.CheckKindTest)
	if test.Status != evidence.CheckSkipped || test.SkipReason != "worktree has uncommitted changes" {
		t.Fatalf("test check = %+v, want skipped: worktree has uncommitted changes", test)
	}
	gate := checkOfKind(t, ev, evidence.CheckKindOther)
	if gate.Status != evidence.CheckFailed || gate.Command != gateCheckCommand {
		t.Fatalf("run-level check = %+v, want failed %q", gate, gateCheckCommand)
	}
	if len(ev.ArtifactRefs) != 0 {
		t.Fatalf("artifact_refs = %v, want none (nothing ran)", ev.ArtifactRefs)
	}
	if ev.Reason != sink.published[0].Reason || !strings.Contains(ev.Reason, "uncommitted") {
		t.Fatalf("reason %q must be the operator text", ev.Reason)
	}
}

// TestFinalizeEvidenceUnstableVerification: the test run mutated tracked
// files, so the observation is discarded: skipped test check with the
// instability reason plus the run-level failure.
func TestFinalizeEvidenceUnstableVerification(t *testing.T) {
	requireSh(t)
	f := newFinalizeFixture(t)
	head := f.commit(t, "feature.txt", "value=2\n", "feat: initial")

	in := f.inputs()
	in.testCommand = "echo mutated >> feature.txt"
	sink := &recordingSink{}
	if finalizeImplementation(context.Background(), in, sink) {
		t.Fatal("finalize returned success although verification was unstable")
	}

	ev := publishedEvidence(t, f, sink)
	if ev.Outcome != evidence.OutcomeFailed || ev.HeadSHA != head {
		t.Fatalf("outcome=%q head=%s, want failed at %s", ev.Outcome, ev.HeadSHA, head)
	}
	test := checkOfKind(t, ev, evidence.CheckKindTest)
	if test.Status != evidence.CheckSkipped || test.SkipReason != "verification unstable" {
		t.Fatalf("test check = %+v, want skipped: verification unstable", test)
	}
	if checkOfKind(t, ev, evidence.CheckKindOther).Status != evidence.CheckFailed {
		t.Fatal("run-level failure check missing")
	}
	if !strings.Contains(ev.Reason, "verification unstable") || !strings.Contains(ev.Reason, "feature.txt") {
		t.Fatalf("reason %q must report instability and name the file", ev.Reason)
	}
}

// TestFinalizeEvidencePersistFailureRollsBackSuccess: the evidence row and
// the implementation.complete outbox row commit together (M1-04), so a
// failing insert takes the success event down with it. What is committed
// instead is a blocking failure that still carries the evidence, unchanged
// (verified, bound to the pushed head), so downstream learns the task did
// not complete and a human sees why.
func TestFinalizeEvidencePersistFailureRollsBackSuccess(t *testing.T) {
	requireSh(t)
	f := newFinalizeFixture(t)
	head := f.commit(t, "feature.txt", "value=2\n", "feat: initial")
	f.store.err = errors.New("postgres is down")

	sink := &recordingSink{}
	if finalizeImplementation(context.Background(), f.inputs(), sink) {
		t.Fatal("finalize reported success although the success outcome never committed")
	}
	if len(sink.attempts) != 2 || sink.attempts[0].msg == nil || !sink.attempts[0].msg.Success || sink.attempts[0].evidence == nil {
		t.Fatalf("attempts = %+v, want the success outcome with its evidence, then the fallback", sink.attempts)
	}
	ev := publishedEvidence(t, f, sink)
	if ev.Outcome != evidence.OutcomeVerified || ev.HeadSHA != head {
		t.Fatalf("outcome=%q head=%s, want the unchanged verified evidence at %s", ev.Outcome, ev.HeadSHA, head)
	}
	if ev != sink.attempts[0].evidence {
		t.Fatal("the fallback message must carry the very evidence record the success outcome carried")
	}
	if len(f.store.inserted) != 0 {
		t.Fatalf("store recorded %d inserts although it was failing", len(f.store.inserted))
	}
	if sink.attempts[1].evidence != nil {
		t.Fatal("the fallback retried the evidence insert that just failed")
	}
	msg := sink.published[0]
	if msg.Success || msg.Verified || !strings.Contains(msg.Reason, head) || !strings.Contains(msg.Reason, "could not be recorded") {
		t.Fatalf("committed message = %+v, want success=false naming the pushed head and the recording failure", msg)
	}
	if len(sink.blocked) != 1 || sink.blocked[0] != msg.Reason {
		t.Fatalf("blocked = %q, want one block with the message's reason", sink.blocked)
	}
	if len(sink.statuses) != 1 || sink.statuses[0] != models.AssignmentStatusFailed {
		t.Fatalf("assignment statuses = %v, want [failed]", sink.statuses)
	}
}

// TestFinalizeFailureFallbackTier1KeepsTheBlock: when a failure outcome
// cannot commit, tier 1 drops only the evidence row and keeps the parent
// block and the unchanged event (the evidence insert is the likeliest
// statement to have failed).
func TestFinalizeFailureFallbackTier1KeepsTheBlock(t *testing.T) {
	requireSh(t)
	f := newFinalizeFixture(t)
	broken := f.commit(t, "feature.txt", "value=3\n", "feat: wrong")

	sink := &recordingSink{failNext: 1}
	if finalizeImplementation(context.Background(), f.inputs(), sink) {
		t.Fatal("finalize succeeded on failing tests")
	}
	if len(sink.attempts) != 2 || sink.attempts[0].evidence == nil || sink.attempts[1].evidence != nil {
		t.Fatalf("attempts = %+v, want the full failure, then tier 1 without the evidence row", sink.attempts)
	}
	if sink.attempts[1].blockReason != sink.attempts[0].blockReason {
		t.Fatalf("tier 1 block = %q, want the original %q", sink.attempts[1].blockReason, sink.attempts[0].blockReason)
	}
	if len(sink.published) != 1 || sink.published[0].Success || sink.published[0].HeadSHA != broken {
		t.Fatalf("published = %+v, want the failure event for %s", sink.published, broken)
	}
	if !reflect.DeepEqual(sink.published[0], *sink.attempts[0].msg) {
		t.Fatalf("fallback changed the failure message:\n%+v\n%+v", sink.published[0], *sink.attempts[0].msg)
	}
	if len(sink.blocked) != 1 || !strings.Contains(sink.blocked[0], broken) || len(sink.statuses) != 1 || sink.statuses[0] != models.AssignmentStatusFailed {
		t.Fatalf("blocked=%q statuses=%v, want the original block and [failed]", sink.blocked, sink.statuses)
	}
}

// TestFinalizeFailureFallbackTier2KeepsTheEvent: only when tier 1 also
// fails (an item that can no longer be blocked) is the block dropped, so
// the completion event itself is not lost.
func TestFinalizeFailureFallbackTier2KeepsTheEvent(t *testing.T) {
	requireSh(t)
	f := newFinalizeFixture(t)
	broken := f.commit(t, "feature.txt", "value=3\n", "feat: wrong")

	sink := &recordingSink{failNext: 2}
	if finalizeImplementation(context.Background(), f.inputs(), sink) {
		t.Fatal("finalize succeeded on failing tests")
	}
	if len(sink.attempts) != 3 || sink.attempts[1].blockReason == "" || sink.attempts[2].blockReason != "" {
		t.Fatalf("attempts = %+v, want full, tier 1 with the block, tier 2 without", sink.attempts)
	}
	if len(sink.published) != 1 || sink.published[0].Success || sink.published[0].HeadSHA != broken {
		t.Fatalf("published = %+v, want the failure event for %s", sink.published, broken)
	}
	if len(sink.blocked) != 0 || len(sink.statuses) != 1 || sink.statuses[0] != models.AssignmentStatusFailed {
		t.Fatalf("blocked=%q statuses=%v, want no block and [failed]", sink.blocked, sink.statuses)
	}
}

// TestFinalizeEvidenceWithoutStore: a nil store (no pool wired) still
// publishes evidence.
func TestFinalizeEvidenceWithoutStore(t *testing.T) {
	requireSh(t)
	f := newFinalizeFixture(t)
	f.commit(t, "feature.txt", "value=2\n", "feat: initial")

	in := f.inputs()
	in.evidenceStore = nil
	sink := &recordingSink{}
	if !finalizeImplementation(context.Background(), in, sink) {
		t.Fatalf("finalize failed without a store: %+v", sink.published)
	}
	if ev := sink.published[0].Evidence; ev == nil || ev.Outcome != evidence.OutcomeVerified {
		t.Fatalf("evidence = %+v, want verified", ev)
	}
}

// TestFinalizeEvidenceOutputFallsBackToFile: when the artifact store
// refuses the raw output, it is written under the worktree-adjacent raw
// directory and referenced by path, so the output is never lost silently.
func TestFinalizeEvidenceOutputFallsBackToFile(t *testing.T) {
	requireSh(t)
	f := newFinalizeFixture(t)
	f.commit(t, "feature.txt", "value=2\n", "feat: initial")

	in := f.inputs()
	in.clonePath = filepath.Join(t.TempDir(), "repo-cache", "product", "repo")
	sink := &recordingSink{storeErr: errors.New("artifact table unavailable")}
	if !finalizeImplementation(context.Background(), in, sink) {
		t.Fatalf("finalize failed: %+v", sink.published)
	}
	ev := publishedEvidence(t, f, sink)
	test := checkOfKind(t, ev, evidence.CheckKindTest)
	wantDir := rawOutputDir(in.clonePath, in.taskID)
	if !strings.HasPrefix(test.OutputRef, rawOutputFileRefPrefix) {
		t.Fatalf("output_ref = %q, want a %s reference", test.OutputRef, rawOutputFileRefPrefix)
	}
	path := strings.TrimPrefix(test.OutputRef, rawOutputFileRefPrefix)
	if filepath.Dir(path) != filepath.Clean(wantDir) {
		t.Fatalf("fallback path %q is not under the raw dir %q", path, wantDir)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("fallback file missing: %v", err)
	}
	if len(ev.ArtifactRefs) != 2 {
		t.Fatalf("artifact_refs = %v, want test and push file refs", ev.ArtifactRefs)
	}
}

// TestFinalizeEvidenceStoredOutputIsCapped: only the tail of an oversized
// output is stored, marked as cut, and NUL bytes (which JSONB rejects) are
// replaced.
func TestFinalizeEvidenceStoredOutputIsCapped(t *testing.T) {
	big := strings.Repeat("x", maxStoredOutputBytes+10) + "END"
	got := string(capOutput([]byte(big)))
	if len(got) > maxStoredOutputBytes+len(outputCutMarker) || !strings.HasSuffix(got, "END") || !strings.HasPrefix(got, outputCutMarker) {
		t.Fatalf("capOutput kept %d bytes, prefix %q suffix %q", len(got), got[:3], got[len(got)-3:])
	}
	if small := capOutput([]byte("ok\x00bye")); strings.ContainsRune(string(small), 0) || !strings.HasPrefix(string(small), "ok") || !strings.HasSuffix(string(small), "bye") {
		t.Fatalf("NUL bytes must be replaced for JSONB storage, got %q", small)
	}
}
