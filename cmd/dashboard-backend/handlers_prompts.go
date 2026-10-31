// Package main — REST handlers for the unified interaction layer.
// Routes (all mounted under /api/):
//   GET    /api/prompts           — list pending (optional ?product_id=)
//   GET    /api/prompts/{id}      — fetch single
//   POST   /api/prompts/{id}/answer
//   POST   /api/prompts/{id}/revoke
package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/interactions"
	"github.com/google/uuid"
)

// PromptsStore is the narrow Store interface this handler needs. Narrow
// intentionally so unit tests don't have to stand up a real Postgres.
type PromptsStore interface {
	GetByID(ctx context.Context, id uuid.UUID) (*interactions.Prompt, error)
	ListPending(ctx context.Context, productID *uuid.UUID, limit int) ([]*interactions.Prompt, error)
	MarkAnswered(ctx context.Context, id uuid.UUID, ans interactions.AnswerInput) error
	MarkRevoked(ctx context.Context, id uuid.UUID, by, reason string) error
}

// PromptsPublisher is the narrow publisher contract.
type PromptsPublisher interface {
	PublishAnswered(ctx context.Context, a *interactions.AnsweredPayload) error
	PublishRevoked(ctx context.Context, r *interactions.RevokedPayload) error
}

// OnApprovalFunc is called after a PRD-approval prompt is answered with
// "approve" or "reject". It lets the generic prompt handler trigger the
// domain-specific side effects (state transition, Kafka publish) that live
// in the main Handlers struct.
type OnApprovalFunc func(ctx context.Context, workItemID uuid.UUID, action string, answerText string, userID string) error

// promptWSPusher is the WS broadcast interface the handlers need to push
// realtime events without importing the full WSHub.
type promptWSPusher interface {
	PushPromptEvent(eventType string, payload map[string]any)
	Broadcast(channel string, data interface{})
}

type PromptsHandlers struct {
	store               PromptsStore
	publisher           PromptsPublisher
	defaultUser         string
	onPRDAnswer         OnApprovalFunc
	onHumanReviewAnswer OnApprovalFunc
	onPRGateAnswer      OnApprovalFunc
	onBlockedAnswer     OnApprovalFunc
	ws                  promptWSPusher
}

func NewPromptsHandlers(s PromptsStore, p PromptsPublisher, defaultUser string) *PromptsHandlers {
	return &PromptsHandlers{store: s, publisher: p, defaultUser: defaultUser}
}

func (h *PromptsHandlers) SetOnPRDAnswer(fn OnApprovalFunc) {
	h.onPRDAnswer = fn
}

func (h *PromptsHandlers) SetOnHumanReviewAnswer(fn OnApprovalFunc) {
	h.onHumanReviewAnswer = fn
}

func (h *PromptsHandlers) SetOnPRGateAnswer(fn OnApprovalFunc) {
	h.onPRGateAnswer = fn
}

func (h *PromptsHandlers) SetOnBlockedAnswer(fn OnApprovalFunc) {
	h.onBlockedAnswer = fn
}

func (h *PromptsHandlers) SetWSHub(ws promptWSPusher) {
	h.ws = ws
}

func (h *PromptsHandlers) ListPrompts(w http.ResponseWriter, r *http.Request) {
	var productID *uuid.UUID
	if s := r.URL.Query().Get("product_id"); s != "" {
		id, err := uuid.Parse(s)
		if err != nil {
			http.Error(w, "invalid product_id", http.StatusBadRequest)
			return
		}
		productID = &id
	}
	list, err := h.store.ListPending(r.Context(), productID, 100)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(list)
}

func (h *PromptsHandlers) GetPrompt(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	p, err := h.store.GetByID(r.Context(), id)
	if errors.Is(err, interactions.ErrNotFound) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(p)
}

type answerRequest struct {
	ActionChosen   string                       `json:"action_chosen"`
	AnswerText     string                       `json:"answer_text"`
	SaveAsOverride *interactions.SaveAsOverride `json:"save_as_override,omitempty"`
}

