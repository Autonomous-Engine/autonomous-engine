package controller

import (
	"path"
	"regexp"
	"strings"
)

// Language names used by the test-file heuristic.
const (
	LangGo         = "go"
	LangTypeScript = "typescript"
	LangJavaScript = "javascript"
	LangPython     = "python"
)

// LanguageOf infers a file's language from its extension; "" when the
// extension is not one the heuristic knows.
func LanguageOf(p string) string {
	switch strings.ToLower(path.Ext(normalizePath(p))) {
	case ".go":
		return LangGo
	case ".ts", ".tsx", ".mts", ".cts":
		return LangTypeScript
	case ".js", ".jsx", ".mjs", ".cjs":
		return LangJavaScript
	case ".py":
		return LangPython
	}
	return ""
}

var jsTestFile = regexp.MustCompile(`\.(test|spec)\.[cm]?[jt]sx?$`)

// IsTestFile reports whether path is a test file. lang selects the
// language rule; "" infers it from the extension (LanguageOf). Rules:
//
//	go:                      *_test.go
//	typescript / javascript: *.test.{ts,tsx,js,jsx,mjs,cjs,...}, *.spec.*, anything under __tests__/ or test/
//	python:                  test_*.py, *_test.py, anything under test/
//	any language:            anything under a tests/ directory, *.spec.<code extension>
//
// test/ (singular) counts only for JS/TS/Python: in Go it is commonly a
// fixtures or harness directory of non-test code.
func IsTestFile(lang, p string) bool {
	p = normalizePath(p)
	if p == "" || p == "." {
		return false
	}
	if lang == "" {
		lang = LanguageOf(p)
	}
	base := path.Base(p)
	switch lang {
	case LangGo:
		if strings.HasSuffix(base, "_test.go") {
			return true
		}
	case LangTypeScript, LangJavaScript:
		if jsTestFile.MatchString(base) || hasDir(p, "__tests__") || hasDir(p, "test") {
			return true
		}
	case LangPython:
		if strings.HasPrefix(base, "test_") || strings.HasSuffix(base, "_test.py") || hasDir(p, "test") {
			return true
		}
	}
	return hasDir(p, "tests") || specFile.MatchString(base)
}

// specFile matches *.spec.<code extension>; a .spec. infix on a non-code
// file (deploy.spec.yaml) is not a test.
var specFile = regexp.MustCompile(`\.spec\.(rb|[cm]?[jt]sx?|py|go|java|kt|php|cs|exs?|swift|scala)$`)

// hasDir reports whether dir is one of p's directory components.
func hasDir(p, dir string) bool {
	parts := strings.Split(p, "/")
	for _, part := range parts[:len(parts)-1] {
		if part == dir {
			return true
		}
	}
	return false
}

// normalizePath makes a tool-call path comparable: forward slashes, no
// leading "./", cleaned.
func normalizePath(p string) string {
	p = strings.TrimSpace(strings.ReplaceAll(p, "\\", "/"))
	if p == "" {
		return ""
	}
	return strings.TrimPrefix(path.Clean(p), "./")
}

// redirectFD matches file-descriptor redirections such as 2>&1, >&2, so
// their '&' is not mistaken for backgrounding.
var redirectFD = regexp.MustCompile(`\d*>&\d+`)

// IsTestCommand reports whether a run_command command line runs tests in
// a way whose exit status is the test runner's.
//
// The configured test command (detected — internal/testdetect, or a
// repository policy override) is trusted as written: the command counts
// when it equals it, equals "cd <dir> && <configured>", or is the
// configured command followed by extra arguments. This is checked before
// any metacharacter rule, so a compound configured command ("make lint;
// make test", "go test ./... 2>&1 | tee out.log") run verbatim is
// evidence. Extra arguments must not add shell operators or a
// non-evidence flag (hasNonEvidenceFlag).
//
// Otherwise the usual runners are recognised: go test, npm/yarn/pnpm/bun
// test, npm run test[:x], npx/yarn/pnpm exec vitest|jest|mocha, vitest,
// jest, mocha, node --test, pytest, python -m pytest|unittest, cargo
// test, mvn/gradle test, make test, dotnet test, rspec, phpunit; the
// package managers also with a leading workspace selector (npm -w <ws>
// test, pnpm --filter <sel> test, yarn workspace <name> test). Such a command may be a
// chain joined by && ("cd web && npm test"), may set environment
// variables or wrap the runner in timeout, and must not carry a
// non-evidence flag. It is NOT a test command when the shell could mask
// the runner's exit status: a pipe (go test | tail — the exit status is
// tail's), ||, ;, a newline, backgrounding with &, or command
// substitution. Such runs are recorded but are not evidence.
func IsTestCommand(cmd, detected string) bool {
	c := normalizeCommand(cmd)
	if c == "" {
		return false
	}
	if det := normalizeCommand(detected); det != "" && matchesConfigured(c, det) {
		return true
	}
	if hasShellOperators(c) {
		return false
	}
	for _, seg := range strings.Split(c, "&&") {
		fields := stripWrappers(strings.Fields(seg))
		if len(fields) == 0 {
			continue
		}
		if isRunnerInvocation(fields) && !hasNonEvidenceFlag(fields[1:]) && !hasNamePatternT(fields) {
			return true
		}
	}
	return false
}

