package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/autonomousengine/autonomous-engine/internal/gitremote"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/google/uuid"
)

// ─── Gate approval: repositories without a GitHub remote (G1) ────────────────
//
// pr-coordinator gates every change of a repository whose remote is not on
// github.com (a local or bare mirror, file://, another host). There is no PR
// to open for such a change: github-service answers 409. Both approval paths
// check the remote before marking the row approved, so the row stays pending
// and the operator can merge the branch by hand and then reject it; and if
// github-service still answers 409, the approval is put back to pending.

// noGitHubRemoteError: the pending change's repository has no GitHub remote.
type noGitHubRemoteError struct {
	kind   gitremote.Kind
	branch string
}

func (e *noGitHubRemoteError) Error() string {
	return fmt.Sprintf("repository has no GitHub remote (remote kind %s): no PR can be opened; merge branch %s by hand, then reject this pending change", e.kind, e.branch)
}

// pendingChangeRemoteCheck returns a *noGitHubRemoteError when the change's
// repository remote is not on github.com. A repository that cannot be read
// is not an error here: github-service decides, as before.
func pendingChangeRemoteCheck(ctx context.Context, q pgxRowQuerier, pc pendingPRChange) error {
	var u string
	if err := q.QueryRow(ctx, `SELECT github_url FROM repositories WHERE id = $1`, pc.RepositoryID).Scan(&u); err != nil {
		return nil
	}
	if k, _ := gitremote.Classify(u); !k.IsGitHub() {
		return &noGitHubRemoteError{kind: k, branch: pc.Branch}
	}
	return nil
}

// pgxExecer is the slice of *pgxpool.Pool revertApproval needs.
type pgxExecer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// revertApproval puts an approved change back to pending after github-service
// refused to open its PR because the repository has no GitHub remote.
// id is the row's uuid.UUID or its string form.
func revertApproval(ctx context.Context, db pgxExecer, id any) {
	if _, err := db.Exec(ctx, `UPDATE pending_pr_changes SET status = 'pending', approved_by = NULL, approved_at = NULL WHERE id = $1 AND status = 'approved'`, id); err != nil {
		log.Printf("handlers: approve-pr: put %s back to pending: %v", id, err)
	}
}

// ─── Gate approval: forwarding the verified head SHA ─────────────────────────
//
// pr-coordinator records the verified head SHA (the remotely resolvable SHA handed to review and PR services)
// with each gated change (pending_pr_changes.head_sha, migration 046) and
// marks a change opened on partially verified evidence in the stored title
// and body. When a person approves the change, the pr.ready message must
// carry both: under EVIDENCE_COMPAT_WINDOW=strict the review-agent refuses
// a pr.ready without head_sha, and the SHA is what ties the review to the
// commit that was actually verified. Both approval paths (ApprovePendingChange
// and HandlePRGatePromptAnswer) go through loadPendingPRChange and
// commitGatedPRReady (approvals_outbox.go) so they cannot drift apart again.

// partialVerificationTitlePrefix and partialVerificationBodyMarker are the
// stable D2/D3 markers cmd/pr-coordinator writes into a pending change's
// pr_title and pr_description (its partialVerificationTitlePrefix and
// partialVerificationBodyMarker). pending_pr_changes has no column for the
// flag; the stored title and body are its persisted record, and the
// approval paths re-use them verbatim, so this is what the approver saw.
// Keep the text identical to pr-coordinator's.
const (
	partialVerificationTitlePrefix = "[PARTIALLY VERIFIED — human review required] "
	partialVerificationBodyMarker  = "PARTIALLY VERIFIED — human review required"
)

// pgxRowQuerier is the slice of *pgxpool.Pool loadPendingPRChange needs, so
// the approval read can be tested against a fake.
type pgxRowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// pendingPRChange is the pending_pr_changes row an approval path acts on.
type pendingPRChange struct {
	ID            string
	WorkItemID    string
	RepositoryID  string
	Branch        string
	BaseBranch    string
	PRTitle       string
	PRDescription string
	Status        string
	FilesChanged  []string
	// HeadSHA is the verified head SHA pr-coordinator stored with the
	// change (migration 046). Empty for a row written before 046, by a
	// legacy producer, or when the column does not exist yet.
	HeadSHA string
}

