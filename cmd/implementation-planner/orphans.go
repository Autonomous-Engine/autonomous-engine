package main

// Orphaned criteria after the repair round (R8). A live run (attempt 8)
// blocked on a criterion ("The \"no sign-up required\" claim in
// `landing/index.html` is corrected.") that the model left out of both the
// decomposition and the repair round, although two tasks already changed
// landing/index.html. Like R7's normaliseDerivedCriteria, the planner fixes
// that deterministically instead of blocking:
//
//   - a criterion owned by no task after the repair round is re-homed to the
//     active step whose files contain every repository path its text names
//     (the step with the fewest files; a tie goes to the lower index), and
//     the re-homing is recorded on the plan (rehomed_criteria). The criterion
//     is an ordinary cited criterion of that step: the verifier judges it
//     against the task's evidence as usual;
//   - a criterion that names no path, or whose paths no single step covers,
//     is recorded as an unowned gap (R3's non-code plumbing: review records
//     it as not_verifiable with the gap line, and the gate escalates);
//   - too many orphans in one plan is a bad plan, and it still blocks.
//
// The repair round's prompt names, per orphan, the paths in its text and the
// steps that change them, so the model can fix it first.

import (
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/plan"
	"github.com/google/uuid"
)

// Orphan thresholds (R8 O3). Re-homing and gap recording repair a model
// that forgot a criterion or two; a plan that leaves more unowned than
// either limit misunderstood the work and is blocked for re-decomposition.
const (
	// maxResolvedOrphans is the most orphaned criteria one plan may have
	// re-homed or recorded as gaps.
	maxResolvedOrphans = 2
	// orphanShareDivisor: the orphans may be at most 1/orphanShareDivisor
	// (a third) of the plan's criteria.
	orphanShareDivisor = 3
)

// rehomedCriterion records one criterion the planner attached to a step by
// file overlap (plan_tasks.message rehomed_criteria, beside R3's
// non_code_criteria).
type rehomedCriterion struct {
	CriterionID string   `json:"criterion_id"`
	TaskIndex   int      `json:"task_index"`
	TaskTitle   string   `json:"task_title,omitempty"`
	Files       []string `json:"files"`
	// Words are the criterion's words the step's title or description
	// shares, when the criterion names no path and was re-homed by wording
	// (wordHome) rather than by file overlap.
	Words []string `json:"words,omitempty"`
}

var (
	backtickSpan = regexp.MustCompile("`([^`\n]+)`")
	// barePath is a slash-separated token (dir/file.ext); the candidates are
	// then held to isRepositoryFilePath.
	barePath     = regexp.MustCompile(`[A-Za-z0-9_@.\-]+(?:[/\\][A-Za-z0-9_@.\-]+)+`)
	lineSuffix   = regexp.MustCompile(`:\d+(?:-\d+)?$`)
	pathTrailing = ".,;:!?)]}'\""
)

// criterionPaths are the repository paths a criterion's text names: every
// backticked span that is a repository file path, and every bare
// dir/file.ext token outside them, normalised and de-duplicated in order.
// URLs, absolute paths, routes ("/api/x") and host paths are not paths.
func criterionPaths(text string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		p = strings.TrimRight(strings.TrimSpace(p), pathTrailing)
		p = lineSuffix.ReplaceAllString(p, "")
		if !isRepositoryFilePath(p) {
			return
		}
		p = plan.NormalizeScopePath(p)
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, m := range backtickSpan.FindAllStringSubmatch(text, -1) {
		add(m[1])
	}
	rest := backtickSpan.ReplaceAllString(text, " ")
	for _, loc := range barePath.FindAllStringIndex(rest, -1) {
		// Not part of a URL or an absolute path.
		if loc[0] > 0 && strings.ContainsRune(":/\\", rune(rest[loc[0]-1])) {
			continue
		}
		add(rest[loc[0]:loc[1]])
	}
	return out
}

