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

// These tests pin the M1-04 conversion of research-coordinator: the research
// fan-out (transition to research + every research.tasks event) commits in
// one transaction, the technical brief artifact commits with its
// technical-brief.ready event, research.additional goes through the outbox,
// and the service never publishes to Kafka directly.

func coordinatorSources(t *testing.T) map[string]string {
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
		out[name] = strings.ReplaceAll(string(b), "\r\n", "\n")
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
		body := string(src[fset.Position(fd.Pos()).Offset:fset.Position(fd.End()).Offset])
		return stripComments(strings.ReplaceAll(body, "\r\n", "\n"))
	}
	t.Fatalf("function %s not found in %s", name, file)
	return ""
}

func TestCoordinator_NeverPublishesDirectly(t *testing.T) {
	forbidden := []string{".Publish(", ".PublishMessage(", "publishJSON(", "PublishRequested("}
	for name, src := range coordinatorSources(t) {
		code := stripComments(src)
		for _, needle := range forbidden {
			if strings.Contains(code, needle) {
				t.Errorf("%s calls %s — research-coordinator events must go through the outbox", name, needle)
			}
		}
	}
}

func TestCoordinator_FanOutCommitsTransitionAndAllTasksInOneTransaction(t *testing.T) {
	body := funcSource(t, "main.go", "commitResearchDispatch")
	if n := strings.Count(body, "workgraph.WithTx("); n != 1 {
		t.Fatalf("commitResearchDispatch has %d WithTx calls, want 1", n)
	}
	for _, want := range []string{
		"wg.TransitionStateTx(ctx, tx, workID, models.WorkItemStateResearch,",
		"outboxEnqueueAll(ctx, tx, events...)",
		"researchTaskEvents(aggregateID, tasks)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("commitResearchDispatch does not contain %q", want)
		}
	}
	if strings.Contains(body, "outbox.Enqueue(") || strings.Contains(body, "outbox.EnqueueJSON(") {
		t.Error("commitResearchDispatch enqueues tasks one at a time; the fan-out must be one EnqueueAll")
	}
	if !strings.Contains(stripComments(coordinatorSources(t)["main.go"]), "var outboxEnqueueAll = outbox.EnqueueAll") {
		t.Error("outboxEnqueueAll is not outbox.EnqueueAll")
	}

	handler := funcSource(t, "main.go", "handlePRDReady")
	if strings.Contains(handler, "wg.TransitionState(") {
		t.Error("handlePRDReady still transitions to research outside the fan-out transaction")
	}
	dispatch := funcSource(t, "main.go", "dispatchResearchRound")
	if strings.Contains(dispatch, "wg.TransitionState(") {
		t.Error("dispatchResearchRound transitions to research outside the fan-out transaction")
	}
	if !strings.Contains(handler, "dispatchResearchRound(") || !strings.Contains(dispatch, "commitResearchDispatch(") {
		t.Error("handlePRDReady does not dispatch through dispatchResearchRound → commitResearchDispatch")
	}
}

func TestCoordinator_TechnicalBriefCommitsArtifactAndEventTogether(t *testing.T) {
	body := funcSource(t, "main.go", "commitTechnicalBrief")
	if n := strings.Count(body, "workgraph.WithTx("); n != 1 {
		t.Fatalf("commitTechnicalBrief has %d WithTx calls, want 1", n)
	}
	for _, want := range []string{
		"wg.AddArtifactTx(ctx, tx, artifact)",
		"outbox.EnqueueJSON(ctx, tx, aggregateID, bus.TopicTechnicalBriefReady, msg.WorkID, msg)",
		"wg.CompleteAssignmentTx(ctx, tx, assignmentID, models.AssignmentStatusCompleted)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("commitTechnicalBrief does not contain %q", want)
		}
	}
	handler := funcSource(t, "main.go", "handleResearchResult")
	finish := funcSource(t, "main.go", "finishRound")
	if strings.Contains(handler, "wg.AddArtifact(") || strings.Contains(finish, "wg.AddArtifact(") {
		t.Error("the brief artifact is still saved outside its event's transaction")
	}
	if !strings.Contains(finish, "commitTechnicalBrief(") {
		t.Error("finishRound does not commit through commitTechnicalBrief")
	}
	if !strings.Contains(handler, "finishRound(") {
		t.Error("handleResearchResult does not finish a claimed round through finishRound")
	}
	if !strings.Contains(handler, "enqueueFollowup(") {
		t.Error("handleResearchResult does not enqueue research.additional through enqueueFollowup")
	}
	followup := funcSource(t, "main.go", "enqueueFollowup")
	if !strings.Contains(followup, "workgraph.WithTx(") ||
		!strings.Contains(followup, "outbox.EnqueueJSON(ctx, tx, aggregateID, bus.TopicResearchAdditional, task.TaskID, task)") {
		t.Error("enqueueFollowup does not enqueue research.additional in a short transaction")
	}
	if !strings.Contains(followup, "insertRoundTaskTx(ctx, tx, roundID,") {
		t.Error("enqueueFollowup does not add the follow-up to its round in the same transaction")
	}
}

