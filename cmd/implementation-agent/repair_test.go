package main

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/controller"
	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/failures"
	"github.com/autonomousengine/autonomous-engine/internal/interactions"
	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
	"github.com/autonomousengine/autonomous-engine/internal/llm/strategy"
	"github.com/autonomousengine/autonomous-engine/internal/llm/tools"
	"github.com/autonomousengine/autonomous-engine/internal/logging"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// M3-06: the fingerprint-bounded repair loop and the D4 budgets, driven
// through the real generation phase over a real git worktree with a
// scripted model and a scripted test runner, then reported through
// finalizeStopped into the recording sink.

// repairRunner is the repository's test runner: it passes when
// feature.txt says "done" and otherwise fails like go test, with a test
// named after the word in feature.txt — so each distinct content is a
// distinct failure fingerprint and the same content is the same one.
const repairRunner = `w=$(cat feature.txt)
if [ "$w" = "done" ]; then echo "ok  	example.com/app	0.01s"; exit 0; fi
printf -- '--- FAIL: Test_%s (0.00s)\n    feature_test.go:7: got %s, want done\nFAIL\nFAIL	example.com/app	0.01s\n' "$w" "$w"
exit 1
`

// fakeClock is a settable clock for the active-time budget.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)} }
func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// step is one scripted model response.
type step struct {
	calls []adapter.ToolCall
	text  string
}

// queueAdapter answers each Complete with the next step (then "Done."),
// charging tokens and advancing the clock per call.
type queueAdapter struct {
	mu      sync.Mutex
	steps   []step
	n       int
	tokens  int
	clock   *fakeClock
	perCall time.Duration
	calls   int
	prompts []string
}

func (a *queueAdapter) Complete(_ context.Context, req adapter.CompletionRequest) (*adapter.CompletionResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	if a.clock != nil {
		a.clock.Advance(a.perCall)
	}
	for _, m := range req.Messages {
		if m.Role == adapter.RoleUser && len(req.Messages) == 2 {
			a.prompts = append(a.prompts, m.Content)
		}
	}
	half := a.tokens / 2
	if a.n < len(a.steps) {
		s := a.steps[a.n]
		a.n++
		if len(s.calls) > 0 {
			return &adapter.CompletionResponse{ToolCalls: s.calls, FinishReason: "tool_calls", InputTokens: half, OutputTokens: half}, nil
		}
		return &adapter.CompletionResponse{Content: s.text, FinishReason: "stop", InputTokens: half, OutputTokens: half}, nil
	}
	return &adapter.CompletionResponse{Content: "Done.", FinishReason: "stop", InputTokens: half, OutputTokens: half}, nil
}
func (a *queueAdapter) Stream(context.Context, adapter.CompletionRequest) (<-chan adapter.StreamChunk, error) {
	return nil, errors.New("not used")
}
func (a *queueAdapter) Capabilities() adapter.ModelCapabilities {
	return adapter.ModelCapabilities{MaxContextTokens: 100000, SupportsFunctions: true}
}
func (a *queueAdapter) HealthCheck(context.Context) error { return nil }
func (a *queueAdapter) Name() string                      { return "queue/test" }

func readStep() step { return step{calls: call("read_file", map[string]any{"path": "feature.txt"})} }
func writeStep(w string) step {
	return step{calls: call("write_file", map[string]any{"path": "feature.txt", "content": w + "\n"})}
}
func textStep(s string) step { return step{text: s} }

// detailRecorder is memRecorder plus MergeDetail (the store's detail
// merge), so detail.repair / detail.budget can be asserted.
type detailRecorder struct {
	memRecorder
	dmu    sync.Mutex
	detail map[string]json.RawMessage
}

func (d *detailRecorder) MergeDetail(_ context.Context, _ uuid.UUID, patch json.RawMessage) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(patch, &m); err != nil {
		return err
	}
	d.dmu.Lock()
	defer d.dmu.Unlock()
	if d.detail == nil {
		d.detail = map[string]json.RawMessage{}
	}
	for k, v := range m {
		d.detail[k] = v
	}
	return nil
}

func (d *detailRecorder) repair(t *testing.T) repairState {
	t.Helper()
	d.dmu.Lock()
	defer d.dmu.Unlock()
	var s repairState
	if err := json.Unmarshal(d.detail["repair"], &s); err != nil {
		t.Fatalf("detail.repair: %v (%s)", err, d.detail["repair"])
	}
	return s
}

func (d *detailRecorder) budgetState(t *testing.T) budgetState {
	t.Helper()
	d.dmu.Lock()
	defer d.dmu.Unlock()
	var s budgetState
	if err := json.Unmarshal(d.detail["budget"], &s); err != nil {
		t.Fatalf("detail.budget: %v (%s)", err, d.detail["budget"])
	}
	return s
}

