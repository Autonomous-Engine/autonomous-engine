package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/verifier"
	"github.com/autonomousengine/autonomous-engine/internal/verifier/stage"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/segmentio/kafka-go"
)

// TestE2EEvidenceOutcomes is the end-to-end evidence fixture: for
// every implementation-evidence outcome it publishes a synthetic
// implementation.complete message to the real Kafka and asserts what the
// running status-orchestrator, pr-coordinator and dashboard-backend wrote to
// PostgreSQL (work-item states and transition reasons, prompts, pending PR
// rows), what they published (pr.gated, qa.ready — observed directly on the
// topics) and what the consumers' legacy-fallback counters did.
//
// Required environment (the test skips without both):
//
//	TEST_POSTGRES_DSN    the database the services under test read
//	TEST_KAFKA_BROKERS   comma-separated brokers, e.g. kafka:9092
//
// Optional:
//
//	E2E_COMPAT_WINDOW          legacy (default) | strict — the
//	                           EVIDENCE_COMPAT_WINDOW the consumers were
//	                           started with; selects the applicable cases
//	E2E_HUMAN_REVIEW_REQUIRED  true (default) | false — status-orchestrator's
//	                           HUMAN_REVIEW_REQUIRED; selects the expected
//	                           post-review destination of an ungated parent
//	E2E_METRICS_URLS           comma-separated /metrics URLs of the consumers
//	                           whose ae_evidence_compat_legacy_fallback_total
//	                           is asserted (default: status-orchestrator,
//	                           pr-coordinator and implementation-planner on
//	                           the compose network)
//
//	E2E_REVIEW_SECOND_PASS     true (default) | false — review-agent's
//	                           REVIEW_LLM_SECOND_PASS; a verifier approval
//	                           is approve_pending_second_pass when true
//
// R14: the verification stage (review-agent's second consumer group on
// implementation.complete) runs live and is observed on
// verification.complete. A real verifier verdict needs a clone and a model,
// so for the approve and escalate cases the fixture first records the
// verdict the stage would have committed for the evidence's head; the live
// stage reuses it (a verdict is per head). One case lets the stage run for
// real on a repository without a clone: a structured failure that blocks
// the parent. No case calls a model.
//
// Not exercised here: review-agent's pr.ready review, and qa-agent beyond
// its refusal.
// pr-coordinator refuses to publish pr.ready without a real GitHub PR URL and
// review-agent refuses a pr.ready without one, so the fixture routes PRs
// through pr_gate_mode=manual and observes the pending_pr_changes row and the
// pr.gated message (with the verified head SHA and the partial-verification
// marker) that a human would approve instead. The review step is driven with
// a synthetic review.complete and the merge with a synthetic pr.merged.
func TestE2EEvidenceOutcomes(t *testing.T) {
	s := connect(t)
	t.Logf("services window: %s, HUMAN_REVIEW_REQUIRED=%t", s.window, s.humanReviewRequired)

	for _, c := range outcomeCases() {
		if !c.runsIn(s.window) {
			continue
		}
		c := c
		t.Run(c.name, func(t *testing.T) { s.runCase(t, c) })
	}
}

// fixturePrefix names every row the fixture creates so leftovers from a
// crashed run can be found and swept. Do not shorten it: the sweep is a LIKE
// on this prefix.
const fixturePrefix = "e2e-evidence-fixture"

const (
	stateImplementation = "implementation"
	stateReview         = "review"
	stateHumanReview    = "human_review"
	stateQA             = "qa"
	statePassed         = "passed"
	stateBlocked        = "blocked"

	triggerBlocked     = "blocked"
	triggerHumanReview = "approval:human_review"
	triggerPRGate      = "approval:pr_gate"

	// partialVerificationMarker mirrors the load-bearing constant in
	// cmd/status-orchestrator/evidence_decision.go; the parent gate is read
	// from it.
	partialVerificationMarker = "partially verified: mandatory human review required before QA"
	// partialTitlePrefix and partialBodyMarker mirror cmd/pr-coordinator.
	partialTitlePrefix = "[PARTIALLY VERIFIED — human review required] "
	partialBodyMarker  = "PARTIALLY VERIFIED — human review required"
	// legacyLabel is pr-coordinator's outcome label for a pre-evidence
	// message accepted in the legacy window.
	legacyLabel = "completed (legacy message, no evidence)"
	// gateReasonFragment is the parent-level gate text status-orchestrator
	// records when a subtask completed on partially verified evidence.
	gateReasonFragment = "completed on partially verified evidence and require mandatory human review before QA"
	// reviewApprovedHumanReview / reviewApprovedAutoMerge are the ungated
	// post-review reasons for HUMAN_REVIEW_REQUIRED true / false.
	reviewApprovedHumanReview = "review approved, awaiting human review"
	reviewApprovedAutoMerge   = "review approved, auto-merge allowed"
	// qaRefusedFragment is qa-agent's block reason for a fixture repository
	// (no clone); tolerated after a qa.ready.
	qaRefusedFragment = "Cannot run QA: no repository available"

	legacyFallbackCounter = "ae_evidence_compat_legacy_fallback_total"
)

// stack is the live stack under test.
type stack struct {
	db                  *pgxpool.Pool
	kb                  *bus.KafkaBus
	evidence            *evidence.Store
	obs                 *observer
	window              evidence.CompatWindow
	humanReviewRequired bool
	// secondPass is review-agent's REVIEW_LLM_SECOND_PASS (R14): a
	// verifier approval is approve_pending_second_pass when it is on.
	secondPass  bool
	metricsURLs []string
}

func connect(t *testing.T) *stack {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	brokerEnv := os.Getenv("TEST_KAFKA_BROKERS")
	if dsn == "" || brokerEnv == "" {
		t.Skip("TEST_POSTGRES_DSN and TEST_KAFKA_BROKERS must both be set — skipping end-to-end evidence fixture")
	}
	window, ok := evidence.ParseCompatWindow(os.Getenv("E2E_COMPAT_WINDOW"))
	if !ok {
		t.Fatalf("E2E_COMPAT_WINDOW=%q is not legacy|strict", os.Getenv("E2E_COMPAT_WINDOW"))
	}
	hrr := true
	if v := os.Getenv("E2E_HUMAN_REVIEW_REQUIRED"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			t.Fatalf("E2E_HUMAN_REVIEW_REQUIRED=%q is not a bool", v)
		}
		hrr = b
	}
	secondPass := true
	if v := os.Getenv("E2E_REVIEW_SECOND_PASS"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			t.Fatalf("E2E_REVIEW_SECOND_PASS=%q is not a bool", v)
		}
		secondPass = b
	}
	metricsURLs := []string{
		"http://status-orchestrator:8090/metrics",
		"http://pr-coordinator:8096/metrics",
		"http://implementation-planner:8094/metrics",
	}
	if v := os.Getenv("E2E_METRICS_URLS"); v != "" {
		metricsURLs = strings.Split(v, ",")
	}
	brokers := strings.Split(brokerEnv, ",")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	if err := db.Ping(ctx); err != nil {
		t.Fatalf("ping %s: %v", dsn, err)
	}
	kb, err := bus.NewKafkaBus(brokers)
	if err != nil {
		db.Close()
		t.Fatalf("kafka %s: %v", brokerEnv, err)
	}
	s := &stack{
		db:                  db,
		kb:                  kb,
		evidence:            evidence.NewStore(db),
		window:              window,
		humanReviewRequired: hrr,
		secondPass:          secondPass,
		metricsURLs:         metricsURLs,
	}
	s.sweep(t)
	s.obs = startObserver(t, brokers, bus.TopicPRGated, bus.TopicQAReady, bus.TopicVerificationComplete)
	t.Cleanup(func() {
		s.obs.stop()
		s.sweep(t)
		_ = kb.Close()
		db.Close()
	})
	return s
}