// R2/B2: the round (PRD hash + dispatched set) is persisted in the dispatch
// transaction, and a result's round completion is judged from the persisted
// round, not from in-memory session state.
func TestCoordinator_RoundPersistedWithDispatchAndJudgedFromStore(t *testing.T) {
	body := funcSource(t, "main.go", "commitResearchDispatch")
	if !strings.Contains(body, "insertRoundTx(ctx, tx, round)") {
		t.Error("commitResearchDispatch does not persist the round in its transaction")
	}
	handler := funcSource(t, "main.go", "handleResearchResult")
	if !strings.Contains(handler, "recordOrBlock(msgCtx, rounds, result,") {
		t.Error("handleResearchResult does not record results on the persisted round through recordOrBlock")
	}
	if !strings.Contains(handler, "blockInOwnTx(") {
		t.Error("handleResearchResult does not block the item when recording keeps failing")
	}
	for _, stale := range []string{"coord.getOrCreate(", "session.taskIDs", "session.results"} {
		if strings.Contains(handler, stale) {
			t.Errorf("handleResearchResult still judges convergence from in-memory state (%s)", stale)
		}
	}
	prd := funcSource(t, "main.go", "handlePRDReady") + funcSource(t, "main.go", "dispatchResearchRound")
	for _, want := range []string{"selectResearchTypes(", "prepareRound(", "resolveSeeds(", "budgetHintTokens(tools.TokenBudget()"} {
		if !strings.Contains(prd, want) {
			t.Errorf("handlePRDReady/dispatchResearchRound do not call %s", want)
		}
	}
	// The all-reused path records prd.ready's guard with the brief, not the
	// dispatch (a crash during synthesis must leave prd.ready redeliverable).
	if !strings.Contains(prd, "idempotency.Wrap(idempotency.Unguarded(msgCtx), pool)") {
		t.Error("the all-reused dispatch is not unguarded")
	}
	if brief := funcSource(t, "main.go", "commitTechnicalBrief"); !strings.Contains(brief, "closeRoundTx(ctx, tx, roundID, roundCompleted)") {
		t.Error("commitTechnicalBrief does not close the round in its transaction")
	}
	if followup := funcSource(t, "main.go", "enqueueFollowup"); !strings.Contains(followup, "lockOpenRoundTx(ctx, tx, roundID)") {
		t.Error("enqueueFollowup does not lock the round and require it open")
	}
	if strings.Contains(prd, `case "bug":`) {
		t.Error("handlePRDReady still has the fixed per-work-type switch")
	}
}

func TestCoordinator_StartsAndWaitsForItsDispatcher(t *testing.T) {
	code := stripComments(coordinatorSources(t)["main.go"])
	if n := strings.Count(code, "outbox.StartForService("); n != 1 {
		t.Errorf("found %d outbox.StartForService calls, want 1", n)
	}
	if !strings.Contains(code, "outbox.StartForService(ctx, pool, kb, log.Default(), agentID)") {
		t.Error("dispatcher is not named after research-coordinator")
	}
	if n := strings.Count(code, "dispatcher.Wait()"); n != 1 {
		t.Errorf("found %d dispatcher.Wait() calls, want 1", n)
	}
}

// research.tasks events carry the plain message (plus event_id), keyed by
// task id, under the work item's aggregate.
func TestResearchTaskEvents_AddressedPerTaskUnderOneAggregate(t *testing.T) {
	agg := uuid.New()
	tasks := []researchTaskMessage{
		{TaskID: uuid.NewString(), WorkID: agg.String(), ResearchType: "impact_assessment"},
		{TaskID: uuid.NewString(), WorkID: agg.String(), ResearchType: "pattern_match"},
	}
	evs, err := researchTaskEvents(agg, tasks)
	if err != nil {
		t.Fatalf("researchTaskEvents: %v", err)
	}
	if len(evs) != len(tasks) {
		t.Fatalf("got %d events, want %d", len(evs), len(tasks))
	}
	for i, ev := range evs {
		if ev.Topic != "research.tasks" || ev.Key != tasks[i].TaskID || ev.AggregateID != agg || ev.AggregateType != "work_item" {
			t.Errorf("event %d addressed %s/%s/%s/%s", i, ev.AggregateType, ev.AggregateID, ev.Topic, ev.Key)
		}
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