func (d *detailRecorder) eventsOf(tool string) []controller.EventRow {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []controller.EventRow
	for _, r := range d.rows {
		if r.Tool == tool || (tool == "note" && r.Kind == controller.KindNote) {
			out = append(out, r)
		}
	}
	return out
}

// repairHarness is one attempt: a finalize fixture (agent branch, bare
// origin) whose base ships repairRunner, a controller run, a budget and
// the production executor and callback.
type repairHarness struct {
	f        *finalizeFixture
	workID   uuid.UUID
	taskID   uuid.UUID
	product  uuid.UUID
	rec      *detailRecorder
	run      *controller.Run
	budget   *attemptBudget
	executor *tools.Executor
	sink     *recordingSink
	model    *queueAdapter
	clock    *fakeClock
	calls    []strategy.ToolCall
	planned  []string
	// clonePath, when set, is the shared clone generation returns to its
	// base branch after the first commit; "" uses the worktree itself (the
	// worktree then ends up on main, R11 S3 tests set a separate one).
	clonePath string
	// evidenceStore overrides the fixture's recording store (integration).
	evidenceStore evidenceSink
}

// newRepairHarness builds the attempt; run nil records in memory
// (detailRecorder), otherwise the given (database) run is used.
func newRepairHarness(t *testing.T, model *queueAdapter, clock *fakeClock, run *controller.Run) *repairHarness {
	t.Helper()
	requireSh(t)
	f := newFinalizeFixture(t)
	gitRun(t, f.worktree, "checkout", "main")
	writeFile(t, f.worktree, "run_tests.sh", repairRunner)
	writeFile(t, f.worktree, "feature.txt", "start\n")
	gitRun(t, f.worktree, "add", "-A")
	gitRun(t, f.worktree, "commit", "-m", "base: scripted runner")
	gitRun(t, f.worktree, "push", "origin", "main")
	gitRun(t, f.worktree, "checkout", f.branch)
	gitRun(t, f.worktree, "reset", "--hard", "main")
	f.baseSHA = gitOutput(t, f.worktree, "rev-parse", "HEAD")

	h := &repairHarness{f: f, workID: uuid.New(), taskID: uuid.New(), product: uuid.New(), model: model, clock: clock, sink: &recordingSink{}, planned: []string{"feature.txt"}}
	if run == nil {
		h.rec = &detailRecorder{}
		run = controller.NewRun(h.rec, controller.Started{ID: uuid.New(), Attempt: 1, NextSeq: 2}, h.workID, h.taskID.String())
	} else {
		h.workID, h.taskID = run.WorkItemID, uuid.MustParse(run.TaskID)
	}
	h.run = run
	h.run.SetTestCommand(fixtureTestCommand)
	now := time.Now
	if clock != nil {
		now = clock.Now
	}
	h.budget = newAttemptBudget(now, h.run, logging.New("implementation-agent-test"), h.taskID.String())
	kb := &consoleBus{}
	h.executor = tools.NewExecutorWithCodex(f.worktree, nil, nil, &h.product, kb, &h.workID)
	h.executor.SetAskingAgent(agentID)
	h.executor.SetCallback(newToolCallback(context.Background(), kb, h.workID, h.taskID, h.run, h.budget, &h.calls))
	h.executor.SetIterationHook(h.budget.iterationHook)
	h.executor.SetPromptHook(h.budget.promptPending)
	return h
}

func (h *repairHarness) clone() string {
	if h.clonePath != "" {
		return h.clonePath
	}
	return h.f.worktree
}

func (h *repairHarness) task() implementationTaskMessage {
	return implementationTaskMessage{TaskID: h.taskID.String(), WorkID: h.workID.String(), ProductID: h.product.String(), Title: "make feature done", FilesToChange: h.planned}
}

// attempt runs generation and, when it stopped, finalizeStopped; it
// returns generation's result.
func (h *repairHarness) attempt(t *testing.T, pool *pgxpool.Pool, sink completionSink) generationResult {
	t.Helper()
	ctx := context.Background()
	logger := logging.New("implementation-agent-test")
	gen, ok := runGeneration(ctx, generationInputs{
		task: h.task(), taskID: h.taskID, workID: h.workID, clonePath: h.clone(), worktreePath: h.f.worktree,
		subtaskBranch: h.f.branch, baseBranch: "main", testCommand: fixtureTestCommand, baseSHA: h.f.baseSHA,
		req: adapter.CompletionRequest{
			Messages: []adapter.ChatMessage{{Role: adapter.RoleSystem, Content: "sys"}, {Role: adapter.RoleUser, Content: "make feature.txt say done"}},
			Tools:    tools.SafeImplementationTools(), ToolChoice: "auto",
		},
		systemPrompt: "sys", maxIters: 30, llmAdapter: h.model, executor: h.executor, pool: pool,
		turnToolCalls: &h.calls, logger: logger, budget: h.budget, run: h.run,
	}, sink)
	if gen.stopped != nil {
		if ok {
			t.Fatal("generation stopped but reported ok")
		}
		in := h.f.inputs()
		in.task, in.taskID = h.task(), h.taskID
		in.summary = gen.summary
		if h.evidenceStore != nil {
			in.evidenceStore = h.evidenceStore
		}
		finalizeStopped(ctx, in, *gen.stopped, stopContext{productID: &h.product, workID: h.workID, budget: h.budget, attempt: runAttempt(h.run)}, sink)
	}
	return gen
}

