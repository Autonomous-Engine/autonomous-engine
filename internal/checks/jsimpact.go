package checks

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/jsresolve"
	"github.com/autonomousengine/autonomous-engine/internal/testdetect"
)

// JSImpact selects checks for a JavaScript/TypeScript repository: a root
// package.json, optionally an npm workspace (its "workspaces" globs).
//
// A changed file belongs to the nearest enclosing package (a workspace
// package, else the root). The import graph is built from source with a
// conservative scan (import/export … from, import(), require(), and
// jest/vi.mock specifiers) over every JS/TS file outside node_modules and
// hidden directories: relative specifiers resolve to files (extensions,
// index files, the TypeScript `./x.js` → `./x.ts` convention), and a bare
// specifier naming a workspace package is an edge to that whole package.
// No toolchain is needed and selection is deterministic.
//
// Test targets narrow the gate's own command (see jscmd.go): the gate's
// runner and flags verbatim, plus the affected test files as filters when
// the runner's invocation and config leave discovery at its defaults, else
// the owning package's whole test script. Typecheck (`tsc --noEmit -p
// <tsconfig>`) and the package's lint script are advisory unless the
// policy requires typecheck / lint.
type JSImpact struct{}

// MaxJSTestFiles caps per-file test targets per package; above it the
// package's whole test script runs.
const MaxJSTestFiles = 25

func (JSImpact) Language() string { return testdetect.LanguageJavaScript }

func (JSImpact) Applies(repoTree string) bool {
	return repoTree != "" && testdetect.HasNodePackage(repoTree)
}

// jsPackage is the root package or one workspace package.
type jsPackage struct {
	dir     string // "." for the root, else "packages/app"
	name    string
	scripts map[string]string
	deps    map[string]bool
	raw     map[string]json.RawMessage
}

type jsRepo struct {
	root    *jsPackage
	pkgs    []*jsPackage // workspace packages, sorted by dir
	byName  map[string]*jsPackage
	files   map[string]bool
	code    []string // JS/TS files, sorted
	lockPM  string   // package manager from the lockfile
	tsAvail bool     // typescript is a dependency of the root
}

var jsSourceExt = regexp.MustCompile(`\.([cm]?[jt]sx?|vue|svelte)$`)

func isTSFile(f string) bool {
	switch path.Ext(f) {
	case ".ts", ".tsx", ".mts", ".cts":
		return true
	}
	return false
}

func readJSPackage(root, dir string) (*jsPackage, error) {
	rel := path.Join(dir, "package.json")
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return nil, fmt.Errorf("read %s: %v", rel, err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("parse error in %s: %v", rel, err)
	}
	p := &jsPackage{dir: dir, scripts: map[string]string{}, deps: map[string]bool{}, raw: raw}
	_ = json.Unmarshal(raw["name"], &p.name)
	_ = json.Unmarshal(raw["scripts"], &p.scripts)
	for _, k := range []string{"dependencies", "devDependencies", "peerDependencies", "optionalDependencies"} {
		var m map[string]json.RawMessage
		if json.Unmarshal(raw[k], &m) == nil {
			for n := range m {
				p.deps[n] = true
			}
		}
	}
	return p, nil
}

// workspaceGlobs reads "workspaces": [..] or {"packages": [..]}.
func (p *jsPackage) workspaceGlobs() []string {
	var list []string
	if json.Unmarshal(p.raw["workspaces"], &list) == nil {
		return list
	}
	var obj struct {
		Packages []string `json:"packages"`
	}
	if json.Unmarshal(p.raw["workspaces"], &obj) == nil {
		return obj.Packages
	}
	return nil
}

