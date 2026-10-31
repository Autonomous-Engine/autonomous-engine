package plan

import (
	"regexp"
	"strings"
)

// Intake-stated PR requirements (R15 T2, live-run attempt 13b). The intake
// said "the pull request description MUST state explicitly that the
// repository owner must obtain a TileHost API key and set TILES_API_KEY …",
// but that run's brief made no criterion of it, so the PR description
// (pr-coordinator's "Requirements stated for this change", which lists
// PR-text criteria) did not say so. Research now extracts such sentences
// from the intake text (work item description + PRD) into the brief's
// pr_requirements; the planner copies them onto every subtask's plan task
// message, and pr-coordinator lists them next to the PR-text criteria.

// MaxPRRequirements bounds the intake sentences carried (the PR section's
// own bound is the same).
const MaxPRRequirements = 20

// sentenceEndRE ends a sentence: ., ! or ? followed by whitespace.
var sentenceEndRE = regexp.MustCompile(`[.!?]\s+`)

// listMarkerRE is a leading list marker ("- ", "* ", "6. ", "A) ").
var listMarkerRE = regexp.MustCompile(`^(?:[-*•]\s+|\d{1,3}[.)]\s+|[A-Za-z][.)]\s+)`)

// PRTextRequirements returns, in order, the sentences of texts (each split
// into lines, then sentences) that ask for something to be stated in the PR
// text (TargetsPRText), verbatim apart from a leading list marker and
// surrounding whitespace. A sentence that restates one already kept
// (RestatesRequirement: the PRD paraphrasing the intake) is left out. At
// most MaxPRRequirements; nil when there are none.
func PRTextRequirements(texts ...string) []string {
	var out []string
	for _, text := range texts {
		for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
			for _, s := range splitSentences(line) {
				if !TargetsPRText(s) || restatesAny(s, out) {
					continue
				}
				out = append(out, s)
				if len(out) == MaxPRRequirements {
					return out
				}
			}
		}
	}
	return out
}

// splitSentences splits one line into trimmed sentences, each keeping its
// closing punctuation, with a leading list marker removed.
func splitSentences(line string) []string {
	var out []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		s = strings.TrimSpace(listMarkerRE.ReplaceAllString(s, ""))
		if s != "" {
			out = append(out, s)
		}
	}
	start := 0
	for _, loc := range sentenceEndRE.FindAllStringIndex(line, -1) {
		add(line[start : loc[0]+1])
		start = loc[1]
	}
	add(line[start:])
	return out
}

func restatesAny(s string, kept []string) bool {
	for _, k := range kept {
		if RestatesRequirement(s, k) {
			return true
		}
	}
	return false
}

// requirementWordRE splits a requirement into words.
var requirementWordRE = regexp.MustCompile(`[a-z0-9_]+`)

// RestatesRequirement reports whether a and b state the same requirement:
// at least 80% of the distinct words (3+ characters) of the one with fewer
// words are words of the other ("Explicitly state in the pull request
// description that …" and "The pull request description explicitly states
// that …").
func RestatesRequirement(a, b string) bool {
	wa, wb := requirementWords(a), requirementWords(b)
	if len(wa) == 0 || len(wb) == 0 {
		return false
	}
	if len(wa) > len(wb) {
		wa, wb = wb, wa
	}
	n := 0
	for w := range wa {
		if wb[w] {
			n++
		}
	}
	return 5*n >= 4*len(wa)
}

func requirementWords(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range requirementWordRE.FindAllString(strings.ToLower(s), -1) {
		if len(w) >= 3 {
			out[w] = true
		}
	}
	return out
}
