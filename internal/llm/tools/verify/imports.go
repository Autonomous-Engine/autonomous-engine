// Package verify implements cheap, fast pre-checks for generated code —
// the kind of hallucination trap small models fall into before the
// TypeScript/Go compiler ever sees the file. The headline check is
// VerifyImports: extract every import/require, walk up to the nearest
// package.json / go.mod, and flag anything that isn't a built-in, isn't
// declared as a dependency, and doesn't resolve to a file on disk.
//
// This is NOT parser-accurate. It catches the `template/leaflet` and
// `distaps` class of typo in under 200ms and buys us a first-pass sanity
// signal before we reach for the real LSP diagnostics.
package verify

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/jsresolve"
)

// Unresolved describes a single import reference that couldn't be matched
// to a built-in, a declared dependency, or an on-disk file.
type Unresolved struct {
	Import string `json:"import"` // the literal module string the code wrote
	Line   int    `json:"line"`   // 1-based source line
	Reason string `json:"reason"` // one-line human-readable explanation
}

// Result is returned to the caller of VerifyImports.
type Result struct {
	Checked    int          `json:"checked"`
	Unresolved []Unresolved `json:"unresolved"`
	// PackageJSON is the path that was used to resolve packages, empty if
	// none was found walking up from the target file.
	PackageJSON string `json:"package_json,omitempty"`
}

// ─── Regexes (compiled once) ─────────────────────────────────────────────────
//
// We deliberately keep these relaxed and split across multiple patterns.
// A full-grammar parser in Go would cost 10x more code and this file
// ships hallucination-catching in ~200 lines.

var (
	// `import … from 'X'` — covers default, named, namespace, type imports.
	reImportFrom = regexp.MustCompile(`(?m)^\s*import\s+(?:type\s+)?[^'"\n;]*\bfrom\s+['"]([^'"]+)['"]`)
	// `import 'X'` — side-effect import.
	reImportBare = regexp.MustCompile(`(?m)^\s*import\s+['"]([^'"]+)['"]`)
	// `import X = require('Y')` — legacy TS CJS shim.
	reImportEqRequire = regexp.MustCompile(`(?m)^\s*import\s+\w+\s*=\s*require\(['"]([^'"]+)['"]\)`)
	// `const X = require('Y')` / `require('Y')` standalone.
	reRequire = regexp.MustCompile(`\brequire\(['"]([^'"]+)['"]\)`)
	// Dynamic import: `import('X')`.
	reDynamicImport = regexp.MustCompile(`\bimport\(['"]([^'"]+)['"]\)`)
)

// Node built-ins we accept without a package.json entry.
var nodeBuiltins = map[string]struct{}{
	"assert": {}, "async_hooks": {}, "buffer": {}, "child_process": {},
	"cluster": {}, "console": {}, "constants": {}, "crypto": {},
	"dgram": {}, "diagnostics_channel": {}, "dns": {}, "domain": {},
	"events": {}, "fs": {}, "fs/promises": {}, "http": {}, "http2": {},
	"https": {}, "inspector": {}, "module": {}, "net": {}, "os": {},
	"path": {}, "path/posix": {}, "path/win32": {}, "perf_hooks": {},
	"process": {}, "punycode": {}, "querystring": {}, "readline": {},
	"repl": {}, "stream": {}, "stream/promises": {}, "stream/web": {},
	"string_decoder": {}, "sys": {}, "timers": {}, "timers/promises": {},
	"tls": {}, "trace_events": {}, "tty": {}, "url": {}, "util": {},
	"util/types": {}, "v8": {}, "vm": {}, "wasi": {}, "worker_threads": {},
	"zlib": {},
}

