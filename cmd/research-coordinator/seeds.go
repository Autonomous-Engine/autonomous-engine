package main

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Seeds (R2): repository-relative files the intake or the PRD names, with
// the line numbers given next to them. research-agent opens them first
// instead of rediscovering them with codex_find_module / list_files, which
// was most of the wasted calls in an early live run.

// maxSeedFiles caps the seeds sent per task.
const maxSeedFiles = 12

// Where the kept seeds were checked.
const (
	seedSourceClone      = "clone"      // exists in a repository clone
	seedSourceCodex      = "codex"      // no clone on disk; the Codex knows the path
	seedSourceUnverified = "unverified" // nothing to check against; accepted as written
)

// seedCandidate is one path found in the text, with its line numbers.
type seedCandidate struct {
	Path  string
	Lines []int
	// explicit: the text marked it as a path (backticks or a line number),
	// so a bare file name without a directory is accepted.
	explicit bool
}

// seedSet is the validated result carried on every research task.
type seedSet struct {
	Files      []string
	Lines      map[string][]int
	Unverified bool
	Source     string
}

var (
	// seedURL removes URLs first: their paths are not repository paths.
	seedURL = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.\-]*://[^\s)>\]"'` + "`" + `]+`)
	// seedPath finds a path token: optional ./, directory segments, and a
	// file name with an extension, optionally followed by :line or
	// :line-line. Group 1 is the delimiter before the token (Go regexp has
	// no lookbehind), 2 the path, 3 and 4 the line numbers.
	seedPath = regexp.MustCompile(`(^|[\s(\[<"',;:` + "`" + `])((?:\./)*(?:[A-Za-z0-9_@.\-]+/)*[A-Za-z0-9_@.\-]*[A-Za-z0-9_\-]\.[A-Za-z][A-Za-z0-9]{0,7})(?::(\d+)(?:-(\d+))?)?`)
	// seedLRef matches L840, ~L840 and L840-L848 / L840-848.
	seedLRef = regexp.MustCompile(`~?\bL(\d+)(?:\s*[-–]\s*L?(\d+))?\b`)
	// seedLinesWord matches "line 12", "lines 12 and 40", "lines 12-20, 31".
	seedLinesWord = regexp.MustCompile(`(?i)\blines?\s+~?(\d+(?:\s*(?:,|and|&|to|-|–)\s*~?\d+)*)`)
	seedNumber    = regexp.MustCompile(`\d+`)
)

// seedLineWindow bounds how far after a path its line numbers are looked for.
const seedLineWindow = 100

// extractSeedCandidates returns the paths named in texts, in first-seen
// order, de-duplicated, with their line numbers merged and sorted.
func extractSeedCandidates(texts ...string) []seedCandidate {
	var out []seedCandidate
	index := map[string]int{}
	for _, raw := range texts {
		text := strings.ReplaceAll(raw, "\r\n", "\n")
		// Blank URLs out with spaces so offsets and windows stay intact.
		text = seedURL.ReplaceAllStringFunc(text, func(s string) string { return strings.Repeat(" ", len(s)) })
		matches := seedPath.FindAllStringSubmatchIndex(text, -1)
		for i, m := range matches {
			delim := ""
			if m[2] >= 0 {
				delim = text[m[2]:m[3]]
			}
			p := text[m[4]:m[5]]
			end := m[1]
			// The token must end there: not mid-word, not a directory, not
			// part of a dotted name the expression cut short.
			if end < len(text) {
				if c := text[end]; isSeedWordByte(c) || c == '/' || c == '\\' {
					continue
				}
				if text[end] == '.' && end+1 < len(text) && isSeedWordByte(text[end+1]) {
					continue
				}
			}
			var lines []int
			if m[6] >= 0 {
				lines = appendLine(lines, text[m[6]:m[7]])
				if m[8] >= 0 {
					lines = appendLine(lines, text[m[8]:m[9]])
				}
			}
			// Line references written after the path, up to the next path,
			// the end of the line, or the window.
			stop := len(text)
			if i+1 < len(matches) {
				stop = matches[i+1][4]
			}
			if nl := strings.IndexByte(text[end:], '\n'); nl >= 0 && end+nl < stop {
				stop = end + nl
			}
			if end+seedLineWindow < stop {
				stop = end + seedLineWindow
			}
			window := text[end:stop]
			for _, lm := range seedLRef.FindAllStringSubmatch(window, -1) {
				lines = appendLine(lines, lm[1])
				if lm[2] != "" {
					lines = appendLine(lines, lm[2])
				}
			}
			if len(lines) == 0 || m[6] < 0 {
				for _, wm := range seedLinesWord.FindAllStringSubmatch(window, -1) {
					for _, n := range seedNumber.FindAllString(wm[1], -1) {
						lines = appendLine(lines, n)
					}
				}
			}

			p = normalizeSeedPath(p)
			if p == "" {
				continue
			}
			explicit := delim == "`" || len(lines) > 0
			if !strings.Contains(p, "/") && !explicit {
				continue
			}
			if j, ok := index[p]; ok {
				out[j].Lines = mergeLines(out[j].Lines, lines)
				out[j].explicit = out[j].explicit || explicit
				continue
			}
			index[p] = len(out)
			out = append(out, seedCandidate{Path: p, Lines: mergeLines(nil, lines), explicit: explicit})
		}
	}
	return out
}