// stepScope is the files a step changes, normalised as scope paths are.
func stepScope(s plan.TaskSpecWithFiles) []string {
	var out []string
	seen := map[string]bool{}
	for _, f := range s.Spec.Files {
		p := plan.NormalizeScopePath(f.Path)
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// coveringSteps are the active (not dropped) steps whose files contain
// every path, in step order. None when paths is empty.
func coveringSteps(specs []plan.TaskSpecWithFiles, paths []string) []plan.TaskSpecWithFiles {
	if len(paths) == 0 {
		return nil
	}
	var out []plan.TaskSpecWithFiles
	for _, s := range specs {
		if s.Dropped != "" {
			continue
		}
		scope := map[string]bool{}
		for _, p := range stepScope(s) {
			scope[p] = true
		}
		all := true
		for _, p := range paths {
			if !scope[p] {
				all = false
				break
			}
		}
		if all {
			out = append(out, s)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Index < out[j].Index })
	return out
}

// homeStep is the covering step a re-homed criterion goes to: the one with
// the fewest files (the narrowest change), the lower index on a tie.
func homeStep(cands []plan.TaskSpecWithFiles) plan.TaskSpecWithFiles {
	best := cands[0]
	for _, c := range cands[1:] {
		if n, m := len(stepScope(c)), len(stepScope(best)); n < m || (n == m && c.Index < best.Index) {
			best = c
		}
	}
	return best
}

// joinWords is "a", "a and b", "a, b and c".
func joinWords(xs []string) string {
	switch len(xs) {
	case 0:
		return ""
	case 1:
		return xs[0]
	}
	return strings.Join(xs[:len(xs)-1], ", ") + " and " + xs[len(xs)-1]
}

func stepNumbers(steps []plan.TaskSpecWithFiles) string {
	var ns []string
	for _, s := range steps {
		ns = append(ns, fmt.Sprint(s.Index))
	}
	return joinWords(ns)
}

// criterionByID finds a criterion of the build's (folded) criteria.
func (b *planBuild) criterionByID(id string) (plan.Criterion, bool) {
	if b == nil || b.criteria == nil {
		return plan.Criterion{}, false
	}
	for _, c := range b.criteria.Criteria {
		if c.ID == id {
			return c, true
		}
	}
	return plan.Criterion{}, false
}

// orphanIDs are the ids of the orphan_criterion errors, in order, once each.
func orphanIDs(vs []plan.Violation) []string {
	var ids []string
	seen := map[string]bool{}
	for _, v := range plan.Errors(vs) {
		if v.Code == plan.CodeOrphanCriterion && v.CriterionID != "" && !seen[v.CriterionID] {
			seen[v.CriterionID] = true
			ids = append(ids, v.CriterionID)
		}
	}
	return ids
}

// orphanRepairHints (O4) is, by criterion id, a sentence the repair prompt
// adds under each orphan_criterion violation: the paths the criterion's text
// names and the steps whose files cover them. No hint for a criterion whose
// text names no path.
func orphanRepairHints(b *planBuild, vs []plan.Violation) map[string]string {
	hints := map[string]string{}
	if b == nil {
		return hints
	}
	for _, id := range orphanIDs(vs) {
		c, ok := b.criterionByID(id)
		if !ok {
			continue
		}
		paths := criterionPaths(c.Text)
		if len(paths) == 0 {
			continue
		}
		cands := coveringSteps(b.specs, paths)
		switch len(cands) {
		case 0:
			it := "them"
			if len(paths) == 1 {
				it = "it"
			}
			hints[id] = fmt.Sprintf("%s mentions %s, which no task changes; add %s to the criterion_ids of the task that changes %s, and list %s in that task's files",
				id, joinWords(paths), id, it, it)
		case 1:
			hints[id] = fmt.Sprintf("%s mentions %s, which task %s changes; add %s to its criterion_ids",
				id, joinWords(paths), stepNumbers(cands), id)
		default:
			hints[id] = fmt.Sprintf("%s mentions %s, which tasks %s change; add %s to one of their criterion_ids",
				id, joinWords(paths), stepNumbers(cands), id)
		}
	}
	return hints
}

// resolveOrphans runs after the repair round, on a plan whose every error is
// orphan_criterion. Within the thresholds it re-homes each orphan to the
// step whose files cover every path its text names, records the rest as
// unowned gaps, rebuilds the plan and validates it again; the result may
// still be rejected (for example a re-homed criterion's required check is
// missing on its new step). Above a threshold, or with any other error, the
// plan is returned unchanged; note (a sentence for the block reason) says
// when a threshold was the cause.
func resolveOrphans(workID uuid.UUID, b *planBuild, vs []plan.Violation, build func([]decomposedTask) *planBuild) (*planBuild, []plan.Violation, string) {
	return normaliseOrphans(workID, b, vs, build, true)
}

// rehomeOrphans is orphan_criterion's normaliser before a re-prompt (R9 P1:
// normalise codes are normalised before any re-prompt). It applies only the
// unambiguous part of resolveOrphans: within the same thresholds, each
// orphan a step's files cover is re-homed to that step, whatever other
// errors the plan has (the model is re-prompted for those anyway, and is
// shown the re-homed criterion in the plan it is asked to correct). An
// orphan no step covers is left to the model, with its hint; it becomes a
// gap only after the last round. The plan is rebuilt and validated again
// when anything was re-homed.
func rehomeOrphans(workID uuid.UUID, b *planBuild, vs []plan.Violation, build func([]decomposedTask) *planBuild) (*planBuild, []plan.Violation) {
	nb, nvs, _ := normaliseOrphans(workID, b, vs, build, false)
	return nb, nvs
}

// normaliseOrphans is resolveOrphans (final: after the last round, every
// error an orphan, gaps recorded, the threshold note) and rehomeOrphans
// (not final).
func normaliseOrphans(workID uuid.UUID, b *planBuild, vs []plan.Violation, build func([]decomposedTask) *planBuild, final bool) (*planBuild, []plan.Violation, string) {
	errs := plan.Errors(vs)
	if b == nil || b.criteria == nil || len(errs) == 0 {
		return b, vs, ""
	}
	if final {
		for _, v := range errs {
			if v.Code != plan.CodeOrphanCriterion {
				return b, vs, ""
			}
		}
	}
	ids := orphanIDs(vs)
	if len(ids) == 0 {
		return b, vs, ""
	}
	total := len(b.criteria.Criteria)
	if len(ids) > maxResolvedOrphans || len(ids)*orphanShareDivisor > total {
		if !final {
			return b, vs, ""
		}
		note := fmt.Sprintf(" %d of the plan's %d criteria are owned by no task after the repair round (%s): more than the planner re-homes or records as gaps (at most %d, and at most a third of the criteria).",
			len(ids), total, strings.Join(ids, ", "), maxResolvedOrphans)
		log.Printf("implementation-planner: work_id=%s:%s", workID, note)
		return b, vs, note
	}

	raw := cloneRaw(b.raw)
	byIndex := map[int]int{}
	for i, t := range b.raw {
		byIndex[t.index] = i
	}
	var rehomed []rehomedCriterion
	var unowned []string
	unownedWhy := map[string]string{}
	for _, id := range ids {
		c, ok := b.criterionByID(id)
		if !ok {
			return b, vs, ""
		}
		paths := criterionPaths(c.Text)
		cands := coveringSteps(b.specs, paths)
		if len(cands) == 0 && !final {
			continue
		}
		if len(cands) == 0 {
			why := "its text names no repository path"
			if len(paths) > 0 {
				why = "no task changes all of " + strings.Join(paths, ", ")
			} else if home, words, tied, ok := wordHome(b.raw, b.specs, c.Text); ok {
				// Live (ciblame AC-3 "Rows are sorted chronologically by
				// date"): a path-less orphan the step "Sort the day rows"
				// was plainly for became a gap and a human escalation. The
				// step whose wording shares the criterion's words owns it;
				// the verifier judges it there, and an unmet criterion is
				// a fix round, not an escalation.
				i, ok := byIndex[home.Index]
				if !ok {
					return b, vs, ""
				}
				raw[i].CriterionIDs = append(raw[i].CriterionIDs, id)
				rehomed = append(rehomed, rehomedCriterion{CriterionID: id, TaskIndex: home.Index, TaskTitle: home.Title, Words: words})
				log.Printf("implementation-planner: work_id=%s: criterion %s re-homed to task %d by wording (%s)", workID, id, home.Index, strings.Join(words, ", "))
				continue
			} else if len(tied) > 1 {
				var ns []string
				for _, n := range tied {
					ns = append(ns, fmt.Sprint(n))
				}
				why += ", and steps " + joinWords(ns) + " share its wording equally"
			} else {
				why += ", and no step shares its wording"
			}
			log.Printf("implementation-planner: work_id=%s: criterion %s is unowned after planning (%s) — recorded as a gap for review", workID, id, why)
			unowned = append(unowned, id)
			unownedWhy[id] = why
			continue
		}
		home := homeStep(cands)
		i, ok := byIndex[home.Index]
		if !ok {
			return b, vs, ""
		}
		raw[i].CriterionIDs = append(raw[i].CriterionIDs, id)
		rehomed = append(rehomed, rehomedCriterion{CriterionID: id, TaskIndex: home.Index, TaskTitle: home.Title, Files: paths})
		log.Printf("implementation-planner: work_id=%s: criterion %s re-homed to task %d by file overlap (%s)", workID, id, home.Index, strings.Join(paths, ", "))
	}

	if len(rehomed) == 0 && len(unowned) == 0 {
		return b, vs, ""
	}
	nb := build(raw)
	if nb == nil || nb.noRepository || len(nb.planned) == 0 {
		return b, vs, ""
	}
	// What this build already carried (an earlier normaliser of the same
	// round) is kept.
	nb.rehomed = append(append([]rehomedCriterion(nil), b.rehomed...), rehomed...)
	nb.unowned = append(append([]string(nil), b.unowned...), unowned...)
	nb.unownedWhy = map[string]string{}
	for id, why := range b.unownedWhy {
		nb.unownedWhy[id] = why
	}
	for id, why := range unownedWhy {
		nb.unownedWhy[id] = why
	}
	nb.attached = append([]attachedCheck(nil), b.attached...)
	nvs := nb.validate()
	countViolations(nvs)
	when := "after re-homing"
	if !final {
		when = "after re-homing, before the repair round"
	}
	for _, v := range nvs {
		log.Printf("implementation-planner: work_id=%s: plan validation %s: %s [%s] %s", workID, when, v.Severity, v.Code, v.Message)
	}
	return nb, nvs, ""
}

// unownedViolations turns the orphan_criterion errors of the criteria the
// planner recorded as unowned into non_code_criterion warnings carrying the
// gap line: they no longer reject the plan, and the warning says why.
func (b *planBuild) unownedViolations(vs []plan.Violation) []plan.Violation {
	if len(b.unowned) == 0 {
		return vs
	}
	gap := map[string]bool{}
	for _, id := range b.unowned {
		gap[id] = true
	}
	out := make([]plan.Violation, 0, len(vs))
	for _, v := range vs {
		if v.Code == plan.CodeOrphanCriterion && gap[v.CriterionID] {
			c, _ := b.criterionByID(v.CriterionID)
			c.Unowned = true
			c.NonCodeReason = b.unownedWhy[v.CriterionID]
			out = append(out, plan.Violation{Code: plan.CodeNonCodeCriterion, Severity: plan.SeverityWarning, CriterionID: v.CriterionID, Message: plan.UnownedGap(c)})
			continue
		}
		out = append(out, v)
	}
	return out
}

// Re-homing by wording (a path-less orphan after the last round).

// minSharedWords is how many of a criterion's words a step's wording must
// share to own the criterion: one shared word is a coincidence.
const minSharedWords = 2

// wordStopList are words too common in criteria and plan text to tie one
// to the other.
var wordStopList = map[string]bool{
	"the": true, "and": true, "for": true, "are": true, "but": true, "not": true, "with": true, "that": true,
	"this": true, "from": true, "into": true, "onto": true, "when": true, "than": true, "then": true, "each": true,
	"all": true, "any": true, "one": true, "two": true, "new": true, "via": true, "per": true, "its": true,
	"has": true, "have": true, "had": true, "can": true, "may": true, "must": true, "should": true, "will": true,
	"also": true, "only": true, "same": true, "such": true, "there": true, "their": true, "they": true, "them": true,
	"after": true, "before": true, "being": true, "been": true, "does": true, "did": true, "done": true, "every": true,
	"some": true, "more": true, "most": true, "other": true, "over": true, "under": true, "out": true, "off": true,
	"use": true, "used": true, "uses": true, "using": true, "set": true, "get": true, "add": true, "adds": true,
	"added": true, "make": true, "makes": true, "made": true, "run": true, "runs": true, "ran": true, "work": true,
	"works": true, "code": true, "task": true, "step": true, "file": true, "files": true, "change": true,
	"changes": true, "changed": true, "update": true, "updates": true, "updated": true, "implement": true,
	"implements": true, "implemented": true, "ensure": true, "ensures": true, "support": true, "supports": true,
	"supported": true, "user": true, "users": true, "value": true, "values": true, "field": true, "fields": true,
	"function": true, "functions": true, "method": true, "methods": true, "test": true, "tests": true,
	"tested": true, "return": true, "returns": true, "returned": true, "create": true, "creates": true,
	"created": true, "remove": true, "removes": true, "removed": true, "show": true, "shows": true, "shown": true,
	"like": true, "data": true, "type": true, "types": true, "name": true, "names": true, "named": true,
	"list": true, "lists": true, "existing": true, "current": true, "correct": true, "correctly": true,
	"properly": true, "true": true, "false": true, "nil": true, "null": true,
}

// contentWords are the content words of text: lower-case runs of ASCII
// letters and digits of three or more characters that are not stop words,
// once each in order.
func contentWords(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	}) {
		if len(w) < 3 || wordStopList[w] || seen[w] {
			continue
		}
		seen[w] = true
		out = append(out, w)
	}
	return out
}