// VerifyImports extracts every import/require reference from src and
// checks each one. path is the repo-relative file-path where src would
// live — used to resolve the nearest package.json and to resolve relative
// imports. repoRoot is the absolute filesystem path to the repository
// (the root that path is relative to).
//
// Currently supports TypeScript/JavaScript (.ts/.tsx/.js/.jsx/.mjs/.cjs).
// For unsupported languages we return an empty Result with no error — the
// check is a best-effort net, not a gatekeeper.
func VerifyImports(repoRoot, path, src string) (Result, error) {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs":
		// supported
	default:
		return Result{}, nil
	}

	targets := extractImports(src)
	result := Result{Checked: len(targets)}

	// Find the nearest package.json walking up from path.
	pkgPath, deps := findPackageJSON(repoRoot, path)
	result.PackageJSON = pkgPath

	// Find tsconfig.json / jsconfig.json path aliases (e.g. "@/*" → "./src/*").
	// Loaded once and reused for every import in this file.
	aliases := findTSConfigPaths(repoRoot, path)

	for _, t := range targets {
		imp := t.name
		if imp == "" {
			continue
		}
		// Ignore URL imports (`https://…`) and node:-prefixed specifiers.
		if strings.HasPrefix(imp, "http://") || strings.HasPrefix(imp, "https://") {
			continue
		}
		if strings.HasPrefix(imp, "node:") {
			continue
		}
		// Relative / absolute path → try to resolve on disk.
		if isRelative(imp) {
			if !resolveRelative(repoRoot, path, imp) {
				result.Unresolved = append(result.Unresolved, Unresolved{
					Import: imp, Line: t.line,
					Reason: "relative path does not resolve to any existing file or index",
				})
			}
			continue
		}
		// Path-like absolute inside repo: treat like relative from repo root.
		if strings.HasPrefix(imp, "/") {
			if !resolveRelative(repoRoot, "", strings.TrimPrefix(imp, "/")) {
				result.Unresolved = append(result.Unresolved, Unresolved{
					Import: imp, Line: t.line,
					Reason: "absolute path does not resolve inside repo",
				})
			}
			continue
		}
		// tsconfig.json / jsconfig.json path alias (e.g. "@/components/ui/button").
		// Resolve the alias to a filesystem path before falling through to the
		// package-name check, which would incorrectly treat "@/foo" as a scoped
		// npm package.
		if resolved, ok := resolvePathAlias(repoRoot, imp, aliases); ok {
			if !resolved {
				result.Unresolved = append(result.Unresolved, Unresolved{
					Import: imp, Line: t.line,
					Reason: "path alias resolved but target file does not exist",
				})
			}
			continue
		}
		// Node built-in?
		if _, ok := nodeBuiltins[imp]; ok {
			continue
		}
		// Otherwise it's a package: check deps.
		pkg := packageNameFor(imp)
		if _, ok := deps[pkg]; !ok {
			reason := "not in package.json, not a built-in"
			if pkgPath == "" {
				reason = "no package.json found walking up from the file; cannot verify package"
			}
			result.Unresolved = append(result.Unresolved, Unresolved{
				Import: imp, Line: t.line, Reason: reason,
			})
		}
	}
	return result, nil
}

// extracted is a single import site with its source line.
type extracted struct {
	name string
	line int
}

func extractImports(src string) []extracted {
	out := []extracted{}
	collect := func(re *regexp.Regexp) {
		for _, m := range re.FindAllStringSubmatchIndex(src, -1) {
			if len(m) < 4 {
				continue
			}
			start, end := m[2], m[3]
			if start < 0 || end < 0 {
				continue
			}
			name := src[start:end]
			line := 1 + strings.Count(src[:start], "\n")
			out = append(out, extracted{name: name, line: line})
		}
	}
	collect(reImportFrom)
	collect(reImportBare)
	collect(reImportEqRequire)
	collect(reRequire)
	collect(reDynamicImport)
	return out
}

func isRelative(imp string) bool {
	return strings.HasPrefix(imp, "./") || strings.HasPrefix(imp, "../") || imp == "." || imp == ".."
}

// packageNameFor returns the top-level package name for an import spec.
// "foo" → "foo"; "foo/bar" → "foo"; "@scope/foo" → "@scope/foo";
// "@scope/foo/bar" → "@scope/foo".
func packageNameFor(imp string) string {
	if strings.HasPrefix(imp, "@") {
		parts := strings.SplitN(imp, "/", 3)
		if len(parts) >= 2 {
			return parts[0] + "/" + parts[1]
		}
		return imp
	}
	if i := strings.Index(imp, "/"); i > 0 {
		return imp[:i]
	}
	return imp
}

