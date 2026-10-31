package onboarding

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store persists and reads Candidate rows. pgx v5 pool-backed; no business
// logic lives here — all data is validated by the check constraints on the
// onboarding_candidates table.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore constructs a Store bound to the given pool.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Upsert inserts or updates a candidate row keyed by (product_id, question_key).
// Value, confidence, evidence, extractor, and generated_at are overwritten on
// every call. user_status + user_value are PRESERVED on conflict — a re-run of
// the indexer must never silently wipe the reviewer's decision. Use Review to
// change user-facing state.
func (s *Store) Upsert(ctx context.Context, c *Candidate) error {
	evidenceJSON, err := json.Marshal(c.Evidence)
	if err != nil {
		return fmt.Errorf("marshal evidence: %w", err)
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO onboarding_candidates (
			product_id, repository_id, question_key, value, confidence,
			evidence, extractor, user_status, generated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, 'pending', NOW())
		ON CONFLICT (product_id, question_key) DO UPDATE SET
			repository_id = EXCLUDED.repository_id,
			value         = EXCLUDED.value,
			confidence    = EXCLUDED.confidence,
			evidence      = EXCLUDED.evidence,
			extractor     = EXCLUDED.extractor,
			generated_at  = NOW()
	`, c.ProductID, c.RepositoryID, c.QuestionKey, c.Value, string(c.Confidence),
		evidenceJSON, c.Extractor)
	if err != nil {
		return fmt.Errorf("upsert candidate %s/%s: %w", c.ProductID, c.QuestionKey, err)
	}
	return nil
}

// List returns every candidate for a product, ordered by question_key for
// deterministic rendering.
func (s *Store) List(ctx context.Context, productID uuid.UUID) ([]Candidate, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, product_id, repository_id, question_key, value, confidence,
		       evidence, extractor, user_status, user_value, generated_at, reviewed_at
		  FROM onboarding_candidates
		 WHERE product_id = $1
		 ORDER BY question_key ASC
	`, productID)
	if err != nil {
		return nil, fmt.Errorf("list candidates: %w", err)
	}
	defer rows.Close()

	var out []Candidate
	for rows.Next() {
		c, err := scanCandidate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Get returns a single candidate by (product_id, question_key). Returns
// (nil, nil) when no row exists — callers must handle missing rows explicitly.
func (s *Store) Get(ctx context.Context, productID uuid.UUID, questionKey string) (*Candidate, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, product_id, repository_id, question_key, value, confidence,
		       evidence, extractor, user_status, user_value, generated_at, reviewed_at
		  FROM onboarding_candidates
		 WHERE product_id = $1 AND question_key = $2
	`, productID, questionKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, nil
	}
	c, err := scanCandidate(rows)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// Review updates user_status (and user_value, if provided) for a single
// candidate. Sets reviewed_at=NOW().
func (s *Store) Review(ctx context.Context, productID uuid.UUID, questionKey string, status UserStatus, userValue *string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE onboarding_candidates
		   SET user_status = $1,
		       user_value  = $2,
		       reviewed_at = NOW()
		 WHERE product_id = $3 AND question_key = $4
	`, string(status), userValue, productID, questionKey)
	if err != nil {
		return fmt.Errorf("review candidate: %w", err)
	}
	return nil
}

// Delete removes all candidates for a product (used when a product is deleted
// or the operator forces a full regen from scratch).
func (s *Store) Delete(ctx context.Context, productID uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM onboarding_candidates WHERE product_id = $1`, productID)
	if err != nil {
		return fmt.Errorf("delete candidates: %w", err)
	}
	return nil
}

// rowScanner is the subset of pgx.Rows and pgx.Row needed by scanCandidate.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanCandidate(r rowScanner) (Candidate, error) {
	var c Candidate
	var evidenceRaw []byte
	var confidence, userStatus string
	var userValue *string
	var reviewedAt *time.Time
	if err := r.Scan(
		&c.ID, &c.ProductID, &c.RepositoryID, &c.QuestionKey, &c.Value,
		&confidence, &evidenceRaw, &c.Extractor, &userStatus, &userValue,
		&c.GeneratedAt, &reviewedAt,
	); err != nil {
		return Candidate{}, fmt.Errorf("scan candidate: %w", err)
	}
	c.Confidence = Confidence(confidence)
	c.UserStatus = UserStatus(userStatus)
	c.UserValue = userValue
	c.ReviewedAt = reviewedAt
	if len(evidenceRaw) > 0 {
		if err := json.Unmarshal(evidenceRaw, &c.Evidence); err != nil {
			return Candidate{}, fmt.Errorf("unmarshal evidence: %w", err)
		}
	}
	return c, nil
}
