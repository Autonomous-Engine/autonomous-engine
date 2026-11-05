package main

import "testing"

// The repair round's answer carried only the task it repaired (the model
// read "fix task 1" literally); the planner took it as the whole plan and
// the other three tasks' criteria went orphan. A repair answer that names
// a subset of the previous plan's tasks replaces those tasks in it.
func TestMergeRepairedTasksReplacesTheNamedTasks(t *testing.T) {
	previous := []decomposedTask{
		{Title: "Implement option", CriterionIDs: []string{"AC-1"}},
		{Title: "Apply mapping", CriterionIDs: []string{"AC-2"}},
		{Title: "Tests", CriterionIDs: []string{"AC-3"}},
	}
	repaired := []decomposedTask{{Title: "implement option ", CriterionIDs: []string{"AC-1", "AC-5"}, Deliverable: "d"}}
	merged, ok := mergeRepairedTasks(previous, repaired)
	if !ok || len(merged) != 3 || merged[0].Deliverable != "d" || len(merged[0].CriterionIDs) != 2 || merged[1].Title != "Apply mapping" || merged[2].Title != "Tests" {
		t.Fatalf("merged = %+v ok = %v, want the first task replaced and the others kept", merged, ok)
	}
	// A full plan, or a task the previous plan did not have, is taken as is.
	if _, ok := mergeRepairedTasks(previous, []decomposedTask{{Title: "New task"}}); ok {
		t.Fatal("an unknown task must not merge")
	}
	if _, ok := mergeRepairedTasks(previous, previous); ok {
		t.Fatal("a full plan must not merge")
	}
}