// requireStoppedOutcome asserts the FAILURE path of a stopped attempt:
// one failed implementation.complete with failed evidence (the gate's
// test check not_run with skipReason), the work item blocked with reason
// prefix, an escalation prompt listing wantFingerprints, the assignment
// failed, the work committed on the agent branch and never pushed.
func requireStoppedOutcome(t *testing.T, h *repairHarness, prefix, skipReason string, wantFingerprints []string, wantContent string) *evidence.ImplementationEvidence {
	t.Helper()
	s := h.sink
	if len(s.published) != 1 {
		t.Fatalf("published %d messages, want 1", len(s.published))
	}
	msg := s.published[0]
	if msg.Success || msg.Verified || !strings.HasPrefix(msg.Reason, prefix) {
		t.Fatalf("message success=%v verified=%v reason=%q, want failure with reason %q…", msg.Success, msg.Verified, msg.Reason, prefix)
	}
	if len(s.blocked) != 1 || !strings.HasPrefix(s.blocked[0], prefix) {
		t.Fatalf("blocks = %q, want one starting %q", s.blocked, prefix)
	}
	if len(s.statuses) != 1 || s.statuses[0] != models.AssignmentStatusFailed {
		t.Fatalf("assignment statuses = %v", s.statuses)
	}
	ev := msg.Evidence
	if ev == nil || ev.Outcome != evidence.OutcomeFailed || ev.Validate() != nil {
		t.Fatalf("evidence = %+v", ev)
	}
	if len(h.f.store.inserted) != 1 || h.f.store.inserted[0] != ev {
		t.Fatalf("evidence persisted %d times", len(h.f.store.inserted))
	}
	gate := ev.Checks[0]
	if gate.Kind != evidence.CheckKindTest || gate.Status != evidence.CheckNotRun || gate.SkipReason != skipReason || gate.Command != fixtureTestCommand {
		t.Fatalf("gate check = %+v, want test not_run %q", gate, skipReason)
	}
	head := gitOutput(t, h.f.worktree, "rev-parse", "HEAD")
	if ev.HeadSHA != head || head == h.f.baseSHA {
		t.Fatalf("evidence head %s, branch head %s, base %s: the work in progress must be committed", ev.HeadSHA, head, h.f.baseSHA)
	}
	if got := strings.TrimSpace(gitOutput(t, h.f.worktree, "show", "HEAD:feature.txt")); got != wantContent {
		t.Fatalf("committed feature.txt = %q, want %q", got, wantContent)
	}
	if pushed := h.f.remoteSHA(t); pushed != "" {
		t.Fatalf("stopped work was pushed (%s)", pushed)
	}
	// The work in progress is preserved under refs/ae/wip/ (a retry
	// recreates the agent branch), named in the evidence.
	ref := wipRefName(shortID(h.taskID), runAttempt(h.run))
	if got := gitOutput(t, h.f.worktree, "rev-parse", ref); got != head {
		t.Fatalf("%s = %s, want the WIP head %s", ref, got, head)
	}
	var named bool
	for _, r := range ev.ArtifactRefs {
		named = named || r == "git_ref:"+ref
	}
	if !named {
		t.Fatalf("evidence artifact_refs %v do not name %s", ev.ArtifactRefs, ref)
	}
	if len(s.prompts) != 1 {
		t.Fatalf("prompts = %d, want the escalation", len(s.prompts))
	}
	p := s.prompts[0]
	if p.PromptType != interactions.PromptTypeEscalation || p.ProductID != h.product || p.WorkItemID != h.workID {
		t.Fatalf("prompt = %+v", p)
	}
	if !strings.Contains(p.Question, "preserved at "+ref) || !strings.Contains(p.Question, "Retry starts a new attempt") {
		t.Fatalf("escalation does not name the preserved ref %s:\n%s", ref, p.Question)
	}
	for _, fp := range wantFingerprints {
		if !strings.Contains(p.Question, failures.Short(fp)) {
			t.Fatalf("escalation does not list fingerprint %s:\n%s", failures.Short(fp), p.Question)
		}
	}
	if len(wantFingerprints) == 0 && !strings.Contains(p.Question, "No failure records were gathered") {
		t.Fatalf("escalation question:\n%s", p.Question)
	}
	return ev
}

