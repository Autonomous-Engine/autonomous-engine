package checks

import (
	"fmt"
	"regexp"
	"strings"
)

// goTestCmd is the policy's `go test` command taken apart, so fast targets
// can keep every non-package argument and stay within its package patterns:
// the fast loop must never be stricter than the gate (a final-attempt fast
// failure aborts the task).
type goTestCmd struct {
	// flags are the non-package arguments before the packages, verbatim
	// and in order (-short, -tags x, -race, -timeout 10m, -count=1, ...).
	flags []string
	// trailing is -args and everything after it (placed after packages).
	trailing []string
	// patterns are the package patterns ("." when none: go test then tests
	// the current directory only).
	patterns []string
	// buildFlags / vetFlags are the subsets of flags go build / go vet
	// accept, so their package set and build configuration match.
	buildFlags []string
	vetFlags   []string
}

// goTestValueFlags take a value as the next argument when written without
// "=". Every other recognised flag is boolean.
var goTestValueFlags = map[string]bool{
	"run": true, "skip": true, "timeout": true, "tags": true, "count": true, "p": true,
	"parallel": true, "bench": true, "benchtime": true, "cpu": true, "coverprofile": true,
	"covermode": true, "coverpkg": true, "cpuprofile": true, "memprofile": true,
	"memprofilerate": true, "blockprofile": true, "blockprofilerate": true,
	"mutexprofile": true, "mutexprofilefraction": true, "trace": true, "o": true,
	"exec": true, "ldflags": true, "gcflags": true, "asmflags": true, "mod": true,
	"modfile": true, "shuffle": true, "outputdir": true, "vet": true, "fuzz": true,
	"fuzztime": true, "fuzzminimizetime": true, "C": true, "pgo": true, "overlay": true,
	"toolexec": true, "compiler": true, "installsuffix": true, "buildmode": true,
	"list": true, "gccgoflags": true,
}

var goTestBoolFlags = map[string]bool{
	"short": true, "race": true, "v": true, "failfast": true, "cover": true, "json": true,
	"x": true, "n": true, "a": true, "trimpath": true, "benchmem": true, "msan": true,
	"asan": true, "i": true, "work": true, "linkshared": true, "fullpath": true,
	"buildvcs": true,
}

// goBuildFlagNames are the test flags that also configure go build.
var goBuildFlagNames = map[string]bool{
	"tags": true, "mod": true, "modfile": true, "race": true, "gcflags": true, "ldflags": true,
	"asmflags": true, "trimpath": true, "msan": true, "asan": true, "overlay": true,
	"pgo": true, "buildvcs": true, "toolexec": true, "compiler": true,
}

// goVetFlagNames are the test flags go vet accepts (package resolution).
var goVetFlagNames = map[string]bool{"tags": true, "mod": true, "modfile": true, "overlay": true, "toolexec": true}

// parseGoTest splits a plain `go test` command. An unrecognised flag
// written without "=" makes the split ambiguous (is the next word its value
// or a package?), so it is an error and selection falls back to the full
// suite.
func parseGoTest(cmd string) (goTestCmd, error) {
	f := strings.Fields(cmd)
	if len(f) < 2 || f[0] != "go" || f[1] != "test" || strings.ContainsAny(cmd, ";|&`$<>\n") {
		return goTestCmd{}, runnerMismatch("the policy test command %q is not a plain go test invocation; impact selection does not narrow a custom command", cmd)
	}
	var c goTestCmd
	args := f[2:]
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-args" || a == "--args" {
			c.trailing = append([]string{}, args[i:]...)
			break
		}
		if !strings.HasPrefix(a, "-") {
			c.patterns = append(c.patterns, a)
			continue
		}
		name := strings.TrimLeft(a, "-")
		group := []string{a}
		if eq := strings.IndexByte(name, '='); eq >= 0 {
			name = name[:eq]
		} else {
			switch {
			case goTestValueFlags[name]:
				if i+1 >= len(args) {
					return goTestCmd{}, fmt.Errorf("the policy test command %q: flag %s has no value", cmd, a)
				}
				i++
				group = append(group, args[i])
			case goTestBoolFlags[name]:
			default:
				return goTestCmd{}, fmt.Errorf("the policy test command %q has flag %s, which impact selection cannot place; running the full suite", cmd, a)
			}
		}
		c.flags = append(c.flags, group...)
		if goBuildFlagNames[name] {
			c.buildFlags = append(c.buildFlags, group...)
		}
		if goVetFlagNames[name] {
			c.vetFlags = append(c.vetFlags, group...)
		}
	}
	if len(c.patterns) == 0 {
		c.patterns = []string{"."}
	}
	return c, nil
}

// hasFlag reports whether name is among the flags.
func (c goTestCmd) hasFlag(name string) bool {
	for _, a := range c.flags {
		n := strings.TrimLeft(a, "-")
		if i := strings.IndexByte(n, '='); i >= 0 {
			n = n[:i]
		}
		if strings.HasPrefix(a, "-") && n == name {
			return true
		}
	}
	return false
}

// command renders `go <verb> <flags> <dirs> <trailing>`.
func goCommand(verb string, flags, dirs, trailing []string) string {
	parts := append([]string{"go", verb}, flags...)
	parts = append(parts, dirs...)
	parts = append(parts, trailing...)
	return strings.Join(parts, " ")
}

// patternMatcher matches package directories ("./x/y", ".") against the
// policy's package patterns.
type patternMatcher []*regexp.Regexp

// newPatternMatcher compiles the patterns relative to the module root. A
// pattern that is neither relative ("./...", "./internal/...", ".") nor
// inside the module path ("example.com/m/...") cannot be matched to
// directories (std, all, absolute paths, other modules): an error, so
// selection falls back to the full suite.
func newPatternMatcher(patterns []string, modPath string) (patternMatcher, error) {
	var m patternMatcher
	for _, p := range patterns {
		rel := ""
		switch {
		case p == "." || p == "./":
			rel = "."
		case strings.HasPrefix(p, "./"):
			rel = strings.TrimPrefix(p, "./")
		case p == modPath:
			rel = "."
		case strings.HasPrefix(p, modPath+"/"):
			rel = strings.TrimPrefix(p, modPath+"/")
		default:
			return nil, fmt.Errorf("the policy's package pattern %q cannot be matched to directories of module %s", p, modPath)
		}
		rel = strings.TrimSuffix(rel, "/")
		if strings.Contains(rel, "..") && !strings.Contains(rel, "...") {
			return nil, fmt.Errorf("the policy's package pattern %q leaves the module", p)
		}
		var expr string
		if rel == "..." {
			expr = ".*"
		} else {
			expr = regexp.QuoteMeta(rel)
			expr = strings.ReplaceAll(expr, `\.\.\.`, `.*`)
			if strings.HasSuffix(rel, "/...") {
				// "x/..." also matches x itself.
				expr = strings.TrimSuffix(expr, "/.*") + "(/.*)?"
			}
		}
		re, err := regexp.Compile("^" + expr + "$")
		if err != nil {
			return nil, fmt.Errorf("the policy's package pattern %q: %v", p, err)
		}
		m = append(m, re)
	}
	return m, nil
}

// matches reports whether the package directory ("./x" or ".") is selected.
func (m patternMatcher) matches(dir string) bool {
	rel := strings.TrimPrefix(dir, "./")
	if dir == "." {
		rel = "."
	}
	for _, re := range m {
		if re.MatchString(rel) {
			return true
		}
	}
	return false
}
