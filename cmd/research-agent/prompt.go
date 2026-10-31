package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
	"github.com/autonomousengine/autonomous-engine/internal/llm/strategy"
	"github.com/autonomousengine/autonomous-engine/internal/llm/tools"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/google/uuid"
)

// Research call cost (R1). Every research call re-sends the system prompt,
// the user message and the tool schemas, so all three are kept to what the
// task needs: the PRD travels once, in the user message; the system prompt
// is the role, a short strategy and a few lines for the research type; the
// tools are the scoped researchToolSet. TestFirstCallFloor holds the first
// call of a code_trace task on a 4.6k-char PRD to 3,200 estimated tokens.

// researchStrategy is shared by every research type. At most 25 lines.
const researchStrategy = `You are a read-only research agent. Investigate the codebase with the tools and report what the code actually does. Never guess: every claim cites file:line from a tool result.

## Strategy
- Graph first: codex_find_module with a short identifier ("auth", "map"), then codex_dependents_of / codex_dependencies_of on the modules it returns.
- Files by name: codex_list_tree. codex_grep searches file contents, not file names.
- Unknown names: codex_similar_code with a plain description. Conventions: codex_patterns_for.
- Read last: codex_read_file only on files the graph or a search pointed at, in line ranges.
- Files or symbols the request names are starting points: go to them directly.
- Never repeat a call already in the evidence ledger; use its recorded result.
- Stop as soon as the request's acceptance criteria can be answered with file:line evidence, then write the answer with no further tool calls. If something cannot be found, say what is missing and stop.`

// researchTypeGuidance is the per-type task statement. At most 8 lines each.
func researchTypeGuidance(researchType string) string {
	switch researchType {
	case "code_trace":
		return `## Task: code trace
Trace the code paths the request touches: entry points, the functions they call, and side effects (DB writes, external calls).
For a new feature with no entry point, list the files that would change and why.
Report each item with file:line.`
	case "history_analysis":
		return `## Task: history analysis
Find the conventions and tech debt around the area the request touches.
Use git_log on the files you identify to see when and why they changed, codex_grep for TODO|FIXME|HACK|DEPRECATED near them, and codex_patterns_for for the team's conventions.
Report: conventions in force, debt markers, drift from convention, each with file:line.`
	case "pattern_match":
		return `## Task: pattern match
Find prior art for the request: the 2–3 closest existing implementations (codex_patterns_for, codex_similar_code).
Report each precedent with file:line and the standard approach they share.`
	case "impact_assessment":
		return `## Task: impact assessment
Find everything a change for the request would affect: importers (codex_dependents_of) and looser coupling (codex_similar_code).
Report each affected component, the nature of the dependency and its risk, with file:line.`
	case "bug_archaeology":
		return `## Task: bug archaeology
Look for past bugs near the area the request touches: git_log for fix commits on the files involved, git_diff to inspect one, codex_grep for bug|fix|regression|workaround comments, and test names that encode past fixes.
Report past bug signals, how they were handled, and the remaining risk, with file:line.`
	default:
		return `## Task: investigation
Investigate the request and report the findings with file:line evidence.`
	}
}

// buildResearchSystemPrompt is the role, the strategy and the type guidance.
// It never carries the PRD: that is in the user message.
func buildResearchSystemPrompt(researchType string) string {
	return researchStrategy + "\n\n" + researchTypeGuidance(researchType)
}

// assembleResearchSystemPrompt is the complete system prompt the handler
// sends: role, strategy and type guidance, the codex grounding section (may
// be empty), then the operating mode's scaffold preamble. The floor test
// measures this same function.
func assembleResearchSystemPrompt(researchType, codexGrounding string, mode models.OperatingMode) string {
	system := buildResearchSystemPrompt(researchType) + codexGrounding
	if pre := strategy.PromptScaffolding(mode).PromptPreamble(); pre != "" {
		system += "\n\n" + pre
	}
	return system
}

// buildResearchUserMessage carries the task identity and, once, the PRD.
func buildResearchUserMessage(task researchTaskMessage, primaryPath, snapshotLine string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Research task: %s\nWork ID: %s\nTask ID: %s\nRepository path: %s%s\n\n",
		task.ResearchType, task.WorkID, task.TaskID, primaryPath, snapshotLine)
	b.WriteString("## Request (PRD): answer its acceptance criteria\n")
	b.WriteString(strings.TrimSpace(task.StartingContext))
	if cs := strings.TrimSpace(task.CodexSections); cs != "" {
		b.WriteString("\n\n## Codex sections (project knowledge)\n")
		b.WriteString(cs)
	}
	return b.String()
}

// researchToolSet is the tool list one task is offered.
func researchToolSet(researchType string, lspReady bool) []adapter.Tool {
	return tools.ResearchToolsFor(researchType, tools.ResearchToolOptions{LSPReady: lspReady})
}

// researchLoopOptions turns on the evidence ledger and the dead-tool guard,
// and re-reads LSP readiness every iteration so the LSP tools appear once
// the language server has warmed up.
//
// R17: research also gets the stuck control (identical failing calls,
// INSUFFICIENT, whole-turn cycles) and re-read dedup, with ReadOnly: the
// task reads a pinned snapshot, so the third repeat of the same read ends
// the loop. A research LoopStuck fails the task with status loop_stuck
// (structured, in the result); the coordinator's honest-failure gate
// records it — research-agent does not block the item itself.
//
// R18: research never succeeds without running a tool. An answer before
// any tool call gets the evidence nudge once; a second one ends the task
// with status no_evidence (researchTaskLoopOptions names the task's seeds).
func researchLoopOptions(researchType string, lspReady func() bool) tools.LoopOptions {
	return tools.LoopOptions{
		EvidenceLedger: true,
		DeadToolGuard:  true,
		StuckControl:   true,
		DedupReads:     true,
		ReadOnly:       true,
		EvidenceNudge:  evidenceNudge(nil),
		RefreshTools: func() []adapter.Tool {
			return researchToolSet(researchType, lspReady())
		},
	}
}

