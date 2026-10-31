package checks

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/testdetect"
)

// pytestCmd is the policy's pytest command taken apart: the invocation
// prefix ("python -m pytest", "pytest"), the flags verbatim and in order,
// and the positional test paths (the gate's scope).
type pytestCmd struct {
	python string   // interpreter for compileall / ruff / flake8
	prefix []string // ["python", "-m", "pytest"] or ["pytest"]
	flags  []string
	paths  []string
	// ignores are --ignore paths (the gate never collects below them).
	ignores []string
	// ignoreGlobs are --ignore-glob patterns (fnmatch over the path).
	ignoreGlobs []string
	cov         bool // --cov given
}

var (
	pytestShortBool  = set("q", "v", "x", "s", "l")
	pytestShortValue = set("k", "m", "p", "c", "o", "n", "W", "r")
	pytestLongValue  = set("--maxfail", "--tb", "--rootdir", "--basetemp", "--confcutdir", "--junitxml", "--junit-xml",
		"--cov-report", "--cov-config", "--cov-fail-under", "--durations", "--durations-min", "--deselect", "--ignore",
		"--ignore-glob", "--log-level", "--log-cli-level", "--log-file", "--log-file-level", "--log-format",
		"--log-date-format", "--capture", "--import-mode", "--override-ini", "--dist", "--timeout", "--pythonwarnings",
		"--color", "--code-highlight", "--randomly-seed", "--reruns", "--reruns-delay", "--html", "--numprocesses",
		"--maxprocesses", "--show-capture", "--report-log", "--tx")
	pytestLongBool = set("--verbose", "--quiet", "--exitfirst", "--ff", "--failed-first", "--nf", "--new-first",
		"--strict-markers", "--strict-config", "--strict", "--no-header", "--no-summary", "--disable-warnings",
		"--disable-pytest-warnings", "--showlocals", "--runxfail", "--cache-clear", "--no-cov", "--no-cov-on-fail",
		"--cov-append", "--cov-branch", "--full-trace", "--doctest-modules", "--continue-on-collection-errors",
		"--setup-show", "--trace-config", "--debug", "--pdb", "--randomly-dont-reorganize", "--forked",
		"--looponfail", "--lf-no-failures")
	// Selections that make a narrowed run select zero tests (pytest exits
	// 5) or depend on state another run writes: not narrowed.
	pytestNoNarrow = set("-k", "-m", "--keyword", "--deselect", "--lf", "--last-failed", "--sw", "--stepwise",
		"--sw-skip", "--stepwise-skip", "--cov-fail-under", "--doctest-modules", "-c", "--config-file",
		"--rootdir", "--confcutdir")
)

// parsePytest splits a plain pytest command. A command that is not pytest
// is a runner mismatch; a pytest command selection cannot narrow safely is
// an error (the full suite runs).
func parsePytest(cmd string) (pytestCmd, error) {
	if !testdetect.IsPytestCommand(cmd) || hasShellMeta(cmd) {
		return pytestCmd{}, runnerMismatch("the policy test command %q is not a plain pytest invocation; impact selection does not narrow it", cmd)
	}
	f := strings.Fields(cmd)
	var c pytestCmd
	switch binName(f[0]) {
	case "pytest", "py.test":
		c.python, c.prefix = "python", f[:1]
	default:
		c.python, c.prefix = f[0], f[:3]
	}
	args := f[len(c.prefix):]
	noNarrow := func(flag string) error {
		return fmt.Errorf("the policy's pytest option %s can make a narrowed run select no tests (or depends on another run's state); running the full suite", flag)
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		start := i
		if !strings.HasPrefix(a, "-") || a == "-" {
			if strings.Contains(a, "::") {
				return pytestCmd{}, fmt.Errorf("the policy selects test node %s; running the full suite", a)
			}
			c.paths = append(c.paths, strings.TrimSuffix(strings.TrimPrefix(filepath.ToSlash(a), "./"), "/"))
			continue
		}
		if strings.HasPrefix(a, "--") {
			name, val, hasEq := strings.Cut(a, "=")
			if pytestNoNarrow[name] {
				return pytestCmd{}, noNarrow(name)
			}
			switch {
			case name == "--cov":
				c.cov = true
				if !hasEq && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
					return pytestCmd{}, fmt.Errorf("the policy's pytest option --cov %s is ambiguous (a coverage source or a test path); running the full suite", args[i+1])
				}
			case pytestLongValue[name]:
				if !hasEq {
					if i+1 >= len(args) {
						return pytestCmd{}, fmt.Errorf("the policy's pytest option %s has no value", a)
					}
					i++
					val = args[i]
				}
				if err := c.noteOption(name, val, "the policy test command"); err != nil {
					return pytestCmd{}, err
				}
			case pytestLongBool[name] && !hasEq:
			case hasEq:
				// an unknown --name=value cannot swallow a path
			default:
				return pytestCmd{}, fmt.Errorf("the policy test command %q has option %s, which impact selection cannot place; running the full suite", cmd, a)
			}
			c.flags = append(c.flags, args[start:i+1]...)
			continue
		}
		// Short option cluster: -qx, -n4, -n 4, -ra, -k expr.
		group := []string{a}
		for j := 1; j < len(a); j++ {
			ch := string(a[j])
			if pytestShortBool[ch] {
				continue
			}
			if pytestShortValue[ch] {
				if pytestNoNarrow["-"+ch] {
					return pytestCmd{}, noNarrow("-" + ch)
				}
				val := a[j+1:]
				if j == len(a)-1 { // value is the next word
					if i+1 >= len(args) {
						return pytestCmd{}, fmt.Errorf("the policy's pytest option %s has no value", a)
					}
					i++
					group = append(group, args[i])
					val = args[i]
				}
				if err := c.noteOption("-"+ch, val, "the policy test command"); err != nil {
					return pytestCmd{}, err
				}
				break
			}
			return pytestCmd{}, fmt.Errorf("the policy test command %q has option %s, which impact selection cannot place; running the full suite", cmd, a)
		}
		c.flags = append(c.flags, group...)
	}
	return c, nil
}

