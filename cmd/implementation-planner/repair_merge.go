package main

import "strings"

// mergeRepairedTasks merges a repair round's answer into the previous plan
// when the answer names only some of its tasks: the model read "fix task 1"
// literally and returned that task alone, and taking it as the whole plan
// orphaned every other task's criteria. Each returned task replaces the
// previous task with the same title (trimmed, case-insensitive); the rest
// of the plan stands. A full plan, or a task the previous plan did not
// have, is not merged (false): it is the new plan as the model gave it.
func mergeRepairedTasks(previous, repaired []decomposedTask) ([]decomposedTask, bool) {
	if len(previous) == 0 || len(repaired) == 0 || len(repaired) >= len(previous) {
		return nil, false
	}
	key := func(title string) string { return strings.ToLower(strings.TrimSpace(title)) }
	index := map[string]int{}
	for i, p := range previous {
		index[key(p.Title)] = i
	}
	merged := append([]decomposedTask(nil), previous...)
	for _, r := range repaired {
		i, ok := index[key(r.Title)]
		if !ok {
			return nil, false
		}
		r.index = merged[i].index
		merged[i] = r
	}
	return merged, true
}