// (a) The same failure twice — once before and once after a repair —
// stops the loop deterministically after one repair, with retries left,
// and blocks.
func TestRepairIdenticalFailureTwiceStopsAndBlocks(t *testing.T) {
	before := repeatedFailureStops.Value()
	model := &queueAdapter{tokens: 100, steps: []step{
		readStep(), writeStep("alpha"), textStep("Implemented."),
		// repair 1: rewrites the same wrong value
		readStep(), writeStep("alpha"), textStep("Fixed."),
	}}
	h := newRepairHarness(t, model, nil, nil)
	gen := h.attempt(t, nil, h.sink)

	if gen.stopped == nil || gen.stopped.reason != stopRepeatedFailure {
		t.Fatalf("stopped = %+v, want repeated_failure", gen.stopped)
	}
	st := h.rec.repair(t)
	if st.Repairs != 1 || st.Runs != 2 || st.StopReason != stopRepeatedFailure {
		t.Fatalf("repair state = %+v, want 1 repair, 2 runs, repeated_failure", st)
	}
	if len(st.Failures) != 1 || st.Failures[0].Count != 2 || st.Failures[0].Test != "Test_alpha" || st.Failures[0].File != "feature_test.go" || st.Failures[0].Line != 7 {
		t.Fatalf("failures = %+v, want Test_alpha ×2 at feature_test.go:7", st.Failures)
	}
	if len(st.Attempts) != 1 || st.Attempts[0].Outcome != stopRepeatedFailure {
		t.Fatalf("attempts = %+v", st.Attempts)
	}
	if got := repeatedFailureStops.Value() - before; got != 1 {
		t.Fatalf("ae_repeated_failure_stops_total += %d, want 1", got)
	}
	// Each check run is a tool event with the records in its detail.
	evs := h.rec.eventsOf(preCommitTool)
	if len(evs) != 2 {
		t.Fatalf("pre-commit events = %d, want 2", len(evs))
	}
	var d struct {
		Records  []failures.Record `json:"records"`
		Repeated []string          `json:"repeated"`
		RawRef   string            `json:"raw_ref"`
	}
	if err := json.Unmarshal(evs[1].Detail, &d); err != nil || len(d.Records) != 1 || len(d.Repeated) != 1 || d.RawRef == "" {
		t.Fatalf("second check event detail = %s (%v)", evs[1].Detail, err)
	}
	if evs[1].Kind != controller.KindTool || evs[1].ExitCode == nil || *evs[1].ExitCode != 1 {
		t.Fatalf("check event = %+v", evs[1])
	}
	// The repair prompt carried the structured record and the raw ref,
	// not the raw output.
	var repairPrompt string
	for _, p := range model.prompts {
		if strings.HasPrefix(p, "Repair attempt 1") {
			repairPrompt = p
		}
	}
	var first struct {
		RawRef string `json:"raw_ref"`
	}
	_ = json.Unmarshal(evs[0].Detail, &first)
	if first.RawRef == "" || !strings.Contains(repairPrompt, "### feature_test.go") || !strings.Contains(repairPrompt, failures.Short(d.Records[0].Fingerprint)) || !strings.Contains(repairPrompt, first.RawRef) || strings.Contains(repairPrompt, "FAIL\texample.com") {
		t.Fatalf("repair prompt:\n%s", repairPrompt)
	}

	ev := requireStoppedOutcome(t, h, "repeated_failure: ", "repair stopped: repeated_failure", []string{d.Records[0].Fingerprint}, "alpha")
	// The last failing run describes the committed tree: recorded as failed.
	if len(ev.Checks) != 2 || ev.Checks[1].Status != evidence.CheckFailed || ev.Checks[1].OutputRef != d.RawRef {
		t.Fatalf("checks = %+v, want the gate's not_run and the failing pre-commit run", ev.Checks)
	}
	if !strings.Contains(h.sink.prompts[0].Question, "2 runs") {
		t.Fatalf("escalation lacks the count:\n%s", h.sink.prompts[0].Question)
	}
}

