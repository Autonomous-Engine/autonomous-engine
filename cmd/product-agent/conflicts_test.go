package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/llm/strategy"
)

// Live (ciblame item 0f565594): the PRD the product agent wrote said both
// "rows sorted ascending" and "`--top` keeps the newest days". The PRD's
// own self-review did not catch it, the planner could not own the
// ascending criterion, and the verifier escalated it to a person after
// implementation. A dedicated check of the acceptance criteria asks the
// model for the pairs that cannot both hold; a conflicting draft is
// regenerated once with the conflicts as guidance, and what still conflicts
// is shown on the PRD approval prompt — the gate that already exists for
// a person to settle it, before any research or code.

// scriptedLLM answers Execute calls in order, by intent type.
type scriptedLLM struct {
	answers map[strategy.IntentType][]string
	errs    map[strategy.IntentType]error
	calls   []strategy.Intent
}

func (s *scriptedLLM) Execute(_ context.Context, in strategy.Intent) (*strategy.Result, error) {
	s.calls = append(s.calls, in)
	if err := s.errs[in.Type]; err != nil {
		return nil, err
	}
	q := s.answers[in.Type]
	if len(q) == 0 {
		return nil, errors.New("no scripted answer for " + string(in.Type))
	}
	s.answers[in.Type] = q[1:]
	return &strategy.Result{Content: q[0]}, nil
}

func (s *scriptedLLM) calledTypes() string {
	var ts []string
	for _, c := range s.calls {
		ts = append(ts, string(c.Type))
	}
	return strings.Join(ts, ",")
}

const conflictingPRD = "# Grouping by day\n\n## Acceptance Criteria\n\n- AC-1: `--by day` groups commits by date\n- AC-3: rows are sorted ascending by date\n- AC-5: `--top N` keeps the N most recent days\n"
const settledPRD = "# Grouping by day\n\n## Acceptance Criteria\n\n- AC-1: `--by day` groups commits by date\n- AC-3: rows are sorted newest first\n- AC-5: `--top N` keeps the N most recent days\n"

func TestParsePRDConflictsReadsAFencedOrBareObject(t *testing.T) {
	cs, err := parsePRDConflicts("Here is my assessment.\n```json\n{\"conflicts\":[{\"criteria\":[\"AC-3\",\"AC-5\"],\"conflict\":\"ascending order and keeping the newest days cannot both hold\"}]}\n```\nDone.")
	if err != nil || len(cs) != 1 || strings.Join(cs[0].Criteria, "+") != "AC-3+AC-5" || !strings.Contains(cs[0].Conflict, "cannot both hold") {
		t.Fatalf("fenced: %+v, %v", cs, err)
	}
	cs, err = parsePRDConflicts(`{"conflicts": []}`)
	if err != nil || len(cs) != 0 {
		t.Fatalf("bare empty: %+v, %v", cs, err)
	}
	// An entry naming fewer than two criteria, or saying nothing, is not a conflict.
	cs, err = parsePRDConflicts(`{"conflicts":[{"criteria":["AC-2"],"conflict":"x"},{"criteria":["AC-1","AC-2"],"conflict":"  "}]}`)
	if err != nil || len(cs) != 0 {
		t.Fatalf("degenerate entries: %+v, %v", cs, err)
	}
	if _, err = parsePRDConflicts("I see no JSON to give you."); err == nil {
		t.Fatal("no object: want an error")
	}
}

func TestReconcilePRDRegeneratesOnceAndKeepsWhatStillConflicts(t *testing.T) {
	llm := &scriptedLLM{answers: map[strategy.IntentType][]string{
		strategy.IntentCheckPRDConflicts: {
			`{"conflicts":[{"criteria":["AC-3","AC-5"],"conflict":"ascending order and keeping the newest days cannot both hold"}]}`,
			`{"conflicts":[]}`,
		},
	}}
	var guidance []string
	regenerate := func(_ context.Context, g string) (string, error) {
		guidance = append(guidance, g)
		return "Sure, here is the revised PRD.\n\n" + settledPRD, nil
	}
	prd, remaining := reconcilePRD(context.Background(), llm, regenerate, conflictingPRD)
	if prd != settledPRD {
		t.Errorf("prd = %q, want the regenerated PRD with its preamble stripped", prd)
	}
	if len(remaining) != 0 {
		t.Errorf("remaining = %+v, want none", remaining)
	}
	if len(guidance) != 1 || !strings.Contains(guidance[0], "AC-3") || !strings.Contains(guidance[0], "AC-5") || !strings.Contains(guidance[0], "cannot both hold") {
		t.Errorf("regenerate guidance = %q, want the conflict named", guidance)
	}
	if got := llm.calledTypes(); got != "check_prd_conflicts,check_prd_conflicts" {
		t.Errorf("calls = %s, want a check before and after the regeneration", got)
	}
	if !strings.Contains(llm.calls[1].Context["prd"], "newest first") {
		t.Errorf("second check did not see the regenerated PRD: %q", llm.calls[1].Context["prd"])
	}

	// Still conflicting after the one regeneration: the person sees it.
	llm = &scriptedLLM{answers: map[strategy.IntentType][]string{
		strategy.IntentCheckPRDConflicts: {
			`{"conflicts":[{"criteria":["AC-3","AC-5"],"conflict":"first"}]}`,
			`{"conflicts":[{"criteria":["AC-3","AC-5"],"conflict":"still"}]}`,
		},
	}}
	calls := 0
	prd, remaining = reconcilePRD(context.Background(), llm, func(context.Context, string) (string, error) { calls++; return conflictingPRD, nil }, conflictingPRD)
	if calls != 1 || prd != conflictingPRD || len(remaining) != 1 || remaining[0].Conflict != "still" {
		t.Errorf("calls=%d prd=%q remaining=%+v, want one regeneration and the remaining conflict", calls, prd, remaining)
	}
}

