package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/plan"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// R8 O3: a plan with too many orphans is a bad plan and still blocks.

// orphanPlan is a brief of n criteria, AC-1..AC-n, each naming its own file,
// and two steps that change every file but cite only the first n-orphans
// criteria: every orphan could be re-homed by file overlap.
func orphanPlan(n, orphans int) (*plan.BriefCriteria, []decomposedTask) {
	var texts, files []string
	for i := 1; i <= n; i++ {
		f := "src/f" + string(rune('0'+i)) + ".ts"
		files = append(files, f)
		texts = append(texts, "`"+f+"` is updated")
	}
	var a, b []string
	for i := 1; i <= n-orphans; i++ {
		id := "AC-" + string(rune('0'+i))
		if i%2 == 1 {
			a = append(a, id)
		} else {
			b = append(b, id)
		}
	}
	if len(b) == 0 {
		b = a
	}
	return orphanBrief(texts...), []decomposedTask{
		orphanStep("Step A", a, files...),
		orphanStep("Step B", b, files...),
	}
}

// 3 orphans in a 6-criterion plan: more than maxResolvedOrphans → block,
// nothing re-homed, and the reason names the threshold and the orphans.
func TestThreeOrphansInSixCriteriaBlock(t *testing.T) {
	brief, steps := orphanPlan(6, 3)
	out, _ := planTwice(t, brief, steps)
	if !out.rejected() {
		t.Fatalf("plan accepted with 3 orphans of 6: rehomed=%+v unowned=%v", out.build.rehomed, out.build.unowned)
	}
	errs := plan.Errors(out.violations)
	if len(errs) != 3 {
		t.Fatalf("errors = %+v, want the 3 orphan_criterion violations", errs)
	}
	for _, v := range errs {
		if v.Code != plan.CodeOrphanCriterion {
			t.Errorf("error %+v is not an orphan", v)
		}
	}
	if len(out.build.rehomed) != 0 || len(out.build.unowned) != 0 {
		t.Fatalf("rehomed=%+v unowned=%v, want nothing resolved above the threshold", out.build.rehomed, out.build.unowned)
	}
	for _, s := range out.build.raw {
		for _, id := range []string{"AC-4", "AC-5", "AC-6"} {
			if containsString(s.CriterionIDs, id) {
				t.Errorf("%s was attached to %q although the plan is blocked", id, s.Title)
			}
		}
	}
	want := "3 of the plan's 6 criteria are owned by no task after the repair round (AC-4, AC-5, AC-6)"
	if !strings.Contains(out.orphanNote, want) {
		t.Fatalf("orphanNote = %q, want %q", out.orphanNote, want)
	}
	reason := planValidationReason(errs, out.attempts) + out.orphanNote
	for _, w := range []string{"[orphan_criterion] AC-4", want, "at most 2, and at most a third of the criteria"} {
		if !strings.Contains(reason, w) {
			t.Errorf("block reason lacks %q:\n%s", w, reason)
		}
	}
}

// The share rule on its own: 2 orphans of 5 criteria is more than a third.
func TestOrphanShareThresholdBlocks(t *testing.T) {
	brief, steps := orphanPlan(5, 2)
	out, _ := planTwice(t, brief, steps)
	if !out.rejected() || !strings.Contains(out.orphanNote, "2 of the plan's 5 criteria") {
		t.Fatalf("rejected=%t note=%q, want blocked by the share rule", out.rejected(), out.orphanNote)
	}
}

// At both limits (2 of 6): resolved.
func TestOrphansAtTheThresholdAreResolved(t *testing.T) {
	brief, steps := orphanPlan(6, 2)
	out, _ := planTwice(t, brief, steps)
	if out.rejected() || out.orphanNote != "" {
		t.Fatalf("rejected=%t note=%q violations=%+v", out.rejected(), out.orphanNote, out.violations)
	}
	if len(out.build.rehomed) != 2 {
		t.Fatalf("rehomed = %+v, want AC-5 and AC-6", out.build.rehomed)
	}
	if maxResolvedOrphans != 2 || orphanShareDivisor != 3 {
		t.Fatalf("thresholds changed: %d, 1/%d", maxResolvedOrphans, orphanShareDivisor)
	}
}

// An orphan next to any other error is not resolved: the plan blocks as
// before, without the threshold note.
func TestOrphanWithAnotherErrorStillBlocks(t *testing.T) {
	brief, steps := orphanPlan(6, 1)
	steps[1].Deliverable = ""
	out, _ := planTwice(t, brief, steps)
	if !out.rejected() || out.orphanNote != "" || len(out.build.rehomed) != 0 {
		t.Fatalf("rejected=%t note=%q rehomed=%+v", out.rejected(), out.orphanNote, out.build.rehomed)
	}
	var orphan bool
	for _, v := range plan.Errors(out.violations) {
		orphan = orphan || v.Code == plan.CodeOrphanCriterion
	}
	if !orphan {
		t.Fatalf("errors = %+v, want the orphan kept", out.violations)
	}
}

// The block carries the note: in the reason of the blocked transition and
// work.blocked, and in the plan-validation artifact. The handler passes it.
func TestBlockForPlanValidationCarriesTheOrphanNote(t *testing.T) {
	brief, steps := orphanPlan(6, 3)
	out, _ := planTwice(t, brief, steps)
	var artifact []byte
	saved := addArtifactTx
	addArtifactTx = func(_ context.Context, _ pgx.Tx, a *models.WorkItemArtifact) error {
		artifact = a.Content
		return nil
	}
	defer func() { addArtifactTx = saved }()
	tx := &decompTx{state: models.WorkItemStatePlanning}
	if err := blockForPlanValidation(context.Background(), decompDB{tx}, uuid.New(), out.violations, out.attempts, out.orphanNote); err != nil {
		t.Fatal(err)
	}
	var reason string
	for i, sql := range tx.sqls {
		if strings.Contains(sql, "INSERT INTO outbox_events") && tx.args[i][3] == bus.TopicWorkBlocked {
			var body map[string]any
			if err := json.Unmarshal(tx.args[i][5].([]byte), &body); err != nil {
				t.Fatal(err)
			}
			reason, _ = body["reason"].(string)
		}
	}
	if !strings.HasSuffix(reason, out.orphanNote) || !strings.Contains(reason, "Plan validation failed") {
		t.Fatalf("work.blocked reason = %q", reason)
	}
	var content planValidationContent
	if err := json.Unmarshal(artifact, &content); err != nil {
		t.Fatal(err)
	}
	if content.Note != strings.TrimSpace(out.orphanNote) || len(plan.Errors(content.Violations)) != 3 {
		t.Fatalf("artifact = %+v", content)
	}
	code := stripComments(plannerSources(t)["main.go"])
	for _, want := range []string{
		"workID, outcome.violations, outcome.attempts, outcome.blockNote())",
		"directBlock(planValidationReason(errs, outcome.attempts) + outcome.blockNote())",
	} {
		if !strings.Contains(code, want) {
			t.Errorf("main.go lacks %q", want)
		}
	}
}