// (b) Different failures each time: up to 3 repairs, then repairs_exhausted.
func TestRepairDifferentFailuresUpToThreeRepairs(t *testing.T) {
	model := &queueAdapter{tokens: 100, steps: []step{
		readStep(), writeStep("alpha"), textStep("Implemented."),
		readStep(), writeStep("beta"), textStep("Fixed."),
		readStep(), writeStep("gamma"), textStep("Fixed."),
		readStep(), writeStep("delta"), textStep("Fixed."),
	}}
	h := newRepairHarness(t, model, nil, nil)
	gen := h.attempt(t, nil, h.sink)
	if gen.stopped == nil || gen.stopped.reason != stopRepairsExhausted {
		t.Fatalf("stopped = %+v, want repairs_exhausted", gen.stopped)
	}
	st := h.rec.repair(t)
	if st.Repairs != 3 || st.Runs != 4 {
		t.Fatalf("repairs=%d runs=%d, want 3 and 4", st.Repairs, st.Runs)
	}
	if len(st.Failures) != 4 {
		t.Fatalf("failures = %+v, want four distinct fingerprints", st.Failures)
	}
	for _, a := range st.Attempts {
		if a.Outcome != "changed" {
			t.Fatalf("attempt %+v, want outcome changed", a)
		}
	}
	var fps []string
	for _, f := range st.Failures {
		fps = append(fps, f.Fingerprint)
	}
	requireStoppedOutcome(t, h, "repairs_exhausted: ", "repair stopped: repairs_exhausted", fps, "delta")
}

// A repair that fixes the failure lets generation commit and continue.
func TestRepairFixesAndGenerationContinues(t *testing.T) {
	model := &queueAdapter{tokens: 100, steps: []step{
		readStep(), writeStep("alpha"), textStep("Implemented."),
		readStep(), writeStep("done"), textStep("Fixed."),
	}}
	h := newRepairHarness(t, model, nil, nil)
	before := repairAttemptsFixed.Value()
	gen := h.attempt(t, nil, h.sink)
	if gen.stopped != nil || gen.repair == nil || !gen.repair.passed || gen.repair.state.Repairs != 1 {
		t.Fatalf("gen = %+v", gen)
	}
	if repairAttemptsFixed.Value()-before != 1 {
		t.Fatal("ae_repair_attempts_total{outcome=fixed} not incremented")
	}
	if len(h.sink.published) != 0 {
		t.Fatalf("generation reported %+v on success", h.sink.published)
	}
	f := repairSummaryFields(gen.repair, nil)
	if f["repair_stop_reason"] != stopPassed || f["repair_attempts"] != "1" || !strings.Contains(f["failure_fingerprints"], "Test_alpha") {
		t.Fatalf("summary fields = %+v", f)
	}
}

// (c) The token budget runs out inside the repair loop: the loop stops
// before paying for another call, and the attempt ends on the failure
// path with evidence (gate not_run "budget exhausted", a failed budget
// check) and a block whose escalation lists the gathered record.
func TestTokenBudgetExhaustedMidRepairBlocksWithEvidence(t *testing.T) {
	t.Setenv("WORK_ITEM_TOKEN_BUDGET", "1000")
	model := &queueAdapter{tokens: 200, steps: []step{
		readStep(), writeStep("alpha"), textStep("Implemented."),
		readStep(), writeStep("beta"), readStep(), writeStep("gamma"), readStep(), writeStep("delta"),
	}}
	h := newRepairHarness(t, model, nil, nil)
	gen := h.attempt(t, nil, h.sink)
	if gen.stopped == nil || gen.stopped.budget == nil || gen.stopped.budget.kind != budgetKindTokens {
		t.Fatalf("stopped = %+v, want a token budget stop", gen.stopped)
	}
	st := h.rec.repair(t)
	if st.StopReason != stopBudgetExhausted || st.Repairs != 1 || len(st.Attempts) != 1 || st.Attempts[0].Outcome != stopBudgetExhausted {
		t.Fatalf("repair state = %+v", st)
	}
	b := h.rec.budgetState(t)
	if b.Exhausted != budgetKindTokens || b.TokensUsed < b.TokenLimit || b.TokenLimit != 1000 {
		t.Fatalf("budget = %+v", b)
	}
	alpha := st.Failures[0].Fingerprint
	ev := requireStoppedOutcome(t, h, "budget_exhausted: tokens", budgetSkipReason, []string{alpha}, "beta")
	// The repair changed the tree after the failing run: that run is not
	// evidence for the committed head; the budget check is.
	if len(ev.Checks) != 2 || ev.Checks[1].Kind != evidence.CheckKindOther || ev.Checks[1].Status != evidence.CheckFailed || ev.Checks[1].Command != budgetCheckCommand+budgetKindTokens {
		t.Fatalf("checks = %+v", ev.Checks)
	}
	if !strings.Contains(h.sink.prompts[0].Question, "Budget: tokens") || !strings.Contains(h.sink.prompts[0].Question, "raise WORK_ITEM_TOKEN_BUDGET before Retry") {
		t.Fatalf("escalation lacks the budget:\n%s", h.sink.prompts[0].Question)
	}
}