// sweep deletes every row named with fixturePrefix, whether from this run or
// a crashed earlier one. Work items go first (their product_id and
// agent_definition_id are ON DELETE SET NULL, so deleting the parents alone
// would orphan them); the product delete cascades to repositories and
// user_interactions; the work item delete cascades to transitions, prompts,
// pending PR rows, assignments and stored evidence.
func (s *stack) sweep(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	like := fixturePrefix + "%"
	if _, err := s.db.Exec(ctx, `UPDATE work_items SET awaiting_prompt_id = NULL WHERE title LIKE $1`, like); err != nil {
		t.Errorf("sweep: clear awaiting_prompt_id: %v", err)
	}
	// Children the services created under a fixture parent (R16: the
	// verifier's fix subtask) carry no prefix; parent_id is ON DELETE SET
	// NULL, so they go first or they would be orphaned.
	if _, err := s.db.Exec(ctx, `UPDATE work_items SET awaiting_prompt_id = NULL WHERE parent_id IN (SELECT id FROM work_items WHERE title LIKE $1)`, like); err != nil {
		t.Errorf("sweep: clear children's awaiting_prompt_id: %v", err)
	}
	if _, err := s.db.Exec(ctx, `DELETE FROM work_items WHERE parent_id IN (SELECT id FROM work_items WHERE title LIKE $1) AND title NOT LIKE $1`, like); err != nil {
		t.Errorf("sweep: delete service-created children: %v", err)
	}
	items, err := s.db.Exec(ctx, `DELETE FROM work_items WHERE title LIKE $1`, like)
	if err != nil {
		t.Errorf("sweep: delete work items: %v", err)
	}
	products, err := s.db.Exec(ctx, `DELETE FROM products WHERE name LIKE $1`, like)
	if err != nil {
		t.Errorf("sweep: delete products: %v", err)
	}
	defs, err := s.db.Exec(ctx, `DELETE FROM agent_definitions WHERE name LIKE $1`, like)
	if err != nil {
		t.Errorf("sweep: delete agent definitions: %v", err)
	}
	if n := items.RowsAffected() + products.RowsAffected() + defs.RowsAffected(); n > 0 {
		t.Logf("sweep: removed %d fixture work item(s), %d fixture product(s), %d fixture agent definition(s)", items.RowsAffected(), products.RowsAffected(), defs.RowsAffected())
	}
}

// fixture is one prepared parent + subtask pair under its own product,
// repository and agent definition, mirroring what the planner leaves behind
// when a subtask enters implementation.
type fixture struct {
	name      string
	productID uuid.UUID
	repoID    uuid.UUID
	defID     uuid.UUID
	parentID  uuid.UUID
	subtaskID uuid.UUID
	baseSHA   string
	// headSHA is the SHA the evidence record was verified against.
	headSHA string
	// messageSHA is what the message's own head_sha field claims. It is
	// deliberately different from headSHA so an assertion on a consumer's
	// SHA can tell which one it copied.
	messageSHA string
	branch     string
}

// fixtureSeq numbers fixtures within a run so every case gets a distinct,
// recognisable pair of SHAs.
var fixtureSeq int

func (s *stack) newFixture(t *testing.T, name string, optIn bool) *fixture {
	t.Helper()
	fixtureSeq++
	suffix := fmt.Sprintf("%08x", fixtureSeq)
	f := &fixture{
		name:       name,
		productID:  uuid.New(),
		repoID:     uuid.New(),
		defID:      uuid.New(),
		parentID:   uuid.New(),
		subtaskID:  uuid.New(),
		baseSHA:    strings.Repeat("a", 40),
		headSHA:    strings.Repeat("b", 32) + suffix,
		messageSHA: strings.Repeat("c", 32) + suffix,
	}
	// pr-coordinator names the branch agent/<work_id>/<task_id[:8]>.
	f.branch = fmt.Sprintf("agent/%s/%s", f.parentID, f.subtaskID.String()[:8])

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	short := f.productID.String()[:8]
	mustExec(t, s.db, ctx, `INSERT INTO products (id, name, partial_verification_opt_in) VALUES ($1, $2, $3)`,
		f.productID, fmt.Sprintf("%s-%s-%s", fixturePrefix, name, short), optIn)
	mustExec(t, s.db, ctx, `INSERT INTO repositories (id, product_id, name, github_url, base_branch) VALUES ($1, $2, $3, $4, 'main')`,
		f.repoID, f.productID, fixturePrefix+"-repo", "https://github.com/example/"+fixturePrefix+"-"+short)
	// An agent definition with the human-review step disabled: the parent's
	// next step after review is then qa, so only the D2/D3 gate can divert
	// it to human_review. With the default (all steps enabled) definition
	// every parent goes to human_review after review and the gate's
	// destination would be indistinguishable from the ordinary path.
	// enabled=false: loadDefForWorkItem resolves the definition by id
	// (Registry.Get ignores enabled), while Registry.ResolveForWorkItem
	// only routes real work items to enabled definitions, so a fixture
	// definition can never be picked for a real item during the run.
	mustExec(t, s.db, ctx, `INSERT INTO agent_definitions (id, name, display_name, description, step_human_review, enabled) VALUES ($1, $2, $3, 'end-to-end evidence fixture: human-review step disabled', false, false)`,
		f.defID, fmt.Sprintf("%s-def-%s", fixturePrefix, short), fixturePrefix+" definition")
	// pr_gate_mode=manual: the PR is recorded in pending_pr_changes for a
	// human instead of being opened on GitHub.
	mustExec(t, s.db, ctx, `
		INSERT INTO work_items (id, type, state, title, description, priority, product_id, repository_id, agent_definition_id, pr_gate_mode)
		VALUES ($1, 'feature', 'implementation', $2, 'end-to-end evidence fixture parent', 'medium', $3, $4, $5, 'manual')`,
		f.parentID, fmt.Sprintf("%s parent %s", fixturePrefix, name), f.productID, f.repoID, f.defID)
	mustExec(t, s.db, ctx, `
		INSERT INTO work_items (id, type, state, title, description, priority, parent_id, product_id, repository_id)
		VALUES ($1, 'subtask', 'implementation', $2, 'end-to-end evidence fixture subtask', 'medium', $3, $4, $5)`,
		f.subtaskID, fmt.Sprintf("%s subtask %s", fixturePrefix, name), f.parentID, f.productID, f.repoID)
	t.Logf("fixture %s: product=%s (opt_in=%t) repo=%s def=%s parent=%s subtask=%s evidence_sha=%s message_sha=%s",
		name, f.productID, optIn, f.repoID, f.defID, f.parentID, f.subtaskID, f.headSHA, f.messageSHA)
	return f
}

func mustExec(t *testing.T, db *pgxpool.Pool, ctx context.Context, sql string, args ...any) {
	t.Helper()
	if _, err := db.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("exec %q: %v", firstLine(sql), err)
	}
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

// ─── evidence records ────────────────────────────────────────────────────────

// verifiedEvidence is a valid schema-1 record: one passed test check against
// the fixture's head SHA, with the repository resolved.
func verifiedEvidence(f *fixture) evidence.ImplementationEvidence {
	now := time.Now().UTC().Truncate(time.Second)
	exit := 0
	repo := f.repoID.String()
	return evidence.ImplementationEvidence{
		SchemaVersion: evidence.SchemaVersion,
		WorkID:        f.parentID.String(),
		TaskID:        f.subtaskID.String(),
		RepositoryID:  &repo,
		BaseSHA:       f.baseSHA,
		HeadSHA:       f.headSHA,
		Branch:        f.branch,
		ChangedFiles:  []string{"internal/example/example.go", "internal/example/example_test.go"},
		Checks: []evidence.CheckResult{{
			Kind:       evidence.CheckKindTest,
			Command:    "go test ./...",
			ExitCode:   &exit,
			Status:     evidence.CheckPassed,
			StartedAt:  &now,
			FinishedAt: &now,
			OutputRef:  "artifact://" + fixturePrefix + "/go-test.log",
		}},
		Outcome:      evidence.OutcomeVerified,
		StartedAt:    now,
		FinishedAt:   now.Add(time.Minute),
		ArtifactRefs: []string{"artifact://" + fixturePrefix + "/diff.patch"},
	}
}

func partiallyVerifiedEvidence(f *fixture) evidence.ImplementationEvidence {
	ev := verifiedEvidence(f)
	ev.Checks = append(ev.Checks, evidence.CheckResult{
		Kind:       evidence.CheckKindLint,
		Command:    "golangci-lint run ./...",
		Status:     evidence.CheckSkipped,
		SkipReason: "no linter configured in repository",
	})
	ev.Outcome = evidence.OutcomePartiallyVerified
	return ev
}

func failedEvidence(f *fixture) evidence.ImplementationEvidence {
	ev := verifiedEvidence(f)
	exit := 1
	ev.Checks[0].ExitCode = &exit
	ev.Checks[0].Status = evidence.CheckFailed
	ev.Outcome = evidence.OutcomeFailed
	ev.Reason = "go test ./... exited 1: TestExample failed"
	return ev
}

// unknownSchemaEvidence is a verified record from a future producer: only
// its schema_version is wrong, so a consumer that read "outcome" without
// checking the version would release it.
func unknownSchemaEvidence(f *fixture) evidence.ImplementationEvidence {
	ev := verifiedEvidence(f)
	ev.SchemaVersion = evidence.SchemaVersion + 1
	return ev
}

