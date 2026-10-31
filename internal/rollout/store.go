package rollout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Rollout statuses (rollouts.status).
const (
	StatusProposed   = "proposed"
	StatusShadow     = "shadow"
	StatusEvaluating = "evaluating"
	StatusApproved   = "approved"
	StatusBlocked    = "blocked"
	StatusRolledBack = "rolled_back"
)

// Scopes (rollouts.scope).
const (
	ScopeGlobal  = "global"
	ScopeProduct = "product"
)

// Errors.
var (
	ErrNotFound         = errors.New("rollout: not found")
	ErrBadTransition    = errors.New("rollout: status transition not allowed")
	ErrNotApprovable    = errors.New("rollout: the latest decision is not APPROVED; approving needs an override reason")
	ErrRolloutRequired  = errors.New("rollout: this change sets a production default configuration without an approved rollout; approve a rollout for it or give an override reason")
	ErrSameSnapshot     = errors.New("rollout: candidate and baseline are the same snapshot")
	ErrOverrideNoReason = errors.New("rollout: an override needs a non-empty reason")
)

// DB is the database surface the store needs (*pgxpool.Pool, pgx.Tx).
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Rollout is one rollouts row.
type Rollout struct {
	ID                  uuid.UUID       `json:"id"`
	CandidateSnapshotID *string         `json:"candidate_snapshot_id"`
	BaselineSnapshotID  *string         `json:"baseline_snapshot_id"`
	Scope               string          `json:"scope"`
	ProductID           *uuid.UUID      `json:"product_id"`
	Status              string          `json:"status"`
	CreatedBy           string          `json:"created_by"`
	CreatedAt           time.Time       `json:"created_at"`
	UpdatedAt           time.Time       `json:"updated_at"`
	DecidedAt           *time.Time      `json:"decided_at"`
	DecidedBy           *string         `json:"decided_by"`
	DecisionReport      json.RawMessage `json:"decision_report"`
	Notes               string          `json:"notes"`
}

// Verdict returns the stored decision's verdict ("" when none).
func (r *Rollout) Verdict() string {
	if len(r.DecisionReport) == 0 {
		return ""
	}
	var d struct {
		Verdict string `json:"verdict"`
	}
	_ = json.Unmarshal(r.DecisionReport, &d)
	return d.Verdict
}

// transitions lists the allowed status changes. approved and blocked are
// decisions; rolled_back is terminal. A blocked rollout can only be
// re-proposed as a new row (a new candidate needs a new evaluation).
var transitions = map[string][]string{
	StatusProposed:   {StatusShadow, StatusEvaluating, StatusBlocked},
	StatusShadow:     {StatusShadow, StatusEvaluating, StatusBlocked},
	StatusEvaluating: {StatusShadow, StatusEvaluating, StatusApproved, StatusBlocked},
	StatusApproved:   {StatusRolledBack, StatusBlocked},
	StatusBlocked:    {},
	StatusRolledBack: {},
}

