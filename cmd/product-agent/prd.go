package main

import (
	"regexp"
	"strings"
)

// prdHeading is a markdown H1 or H2 line: where a PRD body starts.
var prdHeading = regexp.MustCompile(`^#{1,2}[ \t]+\S`)

func isFence(line string) bool { return strings.HasPrefix(strings.TrimSpace(line), "```") }

// isMarkdownWrapper reports a fence opener that may wrap a whole PRD:
// ``` or ```markdown / ```md.
func isMarkdownWrapper(line string) bool {
	switch strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "```"))) {
	case "", "markdown", "md":
		return true
	}
	return false
}

// stripPRDPreamble keeps only the PRD body of a generate_prd response: from
// the first markdown H1/H2 line onward. Models often open with chatter
// ("After reviewing the PRD … I will output the PRD unchanged"), and the
// stored PRD is re-sent to every research task.
//
// Lines inside a code fence are never headings (a "# install deps" shell
// comment is not a title). When the body is wrapped in a ``` / ```markdown
// fence whose opener is followed only by blank lines before the first
// heading, the wrapper is removed: the opener goes with the preamble and
// the final bare ``` that closes it (with anything after it) is dropped;
// code blocks inside the body are kept. A response with no H1/H2 outside a
// fence is kept as is.
func stripPRDPreamble(s string) string {
	lines := strings.Split(s, "\n")
	start, wrapped := -1, false
	inFence := false
	for i := 0; i < len(lines) && start < 0; i++ {
		ln := strings.TrimRight(lines[i], "\r")
		if isFence(ln) {
			if !inFence && isMarkdownWrapper(ln) {
				// A wrapper only if the next non-blank line is a heading.
				j := i + 1
				for j < len(lines) && strings.TrimSpace(lines[j]) == "" {
					j++
				}
				if j < len(lines) && prdHeading.MatchString(strings.TrimRight(lines[j], "\r")) {
					start, wrapped = j, true
					break
				}
			}
			inFence = !inFence
			continue
		}
		if !inFence && prdHeading.MatchString(ln) {
			start = i
		}
	}
	if start < 0 || (start == 0 && !wrapped) {
		return s
	}
	body := lines[start:]
	if !wrapped {
		return strings.Join(body, "\n")
	}
	// The wrapper's closing fence is the last bare ``` that follows a
	// balanced number of fence lines in the body.
	closing, fences := -1, 0
	for j, ln := range body {
		if !isFence(ln) {
			continue
		}
		if strings.TrimSpace(ln) == "```" && fences%2 == 0 {
			closing = j
		}
		fences++
	}
	if closing >= 0 {
		body = body[:closing]
	}
	return strings.TrimRight(strings.Join(body, "\n"), " \t\r\n")
}
