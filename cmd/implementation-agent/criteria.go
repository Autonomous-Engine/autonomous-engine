package main

import (
	"log"
	"strings"
)

// Acceptance criteria on implementation.tasks (M2-04). The agent does not
// act on them yet: it logs the criterion ids it received and records them
// (and the deliverable) in the implementation summary artifact, so M3-07 can
// verify the run against the criteria the plan assigned. A message planned
// before M2-04 carries neither and logs that.

// criterionIDs are the ids the task cites: the TaskSpec's, else those of
// the criteria it carries.
func criterionIDs(task implementationTaskMessage) []string {
	if task.TaskSpec != nil && len(task.TaskSpec.CriterionIDs) > 0 {
		return task.TaskSpec.CriterionIDs
	}
	ids := make([]string, 0, len(task.Criteria))
	for _, c := range task.Criteria {
		ids = append(ids, c.ID)
	}
	return ids
}

// logTaskCriteria logs what the task was planned against.
func logTaskCriteria(task implementationTaskMessage) {
	ids := criterionIDs(task)
	switch {
	case task.TaskSpec == nil && len(task.Criteria) == 0:
		log.Printf("implementation-agent: task_id=%s carries no task spec (planned before M2-04, or built outside the planner)", task.TaskID)
	case len(ids) == 0:
		log.Printf("implementation-agent: task_id=%s cites no acceptance criteria (hypothesis task)", task.TaskID)
	default:
		log.Printf("implementation-agent: task_id=%s acceptance criteria received: %s", task.TaskID, strings.Join(ids, ","))
	}
}

// criteriaSummaryFields are the summary artifact's criteria fields:
// criterion_ids (comma-separated, "" when none) and deliverable.
func criteriaSummaryFields(task implementationTaskMessage) map[string]string {
	out := map[string]string{"criterion_ids": strings.Join(criterionIDs(task), ",")}
	if task.TaskSpec != nil {
		out["deliverable"] = task.TaskSpec.Deliverable
	}
	return out
}
