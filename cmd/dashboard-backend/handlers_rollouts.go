package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/rollout"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ─── Shadow evaluation and rollout gates (M4-06) ─────────────────────────────
//
// GET  /api/admin/rollouts                       rollouts, newest first
// GET  /api/admin/rollouts/detail?id=            one rollout, its decision report and
//                                                the candidate's/baseline's production
//                                                pass rates per task class and repository
// POST /api/admin/rollouts/approve?id=           {override_reason?}
// POST /api/admin/rollouts/block?id=             {reason?}
// GET  /api/admin/rollouts/overrides             the override audit
// POST /api/admin/products/replay                {product_id, allow_replay}
//
// Every route is AdminOnly. Configuration-changing admin routes run the
// default-setting guard first (guardConfigChange).

func registerRolloutRoutes(mux *http.ServeMux, auth func(http.HandlerFunc) http.HandlerFunc, h *RolloutHandlers) {
	mux.HandleFunc("/api/admin/rollouts", auth(AdminOnly(h.List)))
	mux.HandleFunc("/api/admin/rollouts/detail", auth(AdminOnly(h.Detail)))
	mux.HandleFunc("/api/admin/rollouts/approve", auth(AdminOnly(h.Approve)))
	mux.HandleFunc("/api/admin/rollouts/block", auth(AdminOnly(h.Block)))
	mux.HandleFunc("/api/admin/rollouts/overrides", auth(AdminOnly(h.Overrides)))
	mux.HandleFunc("/api/admin/products/replay", auth(AdminOnly(h.SetProductReplay)))
}

// productScopeNote is shown for product-scoped rollouts.
const productScopeNote = "product-scoped rollouts cannot unlock the configuration guard (there is no product-level default); an approval only scopes replay to the product"

// RolloutHandlers serves the rollout admin API.
type RolloutHandlers struct {
	db    configDB
	store *rollout.Store
}

// NewRolloutHandlers returns the handlers over pool.
func NewRolloutHandlers(pool *pgxpool.Pool) *RolloutHandlers {
	return &RolloutHandlers{db: pool, store: rollout.NewStore(pool)}
}

func actorString(r *http.Request) string {
	if id := actorIDFromCtx(r); id != nil {
		return id.String()
	}
	return ""
}

