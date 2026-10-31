package plan

import "strings"

// groundingNotePrefix starts the note the planner's grounding writes on a
// path it could not find (or that escapes the repository). Such a path is
// a hypothesis kept in the TaskSpec for the record, never a file to change.
const groundingNotePrefix = "grounding:"

// GroundingMiss reports whether the planner's grounding found no such file
// in the repository: a hypothesis with a grounding note. A hypothesis that
// could not be checked at all (no clone path) is not a miss.
func (f FileRef) GroundingMiss() bool {
	return f.Evidence.Kind == EvidenceHypothesis && strings.HasPrefix(f.Evidence.Note, groundingNotePrefix)
}

// StepFiles are the repository files a subtask's steps cite: every TaskSpec
// file except grounding misses (existing, new and unverified paths), in
// spec order, de-duplicated.
func StepFiles(spec *TaskSpec) []string {
	if spec == nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, f := range spec.Files {
		if f.GroundingMiss() {
			continue
		}
		out = appendScopePath(out, seen, f.Path)
	}
	return out
}

// ScopeFiles is a subtask's file scope: the de-duplicated union of its own
// files_to_change and StepFiles(spec), own list first. The planner sets
// files_to_change to it and the implementer's continuation check uses it,
// so a step whose files nobody listed is still work to do.
func ScopeFiles(own []string, spec *TaskSpec) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, p := range own {
		out = appendScopePath(out, seen, p)
	}
	for _, p := range StepFiles(spec) {
		out = appendScopePath(out, seen, p)
	}
	return out
}

// NormalizeScopePath is the form scope paths are compared in: trimmed,
// forward slashes, no leading "./".
func NormalizeScopePath(p string) string {
	p = strings.TrimSpace(strings.ReplaceAll(p, "\\", "/"))
	for strings.HasPrefix(p, "./") {
		p = strings.TrimPrefix(p, "./")
	}
	return p
}

func appendScopePath(out []string, seen map[string]bool, p string) []string {
	p = NormalizeScopePath(p)
	if p == "" || seen[p] {
		return out
	}
	seen[p] = true
	return append(out, p)
}
