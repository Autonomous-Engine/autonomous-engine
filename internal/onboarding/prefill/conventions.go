package prefill

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/onboarding"
)

// The patterns_discovered and test_conventions answers were statistics
// ("test pattern in 39 files", "`_test.` suffix across 39 files"), which
// confirming gave agents nothing to follow (F26). Each answer now states
// the rule the files show: the suffix form a role uses, where test files
// live and how the suite runs.

// roleGroups are the pattern groups that name a file's role; test and
// mock files are test_conventions' subject.
func roleGroups(groups []PatternGroupSummary) []PatternGroupSummary {
	var out []PatternGroupSummary
	for _, g := range groups {
		if g.Count < 2 || g.Pattern == "test" || g.Pattern == "mock" || len(g.RefFiles) == 0 {
			continue
		}
		out = append(out, g)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	return out
}

// suffixForm is how the sampled files spell the role: "_handler.go" for
// user_handler.go, ".handler.ts" for user.handler.ts; "" when none of the
// samples carries the role in its name.
func suffixForm(role string, refFiles []string) string {
	for _, f := range refFiles {
		base := path.Base(strings.ReplaceAll(f, "\\", "/"))
		for _, sep := range []string{"_", "."} {
			if i := strings.LastIndex(base, sep+role+"."); i >= 0 {
				return base[i:]
			}
		}
	}
	return ""
}

// patternsAnswer is the patterns_discovered answer, or why there is none.
func patternsAnswer(groups []PatternGroupSummary) (value string, evidence []onboarding.Evidence, notDetected string) {
	roles := roleGroups(groups)
	if len(roles) == 0 {
		return "", nil, "only test and mock files group by name; no role-based naming convention (handler, service, repository, …) with 2+ files — ask the user how files are named"
	}
	var rules []string
	for _, g := range roles {
		form := suffixForm(g.Pattern, g.RefFiles)
		if form == "" {
			continue
		}
		rules = append(rules, fmt.Sprintf("- `*%s` for a %s (%d files, e.g. %s)", form, g.Pattern, g.Count, strings.Join(baseNames(firstN(g.RefFiles, 2)), ", ")))
		for _, f := range firstN(g.RefFiles, 2) {
			evidence = append(evidence, onboarding.Evidence{Kind: "file", Path: f, Name: g.Pattern})
		}
	}
	if len(rules) == 0 {
		return "", nil, "the sampled files do not carry their role in the file name — ask the user how files are named"
	}
	value = "Files are named by role, with the role as a suffix:\n" + strings.Join(rules, "\n") +
		"\nNew code of the same kind takes the same suffix, next to the files of its package."
	return value, evidence, ""
}

// testConventionsAnswer is the test_conventions answer, or why there is
// none: the test file form, where the sampled files live, the command,
// and the mock form when mocks group by name.
func testConventionsAnswer(groups []PatternGroupSummary, testCommand string) (value string, evidence []onboarding.Evidence, notDetected string) {
	var tests, mocks *PatternGroupSummary
	for i := range groups {
		switch groups[i].Pattern {
		case "test":
			tests = &groups[i]
		case "mock":
			mocks = &groups[i]
		}
	}
	if tests == nil || tests.Count < 2 || len(tests.RefFiles) == 0 {
		return "", nil, "no group of 2+ test files (*_test.*, *.test.*, *.spec.*) — ask the user where tests live and how they run"
	}
	forms := testForms(tests.RefFiles)
	if len(forms) == 0 {
		return "", nil, "the sampled test files carry no test marker in their names — ask the user how tests are named"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Tests are %s files %s (%d files, e.g. %s).", strings.Join(forms, " / "), testPlacement(tests.RefFiles), tests.Count, strings.Join(baseNames(firstN(tests.RefFiles, 2)), ", "))
	if testCommand != "" {
		fmt.Fprintf(&b, " The suite runs with `%s`.", testCommand)
	}
	b.WriteString(" A new test file is named after the file it covers and placed the same way.")
	for _, f := range firstN(tests.RefFiles, 2) {
		evidence = append(evidence, onboarding.Evidence{Kind: "file", Path: f, Name: "test"})
	}
	if mocks != nil && mocks.Count >= 1 && len(mocks.RefFiles) > 0 {
		if mf := testForm(mocks.RefFiles); mf != "" {
			fmt.Fprintf(&b, " Mocks are `*%s` files (%d).", mf, mocks.Count)
		} else if strings.Contains(strings.ReplaceAll(mocks.RefFiles[0], "\\", "/"), "__mocks__/") {
			fmt.Fprintf(&b, " Mocks live in `__mocks__` directories (%d files).", mocks.Count)
		}
		evidence = append(evidence, onboarding.Evidence{Kind: "file", Path: mocks.RefFiles[0], Name: "mock"})
	}
	return b.String(), evidence, ""
}

// testForms are the distinct test file forms the samples use, as glob
// text ("`*.test.ts`", "`*.test.tsx`"), sorted.
func testForms(refFiles []string) []string {
	seen := map[string]bool{}
	for _, f := range refFiles {
		if form := testForm([]string{f}); form != "" && !seen[form] {
			seen[form] = true
		}
	}
	out := make([]string, 0, len(seen))
	for form := range seen {
		out = append(out, "`*"+form+"`")
	}
	sort.Strings(out)
	return out
}

// testForm is the test (or mock) marker and extension the sampled files
// use: "_test.go", ".test.ts", ".spec.js", "_mock.go".
func testForm(refFiles []string) string {
	for _, f := range refFiles {
		base := path.Base(strings.ReplaceAll(f, "\\", "/"))
		for _, marker := range []string{"_test.", ".test.", ".spec.", "_spec.", "_mock.", ".mock."} {
			if i := strings.LastIndex(base, marker); i >= 0 {
				return base[i:]
			}
		}
	}
	return ""
}

// testPlacement says where the sampled test files live: in a tests
// directory (`__tests__`, `test`, `tests`, `spec`) or beside the code.
func testPlacement(refFiles []string) string {
	dirs := map[string]bool{}
	for _, f := range refFiles {
		for _, seg := range strings.Split(path.Dir(strings.ReplaceAll(f, "\\", "/")), "/") {
			switch seg {
			case "__tests__", "test", "tests", "spec", "specs", "testdata":
				dirs[seg] = true
			}
		}
	}
	if len(dirs) == 0 {
		return "beside the code they test"
	}
	names := make([]string, 0, len(dirs))
	for d := range dirs {
		names = append(names, "`"+d+"`")
	}
	sort.Strings(names)
	return "in " + strings.Join(names, " and ") + " directories"
}

func baseNames(files []string) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, path.Base(strings.ReplaceAll(f, "\\", "/")))
	}
	return out
}
