package verifier

import (
	"fmt"
	"sort"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
	"github.com/autonomousengine/autonomous-engine/internal/llm/tools"
	"github.com/autonomousengine/autonomous-engine/internal/plan"
)

// Tools is the verifier's toolset: read the tree at head, search it, diff
// it against the pinned base, run commands in the scratch repository, and the
// codex tools. Deliberately absent: write/edit (the verdict is not a fix),
// request_user_input, and past_decisions_query / recall_context, which read
// prior conversations — the author's among them.
func Tools() []adapter.Tool {
	base := []adapter.Tool{tools.ReadFile(), tools.SearchCode(), tools.ListFiles(), tools.GitDiff(), tools.RunCommand()}
	return append(base, tools.CodexTools()...)
}

// ToolNames are the names of Tools().
func ToolNames() []string {
	ts := Tools()
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Name)
	}
	return out
}

// CriterionIDs are the ids of in.Criteria, in order.
func (in Input) CriterionIDs() []string {
	out := make([]string, 0, len(in.Criteria))
	for _, c := range in.Criteria {
		out = append(out, c.ID)
	}
	return out
}

// outputContract is the strict JSON shape ParseVerdict accepts. It rides in
// the user prompt so an edited system-prompt template cannot change it.
const outputContract = `## Output

When you have finished investigating, reply with ONE JSON object and nothing else (no prose before or after it; a single ` + "```json" + ` fence is tolerated):

{
  "schema_version": 1,
  "per_criterion": [
    {"id": "AC-1", "status": "met|not_met|not_verifiable",
     "evidence": ["<tool call id from this conversation>", "path/to/file.go:42"],
     "note": "why — required unless met"}
  ],
  "defects": [
    {"severity": "critical|major|minor", "file": "path/to/file.go", "line": 42,
     "category": "off_by_one|missing_error_path|nil_dereference|wrong_status_code|swallowed_error|empty_test|criterion_dropped|injection|secrets|authz|ssrf|path_traversal|deserialization|xss|other",
     "description": "what is wrong", "reproduction": "how to observe it (a command, an input)"}
  ],
  "overall": "approve|request_changes|escalate",
  "summary": "one paragraph"
}

Rules the parser enforces: exactly one per_criterion entry for every criterion listed above and no others; "met" must cite at least one evidence reference, each either a tool call id you received in this conversation, a path:line (path:start-end) reference, or the path of a file that exists at head (file-level, weaker than path:line); a citation that is none of these makes that criterion not_verifiable; every other status needs a note; defects may be empty; unknown fields are rejected.`

// RenderPrompt is the verifier's user message. It is a pure function of
// Input, so what the model sees is exactly what Input admits.
func RenderPrompt(in Input) string {
	var b strings.Builder
	b.WriteString("# Independent verification\n\n")
	b.WriteString("You are verifying a change you did not write. You are given the requirements, the diff against the pinned base, and what the implementation run recorded. You are not given the author's description of the change; do not ask for it.\n\n")
	fmt.Fprintf(&b, "Work item: %s\n", in.WorkID)
	fmt.Fprintf(&b, "Diff base: %s", in.Diff.BaseRef)
	if in.Diff.BaseSHA != "" && in.Diff.BaseSHA != in.Diff.BaseRef {
		fmt.Fprintf(&b, " (pinned %s)", in.Diff.BaseSHA)
	}
	fmt.Fprintf(&b, "\nHead commit under verification: %s\n", in.Diff.HeadSHA)
	b.WriteString("Your working directory is a scratch copy of the repository checked out at the head commit (git commands that write refs or config are refused); git_diff with no arguments shows the change against the base.\n\n")

	b.WriteString("## Acceptance criteria\n\n")
	if len(in.Criteria) == 0 {
		b.WriteString("None were recorded for this work item. per_criterion must then be an empty array; judge the change on defects alone.\n\n")
	}
	for _, c := range in.Criteria {
		renderCriterion(&b, c)
	}

	b.WriteString("## Task specifications\n\n")
	if len(in.TaskSpecs) == 0 {
		b.WriteString("None recorded.\n\n")
	}
	for _, t := range in.TaskSpecs {
		renderTaskSpec(&b, t)
	}

	b.WriteString("## Recorded evidence (what the implementation run observed)\n\n")
	renderEvidence(&b, in.Evidence)

	b.WriteString("## Diff\n\n")
	if len(in.Diff.Files) > 0 {
		fmt.Fprintf(&b, "Changed files (git-derived): %s\n\n", strings.Join(in.Diff.Files, ", "))
	} else {
		b.WriteString("Changed files (git-derived): none\n\n")
	}
	if in.Diff.Patch != "" {
		b.WriteString("```diff\n")
		b.WriteString(in.Diff.Patch)
		if !strings.HasSuffix(in.Diff.Patch, "\n") {
			b.WriteString("\n")
		}
		b.WriteString("```\n")
		if in.Diff.Truncated {
			fmt.Fprintf(&b, "(diff truncated at %d bytes; use git_diff and read_file for the rest)\n", MaxPatchBytes)
		}
		b.WriteString("\n")
	}

	fmt.Fprintf(&b, "## Tools\n\nAvailable: %s.\n\n", strings.Join(in.Tools, ", "))
	b.WriteString(outputContract)
	b.WriteString("\n")
	return b.String()
}

