package tools

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/controller"
)

// Test-output filter (T5). A live run's (attempt 6) two `npm test` results were
// 9.9k and 9.8k chars, about 60% pino JSON log lines and passing tests,
// around a failure block and totals under 1k, and two of them sat in the
// transcript at once. For a known test runner run_command keeps the
// summary lines, every failure block (bounded) and the last 40 lines, drops
// log and progress noise, and says how much it dropped. raw=true returns
// the output as it was.
const (
	testFilterMinLines   = 60  // shorter output is returned as is
	testFilterTailLines  = 40  // the last non-noise lines, always kept
	testFilterBlockLines = 25  // lines kept per failure block
	testFilterFailureMax = 200 // failure-block lines kept in all
)

var (
	ansiRE = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

	// noiseRE: log lines, passing tests, progress lines.
	noiseRE = regexp.MustCompile(`^(\s*\{.*"(level|msg|time)"\s*:.*\}\s*$` + // JSON log lines (pino, bunyan, zap)
		`|\s*[✓√✔]\s` + // a passing test (vitest, jest, mocha)
		`|PASS\s+\S` + // jest: a passing file
		`|=== (RUN|PAUSE|CONT|NAME)\s` + // go test -v progress
		`|\s*--- (PASS|SKIP):\s` + // go test -v: passed / skipped
		`|ok\s+\S+\s+(\(cached\)|[\d.]+s)` + // go test: a passing package
		`|\S+::\S+ PASSED` + // pytest -v
		`|(stderr|stdout) \|\s` + // vitest console capture headers
		`|\s*console\.(log|info|debug|warn)\s*$` + // jest console capture headers
		`|\s*● Console\s*$` +
		`)`)

	// failureStartRE opens a failure block.
	failureStartRE = regexp.MustCompile(`^(\s*[×✗✕]\s` + // vitest, mocha
		`|\s*FAIL\s+\S` + // vitest / jest: a failing file or test
		`|\s*●\s` + // jest: a failing test
		`|\s*--- FAIL:` + // go test
		`|panic:` + // go
		`|_{3,} .+ _{3,}$` + // pytest failure header
		`|FAILED\s` + // pytest short summary
		`|.*(AssertionError|Error:|Exception:))`)

	// summaryRE: totals and verdicts.
	summaryRE = regexp.MustCompile(`^\s*(Test Files|Tests|Test Suites|Snapshots|Time|Duration|Ran all|Ran \d+` +
		`|FAIL\s*$|FAIL\s+\S+\s+[\d.]+s|ok\s*$|PASS\s*$|=+ .*(passed|failed|error).* =+$` +
		`|\d+ (passing|failing|pending)\b|.*Failed Tests \d+)`)

	goRunRE  = regexp.MustCompile(`^=== RUN\s+(\S+)`)
	goPassRE = regexp.MustCompile(`^\s*--- (PASS|SKIP): (\S+)`)
	goFailRE = regexp.MustCompile(`^\s*--- FAIL: (\S+)`)
)

// isTestRunnerCommand reports a command the filter applies to: the usual
// test runners (controller.IsTestCommand), also when piped (npm test |
// tail).
func isTestRunnerCommand(command string) bool {
	if controller.IsTestCommand(command, "") {
		return true
	}
	if i := strings.Index(command, "|"); i > 0 {
		return controller.IsTestCommand(strings.TrimSpace(command[:i]), "")
	}
	return false
}

// ShapeRunOutput is run_command's result text for command's output: cut to
// failures, totals and the last lines for a test runner unless raw is set.
// ok reports whether anything was dropped. Offline harnesses use it to
// reproduce run_command results.
func ShapeRunOutput(command, out string, raw bool) (string, bool) {
	return shapeRunOutput(command, out, raw)
}

// shapeRunOutput is ShapeRunOutput.
func shapeRunOutput(command, out string, raw bool) (string, bool) {
	if raw || !isTestRunnerCommand(command) {
		return out, false
	}
	return filterTestOutput(out)
}

