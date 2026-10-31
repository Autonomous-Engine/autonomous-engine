package plan

// The deterministic plan validator (M2-05).
//
// ValidatePlan checks a decomposition against the brief's acceptance
// criteria, the repository snapshot and the dependency graph, and returns
// every problem it finds as a Violation whose Message is specific enough to
// be handed back to the model verbatim ("task 2 \"Add handler\" cites AC-7,
// which does not exist; known ids: AC-1..AC-6"). It is pure: no LLM, no
// database, no network. The only view of the repository is snapshotStat.
//
// (section "Plan validation (M2-05)").

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/evidence"
)

// ViolationCode names one plan rule.
type ViolationCode string

const (
	// CodeOrphanCriterion: a criterion no (kept) task owns.
	CodeOrphanCriterion ViolationCode = "orphan_criterion"
	// CodeUnknownCriterion: a task cites an id the brief (and the plan's
	// derived criteria) does not have.
	CodeUnknownCriterion ViolationCode = "unknown_criterion"
	// CodeNoCriteria: a task cites no criterion although the brief has some.
	CodeNoCriteria ViolationCode = "no_criteria"
	// CodeNoDeliverable: a task states no deliverable.
	CodeNoDeliverable ViolationCode = "no_deliverable"
	// CodeEmptyVerification: a task has no verification step.
	CodeEmptyVerification ViolationCode = "empty_verification"
	// CodeVerifierMissing: a check kind a criterion requires (or the
	// repository's verification policy requires) has no verification step of
	// that kind in the tasks that own it (in that repository).
	CodeVerifierMissing ViolationCode = "verifier_missing"
	// CodeUnevidencedPath: a task names a path that exists at the snapshot
	// with no evidence and without marking it a hypothesis.
	CodeUnevidencedPath ViolationCode = "unevidenced_path"
	// CodeStaleEvidence: repo evidence for a path that does not exist at the
	// snapshot.
	CodeStaleEvidence ViolationCode = "stale_evidence"
	// CodeUnknownDependency: a depends_on title that names no task of the
	// plan, or names steps in two different subtasks (M1-06).
	CodeUnknownDependency ViolationCode = "unknown_dependency"
	// CodeDependencyCycle: the subtask dependency graph has a cycle (M1-06).
	CodeDependencyCycle ViolationCode = "dependency_cycle"
	// CodeOneSidedContract: a criterion's cross-repository contract (one
	// naming a producer and a consumer repository, M2-06) is changed by a
	// task on one side only: no task of the plan is in the other repository.
	CodeOneSidedContract ViolationCode = "one_sided_contract"
	// CodeHypothesisOnlyTask: every file a task names is a hypothesis. A
	// warning: it never rejects a plan; the planner shows it in the subtask
	// description.
	CodeHypothesisOnlyTask ViolationCode = "hypothesis_only_task"
	// CodeNonCodeCriterion: a criterion owned by no kept task whose only
	// citing steps were dropped as non-code work (process, deployment). A
	// warning: the criterion is marked non-code and surfaced to review as a
	// gap to verify manually instead of blocking the plan (R3).
	CodeNonCodeCriterion ViolationCode = "non_code_criterion"
)

// ViolationCodes lists every code, in rule order (metrics register one
// series per code from it).
var ViolationCodes = []ViolationCode{
	CodeOrphanCriterion, CodeUnknownCriterion, CodeNoCriteria, CodeNoDeliverable,
	CodeEmptyVerification, CodeVerifierMissing, CodeUnevidencedPath, CodeStaleEvidence,
	CodeUnknownDependency, CodeDependencyCycle, CodeOneSidedContract, CodeHypothesisOnlyTask,
	CodeNonCodeCriterion,
}

// Severity of a violation: an error rejects the plan, a warning never does.
type Severity string

const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
)

// Violation is one broken rule.
type Violation struct {
	Code     ViolationCode `json:"code"`
	Severity Severity      `json:"severity"`
	// TaskIndex is the 1-based position of the task in the model's task list
	// (the number the message uses); 0 when the violation is not about one
	// task.
	TaskIndex int `json:"task_index,omitempty"`
	// CriterionID is set when the violation is about one criterion.
	CriterionID string `json:"criterion_id,omitempty"`
	// CheckKind, Path and Dependency name what else the violation is about
	// (a missing check kind; an unevidenced or stale path; an unknown
	// dependency title), so Key tells two violations of one task apart (R9).
	CheckKind  evidence.CheckKind `json:"check_kind,omitempty"`
	Path       string             `json:"path,omitempty"`
	Dependency string             `json:"dependency,omitempty"`
	Message    string             `json:"message"`
}

