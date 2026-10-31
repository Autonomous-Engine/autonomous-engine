package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/guards"
	"github.com/autonomousengine/autonomous-engine/internal/idempotency"
	"github.com/autonomousengine/autonomous-engine/internal/interactions"
	"github.com/autonomousengine/autonomous-engine/internal/logging"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/outbox"
	"github.com/autonomousengine/autonomous-engine/internal/snapshot"
	"github.com/autonomousengine/autonomous-engine/internal/verifypolicy"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/autonomousengine/autonomous-engine/internal/zones"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// completionSink is the narrow surface through which the generation and
// finalization phases report an outcome. Production commits each outcome
// to Postgres in one transaction, with implementation.complete going out
// through the transactional outbox (M1-04); tests record the outcomes in
// memory so the phases can run against a temp git repo with no broker or
// database.
//
// There is deliberately no way to publish through this interface: every
// state write an outcome carries (evidence row, escalation prompt, block
// transition, assignment completion) and the implementation.complete event
// commit together or not at all.
type completionSink interface {
	// Report commits one outcome atomically. An error means nothing in it
	// was committed.
	Report(o completionOutcome) error
	// StoreOutput persists one raw command output (the final test run, the
	// push) as a work-item artifact and returns the reference the evidence
	// record carries in output_ref / artifact_refs. name labels the output
	// (rawOutputTest, rawOutputPush).
	StoreOutput(name string, content []byte) (ref string, err error)
}

// completionOutcome is everything one exit of the agent writes. Zero
// fields are skipped. Inside the transaction the writes run in the order
// they had as separate calls before the outbox: evidence, prompt, block,
// event, assignment.
type completionOutcome struct {
	// evidence is persisted through evidenceStore when both are set.
	evidence      *evidence.ImplementationEvidence
	evidenceStore evidenceSink
	// prompt is an escalation prompt raised alongside the block.
	prompt *interactions.Prompt
	// blockReason, when non-empty, transitions the work item to blocked and
	// enqueues work.blocked.
	blockReason string
	// msg, when set, is enqueued as implementation.complete.
	msg *implementationCompleteMessage
	// assignment, when set, closes the agent assignment with this status.
	assignment models.AssignmentStatus
	// baseEvents are base_sha_events rows (M2-02 drift audit) committed
	// with the outcome. Fallback tiers drop them with the evidence row.
	baseEvents []snapshot.Event
}

// evidenceSink persists the v1 evidence record. *evidence.Store satisfies
// it; tests capture the inserts in memory. A nil sink means "publish only".
// InsertTx is the path finalization uses: the evidence row commits in the
// same transaction as the implementation.complete outbox row.
type evidenceSink interface {
	Insert(ctx context.Context, ev *evidence.ImplementationEvidence) (uuid.UUID, error)
	InsertTx(ctx context.Context, tx pgx.Tx, ev *evidence.ImplementationEvidence) (uuid.UUID, error)
}

// outboxCompletionSink is the production completionSink.
type outboxCompletionSink struct {
	// ctx is the process-lifetime context. Outcomes are committed with it
	// so a cancelled message context cannot drop the event that releases
	// (or fails) downstream work.
	ctx context.Context
	// db begins the outcome transaction (*pgxpool.Pool in production).
	db           workgraph.TxBeginner
	wg           *workgraph.WorkGraph
	prompts      *interactions.Publisher
	workID       uuid.UUID
	assignmentID uuid.UUID
	taskID       string
	logger       *logging.Logger
	// run, when set, records the controller run's terminal state
	// (final_verified / blocked / failed) in the outcome's transaction
	// (M3-02, controller.go).
	run outcomeRecorder
}

// Report commits o in one transaction. The outbox aggregate is the parent
// work item (workID); the event key stays the task id, as it was when the
// event was published directly.
func (s *outboxCompletionSink) Report(o completionOutcome) error {
	var evidenceID uuid.UUID
	persisted := false
	err := workgraph.WithTx(s.ctx, s.db, func(tx pgx.Tx) error {
		if o.evidence != nil && o.evidenceStore != nil {
			id, err := o.evidenceStore.InsertTx(s.ctx, tx, o.evidence)
			if err != nil {
				return fmt.Errorf("persist implementation evidence: %w", err)
			}
			evidenceID, persisted = id, true
		}
		for _, ev := range o.baseEvents {
			if err := snapshots.RecordEventTx(s.ctx, tx, ev); err != nil {
				return err
			}
		}
		if o.prompt != nil {
			if err := s.prompts.PublishTx(s.ctx, tx, s.workID, o.prompt); err != nil {
				return err
			}
		}
		if o.blockReason != "" {
			if err := guards.BlockWorkItemTx(s.ctx, tx, s.workID, s.workID, agentID, o.blockReason); err != nil {
				return err
			}
		}
		if o.msg != nil {
			if err := outbox.EnqueueJSON(s.ctx, tx, s.workID, bus.TopicImplementationComplete, s.taskID, o.msg); err != nil {
				return err
			}
		}
		if o.assignment != "" {
			if err := s.wg.CompleteAssignmentTx(s.ctx, tx, s.assignmentID, o.assignment); err != nil {
				return err
			}
		}
		if s.run != nil {
			s.run.recordOutcomeTx(s.ctx, tx, o)
		}
		return nil
	})
	// The evidence id is only real once the transaction has committed.
	if err == nil && persisted && s.logger != nil {
		s.logger.Info(s.ctx, "persisted implementation evidence", map[string]interface{}{
			"task_id":         s.taskID,
			"work_id":         s.workID.String(),
			"verification_id": evidenceID.String(),
		})
	}
	return err
}

// reportOutcome commits o through the sink and returns whether it
// committed. When it did not, nothing in it reached the database or the
// outbox, so degraded outcomes (outcomeFallbacks) are tried in order until
// one commits: downstream must still learn that this task did not succeed.
// Each tier is logged; when every tier fails the event is lost (logged).
func reportOutcome(sink completionSink, o completionOutcome) bool {
	err := sink.Report(o)
	if err == nil {
		return true
	}
	if idempotency.IsDuplicate(err) {
		// M1-05: this implementation.tasks event was already processed (a
		// delivery that raced the first past the up-front Seen check). The
		// first delivery recorded the outcome; no fallback applies.
		log.Printf("implementation-agent: outcome not recorded — duplicate delivery of an already-processed implementation.tasks event")
		return false
	}
	log.Printf("implementation-agent: ERROR recording outcome failed, nothing was committed: %v", err)
	for i, fb := range outcomeFallbacks(o, err) {
		fbErr := sink.Report(fb)
		if fbErr == nil {
			log.Printf("implementation-agent: recorded fallback outcome tier %d (block=%t, event=%t)", i+1, fb.blockReason != "", fb.msg != nil)
			return false
		}
		log.Printf("implementation-agent: ERROR recording fallback outcome tier %d failed, nothing was committed: %v", i+1, fbErr)
	}
	log.Printf("implementation-agent: ERROR every fallback tier failed — implementation.complete is lost for this task")
	return false
}

