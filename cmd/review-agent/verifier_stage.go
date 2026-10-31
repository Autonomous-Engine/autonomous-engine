package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
	"github.com/autonomousengine/autonomous-engine/internal/llm/tools"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/verifier"
	"github.com/autonomousengine/autonomous-engine/internal/verifier/stage"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// M3-07: the independent verifier sees the requirements, the diff against
// the pinned base, the recorded evidence and a scratch repository at head —
// never the PR description or the implementation summary
// (internal/verifier). Its gate decides:
//
//	escalate         → human_review (existing transition), verdict recorded
//	request_changes  → review.complete changes_requested, feedback = defects
//	approve          → the existing LLM review as a second pass, unless
//	                   REVIEW_LLM_SECOND_PASS=false (then approve directly)
//
// R14: the verifier runs BEFORE the PR, in the verification stage
// (verification.go, a second consumer group of this binary on
// implementation.complete). On pr.ready the handler reuses the verdict the
// stage stored for the PR's verified head and runs the verifier itself
// only when none exists (a legacy message, a head the stage never saw),
// logging why. The scratch-clone and Run logic is internal/verifier/stage,
// shared by both paths.
//
// The verdict is a review_comments artifact with kind "verifier",
// committed in the same transaction as the outcome it caused.

// secondPassEnv names the switch for the LLM review after a verifier
// approval (stage.SecondPassEnv).
const secondPassEnv = stage.SecondPassEnv

func secondPassEnabled() bool { return stage.SecondPassEnabled() }

// Artifact kinds of review_comments written by review-agent.
const (
	artifactKindVerifier   = stage.ArtifactKindVerifier
	artifactKindSecondPass = "second_pass"
	// artifactKindVerifierReused records the review outcome decided from a
	// verdict the verification stage stored (R14) when no second pass runs.
	artifactKindVerifierReused = "verifier_reused"
)

// verifierArtifactContent is the verdict artifact (stage.ArtifactContent).
type verifierArtifactContent = stage.ArtifactContent

// verifierArtifact builds the verdict artifact. createdAt is set now so a
// second-pass artifact written later in the same transaction sorts after
// it (readers take the latest review_comments row).
func verifierArtifact(workID uuid.UUID, v verifier.Verdict, d verifier.Decision, p verifierPlan, branch, verifiedSHA, reviewedSHA string) *models.WorkItemArtifact {
	return stage.NewArtifact(workID, agentID, stage.NewArtifactContent(v, d, p.artifactDecision(d), branch, verifiedSHA, reviewedSHA, ""))
}

// verifierPlan is what the handler does after the verdict.
type verifierPlan struct {
	// decision, when set, is committed now with the verdict artifact.
	decision *reviewDecision
	// secondPass: run the LLM review; the verdict artifact is committed
	// with its outcome.
	secondPass bool
}

// decisionApprovePending is the verdict artifact's decision when the
// verifier approved and the second-pass review decides. It is not an
// approval: no reader of the newest approved review (dashboard-backend
// reviewedHead) may treat it as one.
const decisionApprovePending = string(stage.DecisionApprovePendingSecondPass)

// artifactDecision is the verdict artifact's "decision": "approve" only when
// that approval is the final outcome, approve_pending_second_pass when the
// second pass decides, otherwise the gate action (request_changes,
// escalate).
func (p verifierPlan) artifactDecision(d verifier.Decision) string {
	if d.Action == verifier.ActionApprove && p.secondPass {
		return decisionApprovePending
	}
	return string(d.Action)
}

// planAfterVerifier applies the gate (see the file comment).
func planAfterVerifier(v verifier.Verdict, d verifier.Decision, secondPass, humanReviewRequired bool) verifierPlan {
	switch d.Action {
	case verifier.ActionEscalate:
		return verifierPlan{decision: &reviewDecision{escalate: true, reason: "independent verifier escalated: " + d.Reason}}
	case verifier.ActionRequestChanges:
		return verifierPlan{decision: &reviewDecision{approve: false, feedback: verifier.RenderFeedback(v, d)}}
	case verifier.ActionApprove:
		if secondPass || d.SecondPassRequired {
			return verifierPlan{secondPass: true}
		}
		return verifierPlan{decision: &reviewDecision{approve: true, humanReviewRequired: humanReviewRequired, feedback: verifier.RenderFeedback(v, d)}}
	}
	return verifierPlan{decision: &reviewDecision{escalate: true, reason: "independent verifier: unknown gate action " + string(d.Action)}}
}