// List handles GET /api/admin/rollouts.
func (h *RolloutHandlers) List(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	list, err := h.store.List(r.Context(), 200)
	if err != nil {
		log.Printf("rollouts: list: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to list rollouts")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rollouts": list})
}

// performanceRow is a production pass rate of one snapshot for one group.
type performanceRow struct {
	Group    string   `json:"group"`
	Items    int64    `json:"items"`
	Released int64    `json:"released"`
	Rate     *float64 `json:"rate"`
}

// snapshotPerformanceSQL groups a snapshot's production outcomes by a
// column of the top-level work item. "Verified" = released on verified
// evidence, as in the configuration-versions rollup. Shadow items are
// excluded. %s is a fixed expression (never user input).
const snapshotPerformanceSQL = `SELECT COALESCE(NULLIF(%s, ''), 'unknown') AS grp,
	count(*), count(*) FILTER (WHERE o.release_decision = 'release')
FROM work_item_outcomes o
JOIN work_items w ON w.id = o.work_item_id
LEFT JOIN repositories rp ON rp.id = w.repository_id
WHERE o.config_snapshot_id = $1
  AND NOT w.replay AND w.shadow_rollout_id IS NULL AND w.requested_config_snapshot_id IS NULL
GROUP BY 1 ORDER BY 1`

// performanceGroups are the groupings the rollout page shows. Production
// work items carry no language field; the repository stands in for it,
// and the benchmark report's per-language strata are in the decision.
var performanceGroups = map[string]string{
	"task_class": "w.type::text",
	"repository": "rp.name",
}

func (h *RolloutHandlers) performance(ctx context.Context, snapshotID string) (map[string][]performanceRow, error) {
	out := map[string][]performanceRow{}
	if snapshotID == "" {
		return out, nil
	}
	for name, expr := range performanceGroups {
		rows, err := h.db.Query(ctx, strings.Replace(snapshotPerformanceSQL, "%s", expr, 1), snapshotID)
		if err != nil {
			return nil, err
		}
		list := []performanceRow{}
		for rows.Next() {
			var p performanceRow
			if err := rows.Scan(&p.Group, &p.Items, &p.Released); err != nil {
				rows.Close()
				return nil, err
			}
			if p.Items > 0 {
				rate := float64(p.Released) / float64(p.Items)
				p.Rate = &rate
			}
			list = append(list, p)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		out[name] = list
	}
	return out, nil
}

// Detail handles GET /api/admin/rollouts/detail?id=.
func (h *RolloutHandlers) Detail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id, err := uuid.Parse(r.URL.Query().Get("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	ctx := r.Context()
	ro, err := h.store.Get(ctx, id)
	if errors.Is(err, rollout.ErrNotFound) {
		writeError(w, http.StatusNotFound, "rollout not found")
		return
	}
	if err != nil {
		log.Printf("rollouts: get: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to load rollout")
		return
	}
	resp := map[string]any{"rollout": ro, "verdict": ro.Verdict(), "not_evaluated": ro.NotEvaluated()}
	if ro.Scope == rollout.ScopeProduct {
		resp["scope_note"] = productScopeNote
	}
	for side, sid := range map[string]*string{"candidate": ro.CandidateSnapshotID, "baseline": ro.BaselineSnapshotID} {
		id := ""
		if sid != nil {
			id = *sid
		}
		perf, err := h.performance(ctx, id)
		if err != nil {
			log.Printf("rollouts: performance %s: %v", side, err)
			writeError(w, http.StatusInternalServerError, "failed to load production pass rates")
			return
		}
		resp[side+"_performance"] = perf
	}
	writeJSON(w, http.StatusOK, resp)
}

type rolloutDecisionRequest struct {
	OverrideReason string `json:"override_reason"`
	Reason         string `json:"reason"`
}

func decodeOptional(r *http.Request, v any) error {
	if r.Body == nil || r.ContentLength == 0 {
		return nil
	}
	err := json.NewDecoder(r.Body).Decode(v)
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

// Approve handles POST /api/admin/rollouts/approve?id=. Without an
// APPROVED decision it needs override_reason (audited).
func (h *RolloutHandlers) Approve(w http.ResponseWriter, r *http.Request) {
	h.decide(w, r, true)
}

// Block handles POST /api/admin/rollouts/block?id=.
func (h *RolloutHandlers) Block(w http.ResponseWriter, r *http.Request) {
	h.decide(w, r, false)
}

func (h *RolloutHandlers) decide(w http.ResponseWriter, r *http.Request, approve bool) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id, err := uuid.Parse(r.URL.Query().Get("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	var req rolloutDecisionRequest
	if err := decodeOptional(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	var ro *rollout.Rollout
	if approve {
		ro, err = h.store.Approve(r.Context(), id, actorString(r), req.OverrideReason)
	} else {
		ro, err = h.store.Block(r.Context(), id, actorString(r), req.Reason)
	}
	switch {
	case errors.Is(err, rollout.ErrNotFound):
		writeError(w, http.StatusNotFound, "rollout not found")
	case errors.Is(err, rollout.ErrNotApprovable):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error(), "code": "override_required"})
	case errors.Is(err, rollout.ErrBadTransition):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error(), "code": "bad_transition"})
	case err != nil:
		log.Printf("rollouts: decide: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to record the decision")
	default:
		writeJSON(w, http.StatusOK, ro)
	}
}

// Overrides handles GET /api/admin/rollouts/overrides.
func (h *RolloutHandlers) Overrides(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	list, err := h.store.ListOverrides(r.Context(), 200)
	if err != nil {
		log.Printf("rollouts: overrides: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to list overrides")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"overrides": list})
}

type productReplayRequest struct {
	ProductID        string `json:"product_id"`
	AllowReplay      *bool  `json:"allow_replay"`
	BenchmarkScratch *bool  `json:"benchmark_scratch"`
}

// SetProductReplay handles POST /api/admin/products/replay: the per-product
// shadow settings — allow_replay (opt-in to having recent items replayed,
// default off) and benchmark_scratch (the product is a scratch target that
// shadow runs and replays may use, default off). Omitted fields are left
// alone. GET lists every product's flags.
func (h *RolloutHandlers) SetProductReplay(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		rows, err := h.db.Query(r.Context(), `SELECT id, name, allow_replay, benchmark_scratch FROM products ORDER BY name`)
		if err != nil {
			log.Printf("rollouts: list replay flags: %v", err)
			writeError(w, http.StatusInternalServerError, "failed to list products")
			return
		}
		defer rows.Close()
		type productReplay struct {
			ID               uuid.UUID `json:"id"`
			Name             string    `json:"name"`
			AllowReplay      bool      `json:"allow_replay"`
			BenchmarkScratch bool      `json:"benchmark_scratch"`
		}
		list := []productReplay{}
		for rows.Next() {
			var p productReplay
			if err := rows.Scan(&p.ID, &p.Name, &p.AllowReplay, &p.BenchmarkScratch); err != nil {
				writeError(w, http.StatusInternalServerError, "failed to list products")
				return
			}
			list = append(list, p)
		}
		writeJSON(w, http.StatusOK, map[string]any{"products": list})
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req productReplayRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	id, err := uuid.Parse(req.ProductID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid product_id")
		return
	}
	if req.AllowReplay == nil && req.BenchmarkScratch == nil {
		writeError(w, http.StatusBadRequest, "give allow_replay and/or benchmark_scratch")
		return
	}
	var allow, scratch bool
	err = h.db.QueryRow(r.Context(), `UPDATE products SET
			allow_replay = COALESCE($2, allow_replay),
			benchmark_scratch = COALESCE($3, benchmark_scratch),
			updated_at = NOW()
		WHERE id = $1 RETURNING allow_replay, benchmark_scratch`, id, req.AllowReplay, req.BenchmarkScratch).Scan(&allow, &scratch)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "product not found")
		return
	}
	if err != nil {
		log.Printf("rollouts: set product shadow flags: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to update product")
		return
	}
	log.Printf("rollouts: product %s allow_replay=%t benchmark_scratch=%t (by %s)", id, allow, scratch, actorString(r))
	writeJSON(w, http.StatusOK, map[string]any{"product_id": id, "allow_replay": allow, "benchmark_scratch": scratch})
}