// partiallyVerified reports whether the change was opened on partially
// verified evidence, from the markers pr-coordinator stored in its title
// and body. A false positive can only make review stricter.
func (pc pendingPRChange) partiallyVerified() bool {
	return strings.HasPrefix(pc.PRTitle, partialVerificationTitlePrefix) ||
		strings.Contains(pc.PRDescription, partialVerificationBodyMarker)
}

const (
	pendingChangeColumns = `id, work_item_id, repository_id, branch, base_branch, pr_title, COALESCE(pr_description, ''), status, files_changed`
	// pendingChangeByWorkItem selects the newest pending change for a work
	// item (the pr_gate prompt answer path); pendingChangeByID selects one
	// change by id (the dashboard approve-pr path).
	pendingChangeByWorkItem = `WHERE work_item_id = $1 AND status = 'pending' ORDER BY created_at DESC LIMIT 1`
	pendingChangeByID       = `WHERE id = $1`
)

// loadPendingPRChange reads one pending_pr_changes row, including its
// stored head SHA. tail is one of the pendingChangeBy* clauses with $1 =
// arg. If the head_sha column does not exist (this service deployed ahead
// of the status-orchestrator that applies migration 046) it retries with
// the pre-046 column set so an approval is never blocked by deploy
// ordering; the SHA is then simply absent and the agents apply their
// compatibility window. Any other error is returned as is.
func loadPendingPRChange(ctx context.Context, q pgxRowQuerier, tail string, arg any) (pendingPRChange, error) {
	var pc pendingPRChange
	err := q.QueryRow(ctx, "SELECT "+pendingChangeColumns+", COALESCE(head_sha, '') FROM pending_pr_changes "+tail, arg).
		Scan(&pc.ID, &pc.WorkItemID, &pc.RepositoryID, &pc.Branch, &pc.BaseBranch, &pc.PRTitle, &pc.PRDescription, &pc.Status, &pc.FilesChanged, &pc.HeadSHA)
	var pgErr *pgconn.PgError
	if err != nil && errors.As(err, &pgErr) && pgErr.Code == pgUndefinedColumn {
		log.Printf("handlers: pending_pr_changes.head_sha column missing (migration 046 not applied yet) — approving without a verified head SHA")
		pc = pendingPRChange{}
		err = q.QueryRow(ctx, "SELECT "+pendingChangeColumns+" FROM pending_pr_changes "+tail, arg).
			Scan(&pc.ID, &pc.WorkItemID, &pc.RepositoryID, &pc.Branch, &pc.BaseBranch, &pc.PRTitle, &pc.PRDescription, &pc.Status, &pc.FilesChanged)
	}
	if err != nil {
		return pendingPRChange{}, err
	}
	return pc, nil
}

// gatedPRReadyMessage is the pr.ready payload for a change a person approved
// through the gate. It mirrors cmd/pr-coordinator's prReadyMessage field for
// field, including the additive head_sha and partially_verified that
// review-agent reads.
type gatedPRReadyMessage struct {
	WorkID        string   `json:"work_id"`
	ProductID     string   `json:"product_id,omitempty"`
	RepositoryID  string   `json:"repository_id,omitempty"`
	PRNumber      int      `json:"pr_number,omitempty"`
	PRURL         string   `json:"pr_url,omitempty"`
	PRDescription string   `json:"pr_description"`
	Branch        string   `json:"branch"`
	FilesChanged  []string `json:"files_changed"`
	Timestamp     string   `json:"timestamp"`

	// HeadSHA is the verified head SHA stored with the pending change
	// (D1.5); empty for a legacy row. Additive.
	HeadSHA string `json:"head_sha,omitempty"`
	// PartiallyVerified is true when the change was opened on partially
	// verified evidence (D2, D3). Additive.
	PartiallyVerified bool `json:"partially_verified,omitempty"`
}

// ─── PR Gate Handlers ────────────────────────────────────────────────────────
//
// These endpoints let dashboard users gate PR creation. Instead of auto-creating
// PRs after implementation, changes accumulate in a "pending" state so users can
// review and approve them.

