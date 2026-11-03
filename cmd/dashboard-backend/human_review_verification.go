package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/outbox"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// A person's approval of a verifier escalation (R14) is the verification
// decision. The verifier escalated a subtask to the human-review prompt
// (an unowned criterion, an evidence gap, the fix-round limit) and
// pr-coordinator refused its PR on that decision; the pre-R14 path sent the
// approved parent to QA instead, where a parent with no repository of its
// own blocked ("Cannot run QA: no repository available") and nobody told
// pr-coordinator. Now each escalated subtask of the parent's latest
// verification gets an approve verification_results row in the person's
// name, and its verification.complete — the same payload, decision approve
// — is enqueued again, so pr-coordinator opens the PR or the manual gate's
// pending change; the parent returns to human_review, where the PR-gate
// prompt or pr.ready moves it on.

// latestVerificationEventsSQL is each subtask's newest verification.complete
// event for the parent (the stage records one per subtask per round).
const latestVerificationEventsSQL = `
	SELECT DISTINCT ON (payload->>'task_id') payload
	  FROM outbox_events
	 WHERE topic = $1 AND aggregate_id = $2
	 ORDER BY payload->>'task_id', created_at DESC, id DESC`

const insertHumanVerificationSQL = `
	INSERT INTO verification_results (id, work_item_id, task_id, head_sha, decision, reason, legacy, verdict_artifact_id, verifier_version, created_at)
	VALUES ($1, $2, $3, $4, 'approve', $5, false, $6, $7, $8)`

// humanVerifierVersion marks a verification_results row a person recorded.
const humanVerifierVersion = "human"

// escalatedDecisions are the verifier decisions a person's approval
// overrides: an escalation, or the changes it could not get fixed within
// the round limit (status-orchestrator escalates those to human_review too).
var escalatedDecisions = map[string]bool{"escalate": true, "request_changes": true}

// approveEscalatedVerification records the person's approval for every
// escalated subtask of parentID and re-enqueues its verification.complete as
// approve, then returns the parent from awaiting_user to human_review, all
// in one transaction. handled is false when the parent has no escalated
// verification (the pre-R14 review path): the caller takes its QA path.
func (h *Handlers) approveEscalatedVerification(ctx context.Context, parentID uuid.UUID, userID, note string) (handled bool, err error) {
	if h.txdb == nil || h.verifications == nil {
		return false, nil
	}
	escalated, err := latestEscalatedVerifications(ctx, h.verifications, parentID)
	if err != nil || len(escalated) == 0 {
		return false, err
	}
	err = workgraph.WithTx(ctx, h.txdb, func(tx pgx.Tx) error {
		now := time.Now().UTC()
		for _, payload := range escalated {
			taskID, perr := uuid.Parse(stringField(payload, "task_id"))
			if perr != nil {
				return fmt.Errorf("verification event task_id %q: %w", stringField(payload, "task_id"), perr)
			}
			reason := fmt.Sprintf("human review: approved by %s", userID)
			if strings.TrimSpace(note) != "" {
				reason += " — " + strings.TrimSpace(note)
			}
			if prior := stringField(payload, "reason"); prior != "" {
				reason += " (the verifier escalated: " + prior + ")"
			}
			resultID := uuid.New()
			var artifactID *uuid.UUID
			if s := stringField(payload, "verdict_artifact_id"); s != "" {
				if id, aerr := uuid.Parse(s); aerr == nil {
					artifactID = &id
				}
			}
			if _, ierr := tx.Exec(ctx, insertHumanVerificationSQL, resultID, parentID, taskID, stringField(payload, "head_sha"), reason, artifactID, humanVerifierVersion+":"+userID, now); ierr != nil {
				return fmt.Errorf("record human verification for task %s: %w", taskID, ierr)
			}
			payload["decision"] = "approve"
			payload["reason"] = reason
			payload["result_id"] = resultID.String()
			payload["verifier_version"] = humanVerifierVersion
			payload["timestamp"] = now.Format(time.RFC3339)
			delete(payload, "feedback")
			delete(payload, outbox.FieldEventID)
			if eerr := outbox.EnqueueJSON(ctx, tx, parentID, bus.TopicVerificationComplete, parentID.String(), payload); eerr != nil {
				return fmt.Errorf("enqueue approve verification for task %s: %w", taskID, eerr)
			}
			log.Printf("handlers: work_id=%s task=%s: human review approved by %s — verification.complete re-enqueued as approve", parentID, taskID, userID)
			handled = true
		}
		if !handled {
			return nil
		}
		// Back to human_review (the prompt parked it in awaiting_user); the
		// orchestrator's restore of the same answer may have got there
		// first, which is the same state.
		terr := workgraph.TransitionFromAwaitingUserToStateTx(ctx, tx, parentID, models.WorkItemStateHumanReview, userID, "human review approved — verification approved for the escalated subtasks")
		if terr != nil && !strings.Contains(terr.Error(), "invalid transition: state is "+string(models.WorkItemStateHumanReview)) {
			return &transitionFailed{err: terr}
		}
		return nil
	})
	if err != nil {
		return handled, err
	}
	return handled, nil
}

// latestEscalatedVerifications is each subtask's newest verification.complete
// payload for parentID whose decision a person's approval overrides.
func latestEscalatedVerifications(ctx context.Context, q rowQuerier, parentID uuid.UUID) ([]map[string]any, error) {
	rows, err := q.Query(ctx, latestVerificationEventsSQL, bus.TopicVerificationComplete, parentID)
	if err != nil {
		return nil, fmt.Errorf("read verification events: %w", err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("scan verification event: %w", err)
		}
		var payload map[string]any
		if err := json.Unmarshal(raw, &payload); err != nil {
			return nil, fmt.Errorf("decode verification event: %w", err)
		}
		if escalatedDecisions[stringField(payload, "decision")] {
			out = append(out, payload)
		}
	}
	if rows.Err() != nil {
		return nil, fmt.Errorf("read verification events: %w", rows.Err())
	}
	return out, nil
}

// stringField is payload[key] as a string, "" when absent or not a string.
func stringField(payload map[string]any, key string) string {
	s, _ := payload[key].(string)
	return s
}
