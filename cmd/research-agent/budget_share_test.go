package main

import (
	"context"
	"errors"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/llm/tools"
)

// Research shares one token budget with planning, implementation and
// verification. It must stop at its share so the later stages can run (F36).
func TestResearchStageHookStopsAtShare(t *testing.T) {
	hook := researchStageHook(0.4)
	if err := hook(context.Background(), 199_999, 500_000); err != nil {
		t.Fatalf("under the share: %v", err)
	}
	err := hook(context.Background(), 200_001, 500_000)
	if !errors.Is(err, tools.ErrTokenBudgetExceeded) {
		t.Fatalf("over the share: err = %v, want ErrTokenBudgetExceeded", err)
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
