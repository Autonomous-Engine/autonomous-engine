package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/idempotency"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/outcomes"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
)

// changesSource is who requested changes: the LLM review (review.complete
// changes_requested, after the PR) or the independent verifier (R14,
// verification.complete request_changes, before any PR).
type changesSource struct {
	// topic names the event in logs.
	topic string
	// reason is the parent's transition reason back to implementation; the
	// circuit breaker counts every reason of every source.
	reason string
	// limitPrefix heads the human_review reason at the cycle limit.
	limitPrefix string
}

var (
	changesFromReview = changesSource{
		topic:       "review.complete",
		reason:      "review requested changes",
		limitPrefix: "review cycle limit reached",
	}
	changesFromVerification = changesSource{
		topic:       "verification.complete",
		reason:      "verification requested changes",
		limitPrefix: "fix-round limit reached after verification",
	}
)

// changeCycleReasons are the transition reasons the circuit breaker counts
// as one review→implementation cycle, whoever requested it.
var changeCycleReasons = map[string]bool{
	changesFromReview.reason:       true,
	changesFromVerification.reason: true,
}

// DefaultMaxChangeCycles is the circuit breaker's limit (R16 G4): at most
// this many fix rounds per parent, review and verifier requests together;
// the next request escalates to human_review with the defect history. It
// was MAX_RETRY_COUNT (shared with the QA-bug breaker) before R16; the
// default keeps that effective live limit (3) — no silent tightening.
const DefaultMaxChangeCycles = 3

// MaxChangeCyclesEnv overrides DefaultMaxChangeCycles (a positive integer).
const MaxChangeCyclesEnv = "MAX_CHANGE_CYCLES"

// maxChangeCycles reads MaxChangeCyclesEnv; unset, unparsable or below 1 is
// DefaultMaxChangeCycles.
func maxChangeCycles() int {
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv(MaxChangeCyclesEnv))); err == nil && n >= 1 {
		return n
	}
	return DefaultMaxChangeCycles
}

// fixSubtaskTitle is the title of every fix subtask requestChanges creates;
// the breaker's defect history reads them back by it.
const fixSubtaskTitle = "Address review feedback"

// Bounds of the defect history in the breaker's human-review prompt.
const (
	historyRoundMax = 1500
	historyMax      = 8000
)

