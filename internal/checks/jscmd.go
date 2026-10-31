package checks

import (
	"fmt"
	"path"
	"regexp"
	"strings"
)

// JavaScript/TypeScript test commands taken apart, so fast targets run the
// gate's own runner with the gate's own flags and only ever add file
// filters the runner applies as a restriction of the gate's test set: the
// fast loop must never be stricter than the gate.

// Runner kinds.
const (
	runnerVitest = "vitest"
	runnerJest   = "jest"
	runnerMocha  = "mocha"
	runnerNode   = "node:test"
)

// jsRunner is a single test-runner invocation (a package's test script, or
// a direct policy command).
type jsRunner struct {
	kind string
	// narrowable is true when test files can be appended as filters: the
	// invocation selects no tests itself (no positional paths or patterns),
	// carries no flag that changes which tests run or makes a subset fail
	// where the whole would pass (coverage thresholds), and no runner
	// config customises discovery. Otherwise only the whole package runs.
	narrowable bool
	why        string // why not narrowable
	recursive  bool   // mocha --recursive
}

type flagTable struct {
	bools       map[string]bool
	values      map[string]bool
	subcommands map[string]bool // allowed as the first positional
	prefixes    []string        // "--name.*=" style flags accepted with "="
}

func set(words ...string) map[string]bool {
	m := map[string]bool{}
	for _, w := range words {
		m[w] = true
	}
	return m
}

// The flags below are those that leave the set of test files unchanged
// (output, parallelism, timeouts, environment). Anything else — coverage,
// config files, name/path patterns, sharding, changed-only — makes the
// invocation package-level only.
var jsRunnerFlags = map[string]flagTable{
	runnerVitest: {
		bools: set("--run", "--silent", "--globals", "--dom", "--isolate", "--no-isolate", "--allowOnly",
			"--passWithNoTests", "--no-color", "--color", "--hideSkippedTests", "--logHeapUsage", "--clearScreen",
			"--no-file-parallelism", "--fileParallelism", "--expandSnapshotDiff", "--printConsoleTrace", "--no-watch"),
		values: set("--reporter", "--outputFile", "--environment", "--pool", "--maxWorkers", "--minWorkers",
			"--testTimeout", "--hookTimeout", "--retry", "--bail", "--mode", "--maxConcurrency", "--teardownTimeout", "--silent"),
		subcommands: set("run", "watch", "dev"),
		prefixes:    []string{"--poolOptions.", "--sequence.", "--reporter."},
	},
	runnerJest: {
		bools: set("--ci", "--runInBand", "-i", "--silent", "--verbose", "--colors", "--detectOpenHandles", "--forceExit",
			"--no-cache", "--noStackTrace", "--useStderr", "--logHeapUsage", "--errorOnDeprecated", "--expand", "-e",
			"--no-watchman", "--watchman", "--bail", "-b", "--json", "--passWithNoTests", "--injectGlobals",
			"--clearMocks", "--resetMocks", "--restoreMocks", "--detectLeaks"),
		values: set("--maxWorkers", "-w", "--testTimeout", "--reporters", "--env", "--testEnvironment", "--cacheDirectory",
			"--outputFile", "--maxConcurrency", "--seed", "--workerIdleMemoryLimit"),
	},
	runnerMocha: {
		bools: set("--recursive", "--exit", "--bail", "-b", "--full-trace", "--color", "-c", "--no-color", "-C",
			"--check-leaks", "--parallel", "-p", "--forbid-only", "--forbid-pending", "--inline-diffs",
			"--allow-uncaught", "--async-only", "-A", "--diff", "--no-diff"),
		values: set("--reporter", "-R", "--reporter-option", "-O", "--timeout", "-t", "--slow", "-s", "--require", "-r",
			"--ui", "-u", "--jobs", "-j", "--retries"),
	},
	runnerNode: {
		bools: set("--test", "--test-force-exit", "--enable-source-maps", "--no-warnings", "--experimental-vm-modules",
			"--experimental-strip-types", "--experimental-transform-types", "--no-experimental-strip-types",
			"--experimental-detect-module", "--no-deprecation", "--trace-warnings"),
		values: set("--test-reporter", "--test-reporter-destination", "--test-concurrency", "--test-timeout", "--import",
			"--require", "-r", "--loader", "--experimental-loader", "--conditions", "-C", "--env-file", "--test-isolation"),
	},
}

