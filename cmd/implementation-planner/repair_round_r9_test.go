package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/plan"
	"github.com/google/uuid"
)

// R9 P2: a repair round that strictly reduced the violations earns exactly
// one more, unless the work item has spent more than 60% of its budget or
// what is left is the planner's to normalise or record.

// twoFixes is a plan with two reprompt violations (two steps without a
// deliverable) that the fake model fixes one per round.
func twoFixes() (*plan.BriefCriteria, func(round int) []decomposedTask) {
	brief := orphanBrief("A", "B")
	return brief, func(round int) []decomposedTask {
		steps := []decomposedTask{
			orphanStep("A", []string{"AC-1"}, "src/a.ts"),
			orphanStep("B", []string{"AC-2"}, "src/b.ts"),
		}
		if round < 2 {
			steps[0].Deliverable = ""
		}
		if round < 3 {
			steps[1].Deliverable = ""
		}
		return steps
	}
}

func runRounds(t *testing.T, brief *plan.BriefCriteria, plans func(int) []decomposedTask, rb repairBudget) (planOutcome, int) {
	t.Helper()
	calls := 0
	out := planWithPolicy(uuid.Nil, func(string, []string) ([]decomposedTask, error) {
		calls++
		return plans(calls), nil
	}, capturedBuilder(brief), rb)
	if out.decomposeErr != nil {
		t.Fatal(out.decomposeErr)
	}
	return out, calls
}

func spentTokens(n int64) repairBudget {
	return repairBudget{budget: 500_000, spent: func() (int64, error) { return n, nil }}
}

// Acceptance: one fixed per round plans in 3 attempts.
func TestProgressEarnsASecondRepairRound(t *testing.T) {
	brief, plans := twoFixes()
	out, calls := runRounds(t, brief, plans, spentTokens(100_000))
	if out.rejected() || calls != 3 || out.attempts != 3 {
		t.Fatalf("rejected=%t calls=%d attempts=%d violations=%+v", out.rejected(), calls, out.attempts, out.violations)
	}
	if out.roundNote != "" {
		t.Errorf("roundNote = %q", out.roundNote)
	}
}

// Acceptance: the same case above 60% of D4 blocks after 2, and says why.
func TestSecondRepairRoundIsSkippedAboveSixtyPercentSpend(t *testing.T) {
	brief, plans := twoFixes()
	out, calls := runRounds(t, brief, plans, spentTokens(300_001))
	if !out.rejected() || calls != 2 || out.attempts != 2 {
		t.Fatalf("rejected=%t calls=%d attempts=%d", out.rejected(), calls, out.attempts)
	}
	want := "a second repair round was not tried: the work item has used 300001 of its 500000-token budget, more than the 60% a further round may start from"
	if !strings.Contains(out.roundNote, want) {
		t.Fatalf("roundNote = %q, want %q", out.roundNote, want)
	}
	reason := planValidationReason(plan.Errors(out.violations), out.attempts) + out.blockNote()
	if !strings.Contains(reason, want) || !strings.Contains(reason, "the decomposition and one repair round") {
		t.Fatalf("block reason: %s", reason)
	}
	// Exactly at 60% is not above it.
	out, calls = runRounds(t, brief, plans, spentTokens(300_000))
	if out.rejected() || calls != 3 {
		t.Fatalf("at 60%%: rejected=%t calls=%d", out.rejected(), calls)
	}
}

// The spend cannot be read: fail closed, no further round, and the reason
// says so. No spend function (planWithRepair) is the default: one round.
func TestSecondRepairRoundNeedsAKnownSpend(t *testing.T) {
	brief, plans := twoFixes()
	out, calls := runRounds(t, brief, plans, repairBudget{budget: 500_000, spent: func() (int64, error) { return 0, errors.New("db down") }})
	if !out.rejected() || calls != 2 || !strings.Contains(out.roundNote, "could not read the work item's token spend") {
		t.Fatalf("rejected=%t calls=%d note=%q", out.rejected(), calls, out.roundNote)
	}
	calls = 0
	out = planWithRepair(uuid.Nil, func(string) ([]decomposedTask, error) {
		calls++
		return plans(calls), nil
	}, capturedBuilder(brief))
	if !out.rejected() || calls != maxPlanAttempts {
		t.Fatalf("planWithRepair: rejected=%t calls=%d, want the default of one repair round", out.rejected(), calls)
	}
}