// RunToolLoop's own token check (WORK_ITEM_TOKEN_BUDGET) in the main tool
// loop also takes the stop path: blocked with evidence, never a bare
// "tool loop error".
func TestTokenBudgetInMainLoopTakesTheStopPath(t *testing.T) {
	t.Setenv("WORK_ITEM_TOKEN_BUDGET", "500")
	model := &queueAdapter{tokens: 600, steps: []step{readStep(), writeStep("alpha"), textStep("Implemented.")}}
	h := newRepairHarness(t, model, nil, nil)
	h.executor.SetIterationHook(nil) // only RunToolLoop's own check
	gen := h.attempt(t, nil, h.sink)
	if gen.stopped == nil || gen.stopped.budget == nil || gen.stopped.budget.kind != budgetKindTokens || !strings.Contains(gen.stopped.budget.detail, "token budget exceeded") {
		t.Fatalf("stopped = %+v", gen.stopped)
	}
	if len(h.sink.blocked) != 1 || !strings.HasPrefix(h.sink.blocked[0], "budget_exhausted: tokens") {
		t.Fatalf("blocked = %q", h.sink.blocked)
	}
	if !strings.Contains(h.sink.prompts[0].Question, "No failure records were gathered") {
		t.Fatalf("question:\n%s", h.sink.prompts[0].Question)
	}
}

// (d) Active time: 30 active minutes, then a pending user prompt for three
// hours, then the repair loop. The paused interval is not counted: the
// attempt is exhausted only once 60 ACTIVE minutes have passed, inside the
// repair loop, not at the first check after the wait.
func TestActiveTimeExcludesPendingPromptAndExhausts(t *testing.T) {
	t.Setenv(activeMinutesEnv, "60")
	clock := newFakeClock()
	model := &queueAdapter{tokens: 10, clock: clock, perCall: 10 * time.Minute, steps: []step{
		readStep(), writeStep("alpha"),
		{calls: call("request_user_input", map[string]any{"headline": "Which value?", "question": "Should feature.txt say done?", "prompt_type": "clarification"})},
		// repair loop: three calls of ten minutes each
		readStep(), writeStep("beta"), readStep(), writeStep("gamma"),
	}}
	h := newRepairHarness(t, model, clock, nil)
	// The answer takes three hours to arrive.
	h.executor.SetPromptHook(func(id uuid.UUID, trig interactions.Trigger) {
		h.budget.promptPending(id, trig)
		clock.Advance(3 * time.Hour)
	})
	gen := h.attempt(t, nil, h.sink)
	if gen.stopped == nil || gen.stopped.budget == nil || gen.stopped.budget.kind != budgetKindActiveTime {
		t.Fatalf("stopped = %+v, want an active-time stop", gen.stopped)
	}
	b := h.rec.budgetState(t)
	if b.Exhausted != budgetKindActiveTime || b.ActiveSeconds != 3600 || b.PausedSeconds != 3*3600 {
		t.Fatalf("budget = %+v, want 60m active with 3h paused excluded", b)
	}
	if len(b.Pauses) != 1 || b.Pauses[0].Seconds != 3*3600 || b.Pauses[0].Trigger != string(interactions.TriggerAgentRequest) {
		t.Fatalf("pauses = %+v", b.Pauses)
	}
	st := h.rec.repair(t)
	// The first check run after the wait ran (the wait did not count), and
	// the repair it started ran out of active time.
	if st.Runs != 1 || st.Repairs != 1 || st.StopReason != stopBudgetExhausted {
		t.Fatalf("repair state = %+v", st)
	}
	if !strings.Contains(gen.stopped.budget.detail, "1h0m0s of 1h0m0s active (3h0m0s awaiting user input excluded)") {
		t.Fatalf("detail = %q", gen.stopped.budget.detail)
	}
	requireStoppedOutcome(t, h, "budget_exhausted: active_time", budgetSkipReason, []string{st.Failures[0].Fingerprint}, "beta")
}

