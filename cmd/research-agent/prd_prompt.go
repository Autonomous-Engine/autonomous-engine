package main

import (
	"strings"
)

// The PRD travelled whole in every research call (4.7k chars on the live
// item). A task carries what it must answer — the acceptance criteria —
// and the other headings; read_document name=prd fetches any of them.

// prdFallbackChars is what a PRD without an acceptance-criteria heading
// contributes to the task message.
const prdFallbackChars = 2500

// prdHeadingTitle is the heading a research task answers.
const prdHeadingTitle = "acceptance criteria"

// prdForTask returns the part of the PRD the task message carries (the H1
// line and the acceptance-criteria section, else the first
// prdFallbackChars) and the titles of the other top-level sections.
func prdForTask(prd string) (kept string, otherHeadings []string) {
	lines := strings.Split(strings.TrimSpace(prd), "\n")
	title := ""
	type section struct {
		name       string
		start, end int
	}
	var sections []section
	for i, l := range lines {
		t := strings.TrimRight(l, "\r")
		switch {
		case strings.HasPrefix(t, "# ") && title == "":
			title = t
		case strings.HasPrefix(t, "## "):
			if n := len(sections); n > 0 {
				sections[n-1].end = i
			}
			sections = append(sections, section{name: strings.TrimSpace(t[3:]), start: i, end: len(lines)})
		}
	}
	criteria := -1
	for i, s := range sections {
		if strings.Contains(strings.ToLower(s.name), prdHeadingTitle) {
			criteria = i
			break
		}
	}
	for i, s := range sections {
		if i != criteria {
			otherHeadings = append(otherHeadings, s.name)
		}
	}
	if criteria < 0 {
		kept = strings.TrimSpace(prd)
		if len(kept) > prdFallbackChars {
			kept = kept[:prdFallbackChars] + "\n[… the rest of the PRD is in read_document name=prd]"
		}
		return kept, otherHeadings
	}
	s := sections[criteria]
	body := strings.TrimSpace(strings.Join(lines[s.start:s.end], "\n"))
	if title != "" {
		return title + "\n\n" + body, otherHeadings
	}
	return body, otherHeadings
}