// findPackageJSON walks up from the directory containing `path` (inside
// repoRoot) looking for package.json. Returns the package path and a set
// of declared dependencies (incl. dev, peer, optional). Returns empty
// results when none is found.
func findPackageJSON(repoRoot, path string) (string, map[string]struct{}) {
	deps := map[string]struct{}{}
	if repoRoot == "" {
		return "", deps
	}
	start := filepath.Dir(filepath.Join(repoRoot, filepath.FromSlash(path)))
	if start == "" {
		start = repoRoot
	}
	absRoot, _ := filepath.Abs(repoRoot)
	dir, _ := filepath.Abs(start)
	for {
		candidate := filepath.Join(dir, "package.json")
		if data, err := os.ReadFile(candidate); err == nil {
			var pkg struct {
				Dependencies         map[string]any `json:"dependencies"`
				DevDependencies      map[string]any `json:"devDependencies"`
				PeerDependencies     map[string]any `json:"peerDependencies"`
				OptionalDependencies map[string]any `json:"optionalDependencies"`
			}
			if err := json.Unmarshal(data, &pkg); err == nil {
				for _, m := range []map[string]any{pkg.Dependencies, pkg.DevDependencies, pkg.PeerDependencies, pkg.OptionalDependencies} {
					for k := range m {
						deps[k] = struct{}{}
					}
				}
				return candidate, deps
			}
			// Malformed package.json — keep walking, the LLM's target may
			// live under a deeper workspace.
		}
		parent := filepath.Dir(dir)
		if parent == dir || !strings.HasPrefix(parent+string(filepath.Separator), absRoot+string(filepath.Separator)) && parent != absRoot {
			return "", deps
		}
		dir = parent
	}
}

// resolveRelative tries Node/TypeScript resolution against the filesystem.
// When path is empty, the import is treated as relative to repoRoot.
// Returns true iff some candidate exists.
func resolveRelative(repoRoot, path, imp string) bool {
	base := repoRoot
	if path != "" {
		base = filepath.Dir(filepath.Join(repoRoot, filepath.FromSlash(path)))
	}
	return resolveRelativeAbs(filepath.Clean(filepath.Join(base, filepath.FromSlash(imp))))
}

// ─── tsconfig.json / jsconfig.json path alias support ───────────────────────

// pathAlias is a single entry from compilerOptions.paths. prefix is the
// literal part before the wildcard ("@/" for "@/*"), and targets are the
// replacement prefixes (e.g. ["./src/"] for ["./src/*"]). baseURL is the
// directory that targets are resolved relative to (the tsconfig's dir when
// baseUrl is not set).
type pathAlias struct {
	prefix  string   // e.g. "@/" — the part before "*"
	targets []string // e.g. ["src/"] — prefix replacements (already joined with baseUrl)
}

// findTSConfigPaths walks up from the directory containing `path` looking
// for tsconfig.json or jsconfig.json, parses compilerOptions.paths, and
// returns a slice of pathAlias entries. Returns nil if no config is found
// or paths is empty. Only wildcard patterns of the form "prefix*" are
// supported (the overwhelmingly common case: "@/*", "~/*", etc.).
func findTSConfigPaths(repoRoot, path string) []pathAlias {
	if repoRoot == "" {
		return nil
	}
	start := filepath.Dir(filepath.Join(repoRoot, filepath.FromSlash(path)))
	if start == "" {
		start = repoRoot
	}
	absRoot, _ := filepath.Abs(repoRoot)
	dir, _ := filepath.Abs(start)

	for {
		for _, name := range []string{"tsconfig.json", "jsconfig.json"} {
			candidate := filepath.Join(dir, name)
			data, err := os.ReadFile(candidate)
			if err != nil {
				continue
			}
			aliases := parseTSConfigPaths(data, dir)
			if len(aliases) > 0 {
				return aliases
			}
			// Found a tsconfig/jsconfig but it had no paths — stop looking.
			return nil
		}
		parent := filepath.Dir(dir)
		if parent == dir ||
			(!strings.HasPrefix(parent+string(filepath.Separator), absRoot+string(filepath.Separator)) && parent != absRoot) {
			return nil
		}
		dir = parent
	}
}

