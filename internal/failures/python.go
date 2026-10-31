package failures

import (
	"regexp"
	"strconv"
	"strings"
)

// Python: pytest's short test summary (with the raise site from the
// failure sections) and plain tracebacks.
//
//	FAILED tests/test_calc.py::test_add - assert -1 == 5     test_assert at tests/test_calc.py:5
//	FAILED tests/test_calc.py::test_div - ZeroDivisionError… test_panic at calc.py:6
//	ERROR tests/test_imports.py                              compile (E   ModuleNotFoundError: …)
//	Traceback (most recent call last): … File "x.py", line 6  runtime at the last repository frame
//	  File "bad.py", line 1 … SyntaxError: invalid syntax     compile

var (
	rePySummary   = regexp.MustCompile(`^(FAILED|ERROR) (\S+?)(?:::(\S+))?(?: - (.*))?$`)
	rePySection   = regexp.MustCompile(`^_{3,} (?:ERROR collecting )?(.+?) _{3,}$`)
	rePyRaiseSite = regexp.MustCompile(`^(\S+\.py):(\d+): ?(\w*)`)
	rePyELine     = regexp.MustCompile(`^E\s+(.*)$`)
	rePyFrame     = regexp.MustCompile(`^\s*File "([^"]+)", line (\d+)`)
	rePyException = regexp.MustCompile(`^([A-Za-z_][\w.]*(?:Error|Exception|Exit|Interrupt|Warning)|AssertionError|StopIteration|KeyboardInterrupt)(?:: (.*))?$`)
)

func parsePython(out string) []Record {
	ls := lines(out)
	if recs := parsePytest(ls); len(recs) > 0 {
		return recs
	}
	return parseTraceback(ls)
}

func parsePytest(ls []string) []Record {
	// Failure sections by test name, for the raise site and E lines.
	sections := map[string][2]int{}
	cur, start := "", 0
	for i, l := range ls {
		if m := rePySection.FindStringSubmatch(l); m != nil {
			if cur != "" {
				sections[cur] = [2]int{start, i}
			}
			cur, start = m[1], i
			continue
		}
		if strings.HasPrefix(l, "====") && cur != "" {
			sections[cur] = [2]int{start, i}
			cur = ""
		}
	}
	var recs []Record
	for _, l := range ls {
		m := rePySummary.FindStringSubmatch(strings.TrimRight(l, " "))
		if m == nil {
			continue
		}
		kind, file, test, msg := m[1], m[2], m[3], m[4]
		rec := Record{File: file, Message: msg, Span: l}
		if test != "" {
			rec.Test = file + "::" + test
		}
		key := test
		if key == "" {
			key = file
		}
		if sec, ok := sections[key]; ok {
			rec.Span = spanOf(ls, sec[0], sec[1])
			// The raise site is the last "file:line: Exc" line of the
			// section; the E lines carry the exception text.
			for j := sec[0]; j < sec[1]; j++ {
				if rm := rePyRaiseSite.FindStringSubmatch(ls[j]); rm != nil && (rec.Line == 0 || !reDependencyPath.MatchString(rm[1])) {
					rec.File = rm[1]
					rec.Line, _ = strconv.Atoi(rm[2])
				}
				if em := rePyELine.FindStringSubmatch(ls[j]); em != nil && (rec.Message == "" || kind == "ERROR") && rePyException.MatchString(strings.TrimSpace(em[1])) {
					rec.Message = strings.TrimSpace(em[1])
				}
			}
		}
		if rec.Message == "" {
			rec.Message = strings.TrimSpace(l)
		}
		rec.Category = pyCategory(rec.Message, kind == "FAILED" && test != "")
		recs = append(recs, rec)
	}
	return recs
}

func parseTraceback(ls []string) []Record {
	var recs []Record
	for i := 0; i < len(ls); i++ {
		isTB := strings.HasPrefix(ls[i], "Traceback (most recent call last):")
		isSyntax := !isTB && rePyFrame.MatchString(ls[i]) && syntaxErrorBelow(ls, i)
		if !isTB && !isSyntax {
			continue
		}
		file, line := "", 0
		j := i
		for ; j < len(ls); j++ {
			if fm := rePyFrame.FindStringSubmatch(ls[j]); fm != nil {
				// The last frame in the repository is the failure's site;
				// library frames only count when there is no other.
				if file == "" || !reDependencyPath.MatchString(fm[1]) {
					file = fm[1]
					line, _ = strconv.Atoi(fm[2])
				}
				continue
			}
			if strings.HasPrefix(ls[j], " ") {
				continue
			}
			if rePyException.MatchString(strings.TrimSpace(ls[j])) {
				msg := strings.TrimSpace(ls[j])
				recs = append(recs, Record{Category: pyCategory(msg, false), File: file, Line: line, Message: msg, Span: spanOf(ls, i, j+1)})
				break
			}
		}
		i = j
	}
	return recs
}

func syntaxErrorBelow(ls []string, i int) bool {
	for j := i + 1; j < len(ls) && j <= i+4; j++ {
		t := strings.TrimSpace(ls[j])
		if strings.HasPrefix(t, "SyntaxError") || strings.HasPrefix(t, "IndentationError") || strings.HasPrefix(t, "TabError") {
			return true
		}
	}
	return false
}

// pyCategory classifies a Python failure message.
func pyCategory(msg string, inTest bool) Category {
	switch {
	case strings.HasPrefix(msg, "SyntaxError") || strings.HasPrefix(msg, "IndentationError") || strings.HasPrefix(msg, "TabError") ||
		strings.HasPrefix(msg, "ModuleNotFoundError") || strings.HasPrefix(msg, "ImportError"):
		return CategoryCompile
	case strings.HasPrefix(msg, "assert") || strings.HasPrefix(msg, "AssertionError"):
		return CategoryTestAssert
	case inTest:
		return CategoryTestPanic
	}
	return CategoryRuntime
}