// message is the implementation.complete body. It mirrors the fields the
// implementation agent emits (cmd/implementation-agent); evidence is the
// additive v1 record and is omitted for a pre-evidence producer.
type message struct {
	TaskID       string          `json:"task_id"`
	WorkID       string          `json:"work_id"`
	FilesChanged []string        `json:"files_changed"`
	TestResults  string          `json:"test_results"`
	Summary      string          `json:"summary,omitempty"`
	Success      bool            `json:"success"`
	Reason       string          `json:"reason,omitempty"`
	HeadSHA      string          `json:"head_sha,omitempty"`
	BaseSHA      string          `json:"base_sha,omitempty"`
	Branch       string          `json:"branch,omitempty"`
	Evidence     json.RawMessage `json:"evidence,omitempty"`
}

// withEvidence wraps ev in a message whose own head_sha is the fixture's
// messageSHA, not the record's. A consumer that copies the message field
// instead of the evidence field is then caught by every SHA assertion.
func withEvidence(t *testing.T, f *fixture, ev evidence.ImplementationEvidence, success bool) message {
	t.Helper()
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal evidence: %v", err)
	}
	return message{
		TaskID:       f.subtaskID.String(),
		WorkID:       f.parentID.String(),
		FilesChanged: ev.ChangedFiles,
		TestResults:  "see evidence",
		Summary:      fixturePrefix + " " + f.name,
		Success:      success,
		Reason:       ev.Reason,
		HeadSHA:      f.messageSHA,
		BaseSHA:      ev.BaseSHA,
		Branch:       ev.Branch,
		Evidence:     raw,
	}
}

// legacyMessage is what a pre-evidence producer sends: no evidence field at
// all, only the success boolean and the planner-sourced file list.
func legacyMessage(f *fixture, success bool, reason string) message {
	return message{
		TaskID:       f.subtaskID.String(),
		WorkID:       f.parentID.String(),
		FilesChanged: []string{"internal/example/example.go"},
		TestResults:  "tests not recorded (legacy producer)",
		Summary:      fixturePrefix + " " + f.name,
		Success:      success,
		Reason:       reason,
	}
}

// ─── outcome cases ───────────────────────────────────────────────────────────

type expectation struct {
	// subtaskTo is the transition the subtask must record (passed|blocked)
	// and subtaskReason a fragment its reason must contain.
	subtaskTo     string
	subtaskReason string
	// parentReachesReview: the parent must record a transition to review
	// (all subtasks done and verified, R14). Otherwise, unless parentTo
	// names another destination, it must stay in implementation with no
	// transitions at all.
	parentReachesReview bool
	// parentTo (R14): the parent must pass through verification and record
	// a transition to this state instead (human_review for an escalated
	// verdict, blocked when the verification stage cannot run).
	parentTo string
	// parentPrompt: a prompt with this trigger must be raised on the parent.
	parentPrompt string
	// fixRound (R16): the verifier's request_changes sends the parent back
	// to implementation ("verification requested changes") with one fix
	// subtask carrying the verdict's findings and no human-review prompt.
	fixRound bool
	// verification (R14) is the decision verification.complete must carry
	// for the subtask (the real verification stage's). reused: it reused
	// the verdict the fixture stored for the evidence's head.
	verification stage.Decision
	reused       bool
	// pendingPR: a pending_pr_changes row must (not) appear for the parent,
	// and a pr.gated message must (not) be observed for it.
	pendingPR bool
	// prTitlePrefix / prBodyContains describe that row.
	prTitlePrefix  string
	prBodyContains string
	// blockedPrompt: a user_interactions row with trigger "blocked" must be
	// recorded for the subtask.
	blockedPrompt bool
	// legacyFallback: the consumers' legacy-fallback counters must each
	// increase by exactly one (the "with a warning" half of the legacy
	// contract). When false they must not move.
	legacyFallback bool
	// storedOutcome: the persisted implementation_verifications row (which
	// the fixture writes before publishing, as the producer does) must read
	// back with this outcome and the evidence SHA. Empty: no row at all.
	storedOutcome evidence.Outcome
	// driveReview: after the PR row appears, publish review.complete
	// approved and then pr.merged for the parent and check the D3 gate on
	// the way to QA.
	driveReview bool
	// gatedParent: the parent must be held at human_review with the
	// partial-verification gate reason and never see qa or qa.ready;
	// otherwise it must reach qa (its definition's next step) and never
	// carry the gate reason.
	gatedParent bool
}

type outcomeCase struct {
	name    string
	windows []evidence.CompatWindow
	optIn   bool
	build   func(t *testing.T, f *fixture) (message, *evidence.ImplementationEvidence)
	// verdict (R14), when set, is the independent verifier's gate the
	// fixture records for the evidence's head before publishing, as the
	// verification stage would have: the live stage finds it and reuses it
	// (no clone and no model are needed, so the run makes no LLM call).
	// Unset: the live stage runs for real.
	verdict verifier.Action
	expect  expectation
}

func (c outcomeCase) runsIn(w evidence.CompatWindow) bool {
	for _, x := range c.windows {
		if x == w {
			return true
		}
	}
	return false
}

func outcomeCases() []outcomeCase {
	both := []evidence.CompatWindow{evidence.CompatWindowLegacy, evidence.CompatWindowStrict}
	legacy := []evidence.CompatWindow{evidence.CompatWindowLegacy}
	strict := []evidence.CompatWindow{evidence.CompatWindowStrict}
	evidenceCase := func(mk func(*fixture) evidence.ImplementationEvidence, success bool) func(*testing.T, *fixture) (message, *evidence.ImplementationEvidence) {
		return func(t *testing.T, f *fixture) (message, *evidence.ImplementationEvidence) {
			ev := mk(f)
			return withEvidence(t, f, ev, success), &ev
		}
	}
	legacyCase := func(success bool, reason string) func(*testing.T, *fixture) (message, *evidence.ImplementationEvidence) {
		return func(t *testing.T, f *fixture) (message, *evidence.ImplementationEvidence) {
			return legacyMessage(f, success, reason), nil
		}
	}
	return []outcomeCase{
		{
			name:    "verified",
			windows: both,
			build:   evidenceCase(verifiedEvidence, true),
			verdict: verifier.ActionApprove,
			expect: expectation{
				subtaskTo:           statePassed,
				subtaskReason:       "implementation complete: outcome verified",
				parentReachesReview: true,
				verification:        stage.DecisionApprove,
				reused:              true,
				pendingPR:           true,
				prBodyContains:      "**verified**",
				storedOutcome:       evidence.OutcomeVerified,
				driveReview:         true,
			},
		},
		{
			name:    "partially_verified_opt_in",
			windows: legacy,
			optIn:   true,
			build:   evidenceCase(partiallyVerifiedEvidence, true),
			verdict: verifier.ActionApprove,
			expect: expectation{
				subtaskTo:           statePassed,
				subtaskReason:       partialVerificationMarker,
				parentReachesReview: true,
				verification:        stage.DecisionApprove,
				reused:              true,
				pendingPR:           true,
				prTitlePrefix:       partialTitlePrefix,
				prBodyContains:      partialBodyMarker,
				storedOutcome:       evidence.OutcomePartiallyVerified,
				driveReview:         true,
				gatedParent:         true,
			},
		},
		{
			name:    "partially_verified_no_opt_in",
			windows: legacy,
			build:   evidenceCase(partiallyVerifiedEvidence, true),
			expect: expectation{
				subtaskTo:     stateBlocked,
				subtaskReason: "outcome partially_verified and product has not opted in",
				verification:  stage.DecisionNotRun,
				blockedPrompt: true,
				storedOutcome: evidence.OutcomePartiallyVerified,
			},
		},
		{
			name:    "failed",
			windows: legacy,
			build:   evidenceCase(failedEvidence, false),
			expect: expectation{
				subtaskTo:     stateBlocked,
				subtaskReason: "outcome failed",
				verification:  stage.DecisionNotRun,
				blockedPrompt: true,
				storedOutcome: evidence.OutcomeFailed,
			},
		},
		{
			name:    "unknown_schema_version",
			windows: both,
			build:   evidenceCase(unknownSchemaEvidence, true),
			expect: expectation{
				subtaskTo:     stateBlocked,
				subtaskReason: "unknown evidence schema version 2 (supported: 1)",
				verification:  stage.DecisionNotRun,
				blockedPrompt: true,
				// The store rejects an unknown schema version on insert, as
				// the producer's Validate would; nothing is persisted.
			},
		},
		{
			name:    "no_evidence_legacy_success",
			windows: legacy,
			build:   legacyCase(true, ""),
			expect: expectation{
				subtaskTo:           statePassed,
				subtaskReason:       "implementation complete (legacy message without evidence)",
				parentReachesReview: true,
				verification:        stage.DecisionNotRun,
				pendingPR:           true,
				prBodyContains:      legacyLabel,
				legacyFallback:      true,
			},
		},
		{
			name:    "no_evidence_legacy_failure",
			windows: legacy,
			build:   legacyCase(false, "build failed: undefined: Foo"),
			expect: expectation{
				subtaskTo:      stateBlocked,
				subtaskReason:  "implementation failed: build failed: undefined: Foo",
				verification:   stage.DecisionNotRun,
				blockedPrompt:  true,
				legacyFallback: true,
			},
		},
		{
			name:    "no_evidence_strict",
			windows: strict,
			build:   legacyCase(true, ""),
			expect: expectation{
				subtaskTo:     stateBlocked,
				subtaskReason: "no evidence attached to implementation.complete (EVIDENCE_COMPAT_WINDOW=strict)",
				verification:  stage.DecisionNotRun,
				blockedPrompt: true,
			},
		},
		// R14: the verifier escalates before any PR: the parent goes
		// through verification to human_review with the verdict and its
		// prompt; no pending change, no pr.gated.
		{
			name:    "verified_verifier_escalates",
			windows: both,
			build:   evidenceCase(verifiedEvidence, true),
			verdict: verifier.ActionEscalate,
			expect: expectation{
				subtaskTo:     statePassed,
				subtaskReason: "implementation complete: outcome verified",
				parentTo:      stateHumanReview,
				parentPrompt:  triggerHumanReview,
				verification:  stage.DecisionEscalate,
				reused:        true,
				storedOutcome: evidence.OutcomeVerified,
			},
		},
		// R16: the verifier finds a fixable problem (a criterion not met at
		// a file:line, a located major defect) next to a recorded gap (a
		// PR-text criterion). That is a fix round, not a human: the parent
		// goes through verification back to implementation with a fix
		// subtask carrying the findings; no pending change, no pr.gated, no
		// human-review prompt.
		{
			name:    "verified_verifier_requests_changes",
			windows: both,
			build:   evidenceCase(verifiedEvidence, true),
			verdict: verifier.ActionRequestChanges,
			expect: expectation{
				subtaskTo:     statePassed,
				subtaskReason: "implementation complete: outcome verified",
				parentTo:      stateImplementation,
				fixRound:      true,
				verification:  stage.DecisionRequestChanges,
				reused:        true,
				storedOutcome: evidence.OutcomeVerified,
			},
		},
		// R14: the live verification stage runs for real on a verified head
		// in a repository it has no clone of: a structured failure that
		// blocks the parent with the escalation prompt, and no PR — the
		// attempt-13b situation, now stopped before the gate.
		{
			name:    "verified_stage_cannot_verify",
			windows: both,
			build:   evidenceCase(verifiedEvidence, true),
			expect: expectation{
				subtaskTo:     statePassed,
				subtaskReason: "implementation complete: outcome verified",
				parentTo:      stateBlocked,
				parentPrompt:  triggerBlocked,
				verification:  stage.DecisionError,
				storedOutcome: evidence.OutcomeVerified,
			},
		},
	}
}