func loadJSRepo(ctx context.Context, root string) (*jsRepo, error) {
	rootPkg, err := readJSPackage(root, ".")
	if err != nil {
		return nil, err
	}
	repo := &jsRepo{root: rootPkg, byName: map[string]*jsPackage{}, files: map[string]bool{}, lockPM: "npm"}
	var include, exclude []*regexp.Regexp
	for _, g := range rootPkg.workspaceGlobs() {
		neg := strings.HasPrefix(g, "!")
		re, err := globRegexp(strings.TrimPrefix(g, "!"))
		if err != nil {
			return nil, fmt.Errorf("workspace glob %q: %v", g, err)
		}
		if neg {
			exclude = append(exclude, re)
		} else {
			include = append(include, re)
		}
	}
	isWorkspace := func(dir string) bool {
		ok := false
		for _, re := range include {
			ok = ok || re.MatchString(dir)
		}
		for _, re := range exclude {
			ok = ok && !re.MatchString(dir)
		}
		return ok
	}
	err = walkRepo(root, nil, func(rel string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		repo.files[rel] = true
		if jsSourceExt.MatchString(rel) {
			repo.code = append(repo.code, rel)
		}
		if path.Base(rel) == "package.json" && rel != "package.json" && len(include) > 0 {
			dir := path.Dir(rel)
			if isWorkspace(dir) {
				pk, err := readJSPackage(root, dir)
				if err != nil {
					return err
				}
				repo.pkgs = append(repo.pkgs, pk)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("import graph: %v", err)
	}
	sort.Strings(repo.code)
	sort.Slice(repo.pkgs, func(i, j int) bool { return repo.pkgs[i].dir < repo.pkgs[j].dir })
	for _, pk := range append([]*jsPackage{rootPkg}, repo.pkgs...) {
		if pk.name != "" {
			if _, dup := repo.byName[pk.name]; !dup {
				repo.byName[pk.name] = pk
			}
		}
	}
	switch {
	case repo.files["pnpm-lock.yaml"]:
		repo.lockPM = "pnpm"
	case repo.files["yarn.lock"]:
		repo.lockPM = "yarn"
	}
	repo.tsAvail = rootPkg.deps["typescript"]
	return repo, nil
}

// packageOf is the nearest enclosing workspace package of rel, else the
// root.
func (r *jsRepo) packageOf(rel string) *jsPackage {
	var best *jsPackage
	for _, pk := range r.pkgs {
		if within(rel, pk.dir) && (best == nil || len(pk.dir) > len(best.dir)) {
			best = pk
		}
	}
	if best == nil {
		return r.root
	}
	return best
}

// Import specifiers. The scan over-approximates (a specifier in a comment
// counts), which can only add affected files.
var (
	jsFromSpec   = regexp.MustCompile(`\bfrom\s*['"]([^'"\n]+)['"]`)
	jsBareImport = regexp.MustCompile(`\bimport\s*['"]([^'"\n]+)['"]`)
	jsCallSpec   = regexp.MustCompile(`\b(?:require|import|mock|doMock|unmock|requireActual|importActual|requireMock)\s*\(\s*['"]([^'"\n]+)['"]`)
)

// jsGraph maps each node to its importers. Nodes are repository-relative
// files and "pkg:<dir>" for a whole workspace package.
type jsGraph struct {
	importers map[string][]string
}

func pkgNode(pk *jsPackage) string { return "pkg:" + pk.dir }

func buildJSGraph(root string, repo *jsRepo, deleted map[string]bool) *jsGraph {
	g := &jsGraph{importers: map[string][]string{}}
	exists := func(f string) bool { return repo.files[f] || deleted[f] }
	resolve := func(from, spec string) string {
		base := path.Clean(path.Join(path.Dir(from), spec))
		if strings.HasPrefix(base, "../") || base == ".." {
			return ""
		}
		dep, _ := jsresolve.Resolve(base, exists)
		return dep
	}
	add := func(dep, importer string) {
		for _, x := range g.importers[dep] {
			if x == importer {
				return
			}
		}
		g.importers[dep] = append(g.importers[dep], importer)
	}
	for _, f := range repo.code {
		src, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f)))
		if err != nil {
			continue
		}
		s := string(src)
		var specs []string
		for _, re := range []*regexp.Regexp{jsFromSpec, jsBareImport, jsCallSpec} {
			for _, m := range re.FindAllStringSubmatch(s, -1) {
				specs = append(specs, m[1])
			}
		}
		for _, spec := range specs {
			if strings.HasPrefix(spec, "./") || strings.HasPrefix(spec, "../") {
				if dep := resolve(f, spec); dep != "" && dep != f {
					add(dep, f)
				}
				continue
			}
			name := spec
			parts := strings.Split(spec, "/")
			if strings.HasPrefix(spec, "@") && len(parts) >= 2 {
				name = parts[0] + "/" + parts[1]
			} else {
				name = parts[0]
			}
			if pk := repo.byName[name]; pk != nil && pk != repo.root && !within(f, pk.dir) {
				add(pkgNode(pk), f)
			}
		}
	}
	return g
}