// wordsMatch reports whether two content words are the same word in another
// form: one is a prefix of the other (sorted/sort, chronologically/
// chronological, date/dates). A crude stem, enough for plan text.
func wordsMatch(a, b string) bool {
	if len(a) > len(b) {
		a, b = b, a
	}
	return len(a) >= 3 && strings.HasPrefix(b, a)
}

// sharedWords are the words of criterion that step matches, in the
// criterion's order.
func sharedWords(criterion, step []string) []string {
	var out []string
	for _, c := range criterion {
		for _, s := range step {
			if wordsMatch(c, s) {
				out = append(out, c)
				break
			}
		}
	}
	return out
}

// wordHome is the active step whose title and description share the most
// of a criterion's words — at least minSharedWords, and more than any other
// step. ok is false when no step qualifies; tied then names the steps that
// share the best score when that score is reached by more than one (the
// planner does not take a coin flip).
func wordHome(raw []decomposedTask, specs []plan.TaskSpecWithFiles, text string) (home plan.TaskSpecWithFiles, words []string, tied []int, ok bool) {
	criterion := contentWords(text)
	if len(criterion) < minSharedWords {
		return home, nil, nil, false
	}
	byIndex := map[int]decomposedTask{}
	for _, t := range raw {
		byIndex[t.index] = t
	}
	best := 0
	for _, s := range specs {
		if s.Dropped != "" {
			continue
		}
		t := byIndex[s.Index]
		shared := sharedWords(criterion, contentWords(t.Title+" "+t.Description))
		switch n := len(shared); {
		case n < minSharedWords || n < best:
			continue
		case n == best:
			tied = append(tied, s.Index)
		default:
			best, home, words, tied = n, s, shared, []int{s.Index}
		}
	}
	if best == 0 || len(tied) != 1 {
		return home, nil, tied, false
	}
	return home, words, nil, true
}
