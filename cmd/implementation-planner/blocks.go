package main

// The planner's hard blocks (R9 P1). A plan violation has a remediation in
// internal/plan's policy table; the blocks below are not violations of a
// plan but reasons there is no plan to validate, or no plan to commit. Each
// is classified here with what a Retry does, and each goes through
// escalateBlock: the block and its work.blocked event commit together, and
// status-orchestrator's work.blocked consumer raises the escalation prompt
// (Retry/Cancel), so no planning block leaves a bare blocked item (R7 found
// three that did). The plan-validation block (blockForPlanValidation) takes
// the same outbox path with its artifact.

import (
	"context"
	"errors"
	"log"

	"github.com/autonomousengine/autonomous-engine/internal/idempotency"
	"github.com/autonomousengine/autonomous-engine/internal/plan"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
)

// planningBlock names one hard block of the planner.
type planningBlock string

const (
	// blockContextValidation: onboarding is incomplete or the codex has no
	// grounding for the product.
	blockContextValidation planningBlock = "context_validation"
	// blockProvider: no LLM, or the call failed or came back empty.
	blockProvider planningBlock = "decomposition_provider"
	// blockEmptyBrief: the technical brief has no prose (R7).
	blockEmptyBrief planningBlock = "empty_brief"
	// blockDecompositionOutput: the model's output was unparseable, or the
	// strict pass rejected it, after the one correction (R7).
	blockDecompositionOutput planningBlock = "decomposition_output"
	// blockNoRepository: no step resolved to a repository of the product.
	blockNoRepository planningBlock = "no_repository"
	// blockAllNonCode: every step is non-code work (R3).
	blockAllNonCode planningBlock = "all_non_code"
	// blockDependencyGraph: the subtask dependency graph is invalid (M1-06,
	// M2-06 contract edges).
	blockDependencyGraph planningBlock = "invalid_dependency_graph"
	// blockCommitFailed: the plan's transaction could not commit.
	blockCommitFailed planningBlock = "commit_failed"
	// blockFrozenZone: a subtask's files fall in a frozen codex zone
	// (migration 063, zones.go).
	blockFrozenZone planningBlock = "frozen_zone"
)

// planningBlocks classifies every hard block. All are plan.RemediationBlock:
// the planner cannot repair them itself, and the escalation prompt hands the
// item to a human with a Retry.
var planningBlocks = map[planningBlock]plan.Policy{
	blockContextValidation: {Remediation: plan.RemediationBlock,
		Rationale: "planning needs completed onboarding and codex grounding for the product; Retry after onboarding re-runs planning"},
	blockProvider: {Remediation: plan.RemediationBlock,
		Rationale: "the LLM provider is missing or failing; Retry once it is healthy re-runs planning"},
	blockEmptyBrief: {Remediation: plan.RemediationBlock,
		Rationale: "the technical brief has no prose to decompose; research must produce one, and Retry re-runs planning against it"},
	blockDecompositionOutput: {Remediation: plan.RemediationBlock,
		Rationale: "the model's decomposition stayed unparseable or invalid after one correction (derived-id reuse is normalised first, R7); Retry re-decomposes"},
	blockNoRepository: {Remediation: plan.RemediationBlock,
		Rationale: "no step maps to a repository of the product, so there is nothing to validate; Retry after fixing the product's repositories re-decomposes"},
	blockAllNonCode: {Remediation: plan.RemediationBlock,
		Rationale: "every step is process or deployment work a human must do or verify (R3); Retry with a request that changes repository files re-decomposes"},
	blockDependencyGraph: {Remediation: plan.RemediationBlock,
		Rationale: "the validator re-prompts unknown dependencies and cycles, so one left at commit is a planner fault; Retry re-decomposes"},
	blockCommitFailed: {Remediation: plan.RemediationBlock,
		Rationale: "the plan's single transaction rolled back, so nothing was created; Retry once the database is healthy re-plans from a clean slate"},
	blockFrozenZone: {Remediation: plan.RemediationBlock,
		Rationale: "a planned subtask changes files in a frozen codex zone, so nothing was created; Retry after the zone is thawed (or the request changed) re-plans"},
}

// decompositionBlockKind classifies a decomposition failure.
func decompositionBlockKind(err error) planningBlock {
	var out *decompositionOutputError
	var empty *emptyBriefError
	switch {
	case errors.As(err, &out):
		return blockDecompositionOutput
	case errors.As(err, &empty):
		return blockEmptyBrief
	}
	return blockProvider
}

// escalateBlock blocks the parent with reason through the outbox
// (blockForDecompositionFailure: guards.BlockWorkItemTx enqueues
// work.blocked, and status-orchestrator raises the escalation prompt). db
// is idempotency.Wrap(msgCtx, pool), so the delivery's guard row commits
// with the block. A duplicate delivery reports true and blocks nothing. Any
// other failure of that transaction falls back to direct, which blocks
// without the event.
func escalateBlock(ctx context.Context, db workgraph.TxBeginner, workID uuid.UUID, kind planningBlock, reason string, direct func(reason string) error) (duplicate bool) {
	log.Printf("implementation-planner: work_id=%s: planning blocked (%s): %s", workID, kind, reason)
	err := blockForDecompositionFailure(ctx, db, workID, reason)
	if err == nil {
		return false
	}
	if idempotency.IsDuplicate(err) {
		log.Printf("implementation-planner: work_id=%s technical-brief.ready already handled — duplicate delivery, nothing committed", workID)
		return true
	}
	log.Printf("implementation-planner: work_id=%s: block through the outbox: %v — blocking without the event", workID, err)
	if bErr := direct(reason); bErr != nil {
		log.Printf("implementation-planner: block work item: %v", bErr)
	}
	return false
}
