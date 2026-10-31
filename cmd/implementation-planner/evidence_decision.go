package main

import (
	"bytes"
	"context"
	"log"

	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/products"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
)

// hasEvidence reports whether the message carries an evidence value at all.
// An absent field and an explicit JSON null are both "no evidence" and fall
// under the compatibility window (evidence.CompatWindow); anything else
// (including {} or malformed JSON) is present and is judged by
// evidence.ReleaseDecisionForMessage.
func (p implementationCompletePayload) hasEvidence() bool {
	raw := bytes.TrimSpace(p.Evidence)
	return len(raw) > 0 && !bytes.Equal(raw, []byte("null"))
}

// identityFor is what the message says about itself plus the task's
// repository, used to reject a record that belongs to another task (a
// producer bug or a replayed message). The ids are the message's own,
// exactly as received; repositoryID is the task's work item row's
// repository, nil when it has none (sibling-repository fallback items) or
// the caller could not load it, which skips only the repository check.
func (p implementationCompletePayload) identityFor(repositoryID *string) evidence.MessageIdentity {
	return evidence.MessageIdentity{WorkID: p.WorkID, TaskID: p.TaskID, RepositoryID: repositoryID}
}

// repositoryIDString converts a work item's repository id for the evidence
// identity check; nil stays nil.
func repositoryIDString(id *uuid.UUID) *string {
	if id == nil {
		return nil
	}
	s := id.String()
	return &s
}

// decideDependentRelease is the pure decision for whether a completed task
// may release the tasks held behind it. It returns the evidence.Decision the
// handler acts on; the Reason is operator-readable and always non-empty.
//
//	release      — evidence.ReleaseDecision returned release (verified), or
//	               a legacy message without evidence reported success in
//	               the legacy window. The task is marked completed and its
//	               dependents are released (D1.2).
//	human_review — ReleaseDecision returned human_review (partially_verified,
//	               product opted in). The implementation may advance only to
//	               mandatory human review (D2), so nothing downstream is
//	               released on the strength of it.
//	block        — everything else, including no evidence in strict mode,
//	               unknown schema versions in either mode, a failed legacy
//	               message in legacy mode, and a record whose work_id or
//	               task_id is not the message's (identity mismatch): a
//	               verified record for another task releases nothing here.
//
// policy comes from products.LoadReleasePolicyForItem; its zero value
// (unknown product or lookup failure) is "not opted in", so
// partially_verified blocks. The record must also match the message's ids;
// decideDependentRelease checks work_id and task_id only,
// decideDependentReleaseFor additionally the task's repository when the
// caller has it (releasePolicyForTask returns it).
func decideDependentRelease(payload implementationCompletePayload, policy evidence.ReleasePolicy, window evidence.CompatWindow) evidence.Decision {
	return decideDependentReleaseFor(payload, policy, window, nil)
}

// decideDependentReleaseFor is decideDependentRelease with the task's
// repository (from its work item row) added to the identity the record must
// match. nil skips the repository check.
func decideDependentReleaseFor(payload implementationCompletePayload, policy evidence.ReleasePolicy, window evidence.CompatWindow, repositoryID *string) evidence.Decision {
	if !payload.hasEvidence() {
		if window == evidence.CompatWindowStrict {
			reason := "no evidence attached to implementation.complete (" + evidence.CompatWindowEnv + "=strict)"
			if payload.Reason != "" {
				reason += "; implementation reported: " + payload.Reason
			}
			return evidence.Decision{Action: evidence.ActionBlock, Reason: reason}
		}
		// Legacy window: the pre-evidence contract.
		if payload.Success {
			return evidence.Decision{Action: evidence.ActionRelease, Reason: "implementation complete (legacy message without evidence)"}
		}
		return evidence.Decision{Action: evidence.ActionBlock, Reason: "implementation failed: " + payload.Reason}
	}

	// The record must be this message's: a verified record for another
	// task must never release this task's dependents (D1.3).
	d := evidence.ReleaseDecisionForMessage(payload.Evidence, policy, payload.identityFor(repositoryID))
	switch d.Action {
	case evidence.ActionRelease:
		if !payload.Success {
			// A producer that reports success=false alongside verified
			// evidence is contradicting itself; fail closed.
			return evidence.Decision{
				Action: evidence.ActionBlock,
				Reason: "implementation blocked: evidence is verified but the message reports success=false" + reasonDetail(payload.Reason),
			}
		}
		return evidence.Decision{Action: evidence.ActionRelease, Reason: "implementation complete: " + d.Reason}
	case evidence.ActionHumanReview:
		return d
	default:
		return evidence.Decision{Action: evidence.ActionBlock, Reason: "implementation blocked: " + d.Reason}
	}
}

func reasonDetail(reason string) string {
	if reason == "" {
		return ""
	}
	return " (" + reason + ")"
}

// releasePolicyForTask resolves the product policy that governs the
// completed task: the subtask's own product, else its parent's, through the
// shared products.LoadReleasePolicyForItem. Any failure (unparseable id,
// unknown work item, no product, store error) is logged and yields the zero
// policy ("not opted in"), so a lookup error can only make the gate stricter.
// It also returns the task's repository id from the same work item row for
// the evidence identity check: nil when the row has none or could not be
// loaded, which skips only the repository comparison.
func releasePolicyForTask(ctx context.Context, wg *workgraph.WorkGraph, store products.PolicyStore, rawTaskID string) (evidence.ReleasePolicy, *string) {
	taskID, err := uuid.Parse(rawTaskID)
	if err != nil {
		log.Printf("implementation-planner: implementation.complete: parse task id %q: %v — partial verification policy defaults to not opted in", rawTaskID, err)
		return evidence.ReleasePolicy{}, nil
	}
	if wg == nil || store == nil {
		return evidence.ReleasePolicy{}, nil
	}
	item, err := wg.GetWorkItem(ctx, taskID)
	if err != nil {
		log.Printf("implementation-planner: implementation.complete: get work item %s for product policy: %v — partial verification policy defaults to not opted in", taskID, err)
		return evidence.ReleasePolicy{}, nil
	}
	policy, err := products.LoadReleasePolicyForItem(ctx, store, wg, item)
	if err != nil {
		log.Printf("implementation-planner: implementation.complete: %v — partial verification policy defaults to not opted in", err)
	}
	return policy, repositoryIDString(item.RepositoryID)
}