// outcomeFallbacks are what is tried, in order, after o failed to commit
// with err. No tier claims success, persists evidence (the evidence still
// travels on the message, unchanged) or raises a prompt.
//
//   - Tier 1 drops the evidence row and the prompt — the statements this
//     change made part of the outcome, and the likeliest to have failed (a
//     foreign key, a transient error) — and keeps the block. A failed
//     success becomes a failure that blocks the work item, because pushed
//     code without a recorded outcome needs a human; a failed failure keeps
//     its own block.
//   - Tier 2, only when tier 1 had a block, also drops the block (an item
//     that can no longer be blocked), so that the completion event itself
//     is not lost with it.
func outcomeFallbacks(o completionOutcome, err error) []completionOutcome {
	tier1 := completionOutcome{blockReason: o.blockReason}
	if o.assignment != "" {
		tier1.assignment = models.AssignmentStatusFailed
	}
	if o.msg != nil {
		msg := *o.msg
		if msg.Success {
			reason := fmt.Sprintf("implementation outcome could not be recorded: %v", err)
			if msg.HeadSHA != "" {
				reason = fmt.Sprintf("commit %s on branch %s was pushed but its implementation outcome could not be recorded: %v", msg.HeadSHA, msg.Branch, err)
			}
			msg.Success = false
			msg.Verified = false
			msg.Reason = reason
			tier1.blockReason = reason
		}
		tier1.msg = &msg
	}
	tiers := []completionOutcome{tier1}
	if tier1.blockReason != "" {
		tier2 := tier1
		tier2.blockReason = ""
		tiers = append(tiers, tier2)
	}
	return tiers
}

const (
	// rawOutputTest and rawOutputPush name the two raw outputs finalization
	// stores. They appear in the artifact envelope and in fallback file names.
	rawOutputTest = "final-test-output"
	rawOutputPush = "push-output"

	// rawOutputArtifactType is the artifact_type enum value raw outputs are
	// stored under. The enum (migration 001) has no dedicated value and
	// adding one is a migration outside this change, so raw outputs follow
	// the existing implementation-summary convention: research_finding with
	// a typed JSON envelope (rawOutputArtifact) that identifies them.
	rawOutputArtifactType = models.ArtifactTypeResearchFinding
	// rawOutputArtifactKind is the envelope's type discriminator.
	rawOutputArtifactKind = "implementation_evidence_output"
	// rawOutputArtifactRefPrefix prefixes references to stored artifacts;
	// the remainder is the work_item_artifacts row id.
	rawOutputArtifactRefPrefix = "work_item_artifact:"
	// rawOutputFileRefPrefix prefixes references to the file fallback; the
	// remainder is an absolute path on the agent host.
	rawOutputFileRefPrefix = "file:"

	// maxStoredOutputBytes caps a stored raw output; only the tail is kept
	// beyond it (the failure is at the end of a test log), marked with
	// outputCutMarker.
	maxStoredOutputBytes = 1 << 20
	outputCutMarker      = "…"

	// gateCheckCommand names the run-level failure check recorded when the
	// final gate aborts before, or discards, a verification.
	gateCheckCommand = "final verification gate"
	// diagnosticCheckCommand names the import-diagnostics check: the same
	// verifier the model is told to call during generation
	// (codex_verify_imports), rerun by the gate against the committed files.
	diagnosticCheckCommand = "codex_verify_imports"
	// maxUnresolvedImportsInReason bounds how many unresolved imports the
	// operator-facing reason lists.
	maxUnresolvedImportsInReason = 20

	// policyCheckCommand names the run-level failure check recorded when the
	// repository verification policy blocks a run with no executable check.
	policyCheckCommand = "repository verification policy (no_checks_policy=block)"
	// policyBlockedSkipReason is the test check's skip_reason on that path.
	policyBlockedSkipReason = "no test runner detected; repository policy blocks runs with no executable checks"
	// policyBlockedDetail is the gate failure detail on that path; it ends
	// up in the block reason the operator sees.
	policyBlockedDetail = "no executable checks and repository policy blocks: no test runner was detected and the repository's verification policy sets no test_command override. Configure a test command, or set no_checks_policy to partial_with_human_review, in the repository's verification policy"
)

// verificationPolicySource is the slice of *verifypolicy.Store finalization
// needs.
type verificationPolicySource interface {
	LoadVerificationPolicy(ctx context.Context, repoID uuid.UUID) (verifypolicy.Policy, bool, error)
}

// applyVerificationPolicy resolves the repository's verification policy
// into in.policy and applies its test_command override. Every failure to
// resolve one (no repository id, no store, a lookup error) falls back to
// the fail-closed default, logged; the resolution is logged either way so
// the operator can see which policy governed the gate.
func applyVerificationPolicy(ctx context.Context, in finalizeInputs) finalizeInputs {
	logger := in.logger
	fields := map[string]interface{}{
		"task_id":       in.taskID.String(),
		"repository_id": in.task.RepositoryID,
	}
	repoID, parseErr := uuid.Parse(strings.TrimSpace(in.task.RepositoryID))
	found := false
	switch {
	case parseErr != nil:
		in.policy = verifypolicy.Default(uuid.Nil)
		logger.Warn(ctx, "verification policy: task names no valid repository — fail-closed default applies", fields)
	case in.policies == nil:
		in.policy = verifypolicy.Default(repoID)
		logger.Warn(ctx, "verification policy: no policy store configured — fail-closed default applies", fields)
	default:
		p, ok, err := in.policies.LoadVerificationPolicy(ctx, repoID)
		if err != nil {
			p = verifypolicy.Default(repoID)
			ok = false
			logger.Warn(ctx, "verification policy: lookup failed — fail-closed default applies", map[string]interface{}{
				"task_id": in.taskID.String(), "repository_id": in.task.RepositoryID, "err": err.Error(),
			})
		}
		in.policy, found = p, ok
	}
	detected := in.testCommand
	in.testCommand = in.policy.EffectiveTestCommand(detected)
	required := make([]string, 0, len(in.policy.RequiredChecks))
	for _, k := range in.policy.RequiredChecks {
		required = append(required, string(k))
	}
	fields["found"] = found
	fields["source"] = string(in.policy.Source)
	fields["no_checks_policy"] = string(in.policy.NoChecksPolicy)
	fields["required_checks"] = strings.Join(required, ",")
	fields["input_test_command"] = detected
	fields["test_command"] = in.testCommand
	// The generation phase already applies the same override
	// (policyTestCommand), so the input may equal the result; report
	// whether the command comes from the policy rather than detection.
	fields["test_command_from_policy"] = in.policy.TestCommand != nil && strings.TrimSpace(*in.policy.TestCommand) != ""
	fields["security_review_paths"] = len(in.policy.SecurityReviewPaths)
	logger.Info(ctx, "verification policy applied", fields)
	return in
}

// unimplementedRequiredChecks are the required check kinds the
// implementation agent has no runner for yet. Each is recorded as a not_run
// check so a policy that requires it can never be satisfied vacuously.
var unimplementedRequiredChecks = []evidence.CheckKind{evidence.CheckKindBuild, evidence.CheckKindLint}

// addUnimplementedRequiredChecks appends a not_run check (skip_reason
// "not implemented: <kind>") for every required kind the agent cannot run,
// capping the outcome at partially_verified. It returns those kinds.
func addUnimplementedRequiredChecks(policy verifypolicy.Policy, ev *evidence.ImplementationEvidence) []evidence.CheckKind {
	var added []evidence.CheckKind
	for _, k := range unimplementedRequiredChecks {
		if !policy.Requires(k) {
			continue
		}
		ev.Checks = append(ev.Checks, evidence.CheckResult{
			Kind:       k,
			Status:     evidence.CheckNotRun,
			SkipReason: "not implemented: " + string(k),
		})
		added = append(added, k)
	}
	return added
}

