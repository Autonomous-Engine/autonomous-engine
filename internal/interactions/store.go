// Package interactions — Postgres store for user_interactions rows.
// Matches migration 024. The codex back-link is polymorphic: codex_entry_id
// + codex_entry_table together identify the target row, since the codebase
// stores codex entries in per-section tables, not a unified codex_entries.
package interactions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("interaction not found")

// AnswerInput carries the fields collected from a user reply.
type AnswerInput struct {
	AnsweredBy     string
	AnsweredVia    string
	ActionChosen   string
	AnswerText     string
	SaveAsOverride *SaveAsOverride
}

// ClassificationResult is written back by knowledge-indexer after triage.
type ClassificationResult struct {
	ClassifiedAs    ClassifiedAs
	Confidence      float64
	CodexEntryID    *uuid.UUID
	CodexEntryTable string // '' when no codex entry was created
	QdrantPointID   string
}

// Store owns the user_interactions table CRUD surface.
type Store struct {
	db *pgxpool.Pool
}

func NewStore(db *pgxpool.Pool) *Store { return &Store{db: db} }

// execer is the write surface shared by *pgxpool.Pool and pgx.Tx, so the
// prompt insert is spelled once for Store.Insert and Publisher.PublishTx.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// insertPromptSQL is the single definition of the user_interactions insert.
const insertPromptSQL = `
		INSERT INTO user_interactions (
			id, work_item_id, product_id, asking_agent, trigger, prompt_type,
			headline, question, context_link, context_payload, actions,
			state, requested_at, prompted_via
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9, $10::jsonb, $11::jsonb, $12, $13, $14
		)
	`

func (s *Store) Insert(ctx context.Context, p *Prompt) error {
	return insertPrompt(ctx, s.db, p)
}

// insertPrompt writes p on ex — the pool or a caller's transaction.
func insertPrompt(ctx context.Context, ex execer, p *Prompt) error {
	ctxPayload, _ := json.Marshal(p.ContextPayload)
	if ctxPayload == nil {
		ctxPayload = []byte("{}")
	}
	actions, _ := json.Marshal(p.Actions)
	if actions == nil {
		actions = []byte("[]")
	}
	_, err := ex.Exec(ctx, insertPromptSQL,
		p.ID, p.WorkItemID, p.ProductID, p.AskingAgent, p.Trigger, p.PromptType,
		p.Headline, p.Question, p.ContextLink, ctxPayload, actions,
		p.State, p.RequestedAt, p.PromptedVia)
	return err
}

func (s *Store) GetByID(ctx context.Context, id uuid.UUID) (*Prompt, error) {
	row := s.db.QueryRow(ctx, selectPromptCols+` FROM user_interactions WHERE id = $1`, id)
	return scanPrompt(row)
}