// ─── Codex grounding (B1) ───────────────────────────────────────────────

// codexGroundingSource is the slice of *codex.Codex research grounding reads.
type codexGroundingSource interface {
	GetModulesForProductBranch(ctx context.Context, productID uuid.UUID, branch string) ([]*models.CodexModule, error)
	GetPatternsForProductBranch(ctx context.Context, productID uuid.UUID, branch string) ([]*models.CodexPattern, error)
	GetAntiPatternsForProductBranch(ctx context.Context, productID uuid.UUID, branch string) ([]*models.CodexAntiPattern, error)
}

// maxCodexGroundingChars bounds the codex section of the system prompt; it
// is re-sent on every call. The rest is one codex_find_module away.
const maxCodexGroundingChars = 2400

// resolveCodexBranch picks the codex branch for research: the work item's
// branch, else the product's default branch, else "main". The codex layer
// rejects an empty branch, so "" is never returned. When the default-branch
// lookup fails, the error is returned alongside "main" for the caller to
// log; it is never dropped.
func resolveCodexBranch(ctx context.Context, itemBranch string, productDefault func(context.Context) (string, error)) (string, error) {
	if b := strings.TrimSpace(itemBranch); b != "" {
		return b, nil
	}
	if productDefault == nil {
		return "main", nil
	}
	b, err := productDefault(ctx)
	if err != nil {
		return "main", fmt.Errorf("default branch lookup failed, using main: %w", err)
	}
	if b = strings.TrimSpace(b); b == "" {
		return "main", errors.New("default branch is empty, using main")
	}
	return b, nil
}

// buildCodexGrounding renders the product's modules, patterns and
// anti-patterns on branch, bounded to maxCodexGroundingChars. It returns
// what did load together with an error naming every lookup that failed;
// the caller logs it once and carries on.
func buildCodexGrounding(ctx context.Context, cx codexGroundingSource, productID uuid.UUID, branch string) (string, error) {
	var errs []error
	var lines []string
	budget := maxCodexGroundingChars
	add := func(s string) bool {
		if len(s)+1 > budget {
			return false
		}
		lines = append(lines, s)
		budget -= len(s) + 1
		return true
	}

	modules, err := cx.GetModulesForProductBranch(ctx, productID, branch)
	if err != nil {
		errs = append(errs, fmt.Errorf("modules on %q: %w", branch, err))
	}
	if len(modules) > 0 {
		add("### Modules")
		for i, m := range modules {
			if !add(fmt.Sprintf("- %s: %s (risk: %s)", m.Name, oneLine(m.Purpose, 140), m.RiskLevel)) {
				lines = append(lines, fmt.Sprintf("- … %d more modules (codex_find_module)", len(modules)-i))
				budget = 0
				break
			}
		}
	}

	patterns, err := cx.GetPatternsForProductBranch(ctx, productID, branch)
	if err != nil {
		errs = append(errs, fmt.Errorf("patterns on %q: %w", branch, err))
	}
	if len(patterns) > 0 && add("### Patterns to follow") {
		for _, p := range patterns {
			if !add(fmt.Sprintf("- %s (%s): %s", p.Name, p.Category, oneLine(p.Rationale, 160))) {
				break
			}
		}
	}

	anti, err := cx.GetAntiPatternsForProductBranch(ctx, productID, branch)
	if err != nil {
		errs = append(errs, fmt.Errorf("anti-patterns on %q: %w", branch, err))
	}
	if len(anti) > 0 && add("### Anti-patterns to avoid") {
		for _, ap := range anti {
			if !add(fmt.Sprintf("- %s: %s; instead: %s", ap.Name, oneLine(ap.WhyWrong, 100), oneLine(ap.WhatInstead, 100))) {
				break
			}
		}
	}

	out := ""
	if len(lines) > 0 {
		out = "\n\n## Project codex (branch " + branch + ")\n" + strings.Join(lines, "\n")
	}
	return out, errors.Join(errs...)
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// researchTaskLoopOptions are researchLoopOptions for one task: the
// evidence nudge names the task's seed files.
func researchTaskLoopOptions(task researchTaskMessage, lspReady func() bool) tools.LoopOptions {
	o := researchLoopOptions(task.ResearchType, lspReady)
	o.EvidenceNudge = evidenceNudge(task.SeedFiles)
	return o
}

// maxNudgeSeeds bounds the files the evidence nudge names.
const maxNudgeSeeds = 5

// evidenceNudge (R18 E1) is the one message a research loop gets when the
// model answers before running any tool call. Attempt 15's answers wrote
// their calls as JSON text in a fenced block, several per block, which the
// provider adapter did not run: the nudge asks for real tool calls, one at
// a time if need be.
func evidenceNudge(seeds []string) string {
	var b strings.Builder
	b.WriteString("You answered without calling any tool, so nothing was read: you must gather evidence with tools before answering. ")
	if len(seeds) > 0 {
		names := seeds
		if len(names) > maxNudgeSeeds {
			names = names[:maxNudgeSeeds]
		}
		fmt.Fprintf(&b, "Call codex_read_file on the seeds now (%s).", strings.Join(names, ", "))
	} else {
		b.WriteString("Call codex_find_module or codex_read_file now.")
	}
	b.WriteString(" Make each one a tool call, not JSON written in your reply: text is not executed. One call per reply is fine. " +
		"Answering again without a tool call ends this task with no evidence.")
	return b.String()
}
