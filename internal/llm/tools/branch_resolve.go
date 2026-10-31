package tools

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ResolveWorkItemBranch looks up the branch and head commit SHA for a work item.
// Returns ("","") if the work item has no branch context or the lookup fails.
// Work items may have branch context in their metadata JSONB column, populated
// when the work item is created from a branch-scoped trigger (e.g. a push
// event or PR webhook).
func ResolveWorkItemBranch(ctx context.Context, pool *pgxpool.Pool, workItemID uuid.UUID) (branch, headSHA string) {
	_ = pool.QueryRow(ctx,
		`SELECT COALESCE(metadata->>'branch', ''), COALESCE(metadata->>'head_sha', '')
		 FROM work_items WHERE id = $1`, workItemID,
	).Scan(&branch, &headSHA)
	return
}