// flagSecurityReview adds evidence.FlagSecurityReviewRequired when any
// git-derived changed file matches the policy's security_review_paths (D3),
// and returns the matching files.
func flagSecurityReview(ctx context.Context, in finalizeInputs, ev *evidence.ImplementationEvidence) []string {
	matches := in.policy.SecurityReviewMatches(ev.ChangedFiles)
	if len(matches) == 0 {
		return nil
	}
	ev.Flags = append(ev.Flags, evidence.FlagSecurityReviewRequired)
	in.logger.Info(ctx, "changed files match security_review_paths — evidence flagged security_review_required", map[string]interface{}{
		"task_id": in.taskID.String(),
		"files":   strings.Join(matches, ", "),
	})
	return matches
}

// zoneSource is the slice of *zones.Store finalization needs.
type zoneSource interface {
	ListForRepository(ctx context.Context, repoID uuid.UUID) ([]zones.Zone, error)
}

// newZoneSource is the zone store on pool, or a nil interface without a
// pool (so applyZones takes its "no zone store" branch instead of calling
// a nil *zones.Store).
func newZoneSource(pool *pgxpool.Pool) zoneSource {
	if pool == nil {
		return nil
	}
	return zones.NewStore(pool)
}

// zoneCheckCommand is the failed run-level check recorded when a codex zone
// stops the subtask.
const zoneCheckCommand = "codex zone policy"

// zoneOutcome is what the repository's codex zones say about a run's
// changed files (applyZones).
type zoneOutcome struct {
	// review are the human_review zones touched (evidence flagged).
	review []string
	// stop, when non-empty, is why the subtask must not go on: an escalate
	// zone, a frozen zone, or zones that could not be read.
	stop string
}

// applyZones matches the git-derived changed files against the
// repository's codex zones (reusing the security_review_paths glob
// matcher). Every zone touched is recorded on the evidence with its
// policy; a human_review zone adds evidence.FlagZoneReviewRequired, which
// the release gate turns into human review for verified work; an escalate
// zone or a frozen zone (any policy) yields a stop reason, and so does a
// failed lookup (fail closed: the run cannot show it stays out of such
// zones). A task without a valid repository, or no configured store,
// enforces nothing (logged).
func applyZones(ctx context.Context, in finalizeInputs, ev *evidence.ImplementationEvidence) zoneOutcome {
	fields := map[string]interface{}{"task_id": in.taskID.String(), "repository_id": in.task.RepositoryID}
	repoID, err := uuid.Parse(strings.TrimSpace(in.task.RepositoryID))
	if err != nil || in.zones == nil {
		in.logger.Warn(ctx, "codex zones: no repository or no zone store — no zone enforced", fields)
		return zoneOutcome{}
	}
	zs, err := in.zones.ListForRepository(ctx, repoID)
	if err != nil {
		fields["err"] = err.Error()
		in.logger.Warn(ctx, "codex zones: lookup failed — stopping the subtask (fail closed)", fields)
		return zoneOutcome{stop: fmt.Sprintf("codex zones could not be read for repository %s (%v): the change cannot be checked against escalate or frozen zones", repoID, err)}
	}
	hits := zones.Match(zs, ev.ChangedFiles)
	for _, h := range hits {
		ev.Zones = append(ev.Zones, evidence.ZoneTouch{Name: h.Zone.Name, Policy: string(h.Zone.Policy), Files: h.Files})
	}
	var out zoneOutcome
	var stops []string
	for _, h := range zones.FrozenHits(hits) {
		stops = append(stops, fmt.Sprintf("%s (changed: %s)", h.Zone.FrozenText(), strings.Join(h.Files, ", ")))
	}
	for _, h := range zones.WithPolicy(hits, zones.PolicyEscalate) {
		if h.Zone.Frozen() {
			continue
		}
		stops = append(stops, fmt.Sprintf("codex zone %s (policy escalate) — changed: %s", h.Zone.Name, strings.Join(h.Files, ", ")))
	}
	if len(stops) > 0 {
		out.stop = strings.Join(stops, "; ") + ": a human must decide before this change goes further"
	}
	if review := zones.WithPolicy(hits, zones.PolicyHumanReview); len(review) > 0 {
		out.review = zones.Names(review)
		ev.Flags = append(ev.Flags, evidence.FlagZoneReviewRequired)
	}
	fields["zones"] = strings.Join(zones.Names(hits), ", ")
	fields["human_review"] = strings.Join(out.review, ", ")
	fields["stopped"] = out.stop != ""
	in.logger.Info(ctx, "codex zones applied", fields)
	return out
}

func joinKinds(kinds []evidence.CheckKind) string {
	s := make([]string, len(kinds))
	for i, k := range kinds {
		s[i] = string(k)
	}
	return strings.Join(s, ", ")
}

// rawOutputArtifact is the JSON envelope a raw output is stored in.
type rawOutputArtifact struct {
	Type    string `json:"type"`
	Name    string `json:"name"`
	TaskID  string `json:"task_id"`
	Content string `json:"content"`
}

func (s *outboxCompletionSink) StoreOutput(name string, content []byte) (string, error) {
	payload, err := json.Marshal(rawOutputArtifact{
		Type:    rawOutputArtifactKind,
		Name:    name,
		TaskID:  s.taskID,
		Content: string(content),
	})
	if err != nil {
		return "", fmt.Errorf("marshal %s artifact: %w", name, err)
	}
	artifact := &models.WorkItemArtifact{
		WorkItemID: s.workID,
		Type:       rawOutputArtifactType,
		Content:    payload,
		AgentID:    agentID,
	}
	// Pooled on purpose: the reference goes into the evidence record, so
	// the row must exist (be committed) before the outcome is.
	if err := s.wg.AddArtifact(s.ctx, artifact); err != nil {
		return "", err
	}
	return rawOutputArtifactRefPrefix + artifact.ID.String(), nil
}

const (
	// finalTestTimeout bounds the final test run, matching the pre-commit run.
	finalTestTimeout = 6 * time.Minute
	// defaultPushTimeout bounds the push; a hung remote is a failure, not
	// a wait. Overridable per call through finalizeInputs.pushTimeout.
	defaultPushTimeout = 5 * time.Minute
	// pushWaitDelay is how long to wait for a killed push's pipes to close
	// before giving up on its output (child processes may outlive it).
	pushWaitDelay = 2 * time.Second
)

