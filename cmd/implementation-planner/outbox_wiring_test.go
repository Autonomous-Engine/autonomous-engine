package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/outbox"
	"github.com/google/uuid"
)

// plannerSources returns the planner's non-test Go sources, as text.
func plannerSources(t *testing.T) map[string]string {
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
		t.Fatal("found no planner sources to scan")
	}
	return out
}

// stripComments removes // and /* */ comments so a prose mention of the old
// mechanism (there are several, explaining why it went away) cannot fail a
// test that is about code.
var lineComment = regexp.MustCompile(`(?m)//.*$`)
var blockComment = regexp.MustCompile(`(?s)/\*.*?\*/`)

func stripComments(src string) string {
	return lineComment.ReplaceAllString(blockComment.ReplaceAllString(src, " "), " ")
}

// TestPlanner_NeverPublishesImplementationTasksDirectly is the guard on the
// M1-03 conversion.
//
// The defect this ticket closes is that a direct publish can fail while the
// subtask row has already been committed, leaving work nobody will ever pick
// up. Removing the two publish calls fixes it once; this test keeps it fixed,
// because re-adding one is a single plausible-looking line and the failure it
// reintroduces is invisible until a broker has a bad day in production.
//
// The rule is deliberately blunt: the planner must not call Publish or
// PublishMessage at all. Every event it emits belongs in the outbox.
func TestPlanner_NeverPublishesImplementationTasksDirectly(t *testing.T) {
	forbidden := []string{".Publish(", ".PublishMessage(", "publishJSON("}
	for name, src := range plannerSources(t) {
		code := stripComments(src)
		for _, needle := range forbidden {
			if strings.Contains(code, needle) {
				t.Errorf("%s calls %s — implementation.tasks must reach Kafka only through the outbox, so that the event and the subtask row share a commit", name, needle)
			}
		}
	}
}

// TestPlanner_CommitsTheWholePlanInOneTransaction guards the unit of
// atomicity. A transaction per repo group still satisfies "no subtask without
// its event", so the integration tests would pass — but it lets a failure
// partway through leave earlier groups committed and dispatched while the
// parent is blocked asking for a retry, and the retry duplicates them. The
// only Begin on the decomposition path must be the one inside
// commitDecomposition.
func TestPlanner_CommitsTheWholePlanInOneTransaction(t *testing.T) {
	var begins int
	for name, src := range plannerSources(t) {
		code := stripComments(src)
		if n := strings.Count(code, "pool.Begin("); n > 0 && name != "main.go" {
			t.Errorf("%s begins a transaction; the decomposition path lives in main.go", name)
		}
		begins += strings.Count(code, "pool.Begin(")
	}
	// One: commitDecomposition (the plan). Completions and releases (M1-06)
	// run through workgraph.WithTx in plan_tasks.go, each one transaction.
	if begins != 1 {
		t.Errorf("found %d pool.Begin( call(s) in the planner, want exactly 1 (commitDecomposition)", begins)
	}

	for _, src := range plannerSources(t) {
		code := stripComments(src)
		if strings.Contains(code, "createSubtaskWithEvent") {
			t.Error("the per-subtask transaction createSubtaskWithEvent is back; a plan must commit as one unit")
		}
		if !strings.Contains(code, "outboxEnqueueAll(ctx, tx, events...)") {
			continue
		}
		return // found the plan-level enqueue
	}
	t.Error("commitDecomposition does not enqueue every event in one call; the plan's events must share the transaction")
}

// TestPlanner_EnqueuesTheUnchangedTopic checks the other half of the
// compatibility promise: the conversion changed how the message is delivered,
// not where it goes. A consumer subscribed to implementation.tasks must not
// need to know the outbox exists.
func TestPlanner_EnqueuesTheUnchangedTopic(t *testing.T) {
	if bus.TopicImplementationTasks != "implementation.tasks" {
		t.Fatalf("TopicImplementationTasks = %q; the M1-03 conversion promised consumers an unchanged topic", bus.TopicImplementationTasks)
	}
	found := false
	for _, src := range plannerSources(t) {
		if strings.Contains(stripComments(src), "outbox.NewEvent(\"work_item\"") {
			found = true
		}
	}
	if !found {
		t.Error("no outbox.NewEvent(\"work_item\", ...) call in the planner; the task events are not being enqueued against the work-item aggregate")
	}
}

// TestPlanner_StartsExactlyOneDispatcher pins the wiring in main(): the
// dispatcher must be started, and started once. Zero dispatchers means planned
// work is committed to a table nobody drains — silently, because the
// decomposition itself still succeeds. Two in one process would share the
// unlabelled ae_outbox_* gauges and race to overwrite them.
func TestPlanner_StartsExactlyOneDispatcher(t *testing.T) {
	starts := 0
	for _, src := range plannerSources(t) {
		starts += strings.Count(stripComments(src), "outbox.Start(") +
			strings.Count(stripComments(src), "outbox.StartWithOptions(")
	}
	if starts != 1 {
		t.Errorf("found %d outbox dispatcher start call(s) in the planner, want exactly 1", starts)
	}
}