func TestReconcilePRDNeverBlocksTheDraft(t *testing.T) {
	// The check failing (provider error, no JSON) keeps the draft as it is.
	llm := &scriptedLLM{errs: map[strategy.IntentType]error{strategy.IntentCheckPRDConflicts: errors.New("provider down")}}
	calls := 0
	prd, remaining := reconcilePRD(context.Background(), llm, func(context.Context, string) (string, error) { calls++; return settledPRD, nil }, conflictingPRD)
	if prd != conflictingPRD || remaining != nil || calls != 0 {
		t.Errorf("check error: prd=%q remaining=%+v regenerations=%d, want the draft untouched", prd, remaining, calls)
	}
	// The regeneration failing keeps the draft and its conflicts.
	llm = &scriptedLLM{answers: map[strategy.IntentType][]string{
		strategy.IntentCheckPRDConflicts: {`{"conflicts":[{"criteria":["AC-3","AC-5"],"conflict":"c"}]}`},
	}}
	prd, remaining = reconcilePRD(context.Background(), llm, func(context.Context, string) (string, error) { return "", errors.New("timeout") }, conflictingPRD)
	if prd != conflictingPRD || len(remaining) != 1 {
		t.Errorf("regenerate error: prd=%q remaining=%+v, want the draft with its conflict", prd, remaining)
	}
	// An empty regeneration is no regeneration.
	llm = &scriptedLLM{answers: map[strategy.IntentType][]string{
		strategy.IntentCheckPRDConflicts: {`{"conflicts":[{"criteria":["AC-3","AC-5"],"conflict":"c"}]}`},
	}}
	prd, remaining = reconcilePRD(context.Background(), llm, func(context.Context, string) (string, error) { return "  ", nil }, conflictingPRD)
	if prd != conflictingPRD || len(remaining) != 1 {
		t.Errorf("empty regeneration: prd=%q remaining=%+v, want the draft with its conflict", prd, remaining)
	}
}

func TestConflictPromptBlockNamesEachConflict(t *testing.T) {
	block := conflictPromptBlock([]prdConflict{
		{Criteria: []string{"AC-3", "AC-5"}, Conflict: "ascending order and keeping the newest days cannot both hold"},
	})
	for _, want := range []string{"Conflicting acceptance criteria", "AC-3 and AC-5", "cannot both hold", "Reject"} {
		if !strings.Contains(block, want) {
			t.Errorf("block lacks %q:\n%s", want, block)
		}
	}
	if conflictPromptBlock(nil) != "" {
		t.Error("no conflicts: want an empty block")
	}
}

// main.go: the draft is reconciled before it is committed, and the approval
// prompt carries what still conflicts.
func TestProductAgentReconcilesThePRDBeforeThePrompt(t *testing.T) {
	src := stripComments(agentSources(t)["main.go"])
	reconcile := strings.Index(src, "prdContent, prdConflicts := reconcilePRD(")
	block := strings.Index(src, "Question:    conflictPromptBlock(prdConflicts) + prdContent,")
	payload := strings.Index(src, `"prd_conflicts": prdConflicts,`)
	commit := strings.Index(src, "err = commitPRD(")
	if reconcile < 0 || block < 0 || payload < 0 || commit < 0 || !(reconcile < block && block < commit && reconcile < payload && payload < commit) {
		t.Fatalf("want reconcilePRD, then the prompt's question and payload carrying the conflicts, then commitPRD (positions %d, %d, %d, %d)", reconcile, block, payload, commit)
	}
}
