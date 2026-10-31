package main

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
	"github.com/autonomousengine/autonomous-engine/internal/llm/tools"

	"github.com/autonomousengine/autonomous-engine/internal/plan"
)

// taskScope is the subtask's file scope and the part of it its steps cite:
// plan.ScopeFiles (files_to_change ∪ the TaskSpec's repository files) and
// plan.StepFiles. A task without a TaskSpec (legacy) has no step files.
func taskScope(task implementationTaskMessage) (scope, stepFiles []string) {
	return plan.ScopeFiles(task.FilesToChange, task.TaskSpec), plan.StepFiles(task.TaskSpec)
}

// continuationPrompt is the user message of a continuation pass: what
// changed, which planned files remain (in continuationOrder), and (M3) that
// a planned file which needs no change is reported, not edited. stuck are
// the earlier passes' LoopStuck failures (R10 W4): each is named with its
// reason, which carries the edit result's hints (the repeated lines, the
// nearest lines), so the pass does not re-issue the same edit.
func continuationPrompt(changed map[string]bool, missing []string, stuck []tools.LoopStuck) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You have completed part of this task. The following files were changed: %v\n\n"+
		"The following planned files still need changes, in this order:\n", mapKeys(changed))
	for _, f := range continuationOrder(missing) {
		fmt.Fprintf(&b, "  - %s\n", f)
	}
	if len(stuck) > 0 {
		b.WriteString("\nThe previous pass stopped because these calls kept failing the same way:\n")
		for _, s := range stuck {
			where := s.Tool
			if s.Path != "" {
				where += " on " + s.Path
			}
			fmt.Fprintf(&b, "- %s (%d attempts). Last result:\n%s\n", where, s.Attempts, indent(truncate(s.Reason, 700), "    "))
		}
		b.WriteString("Do not repeat those calls. Take a new approach based on the result above (fix what it names, or pick a different old_string after reading the file again). Repeating a failing call ends this task.\n")
	}
	b.WriteString("\nContinue the implementation. Focus ONLY on the remaining files listed above. " +
		"Read each file, then call edit_file to make the required changes. Do not re-explain what you already did. " +
		"If a listed file needs no change, say so and do not edit it.")
	return b.String()
}

func indent(s, pre string) string {
	return pre + strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n"+pre)
}

// continuationOrder orders the remaining files: code, then tests, then
// other files (configuration), then docs (R10 W4: attempt 9's continuation
// spent its budget on docs with the test step last).
func continuationOrder(files []string) []string {
	out := append([]string(nil), files...)
	sort.SliceStable(out, func(i, j int) bool { return fileRank(out[i]) < fileRank(out[j]) })
	return out
}

var testPathRE = regexp.MustCompile(`(^|/)(tests?|__tests__|spec)/|[._](test|spec)\.[a-z]+$|_test\.go$`)

func fileRank(p string) int {
	n := strings.ToLower(plan.NormalizeScopePath(p))
	base := path.Base(n)
	switch {
	case testPathRE.MatchString(n):
		return 1
	case strings.HasSuffix(n, ".md"), strings.HasSuffix(n, ".rst"), strings.HasSuffix(n, ".txt"), strings.HasPrefix(n, "docs/"):
		return 3
	case strings.HasPrefix(base, ".env"), strings.HasSuffix(n, ".yml"), strings.HasSuffix(n, ".yaml"), strings.HasSuffix(n, ".toml"), strings.HasSuffix(n, ".ini"), strings.HasSuffix(n, ".json"):
		return 2
	}
	return 0
}

// sameStuck reports whether s repeats an earlier LoopStuck: the same call,
// or the same tool failing on the same file.
func sameStuck(prior []tools.LoopStuck, s tools.LoopStuck) bool {
	for _, p := range prior {
		if (p.Fingerprint != "" && p.Fingerprint == s.Fingerprint) || (p.Path != "" && p.Path == s.Path && p.Tool == s.Tool) {
			return true
		}
	}
	return false
}

// stuckPaths are the files of LoopStuck failures.
func stuckPaths(stuck []tools.LoopStuck) []string {
	var out []string
	for _, s := range stuck {
		if s.Path != "" {
			out = append(out, s.Path)
		}
	}
	return out
}

// continuationRequest is a continuation pass's first request: the system
// prompt and the task (what each remaining file needs is only there),
// followed by the continuation prompt, in one user message.
func continuationRequest(req adapter.CompletionRequest, prompt string) adapter.CompletionRequest {
	user := prompt
	if len(req.Messages) > 1 && req.Messages[1].Role == adapter.RoleUser {
		user = req.Messages[1].Content + "\n\n## Continuation\n" + prompt
	}
	return adapter.CompletionRequest{
		Messages:    []adapter.ChatMessage{req.Messages[0], {Role: adapter.RoleUser, Content: user}},
		Tools:       req.Tools,
		ToolChoice:  req.ToolChoice,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
	}
}

// incompletePhrases in the model's final message mean it has not finished.
var incompletePhrases = []string{"let me ", "now i need to", "next step", "continue with", "remaining step", "i'll ", "i will ", "still need"}

// continuationDecision decides whether another continuation pass runs after
// pass (0-based) and which scope files it targets. missing is every scope
// file not yet changed on the branch. A missing file a step cites always
// continues: the step is not done, whatever the model's summary says
// (a live run, attempt 6, stopped with the HTML and docs steps untouched). Only
// when the missing files are outside the steps (a legacy task, or a path
// only research listed) does the old heuristic apply: on the first pass,
// with at least half the scope changed and a summary that sounds finished,
// stop. The caller bounds the passes (maxContinuations).
func continuationDecision(scope, stepFiles []string, changed map[string]bool, summary string, pass int, stuckFiles ...string) (missing []string, cont bool) {
	done := make(map[string]bool, len(changed))
	for f := range changed {
		done[plan.NormalizeScopePath(f)] = true
	}
	for _, f := range scope {
		if !done[plan.NormalizeScopePath(f)] {
			missing = append(missing, f)
		}
	}
	if len(missing) == 0 {
		return nil, false
	}
	step := make(map[string]bool, len(stepFiles))
	for _, f := range stepFiles {
		step[plan.NormalizeScopePath(f)] = true
	}
	// A file a loop got stuck on is unfinished work too (R10 W4): the next
	// pass gets the failure and a different approach, or ends at once.
	for _, f := range stuckFiles {
		step[plan.NormalizeScopePath(f)] = true
	}
	for _, f := range missing {
		if step[plan.NormalizeScopePath(f)] {
			return missing, true
		}
	}
	touchedRatio := float64(len(scope)-len(missing)) / float64(len(scope))
	if touchedRatio >= 0.5 && pass == 0 {
		lower := strings.ToLower(summary)
		for _, phrase := range incompletePhrases {
			if strings.Contains(lower, phrase) {
				return missing, true
			}
		}
		return missing, false
	}
	return missing, true
}