// parseTSConfigPaths extracts compilerOptions.paths from the raw JSON of a
// tsconfig.json / jsconfig.json, using configDir as the base for resolving
// compilerOptions.baseUrl. Only simple wildcard patterns ("prefix*") are
// handled; patterns without a "*" or with multiple wildcards are skipped.
func parseTSConfigPaths(data []byte, configDir string) []pathAlias {
	var cfg struct {
		CompilerOptions struct {
			BaseURL string              `json:"baseUrl"`
			Paths   map[string][]string `json:"paths"`
		} `json:"compilerOptions"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil
	}
	if len(cfg.CompilerOptions.Paths) == 0 {
		return nil
	}

	baseDir := configDir
	if cfg.CompilerOptions.BaseURL != "" {
		baseDir = filepath.Join(configDir, filepath.FromSlash(cfg.CompilerOptions.BaseURL))
	}

	var aliases []pathAlias
	for pattern, mappings := range cfg.CompilerOptions.Paths {
		// We only support the simple "prefix*" form (covers "@/*", "~/*",
		// "lib/*", etc.). Skip patterns without exactly one trailing "*".
		idx := strings.Index(pattern, "*")
		if idx < 0 || idx != len(pattern)-1 {
			continue
		}
		prefix := pattern[:idx] // e.g. "@/" from "@/*"

		var targets []string
		for _, m := range mappings {
			mIdx := strings.Index(m, "*")
			if mIdx < 0 || mIdx != len(m)-1 {
				continue
			}
			// Convert the mapping prefix to be relative to baseDir.
			// e.g. "./src/*" → "src/"
			target := m[:mIdx]
			target = strings.TrimPrefix(target, "./")
			// Resolve relative to baseDir so the caller gets an absolute prefix.
			resolved := filepath.Join(baseDir, filepath.FromSlash(target))
			// Ensure trailing separator so filepath.Join works correctly later.
			if !strings.HasSuffix(resolved, string(filepath.Separator)) {
				resolved += string(filepath.Separator)
			}
			targets = append(targets, resolved)
		}
		if len(targets) > 0 {
			aliases = append(aliases, pathAlias{prefix: prefix, targets: targets})
		}
	}
	return aliases
}

// resolvePathAlias checks whether imp matches any of the configured path
// aliases. If it matches, it resolves the import against the filesystem and
// returns (true, true) if the file exists, (false, true) if the alias
// matched but the file doesn't exist, or (_, false) if no alias matched
// (caller should continue to other resolution strategies).
func resolvePathAlias(repoRoot, imp string, aliases []pathAlias) (resolved bool, matched bool) {
	for _, a := range aliases {
		if !strings.HasPrefix(imp, a.prefix) {
			continue
		}
		// Strip the alias prefix and try each target directory.
		rest := imp[len(a.prefix):]
		for _, target := range a.targets {
			candidate := filepath.Join(target, filepath.FromSlash(rest))
			if resolveRelativeAbs(candidate) {
				return true, true
			}
		}
		// Alias matched but no target resolved — report as matched but
		// unresolved so the caller emits a specific error instead of
		// falling through to the "not in package.json" check.
		return false, true
	}
	return false, false
}

// resolveRelativeAbs resolves an absolute candidate path (the specifier
// already joined onto the importer's directory, or an alias target). The
// candidate list is shared with the JS impact graph (internal/jsresolve):
// the file as written, extension appends, the TypeScript ESM mapping
// (./x.js → x.ts/x.tsx, .mjs → .mts, .cjs → .cts) and directory index
// files. A directory with a package.json also resolves.
func resolveRelativeAbs(candidate string) bool {
	exists := func(p string) bool { return fileExists(filepath.FromSlash(p)) }
	if _, ok := jsresolve.Resolve(filepath.ToSlash(candidate), exists); ok {
		return true
	}
	if info, err := os.Stat(candidate); err == nil && info.IsDir() {
		return fileExists(filepath.Join(candidate, "package.json"))
	}
	return false
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	if err != nil {
		return false
	}
	return !info.IsDir()
}

// Summary returns a short human-readable one-liner for logging.
func (r Result) Summary() string {
	if len(r.Unresolved) == 0 {
		return fmt.Sprintf("%d imports, all resolved", r.Checked)
	}
	names := make([]string, 0, len(r.Unresolved))
	for _, u := range r.Unresolved {
		names = append(names, u.Import)
	}
	return fmt.Sprintf("%d imports, %d unresolved: %s", r.Checked, len(r.Unresolved), strings.Join(names, ", "))
}
