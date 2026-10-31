package failures

import (
	"regexp"
	"strconv"
	"strings"
)

// Go: `go build` / `go vet` / `go test` output.
//
//	calc/calc.go:4:13: undefined: c                  compile (lint under go vet)
//	--- FAIL: TestAdd (0.00s)                         test_assert, with the
//	    calc_test.go:7: Add(2, 3) = -1, want 5          indented t.Error lines
//	panic: runtime error: index out of range [3] …   test_panic (runtime outside go test),
//	goroutine 6 [running]: … calc/calc.go:3            located at the first non-std frame
//	panic: test timed out after 1s                    timeout
//	FAIL	pkg [build failed]                         (the compile lines above it)

var (
	reGoDiag     = regexp.MustCompile(`^(?:vet: )?((?:[A-Za-z]:)?[^\s:][^:]*\.go):(\d+)(?::(\d+))?: (.+)$`)
	reGoFail     = regexp.MustCompile(`^(\s*)--- FAIL: (\S+)`)
	reGoTestLine = regexp.MustCompile(`^\s+([^\s:]+\.go):(\d+): (.*)$`)
	reGoPanic    = regexp.MustCompile(`^panic: (.*?)(?: \[recovered(?:, repanicked)?\])?$`)
	reGoFrame    = regexp.MustCompile(`^\t((?:[A-Za-z]:)?/?[^\t]*\.go):(\d+)(?: \+0x[0-9a-f]+)?$`)
	reGoTimeout  = regexp.MustCompile(`^panic: test timed out after (\S+)`)
	reGoRunning  = regexp.MustCompile(`^\s+(Test\S*) \(`)
)

func parseGo(out string) []Record {
	ls := lines(out)
	isGoOutput := false
	var recs []Record
	// Test failures: --- FAIL lines own the indented lines below them.
	type failing struct {
		name   string
		indent int
		msgs   []Record
		hasSub bool
	}
	var stack []*failing
	var tests []*failing
	lastFail := ""
	for i := 0; i < len(ls); i++ {
		l := ls[i]
		if strings.HasPrefix(l, "# ") {
			isGoOutput = true
			continue
		}
		if m := reGoFail.FindStringSubmatch(l); m != nil {
			isGoOutput = true
			f := &failing{name: m[2], indent: len(m[1])}
			for len(stack) > 0 && stack[len(stack)-1].indent >= f.indent {
				stack = stack[:len(stack)-1]
			}
			if len(stack) > 0 {
				stack[len(stack)-1].hasSub = true
			}
			stack = append(stack, f)
			tests = append(tests, f)
			lastFail = f.name
			continue
		}
		if m := reGoTimeout.FindStringSubmatch(l); m != nil {
			isGoOutput = true
			name := ""
			for j := i + 1; j < len(ls) && j < i+6; j++ {
				if mm := reGoRunning.FindStringSubmatch(ls[j]); mm != nil {
					name = mm[1]
					break
				}
			}
			file, line := goPanicSite(ls, i+1)
			recs = append(recs, Record{Category: CategoryTimeout, Test: name, File: file, Line: line,
				Message: "test timed out after " + m[1], Span: spanOf(ls, i, i+4)})
			return recs // the stacks after a timeout are every goroutine; nothing else is reliable
		}
		if m := reGoPanic.FindStringSubmatch(l); m != nil {
			isGoOutput = true
			file, line := goPanicSite(ls, i+1)
			cat := CategoryRuntime
			if lastFail != "" {
				cat = CategoryTestPanic
			}
			recs = append(recs, Record{Category: cat, Test: lastFail, File: file, Line: line,
				Message: "panic: " + m[1], Span: spanOf(ls, i, goStackEnd(ls, i))})
			// The rest of the output is the stack; a test binary stops at
			// its first panic.
			i = goStackEnd(ls, i) - 1
			continue
		}
		if len(stack) > 0 {
			if m := reGoTestLine.FindStringSubmatch(l); m != nil && leadingSpaces(l) > stack[len(stack)-1].indent {
				top := stack[len(stack)-1]
				ln, _ := strconv.Atoi(m[2])
				msg := m[3]
				// Continuation lines of a multi-line t.Error message are
				// indented further than the message line.
				end := i + 1
				for end < len(ls) && leadingSpaces(ls[end]) > leadingSpaces(l) && !reGoFail.MatchString(ls[end]) {
					msg += "\n" + strings.TrimSpace(ls[end])
					end++
				}
				top.msgs = append(top.msgs, Record{Category: CategoryTestAssert, Test: top.name, File: m[1], Line: ln, Message: msg, Span: spanOf(ls, i, end)})
				i = end - 1
				continue
			}
		}
		if m := reGoDiag.FindStringSubmatch(strings.TrimRight(l, " ")); m != nil && !strings.HasPrefix(l, "\t") {
			isGoOutput = true
			ln, _ := strconv.Atoi(m[2])
			col, _ := strconv.Atoi(m[3])
			recs = append(recs, Record{Category: CategoryCompile, File: m[1], Line: ln, Col: col, Message: m[4], Span: l})
			continue
		}
		if strings.HasPrefix(l, "FAIL\t") || strings.HasPrefix(l, "ok  \t") || l == "FAIL" {
			isGoOutput = true
		}
	}
	for _, t := range tests {
		if len(t.msgs) > 0 {
			recs = append(recs, t.msgs...)
			continue
		}
		if t.hasSub || hasTestPanic(recs, t.name) {
			continue // failed only because a subtest failed or it panicked
		}
		recs = append(recs, Record{Category: CategoryTestAssert, Test: t.name, Message: "test failed with no message", Span: "--- FAIL: " + t.name})
	}
	if !isGoOutput {
		return nil
	}
	return recs
}

