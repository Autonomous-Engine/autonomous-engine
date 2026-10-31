package main

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func node(label string, titles []string, dependsOn ...string) planNode {
	return planNode{label: label, titles: titles, dependsOn: dependsOn}
}

func TestResolvePlanDependencies_NoDependencies(t *testing.T) {
	nodes := []planNode{
		node("api", []string{"Add endpoint"}),
		node("web", []string{"Add page"}),
	}
	got, err := resolvePlanDependencies(nodes, titleSteps([]string{"Add endpoint", "Add page"}...))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if want := [][]int{nil, nil}; !reflect.DeepEqual(got.deps, want) {
		t.Errorf("deps = %v, want %v", got.deps, want)
	}
	if len(got.dropped) != 0 {
		t.Errorf("dropped = %v, want none", got.dropped)
	}
}

// A dependency between two granular steps that landed in the same repo-scoped
// subtask is ordering inside one agent run (it is in the description), not an
// edge between subtasks.
func TestResolvePlanDependencies_IntraSubtaskDependencyIsNotAnEdge(t *testing.T) {
	nodes := []planNode{
		node("api", []string{"Add model", "Add endpoint"}, "Add model"),
	}
	got, err := resolvePlanDependencies(nodes, titleSteps([]string{"Add model", "Add endpoint"}...))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.deps[0] != nil {
		t.Errorf("deps[0] = %v, want none", got.deps[0])
	}
}

func TestResolvePlanDependencies_CrossSubtaskEdgesAreResolvedAndDeduplicated(t *testing.T) {
	nodes := []planNode{
		node("api", []string{"Add model", "Add endpoint"}),
		// Two titles of the api subtask, one spelled differently: one edge.
		node("web", []string{"Add page"}, "Add endpoint", "  add MODEL ", ""),
		node("docs-site", []string{"Link page"}, "Add page", "Add model"),
	}
	got, err := resolvePlanDependencies(nodes, titleSteps([]string{"Add model", "Add endpoint", "Add page", "Link page"}...))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if want := [][]int{nil, {0}, {0, 1}}; !reflect.DeepEqual(got.deps, want) {
		t.Errorf("deps = %v, want %v", got.deps, want)
	}
}

