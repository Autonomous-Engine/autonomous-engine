package main

import (
	"log"
	"regexp"
	"strings"
)

// Research fan-out scaled to the work (R2). The fixed per-work-type switch
// sent five parallel tasks for every bug, each paying the per-call floor,
// even when the intake named the exact files. Now:
//
//   - bug with seeds: code_trace, plus impact_assessment when the seeds span
//     more than one top-level directory or the PRD lists more than three
//     acceptance criteria;
//   - bug without seeds: code_trace, bug_archaeology, impact_assessment;
//   - feature: impact_assessment, pattern_match, code_trace;
//   - any other work type: the generic five, as before.
//
// RESEARCH_TASK_TYPES_<WORKTYPE> (a comma list) overrides the choice for one
// work type, e.g. RESEARCH_TASK_TYPES_BUG=code_trace,history_analysis.

// knownResearchTypes are the research types research-agent implements.
var knownResearchTypes = map[string]bool{
	"code_trace":        true,
	"history_analysis":  true,
	"bug_archaeology":   true,
	"impact_assessment": true,
	"pattern_match":     true,
}

// manyCriteria: a PRD with more acceptance criteria than this gets an
// impact assessment even when the bug is seeded.
const manyCriteria = 3

func selectResearchTypes(workType string, seeds []string, seedsVerified bool, acceptanceCriteria int, getenv func(string) string) []string {
	if override := researchTypesOverride(workType, getenv); len(override) > 0 {
		return override
	}
	switch workType {
	case "bug":
		// Only seeds confirmed against a clone or the Codex narrow a bug:
		// unverified paths may be wrong, so the bug keeps the no-seed set.
		if len(seeds) == 0 || !seedsVerified {
			return []string{"code_trace", "bug_archaeology", "impact_assessment"}
		}
		if seedsSpanTopLevelDirs(seeds) || acceptanceCriteria > manyCriteria {
			return []string{"code_trace", "impact_assessment"}
		}
		return []string{"code_trace"}
	case "feature":
		return []string{"impact_assessment", "pattern_match", "code_trace"}
	default:
		// Task, subtask, research, qa — keep the generic 5.
		return []string{"impact_assessment", "pattern_match", "code_trace", "history_analysis", "bug_archaeology"}
	}
}

// researchTypesEnvKey is RESEARCH_TASK_TYPES_ plus the work type in upper
// case with anything other than letters and digits as "_".
func researchTypesEnvKey(workType string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(workType) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return "RESEARCH_TASK_TYPES_" + b.String()
}

// researchTypesOverride reads the operator override for workType. Unknown
// and repeated types are dropped; an override with nothing valid is ignored.
func researchTypesOverride(workType string, getenv func(string) string) []string {
	if getenv == nil || workType == "" {
		return nil
	}
	key := researchTypesEnvKey(workType)
	raw := getenv(key)
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		t := strings.TrimSpace(part)
		if t == "" || seen[t] {
			continue
		}
		if !knownResearchTypes[t] {
			log.Printf("research-coordinator: %s: ignoring unknown research type %q", key, t)
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	if len(out) == 0 {
		log.Printf("research-coordinator: %s=%q names no known research type — using the default fan-out", key, raw)
	}
	return out
}

// seedsSpanTopLevelDirs reports whether the seeds reach into more than one
// top-level directory of the repository (a root file counts as its own).
// The one exception (coordinator ruling): when every seed has the identical
// path below its top-level directory — e.g. public/index.html and
// landing/index.html are both "index.html" — they are one edit mirrored
// in sibling trees, not a cross-cutting change. cmd/a/main.go and
// internal/b/main.go share only the file name and do span.
func seedsSpanTopLevelDirs(seeds []string) bool {
	tops := map[string]bool{}
	rests := map[string]bool{}
	for _, s := range seeds {
		top, rest := ".", s
		if i := strings.IndexByte(s, '/'); i > 0 {
			top, rest = s[:i], s[i+1:]
		}
		tops[top] = true
		rests[rest] = true
	}
	return len(tops) > 1 && len(rests) > 1
}

var (
	acHeading    = regexp.MustCompile(`(?i)^\s*(?:#{1,6}\s*)?(?:\*\*|__)?\s*acceptance\s+criteria\b`)
	mdHeading    = regexp.MustCompile(`^\s*#{1,6}\s`)
	boldHeading  = regexp.MustCompile(`^\s*(?:\*\*|__)[^*_]+(?:\*\*|__)\s*:?\s*$|^\s*(?:\*\*|__)[^*_]+:(?:\*\*|__)\s*$`)
	topListEntry = regexp.MustCompile(`^ ?(?:[-*+]|\d+[.)])\s+\S`)
	// acIDLine is a top-level "AC-n" line: at most one space of indent, like
	// topListEntry, so an indented continuation is not a criterion.
	acIDLine = regexp.MustCompile(`^ ?(?:\*\*|__)?AC-\d+\b`)
)

// headingLevel is the number of leading '#' of a markdown heading line, 0
// for any other line.
func headingLevel(line string) int {
	if !mdHeading.MatchString(line) {
		return 0
	}
	t := strings.TrimLeft(line, " \t")
	return len(t) - len(strings.TrimLeft(t, "#"))
}

// countAcceptanceCriteria counts the criteria under the PRD's "Acceptance
// Criteria" heading(s): top-level list items (bullets or numbered, with or
// without an AC id) and bare "AC-n" lines. Under a markdown heading of level
// L the section runs to the next heading of level L or higher, so lettered
// sub-headings ("### A. ...", live run attempt 7) group criteria instead of
// ending the section; under a bold or plain label it runs to the next
// heading of any kind.
func countAcceptanceCriteria(prd string) int {
	n := 0
	in := false
	level := 0 // the AC heading's markdown level; 0 for a bold or plain label
	for _, line := range strings.Split(strings.ReplaceAll(prd, "\r\n", "\n"), "\n") {
		if acHeading.MatchString(line) {
			in, level = true, headingLevel(line)
			continue
		}
		if k := headingLevel(line); k > 0 {
			if in && level > 0 && k > level {
				continue // a sub-heading inside the section
			}
			in = false
			continue
		}
		if boldHeading.MatchString(line) {
			in = false
			continue
		}
		if in && (topListEntry.MatchString(line) || acIDLine.MatchString(line)) {
			n++
		}
	}
	return n
}