// finalizeInputs is what the finalization phase needs. It deliberately
// carries no LLM adapter and no tool executor: finalization can only
// inspect the worktree, push it, and report. Nothing here can edit a
// file or move HEAD, so no mutation is reachable after the gate.
type finalizeInputs struct {
	task   implementationTaskMessage
	taskID uuid.UUID
	// clonePath is the shared clone that keeps the subtask branch after
	// the worktree is removed; failure reasons point operators at it, and
	// the raw-output file fallback lives next to it.
	clonePath     string
	worktreePath  string
	subtaskBranch string
	baseBranch    string
	// baseSHA is the commit the worktree branched from. FilesChanged is
	// derived from baseSHA...HEAD; baseBranch is the fallback when the
	// SHA could not be captured.
	baseSHA string
	// testCommand is the detected test runner ("" when none). The
	// repository's verification policy may override it (test_command); the
	// result is the input to the final verification gate that runs against
	// HEAD.
	testCommand string
	// policies resolves the repository's verification policy (M3-03,
	// migration 051) before the gate. nil means no store is configured:
	// the fail-closed default (verifypolicy.Default) applies, logged.
	policies verificationPolicySource
	// policy is the resolved verification policy. finalizeImplementation
	// fills it from policies; callers leave it zero.
	policy verifypolicy.Policy
	// zones lists the repository's codex zones (migration 063,
	// applyZones). nil means no zone store is configured: no zone is
	// enforced, logged.
	zones   zoneSource
	summary string
	// pushTimeout bounds the push; zero means defaultPushTimeout.
	pushTimeout time.Duration
	// evidenceStore persists the evidence record in the same transaction
	// as the implementation.complete outbox row. nil disables persistence
	// (logged). A failing insert rolls that transaction back; a success is
	// then recorded as a blocking failure (outcomeFallbacks).
	evidenceStore evidenceSink
	logger        *logging.Logger
	// pins looks up the subtask's pin when the message carries no base_sha
	// (subtasks built outside the planner). nil disables the lookup.
	pins pinLookup
	// baseEvents is set by finalizeImplementation (applyPinnedBase), not
	// by callers: the drift event every outcome of this run commits.
	baseEvents []snapshot.Event

	// fast holds the impact-selected fast checks of the pre-commit run
	// (fastchecks.go, M3-04); appendFastChecks adds them to the evidence
	// after the gate's checks. nil: none. The gate itself is unchanged.
	fast *fastChecks
	// continuationFailures are the continuation passes generation reverted
	// after their pre-commit checks failed (R11 S3); each is recorded as a
	// not_run test check, so the outcome is capped at partially_verified.
	continuationFailures []continuationFailure
}

// gateResult is the evidence produced by the final verification gate.
type gateResult struct {
	ok          bool
	headSHA     string
	files       []string
	testResults string
	verified    bool
	detail      string
	// testCheck is the test check exactly as the gate observed it: passed
	// or failed with exit code and timing when the runner ran, skipped
	// with the gate's reason otherwise. OutputRef is filled in by
	// finalization once testOutput has been stored.
	testCheck evidence.CheckResult
	// testOutput is the raw combined output of the test run (possibly
	// empty); meaningful only when testCheck ran (passed or failed).
	testOutput []byte
	// diagnosticCheck is the import-diagnostics check as the gate observed
	// it: passed when the verifier inspected at least one applicable file
	// and no unresolved imports remain at HEAD, failed when some do. Its
	// Status is empty — and the check is then not recorded — when the gate
	// aborted before the diagnostics ran (or discarded the attempt), or
	// when no changed file was applicable to the verifier: a check that
	// inspected nothing neither ran nor was deliberately skipped, and must
	// neither lift nor lower the outcome.
	diagnosticCheck evidence.CheckResult
	// policyBlocked is set when the gate failed only because no executable
	// check exists and the repository's no_checks_policy is block.
	policyBlocked bool
}