// ─── driving and observing ───────────────────────────────────────────────────

const (
	waitTimeout = 90 * time.Second
	pollEvery   = 500 * time.Millisecond
	// settle is how long a negative assertion (no PR row, parent untouched,
	// no qa.ready) waits after the positive one was observed; pr-coordinator
	// re-checks sibling state after a 3s sleep, so this must comfortably
	// exceed that.
	settle = 8 * time.Second
)

func (s *stack) runCase(t *testing.T, c outcomeCase) {
	f := s.newFixture(t, c.name, c.optIn)
	before := s.legacyFallbackCounts(t)

	msg, ev := c.build(t, f)
	// The producer persists its evidence before it publishes
	// (cmd/implementation-agent/finalize.go); the fixture does the same so
	// the record the consumers act on is also the record the dashboard and
	// pr-coordinator's restart fallback read. The store validates on insert
	// exactly as the producer does, so an unknown-version record is never
	// persisted and the case expects no row.
	s.persistEvidence(t, ev, c.expect.storedOutcome != "")
	// R14: the verdict the verification stage would have recorded for the
	// evidence's head; the live stage reuses it.
	if c.verdict != "" {
		s.storeVerdict(t, f, c.verdict)
	}
	s.publish(t, bus.TopicImplementationComplete, f.parentID.String(), msg)

	// 1. The subtask's decision.
	reason := s.waitTransition(t, f.subtaskID, c.expect.subtaskTo)
	if !strings.Contains(reason, c.expect.subtaskReason) {
		t.Errorf("subtask → %s reason = %q, want it to contain %q", c.expect.subtaskTo, reason, c.expect.subtaskReason)
	}
	if strings.Contains(reason, f.messageSHA) {
		t.Errorf("subtask reason names the message's head_sha %s instead of the evidence's %s: %q", f.messageSHA, f.headSHA, reason)
	}

	// 1b. R14: the live verification stage's decision for the subtask,
	// about the evidence's head (never the message's).
	vc := s.waitVerification(t, f)
	wantDecision := c.expect.verification
	if wantDecision == stage.DecisionApprove && s.secondPass {
		wantDecision = stage.DecisionApprovePendingSecondPass
	}
	if vc.Decision != string(wantDecision) {
		t.Errorf("verification.complete decision = %q (%s), want %q", vc.Decision, vc.Reason, wantDecision)
	}
	if vc.Reused != c.expect.reused {
		t.Errorf("verification.complete reused = %t, want %t", vc.Reused, c.expect.reused)
	}
	wantVerified := ""
	if c.verdict != "" || wantDecision == stage.DecisionError {
		wantVerified = f.headSHA
	}
	if vc.HeadSHA != wantVerified {
		t.Errorf("verification.complete head_sha = %q, want %q (message SHA was %q)", vc.HeadSHA, wantVerified, f.messageSHA)
	}

	// 2. The parent: through verification (R14), then review or the
	// verifier's destination.
	if c.expect.parentReachesReview {
		s.waitTransition(t, f.parentID, stateReview)
		s.assertPassedVerification(t, f)
	}
	if c.expect.parentTo != "" {
		why := s.waitTransition(t, f.parentID, c.expect.parentTo)
		switch c.expect.parentTo {
		case stateHumanReview:
			s.assertPassedVerification(t, f)
			if !strings.Contains(why, "independent verifier escalated before the PR") {
				t.Errorf("parent → human_review reason = %q, want the verifier's escalation", why)
			}
		case stateBlocked:
			if !strings.Contains(why, "Verification before the PR could not run") || !strings.Contains(why, "no clone path") {
				t.Errorf("parent → blocked reason = %q, want the stage's structured failure", why)
			}
		case stateImplementation:
			s.assertPassedVerification(t, f)
			if why != "verification requested changes" {
				t.Errorf("parent → implementation reason = %q, want the verifier's fix round", why)
			}
		}
		if c.expect.parentPrompt != "" {
			s.waitPrompt(t, f.parentID, c.expect.parentPrompt)
		}
	}
	if c.expect.fixRound {
		s.assertFixRound(t, f)
	}

	// 3. The PR: the pending row, the pr_gate prompt and the pr.gated
	// message must all carry the evidence SHA, never the message's.
	var pr *pendingPR
	if c.expect.pendingPR {
		pr = s.waitPendingPR(t, f.parentID)
		if c.expect.prTitlePrefix != "" && !strings.HasPrefix(pr.title, c.expect.prTitlePrefix) {
			t.Errorf("pending PR title = %q, want prefix %q", pr.title, c.expect.prTitlePrefix)
		}
		if c.expect.prTitlePrefix == "" && strings.HasPrefix(pr.title, partialTitlePrefix) {
			t.Errorf("pending PR title = %q carries the partial-verification marker for a %s outcome", pr.title, c.name)
		}
		if !strings.Contains(pr.description, c.expect.prBodyContains) {
			t.Errorf("pending PR description does not contain %q:\n%s", c.expect.prBodyContains, pr.description)
		}
		// R14: a verifier approval is in the PR text, and labelled while the
		// second-pass review still decides.
		if c.verdict != "" {
			if !strings.Contains(pr.description, "## Verification") || !strings.Contains(pr.description, "**"+string(wantDecision)+"**") {
				t.Errorf("pending PR description lacks the verification section for %s:\n%s", wantDecision, pr.description)
			}
			if got := strings.Contains(pr.title, stage.SecondPassTitlePrefix); got != s.secondPass {
				t.Errorf("pending PR title = %q: second-pass label present = %t, want %t", pr.title, got, s.secondPass)
			}
		}
		wantSHA := f.headSHA
		if ev == nil {
			wantSHA = "" // a legacy message never verified a SHA
		}
		if pr.headSHA != wantSHA {
			t.Errorf("pending PR head_sha = %q, want the evidence SHA %q (message SHA was %q)", pr.headSHA, wantSHA, f.messageSHA)
		}
		if strings.Contains(pr.description, f.messageSHA) {
			t.Errorf("pending PR description names the message's head_sha %s", f.messageSHA)
		}
		if pr.branch != f.branch {
			t.Errorf("pending PR branch = %q, want %q", pr.branch, f.branch)
		}
		if pr.status != "pending" {
			t.Errorf("pending PR status = %q, want pending", pr.status)
		}
		gate := s.waitPrompt(t, f.parentID, triggerPRGate)
		if got, ok := gate.payload["head_sha"].(string); !ok {
			t.Errorf("pr_gate prompt context_payload.head_sha is missing or not a string (%T %v); payload keys: %v", gate.payload["head_sha"], gate.payload["head_sha"], payloadKeys(gate.payload))
		} else if got != wantSHA {
			t.Errorf("pr_gate prompt context_payload.head_sha = %q, want the evidence SHA %q", got, wantSHA)
		}
		if pv, _ := gate.payload["partially_verified"].(bool); pv != c.expect.gatedParent {
			t.Errorf("pr_gate prompt context_payload.partially_verified = %v, want %t", gate.payload["partially_verified"], c.expect.gatedParent)
		}
		// V5: the gate shows the verdict the operator decides with.
		if c.verdict != "" && !strings.Contains(gate.question, "Independent verifier: "+string(wantDecision)) {
			t.Errorf("pr_gate prompt question lacks the verdict: %q", gate.question)
		}
		gated := s.obs.wait(t, bus.TopicPRGated, f.parentID)
		if gated.HeadSHA != wantSHA {
			t.Errorf("pr.gated head_sha = %q, want the evidence SHA %q", gated.HeadSHA, wantSHA)
		}
		if gated.PartiallyVerified != c.expect.gatedParent {
			t.Errorf("pr.gated partially_verified = %t, want %t", gated.PartiallyVerified, c.expect.gatedParent)
		}
	}

	// 4. Prompts on the subtask.
	if c.expect.blockedPrompt {
		s.waitPrompt(t, f.subtaskID, triggerBlocked)
	}

	// 5. Negative assertions after the pipeline has settled.
	time.Sleep(settle)
	if !c.expect.pendingPR {
		if rows := s.pendingPRs(t, f.parentID); len(rows) != 0 {
			t.Errorf("expected no pending PR row for a %s outcome, got %d: %+v", c.name, len(rows), rows)
		}
		if m := s.obs.get(bus.TopicPRGated, f.parentID); len(m) != 0 {
			t.Errorf("expected no pr.gated for a %s outcome, observed %v", c.name, m)
		}
	}
	if !c.expect.parentReachesReview && c.expect.parentTo == "" {
		if ts := s.transitions(t, f.parentID); len(ts) != 0 {
			t.Errorf("parent must not move for a %s outcome; transitions: %v", c.name, ts)
		}
		if state, _ := s.state(t, f.parentID); state != stateImplementation {
			t.Errorf("parent state = %s, want %s", state, stateImplementation)
		}
	}

	if c.expect.fixRound {
		if n := s.promptCount(t, f.parentID, triggerHumanReview); n != 0 {
			t.Errorf("a fix round raised %d human-review prompt(s) on the parent, want 0", n)
		}
	}

	// 6. Legacy-fallback counters: +1 on every consumer for a message
	// without evidence in the legacy window, unchanged otherwise.
	after := s.legacyFallbackCounts(t)
	for url, b := range before {
		a, ok := after[url]
		if !ok {
			continue
		}
		want := b
		if c.expect.legacyFallback {
			want = b + 1
		}
		if a != want {
			t.Errorf("%s %s = %d after the message, want %d (was %d)", url, legacyFallbackCounter, a, want, b)
		}
	}

	// 7. Persistence: what the shared store reads back for this parent.
	s.assertStored(t, f, c.expect.storedOutcome)

	// 8. Review and merge: the D3 gate on the way to QA.
	var postReview, postMerge string
	if c.expect.driveReview {
		postReview = s.driveReview(t, f, c)
		postMerge = s.driveMerge(t, f, c)
	}

	// 9. Record the observation for the report.
	subState, subPrev := s.state(t, f.subtaskID)
	parState, parPrev := s.state(t, f.parentID)
	prSummary := "none"
	if pr != nil {
		prSummary = fmt.Sprintf("title=%q head_sha=%q status=%s", pr.title, pr.headSHA, pr.status)
	}
	t.Logf("RESULT %s | subtask=%s%s (→%s: %s) | verification=%s reused=%t (%s) | parent=%s%s %v | post_review=%s | post_merge=%s | qa.ready=%d | prompts subtask=%v parent=%v | pending_pr=%s | stored=%s",
		c.name,
		subState, prev(subPrev),
		c.expect.subtaskTo, reason,
		vc.Decision, vc.Reused, vc.Reason,
		parState, prev(parPrev), s.transitions(t, f.parentID),
		orNone(postReview), orNone(postMerge),
		len(s.obs.get(bus.TopicQAReady, f.parentID)),
		s.promptTriggers(t, f.subtaskID), s.promptTriggers(t, f.parentID),
		prSummary,
		s.storedSummary(t, f),
	)
}

