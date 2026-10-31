package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/google/uuid"
)

// These tests pin the M1-04 conversion of product-agent: the PRD artifact,
// the work item title, the transition to prd_approval, the PRD approval
// prompt and the assignment completion commit in one transaction, and the
// service never publishes to Kafka directly.

func agentSources(t *testing.T) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}
	out := map[string]string{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		out[name] = string(b)
	}
	if len(out) == 0 {
		t.Fatal("found no sources to scan")
	}
	return out
}

var (
	lineComment  = regexp.MustCompile(`(?m)//.*$`)
	blockComment = regexp.MustCompile(`(?s)/\*.*?\*/`)
)

func stripComments(src string) string {
	return lineComment.ReplaceAllString(blockComment.ReplaceAllString(src, " "), " ")
}

func funcSource(t *testing.T, file, name string) string {
	t.Helper()
	fset := token.NewFileSet()
	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	f, err := parser.ParseFile(fset, file, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Recv != nil || fd.Name.Name != name {
			continue
		}
		return stripComments(string(src[fset.Position(fd.Pos()).Offset:fset.Position(fd.End()).Offset]))
	}
	t.Fatalf("function %s not found in %s", name, file)
	return ""
}

func TestProductAgent_NeverPublishesDirectly(t *testing.T) {
	forbidden := []string{".Publish(", ".PublishMessage(", "publishJSON(", "PublishRequested("}
	for name, src := range agentSources(t) {
		code := stripComments(src)
		for _, needle := range forbidden {
			if strings.Contains(code, needle) {
				t.Errorf("%s calls %s — product-agent events must go through the outbox", name, needle)
			}
		}
	}
}

func TestProductAgent_PRDCommitsInOneTransaction(t *testing.T) {
	body := funcSource(t, "main.go", "commitPRD")
	if n := strings.Count(body, "workgraph.WithTx("); n != 1 {
		t.Fatalf("commitPRD has %d WithTx calls, want 1", n)
	}
	for _, want := range []string{
		"tx.Exec(ctx, updateTitleSQL,",
		"wg.AddArtifactTx(ctx, tx, artifact)",
		"wg.TransitionStateTx(ctx, tx, workID, models.WorkItemStatePRDApproval,",
		"pub.PublishTx(ctx, tx, aggregateID, prompt)",
		"wg.CompleteAssignmentTx(ctx, tx, assignmentID, models.AssignmentStatusCompleted)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("commitPRD does not contain %q", want)
		}
	}
	// Order: the prompt goes after the transition it announces.
	if strings.Index(body, "pub.PublishTx(") < strings.Index(body, "wg.TransitionStateTx(") {
		t.Error("commitPRD publishes the prompt before the transition to prd_approval")
	}

	handler := funcSource(t, "main.go", "buildHandler")
	for _, gone := range []string{
		"wg.AddArtifact(",
		"models.WorkItemStatePRDApproval",
		"UPDATE work_items SET title",
		"promptProductID := uuid.Nil",
	} {
		if strings.Contains(handler, gone) {
			t.Errorf("buildHandler still contains %q outside the PRD transaction", gone)
		}
	}
	if !strings.Contains(handler, "commitPRD(") {
		t.Error("buildHandler does not commit through commitPRD")
	}
}

// A prompt needs a product (user_interactions.product_id is NOT NULL with a
// foreign key). The ruling: resolve the product before the PRD transaction
// and block a product-less item with a clear reason in a transaction of its
// own, rather than let the prompt insert roll back the PRD.
func TestProductAgent_BlocksProductlessItemBeforeThePRD(t *testing.T) {
	handler := funcSource(t, "main.go", "buildHandler")
	noProduct := strings.Index(handler, "productID == nil")
	if noProduct < 0 {
		t.Fatal("buildHandler never checks for a missing product")
	}
	if strings.Index(handler, "llm.NewServiceForWorkItem(") < noProduct {
		t.Error("the missing-product check runs after LLM resolution; it must come first")
	}
	if !strings.Contains(handler, "blockInOwnTx(") {
		t.Error("buildHandler does not block through blockInOwnTx")
	}
	block := funcSource(t, "main.go", "blockInOwnTx")
	if !strings.Contains(block, "guards.BlockWorkItemTx(ctx, tx, aggregateID, workID, agentID, reason)") ||
		!strings.Contains(block, "workgraph.WithTx(") {
		t.Error("blockInOwnTx does not use guards.BlockWorkItemTx inside its own WithTx")
	}
}

func TestProductAgent_StartsAndWaitsForItsDispatcher(t *testing.T) {
	code := stripComments(agentSources(t)["main.go"])
	if n := strings.Count(code, "outbox.StartForService("); n != 1 {
		t.Errorf("found %d outbox.StartForService calls, want 1", n)
	}
	if !strings.Contains(code, `outbox.StartForService(ctx, pool, kb, log.Default(), agentID)`) {
		t.Error("dispatcher is not named after product-agent")
	}
	if n := strings.Count(code, "dispatcher.Wait()"); n != 1 {
		t.Errorf("found %d dispatcher.Wait() calls, want 1", n)
	}
}

func TestResolveProduct(t *testing.T) {
	pid := uuid.New()
	other := uuid.New()
	cases := []struct {
		name   string
		intake string
		wi     *models.WorkItem
		want   *uuid.UUID
	}{
		{"intake wins", pid.String(), &models.WorkItem{ProductID: &other}, &pid},
		{"falls back to the work item", "", &models.WorkItem{ProductID: &pid}, &pid},
		{"invalid intake falls back", "not-a-uuid", &models.WorkItem{ProductID: &pid}, &pid},
		{"nil intake uuid falls back", uuid.Nil.String(), &models.WorkItem{ProductID: &pid}, &pid},
		{"none anywhere", "", &models.WorkItem{}, nil},
		{"no work item", "", nil, nil},
		{"nil work item product", "", &models.WorkItem{ProductID: &uuid.Nil}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveProduct(tc.intake, tc.wi)
			switch {
			case tc.want == nil && got != nil:
				t.Errorf("got %s, want nil", got)
			case tc.want != nil && (got == nil || *got != *tc.want):
				t.Errorf("got %v, want %s", got, tc.want)
			}
		})
	}
}

func TestAggregateFor(t *testing.T) {
	id, parent := uuid.New(), uuid.New()
	if got := aggregateFor(id, nil); got != id {
		t.Errorf("no work item: got %s, want the item %s", got, id)
	}
	if got := aggregateFor(id, &models.WorkItem{ID: id}); got != id {
		t.Errorf("top-level: got %s, want %s", got, id)
	}
	if got := aggregateFor(id, &models.WorkItem{ID: id, ParentID: &parent}); got != parent {
		t.Errorf("subtask: got %s, want the parent %s", got, parent)
	}
}