// warningCodes are the codes the validator reports as warnings: they never
// reject a plan (their remediation is "gap").
var warningCodes = map[ViolationCode]bool{
	CodeHypothesisOnlyTask: true,
	CodeNonCodeCriterion:   true,
}

// WarningCode reports whether code is only ever a warning.
func WarningCode(code ViolationCode) bool { return warningCodes[code] }

// TaskSpecWithFiles is one task of the model's output as the validator sees
// it: its position, its repository, and its spec with the files as the model
// claimed them.
type TaskSpecWithFiles struct {
	// Index is the task's 1-based position in the model's output.
	Index int    `json:"index"`
	Title string `json:"title"`
	// RepositoryID is the repository the planner resolved for the task (the
	// first argument of snapshotStat); Repository is its display name.
	RepositoryID string `json:"repository_id,omitempty"`
	Repository   string `json:"repository,omitempty"`
	// Spec carries the task's criterion ids, deliverable, verification and
	// files. A file's Evidence is the model's claim: Kind "" means no claim
	// (a bare path or an empty evidence_ref) and is resolved from the
	// brief's file_evidence when the brief has supported evidence for the
	// path; "hypothesis" is an explicit mark.
	Spec TaskSpec `json:"spec"`
	// PolicyChecks are the check kinds the repository's verification policy
	// requires (M3-03), as stored.
	PolicyChecks []evidence.CheckKind `json:"policy_checks,omitempty"`
	// NoTestRunner: the repository has no test runner (and the request is
	// not about adding tests), so a test cannot run there. The plan is then
	// not required to carry a "test" step for the task — neither for the
	// policy's kinds nor for a criterion's required checks; the gate's
	// no_checks_policy decides that case. One exemption, both forms.
	NoTestRunner bool `json:"no_test_runner,omitempty"`
	// Dropped is why the planner dropped the task (not an implementation
	// task, a test-only task in a repository without a test runner, no
	// resolvable repository). A dropped task owns nothing and is not checked;
	// it is named in the message of a criterion only it cited.
	Dropped string `json:"dropped,omitempty"`
	// NonCode: the step was dropped because it is not code work (process,
	// deployment; it names no repository file). A criterion that only such
	// steps cite is non-code (NonCodeCriteria), not an orphan.
	NonCode bool `json:"non_code,omitempty"`
}

// UnresolvedDependency is a depends_on title M1-06's resolution could not map
// to exactly one subtask.
type UnresolvedDependency struct {
	// TaskIndex/Task: the step that declared it (0/"" when it was inherited
	// through a dropped step and the declaring step is not a kept one).
	TaskIndex int    `json:"task_index,omitempty"`
	Task      string `json:"task,omitempty"`
	// Subtask is the label of the subtask whose dependency it is.
	Subtask    string `json:"subtask,omitempty"`
	Dependency string `json:"dependency"`
	// Through is the dropped step it was inherited through ("" for the
	// task's own).
	Through string `json:"through,omitempty"`
	// Ambiguous lists the labels of the subtasks the title names when it
	// names more than one; empty means it names none.
	Ambiguous []string `json:"ambiguous,omitempty"`
}

// DependencyGraph is the plan's subtask dependency graph as M1-06 resolves
// it: one node per repository subtask (several steps of one repository are
// one node, and ordering inside a node is not an edge).
type DependencyGraph struct {
	// Labels[i] names node i (the subtask title).
	Labels []string `json:"labels,omitempty"`
	// Edges[i] lists the nodes node i depends on.
	Edges [][]int `json:"edges,omitempty"`
	// Unresolved are the dependency titles resolution could not map.
	Unresolved []UnresolvedDependency `json:"unresolved,omitempty"`
	// KnownTitles are the titles a depends_on may name (for messages).
	KnownTitles []string `json:"known_titles,omitempty"`
}