// driveReview publishes review.complete (approved) for the parent and
// asserts where it went. Ungated: to qa (the definition's next step), with
// the reason status-orchestrator uses for the configured
// HUMAN_REVIEW_REQUIRED, and a qa.ready only when human review is not
// required. Gated: to human_review with the gate reason and a human-review
// prompt, and no qa.ready.
func (s *stack) driveReview(t *testing.T, f *fixture, c outcomeCase) string {
	t.Helper()
	known := len(s.transitions(t, f.parentID))
	s.publish(t, bus.TopicReviewComplete, f.parentID.String(), map[string]any{
		"work_id":               f.parentID.String(),
		"decision":              "approved",
		"feedback":              fixturePrefix + " synthetic review approval",
		"human_review_required": false,
	})
	to, why := s.waitTransitionTo(t, f.parentID, known, stateHumanReview, stateQA)
	if c.expect.gatedParent {
		if to != stateHumanReview || !strings.Contains(why, gateReasonFragment) {
			t.Errorf("gated parent left review to %s with reason %q, want %s with %q", to, why, stateHumanReview, gateReasonFragment)
		}
		s.waitPrompt(t, f.parentID, triggerHumanReview)
		return to + " (" + why + ")"
	}
	if to != stateQA {
		t.Errorf("ungated parent left review to %s (%q), want %s (its definition disables human review)", to, why, stateQA)
	}
	if strings.Contains(why, gateReasonFragment) {
		t.Errorf("ungated parent's post-review reason carries the partial gate: %q", why)
	}
	want := reviewApprovedAutoMerge
	if s.humanReviewRequired {
		want = reviewApprovedHumanReview
	}
	if why != want {
		t.Errorf("ungated post-review reason = %q, want %q (HUMAN_REVIEW_REQUIRED=%t)", why, want, s.humanReviewRequired)
	}
	if !s.humanReviewRequired {
		ready := s.obs.wait(t, bus.TopicQAReady, f.parentID)
		// qa-agent answers a fixture parent (no clone) by blocking it;
		// wait for that so the merge step below sees a settled item.
		_, blockReason := s.waitTransitionTo(t, f.parentID, known+1, stateBlocked)
		if !strings.Contains(blockReason, qaRefusedFragment) {
			t.Errorf("parent blocked after qa.ready with %q, want qa-agent's %q", blockReason, qaRefusedFragment)
		}
		return fmt.Sprintf("%s (%s); qa.ready head_sha=%q; then blocked by qa-agent (%s)", to, why, ready.HeadSHA, blockReason)
	}
	return to + " (" + why + ")"
}