func isSeedWordByte(c byte) bool {
	return c == '_' || c == '-' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// normalizeSeedPath makes p repository-relative, or returns "" when it
// cannot be one (parent traversal).
func normalizeSeedPath(p string) string {
	for strings.HasPrefix(p, "./") {
		p = p[2:]
	}
	p = strings.TrimLeft(p, "/")
	if p == "" {
		return ""
	}
	clean := path.Clean(p)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return ""
	}
	for _, seg := range strings.Split(clean, "/") {
		if seg == ".." {
			return ""
		}
	}
	return clean
}

func appendLine(lines []int, s string) []int {
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 || n > 1_000_000 {
		return lines
	}
	return append(lines, n)
}

// mergeLines returns the sorted union of a and b.
func mergeLines(a, b []int) []int {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	seen := map[int]bool{}
	var out []int
	for _, n := range append(append([]int(nil), a...), b...) {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Ints(out)
	return out
}

// seedCodex answers whether the Codex mentions a repository path. It is the
// fallback check when no clone is on disk.
type seedCodex interface {
	Knows(path string) bool
}

// validateSeeds keeps the candidates that exist in one of the clones. With
// no clone on disk it keeps those the Codex knows; when there is no Codex
// either, or it knows none of them, it accepts them as written and flags
// them unverified. The result is capped at maxSeedFiles.
func validateSeeds(cands []seedCandidate, clones []string, codex seedCodex) seedSet {
	set := seedSet{Lines: map[string][]int{}}
	keep := func(c seedCandidate) {
		if len(set.Files) >= maxSeedFiles {
			return
		}
		set.Files = append(set.Files, c.Path)
		if len(c.Lines) > 0 {
			set.Lines[c.Path] = c.Lines
		}
	}

	var roots []string
	for _, c := range clones {
		if c == "" {
			continue
		}
		if fi, err := os.Stat(c); err == nil && fi.IsDir() {
			roots = append(roots, c)
		}
	}

	switch {
	case len(roots) > 0:
		set.Source = seedSourceClone
		for _, c := range cands {
			for _, r := range roots {
				if fi, err := os.Stat(filepath.Join(r, filepath.FromSlash(c.Path))); err == nil && fi.Mode().IsRegular() {
					keep(c)
					break
				}
			}
		}
	default:
		if codex != nil {
			for _, c := range cands {
				if codex.Knows(c.Path) {
					keep(c)
				}
			}
			if len(set.Files) > 0 {
				set.Source = seedSourceCodex
				break
			}
		}
		set.Source = seedSourceUnverified
		set.Unverified = len(cands) > 0
		for _, c := range cands {
			keep(c)
		}
	}
	if len(set.Lines) == 0 {
		set.Lines = nil
	}
	return set
}

// seedPaths and seedLineMap flatten candidates (tests and logs).
func seedPaths(cands []seedCandidate) []string {
	var out []string
	for _, c := range cands {
		out = append(out, c.Path)
	}
	return out
}

func seedLineMap(cands []seedCandidate) map[string][]int {
	out := map[string][]int{}
	for _, c := range cands {
		if len(c.Lines) > 0 {
			out[c.Path] = c.Lines
		}
	}
	return out
}

// codexModuleText is the Codex fallback: the product's module names, public
// interfaces and dependencies as one text, searched for each path. The Codex
// has no per-file table, so "knows" means "mentions".
type codexModuleText struct{ text string }

func (c *codexModuleText) Knows(p string) bool {
	return c != nil && p != "" && strings.Contains(c.text, p)
}

// loadCodexSeedIndex reads the product's Codex modules for the fallback, or
// returns nil when there are none or the read fails.
func loadCodexSeedIndex(ctx context.Context, pool *pgxpool.Pool, productID uuid.UUID) seedCodex {
	if pool == nil {
		return nil
	}
	rows, err := pool.Query(ctx, `
		SELECT name || ' ' || COALESCE(public_interface::text, '') || ' ' || COALESCE(array_to_string(dependencies, ' '), '')
		FROM codex_modules WHERE product_id = $1`, productID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var s string
		if rows.Scan(&s) == nil {
			b.WriteString(s)
			b.WriteByte('\n')
		}
	}
	if rows.Err() != nil || b.Len() == 0 {
		return nil
	}
	return &codexModuleText{text: b.String()}
}
