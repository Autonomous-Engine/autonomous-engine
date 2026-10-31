// internal/codex/extractors/framework/snapshot.go
package framework

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Snapshot struct {
	RepoID      uuid.UUID
	CommitSHA   string
	Branch      string
	ParentSHA   string
	BlobHashes  []string
	ExtractedAt time.Time
}

type SnapshotStore struct {
	db *pgxpool.Pool
}

func NewSnapshotStore(db *pgxpool.Pool) *SnapshotStore { return &SnapshotStore{db: db} }

func (s *SnapshotStore) Put(ctx context.Context, sn *Snapshot) error {
	_, err := s.db.Exec(ctx, `
		INSERT INTO codex_snapshots (repo_id, commit_sha, branch, parent_sha, blob_hashes)
		VALUES ($1, $2, $3, NULLIF($4, ''), $5)
		ON CONFLICT (repo_id, commit_sha) DO UPDATE SET
		  branch = EXCLUDED.branch,
		  parent_sha = EXCLUDED.parent_sha,
		  blob_hashes = EXCLUDED.blob_hashes,
		  extracted_at = NOW()
	`, sn.RepoID, sn.CommitSHA, sn.Branch, sn.ParentSHA, sn.BlobHashes)
	return err
}

func (s *SnapshotStore) Get(ctx context.Context, repoID uuid.UUID, commit string) (*Snapshot, error) {
	sn := &Snapshot{RepoID: repoID, CommitSHA: commit}
	var parent *string
	err := s.db.QueryRow(ctx, `
		SELECT branch, parent_sha, blob_hashes, extracted_at
		FROM codex_snapshots WHERE repo_id = $1 AND commit_sha = $2
	`, repoID, commit).Scan(&sn.Branch, &parent, &sn.BlobHashes, &sn.ExtractedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("snapshot get: %w", err)
	}
	if parent != nil {
		sn.ParentSHA = *parent
	}
	return sn, nil
}

func (s *SnapshotStore) LatestOnBranch(ctx context.Context, repoID uuid.UUID, branch string) (*Snapshot, error) {
	sn := &Snapshot{RepoID: repoID, Branch: branch}
	var parent *string
	err := s.db.QueryRow(ctx, `
		SELECT commit_sha, parent_sha, blob_hashes, extracted_at
		FROM codex_snapshots WHERE repo_id = $1 AND branch = $2
		ORDER BY extracted_at DESC LIMIT 1
	`, repoID, branch).Scan(&sn.CommitSHA, &parent, &sn.BlobHashes, &sn.ExtractedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if parent != nil {
		sn.ParentSHA = *parent
	}
	return sn, nil
}