// A failure owned by the environment contradicts the plan: one tool-less
// plan revision is recorded as a note linked to the record's fingerprint
// and the check run's call id, and the repair prompt carries it.
func TestPlanRevisionLinkedToContradictingEvidence(t *testing.T) {
	model := &queueAdapter{tokens: 10, steps: []step{
		readStep(), writeStep("alpha"), textStep("Implemented."),
		textStep("Sure.\nrevised plan: install the missing tool in the image before running the tests"),
		readStep(), writeStep("alpha"), textStep("Done."),
	}}
	h := newRepairHarness(t, model, nil, nil)
	// A third-party package is missing (dependency, not repairable) and a
	// test fails (repairable): revise the plan, then repair.
	writeFile(t, h.f.worktree, "run_tests.sh", "printf 'cmd/app/main.go:3:8: no required module provides package github.com/third/party; to add it:\\n--- FAIL: TestFeature (0.00s)\\n    feature_test.go:7: got alpha, want done\\nFAIL\\n'\nexit 1\n")
	gitRun(t, h.f.worktree, "add", "-A")
	gitRun(t, h.f.worktree, "commit", "-m", "runner needs a missing package")
	h.f.baseSHA = gitOutput(t, h.f.worktree, "rev-parse", "HEAD")
	gen := h.attempt(t, nil, h.sink)
	if gen.stopped == nil || gen.stopped.reason != stopRepeatedFailure {
		t.Fatalf("stopped = %+v", gen.stopped)
	}
	st := h.rec.repair(t)
	if len(st.Revisions) != 1 {
		t.Fatalf("revisions = %+v", st.Revisions)
	}
	rev := st.Revisions[0]
	var dep string
	for _, f := range st.Failures {
		if f.Ownership == "dependency" {
			dep = f.Fingerprint
		}
	}
	if rev.Revision != "revised plan: install the missing tool in the image before running the tests" || dep == "" || rev.Fingerprint != dep ||
		rev.EvidenceCallID != st.CheckRuns[0].CallID || rev.EvidenceSeq != st.CheckRuns[0].Seq || !strings.Contains(rev.Reason, "dependency") {
		t.Fatalf("revision = %+v (check run %+v)", rev, st.CheckRuns[0])
	}
	notes := h.rec.eventsOf("note")
	var found bool
	for _, n := range notes {
		if strings.HasPrefix(n.Note, "plan_revision: revised plan:") && n.ToolCallID == rev.EvidenceCallID && len(n.Detail) > 0 {
			found = true
		}
	}
	if !found {
		t.Fatalf("no plan_revision note linked to %s: %+v", rev.EvidenceCallID, notes)
	}
	var repairPrompt string
	for _, p := range model.prompts {
		if strings.HasPrefix(p, "Repair attempt 1") {
			repairPrompt = p
		}
	}
	if !strings.Contains(repairPrompt, rev.Revision) || !strings.Contains(repairPrompt, "not fixable by editing the change") {
		t.Fatalf("repair prompt:\n%s", repairPrompt)
	}
}

// When every record is owned by a dependency or the environment, the
// loop stops right after the plan revision: no repair is spent on it.
func TestRepairStopsWhenNothingIsRepairable(t *testing.T) {
	model := &queueAdapter{tokens: 10, steps: []step{
		readStep(), writeStep("alpha"), textStep("Implemented."),
		textStep("revised plan: install frobtest in the image first"),
	}}
	h := newRepairHarness(t, model, nil, nil)
	writeFile(t, h.f.worktree, "run_tests.sh", "echo 'sh: 1: frobtest: not found' >&2\nexit 127\n")
	gitRun(t, h.f.worktree, "add", "-A")
	gitRun(t, h.f.worktree, "commit", "-m", "runner needs a missing tool")
	h.f.baseSHA = gitOutput(t, h.f.worktree, "rev-parse", "HEAD")
	gen := h.attempt(t, nil, h.sink)
	if gen.stopped == nil || gen.stopped.reason != stopNotRepairable {
		t.Fatalf("stopped = %+v", gen.stopped)
	}
	st := h.rec.repair(t)
	if st.Repairs != 0 || st.Runs != 1 || len(st.Revisions) != 1 || !strings.Contains(st.Revisions[0].Reason, "environment") {
		t.Fatalf("repair state = %+v", st)
	}
	for _, p := range model.prompts {
		if strings.HasPrefix(p, "Repair attempt") {
			t.Fatalf("a repair was spent on a non-repairable failure:\n%s", p)
		}
	}
	requireStoppedOutcome(t, h, "not_repairable: ", "repair stopped: not_repairable", []string{st.Failures[0].Fingerprint}, "alpha")
}