// parseJSRunner recognises a single runner invocation: optional VAR=value
// words, `cross-env VAR=value…`, an npx/pnpm exec/yarn prefix, then vitest,
// jest, mocha (also as node_modules/.bin/<runner>) or `node --test`.
func parseJSRunner(cmdline string) (jsRunner, bool) {
	if strings.TrimSpace(cmdline) == "" || hasShellMeta(cmdline) {
		return jsRunner{}, false
	}
	f := strings.Fields(cmdline)
	for len(f) > 0 && isEnvAssignment(f[0]) {
		f = f[1:]
	}
	if len(f) > 0 && binName(f[0]) == "cross-env" {
		f = f[1:]
		for len(f) > 0 && isEnvAssignment(f[0]) {
			f = f[1:]
		}
	}
	if len(f) > 0 {
		switch binName(f[0]) {
		case "npx", "pnpx", "bunx":
			f = f[1:]
			for len(f) > 0 && strings.HasPrefix(f[0], "-") {
				f = f[1:]
			}
		case "pnpm", "yarn":
			if len(f) > 1 && f[1] == "exec" {
				f = f[2:]
			} else if len(f) > 1 && (f[1] == "vitest" || f[1] == "jest" || f[1] == "mocha") {
				f = f[1:]
			}
		}
	}
	if len(f) == 0 {
		return jsRunner{}, false
	}
	var r jsRunner
	args := f[1:]
	switch binName(f[0]) {
	case "vitest":
		r.kind = runnerVitest
	case "jest":
		r.kind = runnerJest
	case "mocha", "_mocha":
		r.kind = runnerMocha
	case "node":
		hasTest := false
		for i := 0; i < len(args); i++ {
			a := args[i]
			if !strings.HasPrefix(a, "-") {
				break
			}
			if a == "--test" {
				hasTest = true
			}
			if jsRunnerFlags[runnerNode].values[a] {
				i++ // --import tsx --test
			}
		}
		if !hasTest {
			return jsRunner{}, false
		}
		r.kind = runnerNode
	default:
		return jsRunner{}, false
	}
	r.narrowable, r.why = analyzeRunnerArgs(args, jsRunnerFlags[r.kind])
	for _, a := range args {
		if a == "--recursive" {
			r.recursive = true
		}
	}
	return r, true
}

// analyzeRunnerArgs reports whether the arguments leave the runner's test
// set to its defaults (so appended file filters restrict it), and why not.
func analyzeRunnerArgs(args []string, t flagTable) (bool, string) {
	positional := 0
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			if i+1 < len(args) {
				return false, fmt.Sprintf("it passes arguments after -- (%s)", strings.Join(args[i+1:], " "))
			}
			continue
		}
		if !strings.HasPrefix(a, "-") {
			if positional == 0 && t.subcommands[a] {
				positional++
				continue
			}
			return false, fmt.Sprintf("it already selects tests (%s)", a)
		}
		name := a
		if eq := strings.IndexByte(a, '='); eq >= 0 {
			name = a[:eq]
			ok := t.bools[name] || t.values[name]
			for _, p := range t.prefixes {
				ok = ok || strings.HasPrefix(name, p)
			}
			if !ok {
				return false, fmt.Sprintf("flag %s may change which tests run", a)
			}
			continue
		}
		switch {
		case t.bools[name]:
		case t.values[name]:
			i++
		default:
			return false, fmt.Sprintf("flag %s may change which tests run", a)
		}
	}
	return true, ""
}

// Runner config files and the keys in them that customise discovery or
// thresholds. Their presence in the runner's package makes it
// package-level only.
var (
	vitestConfigFile = regexp.MustCompile(`^(vitest|vite)\.config\.[cm]?[jt]s$|^vitest\.(workspace|projects)\.`)
	vitestConfigKeys = regexp.MustCompile(`\b(include|exclude|dir|root|projects|workspace|coverage|includeSource|typecheck|alias)\b`)
	jestConfigFile   = regexp.MustCompile(`^jest\.config\.([cm]?[jt]s|json)$`)
	jestConfigKeys   = regexp.MustCompile(`\b(testMatch|testRegex|roots|testPathIgnorePatterns|projects|coverageThreshold|rootDir|modulePathIgnorePatterns|testPathPattern|moduleFileExtensions|preset|moduleNameMapper)\b`)
	jestTestName     = regexp.MustCompile(`\.(test|spec)\.[jt]sx?$`)
	jestCodeExt      = regexp.MustCompile(`\.[jt]sx?$`)
	mochaConfigFile  = regexp.MustCompile(`^\.mocharc(\.[a-z]+)?$`)
)

