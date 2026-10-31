package main

import (
	"fmt"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/llm/strategy"
	"github.com/autonomousengine/autonomous-engine/internal/models"
)

// implementationSystemPrompt is the role and the editing contract of every
// implementation tool loop (T2). It names only tools of the edit phase
// (tools.ImplementationToolsFor) and nothing framework-specific; the task
// itself is the first user message. A live run (attempt 6) sent a 9,786-char
// system prompt on every call: tools listed twice, a Next.js section for an
// Express repo, an empty codex header, a "multi-repo" line for one repo,
// "FIRST message MUST be a tool_call" against the tier preamble's "reply
// with a plan first", and "one tool_call at a time".
const implementationSystemPrompt = `You are the implementation agent: you make one task's changes in one repository, using tools. Code in your reply text changes nothing; only edit_file, insert_after, append_to_file and write_file change files.

How to work:
- Read before you write: read a file (read_file or codex_read_file) before edit_file or write_file on it. When a path the task names is missing, find the real file with codex_grep, codex_find_module or list_files; do not invent paths.
- Read only the file you are about to edit, around the Where-to-edit lines; do not open every in-scope file at once. Put independent searches in one turn as several tool calls. Make edits one at a time and check each result.
- When the task changes behaviour, add or extend a test first, run it and see it fail, then implement until it passes. Never weaken a test to make it pass.
- edit_file: copy old_string verbatim from your latest read of that file, with enough surrounding lines to match once. To add lines, use insert_after instead of re-typing a block around an anchor. Use write_file only for a new file or a whole-file rewrite.
- After you write or edit a source file, call codex_verify_imports on it and fix any unresolved import.
- Change only what the task needs. Git (branch, commit, push) is done for you when you finish.
- Earlier tool results may be replaced by an evidence ledger or a checkpoint summary: rely on them instead of repeating calls. The task message above them never changes.
- You are done when every step of the task is done and the tests pass. Then reply in one or two sentences naming the files you changed, without code.`

// noTestRunnerNote replaces the TDD rule when the repository has no test
// runner and the work item is not about adding one.
const noTestRunnerNote = "## No test runner\n" +
	"This repository has no configured test runner and the task is not about tests. Implement the change directly: " +
	"do not add test files, test tooling or test dependencies, and do not run a test command. " +
	"codex_verify_imports on each changed file is the check."

// testSetupNote keeps TDD when the work item asks for tests in a repository
// without a runner.
const testSetupNote = "## Test setup is in scope\n" +
	"This repository has no test runner, but the work item asks for tests: set up the runner (config, first test " +
	"files, devDependencies) as part of the task."

// maxImplementationGroundingChars bounds the codex section; it is re-sent on
// every call.
const maxImplementationGroundingChars = 1600

// assembleImplementationSystemPrompt is the system prompt of the main
// implementation loop: the role and contract, the tier preamble, the codex
// grounding (bounded; empty adds nothing), the test command or the
// no-runner note, and review feedback. The handler and the floor test both
// use it.
func assembleImplementationSystemPrompt(mode models.OperatingMode, codexGrounding, testCommand, noRunnerNote, reviewFeedback string) string {
	var b strings.Builder
	b.WriteString(implementationSystemPrompt)
	if pre := strings.TrimSpace(strategy.PromptScaffolding(mode).PromptPreamble()); pre != "" {
		b.WriteString("\n\n")
		b.WriteString(pre)
	}
	if g := strings.TrimSpace(codexGrounding); g != "" {
		if len(g) > maxImplementationGroundingChars {
			g = strings.TrimSpace(g[:maxImplementationGroundingChars]) + "\n- … (more: codex_find_module)"
		}
		b.WriteString("\n\n## Project codex\n")
		b.WriteString(g)
	}
	if testCommand != "" {
		fmt.Fprintf(&b, "\n\nTest command: %s", testCommand)
	} else if noRunnerNote != "" {
		b.WriteString("\n\n")
		b.WriteString(noRunnerNote)
	}
	if fb := strings.TrimSpace(reviewFeedback); fb != "" {
		b.WriteString("\n\n## Review feedback to address\n")
		b.WriteString(fb)
	}
	return b.String()
}
