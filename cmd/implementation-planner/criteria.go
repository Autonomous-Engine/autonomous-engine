package main

// Acceptance criteria and task specs in the plan (M2-04).
//
// The brief's criteria (research-agent extracted them, research-coordinator
// merged them) go into the decomposition prompt. Each task the model returns
// must name the criterion ids it serves, its deliverable, its verification
// steps, and its files as {path, evidence_ref}. Parsing is strict about
// kinds: an unknown evidence_ref kind, check kind or risk, or a derived
// criterion without a reason, is rejected and re-prompted once; a second
// failure blocks the item with the error. Cross-task rules (every criterion
// covered, every cited id known) are M2-05's validator, not this file's.
//
// Each repo-scoped subtask gets a plan.TaskSpec merged from its steps and the
// criteria it cites; both ride on implementation.tasks and so in
// plan_tasks.message. The description keeps a readable rendering for agents
// and dashboards that do not read the struct. The os.Stat grounding is kept
// and now produces evidence: an existing path is repo evidence (or the
// brief's codex/lsp/repo evidence), a missing one is a hypothesis with the
// grounding note — recorded in the TaskSpec, never silently dropped.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/plan"
)

// fileClaim is one entry of a decomposed task's "files": an object
// {path, evidence_ref} or, from an older prompt or a weaker model, a bare
// path string (no claim: it is grounded like an unevidenced path).
type fileClaim struct {
	Path        string `json:"path"`
	EvidenceRef string `json:"evidence_ref,omitempty"`
}

func (c *fileClaim) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		*c = fileClaim{Path: s}
		return nil
	}
	type alias fileClaim
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return fmt.Errorf("files entry must be a path or {path, evidence_ref}: %w", err)
	}
	*c = fileClaim(a)
	return nil
}

// derivedCriterion is a criterion the planner adds because the change needs
// one the brief lacks. Only derived, only with a reason.
type derivedCriterion struct {
	ID             string               `json:"id"`
	Text           string               `json:"text"`
	Reason         string               `json:"reason"`
	Risk           plan.Risk            `json:"risk"`
	RequiredChecks []evidence.CheckKind `json:"required_checks,omitempty"`
}

func (d derivedCriterion) criterion() plan.Criterion {
	return plan.Criterion{
		ID:             d.ID,
		Text:           d.Text,
		Source:         plan.Source{Kind: plan.SourceDerived, Reason: d.Reason},
		Risk:           d.Risk,
		RequiredChecks: d.RequiredChecks,
	}
}

// UnmarshalJSON decodes a task and fills Files (the plain paths every older
// code path reads) from the file claims.
func (t *decomposedTask) UnmarshalJSON(data []byte) error {
	type alias decomposedTask
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*t = decomposedTask(a)
	// R9: attached_by is the planner's mark (attachChecks); a model cannot
	// claim it for its own steps.
	for i := range t.Verification {
		t.Verification[i].AttachedBy = ""
	}
	t.Files = nil
	for _, c := range t.FileClaims {
		if p := strings.TrimSpace(c.Path); p != "" {
			t.Files = append(t.Files, p)
		}
	}
	return nil
}

// claims returns the task's file claims; a task built without them (tests,
// older payloads) claims each of its Files with no evidence.
func (t decomposedTask) claims() []fileClaim {
	if len(t.FileClaims) > 0 {
		return t.FileClaims
	}
	out := make([]fileClaim, 0, len(t.Files))
	for _, f := range t.Files {
		out = append(out, fileClaim{Path: f})
	}
	return out
}

// parseEvidenceClaim reads an evidence_ref: "" or "hypothesis" (no
// evidence), "new", or "<codex|lsp|repo>:<ref>".
func parseEvidenceClaim(s string) (plan.EvidenceKind, string, error) {
	s = strings.TrimSpace(s)
	switch s {
	case "", string(plan.EvidenceHypothesis):
		return plan.EvidenceHypothesis, "", nil
	case string(plan.EvidenceNew):
		return plan.EvidenceNew, "", nil
	}
	kind, ref, ok := strings.Cut(s, ":")
	k := plan.EvidenceKind(strings.TrimSpace(kind))
	if !k.Supported() {
		return "", "", fmt.Errorf("evidence_ref %q: unknown kind %q (want codex:<ref>, lsp:<ref>, repo:<ref>, new or hypothesis)", s, kind)
	}
	if !ok || strings.TrimSpace(ref) == "" {
		return "", "", fmt.Errorf("evidence_ref %q: %s evidence needs a ref", s, k)
	}
	return k, strings.TrimSpace(ref), nil
}

