package checks

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/testdetect"
)

// GoImpact selects checks for a Go module rooted at the repository root.
//
// The import graph is built from the source (go/parser, imports only), not
// from `go list`, so selection needs no toolchain, network or module
// download and is deterministic. Build constraints are ignored: every .go
// file counts, which can only add dependents (safe). Directories named
// testdata or vendor, hidden or underscore-prefixed directories, and nested
// modules (a directory with its own go.mod) are outside the module graph.
type GoImpact struct{}

// MaxGoTestTargets caps per-package test targets; above it the impacted
// packages run in one `go test` invocation.
const MaxGoTestTargets = 25

func (GoImpact) Language() string { return "go" }

func (GoImpact) Applies(repoTree string) bool {
	return repoTree != "" && testdetect.HasGoModule(repoTree)
}

// goPkg is one package directory of the module.
type goPkg struct {
	dir          string   // "./internal/x" or "."
	importPath   string   // module path + "/internal/x"
	imports      []string // non-test imports
	testImports  []string // imports of _test.go files (internal and external)
	migrateTests []string // Test*Migrat* functions
}

var migrateTestFunc = regexp.MustCompile(`(?m)^func (Test\w*Migrat\w*)\(`)

// Targets implements Impact.
func (GoImpact) Targets(ctx context.Context, repoTree string, changed []string, p Policy) ([]Target, error) {
	full := strings.TrimSpace(p.FullTestCommand)
	if full == "" {
		full = "go test ./..."
	}
	gt, err := parseGoTest(full)
	if err != nil {
		return nil, err
	}
	for _, f := range changed {
		switch f {
		case "go.mod", "go.sum", "go.work", "go.work.sum":
			return nil, fmt.Errorf("%s changed: a dependency change can affect every package", f)
		}
	}
	modPath, err := goModulePath(filepath.Join(repoTree, "go.mod"))
	if err != nil {
		return nil, err
	}
	inPolicy, err := newPatternMatcher(gt.patterns, modPath)
	if err != nil {
		return nil, err
	}
	pkgs, err := loadGoPackages(ctx, repoTree, modPath)
	if err != nil {
		return nil, err
	}
	byImport := map[string]*goPkg{}
	byDir := map[string]*goPkg{}
	for _, pk := range pkgs {
		byImport[pk.importPath] = pk
		byDir[pk.dir] = pk
	}
	// Reverse edges inside the module.
	rdeps := map[string][]string{}     // import path -> importing packages (non-test)
	testRdeps := map[string][]string{} // import path -> packages whose tests import it
	for _, pk := range pkgs {
		for _, imp := range pk.imports {
			if inModule(imp, modPath) {
				rdeps[imp] = append(rdeps[imp], pk.importPath)
			}
		}
		for _, imp := range pk.testImports {
			if inModule(imp, modPath) && imp != pk.importPath {
				testRdeps[imp] = append(testRdeps[imp], pk.importPath)
			}
		}
	}

	// Owners: compiledOwners changed code that dependents compile against;
	// testOwners changed only what their own tests see.
	type ownerInfo struct {
		reasons  []string
		compiled bool
	}
	owners := map[string]*ownerInfo{} // import path -> info
	addOwner := func(imp, file string, compiled bool) {
		o := owners[imp]
		if o == nil {
			o = &ownerInfo{}
			owners[imp] = o
		}
		o.reasons = append(o.reasons, file)
		o.compiled = o.compiled || compiled
	}
	var migrationFiles []string
	goChanged := false
	for _, f := range changed {
		if isMigrationFile(f) {
			migrationFiles = append(migrationFiles, f)
		}
		if outsideModuleGraph(repoTree, f) {
			continue
		}
		dir := path.Dir(f)
		if strings.HasSuffix(f, ".go") {
			if !strings.HasSuffix(f, "_test.go") {
				goChanged = true
			}
			imp := importPathOf(modPath, dir)
			// A deleted package keeps its import path in the graph so its
			// dependents are still selected.
			addOwner(imp, f, !strings.HasSuffix(f, "_test.go"))
			continue
		}
		// A non-Go file belongs to the nearest enclosing package; under
		// testdata it only affects that package's tests, otherwise it may
		// be embedded and is treated as compiled.
		// The root package owns only files directly in the root, so a
		// docs/ or migrations/ file does not select the whole module.
		for d := dir; ; d = path.Dir(d) {
			if d == "." && dir != "." {
				break
			}
			if pk := byDir[dirKey(d)]; pk != nil {
				addOwner(pk.importPath, f, !hasSegment(f, "testdata"))
				break
			}
			if d == "." || d == "/" || d == "" {
				break
			}
		}
	}

	// Reverse dependents of compiled owners, breadth first in sorted order
	// so each dependent's reason names a deterministic root.
	var ownerImports []string
	for imp := range owners {
		ownerImports = append(ownerImports, imp)
	}
	sort.Strings(ownerImports)
	depReason := map[string]string{} // dependent import path -> reason
	var queue []string
	rootOf := map[string]string{}
	for _, imp := range ownerImports {
		if owners[imp].compiled {
			queue = append(queue, imp)
			rootOf[imp] = imp
		}
	}
	visited := map[string]bool{}
	for _, q := range queue {
		visited[q] = true
	}
	for i := 0; i < len(queue); i++ {
		cur := queue[i]
		next := append([]string{}, rdeps[cur]...)
		sort.Strings(next)
		for _, d := range next {
			if visited[d] {
				continue
			}
			visited[d] = true
			rootOf[d] = rootOf[cur]
			if owners[d] == nil {
				if cur == rootOf[cur] {
					depReason[d] = "reverse dependent of " + dirOf(modPath, rootOf[cur])
				} else {
					depReason[d] = fmt.Sprintf("reverse dependent of %s (via %s)", dirOf(modPath, rootOf[cur]), dirOf(modPath, cur))
				}
			}
			queue = append(queue, d)
		}
	}
	// Packages whose tests import an affected package.
	affected := append([]string{}, queue...)
	sort.Strings(affected)
	for _, a := range affected {
		users := append([]string{}, testRdeps[a]...)
		sort.Strings(users)
		for _, u := range users {
			if owners[u] != nil || depReason[u] != "" {
				continue
			}
			depReason[u] = fmt.Sprintf("tests import %s, affected by %s", dirOf(modPath, a), dirOf(modPath, rootOf[a]))
		}
	}

	// Every target keeps the policy command's non-package arguments and
	// stays within its package patterns: the fast loop is never stricter
	// than the gate.
	testPrefix := goCommand("test", gt.flags, nil, nil)
	testSuffix := strings.Join(gt.trailing, " ")
	testTarget := func(dirs []string, reason string) Target {
		return Target{
			Kind: evidence.CheckKindTest, Command: goCommand("test", gt.flags, dirs, gt.trailing), Scope: dirs,
			Reason: reason, batchPrefix: testPrefix, batchSuffix: testSuffix,
		}
	}
	var targets []Target
	var testTargets []Target
	var vetDirs []string
	for _, imp := range ownerImports {
		pk := byImport[imp]
		if pk == nil || !inPolicy.matches(pk.dir) {
			continue // deleted (its dependents still run) or outside the policy's packages
		}
		files := owners[imp].reasons
		sort.Strings(files)
		testTargets = append(testTargets, testTarget([]string{pk.dir}, "owner of changed file "+strings.Join(files, ", ")))
		vetDirs = append(vetDirs, pk.dir)
	}
	var deps []string
	for d := range depReason {
		if pk := byImport[d]; pk != nil && inPolicy.matches(pk.dir) {
			deps = append(deps, d)
		}
	}
	sort.Strings(deps)
	for _, d := range deps {
		testTargets = append(testTargets, testTarget([]string{byImport[d].dir}, depReason[d]))
	}
	if len(testTargets) > MaxGoTestTargets {
		var dirs []string
		for _, t := range testTargets {
			dirs = append(dirs, t.Scope...)
		}
		testTargets = []Target{testTarget(dirs, fmt.Sprintf("%d impacted packages (owners of the changed files and their reverse dependents) in one invocation", len(dirs)))}
	}
	targets = append(targets, testTargets...)

	// go build and go vet are not run by the gate: advisory unless the
	// policy requires build / lint.
	requiredBuild := p.requires(evidence.CheckKindBuild)
	if goChanged || requiredBuild { // test-only changes are compiled by go test
		reason := "changed Go code: the module must still compile"
		if !goChanged {
			reason = "policy requires build"
		}
		targets = append(targets, Target{
			Kind: evidence.CheckKindBuild, Command: goCommand("build", gt.buildFlags, gt.patterns, nil), Scope: gt.patterns,
			Reason: reason, Advisory: !requiredBuild,
		})
	}
	requiredLint := p.requires(evidence.CheckKindLint)
	switch {
	case len(vetDirs) > 0:
		targets = append(targets, Target{
			Kind: evidence.CheckKindLint, Command: goCommand("vet", gt.vetFlags, vetDirs, nil), Scope: vetDirs,
			Reason: "vet the changed package(s)", Advisory: !requiredLint,
		})
	case requiredLint:
		targets = append(targets, Target{
			Kind: evidence.CheckKindLint, Command: goCommand("vet", gt.vetFlags, gt.patterns, nil), Scope: gt.patterns,
			Reason: "policy requires lint",
		})
	}

	if len(migrationFiles) > 0 {
		var dirs, funcs []string
		for _, pk := range pkgs {
			if len(pk.migrateTests) > 0 && inPolicy.matches(pk.dir) {
				dirs = append(dirs, pk.dir)
				funcs = append(funcs, pk.migrateTests...)
			}
		}
		reason := "changed migration file " + strings.Join(migrationFiles, ", ")
		if len(dirs) > 0 {
			sort.Strings(dirs)
			sort.Strings(funcs)
			flags := append([]string{}, gt.flags...)
			if !gt.hasFlag("v") {
				flags = append(flags, "-v") // tells a run from an all-skipped one
			}
			flags = append(flags, "-run", "Migrat")
			targets = append(targets, Target{
				Kind: evidence.CheckKindMigration, Command: goCommand("test", flags, dirs, gt.trailing), Scope: dirs,
				Reason: reason + "; migration tests detected (" + strings.Join(funcs, ", ") + ")",
			})
		} else {
			targets = append(targets, Target{
				Kind: evidence.CheckKindMigration, Scope: migrationFiles, Reason: reason,
				NotRunReason: "no migration test detected (no Test*Migrat* function in the policy's packages)",
			})
		}
	}
	return targets, nil
}

