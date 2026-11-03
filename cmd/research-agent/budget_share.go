package main

import (
	"context"
	"fmt"
	"os"
	"strconv"

	"github.com/autonomousengine/autonomous-engine/internal/llm/tools"
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
// the loop once the work item's token total passes research's share of the
// budget. cumulative is the live total the loop re-reads before each call.
func researchStageHook(share float64) tools.IterationHook {
	return func(_ context.Context, cumulative, budget int64) error {
		limit := int64(float64(budget) * share)
		if cumulative > limit {
			return fmt.Errorf("%w: research stage used %d of its %d-token share (%.0f%% of WORK_ITEM_TOKEN_BUDGET)",
				tools.ErrTokenBudgetExceeded, cumulative, limit, share*100)
		}
		return nil
	}
}