func renderCriterion(b *strings.Builder, c plan.Criterion) {
	fmt.Fprintf(b, "### %s (risk %s)\n%s\n", c.ID, orDash(string(c.Risk)), strings.TrimSpace(c.Text))
	switch {
	case c.Source.Kind == plan.SourceUser && c.Source.Quote != "":
		fmt.Fprintf(b, "- Source: the user's words: %q\n", c.Source.Quote)
	case c.Source.Reason != "":
		fmt.Fprintf(b, "- Source: derived — %s\n", c.Source.Reason)
	}
	if len(c.RequiredChecks) > 0 {
		fmt.Fprintf(b, "- Required checks: %s\n", plan.CheckList(c.RequiredChecks))
	}
	if len(c.Files) > 0 {
		var fs []string
		for _, f := range c.Files {
			fs = append(fs, f.Path+" ("+string(f.Evidence.Kind)+")")
		}
		fmt.Fprintf(b, "- Files named by the brief: %s\n", strings.Join(fs, ", "))
	}
	b.WriteString("\n")
}

func renderTaskSpec(b *strings.Builder, t TaskSpec) {
	fmt.Fprintf(b, "### Task %s\n", t.TaskID)
	if len(t.Spec.CriterionIDs) > 0 {
		fmt.Fprintf(b, "- Criteria: %s\n", strings.Join(t.Spec.CriterionIDs, ", "))
	} else {
		b.WriteString("- Criteria: none cited\n")
	}
	if t.Spec.Deliverable != "" {
		fmt.Fprintf(b, "- Deliverable: %s\n", oneLine(t.Spec.Deliverable))
	}
	for _, v := range t.Spec.Verification {
		what := v.Command
		if what == "" {
			what = v.Description
		}
		if v.AttachedBy == plan.AttachedByPlanner {
			// R9: the planner attached the repository's one whole-suite
			// command to satisfy the kind; it is no evidence for a criterion.
			fmt.Fprintf(b, "- Verification (%s, attached by the planner): %s — the repository's whole suite; it shows the kind is run, not that any criterion is met\n", v.CheckKind, oneLine(what))
			continue
		}
		fmt.Fprintf(b, "- Verification (%s): %s\n", v.CheckKind, oneLine(what))
	}
	if len(t.Spec.Files) > 0 {
		var fs []string
		for _, f := range t.Spec.Files {
			fs = append(fs, f.Path)
		}
		fmt.Fprintf(b, "- Files: %s\n", strings.Join(fs, ", "))
	}
	b.WriteString("\n")
}

func renderEvidence(b *strings.Builder, ev Evidence) {
	if len(ev.Records) == 0 && len(ev.Runs) == 0 {
		b.WriteString("Nothing was recorded. Do not assume any check ran.\n\n")
		return
	}
	for _, r := range ev.Records {
		fmt.Fprintf(b, "### Verification record, task %s: outcome %s\n", r.TaskID, r.Outcome)
		if r.HeadSHA != "" {
			fmt.Fprintf(b, "- head %s, base %s\n", r.HeadSHA, orDash(r.BaseSHA))
		}
		if len(r.Flags) > 0 {
			fmt.Fprintf(b, "- flags: %s\n", strings.Join(r.Flags, ", "))
		}
		for _, c := range r.Checks {
			line := fmt.Sprintf("- check %s: %s", c.Kind, c.Status)
			if c.Command != "" {
				line += fmt.Sprintf(" — `%s`", oneLine(c.Command))
			}
			if c.ExitCode != nil {
				line += fmt.Sprintf(" (exit %d)", *c.ExitCode)
			}
			if c.SkipReason != "" {
				line += " — skipped: " + oneLine(c.SkipReason)
			}
			b.WriteString(line + "\n")
		}
		b.WriteString("\n")
	}
	for _, r := range ev.Runs {
		fmt.Fprintf(b, "### Implementation run, task %s (attempt %d): final state %s\n", r.TaskID, r.Attempt, r.State)
		if len(r.ToolCounts) > 0 {
			keys := make([]string, 0, len(r.ToolCounts))
			for k := range r.ToolCounts {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			parts := make([]string, 0, len(keys))
			for _, k := range keys {
				parts = append(parts, fmt.Sprintf("%s×%d", k, r.ToolCounts[k]))
			}
			fmt.Fprintf(b, "- tool calls: %s\n", strings.Join(parts, ", "))
		}
		if len(r.WrittenPaths) > 0 {
			fmt.Fprintf(b, "- files written: %s\n", strings.Join(r.WrittenPaths, ", "))
		}
		for _, c := range r.Commands {
			exit := "no exit status"
			if c.ExitCode != nil {
				exit = fmt.Sprintf("exit %d", *c.ExitCode)
			}
			fmt.Fprintf(b, "- #%d run_command `%s` → %s\n", c.Seq, oneLine(c.Command), exit)
		}
		for _, t := range r.Transitions {
			fmt.Fprintf(b, "- #%d %s → %s", t.Seq, t.From, t.To)
			if t.Why != "" {
				fmt.Fprintf(b, " (%s)", oneLine(t.Why))
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