// ValidatePlan applies every plan rule. brief carries the criteria the tasks
// may cite (the technical brief's, after folding, plus the plan's derived
// ones) and the brief's file_evidence. snapshotStat reports whether path
// exists in repository repoID at the pinned snapshot; nil skips the two
// rules that need it (unevidenced_path, stale_evidence). The result is in
// rule order: per task, then per criterion, then dependencies.
func ValidatePlan(brief BriefCriteria, tasks []TaskSpecWithFiles, deps DependencyGraph, snapshotStat func(repoID, path string) bool) []Violation {
	var out []Violation
	add := func(v Violation) {
		if v.Severity == "" {
			v.Severity = SeverityError
			if WarningCode(v.Code) {
				v.Severity = SeverityWarning
			}
		}
		out = append(out, v)
	}

	known := map[string]bool{}
	ids := make([]string, 0, len(brief.Criteria))
	for _, c := range brief.Criteria {
		known[c.ID] = true
		ids = append(ids, c.ID)
	}
	knownList := CriterionIDList(ids)

	var active []TaskSpecWithFiles
	for _, t := range tasks {
		if t.Dropped == "" {
			active = append(active, t)
		}
	}

	// Per task.
	for _, t := range active {
		label := TaskLabel(t.Index, t.Title)
		for _, id := range t.Spec.CriterionIDs {
			if known[id] {
				continue
			}
			msg := fmt.Sprintf("%s cites %s, which does not exist; known ids: %s", label, id, knownList)
			if len(ids) == 0 {
				msg = fmt.Sprintf("%s cites %s, which does not exist; the brief has no acceptance criteria — add it to the task's derived_criteria with a reason, or cite none", label, id)
			}
			add(Violation{Code: CodeUnknownCriterion, TaskIndex: t.Index, CriterionID: id, Message: msg})
		}
		if len(t.Spec.CriterionIDs) == 0 && len(ids) > 0 {
			add(Violation{Code: CodeNoCriteria, TaskIndex: t.Index, Message: fmt.Sprintf(
				"%s cites no acceptance criterion; list in criterion_ids the ids it satisfies (known ids: %s), or add a derived criterion with a reason", label, knownList)})
		}
		if strings.TrimSpace(t.Spec.Deliverable) == "" {
			add(Violation{Code: CodeNoDeliverable, TaskIndex: t.Index, Message: fmt.Sprintf(
				"%s states no deliverable; set deliverable to the observable result the task produces", label)})
		}
		if len(t.Spec.Verification) == 0 {
			add(Violation{Code: CodeEmptyVerification, TaskIndex: t.Index, Message: fmt.Sprintf(
				"%s has an empty verification plan; add at least one verification step {check_kind, command or description}", label)})
		}

		var hypotheses []string
		for _, f := range t.Spec.Files {
			ev := f.Evidence
			fromBrief := false
			if ev.Kind == "" {
				if b, ok := brief.FileEvidence[f.Path]; ok && b.Kind.Supported() {
					ev, fromBrief = b, true
				}
			}
			switch ev.Kind {
			case "":
				if snapshotStat != nil && snapshotStat(t.RepositoryID, f.Path) {
					add(Violation{Code: CodeUnevidencedPath, TaskIndex: t.Index, Path: f.Path, Message: fmt.Sprintf(
						"%s names %s, which exists at the snapshot, with no evidence; copy its evidence_ref from the File Evidence table, or mark it \"hypothesis\"", label, f.Path)})
				} else {
					hypotheses = append(hypotheses, f.Path)
				}
			case EvidenceRepo:
				if snapshotStat != nil && !snapshotStat(t.RepositoryID, f.Path) {
					src := "cites"
					if fromBrief {
						src = "relies on the brief's"
					}
					add(Violation{Code: CodeStaleEvidence, TaskIndex: t.Index, Path: f.Path, Message: fmt.Sprintf(
						"%s %s repo evidence (repo:%s) for %s, which does not exist at the snapshot; remove the path, mark it \"new\" if the task creates it, or \"hypothesis\"", label, src, ev.Ref, f.Path)})
				}
			case EvidenceHypothesis:
				hypotheses = append(hypotheses, f.Path)
			}
		}
		if len(t.Spec.Files) > 0 && len(hypotheses) == len(t.Spec.Files) {
			add(Violation{Code: CodeHypothesisOnlyTask, Severity: SeverityWarning, TaskIndex: t.Index, Message: fmt.Sprintf(
				"%s: every file it names is a hypothesis (%s); no evidence shows they exist — confirm the real files before editing", label, strings.Join(hypotheses, ", "))})
		}
	}

	// Per criterion.
	nonCode := NonCodeCriteria(brief, tasks)
	for _, c := range brief.Criteria {
		var owners []TaskSpecWithFiles
		for _, t := range active {
			if containsString(t.Spec.CriterionIDs, c.ID) {
				owners = append(owners, t)
			}
		}
		if reason, ok := nonCode[c.ID]; ok {
			// Fail-closed by surfacing, not blocking: no code task can
			// satisfy it, so review is told to verify it manually.
			marked := c
			marked.NonCode, marked.NonCodeReason = true, reason
			add(Violation{Code: CodeNonCodeCriterion, Severity: SeverityWarning, CriterionID: c.ID, Message: NonCodeGap(marked)})
			continue
		}
		if len(owners) == 0 {
			msg := fmt.Sprintf("%s (%q) is owned by no task", c.ID, shorten(c.Text, 80))
			var dropped []string
			for _, t := range tasks {
				if t.Dropped != "" && containsString(t.Spec.CriterionIDs, c.ID) {
					dropped = append(dropped, fmt.Sprintf("%s, which the planner dropped (%s)", TaskLabel(t.Index, t.Title), t.Dropped))
				}
			}
			if len(dropped) > 0 {
				msg += "; it was cited only by " + strings.Join(dropped, " and by ")
			}
			msg += "; add it to the criterion_ids of the task that satisfies it"
			add(Violation{Code: CodeOrphanCriterion, CriterionID: c.ID, Message: msg})
			continue
		}
		for _, k := range normalizeChecks(c.RequiredChecks) {
			// The no-runner exemption (as for the policy's kinds): a test
			// is required only of owners whose repository can run one.
			if exemptFromCheck(owners, k) || anyHasCheck(owners, k) {
				continue
			}
			add(Violation{Code: CodeVerifierMissing, CriterionID: c.ID, CheckKind: k, Message: fmt.Sprintf(
				"%s requires a %s check, but no task that owns it (%s) has a %s verification step; add one", c.ID, k, taskLabels(owners), k)})
		}
	}

	// Per repository: the verification policy's required kinds (M3-03), the
	// same "a step of that kind" test as the criterion's.
	var repoOrder []string
	byRepo := map[string][]TaskSpecWithFiles{}
	for _, t := range active {
		if _, ok := byRepo[t.RepositoryID]; !ok {
			repoOrder = append(repoOrder, t.RepositoryID)
		}
		byRepo[t.RepositoryID] = append(byRepo[t.RepositoryID], t)
	}
	for _, repo := range repoOrder {
		group := byRepo[repo]
		var kinds []evidence.CheckKind
		for _, t := range group {
			kinds = append(kinds, t.PolicyChecks...)
		}
		name := group[0].Repository
		if name == "" {
			name = repo
		}
		for _, k := range normalizeChecks(kinds) {
			if exemptFromCheck(group, k) || anyHasCheck(group, k) {
				continue
			}
			add(Violation{Code: CodeVerifierMissing, TaskIndex: group[0].Index, CheckKind: k, Message: fmt.Sprintf(
				"repository %s's verification policy requires a %s check, but no task in it (%s) has a %s verification step; add one", name, k, taskLabels(group), k)})
		}
	}

	// Cross-repository contracts (M2-06): both sides of a contract a
	// criterion declares between two repositories must be changed by the
	// plan, or neither.
	for _, v := range oneSidedContracts(brief, active) {
		add(v)
	}

	// Dependencies (M1-06's resolution and cycle check).
	for _, u := range deps.Unresolved {
		who := fmt.Sprintf("subtask %q", u.Subtask)
		if u.Task != "" {
			who = TaskLabel(u.TaskIndex, u.Task)
		}
		through := ""
		if u.Through != "" {
			through = fmt.Sprintf(" (through dropped step %q)", u.Through)
		}
		var msg string
		if len(u.Ambiguous) > 0 {
			msg = fmt.Sprintf("%s depends on %q%s, which names steps in %d different subtasks (%s); give the steps distinct titles",
				who, u.Dependency, through, len(u.Ambiguous), strings.Join(quoteAll(u.Ambiguous), ", "))
		} else {
			msg = fmt.Sprintf("%s depends on %q%s, which is not a task in this plan; known titles: %s",
				who, u.Dependency, through, strings.Join(quoteAll(deps.KnownTitles), ", "))
		}
		add(Violation{Code: CodeUnknownDependency, TaskIndex: u.TaskIndex, Dependency: u.Dependency, Message: msg})
	}
	if cycle := FindDependencyCycle(deps.Edges); cycle != nil {
		labels := make([]string, 0, len(cycle))
		for _, n := range cycle {
			l := strconv.Itoa(n)
			if n < len(deps.Labels) {
				l = deps.Labels[n]
			}
			labels = append(labels, fmt.Sprintf("%q", l))
		}
		// The cycle's nodes, sorted and de-duplicated, are its key (R9):
		// trading one cycle for another is not progress.
		nodes := map[int]bool{}
		var ns []int
		for _, n := range cycle {
			if !nodes[n] {
				nodes[n] = true
				ns = append(ns, n)
			}
		}
		sort.Ints(ns)
		parts := make([]string, 0, len(ns))
		for _, n := range ns {
			parts = append(parts, strconv.Itoa(n))
		}
		add(Violation{Code: CodeDependencyCycle, Dependency: "cycle:" + strings.Join(parts, ","), Message: fmt.Sprintf(
			"the tasks depend on each other in a cycle: %s; remove one of these depends_on entries", strings.Join(labels, " -> "))})
	}
	return out
}