// Test-file rules per runner (their defaults; a config that changes them
// makes the runner package-level only, see above).
var (
	jsTestName     = regexp.MustCompile(`\.(test|spec)\.[cm]?[jt]sx?$`)
	jsCodeExt      = regexp.MustCompile(`\.[cm]?[jt]sx?$`)
	nodeTestName   = regexp.MustCompile(`(^test|[.\-_]test|^test-.*)\.[cm]?js$`)
	nodeTestJSExt  = regexp.MustCompile(`\.[cm]?js$`)
	mochaTestJSExt = regexp.MustCompile(`\.(js|cjs|mjs)$`)
)

// isRunnerTestFile reports whether the runner, run with its defaults from
// its package directory, picks rel (package-relative) up as a test file.
func isRunnerTestFile(r jsRunner, rel string) bool {
	segs := strings.Split(rel, "/")
	dirs := segs[:len(segs)-1]
	base := segs[len(segs)-1]
	hasDir := func(names ...string) bool {
		for _, d := range dirs {
			for _, n := range names {
				if d == n {
					return true
				}
			}
		}
		return false
	}
	if hasDir("node_modules") {
		return false
	}
	switch r.kind {
	case runnerVitest:
		return jsTestName.MatchString(base) && !hasDir("dist", "cypress")
	case runnerJest:
		// jest 29's default testMatch: **/__tests__/**/*.[jt]s?(x) and
		// **/?(*.)+(spec|test).[jt]s?(x) — no .mjs/.cjs/.mts/.cts.
		return jestTestName.MatchString(base) || (hasDir("__tests__") && jestCodeExt.MatchString(base))
	case runnerMocha:
		if !mochaTestJSExt.MatchString(base) || len(dirs) == 0 || dirs[0] != "test" {
			return false
		}
		return len(dirs) == 1 || r.recursive
	case runnerNode:
		return nodeTestJSExt.MatchString(base) && (nodeTestName.MatchString(base) || hasDir("test"))
	}
	return false
}

var (
	// "." is left as is: as a regexp it still matches the literal dot, and
	// at worst also another test file the gate runs anyway.
	jestRegexMeta = regexp.MustCompile(`[\\^$|?*+()\[\]{}]`)
	globMeta      = regexp.MustCompile(`[*?\[\]{}()!]`)
)

