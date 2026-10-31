package tools

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// materializeCodex resolves the snapshot blobs for a given branch+commit.
// It returns a set of blob hashes that should scope subsequent codex queries.
// If no snapshot exists for the exact commit, it falls back to the latest
// snapshot on the branch. If no branch snapshot exists, returns nil (meaning
// "use live codex tables as before").
func materializeCodex(ctx context.Context, pool *pgxpool.Pool, productID uuid.UUID, branch, headSHA string) (map[string]struct{}, error) {
	// Step 1: find repos for the product.
	rows, err := pool.Query(ctx,
		`SELECT id FROM repositories WHERE product_id = $1 AND enabled = true`,
		productID,
	)
	if err != nil {
		return nil, err
	}
	var repoIDs []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		repoIDs = append(repoIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(repoIDs) == 0 {
		return nil, nil // no repos — fall back to live tables
	}

	blobSet := make(map[string]struct{})
	found := false

	for _, repoID := range repoIDs {
		var hashes []string

		// Step 2: try exact commit first.
		err := pool.QueryRow(ctx,
			`SELECT blob_hashes FROM codex_snapshots WHERE repo_id = $1 AND commit_sha = $2`,
			repoID, headSHA,
		).Scan(&hashes)

		if err == pgx.ErrNoRows {
			// Step 3: fall back to latest snapshot on branch.
			err = pool.QueryRow(ctx,
				`SELECT blob_hashes FROM codex_snapshots
				 WHERE repo_id = $1 AND branch = $2
				 ORDER BY extracted_at DESC LIMIT 1`,
				repoID, branch,
			).Scan(&hashes)
		}

		if err == pgx.ErrNoRows {
			continue // no snapshot for this repo — skip
		}
		if err != nil {
			return nil, err
		}

		// Step 4: collect all blob hashes into the set.
		for _, h := range hashes {
			blobSet[h] = struct{}{}
		}
		found = true
	}

	// Step 5: return nil if no snapshots found at all (fallback to live tables).
	if !found {
		return nil, nil
	}
	return blobSet, nil
}