func TestResolvePlanDependencies_UnknownTitleFails(t *testing.T) {
	nodes := []planNode{
		node("api", []string{"Add endpoint"}),
		node("web", []string{"Add page"}, "Add endpiont"),
	}
	_, err := resolvePlanDependencies(nodes, titleSteps([]string{"Add endpoint", "Add page"}...))
	if !errors.Is(err, errUnknownDependency) {
		t.Fatalf("err = %v, want errUnknownDependency", err)
	}
	for _, want := range []string{`"web"`, `"Add endpiont"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err, want)
		}
	}
}

// A title the LLM produced but the planner dropped (documentation filter,
// test-only filter, no repository) will never run; depending on it is
// recorded and ignored rather than blocking the rest of the plan forever.
func TestResolvePlanDependencies_DroppedTitleIsIgnoredAndReported(t *testing.T) {
	nodes := []planNode{
		node("api", []string{"Add endpoint"}, "Write documentation"),
	}
	got, err := resolvePlanDependencies(nodes, titleSteps([]string{"Add endpoint", "Write documentation"}...))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.deps[0] != nil {
		t.Errorf("deps[0] = %v, want none", got.deps[0])
	}
	if want := []string{"Write documentation"}; !reflect.DeepEqual(got.dropped, want) {
		t.Errorf("dropped = %v, want %v", got.dropped, want)
	}
}

// A -> D -> B with D dropped (say, by the test-only filter): the ordering ran
// through D, so A inherits D's dependency and still waits for B.
func TestResolvePlanDependencies_InheritsThroughADroppedStep(t *testing.T) {
	nodes := []planNode{
		node("web", []string{"A"}, "D"),
		node("api", []string{"B"}),
	}
	steps := []planStep{
		{title: "A", dependsOn: []string{"D"}},
		{title: "D", dependsOn: []string{"B"}},
		{title: "B"},
	}
	got, err := resolvePlanDependencies(nodes, steps)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if want := [][]int{{1}, nil}; !reflect.DeepEqual(got.deps, want) {
		t.Errorf("deps = %v, want %v (A -> B through dropped D)", got.deps, want)
	}
	if want := []string{"D"}; !reflect.DeepEqual(got.dropped, want) {
		t.Errorf("dropped = %v, want %v", got.dropped, want)
	}
	if len(got.inherited) != 1 || !strings.Contains(got.inherited[0], `"D"`) {
		t.Errorf("inherited = %v, want one edge naming D", got.inherited)
	}
}

// Inheritance is transitive across several dropped steps, survives a cycle
// among dropped steps, and still rejects an unknown title found on the way.
func TestResolvePlanDependencies_InheritsTransitivelyAndChecksWhatItFinds(t *testing.T) {
	nodes := []planNode{
		node("web", []string{"A"}, "D1"),
		node("api", []string{"B"}),
	}
	steps := []planStep{
		{title: "A"}, {title: "B"},
		{title: "D1", dependsOn: []string{"D2"}},
		{title: "D2", dependsOn: []string{"D1", "b"}},
	}
	got, err := resolvePlanDependencies(nodes, steps)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if want := [][]int{{1}, nil}; !reflect.DeepEqual(got.deps, want) {
		t.Errorf("deps = %v, want %v (A -> B through D1, D2)", got.deps, want)
	}

	steps[3].dependsOn = []string{"Nope"}
	if _, err := resolvePlanDependencies(nodes, steps); !errors.Is(err, errUnknownDependency) || !strings.Contains(err.Error(), "through dropped step") {
		t.Errorf("err = %v, want errUnknownDependency naming the dropped step", err)
	}

	// An inherited edge can close a cycle; it is rejected like any other.
	nodes[1].dependsOn = []string{"A"}
	steps[3].dependsOn = []string{"B"}
	if _, err := resolvePlanDependencies(nodes, steps); !errors.Is(err, errDependencyCycle) {
		t.Errorf("err = %v, want errDependencyCycle", err)
	}
}

func TestResolvePlanDependencies_AmbiguousTitleFails(t *testing.T) {
	nodes := []planNode{
		node("api", []string{"Wire config"}),
		node("worker", []string{"Wire config"}),
		node("web", []string{"Add page"}, "Wire config"),
	}
	_, err := resolvePlanDependencies(nodes, titleSteps([]string{"Wire config", "Wire config", "Add page"}...))
	if !errors.Is(err, errAmbiguousDependency) {
		t.Fatalf("err = %v, want errAmbiguousDependency", err)
	}
}

func TestResolvePlanDependencies_CycleFails(t *testing.T) {
	cases := map[string][]planNode{
		"two": {
			node("api", []string{"A"}, "B"),
			node("web", []string{"B"}, "A"),
		},
		"three": {
			node("api", []string{"A"}, "C"),
			node("web", []string{"B"}, "A"),
			node("worker", []string{"C"}, "B"),
		},
		// Each step is acyclic on its own; grouping by repository makes the
		// subtasks depend on each other.
		"induced by grouping": {
			node("api", []string{"A1", "A2"}, "B"),
			node("web", []string{"B"}, "A1"),
		},
	}
	for name, nodes := range cases {
		t.Run(name, func(t *testing.T) {
			var known []string
			for _, n := range nodes {
				known = append(known, n.titles...)
			}
			_, err := resolvePlanDependencies(nodes, titleSteps(known...))
			if !errors.Is(err, errDependencyCycle) {
				t.Fatalf("err = %v, want errDependencyCycle", err)
			}
			if !strings.Contains(err.Error(), " -> ") {
				t.Errorf("error %q does not spell out the cycle", err)
			}
		})
	}
}

func TestFindDependencyCycle(t *testing.T) {
	cases := []struct {
		name string
		deps [][]int
		want bool
	}{
		{"empty", nil, false},
		{"chain", [][]int{nil, {0}, {1}}, false},
		{"diamond", [][]int{nil, {0}, {0}, {1, 2}}, false},
		{"self", [][]int{{0}}, true},
		{"back edge", [][]int{{2}, {0}, {1}}, true},
		{"cycle off a DAG", [][]int{nil, {0, 3}, {1}, {2}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cycle := findDependencyCycle(tc.deps)
			if got := cycle != nil; got != tc.want {
				t.Fatalf("findDependencyCycle(%v) = %v, want cycle=%t", tc.deps, cycle, tc.want)
			}
			if cycle == nil {
				return
			}
			if cycle[0] != cycle[len(cycle)-1] {
				t.Errorf("cycle %v does not close on itself", cycle)
			}
			for i := 0; i+1 < len(cycle); i++ {
				if !containsInt(tc.deps[cycle[i]], cycle[i+1]) {
					t.Errorf("cycle %v uses a non-edge %d -> %d", cycle, cycle[i], cycle[i+1])
				}
			}
		})
	}
}

func TestApplyPlanDependencies_SetsIDsAndMessage(t *testing.T) {
	ids := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}
	planned := make([]plannedSubtask, 3)
	for i, title := range []string{"A", "B", "C"} {
		planned[i] = plannedSubtask{
			titles: []string{title},
			msg:    implementationTaskMessage{TaskID: ids[i].String(), Title: title},
		}
	}
	planned[1].dependsOnTitles = []string{"A"}
	planned[2].dependsOnTitles = []string{"B", "Docs"}

	notes, err := applyPlanDependencies(planned, titleSteps([]string{"A", "B", "C", "Docs"}...))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !reflect.DeepEqual(notes.dropped, []string{"Docs"}) {
		t.Errorf("dropped = %v", notes.dropped)
	}
	if len(planned[0].dependsOn) != 0 || planned[0].msg.Dependencies != nil {
		t.Errorf("A has dependencies %v / %v", planned[0].dependsOn, planned[0].msg.Dependencies)
	}
	if !reflect.DeepEqual(planned[1].dependsOn, []uuid.UUID{ids[0]}) ||
		!reflect.DeepEqual(planned[1].msg.Dependencies, []string{ids[0].String()}) {
		t.Errorf("B depends on %v / %v, want A", planned[1].dependsOn, planned[1].msg.Dependencies)
	}
	if !reflect.DeepEqual(planned[2].dependsOn, []uuid.UUID{ids[1]}) {
		t.Errorf("C depends on %v, want B", planned[2].dependsOn)
	}
	if planned[0].initialStatus() != planTaskReleased || planned[2].initialStatus() != planTaskHeld {
		t.Errorf("initial statuses = %s, %s; want released, held", planned[0].initialStatus(), planned[2].initialStatus())
	}
}

func TestApplyPlanDependencies_RejectsACycleWithoutTouchingThePlan(t *testing.T) {
	planned := []plannedSubtask{
		{titles: []string{"A"}, dependsOnTitles: []string{"B"}, msg: implementationTaskMessage{TaskID: uuid.NewString(), Title: "A"}},
		{titles: []string{"B"}, dependsOnTitles: []string{"A"}, msg: implementationTaskMessage{TaskID: uuid.NewString(), Title: "B"}},
	}
	if _, err := applyPlanDependencies(planned, titleSteps([]string{"A", "B"}...)); !errors.Is(err, errDependencyCycle) {
		t.Fatalf("err = %v, want errDependencyCycle", err)
	}
	for i, ps := range planned {
		if ps.dependsOn != nil || ps.msg.Dependencies != nil {
			t.Errorf("planned[%d] was modified by a rejected plan", i)
		}
	}
}

// The release event id is a pure function of the task id, so releasing the
// same task twice (startup reconstruction racing a completion, or a replayed
// completion) writes the same outbox row and the second insert is a no-op.
func TestReleaseEventID_IsDeterministicPerTask(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	if releaseEventID(a) != releaseEventID(a) {
		t.Error("releaseEventID is not deterministic")
	}
	if releaseEventID(a) == releaseEventID(b) {
		t.Error("two tasks share a release event id")
	}
	if releaseEventID(a) == a {
		t.Error("release event id must not be the task id itself")
	}
}

func TestReleaseEvent_CarriesTheDeterministicIDAndUnchangedMessage(t *testing.T) {
	parent, task := uuid.New(), uuid.New()
	msg := implementationTaskMessage{TaskID: task.String(), WorkID: parent.String(), Title: "B", Dependencies: []string{uuid.NewString()}}
	stored, err := encodeStoredMessage(msg)
	if err != nil {
		t.Fatal(err)
	}
	ev, err := releaseEvent(parent, task, stored)
	if err != nil {
		t.Fatalf("releaseEvent: %v", err)
	}
	if ev.ID != releaseEventID(task) {
		t.Errorf("event id = %s, want %s", ev.ID, releaseEventID(task))
	}
	if ev.AggregateID != parent || ev.Key != task.String() || ev.Topic != "implementation.tasks" {
		t.Errorf("addressed as aggregate=%s key=%s topic=%s", ev.AggregateID, ev.Key, ev.Topic)
	}
	again, err := releaseEvent(parent, task, stored)
	if err != nil || again.ID != ev.ID || string(again.Payload) != string(ev.Payload) {
		t.Errorf("a second release built a different event (%v)", err)
	}
	if !strings.Contains(string(ev.Payload), `"event_id":"`+ev.ID.String()+`"`) {
		t.Errorf("payload does not carry the event id: %s", ev.Payload)
	}
}
