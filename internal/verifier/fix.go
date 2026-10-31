package verifier

import (
	"fmt"
	"strings"
)

// R16 G3: the fix subtask a verifier request_changes dispatches carries,
// per problem, the criterion id and text, the defect, and file:line
// references from the verdict — bounded, so the fix round starts from the
// verifier's findings rather than from a re-read of the whole change. The
// original subtask's line seeds for those files travel beside it, as the
// fix task message's line_seeds ("## Where to edit").

// FixBriefMaxChars bounds the fix brief (the fix subtask's description and
// review_feedback).
const FixBriefMaxChars = 4000

// fixBriefMaxSeeds bounds the line seeds a fix brief carries (the
// implementation task message's "## Where to edit" takes at most 8).
const fixBriefMaxSeeds = 8

// Per-field caps inside the brief.
const (
	fixCriterionTextMax = 220
	fixNoteMax          = 300
	fixDefectMax        = 300
	fixReproMax         = 200
)

// LineSeed is one research-located place to edit, as plan_tasks.message
// carries it (line_seeds, R10 W5) and the implementation agent reads it.
type LineSeed struct {
	Path      string `json:"path"`
	LineStart int    `json:"line_start"`
	LineEnd   int    `json:"line_end"`
	Snippet   string `json:"snippet,omitempty"`
	Verified  bool   `json:"verified,omitempty"`
}

// FixSource is one subtask's request_changes verdict and what the fix
// brief needs next to it.
type FixSource struct {
	// TaskID is the subtask whose head the verdict is about.
	TaskID  string
	Verdict Verdict
	// Criteria maps a criterion id to its text (the requirements the
	// verdict was judged against).
	Criteria map[string]string
	// Seeds are the original subtask's line seeds (its own first, then the
	// plan's other subtasks'); only those on files the verdict references
	// are kept.
	Seeds []LineSeed
}

// RenderFixBrief renders the fix subtask's brief from the requesting
// verdicts, at most FixBriefMaxChars long, and returns the line seeds on the
// files the verdicts reference (at most 8) for the fix task message's
// line_seeds. Unmet criteria come first, then critical, major and minor
// defects; items that do not fit are counted, not silently dropped.
func RenderFixBrief(srcs []FixSource) (string, []LineSeed) {
	return RenderFixBriefWithin(srcs, FixBriefMaxChars)
}