// A retry recreates the agent branch (createIsolatedWorktree deletes it);
// the stopped attempt's preserved ref and its WIP commit survive, and a
// local-only branch left without one is preserved before the delete.
func TestRetryPreservesStoppedWork(t *testing.T) {
	model := &queueAdapter{tokens: 100, steps: []step{
		readStep(), writeStep("alpha"), textStep("Implemented."),
		readStep(), writeStep("alpha"), textStep("Fixed."),
	}}
	h := newRepairHarness(t, model, nil, nil)
	h.attempt(t, nil, h.sink)
	ref := wipRefName(shortID(h.taskID), runAttempt(h.run))
	wip := gitOutput(t, h.f.worktree, "rev-parse", "HEAD")
	if got := gitOutput(t, h.f.worktree, "rev-parse", ref); got != wip {
		t.Fatalf("%s = %s, want %s", ref, got, wip)
	}
	// The worktree is gone; the next attempt recreates the branch.
	gitRun(t, h.f.worktree, "checkout", "-q", "main")
	next := filepath.Join(t.TempDir(), "worktree-retry")
	ctx := withWIPRef(context.Background(), wipRefName(shortID(h.taskID), runAttempt(h.run)))
	if err := createIsolatedWorktree(ctx, h.f.worktree, next, h.f.branch, "main"); err != nil {
		t.Fatalf("retry worktree: %v", err)
	}
	if got := gitOutput(t, h.f.worktree, "rev-parse", "refs/heads/"+h.f.branch); got != gitOutput(t, h.f.worktree, "rev-parse", "main") {
		t.Fatalf("the retry's branch is at %s, want the base", got)
	}
	if got := gitOutput(t, h.f.worktree, "rev-parse", ref); got != wip {
		t.Fatalf("after the retry %s = %s, want the WIP commit %s", ref, got, wip)
	}
	if got := strings.TrimSpace(gitOutput(t, h.f.worktree, "show", ref+":feature.txt")); got != "alpha" {
		t.Fatalf("preserved commit content = %q", got)
	}

	// A local-only branch with no preserved ref is kept before the delete.
	gitRun(t, next, "-c", "user.email=t@e", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "local only")
	local := gitOutput(t, next, "rev-parse", "HEAD")
	gitRun(t, h.f.worktree, "worktree", "remove", "--force", next)
	again := filepath.Join(t.TempDir(), "worktree-again")
	if err := createIsolatedWorktree(context.Background(), h.f.worktree, again, h.f.branch, "main"); err != nil {
		t.Fatalf("second retry: %v", err)
	}
	derived := wipRefFrom(context.Background(), h.f.branch)
	if got := gitOutput(t, h.f.worktree, "rev-parse", derived); got != local {
		t.Fatalf("%s = %s, want the local-only commit %s", derived, got, local)
	}

	// A branch whose tip is on a remote is not copied.
	if ref, err := preserveLocalBranch(context.Background(), h.f.worktree, "main", wipRefPrefix+"main/x"); err != nil || ref != "" {
		t.Fatalf("pushed main preserved at %q (%v)", ref, err)
	}
}

// The active-time pause ends on the next tool execution (production never
// waits in-process), except for the request_user_input call that raised it.
func TestActivePauseEndsOnNextToolExecution(t *testing.T) {
	clock := newFakeClock()
	b := newAttemptBudget(clock.Now, nil, nil, "t")
	var calls []strategy.ToolCall
	cb := newToolCallback(context.Background(), &consoleBus{}, uuid.New(), uuid.New(), nil, b, &calls)
	b.promptPending(uuid.New(), interactions.TriggerAgentRequest)
	clock.Advance(10 * time.Minute)
	cb("c1", "request_user_input", `{}`, "ok", false, tools.ResultMeta{})
	clock.Advance(5 * time.Minute)
	cb("c2", "read_file", `{"path":"x"}`, "ok", false, tools.ResultMeta{})
	clock.Advance(20 * time.Minute)
	s := b.snapshot()
	if s.Paused || s.PausedSeconds != 15*60 || s.ActiveSeconds != 20*60 {
		t.Fatalf("budget = %+v, want 15m paused (until the read_file) and 20m active", s)
	}
}

// The repair loop gives the model only the safe tool set: no git or PR
// tools (the old loop leaked ImplementationTools).
func TestRepairUsesSafeTools(t *testing.T) {
	rec := &toolRecordingAdapter{}
	in := repairInputs{
		runChecks: func(context.Context) ([]byte, error) {
			return []byte("--- FAIL: TestX (0.00s)\n    x_test.go:3: boom\nFAIL\n"), errors.New("exit status 1")
		},
		llm: rec, executor: tools.NewExecutor(t.TempDir()), command: "go test ./...",
	}
	rr := runRepairLoop(context.Background(), in)
	if rr.stop != stopRepeatedFailure || rr.state.Repairs != 1 {
		t.Fatalf("result = %s repairs=%d", rr.stop, rr.state.Repairs)
	}
	for _, name := range rec.tools {
		switch name {
		case "git_commit", "git_push", "create_branch", "commit_and_push", "create_pull_request":
			t.Fatalf("repair offered %s", name)
		}
	}
	if len(rec.tools) == 0 {
		t.Fatal("repair offered no tools")
	}
}

type toolRecordingAdapter struct {
	queueAdapter
	tools []string
}

func (a *toolRecordingAdapter) Complete(ctx context.Context, req adapter.CompletionRequest) (*adapter.CompletionResponse, error) {
	if len(req.Tools) > 0 && a.tools == nil {
		for _, t := range req.Tools {
			a.tools = append(a.tools, t.Name)
		}
	}
	return a.queueAdapter.Complete(ctx, req)
}