// loadGoPackages walks the module and parses each package's imports.
func loadGoPackages(ctx context.Context, root, modPath string) ([]*goPkg, error) {
	pkgs := map[string]*goPkg{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel == "." {
				return nil
			}
			name := d.Name()
			if name == "testdata" || name == "vendor" || name == "node_modules" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
				return filepath.SkipDir
			}
			if _, err := os.Stat(filepath.Join(p, "go.mod")); err == nil {
				return filepath.SkipDir // nested module
			}
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			return nil
		}
		src, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		f, err := parser.ParseFile(fset, p, src, parser.ImportsOnly)
		if err != nil {
			return fmt.Errorf("parse error in %s: %v", rel, err)
		}
		dir := path.Dir(rel)
		pk := pkgs[dir]
		if pk == nil {
			pk = &goPkg{dir: dirKey(dir), importPath: importPathOf(modPath, dir)}
			pkgs[dir] = pk
		}
		isTest := strings.HasSuffix(name, "_test.go")
		for _, is := range f.Imports {
			imp, uerr := strconv.Unquote(is.Path.Value)
			if uerr != nil {
				continue
			}
			if isTest {
				pk.testImports = append(pk.testImports, imp)
			} else {
				pk.imports = append(pk.imports, imp)
			}
		}
		if isTest {
			for _, m := range migrateTestFunc.FindAllSubmatch(src, -1) {
				pk.migrateTests = append(pk.migrateTests, string(m[1]))
			}
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("import graph: %w", err)
		}
		return nil, fmt.Errorf("import graph: %v", err)
	}
	out := make([]*goPkg, 0, len(pkgs))
	for _, pk := range pkgs {
		out = append(out, pk)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].dir < out[j].dir })
	return out, nil
}