// goVetCategory: under `go vet` a diagnostic is a lint finding, unless it
// is a type error (vet type-checks first).
func goVetCategory(recs []Record, command string) []Record {
	if !strings.Contains(command, "go vet") {
		return recs
	}
	for i := range recs {
		if recs[i].Category == CategoryCompile && !isTypeError(recs[i].Message) {
			recs[i].Category = CategoryLint
		}
	}
	return recs
}

// isTypeError: go vet type-checks first; a type error under vet is still
// a compile error.
func isTypeError(msg string) bool {
	for _, p := range []string{"undefined:", "cannot use", "not enough arguments", "too many arguments", "imported and not used", "declared and not used", "has no field or method", "missing return", "syntax error", "cannot find package", "no required module", "cannot find module"} {
		if strings.Contains(msg, p) {
			return true
		}
	}
	return false
}

func hasTestPanic(recs []Record, name string) bool {
	for _, r := range recs {
		if r.Test == name && (r.Category == CategoryTestPanic || r.Category == CategoryTimeout) {
			return true
		}
	}
	return false
}

// goPanicSite returns the first stack frame below line from that is not in
// the Go runtime, the testing package or the standard library.
func goPanicSite(ls []string, from int) (string, int) {
	firstFile, firstLine := "", 0
	depFile, depLine := "", 0
	for i := from; i < len(ls); i++ {
		m := reGoFrame.FindStringSubmatch(ls[i])
		if m == nil {
			continue
		}
		ln, _ := strconv.Atoi(m[2])
		fn := ""
		if i > 0 {
			fn = strings.TrimSpace(ls[i-1])
		}
		if firstFile == "" {
			firstFile, firstLine = m[1], ln
		}
		if isStdFrame(fn, m[1]) {
			continue
		}
		if reDependencyPath.MatchString(strings.ReplaceAll(m[1], `\`, "/")) {
			if depFile == "" {
				depFile, depLine = m[1], ln
			}
			continue
		}
		return m[1], ln
	}
	if depFile != "" {
		return depFile, depLine
	}
	return firstFile, firstLine
}

// reGoroot matches a file in a Go installation's standard library:
// /usr/local/go/src, C:/Program Files/Go/src, Homebrew's libexec/src, a
// downloaded toolchain's module.
var reGoroot = regexp.MustCompile(`(?i)(/usr/local/go|/usr/lib/go[^/]*|/program files/go|/libexec|/golang\.org/toolchain@[^/]+|/goroot[^/]*)/src/`)

func isStdFrame(fn, file string) bool {
	f := strings.ReplaceAll(file, `\`, "/")
	return strings.HasPrefix(fn, "panic(") || strings.HasPrefix(fn, "created by ") ||
		strings.HasSuffix(f, "_testmain.go") || reGoroot.MatchString(f)
}

// goStackEnd is the index after the panic's stack: the go test FAIL
// summary, or the end of the output.
func goStackEnd(ls []string, from int) int {
	for i := from + 1; i < len(ls); i++ {
		if strings.HasPrefix(ls[i], "FAIL\t") || ls[i] == "FAIL" || strings.HasPrefix(ls[i], "exit status ") {
			return i
		}
	}
	return len(ls)
}

func leadingSpaces(s string) int {
	n := 0
	for _, c := range s {
		switch c {
		case ' ':
			n++
		case '\t':
			n += 4
		default:
			return n
		}
	}
	return n
}
