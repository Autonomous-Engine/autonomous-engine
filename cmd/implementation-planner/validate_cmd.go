package main

// The -validate developer subcommand (M2-05): run the plan validator over a
// captured brief and a model's task list, with no database, bus or LLM —
// for tuning the decomposition prompt.
//
//	go run ./cmd/implementation-planner -validate [-root DIR] [-policy test,build] [-no-test-runner] brief.json tasks.json
//
// brief.json is a technical_brief artifact's content (its "criteria"
// object is used), a technical-brief.ready message, or a bare criteria
// object ({"schema_version":1,"criteria":[...]}). tasks.json is the model's
// decomposition output (the JSON array; fences and thinking tags are
// tolerated as in production). Steps are grouped into one subtask per
// repository_id, as the planner groups them. -root is the directory paths
// are stat'ed in (a checkout at the pinned base); without it the two stat
// rules (unevidenced_path, stale_evidence) are skipped. -policy lists the
// check kinds the repository's verification policy requires. The test-only
// filter is not applied (it needs the repository). Exit status: 0 when the
// plan is accepted (warnings allowed), 1 when it would be rejected, 2 on a
// usage or input error.

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/plan"
	"github.com/google/uuid"
)

// validateFlag is the first argument that selects the subcommand.
const validateFlag = "-validate"

func runValidateCommand(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("implementation-planner -validate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", "", "directory to stat paths in (a checkout at the pinned base); empty skips the stat rules")
	policy := fs.String("policy", "", "comma-separated check kinds the repository's verification policy requires (e.g. test,build)")
	noRunner := fs.Bool("no-test-runner", false, "the repository has no test runner: \"test\" is not demanded of the plan (policy or criteria)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: implementation-planner -validate [-root DIR] [-policy test,build] [-no-test-runner] brief.json tasks.json")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 2 {
		fs.Usage()
		return 2
	}
	briefData, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "read brief: %v\n", err)
		return 2
	}
	tasksData, err := os.ReadFile(fs.Arg(1))
	if err != nil {
		fmt.Fprintf(stderr, "read tasks: %v\n", err)
		return 2
	}
	brief, err := briefCriteriaFromFile(briefData)
	if err != nil {
		fmt.Fprintf(stderr, "brief: %v\n", err)
		return 2
	}
	raw, err := parseTaskArray(string(tasksData))
	if err != nil {
		fmt.Fprintf(stderr, "tasks: %v\n", err)
		return 2
	}
	var policyChecks []evidence.CheckKind
	for _, k := range strings.Split(*policy, ",") {
		if k = strings.TrimSpace(k); k != "" {
			policyChecks = append(policyChecks, plan.NormalizeCheckKind(evidence.CheckKind(k)))
		}
	}

	vs, folds, strictErr := validateCaptured(brief, raw, *root, policyChecks, *noRunner)
	for _, f := range folds {
		fmt.Fprintf(stdout, "fold     %s\n", f)
	}
	if strictErr != nil {
		// The strict pass rejects before validation in production too (and
		// re-prompts once with this reason).
		fmt.Fprintf(stdout, "rejected by the strict task contract (M2-04): %v\n", strictErr)
		return 1
	}
	errs, warns := 0, 0
	for _, v := range vs {
		where := ""
		switch {
		case v.TaskIndex > 0 && v.CriterionID != "":
			where = fmt.Sprintf("task %d / %s: ", v.TaskIndex, v.CriterionID)
		case v.TaskIndex > 0:
			where = fmt.Sprintf("task %d: ", v.TaskIndex)
		case v.CriterionID != "":
			where = v.CriterionID + ": "
		}
		fmt.Fprintf(stdout, "%-8s [%s] %s%s\n", v.Severity, v.Code, where, v.Message)
		if v.Severity == plan.SeverityWarning {
			warns++
		} else {
			errs++
		}
	}
	fmt.Fprintf(stdout, "%d task(s), %d criteria: %d error(s), %d warning(s) — plan %s\n",
		len(raw), criteriaCount(brief), errs, warns, map[bool]string{true: "REJECTED", false: "accepted"}[errs > 0])
	if errs > 0 {
		return 1
	}
	return 0
}

