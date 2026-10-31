package codex

import (
	"sort"
	"strings"
)

// Typed codex_versions.sections_changed entries (migration 060).
//
// Every entry a version writer records is one of:
//
//	module:<name>   a codex_modules row (by module name) was added or changed
//	section:<key>   a codex_sections row (by section name / onboarding key)
//	patterns        one or more codex_patterns rows
//	anti_patterns   one or more codex_anti_patterns rows
//
// Rows written before migration 060 hold bare values (module names,
// onboarding keys, table names). Readers treat any value without a known
// prefix as module:<value> — the only kind most legacy rows carried — so
// a legacy row can only ever widen, never hide, a module match.
//
// Build entries with ModuleChange / SectionChange / TableChange or the
// ChangePatterns / ChangeAntiPatterns kinds; read them with ParseChange,
// NormalizeChanges, ChangedModules, ChangedSectionKeys and Touches.

// ChangeKind is the type of one sections_changed entry.
type ChangeKind string

const (
	ChangeModule       ChangeKind = "module"
	ChangeSection      ChangeKind = "section"
	ChangePatterns     ChangeKind = "patterns"
	ChangeAntiPatterns ChangeKind = "anti_patterns"
)

const (
	modulePrefix  = "module:"
	sectionPrefix = "section:"
)

// Change is one parsed sections_changed entry. Name is empty for
// ChangePatterns and ChangeAntiPatterns.
type Change struct {
	Kind ChangeKind
	Name string
}

// String is the stored form of c.
func (c Change) String() string {
	switch c.Kind {
	case ChangeModule:
		return modulePrefix + c.Name
	case ChangeSection:
		return sectionPrefix + c.Name
	default:
		return string(c.Kind)
	}
}

// ModuleChange is the entry for a changed module.
func ModuleChange(name string) string { return modulePrefix + name }

// SectionChange is the entry for a changed codex section (onboarding key,
// "codebase_architecture", later "zones").
func SectionChange(key string) string { return sectionPrefix + key }

// ParseChange parses one stored entry. A value without a known prefix (a
// legacy row) is a module of that name.
func ParseChange(s string) Change {
	s = strings.TrimSpace(s)
	switch {
	case s == string(ChangePatterns):
		return Change{Kind: ChangePatterns}
	case s == string(ChangeAntiPatterns):
		return Change{Kind: ChangeAntiPatterns}
	case strings.HasPrefix(s, modulePrefix) && len(s) > len(modulePrefix):
		return Change{Kind: ChangeModule, Name: s[len(modulePrefix):]}
	case strings.HasPrefix(s, sectionPrefix) && len(s) > len(sectionPrefix):
		return Change{Kind: ChangeSection, Name: s[len(sectionPrefix):]}
	default:
		return Change{Kind: ChangeModule, Name: s}
	}
}

// NormalizeChanges returns the typed form of entries: legacy values
// prefixed, empty values dropped, duplicates removed, sorted. Never nil.
func NormalizeChanges(entries []string) []string {
	seen := make(map[string]struct{}, len(entries))
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if strings.TrimSpace(e) == "" {
			continue
		}
		s := ParseChange(e).String()
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// ChangedModules is the sorted, distinct module names among entries
// (legacy un-prefixed values included).
func ChangedModules(entries []string) []string { return namesOf(entries, ChangeModule) }

// ChangedSectionKeys is the sorted, distinct section keys among entries.
func ChangedSectionKeys(entries []string) []string { return namesOf(entries, ChangeSection) }

func namesOf(entries []string, kind ChangeKind) []string {
	var out []string
	for _, e := range NormalizeChanges(entries) {
		if c := ParseChange(e); c.Kind == kind {
			out = append(out, c.Name)
		}
	}
	return out
}

// Touches reports whether any of dependent appears in changed. Both sides
// are parsed with ParseChange, so a bare module name on either side means
// module:<name>.
func Touches(changed, dependent []string) bool {
	set := make(map[string]struct{}, len(changed))
	for _, c := range NormalizeChanges(changed) {
		set[c] = struct{}{}
	}
	for _, d := range NormalizeChanges(dependent) {
		if _, ok := set[d]; ok {
			return true
		}
	}
	return false
}

// TableChange is the entry for a write to one of the codex tables
// (user_interactions.codex_entry_table values): codex_modules and
// codex_sections need the row's name; codex_patterns and
// codex_anti_patterns map to their kind. ok is false for any other table
// or a missing name.
func TableChange(table, name string) (string, bool) {
	switch table {
	case "codex_modules":
		if name == "" {
			return "", false
		}
		return ModuleChange(name), true
	case "codex_sections":
		if name == "" {
			return "", false
		}
		return SectionChange(name), true
	case "codex_patterns":
		return string(ChangePatterns), true
	case "codex_anti_patterns":
		return string(ChangeAntiPatterns), true
	}
	return "", false
}