// ─── Default-setting guard ───────────────────────────────────────────────────

// overrideReasonFrom reads the guard override reason: the override_reason
// query parameter (every guarded route; their bodies are the resource) or
// the X-Override-Reason header.
func overrideReasonFrom(r *http.Request) string {
	if v := strings.TrimSpace(r.URL.Query().Get("override_reason")); v != "" {
		return v
	}
	return strings.TrimSpace(r.Header.Get("X-Override-Reason"))
}

// guardBeginner is the pool surface the guard needs.
type guardBeginner interface {
	rollout.Beginner
	rollout.DB
}

// statusError is an apply error with the HTTP status the handler would
// have answered (404 for a missing row, …).
type statusError struct {
	status int
	msg    string
}

func (e *statusError) Error() string { return e.msg }

func notFound(msg string) error { return &statusError{status: http.StatusNotFound, msg: msg} }

// guardConfigChange performs a configuration change under the M4-06
// default-setting guard: apply is the REAL write, run by
// rollout.ApplyGuarded in one transaction under an advisory lock with the
// before/after capture and any override audit row. It returns true when the
// change committed; otherwise it has written the response (409
// rollout_required with the unapproved snapshot changes, the apply error's
// status, or 500) and nothing was changed.
func guardConfigChange(w http.ResponseWriter, r *http.Request, db guardBeginner, action string, details map[string]any, apply func(ctx context.Context, tx pgx.Tx) error) bool {
	return guardedChange(w, r, db, rollout.DefaultChange{Action: action, Apply: apply, Details: details})
}

// guardedChange is guardConfigChange for a prepared change (an overlay for
// a credential write).
func guardedChange(w http.ResponseWriter, r *http.Request, db guardBeginner, ch rollout.DefaultChange) bool {
	action := ch.Action
	if db == nil {
		writeError(w, http.StatusInternalServerError, "configuration guard unavailable")
		return false
	}
	ch.OverrideReason, ch.Actor = overrideReasonFrom(r), actorString(r)
	res, err := rollout.ApplyGuarded(r.Context(), db, ch)
	if err != nil {
		var se *statusError
		if errors.As(err, &se) {
			writeError(w, se.status, se.msg)
			return false
		}
		var refused *rollout.RefusedError
		if errors.As(err, &refused) {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":      err.Error(),
				"code":       "rollout_required",
				"action":     action,
				"unapproved": refused.Unapproved,
				"hint":       "approve a rollout for the new snapshot (rolloutctl propose/shadow/evaluate/approve), or retry with ?override_reason=<why> (audited)",
			})
			return false
		}
		log.Printf("config guard (%s): %v", action, err)
		writeError(w, http.StatusInternalServerError, "configuration change failed (nothing changed): "+err.Error())
		return false
	}
	if res.Reason != rollout.GuardUnchanged {
		log.Printf("config guard (%s): allowed (%s) %d change(s) at %s", action, res.Reason, len(res.Changes), time.Now().UTC().Format(time.RFC3339))
	}
	return true
}