func (h *PromptsHandlers) AnswerPrompt(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	var body answerRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	existing, err := h.store.GetByID(r.Context(), id)
	if errors.Is(err, interactions.ErrNotFound) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	ans := interactions.AnswerInput{
		AnsweredBy:     h.defaultUser,
		AnsweredVia:    "dashboard",
		ActionChosen:   body.ActionChosen,
		AnswerText:     body.AnswerText,
		SaveAsOverride: body.SaveAsOverride,
	}
	if err := h.store.MarkAnswered(r.Context(), id, ans); err != nil {
		if errors.Is(err, interactions.ErrNotFound) {
			http.Error(w, "already answered or revoked", http.StatusConflict)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := h.publisher.PublishAnswered(r.Context(), &interactions.AnsweredPayload{
		PromptID:       id,
		WorkItemID:     existing.WorkItemID,
		ProductID:      existing.ProductID,
		AskingAgent:    existing.AskingAgent,
		AnsweredBy:     ans.AnsweredBy,
		AnsweredVia:    ans.AnsweredVia,
		ActionChosen:   ans.ActionChosen,
		AnswerText:     ans.AnswerText,
		SaveAsOverride: ans.SaveAsOverride,
		AnsweredAt:     time.Now().UTC().Format(time.RFC3339),
		Question:       existing.Question,
	}); err != nil {
		//nolint:errcheck
		_ = err
	}

	if h.ws != nil {
		h.ws.PushPromptEvent("prompt_answered", map[string]any{
			"prompt_id":    id.String(),
			"work_item_id": existing.WorkItemID.String(),
			"action":       body.ActionChosen,
		})
	}

	var workflowErr error
	switch existing.Trigger {
	case interactions.TriggerApprovalPRD:
		if h.onPRDAnswer != nil {
			workflowErr = h.onPRDAnswer(r.Context(), existing.WorkItemID, body.ActionChosen, body.AnswerText, ans.AnsweredBy)
		}
	case interactions.TriggerApprovalHumanReview:
		if h.onHumanReviewAnswer != nil {
			workflowErr = h.onHumanReviewAnswer(r.Context(), existing.WorkItemID, body.ActionChosen, body.AnswerText, ans.AnsweredBy)
		}
	case interactions.TriggerApprovalPRGate:
		if h.onPRGateAnswer != nil {
			workflowErr = h.onPRGateAnswer(r.Context(), existing.WorkItemID, body.ActionChosen, body.AnswerText, ans.AnsweredBy)
		}
	case interactions.TriggerBlocked:
		if h.onBlockedAnswer != nil {
			workflowErr = h.onBlockedAnswer(r.Context(), existing.WorkItemID, body.ActionChosen, body.AnswerText, ans.AnsweredBy)
		}
	}
	if workflowErr != nil {
		http.Error(w, "prompt answered but workflow failed: "+workflowErr.Error(), http.StatusInternalServerError)
		return
	}
	if h.ws != nil {
		h.ws.Broadcast("pipeline", map[string]any{
			"work_item_id": existing.WorkItemID.String(),
			"action":       body.ActionChosen,
		})
	}

	w.WriteHeader(http.StatusOK)
}

type revokeRequest struct {
	Reason string `json:"reason"`
}

// revokeCodexTables limits the table-name parameter to the per-section
// tables the codex uses. Any value outside this set is a bad request
// (never SQL-injected).
var revokeCodexTables = map[string]bool{
	"codex_modules":       true,
	"codex_patterns":      true,
	"codex_anti_patterns": true,
	"codex_sections":      true,
}

type codexRevokeRequest struct {
	ID     string `json:"id"`
	Table  string `json:"table"`
	Reason string `json:"reason"`
}

func (h *PromptsHandlers) RevokePrompt(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	var body revokeRequest
	_ = json.NewDecoder(r.Body).Decode(&body)

	existing, err := h.store.GetByID(r.Context(), id)
	if errors.Is(err, interactions.ErrNotFound) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := h.store.MarkRevoked(r.Context(), id, h.defaultUser, body.Reason); err != nil {
		if errors.Is(err, interactions.ErrNotFound) {
			http.Error(w, "already revoked", http.StatusConflict)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_ = h.publisher.PublishRevoked(r.Context(), &interactions.RevokedPayload{
		PromptID:                id,
		WorkItemID:              existing.WorkItemID,
		CodexEntryIDToDeprecate: existing.CodexEntryID,
		RevokedBy:               h.defaultUser,
		RevocationReason:        body.Reason,
		RevokedAt:               time.Now().UTC().Format(time.RFC3339),
	})
	w.WriteHeader(http.StatusOK)
}

// interactionLookup is the narrow contract the codex-revoke path needs.
// interactions.Store satisfies it by adding FindByCodexEntry in Part A.3.
type interactionLookup interface {
	FindByCodexEntry(ctx context.Context, codexEntryID uuid.UUID, table string) (promptID, productID uuid.UUID, err error)
}

// RevokeCodexEntry handles POST /api/codex/entries/revoke. Body:
//
//	{"id": "<uuid>", "table": "codex_<section>", "reason": "<optional>"}
//
// It sets the entry state='revoked' immediately, finds the paired
// user_interactions row (if any) and marks it revoked too, then publishes
// user_prompt.revoked so the existing revoked-consumer pipeline bumps
// codex version + flips the qdrant point. The pipeline itself is
// idempotent; this endpoint is the single entry point for codex-side revokes.
func (h *PromptsHandlers) RevokeCodexEntry(w http.ResponseWriter, r *http.Request) {
	var body codexRevokeRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if !revokeCodexTables[body.Table] {
		http.Error(w, "invalid table", http.StatusBadRequest)
		return
	}
	codexEntryID, err := uuid.Parse(body.ID)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	// Let the revoked-consumer handle the full cascade (codex state flip
	// via SQL, version bump, qdrant flip). All we need to do here is:
	//   1. Find the paired interaction (if any) via codex_entry_id + codex_entry_table
	//      lookup — there's no dedicated store method, so query directly.
	//   2. Publish user_prompt.revoked with the codex entry pinned.
	// If no paired interaction exists (e.g., the entry was manually created
	// or auto-indexed), we still publish with a synthetic prompt_id so the
	// revoked-consumer codex-side logic fires. The consumer tolerates
	// missing interaction rows gracefully.
	promptID, productID, err := h.findInteractionForCodexEntry(r.Context(), codexEntryID, body.Table)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Mark the interaction revoked (best-effort — the interaction may be nil).
	if promptID != uuid.Nil {
		_ = h.store.MarkRevoked(r.Context(), promptID, h.defaultUser, body.Reason)
	}

	// Publish. Even if no interaction was found, the revoked-consumer does
	// codex state+version work based on CodexEntryIDToDeprecate.
	_ = h.publisher.PublishRevoked(r.Context(), &interactions.RevokedPayload{
		PromptID:                promptID, // uuid.Nil is acceptable
		WorkItemID:              productID, // Use productID in WorkItemID field for routing
		CodexEntryIDToDeprecate: &codexEntryID,
		RevokedBy:               h.defaultUser,
		RevocationReason:        body.Reason,
		RevokedAt:               time.Now().UTC().Format(time.RFC3339),
	})
	w.WriteHeader(http.StatusOK)
}

// findInteractionForCodexEntry looks up the paired user_interactions row
// via codex_entry_id + codex_entry_table (the polymorphic back-link from
// Task 1's schema). Returns uuid.Nil for both if no interaction exists —
// NOT an error; codex entries created outside the interaction flow are
// valid revoke targets.
func (h *PromptsHandlers) findInteractionForCodexEntry(ctx context.Context, codexEntryID uuid.UUID, table string) (uuid.UUID, uuid.UUID, error) {
	// The store interface is narrow — we need direct DB access. The
	// handler doesn't own a *pgxpool.Pool today; add a narrow method on
	// PromptsStore for this lookup. Meanwhile, inline via a new
	// interactionLookup interface that Store already satisfies.
	lk, ok := h.store.(interactionLookup)
	if !ok {
		return uuid.Nil, uuid.Nil, nil // degrade — revoke still useful without back-link
	}
	return lk.FindByCodexEntry(ctx, codexEntryID, table)
}
