package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/agentregistry"
	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/codex"
	"github.com/autonomousengine/autonomous-engine/internal/codexgap"
	"github.com/autonomousengine/autonomous-engine/internal/configstore"
	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/gitref"
	"github.com/autonomousengine/autonomous-engine/internal/guards"
	"github.com/autonomousengine/autonomous-engine/internal/idempotency"
	"github.com/autonomousengine/autonomous-engine/internal/llm"
	"github.com/autonomousengine/autonomous-engine/internal/llm/tools"
	"github.com/autonomousengine/autonomous-engine/internal/metrics"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/products"
	"github.com/autonomousengine/autonomous-engine/internal/prompts"
	"github.com/autonomousengine/autonomous-engine/internal/verifier"
	"github.com/autonomousengine/autonomous-engine/internal/verifier/stage"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// R14: the verification stage. The owner's ruling is that the verifier
// runs before a PR exists and its decision gates whether one is created.
// This binary already has the verifier's toolchain (node/go in the image,
// `git clone --shared` scratch trees, the verifier model resolution), so the
// stage is a second consumer group of review-agent on
// implementation.complete rather than a new service: no new image, compose
// entry or memory reservation.
//
// Per subtask it verifies the evidence's verified head (never the message's
// own head_sha) against the subtask's requirements, the pinned base and the
// recorded evidence, and commits — in one transaction with the delivery's
// idempotency guard (M1-04/M1-05) — the verdict artifact, a
// verification_results row and the verification.complete outbox event.
// Setup failures (no clone, the head missing from the clone, the scratch
// tree or the diff failing, no verifier model) are structured failures:
// the same transaction blocks the parent (work.blocked, so
// status-orchestrator raises the escalation prompt) with decision "error".
// A commit that keeps failing is retried in process and then blocks; the
// delivery is left unacknowledged only when that block fails too (F2).
const (
	verificationConsumerGroup       = "review-agent-verification"
	verificationIdempotencyConsumer = "verification-stage"
	verificationAgentInstance       = "review-agent/verification"
)

// implementationCompleteMessage is the part of implementation.complete the
// stage reads; the whole payload rides on verification.complete verbatim.
type implementationCompleteMessage struct {
	TaskID   string          `json:"task_id"`
	WorkID   string          `json:"work_id"`
	Success  bool            `json:"success"`
	Reason   string          `json:"reason,omitempty"`
	Evidence json.RawMessage `json:"evidence,omitempty"`
}

func (m implementationCompleteMessage) hasEvidence() bool {
	raw := bytes.TrimSpace(m.Evidence)
	return len(raw) > 0 && !bytes.Equal(raw, []byte("null"))
}

// verificationTarget is what the stage does with one implementation.complete
// before touching git or a model.
type verificationTarget struct {
	// Verify: run (or reuse) a verdict for HeadSHA. Otherwise Decision
	// (not_run or error) and Reason are recorded as they are.
	Verify   bool
	Decision stage.Decision
	Reason   string
	Legacy   bool
	HeadSHA  string
	BaseSHA  string
	Branch   string
}

// planVerification decides whether a subtask is verified, from the same
// evidence rules pr-coordinator and status-orchestrator apply (the product's
// D2 policy, the message's identity): only a releasable implementation is
// verified; everything else is recorded as not_run so the downstream
// consumers apply their own (blocking) decision. A message without evidence
// has no verified head: not_run, Legacy.
func planVerification(m implementationCompleteMessage, policy evidence.ReleasePolicy, window evidence.CompatWindow, repositoryID *string) verificationTarget {
	if !m.hasEvidence() {
		t := verificationTarget{Decision: stage.DecisionNotRun, Legacy: true}
		if window == evidence.CompatWindowStrict {
			t.Reason = "no evidence attached to implementation.complete (" + evidence.CompatWindowEnv + "=strict): nothing to verify"
		} else {
			t.Reason = "legacy message without evidence: no verified head to verify before the PR (" + evidence.CompatWindowEnv + "=legacy); the verifier runs on pr.ready"
		}
		return t
	}
	expect := evidence.MessageIdentity{WorkID: m.WorkID, TaskID: m.TaskID, RepositoryID: repositoryID}
	d := evidence.ReleaseDecisionForMessage(m.Evidence, policy, expect)
	switch d.Action {
	case evidence.ActionRelease, evidence.ActionHumanReview:
	default:
		return verificationTarget{Decision: stage.DecisionNotRun, Reason: "implementation is not releasable, not verified: " + d.Reason}
	}
	if !m.Success {
		return verificationTarget{Decision: stage.DecisionNotRun, Reason: "implementation reports success=false alongside releasable evidence, not verified"}
	}
	var ev evidence.ImplementationEvidence
	if err := json.Unmarshal(m.Evidence, &ev); err != nil {
		return verificationTarget{Decision: stage.DecisionNotRun, Reason: "evidence unreadable, not verified: " + err.Error()}
	}
	head, ok := gitref.Sanitize(ev.HeadSHA)
	if !ok || head == "" {
		return verificationTarget{Decision: stage.DecisionError, Reason: fmt.Sprintf("releasable evidence carries no usable head SHA (%q)", ev.HeadSHA)}
	}
	return verificationTarget{Verify: true, HeadSHA: head, BaseSHA: ev.BaseSHA, Branch: ev.Branch}
}