// Configuration files whose change at the root can affect every package
// (dependencies, toolchain, shared TypeScript settings): selection falls
// back to the full suite.
var jsRootConfig = regexp.MustCompile(`^(package\.json|package-lock\.json|npm-shrinkwrap\.json|yarn\.lock|pnpm-lock\.yaml|pnpm-workspace\.yaml|\.npmrc|\.yarnrc(\.yml)?|\.nvmrc|\.node-version|tsconfig[^/]*\.json|babel\.config\.[^/]+|\.babelrc[^/]*|jest\.config\.[^/]+|(vitest|vite)\.config\.[^/]+|vitest\.(workspace|projects)\.[^/]+|\.mocharc[^/]*|\.swcrc)$`)

// A package's own runner/toolchain configuration: its whole test script
// runs.
var jsPackageConfig = regexp.MustCompile(`^(package\.json|tsconfig[^/]*\.json|babel\.config\.[^/]+|\.babelrc[^/]*|jest\.config\.[^/]+|(vitest|vite)\.config\.[^/]+|vitest\.(workspace|projects)\.[^/]+|\.mocharc[^/]*|\.swcrc)$`)

// looksLikeJSTest is the runner-independent test-file shape: such files
// never make their package "affected" for dependents.
func looksLikeJSTest(rel string) bool {
	if jsTestName.MatchString(path.Base(rel)) {
		return true
	}
	for _, s := range strings.Split(path.Dir(rel), "/") {
		if s == "__tests__" || s == "test" || s == "tests" {
			return true
		}
	}
	return false
}

// Targets implements Impact.
func (JSImpact) Targets(ctx context.Context, repoTree string, changed []string, p Policy) ([]Target, error) {
	gateKind, _, gateErr := jsGateKind(p.FullTestCommand)
	repo, err := loadJSRepo(ctx, repoTree)
	if err != nil {
		if gateKind == "" {
			return nil, runnerMismatch("%s", err.Error())
		}
		return nil, err
	}
	for _, f := range changed {
		if jsRootConfig.MatchString(f) {
			if gateKind == "" {
				break // not this language's gate: no test targets to protect
			}
			return nil, fmt.Errorf("%s changed: a dependency or toolchain configuration change can affect every package", f)
		}
	}

	deleted := map[string]bool{}
	for _, f := range changed {
		if !repo.files[f] && jsSourceExt.MatchString(f) {
			deleted[f] = true
		}
	}
	graph := buildJSGraph(repoTree, repo, deleted)

	// Seeds: changed files (existing or deleted). A workspace package whose
	// non-test code file or manifest is affected affects its importers in
	// other packages (a non-code file matters only through a code file
	// that imports it).
	importers := func(node string) []string {
		out := append([]string{}, graph.importers[node]...)
		code := jsSourceExt.MatchString(node) || path.Base(node) == "package.json"
		if !strings.HasPrefix(node, "pkg:") && code && !looksLikeJSTest(node) {
			if pk := repo.packageOf(node); pk != repo.root {
				out = append(out, pkgNode(pk))
			}
		}
		return out
	}
	affected := propagate(changed, importers)

	nonTest := jsNonTestTargets(repo, changed, affected, p)
	if gateKind == "" {
		return nonTest, runnerMismatch("the policy test command %q is not an npm/yarn/pnpm test script or a vitest, jest, mocha or node --test invocation; impact selection does not narrow it", p.FullTestCommand)
	}
	if gateErr != nil {
		return nil, gateErr
	}
	gate, err := parseJSGate(p.FullTestCommand, repo.root)
	if err != nil {
		return nil, err
	}
	tests, err := jsTestTargets(repoTree, repo, gate, changed, affected)
	if err != nil {
		return nil, err
	}
	return append(tests, nonTest...), nil
}

