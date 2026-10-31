package main

import (
	"github.com/autonomousengine/autonomous-engine/internal/plan"
)

// lineSeed mirrors cmd/research-coordinator's line seed (R10 W5): a place
// to edit research found, with up to 5 quoted lines ("840: …" when
// verified against the clone). Defined locally: package main cannot be
// imported.
type lineSeed struct {
	Path      string `json:"path"`
	LineStart int    `json:"line_start"`
	LineEnd   int    `json:"line_end"`
	Snippet   string `json:"snippet,omitempty"`
	Verified  bool   `json:"verified,omitempty"`
}

// maxTaskLineSeeds bounds the seeds one subtask carries.
const maxTaskLineSeeds = 8

// seedsForTask are the brief's line seeds on the subtask's files
// (files_to_change and the task spec's files), in the brief's order.
func seedsForTask(seeds []lineSeed, filesToChange []string, spec *plan.TaskSpec) []lineSeed {
	if len(seeds) == 0 {
		return nil
	}
	in := map[string]bool{}
	for _, f := range plan.ScopeFiles(filesToChange, spec) {
		in[plan.NormalizeScopePath(f)] = true
	}
	var out []lineSeed
	for _, s := range seeds {
		if in[plan.NormalizeScopePath(s.Path)] && len(out) < maxTaskLineSeeds {
			out = append(out, s)
		}
	}
	return out
}