// goModulePath reads the module directive of go.mod.
func goModulePath(gomod string) (string, error) {
	f, err := os.Open(gomod)
	if err != nil {
		return "", fmt.Errorf("no go.mod at the repository root: %v", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "module") {
			mod := strings.TrimSpace(strings.TrimPrefix(line, "module"))
			if i := strings.Index(mod, "//"); i >= 0 {
				mod = strings.TrimSpace(mod[:i])
			}
			mod = strings.Trim(mod, `"`)
			if mod != "" {
				return mod, nil
			}
		}
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("read go.mod: %v", err)
	}
	return "", errors.New("go.mod has no module directive")
}

func isMigrationFile(f string) bool {
	return strings.HasSuffix(strings.ToLower(f), ".sql") && hasSegment(path.Dir(f), "migrations")
}

func hasSegment(p, seg string) bool {
	for _, s := range strings.Split(p, "/") {
		if s == seg {
			return true
		}
	}
	return false
}

// outsideModuleGraph reports files the graph ignores: under testdata,
// vendor, hidden or underscore directories, or in a nested module. testdata
// files are still attributed to their package by the caller's non-Go path,
// so only .go files there are skipped.
func outsideModuleGraph(root, f string) bool {
	dir := path.Dir(f)
	segs := strings.Split(dir, "/")
	for i, s := range segs {
		if s == "." {
			continue
		}
		if s == "vendor" || s == "node_modules" || strings.HasPrefix(s, ".") || strings.HasPrefix(s, "_") {
			return true
		}
		if s == "testdata" {
			return strings.HasSuffix(f, ".go")
		}
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(strings.Join(segs[:i+1], "/")), "go.mod")); err == nil {
			return true
		}
	}
	return false
}

func inModule(imp, mod string) bool {
	return imp == mod || strings.HasPrefix(imp, mod+"/")
}

func importPathOf(mod, dir string) string {
	if dir == "." || dir == "" {
		return mod
	}
	return mod + "/" + dir
}

// dirOf renders an in-module import path as a package directory.
func dirOf(mod, imp string) string {
	if imp == mod {
		return "."
	}
	return "./" + strings.TrimPrefix(imp, mod+"/")
}

func dirKey(dir string) string {
	if dir == "." || dir == "" {
		return "."
	}
	return "./" + dir
}