// pendingChangeRow is the JSON shape returned when listing pending PR changes.
type pendingChangeRow struct {
	ID            string   `json:"id"`
	WorkItemID    string   `json:"work_item_id"`
	WorkItemTitle string   `json:"work_item_title"`
	RepositoryID  string   `json:"repository_id"`
	RepoName      string   `json:"repo_name"`
	Branch        string   `json:"branch"`
	BaseBranch    string   `json:"base_branch"`
	FilesChanged  []string `json:"files_changed"`
	DiffStats     string   `json:"diff_stats"`
	PRTitle       string   `json:"pr_title"`
	PRDescription string   `json:"pr_description,omitempty"`
	Status        string   `json:"status"`
	ApprovedBy    *string  `json:"approved_by,omitempty"`
	ApprovedAt    *string  `json:"approved_at,omitempty"`
	CreatedAt     string   `json:"created_at"`
	// HeadSHA and Verification (R14, V5) are set on the single-change
	// detail: the verified head and the independent verifier's decision for
	// it, recorded before the change was gated, so the gate card shows the
	// verdict the operator decides with.
	HeadSHA      string               `json:"head_sha,omitempty"`
	Verification *pendingVerification `json:"verification,omitempty"`
}

// pendingVerification is the verification_results row for a pending
// change's head, with the verdict artifact's content (the
// review_comments/verifier shape the dashboard's VerifierVerdict reads).
type pendingVerification struct {
	Decision   string          `json:"decision"`
	Reason     string          `json:"reason"`
	RecordedAt string          `json:"recorded_at"`
	Verdict    json.RawMessage `json:"verdict,omitempty"`
}

// loadPendingVerification reads the newest verification result recorded for
// the work item at headSHA (nil when there is none or headSHA is empty; a
// read error is logged and yields nil — the PR description still carries the
// "## Verification" section).
func loadPendingVerification(ctx context.Context, q pgxRowQuerier, workItemID, headSHA string) *pendingVerification {
	if headSHA == "" {
		return nil
	}
	var v pendingVerification
	var at time.Time
	var verdict []byte
	err := q.QueryRow(ctx, `SELECT r.decision, r.reason, r.created_at, a.content
		FROM verification_results r
		LEFT JOIN work_item_artifacts a ON a.id = r.verdict_artifact_id
		WHERE r.work_item_id = $1 AND r.head_sha = $2
		ORDER BY r.created_at DESC LIMIT 1`, workItemID, headSHA).Scan(&v.Decision, &v.Reason, &at, &verdict)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			log.Printf("handlers: verification of pending change (work item %s, head %s): %v", workItemID, headSHA, err)
		}
		return nil
	}
	v.RecordedAt = at.Format(time.RFC3339)
	if len(verdict) > 0 {
		v.Verdict = verdict
	}
	return &v
}