// No progress (the same violations), or a new violation next to a fixed
// one, earns nothing.
func TestNoProgressNoSecondRound(t *testing.T) {
	brief, plans := twoFixes()
	same := func(int) []decomposedTask { return plans(1) }
	if out, calls := runRounds(t, brief, same, spentTokens(0)); !out.rejected() || calls != 2 || out.roundNote != "" {
		t.Fatalf("same violations: rejected=%t calls=%d note=%q", out.rejected(), calls, out.roundNote)
	}
	traded := func(round int) []decomposedTask {
		steps := plans(round)
		if round == 2 {
			steps[0].Verification = nil // fixes nothing new: trades a deliverable for a verification
		}
		return steps
	}
	if out, calls := runRounds(t, brief, traded, spentTokens(0)); !out.rejected() || calls != 2 {
		t.Fatalf("a new violation: rejected=%t calls=%d", out.rejected(), calls)
	}
}

// Exactly one more round: progress in the second repair round does not
// earn a third.
func TestAtMostOneExtraRound(t *testing.T) {
	brief := orphanBrief("A", "B", "C")
	plans := func(round int) []decomposedTask {
		steps := []decomposedTask{
			orphanStep("A", []string{"AC-1"}, "src/a.ts"),
			orphanStep("B", []string{"AC-2"}, "src/b.ts"),
			orphanStep("C", []string{"AC-3"}, "src/c.ts"),
		}
		for i := round - 1; i < 3; i++ {
			steps[i].Deliverable = ""
		}
		return steps
	}
	out, calls := runRounds(t, brief, plans, spentTokens(0))
	if !out.rejected() || calls != 3 || out.attempts != 3 {
		t.Fatalf("rejected=%t calls=%d attempts=%d", out.rejected(), calls, out.attempts)
	}
	if reason := planValidationReason(plan.Errors(out.violations), out.attempts); !strings.Contains(reason, "after 3 attempt(s) (the decomposition and 2 repair rounds)") {
		t.Fatalf("reason: %s", reason)
	}
}

// What is left after the repair round is only the planner's (an orphan no
// step covers): no further round, it is recorded as a gap.
func TestNoSecondRoundForNormaliseOrGapCodes(t *testing.T) {
	brief := orphanBrief("A", "B", "The notice reads politely")
	plans := func(round int) []decomposedTask {
		steps := []decomposedTask{
			orphanStep("A", []string{"AC-1"}, "src/a.ts"),
			orphanStep("B", []string{"AC-2"}, "src/b.ts"),
		}
		if round == 1 {
			steps[1].Deliverable = ""
		}
		return steps
	}
	out, calls := runRounds(t, brief, plans, spentTokens(0))
	if out.rejected() || calls != 2 || strings.Join(out.build.unowned, ",") != "AC-3" {
		t.Fatalf("rejected=%t calls=%d unowned=%v", out.rejected(), calls, out.build.unowned)
	}
}

// The handler wires the spend query and D4 into the planner.
func TestHandlerBoundsTheSecondRoundBySpend(t *testing.T) {
	src := stripComments(plannerSources(t)["main.go"])
	for _, want := range []string{
		"planWithPolicy(workID,",
		"repairBudget{budget: tools.TokenBudget(), spent: func() (int64, error) { return workItemTokensSpent(msgCtx, pool, workID) }}",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("main.go lacks %q", want)
		}
	}
	if extraRoundMaxSpendPercent != 60 || maxPlanAttempts != 2 {
		t.Fatalf("limits changed: %d%%, %d attempts", extraRoundMaxSpendPercent, maxPlanAttempts)
	}
}