// jsUnit is one runner package of the gate: the root (a root runner or a
// direct command) or a workspace package the delegating root script runs.
type jsUnit struct {
	pkg       *jsPackage
	runner    jsRunner
	isRunner  bool   // its test script is a single runner invocation
	script    string // its test script
	pkgCmd    string // the whole package's tests, as the gate runs them
	filePfx   string // prefix for file-filtered runs ("" = not narrowable)
	whyNot    string // why file filters are not used
	wholeRoot bool   // root unit: every file belongs to it
}

func jsUnits(root string, repo *jsRepo, gate jsGate) []*jsUnit {
	if !gate.delegated {
		u := &jsUnit{pkg: repo.root, runner: gate.rootRunner, isRunner: true, wholeRoot: true}
		if gate.direct != "" {
			u.script, u.pkgCmd, u.filePfx = gate.direct, gate.direct, gate.direct
		} else {
			u.script = repo.root.scripts["test"]
			u.pkgCmd = gate.pm + " test"
			switch gate.pm {
			case "npm":
				u.filePfx = "npm test --"
			default:
				u.filePfx = gate.pm + " test"
			}
		}
		finishUnit(root, repo, u)
		return []*jsUnit{u}
	}
	var out []*jsUnit
	for _, pk := range repo.pkgs {
		if len(gate.wsFilter) > 0 {
			sel := false
			for _, s := range gate.wsFilter {
				sel = sel || npmWorkspaceMatches(s, pk)
			}
			if !sel {
				continue
			}
		}
		script := strings.TrimSpace(pk.scripts["test"])
		if script == "" {
			continue // --if-present skips it; without it the gate itself fails there
		}
		u := &jsUnit{pkg: pk, script: script}
		u.pkgCmd = "npm test --workspace=" + pk.dir
		u.filePfx = u.pkgCmd + " --"
		u.runner, u.isRunner = parseJSRunner(script)
		finishUnit(root, repo, u)
		out = append(out, u)
	}
	return out
}

// finishUnit decides whether file filters may be appended.
func finishUnit(root string, repo *jsRepo, u *jsUnit) {
	switch {
	case !u.isRunner:
		u.whyNot = fmt.Sprintf("its test script %q is not a single vitest, jest, mocha or node --test invocation", u.script)
	case !u.runner.narrowable:
		u.whyNot = fmt.Sprintf("its test command %q cannot take file filters: %s", u.script, u.runner.why)
	default:
		u.whyNot = runnerConfigBlocks(root, repo, u)
	}
	if u.whyNot != "" {
		u.filePfx = ""
	}
}

var (
	tsconfigFile  = regexp.MustCompile(`^(tsconfig[^/]*|jsconfig)\.json$`)
	tsconfigAlias = regexp.MustCompile(`"(paths|baseUrl)"\s*:`)
)