// checkAddopts applies the no-narrowing rules to the configuration's
// addopts (they apply to the gate and to every narrowed run alike) and
// collects --cov and --ignore from them.
func (c *pytestCmd) checkAddopts(source string, addopts []string) error {
	for i := 0; i < len(addopts); i++ {
		a := addopts[i]
		name, val, hasEq := strings.Cut(a, "=")
		if pytestNoNarrow[name] || (len(a) >= 2 && a[0] == '-' && a[1] != '-' && pytestNoNarrow["-"+string(a[1])]) {
			return fmt.Errorf("addopts in %s has %s, which can make a narrowed run select no tests; running the full suite", source, name)
		}
		switch {
		case name == "--cov":
			c.cov = true
			continue
		case name == "--ignore" || name == "--ignore-glob" || name == "--override-ini":
			if !hasEq && i+1 < len(addopts) {
				i++
				val = addopts[i]
			}
		case strings.HasPrefix(a, "-o"):
			name, val = "-o", a[2:]
			if val == "" && i+1 < len(addopts) {
				i++
				val = addopts[i]
			}
		default:
			continue
		}
		if err := c.noteOption(name, val, "addopts in "+source); err != nil {
			return err
		}
	}
	return nil
}

// pytestOverrideBlocks are ini keys that change what pytest collects; an
// -o/--override-ini of one of them is not modelled, so it does not narrow.
var pytestOverrideBlocks = set("python_files", "python_classes", "python_functions", "testpaths", "norecursedirs",
	"addopts", "confcutdir", "rootdir", "collect_imported_tests", "consider_namespace_packages")

// noteOption records an option that bounds collection (--ignore,
// --ignore-glob) and rejects an -o override of a collection setting.
func (c *pytestCmd) noteOption(name, val, where string) error {
	clean := func(v string) string { return strings.TrimSuffix(strings.TrimPrefix(filepath.ToSlash(v), "./"), "/") }
	switch name {
	case "--ignore":
		c.ignores = append(c.ignores, clean(val))
	case "--ignore-glob":
		c.ignoreGlobs = append(c.ignoreGlobs, clean(val))
	case "-o", "--override-ini":
		key, _, _ := strings.Cut(val, "=")
		if key = strings.TrimSpace(key); pytestOverrideBlocks[key] {
			return fmt.Errorf("%s overrides %s (-o %s), which changes what pytest collects; running the full suite", where, key, val)
		}
	}
	return nil
}

// pytestConfig is what selection needs from the pytest configuration
// (pytest.ini, pyproject.toml [tool.pytest.ini_options], tox.ini [pytest],
// setup.cfg [tool:pytest], in pytest's precedence order).
type pytestConfig struct {
	source      string
	testpaths   []string
	pythonFiles []string
	addopts     []string
	norecurse   []string
	pythonpath  []string
	// python_functions / python_classes name patterns (prefixes or globs).
	pythonFunctions []string
	pythonClasses   []string
}

var tomlString = regexp.MustCompile(`"([^"]*)"|'([^']*)'`)