// ListPendingChanges handles GET /api/work-items/pending-changes.
// Optional filters: ?status=pending (default shows all), ?product_id=UUID.
func (h *Handlers) ListPendingChanges(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	statusFilter := r.URL.Query().Get("status")
	productIDFilter := r.URL.Query().Get("product_id")

	query := `
		SELECT ppc.id, ppc.work_item_id, COALESCE(wi.title, ''),
		       ppc.repository_id, COALESCE(r.name, ''),
		       ppc.branch, ppc.base_branch, ppc.files_changed,
		       COALESCE(ppc.diff_stats, ''), COALESCE(ppc.pr_title, ''),
		       ppc.status, ppc.created_at
		FROM pending_pr_changes ppc
		LEFT JOIN work_items wi ON wi.id = ppc.work_item_id
		LEFT JOIN repositories r ON r.id = ppc.repository_id
		WHERE 1=1`

	args := []interface{}{}
	argIdx := 1

	if statusFilter != "" {
		query += fmt.Sprintf(" AND ppc.status = $%d", argIdx)
		args = append(args, statusFilter)
		argIdx++
	}
	if productIDFilter != "" {
		pid, err := uuid.Parse(productIDFilter)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid product_id")
			return
		}
		query += fmt.Sprintf(" AND wi.product_id = $%d", argIdx)
		args = append(args, pid)
		argIdx++
	}

	query += " ORDER BY ppc.created_at DESC"

	rows, err := h.wg.DB().Query(r.Context(), query, args...)
	if err != nil {
		log.Printf("handlers: list pending changes: %v", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	defer rows.Close()

	result := []pendingChangeRow{}
	for rows.Next() {
		var row pendingChangeRow
		var createdAt time.Time
		if err := rows.Scan(
			&row.ID, &row.WorkItemID, &row.WorkItemTitle,
			&row.RepositoryID, &row.RepoName,
			&row.Branch, &row.BaseBranch, &row.FilesChanged,
			&row.DiffStats, &row.PRTitle,
			&row.Status, &createdAt,
		); err != nil {
			log.Printf("handlers: scan pending change: %v", err)
			continue
		}
		row.CreatedAt = createdAt.Format(time.RFC3339)
		result = append(result, row)
	}

	writeJSON(w, http.StatusOK, result)
}

// GetPendingChange handles GET /api/work-items/pending-change?id=UUID.
// Returns a single pending change with full pr_description.
func (h *Handlers) GetPendingChange(w http.ResponseWriter, r *http.Request) {
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

	var row pendingChangeRow
	var createdAt time.Time
	var approvedBy *string
	var approvedAt *time.Time

	err = h.wg.DB().QueryRow(r.Context(), `
		SELECT ppc.id, ppc.work_item_id, COALESCE(wi.title, ''),
		       ppc.repository_id, COALESCE(r.name, ''),
		       ppc.branch, ppc.base_branch, ppc.files_changed,
		       COALESCE(ppc.diff_stats, ''), COALESCE(ppc.pr_title, ''),
		       COALESCE(ppc.pr_description, ''),
		       ppc.status, ppc.approved_by::text, ppc.approved_at, ppc.created_at
		FROM pending_pr_changes ppc
		LEFT JOIN work_items wi ON wi.id = ppc.work_item_id
		LEFT JOIN repositories r ON r.id = ppc.repository_id
		WHERE ppc.id = $1`, id,
	).Scan(
		&row.ID, &row.WorkItemID, &row.WorkItemTitle,
		&row.RepositoryID, &row.RepoName,
		&row.Branch, &row.BaseBranch, &row.FilesChanged,
		&row.DiffStats, &row.PRTitle, &row.PRDescription,
		&row.Status, &approvedBy, &approvedAt, &createdAt,
	)
	if err != nil {
		log.Printf("handlers: get pending change %s: %v", id, err)
		writeError(w, http.StatusNotFound, "pending change not found")
		return
	}

	row.CreatedAt = createdAt.Format(time.RFC3339)
	row.ApprovedBy = approvedBy
	if approvedAt != nil {
		s := approvedAt.Format(time.RFC3339)
		row.ApprovedAt = &s
	}
	// V5: the gate card shows the verdict recorded for the gated head.
	if pc, perr := loadPendingPRChange(r.Context(), h.wg.DB(), pendingChangeByID, id); perr == nil {
		row.HeadSHA = pc.HeadSHA
		row.Verification = loadPendingVerification(r.Context(), h.wg.DB(), row.WorkItemID, pc.HeadSHA)
	}

	writeJSON(w, http.StatusOK, row)
}

// ApprovePendingChange handles POST /api/work-items/approve-pr.
// Approves a pending change, calls github-service to create the PR, and
// publishes a pr.ready event via Kafka.
func (h *Handlers) ApprovePendingChange(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	id, err := uuid.Parse(body.ID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id format")
		return
	}

	userID, _ := r.Context().Value(contextKeyUserID).(string)
	if userID == "" {
		writeError(w, http.StatusUnauthorized, "user_id not found in context")
		return
	}

	// Fetch the pending change record, including the verified head SHA
	// pr-coordinator stored with it.
	pc, err := loadPendingPRChange(r.Context(), h.wg.DB(), pendingChangeByID, id)
	if err != nil {
		log.Printf("handlers: approve-pr lookup %s: %v", id, err)
		writeError(w, http.StatusNotFound, "pending change not found")
		return
	}
	workItemID := pc.WorkItemID

	if pc.Status != "pending" {
		writeError(w, http.StatusConflict, fmt.Sprintf("change is already %q, not pending", pc.Status))
		return
	}
	// G1: no PR for a repository without a GitHub remote; the row stays
	// pending so it can be rejected after a manual merge.
	if err := pendingChangeRemoteCheck(r.Context(), h.wg.DB(), pc); err != nil {
		log.Printf("handlers: approve-pr %s: %v", id, err)
		writeError(w, http.StatusConflict, err.Error())
		return
	}

	// Mark as approved in the database.
	_, err = h.wg.DB().Exec(r.Context(), `
		UPDATE pending_pr_changes
		SET status = 'approved', approved_by = $1, approved_at = NOW()
		WHERE id = $2`, userID, id)
	if err != nil {
		log.Printf("handlers: approve-pr update %s: %v", id, err)
		writeError(w, http.StatusInternalServerError, "failed to update status")
		return
	}

	// Call github-service to create the PR.
	githubServiceURL := os.Getenv("GITHUB_SERVICE_URL")
	if githubServiceURL == "" {
		githubServiceURL = "http://github-service:8085"
	}

	createPRReq := struct {
		RepositoryID string `json:"repository_id"`
		Title        string `json:"title"`
		Body         string `json:"body"`
		Head         string `json:"head"`
		Base         string `json:"base"`
	}{
		RepositoryID: pc.RepositoryID,
		Title:        pc.PRTitle,
		Body:         pc.PRDescription,
		Head:         pc.Branch,
		Base:         pc.BaseBranch,
	}

	reqBody, _ := json.Marshal(createPRReq)
	resp, err := http.Post(githubServiceURL+"/pr", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		log.Printf("handlers: approve-pr github-service call: %v", err)
		writeError(w, http.StatusBadGateway, "failed to reach github-service: "+err.Error())
		return
	}
	defer resp.Body.Close()

	var prResp struct {
		Success  bool   `json:"success"`
		PRURL    string `json:"pr_url,omitempty"`
		PRNumber int    `json:"pr_number,omitempty"`
		Message  string `json:"message,omitempty"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&prResp); err != nil {
		log.Printf("handlers: approve-pr decode github-service response: %v", err)
		writeError(w, http.StatusBadGateway, "invalid response from github-service")
		return
	}
	if !prResp.Success {
		log.Printf("handlers: approve-pr github-service failed: %s", prResp.Message)
		status := http.StatusBadGateway
		if resp.StatusCode == http.StatusConflict {
			// G1: the repository has no GitHub remote; the branch is
			// merged by hand, there is no PR to open. The row goes back
			// to pending so it can still be rejected.
			status = http.StatusConflict
			revertApproval(r.Context(), h.wg.DB(), id)
		}
		writeError(w, status, "github-service: "+prResp.Message)
		return
	}

	// Look up product_id from the work item for the pr.ready message.
	var productID string
	_ = h.wg.DB().QueryRow(r.Context(),
		`SELECT COALESCE(product_id::text, '') FROM work_items WHERE id = $1`, workItemID,
	).Scan(&productID)

	// Enqueue pr.ready with the stored verified head SHA (D1.5) and the
	// partial-verification marker, so review-agent examines the verified
	// commit and does not block the approval under strict mode. The PR
	// exists now and the GitHub call cannot be in a transaction, so this is
	// the event's own transaction; if it fails, the PR has no pr.ready: say
	// so loudly with its URL and fail the request.
	if err := commitGatedPRReady(r.Context(), h.txdb, pc, productID, prResp.PRNumber, prResp.PRURL, time.Now()); err != nil {
		log.Printf("handlers: ERROR approve-pr: PR %s (#%d) was created for work_item=%s but pr.ready was NOT recorded: %v", prResp.PRURL, prResp.PRNumber, workItemID, err)
		writeError(w, http.StatusInternalServerError, "PR "+prResp.PRURL+" was created but pr.ready could not be recorded: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status": "approved",
		"pr_url": prResp.PRURL,
	})
}

// RejectPendingChange handles POST /api/work-items/reject-pr.
// Rejects a pending change and optionally transitions the work item back.
func (h *Handlers) RejectPendingChange(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var body struct {
		ID     string `json:"id"`
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	id, err := uuid.Parse(body.ID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id format")
		return
	}

	// Verify it is still pending.
	var status, workItemID string
	err = h.wg.DB().QueryRow(r.Context(),
		`SELECT status, work_item_id FROM pending_pr_changes WHERE id = $1`, id,
	).Scan(&status, &workItemID)
	if err != nil {
		writeError(w, http.StatusNotFound, "pending change not found")
		return
	}
	if status != "pending" {
		writeError(w, http.StatusConflict, fmt.Sprintf("change is already %q, not pending", status))
		return
	}

	_, err = h.wg.DB().Exec(r.Context(),
		`UPDATE pending_pr_changes SET status = 'rejected' WHERE id = $1`, id)
	if err != nil {
		log.Printf("handlers: reject-pr update %s: %v", id, err)
		writeError(w, http.StatusInternalServerError, "failed to update status")
		return
	}

	// Optionally transition the work item back to human_review so it can be
	// re-examined. Best-effort — if the transition fails it is logged but
	// the rejection itself still succeeds.
	userID, _ := r.Context().Value(contextKeyUserID).(string)
	if userID == "" {
		userID = "dashboard"
	}
	reason := "PR rejected via dashboard"
	if body.Reason != "" {
		reason = "PR rejected: " + body.Reason
	}

	wiID, parseErr := uuid.Parse(workItemID)
	if parseErr == nil {
		if tErr := h.wg.TransitionState(r.Context(), wiID, models.WorkItemStateHumanReview, userID, reason); tErr != nil {
			log.Printf("handlers: reject-pr transition work item %s back to human_review: %v", workItemID, tErr)
		}
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "rejected"})
}

// SetWorkItemPRGate handles POST /api/work-items/pr-gate.
// Updates the pr_gate_mode on a single work item.
func (h *Handlers) SetWorkItemPRGate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var body struct {
		WorkItemID string `json:"work_item_id"`
		Mode       string `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if body.Mode != "auto" && body.Mode != "manual" {
		writeError(w, http.StatusBadRequest, "mode must be 'auto' or 'manual'")
		return
	}

	wiID, err := uuid.Parse(body.WorkItemID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid work_item_id")
		return
	}

	tag, err := h.wg.DB().Exec(r.Context(),
		`UPDATE work_items SET pr_gate_mode = $1, updated_at = NOW() WHERE id = $2`,
		body.Mode, wiID)
	if err != nil {
		log.Printf("handlers: set work item pr gate: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to update pr_gate_mode")
		return
	}
	if tag.RowsAffected() == 0 {
		writeError(w, http.StatusNotFound, "work item not found")
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "mode": body.Mode})
}

// ─── Admin PR Gate Handlers ──────────────────────────────────────────────────

// SetProductPRGate handles GET and POST /api/admin/products/pr-gate.
// GET: returns the current default_pr_gate_mode for a product.
// POST: updates the default_pr_gate_mode for a product.
func (h *AdminHandlers) SetProductPRGate(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.getProductPRGate(w, r)
	case http.MethodPost:
		h.setProductPRGate(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (h *AdminHandlers) getProductPRGate(w http.ResponseWriter, r *http.Request) {
	productIDStr := r.URL.Query().Get("product_id")
	if productIDStr == "" {
		writeError(w, http.StatusBadRequest, "product_id is required")
		return
	}

	productID, err := uuid.Parse(productIDStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid product_id")
		return
	}

	var mode string
	err = h.db.QueryRow(r.Context(),
		`SELECT default_pr_gate_mode FROM products WHERE id = $1`, productID,
	).Scan(&mode)
	if err != nil {
		writeError(w, http.StatusNotFound, "product not found")
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"product_id": productID.String(),
		"mode":       mode,
	})
}

func (h *AdminHandlers) setProductPRGate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ProductID string `json:"product_id"`
		Mode      string `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if body.Mode != "auto" && body.Mode != "manual" {
		writeError(w, http.StatusBadRequest, "mode must be 'auto' or 'manual'")
		return
	}

	productID, err := uuid.Parse(body.ProductID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid product_id")
		return
	}

	tag, err := h.db.Exec(r.Context(),
		`UPDATE products SET default_pr_gate_mode = $1 WHERE id = $2`,
		body.Mode, productID)
	if err != nil {
		log.Printf("handlers: set product pr gate: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to update default_pr_gate_mode")
		return
	}
	if tag.RowsAffected() == 0 {
		writeError(w, http.StatusNotFound, "product not found")
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "mode": body.Mode})
}