// aliasConfig reports an import-alias mechanism the source scan does not
// resolve — tsconfig/jsconfig compilerOptions.paths or baseUrl, or a
// package.json "imports" map (#subpath) — in the unit's package or the
// root (every package for a root runner). A test reaching changed code
// only through an alias would be missed, so the unit runs whole. (jest
// moduleNameMapper and vite resolve.alias are caught with the runner
// configs.)
func aliasConfig(root string, repo *jsRepo, u *jsUnit) string {
	pkgs := []*jsPackage{u.pkg}
	if u.pkg != repo.root {
		pkgs = append(pkgs, repo.root)
	}
	if u.wholeRoot {
		pkgs = append([]*jsPackage{repo.root}, repo.pkgs...)
	}
	for _, pk := range pkgs {
		if len(pk.raw["imports"]) > 0 && string(pk.raw["imports"]) != "{}" {
			return fmt.Sprintf("%s declares \"imports\" (#subpath aliases the import scan cannot resolve)", path.Join(pk.dir, "package.json"))
		}
		var files []string
		for f := range repo.files {
			if path.Dir(f) == pk.dir && tsconfigFile.MatchString(path.Base(f)) {
				files = append(files, f)
			}
		}
		sort.Strings(files)
		for _, f := range files {
			b, _ := os.ReadFile(filepath.Join(root, filepath.FromSlash(f)))
			if m := tsconfigAlias.FindStringSubmatch(string(b)); m != nil {
				return fmt.Sprintf("%s sets compilerOptions.%s (import aliases the import scan cannot resolve)", f, m[1])
			}
		}
	}
	return ""
}

// runnerConfigBlocks reports a runner config in the unit's directory that
// customises discovery (so the defaults isRunnerTestFile assumes may not
// hold).
func runnerConfigBlocks(root string, repo *jsRepo, u *jsUnit) string {
	if why := aliasConfig(root, repo, u); why != "" {
		return why
	}
	dir := u.pkg.dir
	var files []string
	for f := range repo.files {
		if path.Dir(f) == dir || (dir == "." && !strings.Contains(f, "/")) {
			files = append(files, f)
		}
	}
	sort.Strings(files)
	read := func(f string) string {
		b, _ := os.ReadFile(filepath.Join(root, filepath.FromSlash(f)))
		return string(b)
	}
	for _, f := range files {
		base := path.Base(f)
		switch u.runner.kind {
		case runnerVitest:
			if vitestConfigFile.MatchString(base) {
				if m := vitestConfigKeys.FindString(read(f)); m != "" {
					return fmt.Sprintf("%s sets %q, which may change which tests run", f, m)
				}
			}
		case runnerJest:
			if jestConfigFile.MatchString(base) {
				if m := jestConfigKeys.FindString(read(f)); m != "" {
					return fmt.Sprintf("%s sets %q, which may change which tests run", f, m)
				}
			}
		case runnerMocha:
			if mochaConfigFile.MatchString(base) {
				return fmt.Sprintf("%s configures mocha's spec", f)
			}
		}
	}
	switch u.runner.kind {
	case runnerJest:
		if raw := u.pkg.raw["jest"]; len(raw) > 0 {
			if m := jestConfigKeys.FindString(string(raw)); m != "" {
				return fmt.Sprintf("%s sets jest %q, which may change which tests run", path.Join(dir, "package.json"), m)
			}
		}
	case runnerMocha:
		if len(u.pkg.raw["mocha"]) > 0 {
			return fmt.Sprintf("%s configures mocha", path.Join(dir, "package.json"))
		}
	}
	return ""
}

func (u *jsUnit) owns(repo *jsRepo, rel string) bool {
	if u.wholeRoot {
		return true
	}
	return repo.packageOf(rel) == u.pkg
}