// CanTransition reports whether a rollout may move from one status to another.
func CanTransition(from, to string) bool {
	for _, s := range transitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

// Store reads and writes rollouts and the override audit.
type Store struct {
	db DB
}

// NewStore returns a store over db.
func NewStore(db DB) *Store { return &Store{db: db} }

const rolloutColumns = `id, candidate_snapshot_id, baseline_snapshot_id, scope, product_id, status,
	created_by, created_at, updated_at, decided_at, decided_by, decision_report, notes`

func scanRollout(row pgx.Row) (*Rollout, error) {
	var r Rollout
	var report []byte
	if err := row.Scan(&r.ID, &r.CandidateSnapshotID, &r.BaselineSnapshotID, &r.Scope, &r.ProductID, &r.Status,
		&r.CreatedBy, &r.CreatedAt, &r.UpdatedAt, &r.DecidedAt, &r.DecidedBy, &report, &r.Notes); err != nil {
		return nil, err
	}
	if len(report) > 0 {
		r.DecisionReport = json.RawMessage(report)
	}
	return &r, nil
}

// ProposeInput is a new rollout.
type ProposeInput struct {
	CandidateSnapshotID string
	BaselineSnapshotID  string
	Scope               string
	ProductID           *uuid.UUID
	CreatedBy           string
	Notes               string
}

// Propose inserts a proposed rollout. The candidate must be a stored
// snapshot different from the baseline (the production default it would
// replace).
func (s *Store) Propose(ctx context.Context, in ProposeInput) (*Rollout, error) {
	if in.CandidateSnapshotID == "" {
		return nil, fmt.Errorf("rollout: a candidate snapshot is required")
	}
	if in.CandidateSnapshotID == in.BaselineSnapshotID {
		return nil, ErrSameSnapshot
	}
	if in.Scope == "" {
		in.Scope = ScopeGlobal
	}
	switch in.Scope {
	case ScopeGlobal:
		if in.ProductID != nil {
			return nil, fmt.Errorf("rollout: a global rollout names no product")
		}
	case ScopeProduct:
		if in.ProductID == nil {
			return nil, fmt.Errorf("rollout: a product rollout needs a product")
		}
	default:
		return nil, fmt.Errorf("rollout: unknown scope %q", in.Scope)
	}
	var baseline any
	if in.BaselineSnapshotID != "" {
		baseline = in.BaselineSnapshotID
	}
	row := s.db.QueryRow(ctx, `INSERT INTO rollouts (candidate_snapshot_id, baseline_snapshot_id, scope, product_id, created_by, notes)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING `+rolloutColumns,
		in.CandidateSnapshotID, baseline, in.Scope, in.ProductID, in.CreatedBy, in.Notes)
	r, err := scanRollout(row)
	if err != nil {
		return nil, fmt.Errorf("rollout: propose: %w", err)
	}
	return r, nil
}

// Get returns one rollout.
func (s *Store) Get(ctx context.Context, id uuid.UUID) (*Rollout, error) {
	r, err := scanRollout(s.db.QueryRow(ctx, `SELECT `+rolloutColumns+` FROM rollouts WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("rollout: get: %w", err)
	}
	return r, nil
}

// List returns the newest rollouts first.
func (s *Store) List(ctx context.Context, limit int) ([]*Rollout, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.Query(ctx, `SELECT `+rolloutColumns+` FROM rollouts ORDER BY created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("rollout: list: %w", err)
	}
	defer rows.Close()
	out := []*Rollout{}
	for rows.Next() {
		r, err := scanRollout(rows)
		if err != nil {
			return nil, fmt.Errorf("rollout: list: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// fromStates returns the statuses a rollout may move to `to` from.
func fromStates(to string) []string {
	var out []string
	for from, tos := range transitions {
		for _, t := range tos {
			if t == to {
				out = append(out, from)
			}
		}
	}
	return out
}

// setStatus moves a rollout to status, enforcing the transition table in
// the UPDATE itself (a concurrent change cannot slip between a check and
// the write). decided marks a decision (approved/blocked/rolled_back).
func (s *Store) setStatus(ctx context.Context, id uuid.UUID, to, actor, note string, report []byte) (*Rollout, error) {
	return setStatus(ctx, s.db, id, to, actor, note, report)
}

func setStatus(ctx context.Context, q DB, id uuid.UUID, to, actor, note string, report []byte) (*Rollout, error) {
	decided := to == StatusApproved || to == StatusBlocked || to == StatusRolledBack
	var actorArg any
	if actor != "" {
		actorArg = actor
	}
	var reportArg any
	if report != nil {
		reportArg = report
	}
	row := q.QueryRow(ctx, `UPDATE rollouts SET status = $2, updated_at = NOW(),
			decided_at = CASE WHEN $3 THEN NOW() ELSE decided_at END,
			decided_by = CASE WHEN $3 THEN $4 ELSE decided_by END,
			decision_report = COALESCE($5::jsonb, decision_report),
			notes = CASE WHEN $6 = '' THEN notes WHEN notes = '' THEN $6 ELSE notes || E'\n' || $6 END
		WHERE id = $1 AND status = ANY($7) RETURNING `+rolloutColumns,
		id, to, decided, actorArg, reportArg, note, fromStates(to))
	r, err := scanRollout(row)
	if errors.Is(err, pgx.ErrNoRows) {
		var from string
		if err := q.QueryRow(ctx, `SELECT status FROM rollouts WHERE id = $1`, id).Scan(&from); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, ErrNotFound
			}
			return nil, fmt.Errorf("rollout: status: %w", err)
		}
		return nil, fmt.Errorf("%w: %s → %s", ErrBadTransition, from, to)
	}
	if err != nil {
		return nil, fmt.Errorf("rollout: set status: %w", err)
	}
	return r, nil
}

// withTx runs fn in a transaction when the store's db can begin one (a
// pool), else on the db itself (already a tx).
func (s *Store) withTx(ctx context.Context, fn func(q DB) error) error {
	if b, ok := s.db.(interface {
		Begin(ctx context.Context) (pgx.Tx, error)
	}); ok {
		return pgx.BeginFunc(ctx, b, func(tx pgx.Tx) error { return fn(tx) })
	}
	return fn(s.db)
}

// MarkShadow records that shadow runs are in progress.
func (s *Store) MarkShadow(ctx context.Context, id uuid.UUID, note string) (*Rollout, error) {
	return s.setStatus(ctx, id, StatusShadow, "", note, nil)
}

// RecordDecision stores an evaluation. A BLOCKED verdict blocks the rollout
// (D7 blocks; it is not a recommendation); APPROVED and MORE_TRIALS leave
// it evaluating — approval is an admin action (Approve).
func (s *Store) RecordDecision(ctx context.Context, id uuid.UUID, d Decision) (*Rollout, error) {
	report, err := json.Marshal(d)
	if err != nil {
		return nil, fmt.Errorf("rollout: encode decision: %w", err)
	}
	to := StatusEvaluating
	actor := ""
	if d.Verdict == VerdictBlocked {
		to, actor = StatusBlocked, "evaluator"
	}
	return s.setStatus(ctx, id, to, actor, "", report)
}

// Approve approves a rollout whose latest decision is APPROVED. Any other
// state needs overrideReason, which is written to the audit table with
// the evaluator's verdict.
func (s *Store) Approve(ctx context.Context, id uuid.UUID, actor, overrideReason string) (*Rollout, error) {
	r, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if r.Status == StatusApproved {
		return r, nil
	}
	verdict := r.Verdict()
	missing := r.NotEvaluated()
	if verdict != VerdictApproved || r.Status != StatusEvaluating {
		if strings.TrimSpace(overrideReason) == "" {
			if len(missing) > 0 {
				return nil, fmt.Errorf("%w (status %s, verdict %q; gates without data: %s)", ErrNotApprovable, r.Status, verdict, strings.Join(missing, ", "))
			}
			return nil, fmt.Errorf("%w (status %s, verdict %q)", ErrNotApprovable, r.Status, verdict)
		}
		// An override can approve from proposed/shadow/evaluating; a
		// blocked or rolled-back rollout stays closed (propose again).
		if r.Status == StatusBlocked || r.Status == StatusRolledBack {
			return nil, fmt.Errorf("%w: %s → %s", ErrBadTransition, r.Status, StatusApproved)
		}
		// The audit row and the approval commit together.
		var out *Rollout
		err := s.withTx(ctx, func(q DB) error {
			if err := RecordOverride(ctx, q, Override{
				RolloutID: &r.ID, Action: "approve_rollout", Scope: r.Scope, ProductID: r.ProductID,
				Before: deref(r.BaselineSnapshotID), After: deref(r.CandidateSnapshotID),
				Reason: overrideReason, Actor: actor,
				Details: map[string]any{"status": r.Status, "verdict": verdict, "missing_gates": missing},
			}); err != nil {
				return err
			}
			// Overridden approvals pass through evaluating so the
			// transition table stays the single rule.
			if r.Status != StatusEvaluating {
				if _, err := setStatus(ctx, q, id, StatusEvaluating, "", "", nil); err != nil {
					return err
				}
			}
			var err error
			out, err = setStatus(ctx, q, id, StatusApproved, actor, "approved by override: "+strings.TrimSpace(overrideReason), nil)
			return err
		})
		if err != nil {
			return nil, err
		}
		return out, nil
	}
	// The UPDATE itself re-checks the APPROVED decision: a decision written
	// between the read above and this write cannot be approved past.
	row := s.db.QueryRow(ctx, `UPDATE rollouts SET status = 'approved', updated_at = NOW(),
			decided_at = NOW(), decided_by = $2
		WHERE id = $1 AND status = 'evaluating' AND decision_report->>'verdict' = 'APPROVED'
		RETURNING `+rolloutColumns, id, actor)
	out, err := scanRollout(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w (the decision changed while approving)", ErrNotApprovable)
	}
	if err != nil {
		return nil, fmt.Errorf("rollout: approve: %w", err)
	}
	return out, nil
}

// NotEvaluated returns the stored decision's gates without data.
func (r *Rollout) NotEvaluated() []string {
	if len(r.DecisionReport) == 0 {
		return nil
	}
	var d struct {
		NotEvaluated []string `json:"not_evaluated"`
	}
	_ = json.Unmarshal(r.DecisionReport, &d)
	return d.NotEvaluated
}

// Block blocks a rollout (admin decision).
func (s *Store) Block(ctx context.Context, id uuid.UUID, actor, reason string) (*Rollout, error) {
	note := "blocked"
	if strings.TrimSpace(reason) != "" {
		note = "blocked: " + reason
	}
	return s.setStatus(ctx, id, StatusBlocked, actor, note, nil)
}

// Rollback marks an approved rollout rolled back.
func (s *Store) Rollback(ctx context.Context, id uuid.UUID, actor, reason string) (*Rollout, error) {
	note := "rolled back"
	if strings.TrimSpace(reason) != "" {
		note = "rolled back: " + reason
	}
	return s.setStatus(ctx, id, StatusRolledBack, actor, note, nil)
}

// Approved returns the newest approved rollout for snapshotID that covers
// scope (a global approval covers every product; a product approval only
// its product), or nil.
func Approved(ctx context.Context, q DB, snapshotID, scope string, productID *uuid.UUID) (*Rollout, error) {
	if snapshotID == "" {
		return nil, nil
	}
	row := q.QueryRow(ctx, `SELECT `+rolloutColumns+` FROM rollouts
		WHERE candidate_snapshot_id = $1 AND status = 'approved'
		  AND (scope = 'global' OR ($2 = 'product' AND product_id = $3))
		ORDER BY decided_at DESC NULLS LAST LIMIT 1`, snapshotID, scope, productID)
	r, err := scanRollout(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("rollout: approved lookup: %w", err)
	}
	return r, nil
}

// Override is one audit row of rollout_overrides.
type Override struct {
	RolloutID *uuid.UUID
	Action    string
	Scope     string
	ProductID *uuid.UUID
	Before    string
	After     string
	Reason    string
	Actor     string
	Details   map[string]any
}

// RecordOverride writes an audit row.
func (s *Store) RecordOverride(ctx context.Context, o Override) error {
	return RecordOverride(ctx, s.db, o)
}

// RecordOverride writes an audit row on q (a pool or the caller's tx).
func RecordOverride(ctx context.Context, q DB, o Override) error {
	if strings.TrimSpace(o.Reason) == "" {
		return ErrOverrideNoReason
	}
	if o.Scope == "" {
		o.Scope = ScopeGlobal
	}
	details := o.Details
	if details == nil {
		details = map[string]any{}
	}
	raw, err := json.Marshal(details)
	if err != nil {
		return fmt.Errorf("rollout: encode override details: %w", err)
	}
	_, err = q.Exec(ctx, `INSERT INTO rollout_overrides
		(rollout_id, action, scope, product_id, before_snapshot_id, after_snapshot_id, override_reason, actor, details)
		VALUES ($1, $2, $3, $4, NULLIF($5, ''), NULLIF($6, ''), $7, $8, $9)`,
		o.RolloutID, o.Action, o.Scope, o.ProductID, o.Before, o.After, strings.TrimSpace(o.Reason), o.Actor, raw)
	if err != nil {
		return fmt.Errorf("rollout: record override: %w", err)
	}
	return nil
}

// OverrideRow is a stored audit row.
type OverrideRow struct {
	ID        uuid.UUID       `json:"id"`
	RolloutID *uuid.UUID      `json:"rollout_id"`
	Action    string          `json:"action"`
	Scope     string          `json:"scope"`
	ProductID *uuid.UUID      `json:"product_id"`
	Before    *string         `json:"before_snapshot_id"`
	After     *string         `json:"after_snapshot_id"`
	Reason    string          `json:"override_reason"`
	Actor     string          `json:"actor"`
	Details   json.RawMessage `json:"details"`
	CreatedAt time.Time       `json:"created_at"`
}

// ListOverrides returns the newest audit rows first.
func (s *Store) ListOverrides(ctx context.Context, limit int) ([]OverrideRow, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.Query(ctx, `SELECT id, rollout_id, action, scope, product_id, before_snapshot_id,
		after_snapshot_id, override_reason, actor, details, created_at
		FROM rollout_overrides ORDER BY created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("rollout: list overrides: %w", err)
	}
	defer rows.Close()
	out := []OverrideRow{}
	for rows.Next() {
		var o OverrideRow
		var details []byte
		if err := rows.Scan(&o.ID, &o.RolloutID, &o.Action, &o.Scope, &o.ProductID, &o.Before, &o.After,
			&o.Reason, &o.Actor, &details, &o.CreatedAt); err != nil {
			return nil, fmt.Errorf("rollout: list overrides: %w", err)
		}
		o.Details = json.RawMessage(details)
		out = append(out, o)
	}
	return out, rows.Err()
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