// driveMerge publishes pr.merged for the parent and asserts
// resolvePRMergedTarget's contract (cmd/status-orchestrator): a gated
// parent is held in human_review (no transition) or moved back into it with
// the gate reason and a prompt, and never handed qa; an ungated parent
// already at (or past) qa is left where it is. Neither publishes qa.ready
// here: the gated parent is denied, the ungated one already left review.
func (s *stack) driveMerge(t *testing.T, f *fixture, c outcomeCase) string {
	t.Helper()
	known := len(s.transitions(t, f.parentID))
	qaReadyBefore := len(s.obs.get(bus.TopicQAReady, f.parentID))
	humanReviewPromptsBefore := s.promptCount(t, f.parentID, triggerHumanReview)
	stateBefore, _ := s.state(t, f.parentID)
	// No repository_id: the orchestrator would otherwise ask github-service
	// to pull a repository that does not exist. It still publishes
	// codex.updated{trigger: post_merge}; see doc.go.
	s.publish(t, bus.TopicPRMerged, f.parentID.String(), map[string]any{
		"work_id":             f.parentID.String(),
		"title":               fixturePrefix + " parent " + c.name,
		"acceptance_criteria": "end-to-end evidence fixture",
	})
	time.Sleep(settle)

	ts := s.transitions(t, f.parentID)
	var added []transition
	if known < len(ts) {
		// R17: parking on a prompt is a recorded transition now; the
		// prompt itself is asserted below, so only decisions are judged.
		added = withoutPromptParking(ts[known:])
	}
	stateAfter, prevAfter := s.state(t, f.parentID)
	qaReady := s.obs.get(bus.TopicQAReady, f.parentID)
	summary := fmt.Sprintf("state %s→%s%s, transitions %v", stateBefore, stateAfter, prev(prevAfter), added)

	if c.expect.gatedParent {
		for _, tr := range added {
			if tr.To != stateHumanReview || !strings.Contains(tr.Reason, gateReasonFragment) {
				t.Errorf("gated parent on pr.merged recorded %v, want only human_review with %q", tr, gateReasonFragment)
			}
		}
		if stateAfter == stateQA || (stateAfter != stateHumanReview && prevAfter != stateHumanReview) {
			t.Errorf("gated parent after pr.merged is %s (previous_state=%s), want held in human_review", stateAfter, prevAfter)
		}
		if len(qaReady) != qaReadyBefore {
			t.Errorf("gated parent must never get qa.ready; observed %v", qaReady)
		}
		// The gate must be seen to act, or a dropped pr.merged would pass
		// vacuously. From awaiting_user (the human-review prompt yielded
		// it) the orchestrator re-applies the gate as a transition plus a
		// second approval:human_review prompt; both are required. Only
		// when the parent was sitting in human_review itself is the
		// contract a silent hold, and then nothing observable is expected.
		if stateBefore != stateHumanReview {
			if len(added) != 1 {
				t.Errorf("gated parent on pr.merged from %s recorded %d transition(s) %v, want exactly one human_review transition with the gate reason", stateBefore, len(added), added)
			}
			if n := s.promptCount(t, f.parentID, triggerHumanReview); n != humanReviewPromptsBefore+1 {
				t.Errorf("gated parent on pr.merged has %d approval:human_review prompt(s), want %d (a new prompt saying PR merged — gate)", n, humanReviewPromptsBefore+1)
			}
		} else {
			summary += " (held in human_review: silent hold, nothing observable)"
		}
		return summary
	}
	// The ungated parent already left review to qa (and, when a qa.ready
	// was published, was blocked by qa-agent for lack of a clone); the
	// state machine has no qa→qa or blocked→qa edge, so pr.merged records
	// nothing. What must not happen is a diversion into human_review.
	if len(added) != 0 {
		t.Errorf("ungated parent on pr.merged recorded %v, want no transition (already past review)", added)
	}
	if stateAfter == stateHumanReview || prevAfter == stateHumanReview {
		t.Errorf("ungated parent after pr.merged is %s (previous_state=%s), want it at qa or blocked by qa-agent", stateAfter, prevAfter)
	}
	if stateAfter != stateQA && stateAfter != stateBlocked {
		t.Errorf("ungated parent after pr.merged is %s, want qa (or blocked by qa-agent after a qa.ready)", stateAfter)
	}
	if len(qaReady) != qaReadyBefore {
		t.Errorf("pr.merged after review published a new qa.ready for the ungated parent; observed %v", qaReady)
	}
	return summary
}