// subtaskBranch is pr-coordinator's branch name for a subtask
// (agent/<work_id>/<task_id[:8]>), used when the evidence names none.
func subtaskBranch(workID, taskID uuid.UUID) string {
	short := taskID.String()
	if len(short) > 8 {
		short = short[:8]
	}
	return fmt.Sprintf("agent/%s/%s", workID, short)
}

// verificationDeps are the stage's collaborators.
type verificationDeps struct {
	pool     *pgxpool.Pool
	wg       *workgraph.WorkGraph
	registry *agentregistry.Registry
	cs       *configstore.ConfigStore
	kb       bus.MessageBus
	cx       *codex.Codex
	window   evidence.CompatWindow
	products products.PolicyStore
}

// buildVerificationHandler is the verification stage's handler (wrapped by
// the idempotency ledger in main).
func buildVerificationHandler(ctx context.Context, d verificationDeps) bus.Handler {
	return func(msgCtx context.Context, msg *bus.Message) error {
		var m implementationCompleteMessage
		if err := json.Unmarshal(msg.Value, &m); err != nil {
			log.Printf("verification stage: unmarshal: %v", err)
			return nil
		}
		workID, err := uuid.Parse(m.WorkID)
		if err != nil {
			log.Printf("verification stage: parse work_id %q: %v", m.WorkID, err)
			return nil
		}
		taskID := workID
		if m.TaskID != "" {
			if taskID, err = uuid.Parse(m.TaskID); err != nil {
				log.Printf("verification stage: work_id=%s parse task_id %q: %v", workID, m.TaskID, err)
				return nil
			}
		}
		// The verdict, its record, the event and the guard row commit
		// together; the process ctx keeps a cancelled message ctx from
		// dropping them (Wrap captured the delivery already).
		db := idempotency.Wrap(msgCtx, d.pool)

		task, terr := d.wg.GetWorkItem(msgCtx, taskID)
		if terr != nil {
			log.Printf("verification stage: work_id=%s task=%s: get subtask: %v", workID, taskID, terr)
		}
		var repoID *string
		if task != nil && task.RepositoryID != nil {
			s := task.RepositoryID.String()
			repoID = &s
		}
		policy := evidence.ReleasePolicy{}
		if task != nil {
			if p, perr := products.LoadReleasePolicyForItem(msgCtx, d.products, d.wg, task); perr == nil {
				policy = p
			} else {
				log.Printf("verification stage: work_id=%s task=%s: %v — partial verification policy defaults to not opted in", workID, taskID, perr)
			}
		}
		target := planVerification(m, policy, d.window, repoID)
		branch := target.Branch
		if branch == "" {
			branch = subtaskBranch(workID, taskID)
		}

		// The stage's assignment is recorded in the verification's own
		// transaction (stage.Commit), so a commit that fails or is a
		// duplicate leaves no active assignment behind.
		o := &stage.Outcome{
			Message: stage.Message{
				WorkID: workID.String(), TaskID: taskID.String(), HeadSHA: target.HeadSHA, Branch: branch,
				Implementation: json.RawMessage(msg.Value), VerifierVersion: verifier.Version,
			},
			Result:     stage.Result{WorkID: workID, TaskID: taskID, HeadSHA: target.HeadSHA, VerifierVersion: verifier.Version},
			Assignment: &models.AgentAssignment{WorkItemID: taskID, AgentType: models.AgentTypeReview, AgentInstanceID: verificationAgentInstance, Status: models.AssignmentStatusActive},
		}
		if repoID != nil {
			o.Message.RepositoryID = *repoID
		}

		switch {
		case !target.Verify && target.Decision == stage.DecisionError:
			setError(o, taskID, target.Reason)
		case !target.Verify:
			o.Message.Decision, o.Message.Reason, o.Message.Legacy = target.Decision, target.Reason, target.Legacy
			o.Result.Decision, o.Result.Reason, o.Result.Legacy = target.Decision, target.Reason, target.Legacy
			log.Printf("verification stage: work_id=%s task=%s not verified (%s): %s", workID, taskID, target.Decision, target.Reason)
		default:
			if sv, ok, rerr := stage.ReusableVerdict(msgCtx, d.pool, workID, taskID, target.HeadSHA); rerr != nil {
				log.Printf("verification stage: work_id=%s task=%s: look up a stored verdict for %s: %v — verifying", workID, taskID, target.HeadSHA, rerr)
				verifySubtask(ctx, msgCtx, d, o, task, workID, taskID, target, branch)
			} else if ok {
				reuseVerdict(o, sv)
				log.Printf("verification stage: work_id=%s task=%s head=%s reusing the verdict recorded at %s (%s): %s", workID, taskID, target.HeadSHA, sv.Result.CreatedAt.Format(time.RFC3339), o.Message.Decision, o.Message.Reason)
			} else {
				verifySubtask(ctx, msgCtx, d, o, task, workID, taskID, target, branch)
			}
		}
		if d.pool != nil {
			applyCodexGrounding(msgCtx, d.pool, o, workID, taskID)
		}
		return stage.Record(ctx, db, d.wg, agentID, o)
	}
}