// validateDecomposedTasks is the strict pass over the model's output: known
// kinds everywhere, well-formed ids, derived criteria with reasons and ids
// that collide with nothing. brief may be nil (a brief without criteria).
// Callers run normaliseDerivedCriteria first, so a brief id reused by a
// derived criterion only reaches the check below when it was skipped.
// briefFolds are the brief's own folds (foldBrief): a folded id is still a
// brief id, reserved, never a derived one.
func validateDecomposedTasks(tasks []decomposedTask, brief *plan.BriefCriteria, briefFolds []plan.Fold, reserved ...string) error {
	briefIDs := map[string]bool{}
	if brief != nil {
		for _, c := range brief.Criteria {
			briefIDs[c.ID] = true
		}
	}
	for _, f := range briefFolds {
		briefIDs[f.From] = true
	}
	// derivedBy is the 1-based task that first declared each derived id.
	derivedBy := map[string]int{}
	for i, t := range tasks {
		reject := func(problem, fix string) error {
			return &decompositionError{Task: i + 1, Title: t.Title, Problem: problem, Fix: fix}
		}
		seen := map[string]bool{}
		for _, id := range t.CriterionIDs {
			if !plan.ValidCriterionID(id) {
				return reject(fmt.Sprintf("criterion id %q is not of the form AC-<n>", id),
					fmt.Sprintf("replace %q in criterion_ids with the id (AC-<n>) of a listed acceptance criterion", id))
			}
			if seen[id] {
				return reject(fmt.Sprintf("criterion id %s cited twice", id),
					fmt.Sprintf("list %s only once in criterion_ids", id))
			}
			seen[id] = true
		}
		for j, v := range t.Verification {
			if err := v.Validate(); err != nil {
				return reject(fmt.Sprintf("verification[%d]: %v", j, err),
					fmt.Sprintf("give verification[%d] a check_kind of test|build|lint|diagnostic|other and a command or a description", j))
			}
		}
		for _, c := range t.claims() {
			if strings.TrimSpace(c.Path) == "" {
				return reject("a files entry has no path", "give every files entry a path, or remove the entry")
			}
			if _, _, err := parseEvidenceClaim(c.EvidenceRef); err != nil {
				return reject(fmt.Sprintf("file %s: %v", c.Path, err),
					fmt.Sprintf(`set the evidence_ref of %s to <codex|lsp|repo>:<ref> copied from the File Evidence table, "new" or "hypothesis"`, c.Path))
			}
		}
		for _, d := range t.DerivedCriteria {
			if err := d.criterion().Validate(); err != nil {
				return reject(fmt.Sprintf("derived criterion: %v", err),
					fmt.Sprintf("give derived criterion %q an id AC-<n>, a text, a reason and a risk of low|medium|high, or remove it", d.ID))
			}
			if briefIDs[d.ID] {
				return reject(fmt.Sprintf("derived criterion %s reuses a brief criterion id", d.ID),
					fmt.Sprintf("%s is a brief criterion: list it under criterion_ids, do not redeclare it under derived_criteria", d.ID))
			}
			if first, dup := derivedBy[d.ID]; dup {
				return reject(fmt.Sprintf("derived criterion id %s is used twice", d.ID),
					fmt.Sprintf("%s is already declared by task %d: give this derived criterion the next free id, AC-%d, or cite %s in criterion_ids if it is the same criterion",
						d.ID, first, nextFreeCriterionNumber(tasks, brief, briefFolds, reserved...), d.ID))
			}
			derivedBy[d.ID] = i + 1
		}
	}
	return nil
}