// filterArg renders a test file (relative to the runner's directory) as a
// filter argument the runner reads as exactly that file, shell-quoted for
// `sh -c`. jest reads filters as regular expressions, so metacharacters are
// escaped (except "."); node --test and mocha read them as globs, which cannot express
// every literal path, so such a path is not filterable (ok false: the
// unit's whole test script runs). vitest filters are plain substrings of
// the file path.
func filterArg(kind, rel string) (string, bool) {
	// A path starting with "-" would be read as an option: jest gets an
	// escaped "\-", node/mocha a "./" prefix, and vitest (substring
	// filters, where "./" would not match) runs the whole script.
	arg := rel
	switch kind {
	case runnerJest:
		arg = jestRegexMeta.ReplaceAllStringFunc(rel, func(m string) string { return `\` + m })
		if strings.HasPrefix(arg, "-") {
			arg = `\` + arg
		}
	case runnerNode, runnerMocha:
		if globMeta.MatchString(rel) {
			return "", false
		}
		if strings.HasPrefix(arg, "-") {
			arg = "./" + arg
		}
	case runnerVitest:
		if strings.HasPrefix(arg, "-") {
			return "", false
		}
	}
	return shQuote(arg), true
}

// jsGate is the policy's test command as selection understands it.
type jsGate struct {
	pm string // npm | yarn | pnpm; "" for a direct runner command
	// direct is the policy command when it invokes the runner itself
	// (`npx vitest run`); the runner package is the root.
	direct string
	// delegated is true for a root test script that runs every
	// workspace's own test script (`npm test --workspaces --if-present`).
	delegated bool
	wsFilter  []string // --workspace selections; empty = all workspaces
	ifPresent bool
	// rootRunner is the runner of the root script or direct command when
	// not delegated.
	rootRunner jsRunner
}

// jsGateKind classifies the command without reading the repository:
// "script" (npm/yarn/pnpm test), "direct" (a runner invocation) or ""
// (not a JavaScript test command: a runner mismatch).
func jsGateKind(cmd string) (kind, pm string, err error) {
	f := strings.Fields(cmd)
	if len(f) == 0 || hasShellMeta(cmd) {
		return "", "", nil
	}
	switch pm = binName(f[0]); pm {
	case "npm", "yarn", "pnpm":
		isTest := len(f) >= 2 && (f[1] == "test" || f[1] == "t" || f[1] == "tst") ||
			len(f) >= 3 && (f[1] == "run" || f[1] == "run-script") && f[2] == "test"
		if !isTest {
			if _, ok := parseJSRunner(cmd); ok { // pnpm exec vitest, yarn jest
				return "direct", "", nil
			}
			return "", "", nil
		}
		want := 2
		if f[1] == "run" || f[1] == "run-script" {
			want = 3
		}
		if len(f) > want {
			return "script", pm, fmt.Errorf("the policy test command %q passes extra arguments to the test script; impact selection does not narrow it", cmd)
		}
		return "script", pm, nil
	}
	if _, ok := parseJSRunner(cmd); ok {
		return "direct", "", nil
	}
	return "", "", nil
}

// parseJSGate reads the policy command against the root package.
func parseJSGate(cmd string, root *jsPackage) (jsGate, error) {
	kind, pm, err := jsGateKind(cmd)
	if err != nil {
		return jsGate{}, err
	}
	switch kind {
	case "":
		return jsGate{}, runnerMismatch("the policy test command %q is not an npm/yarn/pnpm test script or a vitest, jest, mocha or node --test invocation; impact selection does not narrow it", cmd)
	case "direct":
		r, _ := parseJSRunner(cmd)
		return jsGate{direct: strings.Join(strings.Fields(cmd), " "), rootRunner: r}, nil
	}
	g := jsGate{pm: pm}
	script := strings.TrimSpace(root.scripts["test"])
	if script == "" {
		return jsGate{}, fmt.Errorf("the root package.json has no test script for %q", cmd)
	}
	if ok, filter, ifPresent := parseNpmDelegation(script); ok {
		g.delegated, g.wsFilter, g.ifPresent = true, filter, ifPresent
		return g, nil
	}
	r, ok := parseJSRunner(script)
	if !ok {
		return jsGate{}, fmt.Errorf("the root test script %q is neither a single test-runner invocation nor `npm test --workspaces`; impact selection does not narrow it", script)
	}
	g.rootRunner = r
	return g, nil
}

// parseNpmDelegation recognises `npm test --workspaces [--if-present]`,
// `npm run test -ws`, `npm test --workspace=a -w b`.
func parseNpmDelegation(script string) (ok bool, filter []string, ifPresent bool) {
	if hasShellMeta(script) {
		return false, nil, false
	}
	f := strings.Fields(script)
	if len(f) < 2 || binName(f[0]) != "npm" {
		return false, nil, false
	}
	var words []string
	all := false
	for i := 1; i < len(f); i++ {
		a := f[i]
		switch {
		case a == "--workspaces" || a == "-ws" || a == "--ws":
			all = true
		case a == "--if-present":
			ifPresent = true
		case strings.HasPrefix(a, "--workspace="):
			filter = append(filter, strings.TrimPrefix(a, "--workspace="))
		case a == "--workspace" || a == "-w":
			if i+1 >= len(f) {
				return false, nil, false
			}
			i++
			filter = append(filter, f[i])
		case strings.HasPrefix(a, "-"):
			return false, nil, false // --include-workspace-root, args for the scripts, …
		default:
			words = append(words, a)
		}
	}
	isTest := len(words) == 1 && (words[0] == "test" || words[0] == "t" || words[0] == "tst") ||
		len(words) == 2 && (words[0] == "run" || words[0] == "run-script") && words[1] == "test"
	if !isTest || (!all && len(filter) == 0) {
		return false, nil, false
	}
	if all {
		filter = nil
	}
	return true, filter, ifPresent
}

// npmWorkspaceMatches reports whether a --workspace selection names pkg
// (by name or by directory).
func npmWorkspaceMatches(sel string, pkg *jsPackage) bool {
	sel = strings.TrimSuffix(strings.TrimPrefix(sel, "./"), "/")
	return sel == pkg.name || sel == pkg.dir || path.Clean(sel) == pkg.dir
}