// storeVerdict records, for the fixture parent at the evidence's head, what
// the verification stage commits after a verifier run with the given gate
// action (a verdict artifact and its verification_results row, migration
// 059). The live stage reuses a verdict of the current verifier version
// for the same head instead of verifying again, so the fixture drives an
// approve or an escalate through the real stage without a clone or a model.
func (s *stack) storeVerdict(t *testing.T, f *fixture, action verifier.Action) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	v := verifier.Verdict{SchemaVersion: verifier.SchemaVersion, Defects: []verifier.Defect{}}
	switch action {
	case verifier.ActionApprove:
		v.Overall = verifier.OverallApprove
		v.PerCriterion = []verifier.CriterionResult{{ID: "AC-1", Status: verifier.StatusMet, Evidence: []verifier.Ref{"internal/example/example.go:1"}, Note: fixturePrefix + " met"}}
	case verifier.ActionRequestChanges:
		// R16: a fixable not_met next to a recorded PR-text gap.
		v.Overall = verifier.OverallRequestChanges
		v.PerCriterion = []verifier.CriterionResult{
			{ID: "AC-1", Status: verifier.StatusNotMet, Evidence: []verifier.Ref{fixtureDefectRef}, Note: fixturePrefix + " the boundary is off by one"},
			{ID: "AC-2", Status: verifier.StatusNotVerifiable, Evidence: []verifier.Ref{}, Note: fixtureGapLine},
		}
		v.Defects = []verifier.Defect{{Severity: verifier.SeverityMajor, File: "internal/example/example.go", Line: 3, Category: "off_by_one", Description: fixturePrefix + " the last item is dropped"}}
		v.Gaps = []string{fixtureGapLine}
	default:
		v.Overall = verifier.OverallEscalate
		v.PerCriterion = []verifier.CriterionResult{{ID: "AC-1", Status: verifier.StatusNotVerifiable, Evidence: []verifier.Ref{}, Note: fixturePrefix + " could not be checked"}}
	}
	g := verifier.Gate(v)
	if g.Action != action {
		t.Fatalf("fixture verdict gates to %s, want %s", g.Action, action)
	}
	content := stage.NewArtifactContent(v, g, string(stage.DecisionFor(g, s.secondPass)), f.branch, f.headSHA, f.headSHA, f.subtaskID.String())
	raw, err := json.Marshal(content)
	if err != nil {
		t.Fatal(err)
	}
	artifactID := uuid.New()
	mustExec(t, s.db, ctx, `INSERT INTO work_item_artifacts (id, work_item_id, artifact_type, content, agent_id) VALUES ($1, $2, 'review_comments', $3, 'review-agent')`,
		artifactID, f.parentID, raw)
	mustExec(t, s.db, ctx, `INSERT INTO verification_results (id, work_item_id, task_id, head_sha, decision, reason, verdict_artifact_id, verifier_version)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		uuid.New(), f.parentID, f.subtaskID, f.headSHA, string(stage.DecisionFor(g, s.secondPass)), fixturePrefix+" stored verdict: "+g.Reason, artifactID, verifier.Version)
	t.Logf("stored a %s verdict for %s at %s (artifact %s)", action, f.parentID, f.headSHA, artifactID)
}

// The R16 request_changes verdict's file:line and recorded gap.
const (
	fixtureDefectRef = "internal/example/example.go:3"
	fixtureGapLine   = "criterion AC-2 is non-code: satisfied by the PR text; verify manually"
	// fixSubtaskTitle mirrors cmd/status-orchestrator's fix subtask title.
	fixSubtaskTitle = "Address review feedback"
	// implementationNoRepo is implementation-agent's block reason for a
	// task whose repository has no clone: the fixture's fix subtask stops
	// there, before any model is resolved (no LLM call).
	implementationNoRepo = "Cannot implement: no repository available"
)

// assertFixRound (R16): exactly one fix subtask was dispatched for the
// parent, its description carries the verdict's findings (the unmet
// criterion, its file:line, the defect and the recorded gap), and the live
// implementation agent stops it on the fixture repository's missing clone
// before resolving a model — its outcome blocks the parent; the fix is
// never implemented.
func (s *stack) assertFixRound(t *testing.T, f *fixture) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := s.db.Query(ctx, `SELECT id, description FROM work_items WHERE parent_id = $1 AND title = $2`, f.parentID, fixSubtaskTitle)
	if err != nil {
		t.Fatalf("fix subtasks: %v", err)
	}
	type fix struct {
		id   uuid.UUID
		desc string
	}
	var fixes []fix
	for rows.Next() {
		var x fix
		if err := rows.Scan(&x.id, &x.desc); err != nil {
			t.Fatalf("scan fix subtask: %v", err)
		}
		fixes = append(fixes, x)
	}
	rows.Close()
	if len(fixes) != 1 {
		t.Fatalf("fix subtasks for %s = %d, want 1", f.parentID, len(fixes))
	}
	for _, want := range []string{"INDEPENDENT VERIFIER REQUESTED CHANGES", "AC-1 not met (subtask " + f.subtaskID.String()[:8], fixturePrefix + " the boundary is off by one",
		"At: " + fixtureDefectRef, "[major] " + fixtureDefectRef + " — off_by_one", fixtureGapLine} {
		if !strings.Contains(fixes[0].desc, want) {
			t.Errorf("fix subtask description lacks %q:\n%s", want, fixes[0].desc)
		}
	}
	// The agent's outcome blocks the work item it was sent for (the
	// parent); the fix subtask itself is never implemented.
	seen := 0
	for i, tr := range s.transitions(t, f.parentID) {
		if tr.To == stateImplementation && tr.Reason == "verification requested changes" {
			seen = i + 1
		}
	}
	_, why := s.waitTransitionTo(t, f.parentID, seen, stateBlocked)
	if !strings.Contains(why, implementationNoRepo) {
		t.Errorf("parent → blocked reason after the fix dispatch = %q, want %q (the agent must stop before any model call)", why, implementationNoRepo)
	}
	if st, _ := s.state(t, fixes[0].id); st != stateImplementation && st != stateBlocked {
		t.Errorf("fix subtask state = %s, want it never implemented", st)
	}
	t.Logf("fix subtask %s dispatched; the agent stopped it: %s", fixes[0].id, why)
}

// waitVerification waits for the verification.complete the live stage
// publishes for the fixture subtask.
func (s *stack) waitVerification(t *testing.T, f *fixture) observed {
	t.Helper()
	var got observed
	waitFor(t, "verification.complete for "+f.subtaskID.String(), func() (bool, string) {
		for _, m := range s.obs.get(bus.TopicVerificationComplete, f.parentID) {
			if m.TaskID == f.subtaskID.String() {
				got = m
				return true, m.Raw
			}
		}
		return false, "none observed"
	})
	return got
}

// assertPassedVerification: the parent entered verification (R14) before it
// left it.
func (s *stack) assertPassedVerification(t *testing.T, f *fixture) {
	t.Helper()
	for _, tr := range s.transitions(t, f.parentID) {
		if tr.To == "verification" {
			return
		}
	}
	t.Errorf("parent never entered verification: %v", s.transitions(t, f.parentID))
}

// persistEvidence writes ev through the shared store when the case expects a
// row, and asserts the store refuses it when the case expects none (unknown
// schema version). nil ev (legacy message) writes nothing.
func (s *stack) persistEvidence(t *testing.T, ev *evidence.ImplementationEvidence, wantStored bool) {
	t.Helper()
	if ev == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	id, err := s.evidence.Insert(ctx, ev)
	switch {
	case wantStored && err != nil:
		t.Fatalf("persist evidence: %v", err)
	case !wantStored && err == nil:
		t.Fatalf("store accepted evidence with schema_version %d (row %s); the producer's Validate would have rejected it", ev.SchemaVersion, id)
	case !wantStored:
		t.Logf("store rejected the record as the producer would: %v", err)
	}
}

// assertStored reads the parent's implementation_verifications rows back
// through the shared store (the dashboard's and pr-coordinator's read path)
// and checks the single row carries the evidence SHA and outcome.
func (s *stack) assertStored(t *testing.T, f *fixture, want evidence.Outcome) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := s.evidence.GetByWorkItem(ctx, f.parentID)
	if err != nil {
		t.Fatalf("stored evidence for %s: %v", f.parentID, err)
	}
	if want == "" {
		if len(rows) != 0 {
			t.Errorf("expected no stored evidence for %s, got %d row(s)", f.name, len(rows))
		}
		return
	}
	if len(rows) != 1 {
		t.Fatalf("expected exactly one stored evidence row for %s, got %d", f.name, len(rows))
	}
	got := rows[0].Evidence
	if got.HeadSHA != f.headSHA {
		t.Errorf("stored head_sha = %q, want the evidence SHA %q (message SHA was %q)", got.HeadSHA, f.headSHA, f.messageSHA)
	}
	if got.Outcome != want {
		t.Errorf("stored outcome = %q, want %q", got.Outcome, want)
	}
	if got.TaskID != f.subtaskID.String() || got.WorkID != f.parentID.String() {
		t.Errorf("stored identity = work %s task %s, want work %s task %s", got.WorkID, got.TaskID, f.parentID, f.subtaskID)
	}
	if d := evidence.ReleaseDecision(&got, evidence.ReleasePolicy{}); want == evidence.OutcomeVerified && d.Action != evidence.ActionRelease {
		t.Errorf("stored verified record does not release: %s", d.Reason)
	}
}

func (s *stack) storedSummary(t *testing.T, f *fixture) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := s.evidence.GetByWorkItem(ctx, f.parentID)
	if err != nil {
		return "error: " + err.Error()
	}
	if len(rows) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(rows))
	for _, r := range rows {
		parts = append(parts, string(r.Evidence.Outcome)+"@"+r.Evidence.HeadSHA)
	}
	return strings.Join(parts, ",")
}

func prev(p string) string {
	if p == "" {
		return ""
	}
	return "(previous_state=" + p + ")"
}

func orNone(s string) string {
	if s == "" {
		return "n/a"
	}
	return s
}

func (s *stack) publish(t *testing.T, topic, key string, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %s: %v", topic, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.kb.Publish(ctx, topic, key, data); err != nil {
		t.Fatalf("publish %s: %v", topic, err)
	}
	t.Logf("published %s key=%s: %s", topic, key, data)
}

// waitFor polls cond until it reports done or timeout elapses. detail is the
// last observation, used in the failure message.
func waitFor(t *testing.T, what string, cond func() (done bool, detail string)) string {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for {
		done, detail := cond()
		if done {
			return detail
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s (last: %s)", waitTimeout, what, detail)
		}
		time.Sleep(pollEvery)
	}
}

type transition struct {
	From, To, Reason string
}

func (tr transition) String() string { return fmt.Sprintf("%s→%s (%s)", tr.From, tr.To, tr.Reason) }

// withoutPromptParking drops the rows status-orchestrator records when a
// prompt parks an item in awaiting_user or its answer restores it (R17):
// bookkeeping of the prompt, not a pipeline decision.
func withoutPromptParking(ts []transition) []transition {
	var out []transition
	for _, tr := range ts {
		parked := tr.To == "awaiting_user" && strings.HasPrefix(tr.Reason, "awaiting user: prompt ")
		restored := tr.From == "awaiting_user" && strings.HasPrefix(tr.Reason, "prompt ") && strings.HasSuffix(tr.Reason, " answered")
		if !parked && !restored {
			out = append(out, tr)
		}
	}
	return out
}

func (s *stack) transitions(t *testing.T, id uuid.UUID) []transition {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := s.db.Query(ctx, `SELECT from_state, to_state, reason FROM work_item_transitions WHERE work_item_id = $1 ORDER BY timestamp`, id)
	if err != nil {
		t.Fatalf("transitions of %s: %v", id, err)
	}
	defer rows.Close()
	var out []transition
	for rows.Next() {
		var tr transition
		if err := rows.Scan(&tr.From, &tr.To, &tr.Reason); err != nil {
			t.Fatalf("scan transition: %v", err)
		}
		out = append(out, tr)
	}
	return out
}

// waitTransition waits for id to record a transition to `to` and returns its
// reason.
func (s *stack) waitTransition(t *testing.T, id uuid.UUID, to string) string {
	t.Helper()
	_, reason := s.waitTransitionTo(t, id, 0, to)
	return reason
}

// waitTransitionTo waits for id to record a transition, after the first
// `skip` already-known ones, to any of the given states, returning the
// state it reached and the reason. A new transition to a decision state
// outside the set fails immediately, so a wrong outcome is reported with
// its reason instead of as a timeout.
func (s *stack) waitTransitionTo(t *testing.T, id uuid.UUID, skip int, states ...string) (to, reason string) {
	t.Helper()
	want := make(map[string]bool, len(states))
	for _, st := range states {
		want[st] = true
	}
	var found transition
	waitFor(t, fmt.Sprintf("%s to transition to %v", id, states), func() (bool, string) {
		all := s.transitions(t, id)
		ts := all
		if skip < len(ts) {
			ts = ts[skip:]
		} else {
			ts = nil
		}
		for _, tr := range ts {
			if want[tr.To] {
				found = tr
				return true, tr.Reason
			}
		}
		// Only decisions are final: an intermediate hop (planning →
		// implementation) is fine, a decision to another state is the
		// wrong outcome.
		for _, tr := range ts {
			switch tr.To {
			case statePassed, stateBlocked, stateHumanReview, stateQA, stateReview:
				t.Fatalf("%s transitioned to %s (%q), expected one of %v", id, tr.To, tr.Reason, states)
			}
		}
		state, _ := s.state(t, id)
		return false, fmt.Sprintf("state=%s transitions=%v", state, all)
	})
	return found.To, found.Reason
}

func (s *stack) state(t *testing.T, id uuid.UUID) (state, previous string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var prev *string
	if err := s.db.QueryRow(ctx, `SELECT state::text, previous_state FROM work_items WHERE id = $1`, id).Scan(&state, &prev); err != nil {
		t.Fatalf("state of %s: %v", id, err)
	}
	if prev != nil {
		previous = *prev
	}
	return state, previous
}

type pendingPR struct {
	branch, title, description, status, headSHA string
}

func (s *stack) pendingPRs(t *testing.T, workItemID uuid.UUID) []pendingPR {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := s.db.Query(ctx, `SELECT branch, pr_title, pr_description, status, COALESCE(head_sha, '') FROM pending_pr_changes WHERE work_item_id = $1 ORDER BY created_at`, workItemID)
	if err != nil {
		t.Fatalf("pending_pr_changes for %s: %v", workItemID, err)
	}
	defer rows.Close()
	var out []pendingPR
	for rows.Next() {
		var p pendingPR
		if err := rows.Scan(&p.branch, &p.title, &p.description, &p.status, &p.headSHA); err != nil {
			t.Fatalf("scan pending_pr_changes: %v", err)
		}
		out = append(out, p)
	}
	return out
}

func (s *stack) waitPendingPR(t *testing.T, workItemID uuid.UUID) *pendingPR {
	t.Helper()
	var got pendingPR
	waitFor(t, "pending_pr_changes row for "+workItemID.String(), func() (bool, string) {
		rows := s.pendingPRs(t, workItemID)
		if len(rows) == 0 {
			return false, "no rows"
		}
		if len(rows) > 1 {
			t.Fatalf("%d pending_pr_changes rows for %s, want 1: %+v", len(rows), workItemID, rows)
		}
		got = rows[0]
		return true, got.title
	})
	return &got
}

func (s *stack) promptTriggers(t *testing.T, workItemID uuid.UUID) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := s.db.Query(ctx, `SELECT trigger, headline FROM user_interactions WHERE work_item_id = $1 ORDER BY requested_at`, workItemID)
	if err != nil {
		t.Fatalf("user_interactions for %s: %v", workItemID, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var trigger, headline string
		if err := rows.Scan(&trigger, &headline); err != nil {
			t.Fatalf("scan user_interactions: %v", err)
		}
		out = append(out, trigger+": "+headline)
	}
	return out
}

type prompt struct {
	question string
	payload  map[string]any
}

func payloadKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// promptCount is the number of user_interactions rows with the trigger on
// the work item.
func (s *stack) promptCount(t *testing.T, workItemID uuid.UUID, trigger string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var n int
	if err := s.db.QueryRow(ctx, `SELECT count(*) FROM user_interactions WHERE work_item_id = $1 AND trigger = $2`, workItemID, trigger).Scan(&n); err != nil {
		t.Fatalf("count %s prompts on %s: %v", trigger, workItemID, err)
	}
	return n
}

// waitPrompt waits for the newest user_interactions row with the trigger on
// the work item and returns its question and context payload.
func (s *stack) waitPrompt(t *testing.T, workItemID uuid.UUID, trigger string) prompt {
	t.Helper()
	var got prompt
	waitFor(t, fmt.Sprintf("%s prompt on %s", trigger, workItemID), func() (bool, string) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var payload []byte
		err := s.db.QueryRow(ctx, `SELECT question, context_payload FROM user_interactions WHERE work_item_id = $1 AND trigger = $2 ORDER BY requested_at DESC LIMIT 1`, workItemID, trigger).Scan(&got.question, &payload)
		if err == pgx.ErrNoRows {
			return false, fmt.Sprintf("prompts so far: %v", s.promptTriggers(t, workItemID))
		}
		if err != nil {
			t.Fatalf("user_interactions for %s: %v", workItemID, err)
		}
		got.payload = map[string]any{}
		if len(payload) > 0 {
			if err := json.Unmarshal(payload, &got.payload); err != nil {
				t.Fatalf("context_payload of %s prompt on %s: %v", trigger, workItemID, err)
			}
		}
		return true, got.question
	})
	return got
}

// ─── topic observer ──────────────────────────────────────────────────────────

// observed is one message seen on a watched topic, keyed by its work_id.
type observed struct {
	WorkID            string `json:"work_id"`
	HeadSHA           string `json:"head_sha"`
	PartiallyVerified bool   `json:"partially_verified"`
	// verification.complete (R14).
	TaskID   string `json:"task_id"`
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
	Reused   bool   `json:"reused"`
	Raw      string `json:"-"`
}

// observer tails topics from their current end with plain partition
// readers (no consumer group, so it neither commits nor competes with the
// real consumers) and records every message by topic and work_id.
type observer struct {
	mu     sync.Mutex
	seen   map[string]map[string][]observed
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func startObserver(t *testing.T, brokers []string, topics ...string) *observer {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	o := &observer{seen: make(map[string]map[string][]observed), cancel: cancel}
	conn, err := kafka.Dial("tcp", brokers[0])
	if err != nil {
		t.Fatalf("observer: dial %s: %v", brokers[0], err)
	}
	defer conn.Close()
	for _, topic := range topics {
		o.seen[topic] = make(map[string][]observed)
		parts, err := conn.ReadPartitions(topic)
		if err != nil {
			t.Fatalf("observer: partitions of %s: %v", topic, err)
		}
		for _, p := range parts {
			r := kafka.NewReader(kafka.ReaderConfig{
				Brokers:   brokers,
				Topic:     topic,
				Partition: p.ID,
				MinBytes:  1,
				MaxBytes:  10e6,
			})
			if err := r.SetOffset(kafka.LastOffset); err != nil {
				t.Fatalf("observer: %s/%d: %v", topic, p.ID, err)
			}
			o.wg.Add(1)
			go func(r *kafka.Reader, topic string) {
				defer o.wg.Done()
				defer r.Close()
				for {
					m, err := r.ReadMessage(ctx)
					if err != nil {
						return
					}
					o.record(topic, m.Value)
				}
			}(r, topic)
		}
	}
	// Give the readers time to resolve their end offsets before the first
	// message of the run is published.
	time.Sleep(3 * time.Second)
	return o
}

func (o *observer) record(topic string, value []byte) {
	var m observed
	_ = json.Unmarshal(value, &m)
	m.Raw = string(value)
	o.mu.Lock()
	defer o.mu.Unlock()
	o.seen[topic][m.WorkID] = append(o.seen[topic][m.WorkID], m)
}

func (o *observer) get(topic string, workID uuid.UUID) []observed {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]observed(nil), o.seen[topic][workID.String()]...)
}

func (o *observer) wait(t *testing.T, topic string, workID uuid.UUID) observed {
	t.Helper()
	var got observed
	waitFor(t, fmt.Sprintf("%s message for %s", topic, workID), func() (bool, string) {
		ms := o.get(topic, workID)
		if len(ms) == 0 {
			return false, "none observed"
		}
		got = ms[len(ms)-1]
		return true, got.Raw
	})
	return got
}

func (o *observer) stop() {
	o.cancel()
	o.wg.Wait()
}

// ─── metrics ─────────────────────────────────────────────────────────────────

// legacyFallbackCounts reads ae_evidence_compat_legacy_fallback_total from
// every configured /metrics endpoint. An endpoint that cannot be reached is
// reported and left out, so a missing service narrows the assertion rather
// than failing the outcome.
func (s *stack) legacyFallbackCounts(t *testing.T) map[string]int64 {
	t.Helper()
	out := make(map[string]int64, len(s.metricsURLs))
	client := &http.Client{Timeout: 5 * time.Second}
	for _, url := range s.metricsURLs {
		url = strings.TrimSpace(url)
		if url == "" {
			continue
		}
		resp, err := client.Get(url)
		if err != nil {
			t.Logf("metrics %s unreachable (%v) — legacy-fallback counter not asserted there", url, err)
			continue
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Logf("metrics %s: read: %v", url, err)
			continue
		}
		n, ok := parseCounter(string(body), legacyFallbackCounter)
		if !ok {
			t.Logf("metrics %s does not expose %s", url, legacyFallbackCounter)
			continue
		}
		out[url] = n
	}
	return out
}

func parseCounter(body, name string) (int64, bool) {
	for _, line := range strings.Split(body, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == name {
			n, err := strconv.ParseInt(fields[1], 10, 64)
			if err == nil {
				return n, true
			}
		}
	}
	return 0, false
}