func (s *Store) ListPending(ctx context.Context, productID *uuid.UUID, limit int) ([]*Prompt, error) {
	var rows pgx.Rows
	var err error
	if productID != nil {
		rows, err = s.db.Query(ctx, selectPromptCols+` FROM user_interactions WHERE state = 'pending' AND product_id = $1 ORDER BY requested_at ASC LIMIT $2`, *productID, limit)
	} else {
		rows, err = s.db.Query(ctx, selectPromptCols+` FROM user_interactions WHERE state = 'pending' ORDER BY requested_at ASC LIMIT $1`, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Prompt{}
	for rows.Next() {
		p, err := scanPrompt(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// MarkAnswered transitions pending → answered via CAS. Returns ErrNotFound
// when the prompt doesn't exist OR is already in a terminal state — so a
// concurrent second submit from another channel can't re-answer.
func (s *Store) MarkAnswered(ctx context.Context, id uuid.UUID, ans AnswerInput) error {
	var saveAs *string
	if ans.SaveAsOverride != nil {
		v := string(*ans.SaveAsOverride)
		saveAs = &v
	}
	tag, err := s.db.Exec(ctx, `
		UPDATE user_interactions
		SET state = 'answered',
			answered_at = NOW(),
			answered_by = $2,
			answered_via = $3,
			action_chosen = $4,
			answer_text = $5,
			save_as_override = $6
		WHERE id = $1 AND state = 'pending'
	`, id, ans.AnsweredBy, ans.AnsweredVia, ans.ActionChosen, ans.AnswerText, saveAs)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ApplyClassification writes the triage output (and the polymorphic codex
// back-link, if the answer was promoted). Idempotent — safe to call twice.
func (s *Store) ApplyClassification(ctx context.Context, id uuid.UUID, r ClassificationResult) error {
	return applyClassification(ctx, s.db, id, r)
}

// ApplyClassificationTx is ApplyClassification inside the caller's
// transaction, so the codex back-link can commit together with the
// codex.update.from_interaction event that creates the entry it points at.
// It never commits or rolls back.
func (s *Store) ApplyClassificationTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, r ClassificationResult) error {
	if tx == nil {
		return errors.New("interactions: ApplyClassificationTx requires the caller's transaction")
	}
	return applyClassification(ctx, tx, id, r)
}

// applyClassificationSQL is the single definition of the classification
// write-back, shared by ApplyClassification and ApplyClassificationTx.
const applyClassificationSQL = `
		UPDATE user_interactions
		SET classified_as = $2,
			classification_confidence = $3,
			codex_entry_id = $4,
			codex_entry_table = $5,
			qdrant_point_id = $6
		WHERE id = $1
	`

func applyClassification(ctx context.Context, ex execer, id uuid.UUID, r ClassificationResult) error {
	_, err := ex.Exec(ctx, applyClassificationSQL,
		id, r.ClassifiedAs, r.Confidence, r.CodexEntryID, nullIfEmpty(r.CodexEntryTable), r.QdrantPointID)
	return err
}

// MarkRevoked sets state=revoked only when the prompt is currently pending
// or answered. Already-revoked rows return ErrNotFound (prevents redundant
// downstream codex-version bumps).
func (s *Store) MarkRevoked(ctx context.Context, id uuid.UUID, by, reason string) error {
	tag, err := s.db.Exec(ctx, `
		UPDATE user_interactions
		SET state = 'revoked',
			revoked_at = NOW(),
			revoked_by = $2,
			revocation_reason = $3
		WHERE id = $1 AND state IN ('answered', 'pending')
	`, id, by, reason)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

const selectPromptCols = `
	SELECT id, work_item_id, product_id, asking_agent, trigger, prompt_type,
	       headline, question, COALESCE(context_link, ''), context_payload, actions,
	       state, requested_at, answered_at, prompted_via,
	       COALESCE(answered_by, ''), COALESCE(answered_via, ''),
	       COALESCE(action_chosen, ''), COALESCE(answer_text, ''), save_as_override,
	       COALESCE(classified_as, ''), classification_confidence,
	       codex_entry_id, COALESCE(codex_entry_table, ''), COALESCE(qdrant_point_id, ''),
	       revoked_at, COALESCE(revoked_by, ''), COALESCE(revocation_reason, '')`

type scannerLike interface {
	Scan(dest ...any) error
}

func scanPrompt(r scannerLike) (*Prompt, error) {
	p := &Prompt{}
	var ctxPayload, actions []byte
	var saveAs *string
	var classified string
	if err := r.Scan(&p.ID, &p.WorkItemID, &p.ProductID, &p.AskingAgent, &p.Trigger, &p.PromptType,
		&p.Headline, &p.Question, &p.ContextLink, &ctxPayload, &actions,
		&p.State, &p.RequestedAt, &p.AnsweredAt, &p.PromptedVia,
		&p.AnsweredBy, &p.AnsweredVia, &p.ActionChosen, &p.AnswerText, &saveAs,
		&classified, &p.ClassificationConfidence,
		&p.CodexEntryID, &p.CodexEntryTable, &p.QdrantPointID,
		&p.RevokedAt, &p.RevokedBy, &p.RevocationReason); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("scanPrompt: %w", err)
	}
	if len(ctxPayload) > 0 {
		_ = json.Unmarshal(ctxPayload, &p.ContextPayload)
	}
	if len(actions) > 0 {
		_ = json.Unmarshal(actions, &p.Actions)
	}
	if saveAs != nil {
		v := SaveAsOverride(*saveAs)
		p.SaveAsOverride = &v
	}
	if classified != "" {
		p.ClassifiedAs = ClassifiedAs(classified)
	}
	return p, nil
}

// FindPendingApprovalPromptForWorkItem returns the id of the most recent
// pending approval-type prompt for a work item, or ErrNotFound. Used by
// the dashboard's ApprovePRD / RejectPRD handlers to mark the prompt
// answered when the admin clicks through on the legacy approval page —
// so the bell + sheet stay in sync with the old /approvals flow.
func (s *Store) FindPendingApprovalPromptForWorkItem(ctx context.Context, workItemID uuid.UUID) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.db.QueryRow(ctx, `
		SELECT id FROM user_interactions
		WHERE work_item_id = $1 AND prompt_type = 'approval' AND state = 'pending'
		ORDER BY requested_at DESC LIMIT 1
	`, workItemID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrNotFound
	}
	return id, err
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// FindByCodexEntry resolves the paired user_interactions row from a codex
// entry's polymorphic back-link (codex_entry_id, codex_entry_table).
// Returns uuid.Nil for both fields with nil error when no interaction row
// references this codex entry — codex entries created by auto-indexing or
// manual edits are valid revoke targets, they just have no back-link.
func (s *Store) FindByCodexEntry(ctx context.Context, codexEntryID uuid.UUID, table string) (uuid.UUID, uuid.UUID, error) {
	var promptID, productID uuid.UUID
	err := s.db.QueryRow(ctx, `
		SELECT id, product_id FROM user_interactions
		WHERE codex_entry_id = $1 AND codex_entry_table = $2
		ORDER BY requested_at DESC LIMIT 1
	`, codexEntryID, table).Scan(&promptID, &productID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, uuid.Nil, nil
	}
	if err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	return promptID, productID, nil
}