// applyCodexGrounding applies codex coherence Part 3 to the stage's
// outcome: when the parent's or the subtask's latest research or
// implementation run recorded more empty codex lookups than
// CODEX_GAP_THRESHOLD (codexgap.Ungrounded), the evidence is ungrounded
// and the decision is not an automatic approval (stage.MarkUngrounded).
// A failed read fails closed the same way.
func applyCodexGrounding(ctx context.Context, q codexgap.Querier, o *stage.Outcome, workID, taskID uuid.UUID) {
	if q == nil || o == nil {
		return
	}
	ungrounded, why, err := codexgap.Ungrounded(ctx, q, workID, taskID)
	if err != nil {
		groundingReadErrors.Inc()
		log.Printf("verification stage: work_id=%s task=%s: %v — treating the run as ungrounded", workID, taskID, err)
		o.MarkUngrounded("the codex grounding of its runs could not be read (" + err.Error() + ")")
		return
	}
	if !ungrounded {
		return
	}
	ungroundedVerifications.Inc()
	log.Printf("verification stage: work_id=%s task=%s: ungrounded (%s) — no automatic approval", workID, taskID, why)
	o.MarkUngrounded(why)
}

var (
	// ungroundedVerifications counts outcomes marked ungrounded (codex
	// coherence Part 3): the run worked without the codex.
	ungroundedVerifications = metrics.NewCounter("ae_verification_ungrounded_total",
		"Verification outcomes whose runs were ungrounded (too many empty codex lookups); never approved automatically")
	// groundingReadErrors counts failed reads of the gap records; the stage
	// then fails closed (treats the run as ungrounded), so a spike here is
	// what explains a burst of escalations.
	groundingReadErrors = metrics.NewCounter("ae_verification_grounding_read_errors_total",
		"Verification outcomes treated as ungrounded because the codex gap records could not be read")
)

// setError makes o a structured failure: decision error, the parent
// blocked with the reason (the escalation prompt follows from work.blocked).
func setError(o *stage.Outcome, taskID uuid.UUID, reason string) {
	o.Message.Decision, o.Message.Reason = stage.DecisionError, reason
	o.Result.Decision, o.Result.Reason = stage.DecisionError, reason
	o.Artifact = nil
	o.Block = fmt.Sprintf("Verification before the PR could not run for subtask %s: %s. No PR was opened. "+
		"Fix the cause first: Retry re-runs this item from the start of the pipeline (research, planning and implementation again — costly), "+
		"not just the verification.", taskID, reason)
	log.Printf("verification stage: work_id=%s task=%s ERROR: %s — blocking", o.Message.WorkID, taskID, reason)
}