// jsTestTargets selects, per runner unit, the affected test files (as
// filters on the gate's own runner command) or the unit's whole test
// script.
func jsTestTargets(root string, repo *jsRepo, gate jsGate, changed []string, affected map[string]reachInfo) ([]Target, error) {
	units := jsUnits(root, repo, gate)
	// Changed manifests make the package and the packages depending on it
	// run whole.
	manifestChanged := map[string]string{} // package dir -> reason
	for _, f := range changed {
		if path.Base(f) != "package.json" || f == "package.json" {
			continue
		}
		pk := repo.packageOf(f)
		if pk == repo.root || pk.dir != path.Dir(f) {
			continue
		}
		manifestChanged[pk.dir] = "changed manifest " + f
		for _, other := range repo.pkgs {
			if other != pk && pk.name != "" && other.deps[pk.name] {
				if _, ok := manifestChanged[other.dir]; !ok {
					manifestChanged[other.dir] = fmt.Sprintf("depends on %s, whose manifest %s changed", pk.name, f)
				}
			}
		}
	}

	// Which changed files reach a test the gate runs (any unit)?
	reachedRoots := map[string]bool{}
	for f, ri := range affected {
		if strings.HasPrefix(f, "pkg:") || !repo.files[f] {
			continue
		}
		for _, u := range units {
			if u.owns(repo, f) && isRunnerTestFile(u.runner, relTo(f, u.pkg.dir)) && u.isRunner {
				reachedRoots[ri.root] = true
			}
		}
	}

	var out []Target
	for _, u := range units {
		var why []string
		if r, ok := manifestChanged[u.pkg.dir]; ok {
			why = append(why, r)
		}
		for _, f := range changed {
			if !u.owns(repo, f) {
				continue
			}
			base := path.Base(f)
			if path.Dir(f) == u.pkg.dir || (u.pkg.dir == "." && !strings.Contains(f, "/")) {
				if jsPackageConfig.MatchString(base) && base != "package.json" {
					why = append(why, "changed configuration "+f)
				}
			}
		}
		// Affected test files the unit's runner picks up.
		var tests []string
		for f := range affected {
			if strings.HasPrefix(f, "pkg:") || !repo.files[f] || !u.owns(repo, f) {
				continue
			}
			if u.isRunner && isRunnerTestFile(u.runner, relTo(f, u.pkg.dir)) {
				tests = append(tests, f)
			}
		}
		sort.Strings(tests)
		// Changed code in the unit that reaches no test the gate runs: the
		// whole package runs, since the scan cannot see every import
		// (aliases, dynamic paths).
		for _, f := range changed {
			if !u.owns(repo, f) || !jsSourceExt.MatchString(f) || looksLikeJSTest(f) || reachedRoots[f] {
				continue
			}
			why = append(why, "no test imports changed file "+f+" statically")
		}
		// Each test file as a filter the runner reads literally.
		args := map[string]string{}
		if u.filePfx != "" {
			for _, f := range tests {
				arg, ok := filterArg(u.runner.kind, relTo(f, u.pkg.dir))
				if !ok {
					why = append(why, fmt.Sprintf("test %s cannot be written as a literal %s filter", f, u.runner.kind))
					break
				}
				args[f] = arg
			}
		}
		switch {
		case len(why) > 0 || (len(tests) > 0 && (u.filePfx == "" || len(tests) > MaxJSTestFiles)):
			reason := strings.Join(why, "; ")
			if len(tests) > 0 {
				detail := fmt.Sprintf("%d affected test file(s)", len(tests))
				if u.filePfx == "" {
					detail += "; " + u.whyNot
				} else if len(tests) > MaxJSTestFiles {
					detail += fmt.Sprintf(" (more than %d)", MaxJSTestFiles)
				}
				if reason != "" {
					reason += "; "
				}
				reason += detail
			}
			label := u.pkg.dir
			if label == "." {
				label = "the root package"
			}
			out = append(out, Target{
				Kind: evidence.CheckKindTest, Command: u.pkgCmd, Scope: []string{u.pkg.dir},
				Reason: "whole test script of " + label + ": " + reason,
			})
		case len(tests) > 0:
			for _, f := range tests {
				out = append(out, Target{
					Kind: evidence.CheckKindTest, Command: u.filePfx + " " + args[f], Scope: []string{relTo(f, u.pkg.dir)},
					Reason: jsTestReason(repo, f, affected), batchPrefix: u.filePfx, batchArg: args[f],
				})
			}
		}
	}
	return out, nil
}

func jsTestReason(repo *jsRepo, f string, affected map[string]reachInfo) string {
	ri := affected[f]
	switch {
	case ri.root == f:
		return "changed test file " + f
	case ri.via == "":
		return fmt.Sprintf("test %s imports changed file %s", f, ri.root)
	case strings.HasPrefix(ri.via, "pkg:"):
		return fmt.Sprintf("test %s imports package %s, affected by changed file %s", f, repo.pkgLabel(ri.via), ri.root)
	default:
		return fmt.Sprintf("test %s is affected by changed file %s (via %s)", f, ri.root, ri.via)
	}
}

