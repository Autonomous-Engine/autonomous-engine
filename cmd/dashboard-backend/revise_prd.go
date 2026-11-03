package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/interactions"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/google/uuid"
)

// prdRevisionPayload is the intake event that makes the product agent
// regenerate the PRD against the reviewer's feedback. Shared by RevisePRD
// (item in prd_approval) and the prompt-answer path (item parked in
// awaiting_user while the approval prompt is pending — rerun NEW-5).
func prdRevisionPayload(id uuid.UUID, item *models.WorkItem, feedback string) map[string]interface{} {
	productID := ""
	if item.ProductID != nil {
		productID = item.ProductID.String()
	}
	return map[string]interface{}{
		"work_id":     id.String(),
		"product_id":  productID,
		"description": item.Description,
		"source":      "dashboard",
		"metadata":    map[string]interface{}{"user_guidance": feedback, "revision": true},
	}
}

// RevisePRD handles POST /api/work-items/revise-prd?id=<uuid> with body
// {"reason": "<feedback>"} ("feedback" is accepted too). A PRD awaiting
// approval goes back through intake with the feedback in
// metadata.user_guidance, so the product agent regenerates it. Before
// this, review offered Approve or Reject only, and Reject was terminal
// (F34).
func (h *Handlers) RevisePRD(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id, err := uuid.Parse(r.URL.Query().Get("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	var body struct {
		Reason   string `json:"reason"`
		Feedback string `json:"feedback"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	feedback := strings.TrimSpace(body.Reason)
	if feedback == "" {
		feedback = strings.TrimSpace(body.Feedback)
	}
	if feedback == "" {
		writeError(w, http.StatusBadRequest, "say what should change: the feedback is what the product agent revises against")
		return
	}
	if h.approvals == nil {
		writeError(w, http.StatusInternalServerError, "work graph unavailable")
		return
	}

	item, err := h.approvals.GetWorkItem(r.Context(), id)
	if err != nil || item == nil {
		writeError(w, http.StatusNotFound, "work item not found")
		return
	}
	if item.State != models.WorkItemStatePRDApproval {
		writeError(w, http.StatusConflict, fmt.Sprintf("work item is in state %q, not prd_approval", item.State))
		return
	}

	userID, _ := r.Context().Value(contextKeyUserID).(string)
	if userID == "" {
		userID = "dashboard"
	}
	payload := prdRevisionPayload(id, item, feedback)
	// prd_approval → prd is the state machine's "admin requested changes →
	// regenerate" edge; the intake event makes the product agent run again.
	reason := "PRD changes requested: " + feedback
	if err := h.transitionAndEnqueue(r.Context(), id, item, models.WorkItemStatePRD, userID, reason, bus.TopicIntake, id.String(), payload); err != nil {
		log.Printf("handlers: revise-prd %s: %v", id, err)
		writeTransitionError(w, err)
		return
	}

	// The paired approval prompt is answered as "revise" so the bell and
	// approvals list stay in sync (best effort, as in ApprovePRD).
	if h.interactionsStore != nil && h.interactionsPublisher != nil {
		if promptID, err := h.interactionsStore.FindPendingApprovalPromptForWorkItem(r.Context(), id); err == nil {
			_ = h.interactionsStore.MarkAnswered(r.Context(), promptID, interactions.AnswerInput{
				AnsweredBy: userID, AnsweredVia: "dashboard", ActionChosen: "revise", AnswerText: feedback,
			})
			pID := uuid.Nil
			if item.ProductID != nil {
				pID = *item.ProductID
			}
			_ = h.interactionsPublisher.PublishAnswered(r.Context(), &interactions.AnsweredPayload{
				PromptID: promptID, WorkItemID: id, ProductID: pID, AskingAgent: "product-agent",
				AnsweredBy: userID, AnsweredVia: "dashboard", ActionChosen: "revise", AnswerText: feedback,
				AnsweredAt: time.Now().UTC().Format(time.RFC3339),
			})
		}
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "revision_requested", "work_item_id": id.String()})
}