// finalizeImplementation verifies HEAD, pushes exactly that SHA, and
// commits the completion message (through the outbox) with evidence
// derived from git. It returns true only when success was committed.
//
// Every path builds, persists, and attaches a v1 evidence.ImplementationEvidence.
// The gate records two checks: the import diagnostics (kind diagnostic,
// codex_verify_imports over the committed files) and the test run (kind
// test):
//
//	tests passed, push ok                     -> verified           (test check passed;
//	                                             diagnostics passed or not applicable)
//	no runner, repository policy block        -> failed             (test check skipped,
//	  (the default, M3-03)                                             failed check "repository
//	                                                                   verification policy";
//	                                                                   not pushed, item blocked)
//	no runner, diagnostics passed, push ok,   -> partially_verified (test check skipped
//	  policy partial_with_human_review
//	                                             with reason, diagnostics inspected
//	                                             files and passed; D2: the product's
//	                                             partial_verification_opt_in decides
//	                                             downstream between human review
//	                                             and block)
//	no runner, diagnostics not applicable     -> unverified         (only the skipped
//	                                             test check; blocks even when opted
//	                                             in — no check actually ran)
//	unresolved imports remain at HEAD         -> failed             (diagnostics check
//	                                             failed; tests are not run — broken
//	                                             dependencies are failures under D2)
//	tests failed                              -> failed             (test check failed)
//	gate abort (dirty tree, unstable,
//	  unresolvable HEAD) or push failure      -> failed             (checks as observed +
//	                                             failed check of kind other)
//
// The diagnostics check is recorded only when the verifier inspected at
// least one applicable (JS/TS) file; when nothing was applicable no
// diagnostics check is recorded, so it can neither lift a skipped-runner
// run to partially_verified nor drag a passed-runner run below verified.
// A successful push is never recorded as a passed check: it is not a
// verification of the code, so it must not lift the outcome.
//
// The repository verification policy (applyVerificationPolicy) is resolved
// first: its test_command overrides detection; required kinds the agent
// cannot run (build, lint) are recorded as not_run checks, capping the
// outcome at partially_verified; and a changed file matching
// security_review_paths adds the security_review_required flag, which the
// release gate turns into human review for verified work (D3).
func finalizeImplementation(ctx context.Context, in finalizeInputs, sink completionSink) bool {
	in = applyPinnedBase(ctx, in)
	task := in.task
	taskID := in.taskID
	subtaskBranch := in.subtaskBranch
	summary := in.summary
	logger := in.logger

	startedAt := time.Now().UTC()
	// The repository's verification policy is resolved before the gate: it
	// may override the test command and decides what happens when no
	// executable check exists (M3-03).
	in = applyVerificationPolicy(ctx, in)
	gate := runFinalGate(ctx, in)
	ev := newEvidence(in, gate, startedAt)
	if st := gate.testCheck.Status; st == evidence.CheckPassed || st == evidence.CheckFailed {
		if ref := storeRawOutput(ctx, in, sink, rawOutputTest, gate.testOutput); ref != "" {
			ev.Checks[0].OutputRef = ref
			ev.ArtifactRefs = append(ev.ArtifactRefs, ref)
		}
	}
	appendFastChecks(ctx, in, sink, gate, ev)
	unimplemented := addUnimplementedRequiredChecks(in.policy, ev)
	addRevertedContinuationChecks(in, ev)
	securityFiles := flagSecurityReview(ctx, in, ev)
	zoned := applyZones(ctx, in, ev)
	msg := implementationCompleteMessage{
		TaskID:       task.TaskID,
		WorkID:       task.WorkID,
		FilesChanged: gate.files,
		TestResults:  gate.testResults,
		Summary:      summary,
		HeadSHA:      gate.headSHA,
		BaseSHA:      in.baseSHA,
		Branch:       subtaskBranch,
	}

	if !gate.ok {
		reason := fmt.Sprintf("final verification failed on branch %s at commit %s: %s", subtaskBranch, gate.headSHA, gate.detail)
		logger.Error(ctx, "final verification failed — not pushing", errors.New(gate.detail), map[string]interface{}{
			"task_id":  taskID.String(),
			"branch":   subtaskBranch,
			"head_sha": gate.headSHA,
		})
		if !hasFailedCheck(ev.Checks) {
			// The gate aborted for a reason that is not a failing check
			// (dirty tree, unstable verification, unresolvable HEAD, or the
			// repository policy blocking a run with no executable check):
			// record the run-level failure so the outcome is failed (D1.1).
			// A failed test or diagnostics check already carries the failure.
			command := gateCheckCommand
			if gate.policyBlocked {
				command = policyCheckCommand
			}
			ev.Checks = append(ev.Checks, evidence.CheckResult{
				Kind:    evidence.CheckKindOther,
				Command: command,
				Status:  evidence.CheckFailed,
			})
		}
		msg.Evidence = sealEvidence(ctx, in, ev, reason)
		reportFinalizeFailure(sink, in, msg, reason)
		return false
	}

	// Codex zones (migration 063): an escalate or frozen zone stops the
	// subtask here, before the push and before verification; the block
	// raises the escalation prompt (work.blocked). The commit stays local.
	if zoned.stop != "" {
		reason := fmt.Sprintf("%s. The commit %s is preserved locally in %s on branch %s; Retry once the zone allows the change.", zoned.stop, gate.headSHA, in.clonePath, subtaskBranch)
		logger.Warn(ctx, "codex zone stops the subtask — not pushing", map[string]interface{}{
			"task_id":  taskID.String(),
			"branch":   subtaskBranch,
			"head_sha": gate.headSHA,
			"reason":   zoned.stop,
		})
		ev.Checks = append(ev.Checks, evidence.CheckResult{
			Kind:    evidence.CheckKindOther,
			Command: zoneCheckCommand,
			Status:  evidence.CheckFailed,
		})
		msg.Evidence = sealEvidence(ctx, in, ev, reason)
		reportFinalizeFailure(sink, in, msg, reason)
		return false
	}

	// Push is part of success: a local commit is not a deliverable until
	// origin holds it.
	pushStarted := time.Now().UTC()
	pushOut, pushErr := pushVerifiedHead(ctx, in, gate.headSHA)
	pushFinished := time.Now().UTC()
	pushRef := storeRawOutput(ctx, in, sink, rawOutputPush, []byte(pushOut))
	if pushRef != "" {
		ev.ArtifactRefs = append(ev.ArtifactRefs, pushRef)
	}
	if pushErr != nil {
		reason := fmt.Sprintf("push failed (%s) for branch %s at commit %s: %s. The commit is preserved locally in %s on branch %s; push it manually or retry the task.",
			pushErr.category, subtaskBranch, gate.headSHA, pushErr.detail, in.clonePath, subtaskBranch)
		logger.Error(ctx, "git push failed — work preserved locally", errors.New(pushErr.detail), map[string]interface{}{
			"task_id":  taskID.String(),
			"branch":   subtaskBranch,
			"head_sha": gate.headSHA,
			"category": pushErr.category,
		})
		ev.Checks = append(ev.Checks, evidence.CheckResult{
			Kind:       evidence.CheckKindOther,
			Command:    pushCommand(in, gate.headSHA),
			ExitCode:   pushErr.exitCode,
			Status:     evidence.CheckFailed,
			StartedAt:  &pushStarted,
			FinishedAt: &pushFinished,
			OutputRef:  pushRef,
		})
		msg.Evidence = sealEvidence(ctx, in, ev, reason)
		reportFinalizeFailure(sink, in, msg, reason)
		return false
	}
	logger.Info(ctx, "pushed verified HEAD to remote", map[string]interface{}{
		"task_id":  taskID.String(),
		"branch":   subtaskBranch,
		"head_sha": gate.headSHA,
	})

	var reasons []string
	if !gate.verified {
		if gate.diagnosticCheck.Status == evidence.CheckPassed {
			reasons = append(reasons, fmt.Sprintf("no test runner detected: no tests ran against commit %s (import diagnostics passed); repository policy no_checks_policy=%s, the product's partial-verification policy decides downstream", gate.headSHA, in.policy.NoChecksPolicy))
		} else {
			reasons = append(reasons, fmt.Sprintf("no test runner detected and no changed file was applicable to the import verifier: no check ran against commit %s; the run is unverified and blocks regardless of the product's partial-verification policy", gate.headSHA))
		}
	}
	if len(unimplemented) > 0 {
		reasons = append(reasons, fmt.Sprintf("repository policy requires %s, which the implementation agent cannot run yet (recorded as not_run); the outcome is capped at partially_verified", joinKinds(unimplemented)))
	}
	if len(securityFiles) > 0 {
		reasons = append(reasons, "security review required: changed files match the repository's security_review_paths: "+strings.Join(securityFiles, ", "))
	}
	if len(zoned.review) > 0 {
		reasons = append(reasons, "zone review required: changed files fall in human_review codex zones: "+strings.Join(zoned.review, ", "))
	}
	msg.Evidence = sealEvidence(ctx, in, ev, strings.Join(reasons, "; "))
	msg.Success = true
	// Verified means the sealed outcome is verified: a required check the
	// agent cannot run caps a passing run below it.
	msg.Verified = gate.verified && msg.Evidence.Outcome == evidence.OutcomeVerified
	// The evidence row, the implementation.complete outbox row and the
	// assignment completion commit together. If they cannot, nothing of the
	// success is recorded and reportOutcome commits a blocking failure
	// instead, so true is returned only when success was actually committed.
	return reportOutcome(sink, completionOutcome{
		evidence:      persistableEvidence(in, msg.Evidence),
		evidenceStore: in.evidenceStore,
		msg:           &msg,
		assignment:    models.AssignmentStatusCompleted,
		baseEvents:    in.baseEvents,
	})
}

// newEvidence starts the v1 record from what the gate observed. Checks
// holds the test check first (finalization fills its output_ref at index
// 0), then the diagnostics check when the gate got far enough to run it;
// finalization appends the run-level failure check when one occurs.
// ChangedFiles and ArtifactRefs are non-nil so they encode as [] rather
// than null.
func newEvidence(in finalizeInputs, gate gateResult, startedAt time.Time) *evidence.ImplementationEvidence {
	checks := []evidence.CheckResult{gate.testCheck}
	if gate.diagnosticCheck.Status != "" {
		checks = append(checks, gate.diagnosticCheck)
	}
	return &evidence.ImplementationEvidence{
		SchemaVersion: evidence.SchemaVersion,
		WorkID:        in.task.WorkID,
		TaskID:        in.task.TaskID,
		RepositoryID:  optionalString(in.task.RepositoryID),
		BaseSHA:       in.baseSHA,
		HeadSHA:       gate.headSHA,
		Branch:        in.subtaskBranch,
		ChangedFiles:  append([]string{}, gate.files...),
		Checks:        checks,
		StartedAt:     startedAt,
		ArtifactRefs:  []string{},
	}
}

// hasFailedCheck reports whether any recorded check already failed.
func hasFailedCheck(checks []evidence.CheckResult) bool {
	for _, c := range checks {
		if c.Status == evidence.CheckFailed {
			return true
		}
	}
	return false
}

// formatUnresolvedImports renders the gate's unresolved imports for the
// operator-facing reason, one per line, capped at
// maxUnresolvedImportsInReason.
func formatUnresolvedImports(hits []unresolvedImportHit) string {
	var sb strings.Builder
	for i, h := range hits {
		if i == maxUnresolvedImportsInReason {
			fmt.Fprintf(&sb, "… and %d more", len(hits)-i)
			break
		}
		if i > 0 {
			sb.WriteString("\n")
		}
		fmt.Fprintf(&sb, "%s:%d: %s (%s)", h.File, h.Line, h.Import, h.Reason)
	}
	return sb.String()
}