// pkgLabel names a "pkg:<dir>" node by its package name.
func (r *jsRepo) pkgLabel(node string) string {
	dir := strings.TrimPrefix(node, "pkg:")
	for _, pk := range r.pkgs {
		if pk.dir == dir && pk.name != "" {
			return pk.name
		}
	}
	return dir
}

// jsNonTestTargets: typecheck per package with an affected TypeScript file
// (or changed tsconfig), and the lint script of each package owning a
// changed file. Both are advisory unless the policy requires the kind.
func jsNonTestTargets(repo *jsRepo, changed []string, affected map[string]reachInfo, p Policy) []Target {
	var out []Target
	all := append([]*jsPackage{repo.root}, repo.pkgs...)
	// The reason names, in order of preference, a changed TypeScript file
	// or tsconfig, an affected non-test file, an affected test file.
	tsReason := map[string]string{}
	for pass := 0; pass < 3; pass++ {
		for _, f := range sortedKeys(affected) {
			if strings.HasPrefix(f, "pkg:") {
				continue
			}
			isCfg := strings.HasPrefix(path.Base(f), "tsconfig") && strings.HasSuffix(f, ".json")
			if !isTSFile(f) && !isCfg {
				continue
			}
			ri := affected[f]
			switch pass {
			case 0:
				if ri.root != f {
					continue
				}
			case 1:
				if looksLikeJSTest(f) {
					continue
				}
			}
			pk := repo.packageOf(f)
			if _, ok := tsReason[pk.dir]; ok {
				continue
			}
			if ri.root == f {
				tsReason[pk.dir] = "changed " + f
			} else {
				tsReason[pk.dir] = fmt.Sprintf("%s is affected by changed file %s", f, ri.root)
			}
		}
	}
	reqTC := p.requires(evidence.CheckKindTypecheck)
	for _, pk := range all {
		reason, ok := tsReason[pk.dir]
		if !ok {
			continue
		}
		cfg := path.Join(pk.dir, "tsconfig.json")
		if !repo.files[cfg] || !(repo.tsAvail || pk.deps["typescript"]) {
			continue
		}
		out = append(out, Target{
			Kind: evidence.CheckKindTypecheck, Command: "npx --no-install tsc --noEmit -p " + cfg, Scope: []string{pk.dir},
			Reason: "typecheck " + labelPkg(pk) + ": " + reason, Advisory: !reqTC,
		})
	}

	reqLint := p.requires(evidence.CheckKindLint)
	owners := map[string]string{}
	for _, f := range changed {
		if !repo.files[f] || !jsSourceExt.MatchString(f) {
			continue
		}
		pk := repo.packageOf(f)
		if _, ok := owners[pk.dir]; !ok {
			owners[pk.dir] = f
		}
	}
	for _, pk := range all {
		f, ok := owners[pk.dir]
		if !ok {
			continue
		}
		script := pk.scripts["lint"]
		if strings.TrimSpace(script) == "" || strings.Contains(script, "--fix") || strings.Contains(script, "--write") {
			continue // no lint script, or one that rewrites files in place
		}
		out = append(out, Target{
			Kind: evidence.CheckKindLint, Command: lintCommand(repo.lockPM, pk), Scope: []string{pk.dir},
			Reason: "lint script of " + labelPkg(pk) + " (owner of changed file " + f + ")", Advisory: !reqLint,
		})
	}
	return out
}

func labelPkg(pk *jsPackage) string {
	if pk.dir == "." {
		return "the root package"
	}
	return pk.dir
}

func lintCommand(pm string, pk *jsPackage) string {
	if pk.dir == "." {
		return pm + " run lint"
	}
	switch pm {
	case "pnpm":
		return "pnpm --filter ./" + pk.dir + " run lint"
	case "yarn":
		if pk.name != "" {
			return "yarn workspace " + pk.name + " run lint"
		}
	}
	return "npm run lint --workspace=" + pk.dir
}
