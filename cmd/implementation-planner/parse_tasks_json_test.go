package main

import "testing"

const oneTask = `[{"title":"Add key","description":"uses ] and [ in text","files":["src/a.ts"]}]`

// parseTaskArray has the criteria parser's shape (first [ to last ]); it
// now also takes the first balanced array that decodes to tasks, so
// trailing garbage or a stray array in prose no longer fails the
// decomposition.
func TestParseTaskArrayTolerantEnvelope(t *testing.T) {
	cases := map[string]string{
		"trailing bracket": oneTask + "]",
		"trailing prose":   oneTask + "\nNote: see [1].",
		"two arrays":       oneTask + "\n" + `[{"title":"Other","description":"d"}]`,
		"fenced":           "```json\n" + oneTask + "\n```",
		"think tags":       "<think>[\"draft\"]</think>" + oneTask,
		"prose array":      "Files [1] and [2]:\n" + oneTask,
	}
	for name, content := range cases {
		tasks, err := parseTaskArray(content)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if len(tasks) != 1 || tasks[0].Title != "Add key" {
			t.Errorf("%s: tasks = %+v, want the first array's single task", name, tasks)
		}
	}
}

// R6 review: a truncated decomposition fails (and the nudge follows)
// instead of yielding an inner array as one empty task.
func TestParseTaskArrayTruncatedFails(t *testing.T) {
	tasks, err := parseTaskArray(`[{"title":"Add key","description":"d","files":["src/a.ts"]},{"title":"Docs","fi`)
	if err == nil {
		t.Fatalf("truncated array parsed as %+v", tasks)
	}
}

// Live, the repair round answered with ONE task object (the task it
// repaired) instead of the array; the bracket fallback then took the
// task's own "files" array as the plan — two tasks with no title, no
// criteria, no deliverable — and the planner blocked on 21 violations. An
// array whose elements are not tasks is not a plan; a single task object
// is a one-task plan.
func TestParseTaskArrayTakesASingleTaskObjectNotItsFilesArray(t *testing.T) {
	one := `{"title":"Implement option","description":"d","criterion_ids":["AC-1"],"files":[{"path":"options.go","evidence_ref":"x"},{"path":"parser.go","evidence_ref":"y"}],"depends_on":[]}`
	for name, content := range map[string]string{"bare": one, "fenced": "```json\n" + one + "\n```", "prose": "Here is the repaired task:\n" + one} {
		tasks, err := parseTaskArray(content)
		if err != nil || len(tasks) != 1 || tasks[0].Title != "Implement option" || len(tasks[0].FileClaims) != 2 {
			t.Errorf("%s: tasks = %+v err = %v, want the one task object", name, tasks, err)
		}
	}
	if tasks, err := parseTaskArray(`Files: [{"path":"a.go","evidence_ref":"x"},{"path":"b.go","evidence_ref":"y"}]`); err == nil {
		t.Fatalf("an array of non-tasks parsed as %+v", tasks)
	}
}
