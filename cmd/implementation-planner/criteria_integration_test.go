package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/plan"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
)

// TestIntegration_TaskSpecPersistsInPlanTaskAndEvent: the TaskSpec and the
// cited criteria ride inside the implementation.tasks payload, so they are in
// plan_tasks.message (what a held task is released with after a restart) and
// in the enqueued event, unchanged (M2-04).
func TestIntegration_TaskSpecPersistsInPlanTaskAndEvent(t *testing.T) {
	pool := integrationPool(t)
	f := newFixture(t, pool)
	wg := workgraph.New(pool)

	taskID := uuid.New()
	msg := f.taskMessageFor(taskID)
	spec := plan.TaskSpec{
		SchemaVersion: plan.SchemaVersion,
		CriterionIDs:  []string{"AC-1"},
		Deliverable:   "empty email shows an error",
		Verification:  []plan.VerificationStep{{CheckKind: evidence.CheckKindTest, Command: "npm test"}},
		Files: []plan.FileRef{
			{Path: "src/app.ts", Evidence: plan.EvidenceRef{Kind: plan.EvidenceRepo, Ref: plan.RefStat, Confidence: 1, Basis: plan.BasisClone}},
			{Path: "src/gone.ts", Evidence: plan.EvidenceRef{Kind: plan.EvidenceHypothesis, Note: "grounding: not found in the repository clone"}},
		},
	}
	msg.TaskSpec = &spec
	msg.Criteria = []plan.Criterion{{ID: "AC-1", Text: "Empty email is rejected", Source: plan.Source{Kind: plan.SourceUser, Quote: "no empty email"}, Risk: plan.RiskMedium}}

	subtasks := []plannedSubtask{{repoName: "m103-repo", steps: 1, titles: []string{"step one"}, item: f.subtaskFor(taskID), msg: msg}}
	if err := commitDecomposition(context.Background(), pool, wg, f.parentWorkID, nil, subtasks); err != nil {
		t.Fatalf("commitDecomposition: %v", err)
	}

	var stored []byte
	if err := pool.QueryRow(context.Background(), `SELECT message FROM plan_tasks WHERE id = $1`, taskID).Scan(&stored); err != nil {
		t.Fatalf("read plan_tasks.message: %v", err)
	}
	check := func(where string, payload []byte) {
		t.Helper()
		var back implementationTaskMessage
		if err := json.Unmarshal(payload, &back); err != nil {
			t.Fatalf("%s: decode: %v", where, err)
		}
		if back.TaskSpec == nil {
			t.Fatalf("%s: task_spec missing: %s", where, payload)
		}
		if err := back.TaskSpec.Validate(); err != nil {
			t.Fatalf("%s: task_spec invalid: %v", where, err)
		}
		if back.TaskSpec.Files[1].Evidence.Kind != plan.EvidenceHypothesis || back.TaskSpec.Deliverable != spec.Deliverable {
			t.Errorf("%s: task_spec changed: %+v", where, back.TaskSpec)
		}
		if len(back.Criteria) != 1 || back.Criteria[0].Source.Quote != "no empty email" {
			t.Errorf("%s: criteria: %+v", where, back.Criteria)
		}
	}
	check("plan_tasks.message", stored)

	var found bool
	for _, r := range rowsForAggregate(t, pool, f.parentWorkID) {
		if r.Topic == bus.TopicImplementationTasks && r.Key == taskID.String() {
			found = true
			check("outbox payload", r.Payload)
		}
	}
	if !found {
		t.Fatal("no implementation.tasks event for the released subtask")
	}
}