// sealEvidence fixes the record's reason, finish time, and outcome, and
// checks that it can be persisted. The insert itself happens in the
// outcome's transaction (completionOutcome.evidence), together with the
// implementation.complete outbox row; see persistableEvidence.
func sealEvidence(ctx context.Context, in finalizeInputs, ev *evidence.ImplementationEvidence, reason string) *evidence.ImplementationEvidence {
	ev.Reason = reason
	ev.FinishedAt = time.Now().UTC()
	ev.Outcome = evidence.DeriveOutcome(ev.Checks)
	fields := map[string]interface{}{
		"task_id":  in.taskID.String(),
		"work_id":  ev.WorkID,
		"head_sha": ev.HeadSHA,
		"outcome":  string(ev.Outcome),
	}
	if err := ev.Validate(); err != nil {
		// Unreachable by construction (outcome is derived, skips carry
		// reasons); logged rather than dropped so a future regression is
		// visible instead of silently unpersisted.
		in.logger.Error(ctx, "implementation evidence fails validation — publishing unpersisted", err, fields)
		return ev
	}
	if in.evidenceStore == nil {
		in.logger.Warn(ctx, "no evidence store configured — implementation evidence published but not persisted", fields)
	}
	return ev
}

// persistableEvidence returns ev when it is to be inserted with the
// outcome: a store is configured and the record validates (an invalid
// record is published unpersisted, as sealEvidence logged); nil otherwise.
func persistableEvidence(in finalizeInputs, ev *evidence.ImplementationEvidence) *evidence.ImplementationEvidence {
	if in.evidenceStore == nil || ev == nil || ev.Validate() != nil {
		return nil
	}
	return ev
}

// storeRawOutput stores one raw output through the sink and returns its
// reference. When the artifact store refuses it, the output is written
// under the worktree-adjacent raw directory and referenced by path so it is
// never lost silently; "" means neither worked (logged).
func storeRawOutput(ctx context.Context, in finalizeInputs, sink completionSink, name string, content []byte) string {
	content = capOutput(content)
	ref, err := sink.StoreOutput(name, content)
	if err == nil {
		return ref
	}
	in.logger.Warn(ctx, "cannot store raw output as a work-item artifact — falling back to a file", map[string]interface{}{
		"task_id": in.taskID.String(),
		"output":  name,
		"err":     err.Error(),
	})
	dir := rawOutputDir(in.clonePath, in.taskID)
	path := filepath.Join(dir, name+".txt")
	writeErr := os.MkdirAll(dir, 0o755)
	if writeErr == nil {
		writeErr = os.WriteFile(path, content, 0o644)
	}
	if writeErr != nil {
		in.logger.Error(ctx, "cannot write raw output fallback file — output not stored", writeErr, map[string]interface{}{
			"task_id": in.taskID.String(),
			"output":  name,
			"path":    path,
		})
		return ""
	}
	return rawOutputFileRefPrefix + path
}

// rawOutputDir is the worktree-adjacent directory for the raw-output file
// fallback, a sibling of worktreePathFor's "worktree-<task>" directory.
func rawOutputDir(clonePath string, taskID uuid.UUID) string {
	return filepath.Join(clonePath, "..", "raw-"+taskID.String())
}

// capOutput keeps at most maxStoredOutputBytes of the tail of out (marked
// when cut) and replaces NUL bytes, which JSONB cannot store.
func capOutput(out []byte) []byte {
	s := tailString(string(out), maxStoredOutputBytes)
	return []byte(strings.ReplaceAll(s, "\x00", "�"))
}

func optionalString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// exitCodeOf returns the exit code observed for a finished command: 0 when
// err is nil, the process exit code for an exec.ExitError, and nil when no
// code was observed (killed by timeout, failed to start).
func exitCodeOf(err error) *int {
	if err == nil {
		code := 0
		return &code
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if code := ee.ExitCode(); code >= 0 {
			return &code
		}
	}
	return nil
}

// reportFinalizeFailure persists the evidence, blocks the work item,
// enqueues the evidence with success=false, and fails the assignment, all
// in one transaction.
func reportFinalizeFailure(sink completionSink, in finalizeInputs, msg implementationCompleteMessage, reason string) {
	msg.Success = false
	msg.Verified = false
	msg.Reason = reason
	reportOutcome(sink, completionOutcome{
		evidence:      persistableEvidence(in, msg.Evidence),
		evidenceStore: in.evidenceStore,
		blockReason:   reason,
		msg:           &msg,
		assignment:    models.AssignmentStatusFailed,
		baseEvents:    in.baseEvents,
	})
}

// runFinalGate runs the gate attempts and makes sure the result always
// carries the test check: when the gate aborted before or discarded a
// test run, the check is skipped with the gate's "skipped: <reason>" text
// as skip_reason, so the evidence names why nothing was verified.
func runFinalGate(ctx context.Context, in finalizeInputs) gateResult {
	res := runFinalGateAttempts(ctx, in)
	if res.testCheck.Status == "" {
		reason := strings.TrimSpace(strings.TrimPrefix(res.testResults, "skipped:"))
		if reason == "" {
			reason = "final verification gate did not run the tests"
		}
		res.testCheck = evidence.CheckResult{
			Kind:       evidence.CheckKindTest,
			Command:    in.testCommand,
			Status:     evidence.CheckSkipped,
			SkipReason: reason,
		}
	}
	return res
}

// runFinalGateAttempts binds verification to HEAD: it captures the HEAD
// SHA, refuses a dirty tree, runs import diagnostics and the configured
// tests, then confirms HEAD and the tracked tree are unchanged. If anything
// moved during verification the evidence is discarded and the gate reruns
// once.
func runFinalGateAttempts(ctx context.Context, in finalizeInputs) gateResult {
	logger := in.logger
	const maxAttempts = 2
	var res gateResult
	var headAfter string
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		head, err := gitRead(ctx, in.worktreePath, "rev-parse", "HEAD")
		if err != nil {
			return gateResult{testResults: "skipped: cannot resolve HEAD", detail: fmt.Sprintf("cannot resolve HEAD: %v", err)}
		}
		res = gateResult{headSHA: head}

		// Fail closed: an unreadable status is a gate failure, not a clean tree.
		dirty, err := gitRead(ctx, in.worktreePath, "status", "--porcelain", "--untracked-files=no")
		if err != nil {
			res.testResults = "skipped: cannot read worktree status"
			res.detail = fmt.Sprintf("cannot read worktree status: %v", err)
			return res
		}
		if dirty != "" {
			if attempt > 1 {
				// The previous attempt was clean before its test run and
				// dirty after it: the test run itself mutates tracked
				// files, so no run can ever verify exactly HEAD.
				res.testResults = "skipped: verification unstable"
				res.detail = "verification unstable: the test run modified tracked files, so the verified tree is not HEAD:\n" + dirty
				return res
			}
			res.testResults = "skipped: worktree has uncommitted changes"
			res.detail = "worktree has uncommitted changes after generation; HEAD does not contain them:\n" + dirty
			return res
		}

		base := in.baseSHA
		if base == "" {
			base = in.baseBranch
		}
		diff, err := gitRead(ctx, in.worktreePath, "diff", "--name-only", base+"...HEAD")
		if err != nil {
			res.testResults = "skipped: cannot diff against base"
			res.detail = fmt.Sprintf("cannot diff %s...HEAD: %v", base, err)
			return res
		}
		res.files = splitLines(diff)

		// Import diagnostics rerun against the committed files and are
		// recorded as a check of kind diagnostic when the verifier
		// inspected at least one applicable (JS/TS) file. Unlike
		// generation, where they are advisory and drive autofix, they are
		// blocking here: an unresolved import is a broken dependency, which
		// D2 classifies as a failure, never as partial verification. They
		// are also the check that actually ran when no test runner exists,
		// which is what lets a skipped-runner run reach partially_verified
		// (D2 opt-in). When nothing was applicable no check is recorded: a
		// verifier that inspected nothing neither ran nor was skipped, and
		// recording it as passed would open the D2 hole (partial
		// verification with no real check) while recording it as skipped
		// would cap every non-JS repository below verified.
		diagStarted := time.Now().UTC()
		hits, inspected := inspectImportsIn(ctx, logger, in.worktreePath, in.taskID.String(), res.files)
		diagFinished := time.Now().UTC()
		if inspected > 0 {
			res.diagnosticCheck = evidence.CheckResult{
				Kind:       evidence.CheckKindDiagnostic,
				Command:    diagnosticCheckCommand,
				Status:     evidence.CheckPassed,
				StartedAt:  &diagStarted,
				FinishedAt: &diagFinished,
			}
		} else {
			logger.Info(ctx, "final gate: no changed file applicable to the import verifier — no diagnostics check recorded", map[string]interface{}{"task_id": in.taskID.String(), "head_sha": head})
		}
		if len(hits) > 0 {
			logger.Warn(ctx, "final gate: unresolved imports remain at HEAD — failing the gate", map[string]interface{}{
				"task_id":  in.taskID.String(),
				"head_sha": head,
				"count":    len(hits),
			})
			res.diagnosticCheck.Status = evidence.CheckFailed
			res.testResults = "skipped: unresolved imports remain at HEAD"
			res.detail = fmt.Sprintf("unresolved imports remain at HEAD (%d); tests were not run:\n%s", len(hits), formatUnresolvedImports(hits))
			return res
		}

		if in.testCommand == "" && in.policy.NoChecksPolicy != verifypolicy.NoChecksPartialWithHumanReview {
			// Fail closed (D2): only an explicit partial_with_human_review
			// policy accepts a repository with no executable check; block,
			// the default, and anything unrecognised fail the gate.
			logger.Warn(ctx, "final gate: no executable checks and repository policy blocks — failing the gate", map[string]interface{}{
				"task_id":          in.taskID.String(),
				"head_sha":         head,
				"repository_id":    in.task.RepositoryID,
				"no_checks_policy": string(in.policy.NoChecksPolicy),
				"policy_source":    string(in.policy.Source),
			})
			res.testResults = "skipped: " + policyBlockedSkipReason
			res.detail = policyBlockedDetail
			res.policyBlocked = true
			return res
		}
		if in.testCommand == "" {
			logger.Warn(ctx, "final gate: no test runner detected — HEAD is unverified; repository policy accepts partial verification with human review", map[string]interface{}{"task_id": in.taskID.String(), "head_sha": head})
			res.testResults = "skipped: no test runner detected"
			res.ok = true
		} else {
			logger.Info(ctx, "final gate: running tests against HEAD", map[string]interface{}{"task_id": in.taskID.String(), "head_sha": head, "command": in.testCommand})
			testCtx, cancel := context.WithTimeout(ctx, finalTestTimeout)
			started := time.Now().UTC()
			out, testErr := runTests(testCtx, in.worktreePath, in.testCommand)
			finished := time.Now().UTC()
			cancel()
			res.testOutput = out
			res.testCheck = evidence.CheckResult{
				Kind:       evidence.CheckKindTest,
				Command:    in.testCommand,
				ExitCode:   exitCodeOf(testErr),
				StartedAt:  &started,
				FinishedAt: &finished,
			}
			if testErr != nil {
				res.testCheck.Status = evidence.CheckFailed
				res.testResults = "failed: " + in.testCommand
				res.detail = fmt.Sprintf("tests failed (%s): %v\n%s", in.testCommand, testErr, tailString(string(out), 4000))
			} else {
				res.testCheck.Status = evidence.CheckPassed
				res.testResults = "passed: " + in.testCommand
				res.verified = true
				res.ok = true
			}
		}

		headAfter, _ = gitRead(ctx, in.worktreePath, "rev-parse", "HEAD")
		dirtyAfter, _ := gitRead(ctx, in.worktreePath, "status", "--porcelain", "--untracked-files=no")
		if headAfter == head && dirtyAfter == "" {
			return res
		}
		logger.Warn(ctx, "final gate: HEAD or tracked tree changed during verification — evidence invalidated, rerunning", map[string]interface{}{
			"task_id":     in.taskID.String(),
			"attempt":     attempt,
			"head_before": head,
			"head_after":  headAfter,
			"dirty":       dirtyAfter,
		})
	}
	// The observed test check is discarded with the rest of the attempt:
	// runFinalGate records the skip.
	return gateResult{
		headSHA:     res.headSHA,
		files:       res.files,
		testResults: "skipped: verification unstable",
		detail:      fmt.Sprintf("verification unstable: HEAD moved during every verification attempt (verified %s, now %s)", res.headSHA, headAfter),
	}
}

// resolveBaseSHA returns the integration base for the subtask branch: the
// merge-base of baseBranch and HEAD, computed right after worktree
// creation. baseBranch is the pinned base SHA for a pinned subtask (M2-03:
// the worktree starts there, so the merge-base IS the pin) and the base
// branch name only for a legacy-unpinned one. For a normal task that is the
// branch point; for a review-fix task (started from the parent agent branch)
// it is where the whole PR diverged from the base, so files_changed covers
// the full PR delta. Falls back to HEAD, logged, when the merge-base cannot
// be computed.
func resolveBaseSHA(ctx context.Context, logger *logging.Logger, worktreePath, baseBranch, taskID string) string {
	base, err := gitRead(ctx, worktreePath, "merge-base", baseBranch, "HEAD")
	if err == nil && base != "" {
		return base
	}
	head, headErr := gitRead(ctx, worktreePath, "rev-parse", "HEAD")
	logger.Warn(ctx, "cannot compute merge-base — using worktree HEAD as base SHA", map[string]interface{}{
		"task_id": taskID, "base_branch": baseBranch, "err": fmt.Sprint(err), "head": head,
	})
	if headErr != nil {
		logger.Warn(ctx, "cannot resolve worktree HEAD — evidence will fall back to base branch", map[string]interface{}{
			"task_id": taskID, "err": headErr.Error(),
		})
		return ""
	}
	return head
}

// snapshots records base_sha_events inside the outcome transaction; the *Tx
// methods never use a pool.
var snapshots = snapshot.NewStore(nil)

// The unpinned series reads 0 from startup rather than being absent.
func init() { snapshot.RegisterUnpinned(agentID) }

// pinLookup reads a subtask's pin from the store (*snapshot.Store satisfies
// it). nil disables the lookup.
type pinLookup interface {
	Get(ctx context.Context, workID, repositoryID uuid.UUID) (snapshot.Pin, bool, error)
}

// newPinLookup is the production lookup over pool; nil (not a typed nil) when
// there is no pool.
func newPinLookup(pool *pgxpool.Pool) pinLookup {
	if pool == nil {
		return nil
	}
	return snapshot.NewStore(pool)
}

// driftReason is the reason a finalize drift_detected event carries. Since
// M2-03 a pinned worktree starts at the pin (HEAD asserted), so its base
// equals the pin; the event now fires only when the work was built on
// something else — a review-fix continuing a parent branch that was built on
// an older base.
const driftReason = "worktree base differs from pin"

// driftDetail is the JSON detail of a finalize drift_detected event.
type driftDetail struct {
	HeadSHA string `json:"head_sha"`
	Reason  string `json:"reason"`
}

// subtaskPin is the pin this run is compared with: the message's base_sha,
// or — for subtasks built outside the planner (review fixes, dashboard
// retries), whose message carries none — the subtask's own row in the store.
// "" with known=true means the item is genuinely legacy-unpinned; known=false
// means the store could not be read (logged, not counted as legacy).
func subtaskPin(ctx context.Context, in finalizeInputs, repoID uuid.UUID, haveRepo bool) (pin string, known bool) {
	if in.task.BaseSHA != "" {
		return in.task.BaseSHA, true
	}
	if in.pins == nil || !haveRepo {
		return "", true
	}
	p, found, err := in.pins.Get(ctx, in.taskID, repoID)
	if err != nil {
		logWarn(ctx, in.logger, "cannot read the subtask's base pin — drift not evaluated", map[string]interface{}{
			"task_id": in.taskID.String(), "repository_id": repoID.String(), "err": err.Error(),
		})
		return "", false
	}
	if !found {
		return "", true
	}
	return p.BaseSHA, true
}

// applyPinnedBase compares the base this worktree was actually built and
// diffed from (in.baseSHA, the merge-base of the local base branch and HEAD)
// with the subtask's pinned base (M2-02).
//
// The evidence never changes: evidence.BaseSHA and implementation.complete
// keep the merge-base, because that is what files_changed was diffed from (D1
// evidence contract). When the two differ — the local base branch is behind
// or ahead of the pin — ONE drift_detected event (old = pin, new = the base
// actually used, detail = {head_sha, reason}) is attached to in and commits
// with whatever outcome this run reports. Drift does not fail the task.
// Since M2-03 worktrees start at the pin, so equality is the normal case.
//
// No pin at all is legacy-unpinned (logged once per subtask, counted). An
// unknown worktree base (the worktree could not be read; in.baseSHA empty) is
// skipped with a log rather than reported as drift.
func applyPinnedBase(ctx context.Context, in finalizeInputs) finalizeInputs {
	repoID, repoErr := uuid.Parse(in.task.RepositoryID)
	pin, known := subtaskPin(ctx, in, repoID, repoErr == nil)
	if !known {
		return in
	}
	if pin == "" {
		snapshot.NoteUnpinned(agentID, in.taskID, "no base_sha on implementation.tasks and no pin row for the subtask")
		return in
	}
	if repoErr != nil {
		logWarn(ctx, in.logger, "pinned base without a valid repository id — drift not evaluated", map[string]interface{}{
			"task_id": in.taskID.String(), "repository_id": in.task.RepositoryID, "base_sha": pin,
		})
		return in
	}
	if in.baseSHA == "" {
		logWarn(ctx, in.logger, "worktree base unknown (worktree unreadable) — drift not evaluated", map[string]interface{}{
			"task_id": in.taskID.String(), "base_sha": pin,
		})
		return in
	}
	if in.baseSHA == pin {
		return in
	}
	head, err := gitRead(ctx, in.worktreePath, "rev-parse", "HEAD")
	if err != nil {
		head = ""
	}
	detail, _ := json.Marshal(driftDetail{HeadSHA: head, Reason: driftReason})
	logWarn(ctx, in.logger, "worktree base differs from the pinned base — drift recorded", map[string]interface{}{
		"task_id": in.taskID.String(), "pin": pin, "worktree_base": in.baseSHA, "head": head,
	})
	in.baseEvents = append(in.baseEvents, snapshot.Event{
		WorkItemID:   in.taskID,
		RepositoryID: repoID,
		Kind:         snapshot.EventDriftDetected,
		OldSHA:       pin,
		NewSHA:       in.baseSHA,
		Detail:       string(detail),
	})
	return in
}

// logWarn is logger.Warn tolerating a nil logger.
func logWarn(ctx context.Context, logger *logging.Logger, msg string, fields map[string]interface{}) {
	if logger == nil {
		log.Printf("implementation-agent: %s %v", msg, fields)
		return
	}
	logger.Warn(ctx, msg, fields)
}

// pushFailure classifies a failed push for operators. category is one of
// timeout, auth, remote unavailable, rejected, or error. exitCode is git's
// exit code when one was observed.
type pushFailure struct {
	category string
	detail   string
	exitCode *int
}

// pushArgs is the argument list of the push command, shared by the push
// itself and the command recorded in evidence. The worktree is selected
// with -C at execution time so the recorded command carries no host path.
func pushArgs(in finalizeInputs, sha string) []string {
	return []string{"push", "--force-with-lease", "origin", sha + ":refs/heads/" + in.subtaskBranch}
}

// pushCommand renders the push as recorded in the evidence check.
func pushCommand(in finalizeInputs, sha string) string {
	return "git " + strings.Join(pushArgs(in, sha), " ")
}

// pushVerifiedHead pushes sha to origin as subtaskBranch by explicit
// refspec, so the ref origin receives is by construction the commit the
// gate verified. The push is bounded by pushTimeout; a hung remote is
// reported as a timeout failure. The combined output is returned on every
// path so it can be stored as evidence.
func pushVerifiedHead(ctx context.Context, in finalizeInputs, sha string) (string, *pushFailure) {
	timeout := in.pushTimeout
	if timeout <= 0 {
		timeout = defaultPushTimeout
	}
	pushCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(pushCtx, "git", append([]string{"-C", in.worktreePath}, pushArgs(in, sha)...)...)
	cmd.WaitDelay = pushWaitDelay
	out, err := cmd.CombinedOutput()
	output := string(out)
	if err == nil {
		return output, nil
	}
	detail := strings.TrimSpace(output)
	if detail == "" {
		detail = err.Error()
	}
	exitCode := exitCodeOf(err)
	if pushCtx.Err() == context.DeadlineExceeded {
		return output, &pushFailure{category: "timeout", detail: fmt.Sprintf("no response from remote within %s: %s", timeout, detail), exitCode: exitCode}
	}
	return output, &pushFailure{category: classifyPushOutput(detail), detail: detail, exitCode: exitCode}
}

// classifyPushOutput maps git's push output to a coarse failure category.
func classifyPushOutput(out string) string {
	lower := strings.ToLower(out)
	switch {
	case strings.Contains(lower, "authentication failed"),
		strings.Contains(lower, "could not read username"),
		strings.Contains(lower, "permission denied"),
		strings.Contains(lower, "invalid credentials"):
		return "auth"
	case strings.Contains(lower, "does not appear to be a git repository"),
		strings.Contains(lower, "could not resolve host"),
		strings.Contains(lower, "connection refused"),
		strings.Contains(lower, "could not read from remote repository"),
		strings.Contains(lower, "no such remote"):
		return "remote unavailable"
	case strings.Contains(lower, "[rejected]"),
		strings.Contains(lower, "[remote rejected]"),
		strings.Contains(lower, "hook declined"):
		return "rejected"
	default:
		return "error"
	}
}

// gitRead runs a read-only git command in dir and returns trimmed stdout.
func gitRead(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

func splitLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}

// tailString returns the last n bytes of s, prefixed with a marker when cut.
func tailString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return outputCutMarker + s[len(s)-n:]
}
