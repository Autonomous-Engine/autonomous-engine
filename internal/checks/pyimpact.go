package checks

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/testdetect"
)

// PythonImpact selects checks for a Python repository (pyproject.toml,
// setup.py, setup.cfg, requirements.txt or a pytest marker at the root).
//
// The import graph is built from source (import / from … import lines,
// relative imports resolved against the importing file's package) over
// every .py file outside hidden, virtualenv and pytest-norecurse
// directories. Modules are named from the source roots: the repository
// root, src/ and the pytest `pythonpath` setting. A changed module selects
// the test files that import it, directly or transitively, plus test
// files named after it (test_<module>.py, <module>_test.py); a changed or
// affected conftest.py selects its directory. Test files are those the
// gate's pytest would collect: python_files patterns (default test_*.py,
// *_test.py) within the gate's paths (its arguments, else testpaths),
// outside --ignore paths, and containing a test function or class — an
// explicit file with no test makes pytest exit 5, which the gate would
// not.
//
// Every test target is the gate's pytest invocation with its options
// verbatim and the test files in place of its paths. `python -m
// compileall -q <changed files>` (build) and ruff or flake8 when configured
// (lint) are advisory unless the policy requires build / lint.
type PythonImpact struct{}

// MaxPyTestFiles caps per-file test targets; above it the gate's own test
// paths run.
const MaxPyTestFiles = 50

func (PythonImpact) Language() string { return testdetect.LanguagePython }

func (PythonImpact) Applies(repoTree string) bool {
	return repoTree != "" && testdetect.HasPythonProject(repoTree)
}

// Changes to these root files can affect every test (dependencies,
// interpreter and pytest configuration).
var pyRootConfig = regexp.MustCompile(`^(pyproject\.toml|setup\.py|setup\.cfg|requirements[^/]*\.txt|constraints[^/]*\.txt|Pipfile(\.lock)?|poetry\.lock|uv\.lock|pdm\.lock|pytest\.ini|tox\.ini|\.coveragerc|noxfile\.py|\.python-version)$`)

var defaultNorecurse = []string{"*.egg", ".*", "_darcs", "build", "CVS", "dist", "node_modules", "venv", "{arch}", "__pycache__"}

var (
	pyDefLine   = regexp.MustCompile(`^(\s*)(?:async\s+)?def\s+(\w+)`)
	pyClassLine = regexp.MustCompile(`^class\s+(\w+)\s*(\([^)]*\))?`)
)

// pyHasTest reports whether src defines a test pytest collects under the
// configured python_functions / python_classes (defaults "test" and
// "Test"): a matching top-level function, or a matching method of a
// matching class (or of a unittest TestCase subclass). An explicit file
// without one makes pytest exit 5, which the gate would not.
func pyHasTest(src []byte, cfg pytestConfig) bool {
	funcs, classes := cfg.pythonFunctions, cfg.pythonClasses
	if len(funcs) == 0 {
		funcs = []string{"test"}
	}
	if len(classes) == 0 {
		classes = []string{"Test"}
	}
	classOK := false
	for _, line := range strings.Split(string(src), "\n") {
		if m := pyClassLine.FindStringSubmatch(line); m != nil {
			classOK = pyNameMatches(m[1], classes) || strings.Contains(m[2], "TestCase")
			continue
		}
		if m := pyDefLine.FindStringSubmatch(line); m != nil && pyNameMatches(m[2], funcs) {
			if m[1] == "" || classOK {
				return true
			}
			continue
		}
		if t := strings.TrimSpace(line); t != "" && line[0] != ' ' && line[0] != '\t' && !strings.HasPrefix(t, "#") && !strings.HasPrefix(t, "@") {
			classOK = false // left the class body
		}
	}
	return false
}