// reuseVerdict fills o from a verdict stored for the same head: no new
// artifact, the result points at the stored one.
func reuseVerdict(o *stage.Outcome, sv stage.StoredVerdict) {
	dec := stage.DecisionFor(sv.Content.Gate, secondPassEnabled())
	o.Message.Decision, o.Message.Reason, o.Message.Reused = dec, sv.Content.Gate.Reason, true
	o.Message.Feedback = sv.Content.Feedback
	o.Message.Criteria = stage.Summaries(sv.Content.Verdict)
	o.Message.Gaps = sv.Content.Verdict.Gaps
	o.Message.Defects = sv.Content.Defects
	o.Result.Decision, o.Result.Reason = dec, sv.Content.Gate.Reason
	o.Result.VerdictArtifactID = sv.Result.VerdictArtifactID
}

// verifySubtask sets up the tree and runs the verifier, filling o with the
// verdict, or with a structured failure.
func verifySubtask(ctx, msgCtx context.Context, d verificationDeps, o *stage.Outcome, task *models.WorkItem, workID, taskID uuid.UUID, target verificationTarget, branch string) {
	if task == nil || task.RepositoryID == nil {
		setError(o, taskID, "the subtask has no repository to verify in")
		return
	}
	repoID := task.RepositoryID.String()
	var clonePath, baseBranch string
	if err := d.pool.QueryRow(msgCtx, `SELECT COALESCE(clone_path, ''), COALESCE(base_branch, '') FROM repositories WHERE id = $1`, repoID).Scan(&clonePath, &baseBranch); err != nil {
		setError(o, taskID, fmt.Sprintf("cannot read repository %s: %v", repoID, err))
		return
	}
	if baseBranch == "" {
		baseBranch = "main"
	}
	if clonePath == "" {
		setError(o, taskID, fmt.Sprintf("repository %s has no clone path", repoID))
		return
	}
	if rc := guards.CheckRepoAvailable(clonePath); !rc.Valid {
		setError(o, taskID, "repository not available on disk: "+rc.Message)
		return
	}
	// The head is usually in the clone already (implementation works in a
	// worktree of it); otherwise fetch the branch, retrying in process.
	if err := stage.Retry(msgCtx, "fetch of the verified head", func() error {
		if gitref.CommitPresent(msgCtx, clonePath, target.HeadSHA) {
			return nil
		}
		ferr := fetchBranch(msgCtx, clonePath, branch)
		if gitref.CommitPresent(msgCtx, clonePath, target.HeadSHA) {
			return nil
		}
		return fmt.Errorf("verified head %s of branch %s is not in the clone (fetch: %v)", target.HeadSHA, branch, ferr)
	}); err != nil {
		setError(o, taskID, err.Error())
		return
	}
	rb, err := resolveReviewBase(msgCtx, newReviewBaseDeps(d.pool, clonePath), workID, repoID, "", baseBranch)
	if err != nil {
		setError(o, taskID, err.Error())
		return
	}
	var tree *stage.Tree
	if err := stage.Retry(msgCtx, "verification tree", func() error {
		var perr error
		tree, perr = stage.PrepareTree(msgCtx, clonePath, rb.Ref, rb.SHA, target.HeadSHA)
		return perr
	}); err != nil {
		setError(o, taskID, err.Error())
		return
	}
	defer tree.Close(ctx)

	resolution, rerr := d.registry.Resolve(msgCtx, workID, d.cs, "review-agent", "instance-1")
	if rerr != nil {
		setError(o, taskID, "cannot create the verifier's LLM adapter (provider not configured or credentials missing): "+rerr.Error())
		return
	}
	vctx := llm.WithInferenceTags(msgCtx, llm.TagsForResolution(resolution, workID))
	productID := task.ProductID
	vrun, vctx := newVerifierRun(ctx, vctx, d.pool, d.registry, d.cs, d.kb, d.cx, productID, workID, resolution)

	reqs, ev, lerr := verifier.LoadForTask(msgCtx, d.pool, workID, taskID)
	if lerr != nil {
		log.Printf("verification stage: work_id=%s task=%s: load verifier inputs: %v", workID, taskID, lerr)
	}
	log.Printf("verification stage: work_id=%s task=%s head=%s base=%s: %d criteria, %d task specs, %d evidence records, %d runs",
		workID, taskID, target.HeadSHA, rb.Ref, len(reqs.Criteria), len(reqs.TaskSpecs), len(ev.Records), len(ev.Runs))

	verdict := tree.Verify(vctx, vrun.modelRun(), verifier.Sources{WorkID: workID, Requirements: reqs, Evidence: ev})
	gate := verifier.Gate(verdict)
	dec := stage.DecisionFor(gate, secondPassEnabled())
	log.Printf("verification stage: work_id=%s task=%s head=%s verdict %s (%s): %s", workID, taskID, target.HeadSHA, dec, gate.Action, gate.Reason)

	o.Artifact = stage.NewArtifact(workID, agentID, stage.NewArtifactContent(verdict, gate, string(dec), branch, target.HeadSHA, tree.Diff.HeadSHA, taskID.String()))
	o.Message.Decision, o.Message.Reason = dec, gate.Reason
	o.Message.Feedback = verifier.RenderFeedback(verdict, gate)
	o.Message.Criteria = stage.Summaries(verdict)
	o.Message.Gaps = verdict.Gaps
	o.Message.Defects = verifier.CountDefects(verdict.Defects)
	o.Result.Decision, o.Result.Reason = dec, gate.Reason
}