// RenderFixBriefWithin is RenderFixBrief bounded to max bytes, for a caller
// that reserves part of the budget for text of its own.
func RenderFixBriefWithin(srcs []FixSource, max int) (string, []LineSeed) {
	var entries []string
	var gaps []string
	seenGap := map[string]bool{}
	files := map[string]bool{}
	var fileOrder []string
	addFile := func(p string) {
		p = normPath(p)
		if p != "" && !files[p] {
			files[p] = true
			fileOrder = append(fileOrder, p)
		}
	}
	for _, s := range srcs {
		task := s.TaskID
		if len(task) > 8 {
			task = task[:8]
		}
		for _, r := range s.Verdict.PerCriterion {
			if r.Status != StatusNotMet {
				continue
			}
			var e strings.Builder
			fmt.Fprintf(&e, "- %s %s", r.ID, strings.ReplaceAll(string(r.Status), "_", " "))
			if task != "" {
				fmt.Fprintf(&e, " (subtask %s)", task)
			}
			if txt := s.Criteria[r.ID]; strings.TrimSpace(txt) != "" {
				fmt.Fprintf(&e, ": %q", cut(oneLine(txt), fixCriterionTextMax))
			}
			e.WriteString("\n")
			if strings.TrimSpace(r.Note) != "" {
				fmt.Fprintf(&e, "  Why: %s\n", cut(oneLine(r.Note), fixNoteMax))
			}
			var refs []string
			for _, ref := range r.Evidence {
				if loc := strings.TrimSpace(string(ref)); fileLineRE.MatchString(loc) {
					refs = append(refs, loc)
					addFile(refPath(ref))
				}
			}
			if len(refs) > 0 {
				fmt.Fprintf(&e, "  At: %s\n", strings.Join(refs, ", "))
			}
			entries = append(entries, e.String())
		}
		for _, sev := range []Severity{SeverityCritical, SeverityMajor, SeverityMinor} {
			for _, d := range s.Verdict.Defects {
				if d.Severity != sev {
					continue
				}
				var e strings.Builder
				loc := d.location()
				if loc == "" {
					loc = "(no file)"
				} else if sev != SeverityMinor {
					addFile(d.File)
				}
				if sev == SeverityMinor {
					fmt.Fprintf(&e, "- [minor, optional] %s — %s: %s\n", loc, oneLine(d.Category), cut(oneLine(d.Description), fixDefectMax))
					entries = append(entries, e.String())
					continue
				}
				fmt.Fprintf(&e, "- [%s] %s — %s: %s\n", d.Severity, loc, oneLine(d.Category), cut(oneLine(d.Description), fixDefectMax))
				if strings.TrimSpace(d.Reproduction) != "" {
					fmt.Fprintf(&e, "  Reproduction: %s\n", cut(oneLine(d.Reproduction), fixReproMax))
				}
				for _, id := range criterionIDRE.FindAllString(d.Category+" "+d.Description, -1) {
					if txt := s.Criteria[id]; strings.TrimSpace(txt) != "" {
						fmt.Fprintf(&e, "  Criterion %s: %q\n", id, cut(oneLine(txt), fixCriterionTextMax))
						break
					}
				}
				entries = append(entries, e.String())
			}
		}
		for _, g := range s.Verdict.Gaps {
			if !seenGap[g] {
				seenGap[g] = true
				gaps = append(gaps, g)
			}
		}
	}

	// The seeds on the referenced files, own subtask's first, deduplicated.
	var seeds []LineSeed
	seenSeed := map[string]bool{}
	for _, s := range srcs {
		for _, sd := range s.Seeds {
			p := normPath(sd.Path)
			key := fmt.Sprintf("%s:%d-%d", p, sd.LineStart, sd.LineEnd)
			if !files[p] || sd.LineStart <= 0 || seenSeed[key] || len(seeds) >= fixBriefMaxSeeds {
				continue
			}
			seenSeed[key] = true
			seeds = append(seeds, sd)
		}
	}

	const head = "INDEPENDENT VERIFIER REQUESTED CHANGES before the PR. Fix each item below in the existing change (critical and major ones first; minor ones only if trivial). The change is verified again after this fix.\n\nProblems:\n"
	var tail strings.Builder
	if len(gaps) > 0 {
		tail.WriteString("\nRecorded gaps (not code work; a human verifies them — do not try to fix them):\n")
		for _, g := range gaps {
			fmt.Fprintf(&tail, "- %s\n", cut(oneLine(g), fixNoteMax))
		}
	}
	// The seeds are not copied into the brief: they travel only as the fix
	// task message's line_seeds, which the implementation agent renders once
	// as its "## Where to edit" (review minor 4). The brief points there.
	var pointer string
	if len(seeds) > 0 {
		pointer = fmt.Sprintf("\nWhere to edit: the task's \"## Where to edit\" lists the original subtask's line seeds on these files (%d).\n", len(seeds))
	}

	// Problems take precedence over the gaps; whatever does not fit is
	// counted, never cut silently.
	const moreReserve = 80
	var b strings.Builder
	b.WriteString(head)
	shown := 0
	for _, e := range entries {
		if b.Len()+len(e)+moreReserve+len(pointer) > max {
			break
		}
		b.WriteString(e)
		shown++
	}
	if shown == 0 && len(entries) == 0 {
		b.WriteString("- (the verdict listed no item; see the verifier's reason)\n")
	}
	if n := len(entries) - shown; n > 0 {
		fmt.Fprintf(&b, "- … %d more item(s) not shown; see the verdict artifact\n", n)
	}
	if tail.Len() > 0 {
		lines := strings.SplitAfter(strings.TrimRight(tail.String(), "\n"), "\n")
		for i, ln := range lines {
			if !strings.HasSuffix(ln, "\n") {
				ln += "\n"
			}
			if b.Len()+len(ln)+len(pointer)+moreReserve > max {
				left := len(lines) - i
				if i == 0 {
					left-- // the heading line
				}
				fmt.Fprintf(&b, "- … %d more recorded gap(s) not shown; see the verdict artifact\n", left)
				break
			}
			b.WriteString(ln)
		}
	}
	if pointer != "" && b.Len()+len(pointer) <= max {
		b.WriteString(pointer)
	}
	out := strings.TrimRight(b.String(), "\n")
	if len(out) > max {
		out = cutBytes(out, max)
	}
	return out, seeds
}

// normPath is a repository-relative path in a comparable form.
func normPath(p string) string {
	p = strings.ReplaceAll(strings.TrimSpace(p), `\`, "/")
	p = strings.TrimPrefix(p, "./")
	return strings.TrimPrefix(p, "/")
}

// cutBytes cuts s to at most n bytes on a rune boundary.
func cutBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }
