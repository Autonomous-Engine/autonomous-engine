// Package failures normalises check and tool failures into structured
// records (roadmap 3.3, ticket M3-06): file, span, category, ownership and
// retryability, with a fingerprint that is stable across runs, temp
// directories and line shifts. The implementation agent's repair loop
// feeds the model these records instead of raw output, counts how often
// each fingerprint recurs, and stops deterministically when the same
// failure survives a repair attempt.
package failures

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// Category classifies what failed.
type Category string

const (
	CategoryCompile    Category = "compile"
	CategoryTestAssert Category = "test_assert"
	CategoryTestPanic  Category = "test_panic"
	CategoryLint       Category = "lint"
	CategoryTypecheck  Category = "typecheck"
	CategoryRuntime    Category = "runtime"
	CategoryTooling    Category = "tooling"
	CategoryTimeout    Category = "timeout"
	CategoryUnknown    Category = "unknown"
)

// Ownership says whose problem a failure is.
type Ownership string

const (
	// OwnedChangedFile: the failure is in the repository's code — the
	// change under repair is expected to fix it.
	OwnedChangedFile Ownership = "changed_file"
	// OwnedDependency: a third-party package or module is missing or
	// broken (node_modules, the Go module cache, site-packages, an
	// unresolvable import of a non-local package).
	OwnedDependency Ownership = "dependency"
	// OwnedEnvironment: the toolchain, the machine or the network (a
	// command not found, permission denied, disk full, connection refused).
	OwnedEnvironment Ownership = "environment"
)

// Record is one normalised failure.
type Record struct {
	// Fingerprint is the hex sha256 of (category, file, normalised
	// message, test name); see Fingerprint.
	Fingerprint string   `json:"fingerprint"`
	Category    Category `json:"category"`
	// Tool is what produced the output ("pre_commit_checks", "run_command").
	Tool    string `json:"tool,omitempty"`
	Command string `json:"command,omitempty"`
	// ExitCode is the command's exit status; nil when it did not exit
	// (timeout, failed to start).
	ExitCode *int   `json:"exit_code,omitempty"`
	File     string `json:"file,omitempty"`
	Line     int    `json:"line,omitempty"`
	Col      int    `json:"col,omitempty"`
	// Span is a short excerpt of the output around the failure (at most
	// spanMaxLines lines), so the model sees the evidence without the
	// raw log.
	Span    string `json:"span,omitempty"`
	Message string `json:"message"`
	// Test is the failing test's name ("TestAdd/pos",
	// "tests/test_calc.py::test_add", "sum > adds numbers"), "" when the
	// failure is not a test's.
	Test      string    `json:"test,omitempty"`
	Ownership Ownership `json:"ownership"`
	// Retryable: a repair of the repository's code can plausibly fix it
	// (owned by a changed file, and not a tooling failure).
	Retryable bool `json:"retryable"`
	// RawRef points at the stored raw output the record came from.
	RawRef string `json:"raw_ref,omitempty"`
}

// Result is one tool or check result to parse.
type Result struct {
	Tool     string
	Command  string
	ExitCode *int
	Output   string
	RawRef   string
	// TimedOut is set when the command was killed by its timeout.
	TimedOut bool
	// Root is the directory the command ran in (the worktree); absolute
	// file paths under it are made relative.
	Root string
	// Repo describes the repository, so a missing package that is the
	// repository's own (the main Go module's path, a local Python package,
	// a TS path alias) is classified changed_file, not dependency.
	// RepoContextOf fills it from Root.
	Repo RepoContext
}

// RepoContext is what ownership needs to know about the repository.
type RepoContext struct {
	// ModulePath is the main Go module's path (go.mod under Root).
	ModulePath string
	// LocalNames are top-level names a Python/TS import may use for a
	// repository package: the repository's top-level directories, the
	// directories under src/, and the first path element / stem of every
	// planned file.
	LocalNames []string
}

var reGoModule = regexp.MustCompile(`(?m)^module\s+"?([^\s"]+)"?`)

// RepoContextOf reads root's go.mod module path and its top-level
// directories (and those under src/), plus the planned files' first
// elements and stems. Missing files are ignored.
func RepoContextOf(root string, planned []string) RepoContext {
	var rc RepoContext
	if root != "" {
		if b, err := os.ReadFile(filepath.Join(root, "go.mod")); err == nil {
			if m := reGoModule.FindSubmatch(b); m != nil {
				rc.ModulePath = string(m[1])
			}
		}
		for _, dir := range []string{root, filepath.Join(root, "src")} {
			entries, err := os.ReadDir(dir)
			if err != nil {
				continue
			}
			for _, e := range entries {
				if e.IsDir() && !strings.HasPrefix(e.Name(), ".") && e.Name() != "node_modules" && e.Name() != "vendor" {
					rc.LocalNames = append(rc.LocalNames, e.Name())
				}
			}
		}
	}
	for _, p := range planned {
		p = strings.TrimPrefix(path.Clean(strings.ReplaceAll(p, `\`, "/")), "./")
		parts := strings.Split(p, "/")
		if parts[0] == "src" && len(parts) > 1 {
			parts = parts[1:]
		}
		first := parts[0]
		rc.LocalNames = append(rc.LocalNames, strings.TrimSuffix(first, path.Ext(first)))
	}
	return rc
}

func (rc RepoContext) hasLocal(name string) bool {
	for _, n := range rc.LocalNames {
		if n == name {
			return true
		}
	}
	return false
}

const (
	spanMaxLines   = 12
	spanMaxBytes   = 1200
	messageMax     = 400
	unknownFPChars = 200
)

// Parse normalises one failing result into records. lang selects the
// parser ("go", "typescript"/"ts"/"javascript", "python"); "" or an
// unknown value infers it from the command and then tries every parser.
// A result that no parser understands yields exactly one CategoryUnknown
// record whose fingerprint is that of the first 200 normalised characters
// of the output. A result that timed out always carries a CategoryTimeout
// record.
func Parse(lang string, r Result) []Record {
	out := stripANSI(strings.ReplaceAll(r.Output, "\r\n", "\n"))
	var recs []Record
	for _, p := range parsersFor(lang, r.Command) {
		if recs = p(out); len(recs) > 0 {
			break
		}
	}
	if len(recs) == 0 {
		recs = toolingRecords(out)
	}
	recs = goVetCategory(recs, r.Command)
	if r.TimedOut && !hasCategory(recs, CategoryTimeout) {
		recs = append(recs, Record{Category: CategoryTimeout, Message: "command timed out: " + r.Command, Span: tailLines(out, spanMaxLines)})
	}
	if len(recs) == 0 {
		norm := normalizeMessage(out)
		if len(norm) > unknownFPChars {
			norm = norm[:unknownFPChars]
		}
		rec := Record{Category: CategoryUnknown, Message: firstMeaningfulLine(out), Span: tailLines(out, spanMaxLines)}
		rec.Ownership = OwnedChangedFile
		rec.Retryable = true
		rec.Tool, rec.Command, rec.ExitCode, rec.RawRef = r.Tool, r.Command, r.ExitCode, r.RawRef
		rec.Fingerprint = hashParts(string(CategoryUnknown), "", norm, "")
		return []Record{rec}
	}
	seen := map[string]bool{}
	final := recs[:0]
	for _, rec := range recs {
		rec.Tool, rec.Command, rec.ExitCode, rec.RawRef = r.Tool, r.Command, r.ExitCode, r.RawRef
		rec.File = relativize(rec.File, r.Root)
		rec.Message = truncate(strings.TrimSpace(rec.Message), messageMax)
		rec.Span = truncate(rec.Span, spanMaxBytes)
		if rec.Ownership == "" {
			rec.Ownership = classifyOwnership(rec, r.Repo)
		}
		rec.Retryable = rec.Ownership == OwnedChangedFile && rec.Category != CategoryTooling
		rec.Fingerprint = Fingerprint(rec)
		if seen[rec.Fingerprint] {
			continue // one record per distinct failure in a run
		}
		seen[rec.Fingerprint] = true
		final = append(final, rec)
	}
	return final
}

// Fingerprint is the hex sha256 of the record's category, normalised file,
// normalised message (numbers, hex addresses and temp paths stripped) and
// test name. Line and column are deliberately left out: an edit above a
// failure moves it without changing it.
func Fingerprint(r Record) string {
	return hashParts(string(r.Category), fingerprintFile(r.File), normalizeMessage(r.Message), normalizeTest(r.Test))
}

func hashParts(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

type parser func(out string) []Record

func parsersFor(lang, command string) []parser {
	all := map[string]parser{"go": parseGo, "typescript": parseTypeScript, "python": parsePython}
	order := []string{"go", "typescript", "python"}
	first := normalizeLang(lang)
	if first == "" {
		first = langOfCommand(command)
	}
	var ps []parser
	if p, ok := all[first]; ok {
		ps = append(ps, p)
	}
	for _, l := range order {
		if l != first {
			ps = append(ps, all[l])
		}
	}
	return ps
}

func normalizeLang(lang string) string {
	switch strings.ToLower(strings.TrimSpace(lang)) {
	case "go", "golang":
		return "go"
	case "typescript", "ts", "javascript", "js", "node":
		return "typescript"
	case "python", "py":
		return "python"
	}
	return ""
}

var (
	reGoCmd = regexp.MustCompile(`(^|[\s;&|(])go\s+(test|build|vet|run)\b`)
	reTSCmd = regexp.MustCompile(`\b(npm|yarn|pnpm|bun|npx|pnpx|bunx|vitest|jest|mocha|tsc|eslint|node)\b`)
	rePyCmd = regexp.MustCompile(`\b(pytest|python3?|unittest|tox|nox)\b`)
)

func langOfCommand(cmd string) string {
	switch {
	case reGoCmd.MatchString(cmd):
		return "go"
	case reTSCmd.MatchString(cmd):
		return "typescript"
	case rePyCmd.MatchString(cmd):
		return "python"
	}
	return ""
}

// ─── Normalisation ──────────────────────────────────────────────────────────

var (
	reANSI = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)
	// Temp and per-run directories: /tmp/x/..., /var/folders/..., Go build
	// dirs, Windows %TEMP% paths, pytest's tmp_path.
	reTempPath = regexp.MustCompile(`(?i)(/private)?(/var/folders/[^\s:'"]+|/tmp/[^\s:'"]+|[a-z]:[\\/]users[\\/][^\s:'"]*?[\\/]temp[\\/][^\s:'"]+|/go-build\d+[^\s:'"]*|pytest-of-[^\s:'"]+)`)
	reHex      = regexp.MustCompile(`0x[0-9a-fA-F]+`)
	reUUID     = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	reLongHex  = regexp.MustCompile(`\b[0-9a-f]{12,}\b`)
	reNumber   = regexp.MustCompile(`\d+(\.\d+)?`)
	reSpace    = regexp.MustCompile(`\s+`)
)

func stripANSI(s string) string { return reANSI.ReplaceAllString(s, "") }

// normalizeMessage strips what varies between runs of the same failure:
// temp paths, hex addresses, ids and every number (counts, durations,
// goroutine ids, line numbers quoted in messages), and collapses spaces.
func normalizeMessage(s string) string {
	s = strings.ReplaceAll(stripANSI(s), `\`, "/")
	s = reTempPath.ReplaceAllString(s, "<tmp>")
	s = reUUID.ReplaceAllString(s, "<id>")
	s = reHex.ReplaceAllString(s, "<addr>")
	s = reLongHex.ReplaceAllString(s, "<hash>")
	s = reNumber.ReplaceAllString(s, "N")
	s = reSpace.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

func normalizeTest(s string) string { return strings.TrimSpace(reSpace.ReplaceAllString(s, " ")) }

// fingerprintFile is the file as the fingerprint sees it: slash-separated
// and cleaned; an absolute path that could not be made relative to the
// worktree keeps only its last two elements, so the same file under two
// temp directories fingerprints the same.
func fingerprintFile(f string) string {
	if f == "" {
		return ""
	}
	f = path.Clean(strings.ReplaceAll(f, `\`, "/"))
	if isAbs(f) {
		parts := strings.Split(strings.Trim(f, "/"), "/")
		if len(parts) > 2 {
			parts = parts[len(parts)-2:]
		}
		return strings.Join(parts, "/")
	}
	return strings.TrimPrefix(f, "./")
}

func isAbs(f string) bool {
	return strings.HasPrefix(f, "/") || (len(f) > 2 && f[1] == ':' && (f[2] == '/' || f[2] == '\\'))
}

// relativize makes f relative to root when it lies under it, and
// slash-separates it.
func relativize(f, root string) string {
	if f == "" {
		return ""
	}
	f = strings.ReplaceAll(f, `\`, "/")
	if root != "" {
		r := strings.TrimSuffix(strings.ReplaceAll(root, `\`, "/"), "/") + "/"
		if strings.HasPrefix(strings.ToLower(f), strings.ToLower(r)) {
			f = f[len(r):]
		}
	}
	if !isAbs(f) {
		f = strings.TrimPrefix(path.Clean(f), "./")
	}
	return f
}

// ─── Ownership ──────────────────────────────────────────────────────────────

var (
	reDependencyPath = regexp.MustCompile(`(^|/)(node_modules|site-packages|dist-packages|vendor)/|/pkg/mod/|/go/src/|/usr/lib/python|/usr/local/lib/python`)
	reEnvironment    = regexp.MustCompile(`(?i)command not found|: not found$|executable file not found|permission denied|no space left on device|connection refused|could not resolve host|temporary failure in name resolution|network is unreachable|dial tcp|i/o timeout|cannot allocate memory|killed: 9|out of memory`)
	reMissingPkg     = regexp.MustCompile(`(?i)no required module provides package|cannot find module providing package|cannot find package|modulenotfounderror: no module named|no module named|cannot find module '([^'./][^']*)'|cannot find module "([^"./][^"]*)"|could not resolve "([^"./][^"]*)"|failed to resolve import "([^"./][^"]*)"`)
)

func classifyOwnership(r Record, rc RepoContext) Ownership {
	msg := r.Message
	switch {
	case reEnvironment.MatchString(msg) || r.Category == CategoryTooling:
		return OwnedEnvironment
	case r.File != "" && reDependencyPath.MatchString(strings.ReplaceAll(r.File, `\`, "/")):
		return OwnedDependency
	case reMissingPkg.MatchString(msg) && !localModuleRef(msg, rc):
		return OwnedDependency
	}
	return OwnedChangedFile
}

var (
	reGoMissingPkg = regexp.MustCompile(`(?:provides package|providing package|cannot find package) "?([^\s";:]+)`)
	rePyMissingMod = regexp.MustCompile(`No module named '([^']+)'`)
	reJSMissingMod = regexp.MustCompile(`(?:Cannot find module|Could not resolve|Failed to resolve import) ['"]([^'"]+)['"]`)
)

// localModuleRef reports whether a missing-package message names the
// repository's own code — which is the change's problem, not a
// dependency's: a relative import ('./nope'), a TS path alias ('@/x',
// '~/x', '#x'), a package under the main Go module's path, or a Python /
// TS module whose top-level name is a repository directory or a planned
// file.
func localModuleRef(msg string, rc RepoContext) bool {
	if m := reGoMissingPkg.FindStringSubmatch(msg); m != nil {
		pkg := m[1]
		return rc.ModulePath != "" && (pkg == rc.ModulePath || strings.HasPrefix(pkg, rc.ModulePath+"/"))
	}
	if m := rePyMissingMod.FindStringSubmatch(msg); m != nil {
		top := strings.SplitN(m[1], ".", 2)[0]
		return rc.hasLocal(top)
	}
	if m := reJSMissingMod.FindStringSubmatch(msg); m != nil {
		spec := m[1]
		switch {
		case strings.HasPrefix(spec, "./"), strings.HasPrefix(spec, "../"), strings.HasPrefix(spec, "@/"),
			strings.HasPrefix(spec, "~/"), strings.HasPrefix(spec, "#"), strings.HasPrefix(spec, "/"):
			return true
		}
		top := strings.SplitN(spec, "/", 2)[0]
		return !strings.HasPrefix(top, "@") && rc.hasLocal(top)
	}
	return false
}

// ─── Tooling ────────────────────────────────────────────────────────────────

var reTooling = regexp.MustCompile(`(?im)^.*(command not found|: not found$|executable file not found in \$PATH|npm ERR! Missing script: .*|npm error Missing script: .*|No module named pytest|ENOENT: no such file or directory, open .*package\.json.*)$`)

// toolingRecords recognises a run that failed because its tool could not
// run at all.
func toolingRecords(out string) []Record {
	m := reTooling.FindString(out)
	if m == "" {
		return nil
	}
	return []Record{{Category: CategoryTooling, Message: strings.TrimSpace(m), Span: tailLines(out, spanMaxLines), Ownership: OwnedEnvironment}}
}

// ─── Helpers ────────────────────────────────────────────────────────────────

func hasCategory(recs []Record, c Category) bool {
	for _, r := range recs {
		if r.Category == c {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && cut < len(s) && (s[cut]&0xC0) == 0x80 {
		cut-- // do not split a UTF-8 sequence
	}
	return s[:cut] + "…"
}

func lines(s string) []string { return strings.Split(s, "\n") }

func tailLines(s string, n int) string {
	ls := lines(strings.TrimRight(s, "\n"))
	if len(ls) > n {
		ls = ls[len(ls)-n:]
	}
	return strings.Join(ls, "\n")
}

func spanOf(ls []string, from, to int) string {
	if from < 0 {
		from = 0
	}
	if to > len(ls) {
		to = len(ls)
	}
	if to-from > spanMaxLines {
		to = from + spanMaxLines
	}
	if from >= to {
		return ""
	}
	return strings.TrimRight(strings.Join(ls[from:to], "\n"), "\n ")
}

func firstMeaningfulLine(s string) string {
	for _, l := range lines(s) {
		if t := strings.TrimSpace(l); t != "" {
			return truncate(t, messageMax)
		}
	}
	return "(no output)"
}
