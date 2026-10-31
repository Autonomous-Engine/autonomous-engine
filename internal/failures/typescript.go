package failures

import (
	"regexp"
	"strconv"
	"strings"
)

// TypeScript / JavaScript: tsc diagnostics (plain and --pretty), vitest
// failure blocks and jest failure blocks.
//
//	src/sum.ts(5,14): error TS2322: Type 'string' is …        typecheck
//	src/sum.ts:5:14 - error TS2322: Type 'string' is …        typecheck (pretty)
//	 FAIL  src/sum.test.ts > sum > adds numbers                vitest: the next line is
//	AssertionError: expected -1 to be 3                          the error, "❯ file:line:col"
//	 ❯ src/sum.test.ts:6:23                                      the location
//	FAIL ./other.test.js                                       jest: "● suite › test" blocks,
//	  ● sum › adds numbers                                       message, then "at … (file:l:c)"

var (
	reTSC        = regexp.MustCompile(`^(\S.*?\.[cm]?[jt]sx?)\((\d+),(\d+)\): error (TS\d+): (.*)$`)
	reTSCPretty  = regexp.MustCompile(`^(\S.*?\.[cm]?[jt]sx?):(\d+):(\d+) - error (TS\d+): (.*)$`)
	reVitestFail = regexp.MustCompile(`^ ?FAIL\s+(\S+\.[cm]?[jt]sx?)(?: > (.+?))?(?:\s+\[.*\])?\s*$`)
	reVitestLoc  = regexp.MustCompile(`^\s*(?:❯|>|at)\s+(?:.*\()?([^\s()]+\.[cm]?[jt]sx?):(\d+):(\d+)\)?\s*$`)
	reJestFile   = regexp.MustCompile(`^FAIL\s+(\S+)`)
	reJestBlock  = regexp.MustCompile(`^\s+● (.+)$`)
	reJestAt     = regexp.MustCompile(`^\s+at .*?\(?([^\s()]+\.[cm]?[jt]sx?):(\d+):(\d+)\)?$`)
	reJSError    = regexp.MustCompile(`^(?:[A-Z][A-Za-z]*Error|Error)(?: \[[^\]]+\])?: `)
	reJestCode   = regexp.MustCompile(`^\s*>\s*(\d+) \|`)
)

func parseTypeScript(out string) []Record {
	ls := lines(out)
	var recs []Record
	for i, l := range ls {
		t := strings.TrimRight(l, " ")
		if m := reTSC.FindStringSubmatch(t); m != nil {
			recs = append(recs, tscRecord(m, ls, i))
		} else if m := reTSCPretty.FindStringSubmatch(t); m != nil {
			recs = append(recs, tscRecord(m, ls, i))
		}
	}
	recs = append(recs, parseVitest(ls)...)
	recs = append(recs, parseJest(ls)...)
	return recs
}

func tscRecord(m []string, ls []string, i int) Record {
	ln, _ := strconv.Atoi(m[2])
	col, _ := strconv.Atoi(m[3])
	cat := CategoryTypecheck
	if m[4] == "TS1005" || m[4] == "TS1128" || m[4] == "TS1109" { // syntax errors
		cat = CategoryCompile
	}
	return Record{Category: cat, File: m[1], Line: ln, Col: col, Message: m[4] + ": " + m[5], Span: spanOf(ls, i, i+4)}
}