func readPytestConfig(root string) pytestConfig {
	type src struct{ file, section string }
	for _, s := range []src{{"pytest.ini", "pytest"}, {"pyproject.toml", "tool.pytest.ini_options"}, {"tox.ini", "pytest"}, {"setup.cfg", "tool:pytest"}} {
		vals, ok := readSection(filepath.Join(root, s.file), s.section)
		if !ok {
			continue
		}
		toml := strings.HasSuffix(s.file, ".toml")
		list := func(k string) []string {
			v, ok := vals[k]
			if !ok {
				return nil
			}
			if toml {
				var out []string
				for _, m := range tomlString.FindAllStringSubmatch(v, -1) {
					out = append(out, m[1]+m[2])
				}
				if len(out) == 1 && (k == "addopts") {
					return strings.Fields(out[0])
				}
				return out
			}
			return strings.Fields(v)
		}
		cfg := pytestConfig{source: s.file}
		cfg.testpaths = list("testpaths")
		cfg.pythonFiles = list("python_files")
		cfg.addopts = list("addopts")
		cfg.norecurse = list("norecursedirs")
		cfg.pythonpath = list("pythonpath")
		cfg.pythonFunctions = list("python_functions")
		cfg.pythonClasses = list("python_classes")
		return cfg
	}
	return pytestConfig{}
}

// readSection reads "key = value" pairs (with indented continuation lines)
// from an ini [section] or a TOML [table] with multi-line arrays.
func readSection(file, section string) (map[string]string, bool) {
	fh, err := os.Open(file)
	if err != nil {
		return nil, false
	}
	defer fh.Close()
	vals := map[string]string{}
	in, found := false, false
	key := ""
	depth := 0 // open TOML brackets of the current value
	sc := bufio.NewScanner(fh)
	for sc.Scan() {
		line := sc.Text()
		trim := strings.TrimSpace(line)
		if depth == 0 && strings.HasPrefix(trim, "[") && strings.HasSuffix(trim, "]") && !strings.Contains(trim, "=") {
			name := strings.TrimSpace(strings.Trim(trim, "[]"))
			in = name == section
			found = found || in
			key = ""
			continue
		}
		if !in || trim == "" || strings.HasPrefix(trim, "#") || strings.HasPrefix(trim, ";") {
			continue
		}
		if depth > 0 || (key != "" && (line[0] == ' ' || line[0] == '\t') && !strings.Contains(trim, "=")) {
			vals[key] += " " + trim
			depth += strings.Count(trim, "[") - strings.Count(trim, "]")
			continue
		}
		k, v, ok := strings.Cut(trim, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(k)
		vals[key] = strings.TrimSpace(v)
		depth = strings.Count(v, "[") - strings.Count(v, "]")
		if depth < 0 {
			depth = 0
		}
	}
	return vals, found
}

// coverageFailUnder reports a configured coverage threshold (which a
// narrowed run would miss).
func coverageFailUnder(root string) string {
	for _, s := range []struct{ file, section string }{
		{".coveragerc", "report"}, {"pyproject.toml", "tool.coverage.report"}, {"setup.cfg", "coverage:report"}, {"tox.ini", "coverage:report"},
	} {
		if vals, ok := readSection(filepath.Join(root, s.file), s.section); ok {
			if _, has := vals["fail_under"]; has {
				return s.file
			}
		}
	}
	return ""
}

// ruffConfigured / flake8Configured report a configured linter.
func ruffConfigured(root string) bool {
	for _, f := range []string{"ruff.toml", ".ruff.toml"} {
		if _, err := os.Stat(filepath.Join(root, f)); err == nil {
			return true
		}
	}
	_, ok := readSectionPrefix(filepath.Join(root, "pyproject.toml"), "tool.ruff")
	return ok
}

func flake8Configured(root string) bool {
	if _, err := os.Stat(filepath.Join(root, ".flake8")); err == nil {
		return true
	}
	for _, f := range []string{"setup.cfg", "tox.ini"} {
		if _, ok := readSection(filepath.Join(root, f), "flake8"); ok {
			return true
		}
	}
	return false
}

// readSectionPrefix reports whether a [prefix] or [prefix.*] table exists.
func readSectionPrefix(file, prefix string) (string, bool) {
	b, err := os.ReadFile(file)
	if err != nil {
		return "", false
	}
	for _, l := range strings.Split(string(b), "\n") {
		t := strings.TrimSpace(l)
		if t == "["+prefix+"]" || strings.HasPrefix(t, "["+prefix+".") {
			return t, true
		}
	}
	return "", false
}