// TestPlanner_WaitsForTheDispatcherOnShutdown checks that main() does not exit
// out from under an in-flight publish. Without the Wait, a SIGTERM between the
// broker ack and the MarkPublished write leaves a published row looking
// pending, and the event is delivered a second time for no reason.
func TestPlanner_WaitsForTheDispatcherOnShutdown(t *testing.T) {
	waits := 0
	for _, src := range plannerSources(t) {
		waits += strings.Count(stripComments(src), "dispatcher.Wait()")
	}
	if waits != 1 {
		t.Errorf("found %d dispatcher.Wait() call(s) in the planner, want exactly 1 on the shutdown path", waits)
	}
}

// TestTaskEvent_CarriesTheAdditiveEventIDOnly proves requirement 3 on the
// message itself rather than through the database: building the event adds
// event_id and changes nothing else, so an existing consumer decoding
// implementationTaskMessage is unaffected.
func TestTaskEvent_CarriesTheAdditiveEventIDOnly(t *testing.T) {
	msg := implementationTaskMessage{
		TaskID:        "11111111-1111-4111-8111-111111111111",
		WorkID:        "22222222-2222-4222-8222-222222222222",
		ProductID:     "33333333-3333-4333-8333-333333333333",
		RepositoryID:  "44444444-4444-4444-8444-444444444444",
		Title:         "a title",
		Description:   "a description",
		FilesToChange: []string{"a.ts"},
		CodexVersion:  3,
	}
	parent, err := uuid.Parse(msg.WorkID)
	if err != nil {
		t.Fatalf("parse work id: %v", err)
	}

	ev, err := outbox.NewEvent("work_item", parent, bus.TopicImplementationTasks, msg.TaskID, msg)
	if err != nil {
		t.Fatalf("NewEvent: %v", err)
	}
	if ev.Topic != bus.TopicImplementationTasks {
		t.Errorf("topic = %q, want %q", ev.Topic, bus.TopicImplementationTasks)
	}
	if ev.Key != msg.TaskID {
		t.Errorf("key = %q, want the task id %q", ev.Key, msg.TaskID)
	}
	if ev.AggregateID != parent {
		t.Errorf("aggregate_id = %s, want the parent work id %s", ev.AggregateID, parent)
	}
	if ev.Headers[outbox.HeaderEventID] != ev.ID.String() {
		t.Errorf("%s header = %q, want %q", outbox.HeaderEventID, ev.Headers[outbox.HeaderEventID], ev.ID)
	}

	var fields map[string]any
	if err := json.Unmarshal(ev.Payload, &fields); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if fields[outbox.FieldEventID] != ev.ID.String() {
		t.Errorf("payload event_id = %v, want %s", fields[outbox.FieldEventID], ev.ID)
	}
	delete(fields, outbox.FieldEventID)

	var baseline map[string]any
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal baseline: %v", err)
	}
	if err := json.Unmarshal(raw, &baseline); err != nil {
		t.Fatalf("decode baseline: %v", err)
	}
	if len(fields) != len(baseline) {
		t.Errorf("payload has %d fields besides event_id, the plain message has %d — the wire format changed", len(fields), len(baseline))
	}
	for k, want := range baseline {
		if got, ok := fields[k]; !ok {
			t.Errorf("payload lost field %q", k)
		} else if !reflect.DeepEqual(got, want) {
			t.Errorf("payload field %q = %v, want %v", k, got, want)
		}
	}
}

// TestPlanner_KeepsNoPlanStateInMemory guards M1-06: the held-task queue, the
// completed set and the title -> task id map lived in process memory, so a
// restart lost them and replicas could not share them. They live in
// plan_tasks now; any of them coming back means a second, volatile source of
// truth.
func TestPlanner_KeepsNoPlanStateInMemory(t *testing.T) {
	for name, src := range plannerSources(t) {
		code := stripComments(src)
		for _, needle := range []string{"heldTasks", "titleToID", "markCompleted(", "rehold(", "type planner struct", "enqueueTaskEvent("} {
			if strings.Contains(code, needle) {
				t.Errorf("%s still contains %q; plan execution state belongs in plan_tasks", name, needle)
			}
		}
	}
}

// TestPlanner_ReleasesThroughThePlanTables pins the shape of the release
// path: completions and the startup scan release through plan_tasks in a
// transaction, and the release event id is the deterministic one.
func TestPlanner_ReleasesThroughThePlanTables(t *testing.T) {
	srcs := plannerSources(t)
	code := stripComments(srcs["plan_tasks.go"])
	for _, want := range []string{
		"workgraph.WithTx(ctx, db,",
		"releaseReadyTasksTx(ctx, tx, parent)",
		"guards.BlockWorkItemTx(ctx, tx, parent, parent,",
		"FOR UPDATE",
		"releaseEventID(taskID)",
	} {
		if !strings.Contains(code, want) {
			t.Errorf("plan_tasks.go does not contain %q", want)
		}
	}
	if !strings.Contains(stripComments(srcs["main.go"]), "releaseReadyOnStartup(ctx, pool)") {
		t.Error("main() does not run startup reconstruction")
	}
}
