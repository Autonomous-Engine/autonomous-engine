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
			}
			log.Printf("implementation-planner: work_id=%s: criterion %s is unowned after planning (%s) — recorded as a gap for review", workID, id, why)
			unowned = append(unowned, id)
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
			out = append(out, plan.Violation{Code: plan.CodeNonCodeCriterion, Severity: plan.SeverityWarning, CriterionID: v.CriterionID, Message: plan.UnownedGap(c)})
			continue
		}
		out = append(out, v)
	}
	return out
}
