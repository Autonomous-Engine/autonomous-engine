package main

// R13 review M1: a fresh derived id minted in a repair round (R7's
// renumbering of a brief id redeclared with different text) must skip the
// ids earlier rounds used. Before the fix the repair round minted AC-11 —
// round 1's PR-text derived criterion — and restore then read the new one
// as a redeclaration of it: the redeclaring task was made to cite the
// PR-text AC-11 (wrongly owned, hiding the gap) and its own criterion was
// renumbered AC-12.

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/config"
	"github.com/autonomousengine/autonomous-engine/internal/llm"
	"github.com/autonomousengine/autonomous-engine/internal/plan"
	"github.com/google/uuid"
)

// attempt13Raw reads a recorded response as generic JSON, for editing.
func attempt13Raw(t *testing.T, file string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(corpusDir, "attempt13", file))
	if err != nil {
		t.Fatal(err)
	}
	var tasks []map[string]any
	if err := json.Unmarshal(data, &tasks); err != nil {
		t.Fatal(err)
	}
	return tasks
}

const m1TaskB = "Refuse a malformed TILES_API_KEY"

func TestRepairRoundFreshIDSkipsEarlierDerivedIDs(t *testing.T) {
	// Round 1: attempt 13's first answer (task 9 declares the PR-text
	// AC-11), with task 1's deliverable removed so a repair round is asked.
	round1 := attempt13Raw(t, "response_1.txt")
	delete(round1[0], "deliverable")
	// Round 2: attempt 13's repair (task 9 cites AC-11, no definition),
	// plus task B, which cites AC-1 and redeclares brief AC-3 with
	// different text.
	round2 := attempt13Raw(t, "response_2.txt")
	round2 = append(round2, map[string]any{
		"title":         m1TaskB,
		"description":   "Validate TILES_API_KEY at startup.",
		"criterion_ids": []string{"AC-1"},
		"deliverable":   "The server refuses to start with a malformed key.",
		"verification":  []map[string]any{{"check_kind": "build", "command": "npm run build"}, {"check_kind": "test", "command": "npm test"}},
		"files":         []map[string]any{{"path": "src/server/index.ts", "evidence_ref": "repo:stat"}},
		"depends_on":    []string{},
		"repository_id": "63f733e0-de21-40ad-b652-2cff5190ee40",
		"derived_criteria": []map[string]any{{
			"id": "AC-3", "text": "The server refuses to start when TILES_API_KEY is malformed, exiting with a clear error",
			"reason": "a malformed key fails every tile request", "risk": "low",
		}},
	})

	fake := &corpusLLM{responses: map[string][]string{
		roleDecompose: {string(mustJSON(t, round1))},
		roleRepair:    {string(mustJSON(t, round2))},
	}}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	svc, err := llm.NewService(&config.ModelConfig{Provider: "openai", Endpoint: srv.URL, APIKey: "test", Name: "fake"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	structured := loadCorpusBrief(t, filepath.Join(corpusDir, "attempt13"))
	plan.StripNonCode(structured.Criteria)
	unfolded := structured.Criteria
	var folds []plan.Fold
	structured.Criteria, folds = foldBrief(uuid.Nil, structured.Criteria)

	var prompts []string
	out := planWithPolicy(uuid.Nil,
		func(repair string, reserved []string) ([]decomposedTask, error) {
			if repair != "" {
				prompts = append(prompts, strings.Join(reserved, ","))
			}
			return decomposeIntoTasks(context.Background(), svc, uuid.Nil, structured, folds, "", repair, reserved)
		},
		capturedBuilder(unfolded),
		repairBudget{budget: 500_000, spent: func() (int64, error) { return 0, nil }},
	)
	if fake.unrecorded != "" {
		t.Fatalf("the planner asked an unrecorded %s", fake.unrecorded)
	}
	if out.attempts != 2 || out.build == nil {
		t.Fatalf("attempts = %d, decomposeErr %v", out.attempts, out.decomposeErr)
	}
	if len(prompts) != 1 || !strings.Contains(","+prompts[0]+",", ",AC-11,") {
		t.Errorf("the repair round's reserved ids = %v, want them to include AC-11", prompts)
	}

	var b *decomposedTask
	for i := range out.build.raw {
		if out.build.raw[i].Title == m1TaskB {
			b = &out.build.raw[i]
		}
	}
	if b == nil {
		t.Fatal("task B missing from the repaired plan")
	}
	if got := strings.Join(b.CriterionIDs, ","); got != "AC-1,AC-3,AC-12" {
		t.Errorf("task B cites %s, want AC-1,AC-3,AC-12 (not the earlier AC-11)", got)
	}
	if len(b.DerivedCriteria) != 1 || b.DerivedCriteria[0].ID != "AC-12" {
		t.Errorf("task B declares %+v, want its own criterion as AC-12", b.DerivedCriteria)
	}
	// AC-11 keeps round 1's text, is owned by no kept step (task 9 is
	// dropped) and is a PR-text gap.
	if d := derivedOf(out.build.raw, "AC-11"); d == nil || !plan.TargetsPRText(d.Text) {
		t.Fatalf("AC-11 = %+v, want round 1's PR-text criterion", d)
	}
	if out.rejected() {
		t.Fatalf("plan rejected: %v", plan.Errors(out.violations))
	}
	attachNonCodeCriteria(out.build)
	var gap bool
	for _, c := range out.build.criteria.Criteria {
		if c.ID == "AC-11" {
			gap = c.NonCode && c.NonCodeReason == plan.PRTextReason
		}
	}
	if !gap {
		t.Errorf("AC-11 is not an unowned PR-text gap: %+v", out.build.criteria.Criteria)
	}
	for _, s := range out.build.specs {
		if s.Dropped == "" && containsID(s.Spec.CriterionIDs, "AC-11") {
			t.Errorf("kept step %d %q owns AC-11", s.Index, s.Title)
		}
	}
}

// The prompt's "numbering from AC-n" skips the reserved ids too.
func TestCriteriaPromptNumbersPastReservedIDs(t *testing.T) {
	brief := &plan.BriefCriteria{Criteria: []plan.Criterion{{ID: "AC-1"}, {ID: "AC-10"}}}
	ctx := map[string]string{}
	criteriaPromptContext(ctx, brief, nil, "AC-11", "AC-12")
	var got string
	for _, v := range ctx {
		if strings.Contains(v, "numbering from") {
			got = v
		}
	}
	if !strings.Contains(got, "numbering from AC-13") {
		t.Errorf("prompt = %q, want numbering from AC-13", got)
	}
}