// requestChanges sends the parent back to implementation with a fix subtask
// carrying feedback, or — after maxChangeCycles review/verification fix
// rounds — escalates it to human_review with the full defect history
// instead of looping forever. extra fields are added to the fix task
// message (the verifier's line_seeds, R16 G3); they never replace the
// fields set here.
func requestChanges(ctx context.Context, wg *workgraph.WorkGraph, kb bus.MessageBus, workID uuid.UUID, feedback string, src changesSource, extra map[string]interface{}) {
	limit := maxChangeCycles()
	transitions, tErr := wg.GetTransitions(ctx, workID)
	cycles := 0
	if tErr == nil {
		for _, t := range transitions {
			if t.ToState == models.WorkItemStateImplementation && changeCycleReasons[t.Reason] {
				cycles++
			}
		}
	}

	if cycles >= limit {
		log.Printf("status-orchestrator: %s: work_id=%s hit %d fix rounds (max %d) — escalating to human_review", src.topic, workID, cycles, limit)
		prompt := humanReviewPromptEffect(defectHistory(ctx, wg, workID, cycles, limit, feedback))
		if err := transitionAndPublish(ctx, wg.DB(), wg, kb, workID, models.WorkItemStateHumanReview, fmt.Sprintf("%s (%d cycles): %s", src.limitPrefix, cycles, capBytes(feedback, historyRoundMax)), prompt,
			outcomeEffect(outcomes.Facts{})); err != nil {
			log.Printf("status-orchestrator: %s: transition to human_review: %v", src.topic, err)
			// Legacy behaviour kept: the escalation prompt is raised even
			// when the transition failed.
			if perr := raisePromptAlone(ctx, wg.DB(), wg, workID, prompt); perr != nil {
				log.Printf("status-orchestrator: %s: raise human review prompt: %v", src.topic, perr)
			}
		}
		return
	}

	// M1-05: this transition is unguarded and the guard goes into the fix
	// subtask's transaction (dispatchFixOrBlock). The →implementation edge
	// cannot be taken twice, so a redelivery after a crash between the two
	// transactions fails this step (logged, as before) and still dispatches
	// the fix exactly once; with the guard here that redelivery would be a
	// duplicate and the fix would be lost. Exception: the circuit breaker
	// above counts the committed transition, so a redelivery after such a
	// crash can reach the limit and escalate to human_review (whose
	// transition from implementation fails, raising the prompt alone)
	// instead of dispatching the fix.
	if err := transitionAndPublish(idempotency.Unguarded(ctx), wg.DB(), wg, kb, workID, models.WorkItemStateImplementation, src.reason,
		outcomeEffect(outcomes.Facts{})); err != nil {
		log.Printf("status-orchestrator: %s: transition to implementation: %v", src.topic, err)
	}

	item, _ := wg.GetWorkItem(ctx, workID)
	repoID := ""
	var repoIDPtr *uuid.UUID
	var productIDPtr *uuid.UUID
	if item != nil {
		if item.RepositoryID != nil {
			repoID = item.RepositoryID.String()
			repoIDPtr = item.RepositoryID
		}
		productIDPtr = item.ProductID
	}
	if repoIDPtr == nil {
		if siblings, sErr := wg.GetChildren(ctx, workID); sErr == nil {
			for _, sib := range siblings {
				if sib.RepositoryID != nil {
					repoIDPtr = sib.RepositoryID
					repoID = sib.RepositoryID.String()
					break
				}
			}
		}
	}

	fixTaskID := uuid.New()
	fixItem := &models.WorkItem{
		ID:           fixTaskID,
		Type:         models.WorkItemTypeSubtask,
		State:        models.WorkItemStateImplementation,
		Title:        fixSubtaskTitle,
		Description:  feedback,
		Priority:     models.PriorityHigh,
		ParentID:     &workID,
		RepositoryID: repoIDPtr,
		ProductID:    productIDPtr,
	}
	feedbackMsg := map[string]interface{}{
		"task_id":         fixTaskID.String(),
		"work_id":         workID.String(),
		"repository_id":   repoID,
		"title":           fixSubtaskTitle,
		"description":     feedback,
		"review_feedback": feedback,
		"is_review_fix":   true,
	}
	for k, v := range extra {
		if _, set := feedbackMsg[k]; !set {
			feedbackMsg[k] = v
		}
	}
	dispatchFixOrBlock(ctx, wg.DB(), wg, kb, src.topic, workID, fixItem, feedbackMsg)
}

// defectHistory is the breaker's human-review prompt: why the automatic
// fixes stopped, every earlier fix round's feedback (the fix subtasks'
// descriptions, oldest first, with where each ended) and the request that
// was not dispatched. Each round is capped at historyRoundMax bytes and the
// whole at historyMax.
func defectHistory(ctx context.Context, wg *workgraph.WorkGraph, workID uuid.UUID, cycles, limit int, latest string) string {
	var rounds []*models.WorkItem
	if children, err := wg.GetChildren(ctx, workID); err != nil {
		log.Printf("status-orchestrator: defect history of %s: %v", workID, err)
	} else {
		for _, c := range children {
			if c.Title == fixSubtaskTitle {
				rounds = append(rounds, c)
			}
		}
	}
	sort.SliceStable(rounds, func(i, j int) bool { return rounds[i].CreatedAt.Before(rounds[j].CreatedAt) })

	var b strings.Builder
	fmt.Fprintf(&b, "Fix-round limit reached: %d automatic fix round(s) (limit %d, review and verifier together) did not resolve the requested changes. No further fix is dispatched; a human decides.\n\nDefect history, oldest first:\n", cycles, limit)
	latestSec := "\nLatest request (not dispatched):\n" + capBytes(strings.TrimSpace(latest), historyRoundMax) + "\n"
	for i, r := range rounds {
		sec := fmt.Sprintf("\nRound %d (fix subtask %s, now %s):\n%s\n", i+1, r.ID.String()[:8], r.State, capBytes(strings.TrimSpace(r.Description), historyRoundMax))
		if b.Len()+len(sec)+len(latestSec) > historyMax {
			fmt.Fprintf(&b, "\n… %d earlier round(s) not shown; see the fix subtasks.\n", len(rounds)-i)
			break
		}
		b.WriteString(sec)
	}
	if len(rounds) == 0 {
		b.WriteString("\n(no fix subtask recorded)\n")
	}
	b.WriteString(latestSec)
	return capBytes(strings.TrimRight(b.String(), "\n"), historyMax)
}
