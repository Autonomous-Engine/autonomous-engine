package main

import (
	"context"
	"fmt"
	"os"
	"strconv"

	"github.com/autonomousengine/autonomous-engine/internal/llm/tools"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// defaultResearchTokenShare is the fraction of WORK_ITEM_TOKEN_BUDGET the
// research stage may spend. Research shares one budget with planning,
// implementation and verification; the audit saw research alone spend 1.5×
// the whole budget before planning started (F36).
const defaultResearchTokenShare = 0.4

// researchTokenShare reads RESEARCH_TOKEN_SHARE (0 < share ≤ 1), defaulting
// to defaultResearchTokenShare.
func researchTokenShare() float64 {
	if s := os.Getenv("RESEARCH_TOKEN_SHARE"); s != "" {
		if v, err := strconv.ParseFloat(s, 64); err == nil && v > 0 && v <= 1 {
			return v
		}
	}
	return defaultResearchTokenShare
}

// researchStageHook is the tool loop's IterationHook for research: it stops
// the loop once research's own spend on the work item passes its share of
// the budget. spent reports that spend (inference_logs rows with
// role='research'); when it cannot, the loop's live total stands in. A
// retry after a costly implementation must still be able to research, so
// the lifetime total is not what is compared (review I1).
func researchStageHook(share float64, spent func(ctx context.Context) (int64, bool)) tools.IterationHook {
	return func(ctx context.Context, cumulative, budget int64) error {
		used := cumulative
		if spent != nil {
			if n, ok := spent(ctx); ok {
				used = n
			}
		}
		limit := int64(float64(budget) * share)
		if used > limit {
			return fmt.Errorf("%w: research stage used %d of its %d-token share (%.0f%% of WORK_ITEM_TOKEN_BUDGET)",
				tools.ErrTokenBudgetExceeded, used, limit, share*100)
		}
		return nil
	}
}

// researchSpent sums the work item's research-stage inference rows.
func researchSpent(pool *pgxpool.Pool, workID uuid.UUID) func(ctx context.Context) (int64, bool) {
	return func(ctx context.Context) (int64, bool) {
		if pool == nil {
			return 0, false
		}
		var n int64
		if err := pool.QueryRow(ctx, `SELECT COALESCE(SUM(input_tokens + output_tokens), 0) FROM inference_logs WHERE work_item_id = $1 AND role = 'research'`, workID).Scan(&n); err != nil {
			return 0, false
		}
		return n, true
	}
}