// HasErrors reports whether any violation rejects the plan.
func HasErrors(vs []Violation) bool {
	for _, v := range vs {
		if v.Severity != SeverityWarning {
			return true
		}
	}
	return false
}

// Errors returns the violations that reject the plan, in order.
func Errors(vs []Violation) []Violation {
	var out []Violation
	for _, v := range vs {
		if v.Severity != SeverityWarning {
			out = append(out, v)
		}
	}
	return out
}

// Warnings returns the warnings, in order.
func Warnings(vs []Violation) []Violation {
	var out []Violation
	for _, v := range vs {
		if v.Severity == SeverityWarning {
			out = append(out, v)
		}
	}
	return out
}

// SummarizeViolations renders the first max violations as
// "[code] message; [code] message (and N more)".
func SummarizeViolations(vs []Violation, max int) string {
	parts := make([]string, 0, max)
	for i, v := range vs {
		if i == max {
			break
		}
		parts = append(parts, fmt.Sprintf("[%s] %s", v.Code, v.Message))
	}
	s := strings.Join(parts, "; ")
	if len(vs) > max {
		s += fmt.Sprintf(" (and %d more)", len(vs)-max)
	}
	return s
}

// TaskLabel is how a message names a task: `task 2 "Add handler"`.
func TaskLabel(index int, title string) string {
	return fmt.Sprintf("task %d %q", index, title)
}

