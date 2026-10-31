package contracts

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Reading a materialised tree. Everything here is a plain directory walk:
// the checkers are deterministic text and JSON analysis, no build, no
// network.

// skipDirs are never descended into.
var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, "dist": true, "build": true, ".next": true,
}

// maxFileBytes bounds what one file read takes; larger files (bundles,
// fixtures) are not source a consumer reference would live in.
const maxFileBytes = 2 << 20

// sourceFile is one file of a tree: its slash path relative to the root and
// its lines.
type sourceFile struct {
	rel   string
	lines []string
}

// walkFiles returns the files under root whose extension is in exts (all
// files when exts is nil), sorted by path.
func walkFiles(root string, exts map[string]bool) []string {
	if root == "" {
		return nil
	}
	var out []string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path != root && skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if exts != nil && !exts[strings.ToLower(filepath.Ext(d.Name()))] {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return nil
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	sort.Strings(out)
	return out
}

// readSource reads root/rel; ok is false for an unreadable or oversized
// file.
func readSource(root, rel string) (sourceFile, bool) {
	p := filepath.Join(root, filepath.FromSlash(rel))
	info, err := os.Stat(p)
	if err != nil || info.Size() > maxFileBytes {
		return sourceFile{}, false
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return sourceFile{}, false
	}
	s := strings.ReplaceAll(string(b), "\r\n", "\n")
	return sourceFile{rel: rel, lines: strings.Split(s, "\n")}, true
}

// readSources reads every file under root with one of exts.
func readSources(root string, exts map[string]bool) []sourceFile {
	var out []sourceFile
	for _, rel := range walkFiles(root, exts) {
		if f, ok := readSource(root, rel); ok {
			out = append(out, f)
		}
	}
	return out
}

// consumerExts are the source files a consumer reference is looked for in.
var consumerExts = map[string]bool{
	".go": true, ".ts": true, ".tsx": true, ".js": true, ".jsx": true, ".mjs": true, ".cjs": true,
	".py": true, ".java": true, ".kt": true, ".rb": true, ".cs": true, ".php": true, ".rs": true,
	".sql": true, ".vue": true, ".svelte": true,
}

// location renders file:line: text for a consumer reference.
func location(f sourceFile, idx int) string {
	text := strings.TrimSpace(f.lines[idx])
	if r := []rune(text); len(r) > 120 {
		text = string(r[:119]) + "…"
	}
	return fmt.Sprintf("%s:%d: %s", f.rel, idx+1, text)
}

// fieldRefPattern matches a use of field name as a consumer would write it:
// a property access (.email), an index (["email"]), a struct tag
// (json:"email"), a destructuring ({ email } =) or a quoted key ("email").
func fieldRefPattern(name string) *regexp.Regexp {
	q := regexp.QuoteMeta(name)
	return regexp.MustCompile(
		`\.` + q + `\b` +
			`|\[\s*['"\x60]` + q + `['"\x60]\s*\]` +
			`|json:"` + q + `[",]` +
			`|\{[^}]*\b` + q + `\b[^}]*\}\s*=` +
			`|['"]` + q + `['"]`)
}

// findFieldUse returns the first line of f that uses field name.
func findFieldUse(f sourceFile, name string) (int, bool) {
	re := fieldRefPattern(name)
	for i, l := range f.lines {
		if re.MatchString(l) {
			return i, true
		}
	}
	return 0, false
}

// normalizeName is the loose comparison form of an event or payload name:
// lower case, alphanumerics only, without a Topic/Event prefix or a
// Payload/Message/Event/Data/Schema suffix. "user.created",
// "TopicUserCreated", "UserCreatedPayload" and "user-created.schema" all
// become "usercreated".
func normalizeName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	n := b.String()
	for _, p := range []string{"topic", "event"} {
		if strings.HasPrefix(n, p) && len(n) > len(p) {
			n = strings.TrimPrefix(n, p)
		}
	}
	for changed := true; changed; {
		changed = false
		for _, suf := range []string{"schema", "payload", "message", "event", "data", "json"} {
			if strings.HasSuffix(n, suf) && len(n) > len(suf) {
				n = strings.TrimSuffix(n, suf)
				changed = true
			}
		}
	}
	return n
}

// fieldSet is a payload or response schema: field name -> type.
type fieldSet map[string]string

// diffFields lists what head lost or retyped relative to base, sorted.
func diffFields(base, head fieldSet) (removed, retyped, added []string) {
	for name, t := range base {
		ht, ok := head[name]
		switch {
		case !ok:
			removed = append(removed, name)
		case t != "" && ht != "" && t != ht:
			retyped = append(retyped, name)
		}
	}
	for name := range head {
		if _, ok := base[name]; !ok {
			added = append(added, name)
		}
	}
	sort.Strings(removed)
	sort.Strings(retyped)
	sort.Strings(added)
	return removed, retyped, added
}

// fieldChanges renders each removed or retyped field as a producer change,
// pairing a single removal with a single addition as a rename. what names
// the contract surface ("GET /api/users/{id} response", "user.created
// payload"); where is the producer file that defines it.
func fieldChanges(base, head fieldSet, what, where string) map[string]string {
	removed, retyped, added := diffFields(base, head)
	out := map[string]string{}
	if len(removed) == 1 && len(added) == 1 {
		out[removed[0]] = fmt.Sprintf("renamed field %s to %s in the %s (%s)", removed[0], added[0], what, where)
	} else {
		for _, f := range removed {
			out[f] = fmt.Sprintf("removed field %s from the %s (%s)", f, what, where)
		}
	}
	for _, f := range retyped {
		out[f] = fmt.Sprintf("changed the type of field %s from %s to %s in the %s (%s)", f, base[f], head[f], what, where)
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// maxMismatches bounds how many consumer references one contract reports.
const maxMismatches = 10

// codexConsumerFile is the consumer file the codex recorded for the
// contract (Ref.ConsumerFile), read from the consumer head, with the 0-based
// line to report (the recorded line when it is in range, else the first).
func codexConsumerFile(root string, ref Ref) (sourceFile, int, bool) {
	if root == "" || strings.TrimSpace(ref.ConsumerFile) == "" {
		return sourceFile{}, 0, false
	}
	rel := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(ref.ConsumerFile)), "./")
	if strings.HasPrefix(rel, "../") || filepath.IsAbs(rel) {
		return sourceFile{}, 0, false
	}
	f, ok := readSource(root, rel)
	if !ok {
		return sourceFile{}, 0, false
	}
	line := 0
	if ref.ConsumerLine > 0 && ref.ConsumerLine <= len(f.lines) {
		line = ref.ConsumerLine - 1
	}
	return f, line, true
}

func hasFile(fs []sourceFile, rel string) bool {
	for _, f := range fs {
		if f.rel == rel {
			return true
		}
	}
	return false
}