// pyNameMatches applies pytest's name rule: a pattern with glob
// characters must match the whole name, otherwise it is a prefix.
func pyNameMatches(name string, patterns []string) bool {
	for _, p := range patterns {
		if strings.ContainsAny(p, "*?[") {
			if fnmatchRegexp(p).MatchString(name) {
				return true
			}
		} else if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// fnmatchRegexp compiles a Python fnmatch pattern: * and ? also match
// "/", [...] is a class ([!...] negated).
func fnmatchRegexp(g string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(g); i++ {
		switch c := g[i]; c {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		case '[':
			j := strings.IndexByte(g[i+1:], ']')
			if j < 0 {
				b.WriteString(`\[`)
				continue
			}
			class := g[i+1 : i+1+j]
			if strings.HasPrefix(class, "!") {
				class = "^" + class[1:]
			}
			b.WriteString("[" + strings.ReplaceAll(class, `\`, `\\`) + "]")
			i += j + 1
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return regexp.MustCompile(`^\b$`) // matches nothing
	}
	return re
}

var (
	collectIgnoreRef = regexp.MustCompile(`\bcollect_ignore(?:_glob)?\b`)
	// The list literal must end the statement: `[...] + extra()` or
	// `[...] if X else []` is not a literal and falls back.
	collectIgnoreLiteral = regexp.MustCompile(`(?m)^(collect_ignore(?:_glob)?)\s*(?:=|\+=)\s*\[([^\]]*)\][ \t]*(?:#.*)?\r?$`)
	pyStringLiteral      = regexp.MustCompile(`"([^"\\]*)"|'([^'\\]*)'`)
	pyOnlyLiterals       = regexp.MustCompile(`^(?:\s*(?:"[^"\\]*"|'[^'\\]*')\s*,?)*\s*$`)
)

// conftestIgnores reads collect_ignore / collect_ignore_glob from every
// conftest.py: pytest skips the listed paths (relative to the conftest's
// directory) while collecting, but an explicit file argument would bypass
// that, so selection excludes them. Only module-level list literals are
// understood; any other use of the names (computed, appended) is an error
// and selection falls back to the full suite.
func conftestIgnores(root string, repo *pyRepo) (ignores, globs []string, err error) {
	for _, f := range repo.py {
		if path.Base(f) != "conftest.py" || !repo.files[f] {
			continue
		}
		b, rerr := os.ReadFile(filepath.Join(root, filepath.FromSlash(f)))
		if rerr != nil {
			continue
		}
		src := string(b)
		refs := len(collectIgnoreRef.FindAllStringIndex(src, -1))
		if refs == 0 {
			continue
		}
		lits := collectIgnoreLiteral.FindAllStringSubmatch(src, -1)
		understood := 0
		for _, m := range lits {
			if !pyOnlyLiterals.MatchString(m[2]) {
				continue
			}
			understood++
			for _, s := range pyStringLiteral.FindAllStringSubmatch(m[2], -1) {
				p := path.Clean(path.Join(path.Dir(f), filepath.ToSlash(s[1]+s[2])))
				if m[1] == "collect_ignore_glob" {
					globs = append(globs, p)
				} else {
					ignores = append(ignores, p)
				}
			}
		}
		if understood != refs {
			return nil, nil, fmt.Errorf("%s computes collect_ignore/collect_ignore_glob in a way selection cannot read; running the full suite", f)
		}
	}
	return ignores, globs, nil
}

// pycachePrefix keeps compileall's bytecode out of the worktree
// (PYTHONPYCACHEPREFIX, Python 3.8+), so a fast build check never leaves
// untracked __pycache__ directories behind.
var pycachePrefix = filepath.ToSlash(filepath.Join(os.TempDir(), "ae-checks-pycache"))

type pyRepo struct {
	files     map[string]bool // every non-skipped file
	py        []string        // .py files, sorted (plus deleted changed ones)
	roots     []string        // source roots, longest first
	modules   map[string][]string
	importers map[string][]string
}

func loadPyRepo(ctx context.Context, root string, cfg pytestConfig, changed []string) (*pyRepo, error) {
	var skipRes []*regexp.Regexp
	for _, g := range append(append([]string{}, defaultNorecurse...), cfg.norecurse...) {
		if re, err := globRegexp(g); err == nil {
			skipRes = append(skipRes, re)
		}
	}
	r := &pyRepo{files: map[string]bool{}, modules: map[string][]string{}, importers: map[string][]string{}}
	err := walkRepo(root, func(rel, name string) bool {
		for _, re := range skipRes {
			if re.MatchString(name) {
				return true
			}
		}
		_, venv := os.Stat(filepath.Join(root, filepath.FromSlash(rel), "pyvenv.cfg"))
		return venv == nil
	}, func(rel string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		r.files[rel] = true
		if strings.HasSuffix(rel, ".py") {
			r.py = append(r.py, rel)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("import graph: %v", err)
	}
	present := map[string]bool{}
	for _, f := range r.py {
		present[f] = true
	}
	for _, f := range changed {
		if strings.HasSuffix(f, ".py") && !present[f] {
			if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(f))); err != nil {
				r.py = append(r.py, f) // deleted: importers still resolve to it
			}
		}
	}
	sort.Strings(r.py)

	r.roots = []string{"."}
	for _, d := range append([]string{"src"}, cfg.pythonpath...) {
		d = strings.TrimSuffix(strings.TrimPrefix(filepath.ToSlash(d), "./"), "/")
		if d == "" || d == "." {
			continue
		}
		if st, err := os.Stat(filepath.Join(root, filepath.FromSlash(d))); err == nil && st.IsDir() {
			r.roots = append(r.roots, d)
		}
	}
	sort.Slice(r.roots, func(i, j int) bool {
		return len(r.roots[i]) > len(r.roots[j]) || len(r.roots[i]) == len(r.roots[j]) && r.roots[i] < r.roots[j]
	})
	for _, f := range r.py {
		for _, rt := range r.roots {
			if within(f, rt) {
				if m := pyModuleName(relTo(f, rt)); m != "" {
					r.modules[m] = append(r.modules[m], f)
				}
			}
		}
	}

	for _, f := range r.py {
		src, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f)))
		if err != nil {
			continue // deleted
		}
		for _, dep := range r.resolveImports(f, string(src)) {
			if dep != f {
				r.importers[dep] = appendUnique(r.importers[dep], f)
			}
		}
	}
	return r, nil
}

func appendUnique(list []string, s string) []string {
	for _, x := range list {
		if x == s {
			return list
		}
	}
	return append(list, s)
}

// pyModuleName maps a root-relative .py path to its dotted module name.
func pyModuleName(rel string) string {
	m := strings.TrimSuffix(rel, ".py")
	m = strings.TrimSuffix(m, "/__init__")
	if m == "__init__" {
		return ""
	}
	for _, seg := range strings.Split(m, "/") {
		if seg == "" || strings.ContainsAny(seg, ".- ") {
			return ""
		}
	}
	return strings.ReplaceAll(m, "/", ".")
}

var (
	pyImportStmt = regexp.MustCompile(`^\s*import\s+(.+)$`)
	pyFromStmt   = regexp.MustCompile(`^\s*from\s+(\.*)([\w.]*)\s+import\s+(.+)$`)
)

// logicalLines joins backslash continuations and parenthesised import
// lists, and drops comments.
func logicalLines(src string) []string {
	var out []string
	var cur strings.Builder
	open := 0
	for _, line := range strings.Split(src, "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimRight(line, " \t\r")
		cont := strings.HasSuffix(line, "\\")
		line = strings.TrimSuffix(line, "\\")
		cur.WriteString(line)
		cur.WriteString(" ")
		open += strings.Count(line, "(") - strings.Count(line, ")")
		if cont || open > 0 {
			continue
		}
		open = 0
		out = append(out, cur.String())
		cur.Reset()
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// resolveImports returns the repository files f's imports load: the
// imported module, every parent package's __init__.py, and for `from X
// import a` also the submodule X.a when it exists.
func (r *pyRepo) resolveImports(f, src string) []string {
	var deps []string
	// pytest's default (prepend) import mode puts the directory of a test
	// module that is not inside a package on sys.path, so its siblings
	// import by bare name (tests/helpers.py as `helpers`).
	dir := path.Dir(f)
	sibling := !r.files[path.Join(dir, "__init__.py")]
	addModule := func(m string) {
		parts := strings.Split(m, ".")
		for i := 1; i <= len(parts); i++ {
			deps = append(deps, r.modules[strings.Join(parts[:i], ".")]...)
			if sibling {
				p := path.Join(dir, strings.Join(parts[:i], "/"))
				for _, c := range []string{p + ".py", p + "/__init__.py"} {
					if r.files[c] {
						deps = append(deps, c)
					}
				}
			}
		}
	}
	for _, line := range logicalLines(src) {
		if m := pyFromStmt.FindStringSubmatch(line); m != nil {
			base := m[2]
			if level := len(m[1]); level > 0 {
				var parts []string
				if pkg := r.packageOf(f); pkg != "" {
					parts = strings.Split(pkg, ".")
				}
				if level-1 > len(parts) {
					continue
				}
				parts = parts[:len(parts)-(level-1)]
				if base != "" {
					parts = append(parts, base)
				}
				base = strings.Join(parts, ".")
			}
			if base != "" {
				addModule(base)
			}
			names := strings.Trim(strings.TrimSpace(m[3]), "()")
			for _, n := range strings.Split(names, ",") {
				n = strings.TrimSpace(strings.SplitN(strings.TrimSpace(n), " ", 2)[0])
				if n == "" || n == "*" {
					continue
				}
				sub := n
				if base != "" {
					sub = base + "." + n
				}
				deps = append(deps, r.modules[sub]...)
			}
			continue
		}
		if m := pyImportStmt.FindStringSubmatch(line); m != nil {
			for _, n := range strings.Split(m[1], ",") {
				n = strings.TrimSpace(strings.SplitN(strings.TrimSpace(n), " ", 2)[0])
				if n != "" {
					addModule(n)
				}
			}
		}
	}
	return deps
}

// packageOf is the dotted package f belongs to under its longest source
// root ("" at the root).
func (r *pyRepo) packageOf(f string) string {
	for _, rt := range r.roots {
		if within(f, rt) {
			rel := relTo(f, rt)
			if path.Base(rel) == "__init__.py" {
				return pyModuleName(rel)
			}
			d := path.Dir(rel)
			if d == "." {
				return ""
			}
			return strings.ReplaceAll(d, "/", ".")
		}
	}
	return ""
}

// Targets implements Impact.
func (PythonImpact) Targets(ctx context.Context, repoTree string, changed []string, p Policy) ([]Target, error) {
	gate, gateErr := parsePytest(p.FullTestCommand)
	var rm *runnerMismatchError
	mismatch := errors.As(gateErr, &rm)
	if gateErr != nil && !mismatch {
		return nil, gateErr
	}
	python := "python"
	if !mismatch {
		python = gate.python
	}
	cfg := readPytestConfig(repoTree)
	for _, f := range changed {
		if pyRootConfig.MatchString(f) && !mismatch {
			return nil, fmt.Errorf("%s changed: a dependency or configuration change can affect every test", f)
		}
	}
	repo, err := loadPyRepo(ctx, repoTree, cfg, changed)
	if err != nil {
		if mismatch {
			return nil, gateErr
		}
		return nil, err
	}
	nonTest := pyNonTestTargets(repoTree, repo, python, changed, p)
	if mismatch {
		return nonTest, gateErr
	}
	if err := gate.checkAddopts(cfg.source, cfg.addopts); err != nil {
		return nil, err
	}
	if gate.cov {
		if src := coverageFailUnder(repoTree); src != "" {
			return nil, fmt.Errorf("--cov with a coverage threshold (fail_under in %s): a narrowed run would miss it; running the full suite", src)
		}
	}
	tests, err := pyTestTargets(repoTree, repo, gate, cfg, changed)
	if err != nil {
		return nil, err
	}
	return append(tests, nonTest...), nil
}

func pyTestTargets(root string, repo *pyRepo, gate pytestCmd, cfg pytestConfig, changed []string) ([]Target, error) {
	// conftest collect_ignore / collect_ignore_glob: explicit paths bypass
	// them, so the listed paths are excluded here.
	ignores, ignoreGlobs, err := conftestIgnores(root, repo)
	if err != nil {
		return nil, err
	}
	ignores = append(ignores, gate.ignores...)
	var globRes []*regexp.Regexp
	for _, g := range append(ignoreGlobs, gate.ignoreGlobs...) {
		globRes = append(globRes, fnmatchRegexp(g))
	}
	scope := gate.paths
	if len(scope) == 0 {
		scope = cfg.testpaths
	}
	if len(scope) == 0 {
		scope = []string{"."}
	}
	patterns := cfg.pythonFiles
	if len(patterns) == 0 {
		patterns = []string{"test_*.py", "*_test.py"}
	}
	var fileRes []*regexp.Regexp
	for _, pat := range patterns {
		if re, err := globRegexp(pat); err == nil {
			fileRes = append(fileRes, re)
		}
	}
	inScope := func(f string) bool {
		for _, ig := range ignores {
			if within(f, ig) {
				return false
			}
		}
		for _, re := range globRes {
			// pytest matches the glob against the path and, while
			// recursing, each directory on the way.
			for p := f; p != "." && p != "/"; p = path.Dir(p) {
				if re.MatchString(p) {
					return false
				}
			}
		}
		for _, s := range scope {
			if s == "." || f == s || within(f, s) {
				return true
			}
		}
		return false
	}
	hasTest := map[string]bool{}
	isTest := func(f string) bool {
		if v, ok := hasTest[f]; ok {
			return v
		}
		ok := false
		if repo.files[f] && strings.HasSuffix(f, ".py") && inScope(f) {
			for _, re := range fileRes {
				ok = ok || re.MatchString(path.Base(f))
			}
			if ok {
				b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f)))
				ok = err == nil && pyHasTest(b, cfg)
			}
		}
		hasTest[f] = ok
		return ok
	}

	affected := propagate(changed, func(n string) []string { return repo.importers[n] })
	reasons := map[string]string{} // test file or dir -> reason
	dirs := map[string]bool{}
	for _, f := range sortedKeys(affected) {
		ri := affected[f]
		switch {
		case path.Base(f) == "conftest.py":
			d := path.Dir(f)
			why := "changed " + f
			if ri.root != f {
				why = fmt.Sprintf("%s is affected by changed file %s", f, ri.root)
			}
			reasons[d] = why + ", which applies to every test below " + d
			dirs[d] = true
		case isTest(f):
			switch {
			case ri.root == f:
				reasons[f] = "changed test file " + f
			case ri.via == "":
				reasons[f] = fmt.Sprintf("test %s imports changed module %s", f, ri.root)
			default:
				reasons[f] = fmt.Sprintf("test %s is affected by changed module %s (via %s)", f, ri.root, ri.via)
			}
		}
	}
	// Tests named after a changed module (test_<name>.py, <name>_test.py).
	stems := map[string]string{}
	for _, f := range changed {
		b := path.Base(f)
		if !strings.HasSuffix(b, ".py") || b == "__init__.py" || b == "conftest.py" || isTest(f) {
			continue
		}
		stem := strings.TrimSuffix(b, ".py")
		if _, ok := stems[stem]; !ok {
			stems[stem] = f
		}
	}
	if len(stems) > 0 {
		for _, f := range repo.py {
			if _, done := reasons[f]; done || !isTest(f) {
				continue
			}
			b := strings.TrimSuffix(path.Base(f), ".py")
			for _, cand := range []string{strings.TrimPrefix(b, "test_"), strings.TrimSuffix(b, "_test")} {
				if src, ok := stems[cand]; ok && cand != b {
					reasons[f] = fmt.Sprintf("test %s is named after changed module %s", f, src)
					break
				}
			}
		}
	}

	prefix := strings.Join(append(append([]string{}, gate.prefix...), gate.flags...), " ")
	target := func(arg, reason string) Target {
		q := arg
		if strings.HasPrefix(q, "-") {
			q = "./" + q // not an option
		}
		q = shQuote(q)
		return Target{Kind: evidence.CheckKindTest, Command: prefix + " " + q, Scope: []string{arg}, Reason: reason, batchPrefix: prefix, batchArg: q}
	}
	// conftest directories: within the scope, and only when tests exist
	// below them; a directory that contains the whole scope runs the scope.
	var out []Target
	var dirArgs []string
	for _, d := range sortedKeys(dirs) {
		under := false
		for _, s := range scope {
			under = under || s == "." || within(d, s)
		}
		var args []string
		if under {
			args = []string{d}
		} else {
			for _, s := range scope {
				if within(s, d) {
					args = append(args, s)
				}
			}
		}
		for _, a := range args {
			n := 0
			for _, f := range repo.py {
				if within(f, a) && isTest(f) {
					n++
				}
			}
			if n > 0 && !containsDir(dirArgs, a) {
				dirArgs = append(dirArgs, a)
				out = append(out, target(a, reasons[d]))
			}
		}
	}
	var files []string
	for _, f := range sortedKeys(reasons) {
		if dirs[f] || !isTest(f) || containsDir(dirArgs, f) {
			continue
		}
		files = append(files, f)
	}
	if len(files) > MaxPyTestFiles {
		var quoted []string
		for _, s := range scope {
			quoted = append(quoted, shQuote(s))
		}
		return append(out, Target{
			Kind: evidence.CheckKindTest, Command: prefix + " " + strings.Join(quoted, " "), Scope: scope,
			Reason: fmt.Sprintf("%d affected test files (more than %d): the gate's test paths", len(files), MaxPyTestFiles),
		}), nil
	}
	for _, f := range files {
		out = append(out, target(f, reasons[f]))
	}
	return out, nil
}

// containsDir reports whether f is one of dirs or below one.
func containsDir(dirs []string, f string) bool {
	for _, d := range dirs {
		if within(f, d) {
			return true
		}
	}
	return false
}

// pyNonTestTargets: compileall over the changed Python files (build) and
// the configured linter over them, advisory unless required.
func pyNonTestTargets(root string, repo *pyRepo, python string, changed []string, p Policy) []Target {
	var files []string
	for _, f := range changed {
		if strings.HasSuffix(f, ".py") && repo.files[f] {
			files = append(files, f)
		}
	}
	if len(files) == 0 {
		return nil
	}
	var quoted []string
	for _, f := range files {
		quoted = append(quoted, shQuote(f))
	}
	list := strings.Join(quoted, " ")
	out := []Target{{
		Kind: evidence.CheckKindBuild, Command: "PYTHONPYCACHEPREFIX=" + shQuote(pycachePrefix) + " " + python + " -m compileall -q " + list, Scope: files,
		Reason: "changed Python file(s) must still compile", Advisory: !p.requires(evidence.CheckKindBuild),
	}}
	lint := ""
	switch {
	case ruffConfigured(root):
		lint = python + " -m ruff check " + list
	case flake8Configured(root):
		lint = python + " -m flake8 " + list
	}
	if lint != "" {
		out = append(out, Target{
			Kind: evidence.CheckKindLint, Command: lint, Scope: files,
			Reason: "lint the changed Python file(s) with the configured linter", Advisory: !p.requires(evidence.CheckKindLint),
		})
	}
	return out
}