// CriterionIDList renders ids compactly: consecutive AC numbers become a
// range ("AC-1..AC-6", "AC-1..AC-3, AC-5"); "none" when empty.
func CriterionIDList(ids []string) string {
	if len(ids) == 0 {
		return "none"
	}
	nums := make([]int, 0, len(ids))
	var other []string
	for _, id := range ids {
		n, err := strconv.Atoi(strings.TrimPrefix(id, "AC-"))
		if err != nil || !ValidCriterionID(id) {
			other = append(other, id)
			continue
		}
		nums = append(nums, n)
	}
	sort.Ints(nums)
	var parts []string
	for i := 0; i < len(nums); {
		j := i
		for j+1 < len(nums) && nums[j+1] == nums[j]+1 {
			j++
		}
		switch {
		case j == i:
			parts = append(parts, fmt.Sprintf("AC-%d", nums[i]))
		case j == i+1:
			parts = append(parts, fmt.Sprintf("AC-%d", nums[i]), fmt.Sprintf("AC-%d", nums[j]))
		default:
			parts = append(parts, fmt.Sprintf("AC-%d..AC-%d", nums[i], nums[j]))
		}
		i = j + 1
	}
	return strings.Join(append(parts, other...), ", ")
}

// FindDependencyCycle is the topological check (M1-06, moved here from the
// planner so the validator and the planner share it): nil when deps is
// acyclic, otherwise one cycle as a path of node indices that starts and
// ends on the same node, every step of which is an edge "depends on".
func FindDependencyCycle(deps [][]int) []int {
	const (
		unvisited = iota
		onStack
		done
	)
	state := make([]int, len(deps))
	var stack []int

	var visit func(n int) []int
	visit = func(n int) []int {
		state[n] = onStack
		stack = append(stack, n)
		for _, d := range deps[n] {
			if d < 0 || d >= len(deps) {
				continue
			}
			switch state[d] {
			case onStack:
				// The cycle is the stack from d's position to here, closed.
				for i, s := range stack {
					if s == d {
						cycle := append([]int(nil), stack[i:]...)
						return append(cycle, d)
					}
				}
			case unvisited:
				if c := visit(d); c != nil {
					return c
				}
			}
		}
		stack = stack[:len(stack)-1]
		state[n] = done
		return nil
	}

	for n := range deps {
		if state[n] == unvisited {
			if c := visit(n); c != nil {
				return c
			}
		}
	}
	return nil
}