// decompositionError is a rejection of the model's task list by the strict
// pass: which task, what is wrong, and what to change. Error() keeps the
// "task N ("title"): problem" form the block reason and logs carry; the
// re-prompt (decompositionRetryNudge) adds the fix and quotes the task.
type decompositionError struct {
	Task    int // 1-based position in the model's array
	Title   string
	Problem string
	Fix     string
}

func (e *decompositionError) Error() string {
	return fmt.Sprintf("task %d (%q): %s", e.Task, e.Title, e.Problem)
}

// nextFreeCriterionNumber is one past every AC number in the brief (folded
// ids included) and the plan's derived criteria.
func nextFreeCriterionNumber(tasks []decomposedTask, brief *plan.BriefCriteria, briefFolds []plan.Fold, reserved ...string) int {
	next := nextCriterionNumber(brief, briefFolds, reserved...)
	for _, t := range tasks {
		for _, d := range t.DerivedCriteria {
			if !plan.ValidCriterionID(d.ID) {
				continue
			}
			if n, err := strconv.Atoi(strings.TrimPrefix(d.ID, "AC-")); err == nil && n >= next {
				next = n + 1
			}
		}
	}
	return next
}

// decomposeQuoteMaxBytes bounds the offending task quoted in a re-prompt.
const decomposeQuoteMaxBytes = 8 << 10

