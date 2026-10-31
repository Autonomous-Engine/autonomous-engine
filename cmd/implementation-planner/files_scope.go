package main

import (
	"log"

	"github.com/autonomousengine/autonomous-engine/internal/plan"
)

// plannedFilesToChange is a subtask's files_to_change.
//
// The grounded step files that exist (or could not be checked) are
// intersected with research's affected_files as before (intersectOrRetain:
// the brief's set when the steps named none, the steps' set when the
// intersection is empty); with neither, paths are recovered from the
// description. The result is then widened to plan.ScopeFiles: every
// repository file the steps cite is in scope, so a step whose files
// research did not list is still work the implementer must do (a live
// run, attempt 6, planned eight steps over seven files but files_to_change named
// one, and the implementer stopped after three). A path grounding could not
// find stays out, kept in the TaskSpec as a flagged hypothesis.
func plannedFilesToChange(grounded []groundedFile, affected []string, desc string, spec *plan.TaskSpec) []string {
	var validated []string
	for _, g := range grounded {
		if g.status == fileExists || g.status == fileUnverified {
			validated = append(validated, g.ref.Path)
		}
	}
	own := intersectOrRetain(validated, affected)
	// Fallback: both the steps' files and the research brief's
	// affected_files were empty — the model's prose descriptions often
	// reference specific file paths even when the structured `files` field
	// is omitted.
	if len(own) == 0 {
		own = extractFilePathsFromDescription(desc)
		if len(own) > 0 {
			log.Printf("implementation-planner: recovered %d file paths from description text (structured files field was empty): %v", len(own), own)
		}
	}
	return plan.ScopeFiles(own, spec)
}