// reusedVerdictArtifact records the review outcome decided from a stored
// verdict without a second pass: the review_comments shape every reader
// parses (decision, branch, head_sha, reviewed_sha, defects), pointing at
// the verdict artifact the stage committed.
func reusedVerdictArtifact(workID uuid.UUID, sv stage.StoredVerdict, p verifierPlan, branch, verifiedSHA, reviewedSHA string) *models.WorkItemArtifact {
	verdictID := ""
	if sv.Result.VerdictArtifactID != nil {
		verdictID = sv.Result.VerdictArtifactID.String()
	}
	return &models.WorkItemArtifact{
		WorkItemID: workID,
		Type:       models.ArtifactTypeReviewComments,
		Content: mustMarshal(map[string]any{
			"kind":                artifactKindVerifierReused,
			"decision":            p.artifactDecision(sv.Content.Gate),
			"feedback":            sv.Content.Feedback,
			"branch":              branch,
			"head_sha":            verifiedSHA,
			"reviewed_sha":        reviewedSHA,
			"defects":             sv.Content.Defects,
			"verdict_artifact_id": verdictID,
			"verifier_version":    sv.Content.VerifierVersion,
		}),
		AgentID:   agentID,
		CreatedAt: time.Now().UTC(),
	}
}

// storedVerdictFor is the verdict the verification stage recorded for the
// verified head (R14 V3), or nil with why the verifier must run on pr.ready
// instead: a legacy message without a verified head, no verdict for that
// head, or a failed read (logged; running the verifier again is the safe
// fallback).
func storedVerdictFor(ctx context.Context, q verifier.Querier, workID uuid.UUID, verifiedSHA string) (*stage.StoredVerdict, string) {
	if verifiedSHA == "" {
		return nil, "the pr.ready carries no verified head (legacy message), so no stored verdict can match"
	}
	sv, ok, err := stage.ReusableVerdict(ctx, q, workID, uuid.Nil, verifiedSHA)
	switch {
	case err != nil:
		return nil, fmt.Sprintf("the stored verdict for head %s could not be read (%v)", verifiedSHA, err)
	case !ok:
		return nil, fmt.Sprintf("no verdict of verifier v%s is stored for head %s", verifier.Version, verifiedSHA)
	}
	return &sv, ""
}

// verifierRun is the model side of one verification.
type verifierRun struct {
	adapter      adapter.ModelAdapter
	info         verifier.ModelInfo
	systemPrompt string
	// newExecutor builds the tool executor rooted at the scratch repository.
	newExecutor func(path string) *tools.Executor
	onTool      tools.ToolCallback
	db          *pgxpool.Pool
	workID      *uuid.UUID
}

func (vr verifierRun) modelRun() stage.ModelRun {
	return stage.ModelRun{
		Adapter:      vr.adapter,
		Info:         vr.info,
		SystemPrompt: vr.systemPrompt,
		NewExecutor:  vr.newExecutor,
		OnTool:       vr.onTool,
		DB:           vr.db,
		WorkID:       vr.workID,
	}
}

// verifyAt runs the verifier against clonePath at head (stage.VerifyAt):
// every failure to set up the tree becomes a not_verifiable escalation,
// never an error.
func verifyAt(ctx context.Context, vr verifierRun, workID uuid.UUID, reqs verifier.Requirements, ev verifier.Evidence, clonePath, baseRef, baseSHA, head string) verifier.Verdict {
	return stage.VerifyAt(ctx, vr.modelRun(), workID, reqs, ev, clonePath, baseRef, baseSHA, head)
}

// toolStream publishes each tool call to the dashboard's live activity
// (tool.executed) and console (console.stream) — telemetry, direct
// publishes.
func toolStream(ctx context.Context, kb bus.MessageBus, workID, agentType string) tools.ToolCallback {
	return func(callID, toolName, arguments, result string, isError bool, _ tools.ResultMeta) {
		if kb == nil {
			return
		}
		now := time.Now().UTC().Format(time.RFC3339Nano)
		event, _ := json.Marshal(map[string]interface{}{
			"work_item_id":   workID,
			"task_id":        "",
			"tool":           toolName,
			"arguments":      arguments,
			"result_preview": truncate(result, 200),
			"is_error":       isError,
			"timestamp":      now,
		})
		kb.Publish(ctx, bus.TopicToolExecuted, workID, event) //nolint:errcheck
		console, _ := json.Marshal(map[string]interface{}{
			"work_item_id": workID,
			"task_id":      "",
			"agent_type":   agentType,
			"tool":         toolName,
			"arguments":    arguments,
			"result":       result,
			"is_error":     isError,
			"timestamp":    now,
		})
		kb.Publish(ctx, bus.TopicConsoleStream, workID, console) //nolint:errcheck
	}
}
