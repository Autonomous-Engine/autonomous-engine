package codex

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Codex coherence Part 3: one branch fallback for grounding.
//
// GroundingBranch is the single rule shared by the grounding guard
// (guards.CheckCodexGrounded) and the context builder
// (BuildContextForIntentBranch), so the gate and the context it admits can
// never disagree about which codex a work item is grounded on.

const countModulesSQL = `SELECT COUNT(*) FROM codex_modules WHERE product_id = $1 AND branch = $2`

// GroundingBranch returns the branch whose codex grounds (productID,
// branch) and the number of modules on it:
//
//   - an empty branch is first resolved to the product's default branch
//     (repositories.base_branch, ResolveDefaultBranch); a product with no
//     default branch configured is an error, never the literal "main";
//   - a branch with at least one module is used as it is;
//   - a branch with no module falls back to the product's default branch
//     when that resolves and differs; when it does not resolve, the branch
//     is kept (zero modules).
//
// Only modules count: patterns and anti-patterns alone do not ground a
// product (the tools and the research seeds depend on modules).
func GroundingBranch(ctx context.Context, q Querier, productID uuid.UUID, branch string) (effective string, modules int, err error) {
	if branch == "" {
		resolved, rerr := resolveDefaultBranch(ctx, q, productID)
		if rerr != nil {
			return "", 0, rerr
		}
		branch = resolved
	}
	if err := q.QueryRow(ctx, countModulesSQL, productID, branch).Scan(&modules); err != nil {
		return "", 0, fmt.Errorf("count codex modules for product %s branch %s: %w", productID, branch, err)
	}
	if modules > 0 {
		return branch, modules, nil
	}
	defaultBranch, rerr := resolveDefaultBranch(ctx, q, productID)
	if rerr != nil || defaultBranch == branch {
		return branch, 0, nil
	}
	if err := q.QueryRow(ctx, countModulesSQL, productID, defaultBranch).Scan(&modules); err != nil {
		return "", 0, fmt.Errorf("count codex modules for product %s branch %s: %w", productID, defaultBranch, err)
	}
	return defaultBranch, modules, nil
}

// resolveDefaultBranch is ResolveDefaultBranch over any Querier (a pool or
// a transaction). A product without an enabled repository or with an empty
// base_branch wraps ErrNoDefaultBranch.
func resolveDefaultBranch(ctx context.Context, q Querier, productID uuid.UUID) (string, error) {
	var branch string
	err := q.QueryRow(ctx, `
		SELECT base_branch FROM repositories
		WHERE product_id = $1 AND enabled = true
		ORDER BY created_at
		LIMIT 1
	`, productID).Scan(&branch)
	if err != nil {
		if err == pgx.ErrNoRows {
			return "", fmt.Errorf("%w for product %s; re-run onboarding to populate repositories.base_branch", ErrNoDefaultBranch, productID)
		}
		return "", fmt.Errorf("resolve default branch for product %s: %w", productID, err)
	}
	if branch == "" {
		return "", fmt.Errorf("%w for product %s; re-run onboarding to populate repositories.base_branch", ErrNoDefaultBranch, productID)
	}
	return branch, nil
}