// normalizeCommand strips file-descriptor redirections (2>&1) and
// collapses whitespace. A newline is kept as an operator.
func normalizeCommand(cmd string) string {
	c := redirectFD.ReplaceAllString(cmd, "")
	lines := strings.Split(strings.TrimSpace(c), "\n")
	for i, l := range lines {
		lines[i] = strings.Join(strings.Fields(l), " ")
	}
	return strings.Join(lines, "\n")
}

// hasShellOperators reports operators that can mask a runner's exit
// status: ; | || newline, a lone &, backticks and $(…).
func hasShellOperators(c string) bool {
	if strings.ContainsAny(c, ";|\n`") || strings.Contains(c, "$(") {
		return true
	}
	return strings.Contains(strings.ReplaceAll(c, "&&", ""), "&")
}

// cdPrefix matches a leading "cd <dir> && ".
var cdPrefix = regexp.MustCompile(`^cd \S+ && `)

// matchesConfigured reports whether c runs the configured command det
// verbatim, after a cd, or with extra plain arguments.
func matchesConfigured(c, det string) bool {
	for _, cand := range []string{c, cdPrefix.ReplaceAllString(c, "")} {
		if cand == det {
			return true
		}
		if strings.HasPrefix(cand, det+" ") {
			extra := strings.TrimPrefix(cand, det+" ")
			// A name filter the extra arguments add (npm test -- -t x) is
			// not evidence; one the configured command has is trusted.
			addsNameFilter := hasNamePatternT(strings.Fields(cand)) && !hasNamePatternT(strings.Fields(det))
			if !hasShellOperators(extra) && !hasNonEvidenceFlag(strings.Fields(extra)) && !addsNameFilter {
				return true
			}
		}
	}
	return false
}

// hasNonEvidenceFlag reports arguments that make a test run prove
// nothing: selecting no tests (go test -run NONE / -run '^$'), listing
// instead of running (-list), passing with no tests (--passWithNoTests),
// or skipping them (-DskipTests, -Dmaven.test.skip=true).
func hasNonEvidenceFlag(args []string) bool {
	for i, a := range args {
		a = strings.Trim(a, `'"`)
		switch {
		case a == "-list" || a == "--list" || strings.HasPrefix(a, "-list="):
			return true
		case isTestNameSelection(a):
			return true
		case a == "--passWithNoTests" || strings.HasPrefix(a, "--passWithNoTests="):
			return true
		case a == "-DskipTests" || strings.HasPrefix(a, "-DskipTests=") || strings.HasPrefix(a, "-Dmaven.test.skip"):
			return true
		case a == "-run" || a == "--run":
			if i+1 < len(args) && isEmptySelection(args[i+1]) {
				return true
			}
		case strings.HasPrefix(a, "-run="):
			if isEmptySelection(strings.TrimPrefix(a, "-run=")) {
				return true
			}
		}
	}
	return false
}

// isTestNameSelection reports options that run only the tests whose names
// match (or skip those that do): node's --test-name-pattern,
// --test-skip-pattern and --test-only, jest/vitest --testNamePattern. A
// run filtered by test name proves nothing about the rest of the file.
func isTestNameSelection(a string) bool {
	name := a
	if i := strings.IndexByte(a, '='); i >= 0 {
		name = a[:i]
	}
	switch name {
	case "--test-name-pattern", "--test-skip-pattern", "--test-only", "--testNamePattern":
		return true
	}
	return false
}

// hasNamePatternT reports jest's / vitest's -t (--testNamePattern short
// form). It is only read so when jest or vitest is the runner: for mocha
// -t is the timeout.
//
// Without a runner word (`npm test -- -t add`, where the script is the
// runner) a -t after `--` is read the same way unless mocha appears.
func hasNamePatternT(fields []string) bool {
	runner, mocha := false, false
	for _, f := range fields {
		switch path.Base(f) {
		case "jest", "vitest":
			runner = true
		case "mocha", "_mocha":
			mocha = true
		}
	}
	isT := func(f string) bool { return strings.HasPrefix(f, "-t") && !strings.HasPrefix(f, "--") }
	if runner {
		for _, f := range fields {
			if isT(f) {
				return true
			}
		}
		return false
	}
	if mocha {
		return false
	}
	forwarded := false
	for _, f := range fields {
		if f == "--" {
			forwarded = true
			continue
		}
		if forwarded && isT(f) {
			return true
		}
	}
	return false
}