// parseVitest reads the detailed " FAIL  file > suite > test" blocks
// (vitest prints a one-line summary of each failure first; the detailed
// block after it carries the error and its location).
func parseVitest(ls []string) []Record {
	var recs []Record
	for i := 0; i < len(ls); i++ {
		m := reVitestFail.FindStringSubmatch(ls[i])
		if m == nil || !strings.HasPrefix(strings.TrimLeft(ls[i], " "), "FAIL ") || !strings.Contains(ls[i], "  ") {
			continue
		}
		// vitest's detailed header is " FAIL  <file> > <test>" (two spaces);
		// jest's file header is "FAIL ./x.test.js" (one space, no leading space).
		if !strings.HasPrefix(ls[i], " FAIL ") {
			continue
		}
		rec := Record{File: m[1], Test: strings.TrimSpace(m[2])}
		end := i + 1
		for end < len(ls) && !reVitestFail.MatchString(ls[end]) && !strings.HasPrefix(strings.TrimSpace(ls[end]), "⎯⎯") && !strings.HasPrefix(strings.TrimSpace(ls[end]), "Test Files") {
			end++
		}
		for j := i + 1; j < end; j++ {
			t := strings.TrimSpace(ls[j])
			if rec.Message == "" && t != "" {
				rec.Message = t
			}
			if lm := reVitestLoc.FindStringSubmatch(ls[j]); lm != nil && !strings.Contains(lm[1], "node_modules") {
				rec.File = lm[1]
				rec.Line, _ = strconv.Atoi(lm[2])
				rec.Col, _ = strconv.Atoi(lm[3])
				break
			}
		}
		rec.Category = jsCategory(rec.Message, rec.Test != "")
		rec.Span = spanOf(ls, i, end)
		recs = append(recs, rec)
		i = end - 1
	}
	return recs
}

// parseJest reads "FAIL <file>" sections and their "● name" blocks.
func parseJest(ls []string) []Record {
	var recs []Record
	file := ""
	for i := 0; i < len(ls); i++ {
		if m := reJestFile.FindStringSubmatch(ls[i]); m != nil && !strings.HasPrefix(ls[i], " ") {
			file = strings.TrimPrefix(m[1], "./")
			continue
		}
		m := reJestBlock.FindStringSubmatch(ls[i])
		if m == nil || file == "" {
			continue
		}
		name := strings.TrimSpace(m[1])
		end := i + 1
		for end < len(ls) && !reJestBlock.MatchString(ls[end]) && !reJestFile.MatchString(ls[end]) && !strings.HasPrefix(ls[end], "Test Suites:") {
			end++
		}
		rec := Record{File: file}
		suiteFailed := name == "Test suite failed to run"
		if !suiteFailed {
			rec.Test = strings.ReplaceAll(name, " › ", " > ")
		}
		for j := i + 1; j < end; j++ {
			t := strings.TrimSpace(ls[j])
			if rec.Message == "" && t != "" {
				rec.Message = t
			}
			if cm := reJestCode.FindStringSubmatch(ls[j]); cm != nil && rec.Line == 0 {
				rec.Line, _ = strconv.Atoi(cm[1])
			}
			if am := reJestAt.FindStringSubmatch(ls[j]); am != nil && !strings.Contains(am[1], "node_modules") {
				rec.File = strings.TrimPrefix(am[1], "./")
				rec.Line, _ = strconv.Atoi(am[2])
				rec.Col, _ = strconv.Atoi(am[3])
				break
			}
		}
		rec.Category = jsCategory(rec.Message, !suiteFailed)
		rec.Span = spanOf(ls, i, end)
		recs = append(recs, rec)
		i = end - 1
	}
	return recs
}

// jsCategory classifies a JS test failure's first message line.
func jsCategory(msg string, inTest bool) Category {
	switch {
	case strings.Contains(msg, "Cannot find module") || strings.Contains(msg, "Failed to resolve import") || strings.Contains(msg, "Failed to load url") || strings.HasPrefix(msg, "SyntaxError"):
		return CategoryCompile
	case strings.HasPrefix(msg, "AssertionError") || strings.HasPrefix(msg, "expect(") || strings.HasPrefix(msg, "Expected") || strings.Contains(msg, "toBe") || strings.Contains(msg, "toEqual"):
		return CategoryTestAssert
	case reJSError.MatchString(msg):
		if inTest {
			return CategoryTestPanic
		}
		return CategoryRuntime
	case inTest:
		return CategoryTestAssert
	}
	return CategoryRuntime
}
