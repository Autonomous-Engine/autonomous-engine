// internal/codex/extractors/framework/failure.go
package framework

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type FailureStore struct {
	db *pgxpool.Pool
}

func NewFailureStore(db *pgxpool.Pool) *FailureStore { return &FailureStore{db: db} }

func (s *FailureStore) Record(ctx context.Context, repoID uuid.UUID, commit, extractor, errMsg string) error {
	_, err := s.db.Exec(ctx, `
		INSERT INTO codex_extraction_failure (repo_id, commit_sha, extractor, error_message)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT DO NOTHING
	`, repoID, commit, extractor, errMsg)
	return err
}