// fetchBranch fetches origin <branch> into the clone.
func fetchBranch(ctx context.Context, clonePath, branch string) error {
	if branch == "" || strings.HasPrefix(branch, "-") {
		return fmt.Errorf("no usable branch %q", branch)
	}
	fctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(fctx, "git", "fetch", "--quiet", "origin", branch)
	cmd.Dir = clonePath
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// newVerifierRun resolves the verifier's model (agent type "verifier",
// falling back to the review model, recorded as shared), its prompt and its
// tool executor factory, and tags ctx with the verifier role. Shared by the
// stage and the pr.ready fallback.
func newVerifierRun(procCtx, ctx context.Context, pool *pgxpool.Pool, registry *agentregistry.Registry, cs *configstore.ConfigStore, kb bus.MessageBus, cx *codex.Codex, productID *uuid.UUID, workID uuid.UUID, resolution *agentregistry.Resolution) (verifierRun, context.Context) {
	var author *verifier.AuthorModel
	if am, ok := verifier.LoadAuthorModel(ctx, pool, workID); ok {
		author = &am
	}
	vAdapter, vInfo := verifier.ResolveModel(ctx, pool, cs, verifier.Shared{Adapter: resolution.Adapter, Provider: resolution.Provider, ModelID: resolution.ModelID}, author)
	vCtx := llm.WithInferenceTags(ctx, llm.InferenceTags{Role: llm.RoleVerifier, AgentType: prompts.VerifierAgentType, Provider: vInfo.Provider, ModelID: vInfo.ModelID})
	vMode := models.OperatingModeStandard
	if vInfo.Provider != "" && vInfo.ModelID != "" {
		if mode, modeErr := registry.GetOperatingMode(ctx, vInfo.Provider, vInfo.ModelID); modeErr == nil {
			vMode = mode
		}
	}
	vPrompt, vpErr := prompts.NewStore(pool).Resolve(ctx, prompts.IntentVerifyImplementation, prompts.VerifierAgentType, vInfo.Provider, vInfo.ModelID, string(vMode), "system")
	if vpErr != nil {
		vPrompt = prompts.VerifyImplementationSystemPrompt
	}
	vectorStore := codexVectorStore(ctx, pool, cs)
	return verifierRun{
		adapter:      vAdapter,
		info:         vInfo,
		systemPrompt: vPrompt,
		newExecutor: func(path string) *tools.Executor {
			ex := tools.NewExecutorWithCodex(path, cx, vectorStore, productID, kb, &workID)
			if branch, headSHA := tools.ResolveWorkItemBranch(procCtx, pool, workID); branch != "" {
				ex.SetWorkItemBranch(branch, headSHA)
			}
			return ex
		},
		onTool: toolStream(procCtx, kb, workID.String(), prompts.VerifierAgentType),
		db:     pool,
		workID: &workID,
	}, vCtx
}
