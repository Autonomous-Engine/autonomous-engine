package main

import (
	"context"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/idempotency"
	"github.com/autonomousengine/autonomous-engine/internal/llm/tools"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
)

// The findings a task recorded are stored in the transaction that commits
// its result, with the task, its research type and the product: a result
// that is recorded once stores them once, and a duplicate delivery, refused
// by the idempotency guard, stores nothing again.
func TestIntegration_ResearchResultCommitStoresItsFindings(t *testing.T) {
	pool := budgetIntegrationPool(t)
	ctx := context.Background()
	wg := workgraph.New(pool)
	workID, productID := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO products (id, name) VALUES ($1, 'findings product')`, productID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO work_items (id, type, state, title, product_id) VALUES ($1, 'feature', 'research', 'result findings', $2)`, workID, productID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM work_items WHERE id = $1`, workID)
		_, _ = pool.Exec(ctx, `DELETE FROM products WHERE id = $1`, productID)
	})
	assignment := &models.AgentAssignment{WorkItemID: workID, AgentType: models.AgentTypeResearch, AgentInstanceID: "test", Status: models.AssignmentStatusActive}
	if err := wg.AssignAgent(ctx, assignment); err != nil {
		t.Fatal(err)
	}
	result := researchResultMessage{
		TaskID: "task-1", WorkID: workID.String(), Status: ResearchStatusOK, FindingType: "code_trace", Evidence: "e",
		Findings: []tools.CitedFinding{
			{Claim: "the parser trims", Path: "a.go", StartLine: 3, EndLine: 4, Excerpt: "x"},
			{Claim: "the writer pads", Path: "b.go", StartLine: 1, EndLine: 1, Excerpt: "y"},
		},
	}
	// Both commits are the same delivery of research.tasks (one event id).
	ledger := idempotency.NewLedger(pool)
	msg := &bus.Message{Topic: bus.TopicResearchTasks, Key: workID.String(), Headers: map[string]string{bus.HeaderEventID: uuid.NewString()}}
	commit := func() error {
		dctx := idempotency.WithDelivery(ctx, ledger.NewDelivery(agentID, msg))
		return commitResearchResult(dctx, idempotency.Wrap(dctx, pool), wg, workID, assignment.ID, models.AssignmentStatusCompleted,
			"w-task-1", result, &productID)
	}
	if err := commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := commit(); !idempotency.IsDuplicate(err) {
		t.Fatalf("second commit err = %v, want the duplicate", err)
	}
	rows, err := pool.Query(ctx, `SELECT task_id, research_type, product_id, claim FROM research_findings WHERE work_item_id = $1 ORDER BY claim`, workID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var task, typ, claim string
		var pid *uuid.UUID
		if err := rows.Scan(&task, &typ, &pid, &claim); err != nil {
			t.Fatal(err)
		}
		if task != "task-1" || typ != "code_trace" || pid == nil || *pid != productID {
			t.Fatalf("row %q: task=%s type=%s product=%v", claim, task, typ, pid)
		}
		got = append(got, claim)
	}
	if len(got) != 2 || got[0] != "the parser trims" || got[1] != "the writer pads" {
		t.Fatalf("stored claims = %v, want the two, once", got)
	}
}