// validateCaptured runs the planner's validation pipeline over a captured
// plan without the database: number the steps, the strict contract pass,
// fold the criteria, drop non-implementation steps, group by repository_id,
// resolve dependencies, validate.
func validateCaptured(brief *plan.BriefCriteria, raw []decomposedTask, root string, policyChecks []evidence.CheckKind, noTestRunner bool) ([]plan.Violation, []plan.Fold, error) {
	numberSteps(raw)
	// As in production: the brief's derived duplicates are folded before
	// the prompt (so normalisation and the strict pass see the folded brief
	// and its folds), the plan's derived criteria after the decomposition.
	brief, briefFolds := foldBrief(uuid.Nil, brief)
	normaliseDerivedCriteria(raw, brief, briefFolds)
	citeOwnDerivedCriteria(raw)
	if err := validateDecomposedTasks(raw, brief, briefFolds); err != nil {
		return nil, nil, err
	}
	criteria, folds := foldPlanCriteria(uuid.Nil, brief, briefFolds, raw)
	return capturedBuild(criteria, raw, root, policyChecks, noTestRunner).validate(), folds, nil
}

// capturedBuild is phase 1 without the database: steps grouped by their
// repository_id into one subtask each, non-implementation steps dropped,
// paths stat'ed under root (every repository) or not at all.
func capturedBuild(criteria *plan.BriefCriteria, raw []decomposedTask, root string, policyChecks []evidence.CheckKind, noTestRunner bool) *planBuild {
	kept := map[int]bool{}
	for _, t := range filterInvalidTasks(raw) {
		kept[t.index] = true
	}
	if len(kept) == 0 && len(raw) > 0 {
		b := &planBuild{raw: raw, criteria: criteria, statRoots: map[string]string{}, skipStat: root == ""}
		b.markAllNonCode()
		return b
	}
	var order []string
	groups := map[string][]decomposedTask{}
	var specs []plan.TaskSpecWithFiles
	for _, t := range raw {
		if !kept[t.index] {
			s := stepSpec(t, "", "", nil)
			s.Dropped = nonImplementationDropReason
			s.NonCode = true
			specs = append(specs, s)
			continue
		}
		repo := t.RepositoryID
		if repo == "" {
			repo = "repository"
		}
		if _, ok := groups[repo]; !ok {
			order = append(order, repo)
		}
		groups[repo] = append(groups[repo], t)
		s := stepSpec(t, repo, repo, policyChecks)
		s.NoTestRunner = noTestRunner
		specs = append(specs, s)
	}
	planned := make([]plannedSubtask, 0, len(order))
	for _, repo := range order {
		steps := groups[repo]
		title := steps[0].Title
		if len(steps) > 1 {
			title = fmt.Sprintf("Implement in %s: %s (+%d more steps)", repo, steps[0].Title, len(steps)-1)
		}
		ps := plannedSubtask{repoName: repo, steps: len(steps), stepTasks: steps, msg: implementationTaskMessage{Title: title}, criteria: criteria}
		// The message's task_spec and criteria, as phase 1 builds them (no
		// clone: every file is unverified).
		var briefEvidence map[string]plan.EvidenceRef
		if criteria != nil {
			briefEvidence = criteria.FileEvidence
		}
		spec := buildTaskSpec(steps, groundTaskFilesAt("", "", plan.BasisClone, groupFileClaims(steps), briefEvidence))
		ps.msg.TaskSpec = &spec
		ps.msg.Criteria = relevantCriteria(spec, criteria, nil)
		for _, t := range steps {
			ps.titles = append(ps.titles, t.Title)
			ps.dependsOnTitles = append(ps.dependsOnTitles, t.DependsOn...)
		}
		planned = append(planned, ps)
	}
	b := &planBuild{raw: raw, planned: planned, criteria: criteria, specs: specs,
		graph: planDependencyGraph(planned, planSteps(raw)), statRoots: map[string]string{}, skipStat: root == ""}
	for _, repo := range order {
		b.statRoots[repo] = root
	}
	return b
}

// briefCriteriaFromFile reads the criteria from a technical_brief artifact's
// content, a technical-brief.ready message, or a bare criteria object.
func briefCriteriaFromFile(data []byte) (*plan.BriefCriteria, error) {
	var probe struct {
		Criteria json.RawMessage `json:"criteria"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	raw := strings.TrimSpace(string(probe.Criteria))
	switch {
	case raw == "" || raw == "null":
		return nil, nil // a legacy brief: no criteria
	case strings.HasPrefix(raw, "["):
		return plan.ParseBriefCriteria(data)
	default:
		return plan.ParseBriefCriteria(probe.Criteria)
	}
}

func criteriaCount(b *plan.BriefCriteria) int {
	if b == nil {
		return 0
	}
	return len(b.Criteria)
}
