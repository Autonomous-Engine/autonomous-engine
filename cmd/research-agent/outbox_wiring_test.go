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

// These tests pin the M1-04 conversion of research-agent: every
// research.results event commits with the completion of the agent's
// assignment, and the only direct publishes left are the telemetry topics.

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

// telemetryTopics may still be published directly (common3: pure telemetry
// stays direct).
var telemetryTopics = []string{"bus.TopicToolExecuted", "bus.TopicConsoleStream"}

func TestResearchAgent_OnlyTelemetryIsPublishedDirectly(t *testing.T) {
	for name, src := range agentSources(t) {
		code := stripComments(src)
		for _, needle := range []string{"publishJSON(", ".PublishMessage(", "PublishRequested("} {
			if strings.Contains(code, needle) {
				t.Errorf("%s calls %s — research.results must go through the outbox", name, needle)
			}
		}
		for i, line := range strings.Split(code, "\n") {
			if !strings.Contains(line, ".Publish(") {
				continue
			}
			allowed := false
			for _, topic := range telemetryTopics {
				if strings.Contains(line, topic) {
					allowed = true
				}
			}
			if !allowed {
				t.Errorf("%s:%d publishes directly to a non-telemetry topic: %s", name, i+1, strings.TrimSpace(line))
			}
		}
	}
}

func TestResearchAgent_ResultCommitsWithAssignmentCompletion(t *testing.T) {
	body := funcSource(t, "main.go", "commitResearchResult")
	if n := strings.Count(body, "workgraph.WithTx("); n != 1 {
		t.Fatalf("commitResearchResult has %d WithTx calls, want 1", n)
	}
	for _, want := range []string{
		"wg.CompleteAssignmentTx(ctx, tx, assignmentID, status)",
		"outbox.EnqueueJSON(ctx, tx, aggregateID, bus.TopicResearchResults, key, result)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("commitResearchResult does not contain %q", want)
		}
	}
	handler := funcSource(t, "main.go", "buildHandler")
	// Seven outcomes report a result: no repo, pinned snapshot unavailable
	// (M2-03), no tool calling, tool loop failure, no evidence (R18),
	// rejected acceptance criteria (M2-04), success.
	if n := strings.Count(handler, "report("); n != 7 {
		t.Errorf("buildHandler reports %d results through report(), want 7", n)
	}
	if strings.Contains(handler, "wg.CompleteAssignment(msgCtx, assignment.ID, models.AssignmentStatusCompleted)") {
		t.Error("buildHandler completes the assignment outside the result's transaction")
	}
}

func TestResearchAgent_StartsAndWaitsForItsDispatcher(t *testing.T) {
	code := stripComments(agentSources(t)["main.go"])
	if n := strings.Count(code, "outbox.StartForService("); n != 1 {
		t.Errorf("found %d outbox.StartForService calls, want 1", n)
	}
	if !strings.Contains(code, "outbox.StartForService(ctx, pool, kb, log.Default(), agentID)") {
		t.Error("dispatcher is not named after research-agent")
	}
	if n := strings.Count(code, "dispatcher.Wait()"); n != 1 {
		t.Errorf("found %d dispatcher.Wait() calls, want 1", n)
	}
}

func TestAggregateFor(t *testing.T) {
	id, parent := uuid.New(), uuid.New()
	if got := aggregateFor(id, nil); got != id {
		t.Errorf("no work item: got %s, want %s", got, id)
	}
	if got := aggregateFor(id, &models.WorkItem{ID: id, ParentID: &parent}); got != parent {
		t.Errorf("subtask: got %s, want the parent %s", got, parent)
	}
}
