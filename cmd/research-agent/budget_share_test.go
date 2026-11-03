package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/llm/tools"
)

// Research shares one token budget with planning, implementation and
// verification. It must stop at its share so the later stages can run
// (F36) — measured on research's own spend, not the item's lifetime total:
// a retry after a costly implementation must still be able to research
// (review I1).
func TestResearchStageHookMeasuresResearchSpendOnly(t *testing.T) {
	spent := int64(100)
	hook := researchStageHook(0.4, func(context.Context) (int64, bool) { return spent, true }, nil)
	if err := hook(context.Background(), 600_000, 500_000); err != nil {
		t.Fatalf("lifetime total is over budget but research spent 100: %v", err)
	}
	spent = 200_001
	err := hook(context.Background(), 200_001, 500_000)
	if !errors.Is(err, tools.ErrTokenBudgetExceeded) {
		t.Fatalf("over the share: err = %v, want ErrTokenBudgetExceeded", err)
	}
}

func TestResearchStageHookFallsBackToTheLoopTotal(t *testing.T) {
	hook := researchStageHook(0.4, func(context.Context) (int64, bool) { return 0, false }, nil)
	if err := hook(context.Background(), 199_999, 500_000); err != nil {
		t.Fatalf("under the share: %v", err)
	}
	if err := hook(context.Background(), 200_001, 500_000); !errors.Is(err, tools.ErrTokenBudgetExceeded) {
		t.Fatalf("over the share: err = %v", err)
	}
}

func TestResearchTokenShareDefaultsAndParses(t *testing.T) {
	t.Setenv("RESEARCH_TOKEN_SHARE", "")
	if got := researchTokenShare(); got != 0.4 {
		t.Fatalf("default share = %v, want 0.4", got)
	}
	t.Setenv("RESEARCH_TOKEN_SHARE", "0.25")
	if got := researchTokenShare(); got != 0.25 {
		t.Fatalf("share = %v, want 0.25", got)
	}
	t.Setenv("RESEARCH_TOKEN_SHARE", "7")
	if got := researchTokenShare(); got != 0.4 {
		t.Fatalf("out-of-range share = %v, want the default", got)
	}
}

// The share is split between the round's tasks: a task that starts last
// found the share spent by the other two and failed after 3 iterations
// (live, pattern_match: "used 204107 of its 200000-token share"). Each
// task is capped at its slice (its own loop's spend), and the round at
// the share.
func TestResearchStageHookCapsEachTaskAtItsSlice(t *testing.T) {
	spent := int64(100_000)
	hook := researchStageHook(0.4, func(context.Context) (int64, bool) { return spent, true }, func(context.Context) int { return 3 })
	if err := hook(context.Background(), 50_000, 500_000); err != nil {
		t.Fatalf("under the slice (50k of 66k) and the share: %v", err)
	}
	err := hook(context.Background(), 70_000, 500_000)
	if !errors.Is(err, tools.ErrTokenBudgetExceeded) || !strings.Contains(err.Error(), "slice") {
		t.Fatalf("over the task's slice: err = %v, want ErrTokenBudgetExceeded naming the slice", err)
	}
	spent = 201_000
	if err := hook(context.Background(), 10_000, 500_000); !errors.Is(err, tools.ErrTokenBudgetExceeded) {
		t.Fatalf("over the round's share: err = %v", err)
	}
	// An unknown task count is one task: the whole share.
	hook = researchStageHook(0.4, func(context.Context) (int64, bool) { return 0, true }, func(context.Context) int { return 0 })
	if err := hook(context.Background(), 150_000, 500_000); err != nil {
		t.Fatalf("unknown count: %v", err)
	}
}