// isEmptySelection reports a go test -run pattern conventionally used to
// select no tests.
func isEmptySelection(p string) bool {
	p = strings.Trim(p, `'"`)
	return p == "NONE" || p == "^$" || p == "XXX"
}

// stripWrappers drops leading VAR=value assignments and timeout/env/time
// wrappers so the runner itself is first.
func stripWrappers(f []string) []string {
	for len(f) > 0 {
		switch {
		case isAssignment(f[0]):
			f = f[1:]
		case f[0] == "env" || f[0] == "time" || f[0] == "command":
			f = f[1:]
		case f[0] == "timeout" && len(f) > 1:
			f = f[2:]
		default:
			return f
		}
	}
	return f
}

func isAssignment(tok string) bool {
	i := strings.IndexByte(tok, '=')
	if i <= 0 {
		return false
	}
	for _, r := range tok[:i] {
		if !(r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

func isRunnerInvocation(f []string) bool {
	bin := path.Base(f[0])
	arg := func(i int) string {
		if i < len(f) {
			return f[i]
		}
		return ""
	}
	hasArg := func(want string) bool {
		for _, a := range f[1:] {
			if a == want {
				return true
			}
		}
		return false
	}
	switch bin {
	case "go", "cargo", "dotnet", "bun":
		return arg(1) == "test"
	case "node":
		// node --test [node flags] [files]: the built-in test runner
		// (M3-05 fast checks run `npm test --workspace=x -- <files>` for
		// node:test packages, and models run it directly).
		for i := 1; i < len(f); i++ {
			a := f[i]
			if !strings.HasPrefix(a, "-") {
				return false
			}
			if a == "--test" {
				return true
			}
			if nodeValueFlags[a] {
				i++ // --import tsx --test
			}
		}
		return false
	case "npm", "yarn", "pnpm":
		// Workspace / directory selectors before the subcommand:
		// npm -w packages/a test, pnpm --filter ./a test,
		// yarn workspace @scope/a test.
		if rest := stripPMSelectors(bin, f); len(rest) != len(f) {
			return isRunnerInvocation(rest)
		}
		switch arg(1) {
		case "test", "t":
			return true
		case "run":
			return arg(2) == "test" || strings.HasPrefix(arg(2), "test:")
		case "exec", "dlx":
			return isJSRunner(arg(2))
		}
		return bin != "npm" && isJSRunner(arg(1)) // yarn vitest, pnpm jest
	case "npx", "pnpx", "bunx":
		i := 1
		for i < len(f) && strings.HasPrefix(f[i], "-") {
			i++
		}
		return isJSRunner(arg(i))
	case "vitest", "jest", "mocha":
		return true
	case "pytest", "py.test", "rspec", "phpunit":
		return true
	case "python", "python3":
		return arg(1) == "-m" && (arg(2) == "pytest" || arg(2) == "unittest")
	case "mvn", "gradle", "gradlew", "make":
		return hasArg("test")
	case "bundle":
		return arg(1) == "exec" && path.Base(arg(2)) == "rspec"
	}
	return false
}

// nodeValueFlags are node options written with their value as the next
// word.
var nodeValueFlags = map[string]bool{
	"--import": true, "--require": true, "-r": true, "--loader": true, "--experimental-loader": true,
	"--conditions": true, "-C": true, "--env-file": true, "--test-reporter": true,
	"--test-reporter-destination": true, "--test-concurrency": true, "--test-timeout": true,
}

// pmSelectorFlags are package-manager options that only choose where the
// subcommand runs; the value-taking ones consume the next word.
var pmSelectorFlags = map[string]map[string]bool{
	"npm":  {"-w": true, "--workspace": true, "--prefix": true, "-ws": false, "--workspaces": false, "--if-present": false, "--silent": false, "-s": false},
	"pnpm": {"--filter": true, "-F": true, "-C": true, "--dir": true, "-r": false, "--recursive": false, "--silent": false, "-s": false},
	"yarn": {"--cwd": true, "--silent": false, "-s": false},
}

// stripPMSelectors returns f with the package manager's leading
// selector options (and `yarn workspace <name>`) removed, keeping f[0];
// f itself when there are none.
func stripPMSelectors(bin string, f []string) []string {
	i := 1
	for i < len(f) {
		a := f[i]
		if bin == "yarn" && a == "workspace" && i+1 < len(f) {
			i += 2
			continue
		}
		name := a
		if eq := strings.IndexByte(a, '='); eq >= 0 {
			name = a[:eq]
		}
		takesValue, ok := pmSelectorFlags[bin][name]
		if !ok {
			break
		}
		i++
		if takesValue && name == a {
			i++
		}
	}
	if i == 1 || i > len(f) {
		return f
	}
	return append([]string{f[0]}, f[i:]...)
}

func isJSRunner(tok string) bool {
	switch path.Base(tok) {
	case "vitest", "jest", "mocha":
		return true
	}
	return false
}
