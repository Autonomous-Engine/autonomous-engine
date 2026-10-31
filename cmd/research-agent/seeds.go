package main

import (
	"fmt"
	"strings"
)

// seedPayload is the part of a research task that research-coordinator adds
// for R2: the files the intake or the PRD names (with their lines), and this
// task's share of the work item's remaining token budget. It is embedded in
// researchTaskMessage, so the JSON fields decode at the top level; older
// coordinators send none of them.
type seedPayload struct {
	SeedFiles        []string         `json:"seed_files,omitempty"`
	SeedLines        map[string][]int `json:"seed_lines,omitempty"`
	SeedsUnverified  bool             `json:"seeds_unverified,omitempty"`
	BudgetHintTokens *int64           `json:"budget_hint_tokens,omitempty"`
}

// tokensPerCallEstimate turns the budget hint into a call count: the
// measured per-call input floor in a live run was 9.4k–17k tokens.
const tokensPerCallEstimate = 10_000

// seedsSection is appended to the research user message: the named files to
// open first, and the task's budget. Empty for a task without either.
func seedsSection(task researchTaskMessage) string {
	var b strings.Builder
	if len(task.SeedFiles) > 0 {
		b.WriteString("\n\nStart here (named in the intake):\n")
		for _, f := range task.SeedFiles {
			b.WriteString("- ")
			b.WriteString(f)
			if lines := task.SeedLines[f]; len(lines) > 0 {
				word := "lines"
				if len(lines) == 1 {
					word = "line"
				}
				nums := make([]string, len(lines))
				for i, n := range lines {
					nums[i] = fmt.Sprint(n)
				}
				fmt.Fprintf(&b, " (%s %s)", word, strings.Join(nums, ", "))
			}
			b.WriteByte('\n')
		}
		b.WriteString("Read these first with codex_read_file; do not search for them.")
		if task.SeedsUnverified {
			b.WriteString(" These paths were not verified against the repository: if one does not exist, say so rather than searching the whole repository for it.")
		}
	}
	if task.BudgetHintTokens != nil {
		if b.Len() == 0 {
			b.WriteString("\n")
		}
		n := *task.BudgetHintTokens
		if n < 0 {
			n = 0
		}
		calls := n / tokensPerCallEstimate
		word := "calls"
		if calls == 1 {
			word = "call"
		}
		if calls == 0 && n > 0 {
			calls, word = 1, "call"
		}
		fmt.Fprintf(&b, "\nBudget for this task: about %d tokens (about %d %s).", n, calls, word)
	}
	return b.String()
}