// NormalizeCheckKind is the comparison form of a check kind: lower case,
// trimmed — the same normalisation verifypolicy.Policy.Normalize applies to a
// policy's required kinds, so a criterion's kinds and a policy's compare
// alike.
func NormalizeCheckKind(k evidence.CheckKind) evidence.CheckKind {
	return evidence.CheckKind(strings.ToLower(strings.TrimSpace(string(k))))
}

func normalizeChecks(ks []evidence.CheckKind) []evidence.CheckKind {
	seen := map[evidence.CheckKind]bool{}
	var out []evidence.CheckKind
	for _, k := range ks {
		k = NormalizeCheckKind(k)
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, k)
	}
	return out
}

// exemptFromCheck reports whether kind k cannot be demanded of any of tasks:
// "test" when every one of them is in a repository with no test runner.
// The same rule serves a criterion's required checks and the policy's kinds.
func exemptFromCheck(tasks []TaskSpecWithFiles, k evidence.CheckKind) bool {
	if k != evidence.CheckKindTest || len(tasks) == 0 {
		return false
	}
	for _, t := range tasks {
		if !t.NoTestRunner {
			return false
		}
	}
	return true
}

func anyHasCheck(tasks []TaskSpecWithFiles, k evidence.CheckKind) bool {
	for _, t := range tasks {
		for _, v := range t.Spec.Verification {
			if NormalizeCheckKind(v.CheckKind) == k {
				return true
			}
		}
	}
	return false
}

func taskLabels(tasks []TaskSpecWithFiles) string {
	parts := make([]string, 0, len(tasks))
	for _, t := range tasks {
		parts = append(parts, TaskLabel(t.Index, t.Title))
	}
	return strings.Join(parts, ", ")
}

func containsString(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func quoteAll(xs []string) []string {
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		out = append(out, fmt.Sprintf("%q", x))
	}
	return out
}

func shorten(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// oneSidedContracts is the one_sided_contract rule (M2-06). A criterion's
// contract that names a producer and a consumer repository is a contract
// edge; a side is "changed" when some kept task is in that repository
// (matched by id or name, case-insensitively). A plan with tasks on both
// sides is fine and the planner orders them (producer first); a plan with
// neither does not touch the contract; one side only is a violation.
func oneSidedContracts(brief BriefCriteria, active []TaskSpecWithFiles) []Violation {
	inRepo := func(repo string) []TaskSpecWithFiles {
		var out []TaskSpecWithFiles
		for _, t := range active {
			if strings.EqualFold(t.RepositoryID, repo) || (t.Repository != "" && strings.EqualFold(t.Repository, repo)) {
				out = append(out, t)
			}
		}
		return out
	}
	var out []Violation
	seen := map[string]bool{}
	for _, c := range brief.Criteria {
		for _, k := range c.Contracts {
			if !k.CrossRepository() {
				continue
			}
			producer, consumer := strings.TrimSpace(k.ProducerRepository), strings.TrimSpace(k.ConsumerRepository)
			key := strings.ToLower(strings.Join([]string{c.ID, k.Type, k.Name, producer, consumer}, "|"))
			if seen[key] {
				continue
			}
			seen[key] = true
			p, q := inRepo(producer), inRepo(consumer)
			if (len(p) == 0) == (len(q) == 0) {
				continue
			}
			name := fmt.Sprintf("%q", k.Name)
			if k.Type != "" {
				name = k.Type + " " + name
			}
			var msg string
			taskIndex := 0
			if len(p) > 0 {
				taskIndex = p[0].Index
				msg = fmt.Sprintf("%s's contract %s is changed only on the producer side (repository %s: %s); no task changes the consumer repository %s — add a task in %s that updates its use of the contract, or drop the consumer from the contract if it is unaffected",
					c.ID, name, producer, taskLabels(p), consumer, consumer)
			} else {
				taskIndex = q[0].Index
				msg = fmt.Sprintf("%s's contract %s is changed only on the consumer side (repository %s: %s); no task changes the producer repository %s — add a task in %s that provides the contract change the consumer relies on",
					c.ID, name, consumer, taskLabels(q), producer, producer)
			}
			out = append(out, Violation{Code: CodeOneSidedContract, TaskIndex: taskIndex, CriterionID: c.ID, Message: msg})
		}
	}
	return out
}
