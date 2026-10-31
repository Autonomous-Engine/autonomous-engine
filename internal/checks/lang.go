package checks

import (
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// runnerMismatchError is the error an Impact returns when the gate's test
// command is not its language's runner (a Go repository gated by
// `npm test`, any repository gated by `make test`). The fast loop may never
// be stricter than the gate, so that language contributes no test target;
// the targets returned alongside the error (typecheck, lint, compile —
// advisory unless the policy requires them) are still used. When no
// language's runner is the gate's command the plan falls back to the full
// suite with the mismatch as the reason.
type runnerMismatchError struct{ msg string }

func (e *runnerMismatchError) Error() string { return e.msg }

func runnerMismatch(format string, args ...any) error {
	return &runnerMismatchError{msg: fmt.Sprintf(format, args...)}
}

// prefixLanguage names the language in a reason when several apply.
func prefixLanguage(n int, lang, msg string) string {
	if n <= 1 {
		return msg
	}
	return lang + ": " + msg
}

// hasShellMeta reports shell syntax a plain runner invocation never needs:
// chaining, pipes, redirection, substitution. A command with it is not
// narrowed (its exit status or test set is the shell's, not the runner's).
func hasShellMeta(cmd string) bool {
	return strings.ContainsAny(cmd, ";|&`$<>\n()")
}

// shellSafe is the set of characters an argument may contain unquoted.
var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_./@%+=:,-]+$`)

// shQuote renders s as one POSIX shell word: unchanged when it only has
// safe characters, else single-quoted (an embedded single quote closes the
// quoting, is backslash-escaped, and reopens it). Fast-check commands run under `sh -c`, so a path with spaces,
// brackets, parentheses or quotes stays one literal argument.
func shQuote(s string) string {
	if shellSafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// isEnvAssignment reports a leading VAR=value word.
func isEnvAssignment(tok string) bool {
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

// binName is a command word's program name: base name without a Windows
// launcher extension.
func binName(tok string) string {
	b := path.Base(strings.ReplaceAll(tok, "\\", "/"))
	for _, ext := range []string{".cmd", ".exe", ".ps1", ".bat"} {
		b = strings.TrimSuffix(b, ext)
	}
	return b
}

// globRegexp compiles a workspace / file glob ("packages/*", "apps/**",
// "test_*.py") to an anchored regexp over slash paths: ** spans
// directories, * and ? do not.
func globRegexp(glob string) (*regexp.Regexp, error) {
	g := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(glob), "./"), "/")
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(g); i++ {
		c := g[i]
		switch {
		case c == '*' && i+1 < len(g) && g[i+1] == '*':
			i++
			if i+1 < len(g) && g[i+1] == '/' {
				i++
				b.WriteString("(?:.*/)?")
			} else {
				b.WriteString(".*")
			}
		case c == '*':
			b.WriteString("[^/]*")
		case c == '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

// walkRepo calls visit for every file under root (slash, repo-relative),
// skipping node_modules, hidden directories and any directory skip
// reports. Directories are visited in lexical order (WalkDir), so callers
// see a deterministic sequence.
func walkRepo(root string, skip func(rel, name string) bool, visit func(rel string) error) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel == "." {
				return nil
			}
			name := d.Name()
			if name == "node_modules" || strings.HasPrefix(name, ".") || (skip != nil && skip(rel, name)) {
				return filepath.SkipDir
			}
			return nil
		}
		return visit(rel)
	})
}

// within reports whether rel is dir or below it ("." contains everything).
func within(rel, dir string) bool {
	return dir == "." || rel == dir || strings.HasPrefix(rel, dir+"/")
}

// relTo renders rel relative to dir (which contains it).
func relTo(rel, dir string) string {
	if dir == "." {
		return rel
	}
	return strings.TrimPrefix(rel, dir+"/")
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// reachInfo is why a file is affected: the changed file the reach started
// from and the file through which it arrived ("" when it is the changed
// file itself or imports it directly).
type reachInfo struct {
	root string
	via  string
}

// propagate walks reverse edges from the seeds in sorted order, breadth
// first, so each affected node's reachInfo names a deterministic root.
// next returns the importers of a node (sorted by the caller or here).
func propagate(seeds []string, importers func(node string) []string) map[string]reachInfo {
	out := map[string]reachInfo{}
	seeds = append([]string{}, seeds...)
	sort.Strings(seeds)
	var queue []string
	for _, s := range seeds {
		if _, ok := out[s]; ok {
			continue
		}
		out[s] = reachInfo{root: s}
		queue = append(queue, s)
	}
	for i := 0; i < len(queue); i++ {
		cur := queue[i]
		ci := out[cur]
		next := append([]string{}, importers(cur)...)
		sort.Strings(next)
		for _, n := range next {
			if _, ok := out[n]; ok {
				continue
			}
			via := ""
			if cur != ci.root {
				via = cur
			}
			out[n] = reachInfo{root: ci.root, via: via}
			queue = append(queue, n)
		}
	}
	return out
}