// filterTestOutput cuts a test runner's output to what the model needs; it
// returns out unchanged (and false) when it is short or nothing could be
// dropped.
func filterTestOutput(out string) (string, bool) {
	clean := ansiRE.ReplaceAllString(strings.ReplaceAll(out, "\r\n", "\n"), "")
	lines := strings.Split(clean, "\n")
	if len(lines) < testFilterMinLines {
		return out, false
	}
	noise := make([]bool, len(lines))
	for i, ln := range lines {
		noise[i] = noiseRE.MatchString(ln)
	}
	// go test -v: a test's log lines follow its "=== RUN" line; those of a
	// test that passed are noise, those of a failed test belong to its
	// failure block (the "--- FAIL" line comes after them).
	runStart := map[string]int{}
	passed := map[string]bool{}
	for i, ln := range lines {
		if m := goRunRE.FindStringSubmatch(ln); m != nil {
			runStart[m[1]] = i
		} else if m := goPassRE.FindStringSubmatch(ln); m != nil {
			passed[m[2]] = true
		}
	}
	runEnd := func(start int) int {
		for j := start + 1; j < len(lines); j++ {
			if goRunRE.MatchString(lines[j]) || strings.HasPrefix(lines[j], "--- ") {
				return j
			}
		}
		return len(lines)
	}
	for name, start := range runStart {
		if passed[name] {
			for j := start + 1; j < runEnd(start); j++ {
				noise[j] = true
			}
		}
	}

	keep := make([]bool, len(lines))
	for i, ln := range lines {
		if !noise[i] && summaryRE.MatchString(ln) {
			keep[i] = true
		}
	}
	// Failure blocks: every opening line (a failing test's name) is kept;
	// only the bodies share the testFilterFailureMax budget.
	failureKept := 0
	failure := make([]bool, len(lines))
	keepLine := func(j int) {
		if !keep[j] && failureKept < testFilterFailureMax {
			keep[j], failure[j] = true, true
			failureKept++
		}
	}
	for i := 0; i < len(lines); i++ {
		if noise[i] || !failureStartRE.MatchString(lines[i]) {
			continue
		}
		keep[i], failure[i] = true, true
		if m := goFailRE.FindStringSubmatch(lines[i]); m != nil {
			if start, ok := runStart[m[1]]; ok {
				for j, n := start+1, 0; j < runEnd(start) && n < testFilterBlockLines; j, n = j+1, n+1 {
					keepLine(j)
				}
			}
			continue
		}
		for j, n := i+1, 1; j < len(lines) && n < testFilterBlockLines; j, n = j+1, n+1 {
			if noise[j] || (failureStartRE.MatchString(lines[j]) && !strings.HasPrefix(lines[j], " ")) {
				break
			}
			keepLine(j)
		}
	}
	for i, tail := len(lines)-1, 0; i >= 0 && tail < testFilterTailLines; i-- {
		if !noise[i] && strings.TrimSpace(lines[i]) != "" {
			keep[i] = true
			tail++
		}
	}

	// Duplicates: a failure region (consecutive kept failure lines) that
	// repeats an earlier one word for word is dropped whole (repeated
	// stderr errors); inside a distinct region nothing is deduplicated, so
	// an assertion shared by several failing tests stays under each. Other
	// lines are deduplicated one by one.
	dupRegion := make([]bool, len(lines))
	seenRegion := map[string]bool{}
	for i := 0; i < len(lines); {
		if !failure[i] {
			i++
			continue
		}
		j := i
		var parts []string
		for j < len(lines) && (failure[j] || (strings.TrimSpace(lines[j]) == "" && j+1 < len(lines) && failure[j+1])) {
			parts = append(parts, strings.TrimSpace(lines[j]))
			j++
		}
		key := strings.Join(parts, "\n")
		if seenRegion[key] {
			for k := i; k < j; k++ {
				dupRegion[k] = !summaryRE.MatchString(lines[k])
			}
		}
		seenRegion[key] = true
		i = j
	}

	var b strings.Builder
	dropped := 0
	seen := map[string]bool{}
	gap := false
	for i, ln := range lines {
		t := strings.TrimSpace(ln)
		dup := dupRegion[i] || (!failure[i] && t != "" && seen[t] && !summaryRE.MatchString(ln))
		if !keep[i] || dup {
			if t != "" {
				dropped++
				gap = true
			}
			continue
		}
		if t == "" {
			if b.Len() > 0 && !strings.HasSuffix(b.String(), "\n\n") {
				b.WriteString("\n")
			}
			continue
		}
		if gap && b.Len() > 0 && !strings.HasSuffix(b.String(), "…\n") {
			b.WriteString("…\n")
		}
		gap = false
		seen[t] = true
		b.WriteString(strings.TrimRight(ln, " \t"))
		b.WriteString("\n")
	}
	if dropped == 0 {
		return out, false
	}
	fmt.Fprintf(&b, "(filtered %d lines; rerun with raw=true to see all)", dropped)
	return b.String(), true
}
