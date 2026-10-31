package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/google/uuid"
)

// ─── Verification evidence ───────────────────────────────────────────────────
//
// M0-08: operators need to see the exact verified head SHA, the checks that
// ran, and skipped or partial states (successful evidence names the repository, SHAs, branch, changed files and every check; partial evidence names every skipped check).
// These handlers read the implementation_verifications table (migration 044)
// through internal/evidence and never reinterpret the outcome: what the
// producer recorded is what the dashboard shows.

// EvidenceStore is the narrow evidence.Store surface the handlers need. Narrow
// so unit tests can plug in a fake without a Postgres pool.
type EvidenceStore interface {
	GetByWorkItem(ctx context.Context, workID uuid.UUID) ([]evidence.Verification, error)
}

// verificationRow is the JSON shape of one implementation_verifications row.
// evidence.Verification has no JSON tags, so it is mapped explicitly here to
// keep the wire format snake_case like the rest of the API.
type verificationRow struct {
	ID        string                          `json:"id"`
	CreatedAt time.Time                       `json:"created_at"`
	Evidence  evidence.ImplementationEvidence `json:"evidence"`
}

// toVerificationRows maps store records to the API shape, preserving order
// (the store returns newest first).
func toVerificationRows(vs []evidence.Verification) []verificationRow {
	out := make([]verificationRow, 0, len(vs))
	for _, v := range vs {
		out = append(out, verificationRow{
			ID:        v.ID.String(),
			CreatedAt: v.CreatedAt,
			Evidence:  v.Evidence,
		})
	}
	return out
}

// ListWorkItemEvidence handles GET /api/work-items/evidence?id=<uuid>.
// Returns every verification recorded for the work item, newest first. An
// item with no evidence yields an empty array, not an error.
func (h *Handlers) ListWorkItemEvidence(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	idStr := r.URL.Query().Get("id")
	if idStr == "" {
		writeError(w, http.StatusBadRequest, "id parameter is required")
		return
	}
	id, err := uuid.Parse(idStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id format")
		return
	}

	if h.evidence == nil {
		log.Printf("handlers: list evidence for %s: evidence store not configured", id)
		writeError(w, http.StatusInternalServerError, "evidence store not configured")
		return
	}

	rows, err := h.evidence.GetByWorkItem(r.Context(), id)
	if err != nil {
		if errors.Is(err, evidence.ErrNotFound) {
			writeError(w, http.StatusNotFound, "work item not found")
			return
		}
		log.Printf("handlers: list evidence for %s: %v", id, err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	writeJSON(w, http.StatusOK, toVerificationRows(rows))
}

// latestEvidence returns the newest verification for the work item, or nil
// when none exists, the store is not configured, or the lookup fails. Failures
// are logged but never abort the caller: the work-item detail page must still
// render when the evidence table is unavailable.
func (h *Handlers) latestEvidence(ctx context.Context, id uuid.UUID) *verificationRow {
	if h.evidence == nil {
		return nil
	}
	rows, err := h.evidence.GetByWorkItem(ctx, id)
	if err != nil {
		if !errors.Is(err, evidence.ErrNotFound) {
			log.Printf("handlers: latest evidence for %s: %v", id, err)
		}
		return nil
	}
	if len(rows) == 0 {
		return nil
	}
	mapped := toVerificationRows(rows[:1])
	return &mapped[0]
}