// decompositionRetryNudge is the one re-prompt after the strict pass rejected
// the model's task list: the rejection, the exact change to make, and the
// offending task as the model sent it. sent must be the model's tasks as
// parsed, before normaliseDerivedCriteria. Any other error gets the generic
// message.
func decompositionRetryNudge(vErr error, sent []decomposedTask) string {
	tasks := sent
	var de *decompositionError
	if !errors.As(vErr, &de) {
		return fmt.Sprintf("Your previous response was rejected: %v. Respond with ONLY the corrected JSON array, using only the allowed values in the Output Format.", vErr)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Your previous response was rejected: %s.\n", de.Error())
	if de.Fix != "" {
		fmt.Fprintf(&b, "Fix: %s.\n", de.Fix)
	}
	if de.Task >= 1 && de.Task <= len(tasks) {
		if data, err := json.MarshalIndent(tasks[de.Task-1], "", " "); err == nil {
			fmt.Fprintf(&b, "\nTask %d as you sent it:\n", de.Task)
			if len(data) > decomposeQuoteMaxBytes {
				b.Write(data[:decomposeQuoteMaxBytes])
				fmt.Fprintf(&b, "\n... (truncated: %d of %d bytes shown)", decomposeQuoteMaxBytes, len(data))
			} else {
				b.Write(data)
			}
			b.WriteString("\n")
		}
	}
	b.WriteString("\nChange only what the fix names. Respond with ONLY the corrected JSON array (every task, not only the changed one), using only the allowed values in the Output Format.")
	return b.String()
}

// normaliseDerivedCriteria repairs, in place, the one derived-criterion
// mistake a model commonly makes (R7, attempt 7): redeclaring a brief
// criterion under derived_criteria with the brief's own id instead of citing
// it. Such an entry becomes a citation of the brief id in the task's
// criterion_ids (de-duplicated) and the derived entry is dropped. When its
// text differs materially from the brief criterion's (token Jaccard over
// plan.NormalizeText below plan.FoldJaccard), the model meant something the
// brief does not say: the brief id is still cited, and the derived criterion
// is kept under a fresh id (one past every AC id in the brief or the plan's
// derived criteria) that the task also cites, so the intent is not lost.
// Every other problem, including a derived id used twice, is left to
// validateDecomposedTasks. It returns one log line per entry it changed.
//
// brief is the folded brief and briefFolds its folds (foldBrief): an id the
// brief fold removed counts as the criterion it was folded into (cited as
// that id), and fresh ids skip folded ids too, since foldPlanCriteria would
// rewrite a citation of one.
func normaliseDerivedCriteria(tasks []decomposedTask, brief *plan.BriefCriteria, briefFolds []plan.Fold, reserved ...string) []string {
	if brief == nil || len(brief.Criteria) == 0 {
		return nil
	}
	briefText := map[string]string{}
	for _, c := range brief.Criteria {
		briefText[c.ID] = c.Text
	}
	// target maps a reused id to the brief id to cite.
	target := map[string]string{}
	for id := range briefText {
		target[id] = id
	}
	for _, f := range briefFolds {
		if _, ok := briefText[f.Into]; ok {
			target[f.From] = f.Into
		}
	}
	next := nextFreeCriterionNumber(tasks, brief, briefFolds, reserved...)
	var notes []string
	for i := range tasks {
		t := &tasks[i]
		if len(t.DerivedCriteria) == 0 {
			continue
		}
		label := fmt.Sprintf("task %d (%q)", i+1, t.Title)
		kept := t.DerivedCriteria[:0:0]
		for _, d := range t.DerivedCriteria {
			cite, isBrief := target[d.ID]
			if !isBrief {
				kept = append(kept, d)
				continue
			}
			t.CriterionIDs = appendUnique(t.CriterionIDs, cite)
			sim := plan.TokenJaccard(plan.NormalizeText(d.Text), plan.NormalizeText(briefText[cite]))
			if sim >= plan.FoldJaccard {
				notes = append(notes, fmt.Sprintf("%s: derived criterion %s restates brief criterion %s (token Jaccard %.2f) — cited it instead", label, d.ID, cite, sim))
				continue
			}
			fresh := fmt.Sprintf("AC-%d", next)
			next++
			notes = append(notes, fmt.Sprintf("%s: derived criterion %s reuses a brief id with different text (token Jaccard %.2f) — cited %s and renumbered the derived criterion %s", label, d.ID, sim, cite, fresh))
			d.ID = fresh
			kept = append(kept, d)
			t.CriterionIDs = appendUnique(t.CriterionIDs, fresh)
		}
		t.DerivedCriteria = kept
	}
	return notes
}

// citeOwnDerivedCriteria makes every task cite the derived criteria it
// declares (R13 D1, attempt 13): a task that adds AC-11 under
// derived_criteria serves AC-11, so the id is added to its criterion_ids
// (de-duplicated) when the model left it out. Without it the declaring task
// is rejected as citing nothing (no_criteria) and its own criterion as owned
// by no task (orphan_criterion). An id that is not of the form AC-<n> is
// left to the strict pass, which rejects the derived criterion with its own
// message. It runs after normaliseDerivedCriteria, so a redeclared brief id
// is already a citation and only genuinely derived ids remain. It returns
// one log line per citation it added.
func citeOwnDerivedCriteria(tasks []decomposedTask) []string {
	var notes []string
	for i := range tasks {
		t := &tasks[i]
		for _, d := range t.DerivedCriteria {
			if !plan.ValidCriterionID(d.ID) {
				continue
			}
			before := len(t.CriterionIDs)
			t.CriterionIDs = appendUnique(t.CriterionIDs, d.ID)
			if len(t.CriterionIDs) > before {
				notes = append(notes, fmt.Sprintf("task %d (%q) declares derived criterion %s but did not cite it — cited it", i+1, t.Title, d.ID))
			}
		}
	}
	return notes
}

func appendUnique(ids []string, id string) []string {
	for _, x := range ids {
		if x == id {
			return ids
		}
	}
	return append(ids, id)
}

// briefCriteriaOf is the brief's criteria when they validate; an invalid set
// is logged by the caller and treated as absent (planning continues as for a
// legacy brief, every task then a hypothesis).
func briefCriteriaOf(b structuredBrief) (*plan.BriefCriteria, error) {
	if b.Criteria == nil {
		return nil, nil
	}
	if err := b.Criteria.Validate(); err != nil {
		return nil, err
	}
	return b.Criteria, nil
}

// nextCriterionNumber is one past the brief's highest AC number, counting the
// ids the brief fold removed (briefFolds): a derived criterion numbered as a
// folded id would have its citations rewritten by foldPlanCriteria. reserved
// are further ids to skip: in a repair round, every id an earlier round used
// (R13 review M1, carriedDerived.reservedIDs), so a fresh derived id never
// collides with an earlier round's derived criterion.
func nextCriterionNumber(brief *plan.BriefCriteria, briefFolds []plan.Fold, reserved ...string) int {
	n := 0
	bump := func(id string) {
		if v, err := strconv.Atoi(strings.TrimPrefix(id, "AC-")); err == nil && v > n {
			n = v
		}
	}
	if brief != nil {
		for _, c := range brief.Criteria {
			bump(c.ID)
		}
	}
	for _, f := range briefFolds {
		bump(f.From)
		bump(f.Into)
	}
	for _, id := range reserved {
		bump(id)
	}
	return n + 1
}

// criteriaPromptContext adds the criteria and the evidence table to the
// decomposition prompt's context. brief is the folded brief, briefFolds its
// folds.
func criteriaPromptContext(ctxMap map[string]string, brief *plan.BriefCriteria, briefFolds []plan.Fold, reserved ...string) {
	next := nextCriterionNumber(brief, briefFolds, reserved...)
	var b strings.Builder
	if brief == nil || len(brief.Criteria) == 0 {
		b.WriteString("The technical brief carries no acceptance criteria. Leave criterion_ids empty (each task is then marked as a hypothesis) unless you add derived_criteria with a reason.\n")
	} else {
		for _, c := range brief.Criteria {
			b.WriteString(renderCriterionLine(c))
			b.WriteString("\n")
		}
		b.WriteString("\nEvery task MUST list in criterion_ids the ids above that it satisfies. A task that cites none is marked as a hypothesis.\n")
	}
	fmt.Fprintf(&b, "If the change needs a criterion that is not listed, add it to the task's derived_criteria with a reason, numbering from AC-%d. Never add one silently.", next)
	ctxMap["acceptance_criteria"] = b.String()

	if brief != nil && len(brief.FileEvidence) > 0 {
		paths := make([]string, 0, len(brief.FileEvidence))
		for p := range brief.FileEvidence {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		var e strings.Builder
		for _, p := range paths {
			ev := brief.FileEvidence[p]
			cite := string(ev.Kind)
			if ev.Kind.Supported() {
				cite = string(ev.Kind) + ":" + ev.Ref
			}
			fmt.Fprintf(&e, "- %s — evidence_ref %q\n", p, cite)
		}
		e.WriteString("\nCopy a path's evidence_ref from this table. A path not in this table is \"hypothesis\" if you believe it exists, or \"new\" if the task creates it.")
		ctxMap["file_evidence"] = e.String()
	}
}

// decomposeOutputFormat is the decomposition's output contract.
const decomposeOutputFormat = `JSON array of tasks, nothing else:
[{"title":"...","description":"...",
  "criterion_ids":["AC-1"],
  "deliverable":"the observable result this task produces",
  "verification":[{"check_kind":"test","command":"go test ./pkg/..."},{"check_kind":"other","description":"how to check it when no command applies"}],
  "files":[{"path":"src/existing.ts","evidence_ref":"repo:call_1"},{"path":"src/created.ts","evidence_ref":"new"},{"path":"src/unproven.ts","evidence_ref":"hypothesis"}],
  "depends_on":["Title of a task this one needs first"],
  "repository_id":"...",
  "derived_criteria":[{"id":"AC-9","text":"...","reason":"why the change needs it","risk":"low","required_checks":["build"]}]}]
In "files", list only paths this task creates or modifies, not files read for context.
Allowed values, exactly: evidence_ref is <codex|lsp|repo>:<ref> copied from the File Evidence table, "new", or "hypothesis"; check_kind is test|build|lint|diagnostic|other; risk is low|medium|high. derived_criteria is optional.
The plan is checked before it is accepted: every acceptance criterion is cited by at least one task; every task cites at least one criterion, states a deliverable and has at least one verification step; a criterion's required checks each need a verification step of that check_kind in a task that cites it; a path that exists needs its evidence_ref from the File Evidence table (or "hypothesis"); depends_on names titles of tasks in this array, without cycles.`

// fileGrounding is the outcome of grounding one path.
type fileGrounding int

const (
	fileExists     fileGrounding = iota // os.Stat found it in the clone
	fileMissing                         // not found; hypothesis
	fileNew                             // not found; the task creates it
	fileUnverified                      // no clone path to check
)

type groundedFile struct {
	ref    plan.FileRef
	status fileGrounding
}

// groupFileClaims is the union of a subtask's steps' claims, in step order,
// first claim per path.
func groupFileClaims(tasks []decomposedTask) []fileClaim {
	seen := map[string]bool{}
	var out []fileClaim
	for _, t := range tasks {
		for _, c := range t.claims() {
			p := strings.TrimPrefix(strings.TrimSpace(c.Path), "./")
			if p == "" || seen[p] {
				continue
			}
			seen[p] = true
			out = append(out, fileClaim{Path: p, EvidenceRef: c.EvidenceRef})
		}
	}
	return out
}

// groundTaskFiles applies the planner's os.Stat grounding and turns its
// result into evidence. An existing path keeps the brief's codex/lsp/repo
// evidence when there is some, otherwise it is repo evidence by stat; a
// missing path is "new" when the model said so, otherwise a hypothesis with
// the grounding note. With no clone path nothing can be checked: the brief's
// evidence, or "new", or a hypothesis marked unverified.
func groundTaskFiles(cloneRoot string, claims []fileClaim, briefEvidence map[string]plan.EvidenceRef) []groundedFile {
	return groundTaskFilesAt(cloneRoot, cloneRoot, plan.BasisClone, claims, briefEvidence)
}

// groundTaskFilesAt is groundTaskFiles with the stat done under statRoot and
// recorded with basis: the snapshot worktree at the pin (plan.BasisSnapshot,
// M2-05 via statRootFor) or the clone. Containment and making an absolute
// path relative are against the clone (and the stat root).
func groundTaskFilesAt(cloneRoot, statRoot, basis string, claims []fileClaim, briefEvidence map[string]plan.EvidenceRef) []groundedFile {
	where := "the repository clone"
	if basis == plan.BasisSnapshot {
		where = "the pinned snapshot"
	}
	out := make([]groundedFile, 0, len(claims))
	for _, c := range claims {
		kind, _, err := parseEvidenceClaim(c.EvidenceRef)
		if err != nil {
			kind = plan.EvidenceHypothesis // validated earlier; defensive
		}
		// Containment (the same rule as research-agent's cleanRepoPath): a
		// path that escapes the repository is never stat'ed or trusted.
		rel, inside := containedPath(c.Path, cloneRoot)
		if !inside && statRoot != cloneRoot {
			rel, inside = containedPath(c.Path, statRoot)
		}
		if !inside {
			out = append(out, groundedFile{plan.FileRef{Path: c.Path, Evidence: plan.EvidenceRef{
				Kind: plan.EvidenceHypothesis, Note: "grounding: path is outside the repository"}}, fileMissing})
			continue
		}
		c.Path = rel
		fromBrief, hasBrief := briefEvidence[c.Path]
		hasBrief = hasBrief && fromBrief.Kind.Supported()

		if statRoot == "" {
			switch {
			case hasBrief:
				out = append(out, groundedFile{plan.FileRef{Path: c.Path, Evidence: fromBrief}, fileUnverified})
			case kind == plan.EvidenceNew:
				out = append(out, groundedFile{plan.FileRef{Path: c.Path, Evidence: plan.EvidenceRef{Kind: plan.EvidenceNew}}, fileUnverified})
			default:
				out = append(out, groundedFile{plan.FileRef{Path: c.Path, Evidence: plan.EvidenceRef{
					Kind: plan.EvidenceHypothesis, Note: "repository has no clone path; not verified"}}, fileUnverified})
			}
			continue
		}

		if _, statErr := os.Stat(filepath.Join(statRoot, filepath.Clean(c.Path))); statErr == nil {
			ev := plan.EvidenceRef{Kind: plan.EvidenceRepo, Ref: plan.RefStat, Confidence: 1, Basis: basis}
			if hasBrief {
				ev = fromBrief
			}
			out = append(out, groundedFile{plan.FileRef{Path: c.Path, Evidence: ev}, fileExists})
			continue
		}
		if kind == plan.EvidenceNew {
			ev := plan.EvidenceRef{Kind: plan.EvidenceNew}
			if dir := filepath.Dir(filepath.FromSlash(c.Path)); dir != "." {
				if st, err := os.Stat(filepath.Join(statRoot, dir)); err != nil || !st.IsDir() {
					ev.Note = fmt.Sprintf("%s: directory %s does not exist", newDirNotePrefix, filepath.ToSlash(dir))
				}
			}
			out = append(out, groundedFile{plan.FileRef{Path: c.Path, Evidence: ev}, fileNew})
			continue
		}
		note := "grounding: not found in " + where
		if hasBrief {
			note = fmt.Sprintf("grounding: not found in %s although research cited %s", where, plan.EvidenceLabel(fromBrief))
		}
		out = append(out, groundedFile{plan.FileRef{Path: c.Path, Evidence: plan.EvidenceRef{Kind: plan.EvidenceHypothesis, Note: note}}, fileMissing})
	}
	return out
}

// newDirNotePrefix starts the note of a new file whose directory is absent.
const newDirNotePrefix = "grounding: new file in a new directory"

// containedPath makes p relative to the clone and rejects a path that
// escapes it: "..", or absolute and not under cloneRoot. With no clone root,
// only relative paths that stay relative are accepted.
func containedPath(p, cloneRoot string) (string, bool) {
	p = strings.TrimSpace(strings.ReplaceAll(p, "\\", "/"))
	root := strings.TrimRight(strings.ReplaceAll(cloneRoot, "\\", "/"), "/")
	if root != "" && strings.HasPrefix(p, root+"/") {
		p = strings.TrimPrefix(p, root+"/")
	}
	p = strings.TrimPrefix(p, "./")
	c := filepath.ToSlash(filepath.Clean(filepath.FromSlash(p)))
	if p == "" || c == "." || c == ".." || strings.HasPrefix(c, "../") || strings.HasPrefix(c, "/") || filepath.IsAbs(c) {
		return "", false
	}
	return c, true
}

// buildTaskSpec merges a subtask's steps into its TaskSpec.
func buildTaskSpec(steps []decomposedTask, files []groundedFile) plan.TaskSpec {
	spec := plan.TaskSpec{SchemaVersion: plan.SchemaVersion, CriterionIDs: []string{}, Verification: []plan.VerificationStep{}}
	seenID := map[string]bool{}
	seenV := map[plan.VerificationStep]bool{}
	var deliverables []string
	for i, t := range steps {
		for _, id := range t.CriterionIDs {
			if !seenID[id] {
				seenID[id] = true
				spec.CriterionIDs = append(spec.CriterionIDs, id)
			}
		}
		for _, v := range t.Verification {
			if !seenV[v] {
				seenV[v] = true
				spec.Verification = append(spec.Verification, v)
			}
		}
		d := strings.TrimSpace(t.Deliverable)
		if d == "" {
			continue
		}
		if len(steps) > 1 {
			d = fmt.Sprintf("Step %d (%s): %s", i+1, t.Title, d)
		}
		deliverables = append(deliverables, d)
	}
	spec.Deliverable = strings.Join(deliverables, "\n")
	for _, f := range files {
		spec.Files = append(spec.Files, f.ref)
	}
	return spec
}

// planDerivedCriteria collects every derived criterion of the plan, by id.
func planDerivedCriteria(tasks []decomposedTask) map[string]plan.Criterion {
	out := map[string]plan.Criterion{}
	for _, t := range tasks {
		for _, d := range t.DerivedCriteria {
			out[d.ID] = d.criterion()
		}
	}
	return out
}

// relevantCriteria are the criteria a TaskSpec cites, from the brief or the
// plan's derived ones, in citation order. Unknown ids are left to M2-05.
func relevantCriteria(spec plan.TaskSpec, brief *plan.BriefCriteria, derived map[string]plan.Criterion) []plan.Criterion {
	byID := map[string]plan.Criterion{}
	if brief != nil {
		for _, c := range brief.Criteria {
			byID[c.ID] = c
		}
	}
	for id, c := range derived {
		byID[id] = c
	}
	var out []plan.Criterion
	for _, id := range spec.CriterionIDs {
		if c, ok := byID[id]; ok {
			out = append(out, c)
		}
	}
	return out
}

func renderCriterionLine(c plan.Criterion) string {
	attrs := "risk: " + string(c.Risk)
	if len(c.RequiredChecks) > 0 {
		attrs += "; checks: " + plan.CheckList(c.RequiredChecks)
	}
	line := fmt.Sprintf("- %s [%s]: %s", c.ID, attrs, c.Text)
	switch c.Source.Kind {
	case plan.SourceUser:
		line += fmt.Sprintf("\n  Source: user — %q", c.Source.Quote)
	case plan.SourceDerived:
		line += "\n  Source: derived — " + c.Source.Reason
	}
	return line
}

func renderVerification(v plan.VerificationStep) string {
	s := fmt.Sprintf("[%s] %s", v.CheckKind, v.Description)
	if v.Command != "" {
		s = fmt.Sprintf("[%s] `%s`", v.CheckKind, v.Command)
		if v.Description != "" {
			s += " — " + v.Description
		}
	}
	if v.AttachedBy == plan.AttachedByPlanner {
		s += " (attached by the planner: the repository's whole suite, not a check of any one criterion)"
	}
	return s
}

// renderStepSpec is the per-step criteria/deliverable/verification block of
// the subtask description.
func renderStepSpec(t decomposedTask) string {
	var b strings.Builder
	if len(t.CriterionIDs) == 0 {
		b.WriteString("Criteria: none cited — HYPOTHESIS: no acceptance criterion supports this step.\n")
	} else {
		fmt.Fprintf(&b, "Criteria: %s\n", strings.Join(t.CriterionIDs, ", "))
	}
	if d := strings.TrimSpace(t.Deliverable); d != "" {
		fmt.Fprintf(&b, "Deliverable: %s\n", d)
	}
	if len(t.Verification) > 0 {
		parts := make([]string, 0, len(t.Verification))
		for _, v := range t.Verification {
			parts = append(parts, renderVerification(v))
		}
		fmt.Fprintf(&b, "Verification: %s\n", strings.Join(parts, "; "))
	}
	return b.String()
}

// renderSubtaskSpec is the subtask-level rendering: the criteria it serves,
// its deliverable and verification, and every file with its evidence.
func renderSubtaskSpec(spec plan.TaskSpec, criteria []plan.Criterion) string {
	var b strings.Builder
	b.WriteString("\n## Acceptance criteria\n\n")
	if len(spec.CriterionIDs) == 0 {
		b.WriteString("None cited — this subtask is a HYPOTHESIS: no acceptance criterion from the brief supports it.\n")
	} else {
		known := map[string]plan.Criterion{}
		for _, c := range criteria {
			known[c.ID] = c
		}
		for _, id := range spec.CriterionIDs {
			if c, ok := known[id]; ok {
				b.WriteString(renderCriterionLine(c))
				b.WriteString("\n")
			} else {
				fmt.Fprintf(&b, "- %s: cited, but not in the brief\n", id)
			}
		}
	}
	if spec.Deliverable != "" {
		fmt.Fprintf(&b, "\n## Deliverable\n\n%s\n", spec.Deliverable)
	}
	if len(spec.Verification) > 0 {
		b.WriteString("\n## Verification\n\n")
		for _, v := range spec.Verification {
			fmt.Fprintf(&b, "- %s\n", renderVerification(v))
		}
	}
	if len(spec.Files) > 0 {
		b.WriteString("\n## File evidence\n\n")
		var unjustified []string
		for _, f := range spec.Files {
			fmt.Fprintf(&b, "- %s — %s\n", f.Path, plan.EvidenceLabel(f.Evidence))
			if f.Evidence.Kind == plan.EvidenceNew && strings.HasPrefix(f.Evidence.Note, newDirNotePrefix) {
				unjustified = append(unjustified, f.Path)
			}
		}
		if len(unjustified) > 0 {
			fmt.Fprintf(&b, "\nFiles to be created must be justified: %s would be created in directories that do not exist yet. Confirm no existing file serves the purpose before creating them.\n", strings.Join(unjustified, ", "))
		}
	}
	return b.String()
}
