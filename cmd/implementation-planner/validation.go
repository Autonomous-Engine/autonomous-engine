package main

// Plan validation (M2-05): decompose → build the plan → validate → one
// repair round → validate again → block with the violations, or commit.
//
// Phase 1 of the decomposition (grouping the model's steps into one subtask
// per repository, grounding their files, building each subtask's message) is
// planInputs.build. It writes nothing, so it can run once per attempt. The
// plan it builds is checked by plan.ValidatePlan (internal/plan/validate.go,
// pure) against the brief's criteria — folded first: a derived criterion that
// duplicates a user criterion is merged into it — the repository snapshot and
// M1-06's dependency graph. Violations are handed back to the model verbatim
// for ONE repair round (a second one when the first made progress and the
// item's spend allows it, R9 P2); a plan still failing blocks the parent,
// with a plan-validation artifact, in one guarded transaction. Warnings
// (hypothesis_only_task) never block; they are shown in the subtask
// description. A plan that passes is committed by commitDecomposition exactly
// as before. How each violation code is remedied is internal/plan's
// remediation policy (R9 P1).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/autonomousengine/autonomous-engine/internal/codex"
	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/guards"
	"github.com/autonomousengine/autonomous-engine/internal/metrics"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/plan"
	"github.com/autonomousengine/autonomous-engine/internal/snapshot"
	"github.com/autonomousengine/autonomous-engine/internal/testdetect"
	"github.com/autonomousengine/autonomous-engine/internal/verifypolicy"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// maxPlanAttempts is the first decomposition plus ONE repair round.
const maxPlanAttempts = 2

// planValidationArtifactName names the technical_brief artifact that records
// a rejected plan (content.name). Readers of the technical brief skip it.
const planValidationArtifactName = "plan-validation"

// planValidationBlockShown is how many violations the block reason lists.
const planValidationBlockShown = 5

// planInputs is everything phase 1 reads that does not depend on the model's
// output: resolved once per technical-brief.ready, used for every attempt.
type planInputs struct {
	pool         *pgxpool.Pool
	workID       uuid.UUID
	productID    string // the payload's product_id, as received
	productIDPtr *uuid.UUID
	structured   structuredBrief
	// briefFolds are the folds applied to structured.Criteria before the
	// prompt (foldBrief); build rewrites citations of the folded ids.
	briefFolds    []plan.Fold
	codexPatterns string
	codexVersion  int
	// codexPin is the parent's codex pin (migration 060), nil when unpinned.
	codexPin   *codex.Pin
	parentItem *models.WorkItem
	repoIndex  map[string]string
	parentPins map[uuid.UUID]snapshot.Pin
}

// planBuild is one attempt's phase-1 plan and the validator's view of it.
type planBuild struct {
	raw     []decomposedTask
	planned []plannedSubtask
	// criteria are the criteria tasks may cite: the brief's and the plan's
	// derived ones, folded. nil when the brief has none and the plan adds
	// none.
	criteria *plan.BriefCriteria
	folds    []plan.Fold
	// specs is every step of the model's output, kept or dropped (Dropped).
	specs []plan.TaskSpecWithFiles
	graph plan.DependencyGraph
	// dropped maps a step's 1-based index to why the planner dropped it.
	dropped map[int]string
	// statRoots maps repository id to the directory its paths are stat'ed
	// in ("" when the repository has no clone path).
	statRoots map[string]string
	// skipStat: no repository can be stat'ed at all (the -validate
	// subcommand without -root); the stat rules are skipped.
	skipStat bool
	// noRepository: no step resolved to a repository (the parent is blocked
	// with the pre-M2-05 reason; there is nothing to validate).
	noRepository bool
	// allNonCode: every step was dropped as non-code work (R3 review); no
	// subtask is planned, every criterion is marked non-code, and the
	// parent is blocked with allNonCodeReason.
	allNonCode bool
	// rehomed are the criteria resolveOrphans attached to a step by file
	// overlap after the repair round (R8); unowned the ids it recorded as
	// gaps because no step's files cover them.
	rehomed []rehomedCriterion
	unowned []string
	// attached are the checks attachChecks attached (R9: verifier_missing's
	// normaliser).
	attached []attachedCheck
}

// markAllNonCode records a plan whose every step was dropped as non-code
// work: the dropped specs, and every criterion marked non-code (cited ones
// with their dropped steps, the rest because no step cites them).
func (b *planBuild) markAllNonCode() {
	b.allNonCode = true
	b.specs = b.specs[:0]
	for _, t := range b.raw {
		s := stepSpec(t, "", "", nil)
		s.Dropped = nonImplementationDropReason
		s.NonCode = true
		b.specs = append(b.specs, s)
	}
	if b.criteria == nil {
		return
	}
	marks := plan.NonCodeCriteria(*b.criteria, b.specs)
	for _, c := range b.criteria.Criteria {
		if _, ok := marks[c.ID]; !ok {
			marks[c.ID] = "no step of the plan cites it, and every step is non-code work (process or deployment; none names a repository file)"
		}
	}
	marked := *b.criteria
	marked.Criteria = plan.MarkNonCode(b.criteria.Criteria, marks)
	b.criteria = &marked
}

// allNonCodeReason is the block reason for an all-non-code plan.
func (b *planBuild) allNonCodeReason() string {
	reason := fmt.Sprintf("Planning found nothing to implement: all %d decomposed step(s) are non-code work (process or deployment; no step names a repository file), so no implementation subtask was created.", len(b.raw))
	if b.criteria != nil {
		var gaps []string
		for _, c := range b.criteria.Criteria {
			if c.NonCode {
				gaps = append(gaps, plan.NonCodeGap(c))
			}
		}
		if len(gaps) > 0 {
			reason += " Non-code criteria: " + strings.Join(gaps, " | ") + "."
		}
	}
	return reason + " A human must do or verify this work, or retry from technical_brief with a request that changes repository files."
}

// numberSteps gives every step its 1-based position in the model's output,
// the number validation messages use.
func numberSteps(tasks []decomposedTask) {
	for i := range tasks {
		tasks[i].index = i + 1
	}
}

// foldBrief folds the brief's own derived duplicates of user criteria
// (plan.FoldDerivedDuplicates) before the decomposition prompt. The folds are
// kept: the model may still cite a folded id (it is in research's artifact),
// and foldPlanCriteria rewrites those citations with them.
func foldBrief(workID uuid.UUID, brief *plan.BriefCriteria) (*plan.BriefCriteria, []plan.Fold) {
	if brief == nil {
		return nil, nil
	}
	folded := *brief
	// R3: strip any non-code mark the brief arrived with (a copy: the
	// caller's slice is not modified); only attachNonCodeCriteria sets one.
	folded.Criteria = append([]plan.Criterion(nil), brief.Criteria...)
	plan.StripNonCode(&folded)
	var folds []plan.Fold
	folded.Criteria, folds = plan.FoldDerivedDuplicates(folded.Criteria)
	for _, f := range folds {
		log.Printf("implementation-planner: work_id=%s: brief criterion %s", workID, f)
	}
	return &folded, folds
}

// foldPlanCriteria rewrites the steps' citations of ids the brief fold
// removed (briefFolds), merges the (already folded) brief's criteria with the
// plan's derived ones, and folds derived duplicates of user criteria among
// them, rewriting the citations again. It returns the criteria and every
// fold applied (the brief's first). nil criteria when there are none.
func foldPlanCriteria(workID uuid.UUID, brief *plan.BriefCriteria, briefFolds []plan.Fold, raw []decomposedTask) (*plan.BriefCriteria, []plan.Fold) {
	if len(briefFolds) > 0 {
		for i := range raw {
			raw[i].CriterionIDs = plan.RewriteCriterionIDs(raw[i].CriterionIDs, briefFolds)
		}
	}
	derived := planDerivedCriteria(raw)
	if (brief == nil || len(brief.Criteria) == 0) && len(derived) == 0 {
		return brief, briefFolds
	}
	combined := plan.BriefCriteria{SchemaVersion: plan.SchemaVersion}
	if brief != nil {
		combined.FileEvidence = brief.FileEvidence
		combined.Criteria = append(combined.Criteria, brief.Criteria...)
	}
	ids := make([]string, 0, len(derived))
	for id := range derived {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return criterionNumber(ids[i]) < criterionNumber(ids[j]) })
	for _, id := range ids {
		c := derived[id]
		log.Printf("implementation-planner: work_id=%s: planner added derived criterion %s: %q (reason: %s)", workID, id, c.Text, c.Source.Reason)
		combined.Criteria = append(combined.Criteria, c)
	}
	kept, folds := plan.FoldDerivedDuplicates(combined.Criteria)
	combined.Criteria = kept
	for _, f := range folds {
		log.Printf("implementation-planner: work_id=%s: criterion %s", workID, f)
	}
	if len(folds) > 0 {
		for i := range raw {
			raw[i].CriterionIDs = plan.RewriteCriterionIDs(raw[i].CriterionIDs, folds)
		}
	}
	return &combined, append(append([]plan.Fold(nil), briefFolds...), folds...)
}

func criterionNumber(id string) int {
	n, err := strconv.Atoi(strings.TrimPrefix(id, "AC-"))
	if err != nil {
		return 1 << 30
	}
	return n
}

// stepSpec is the validator's view of one step: its criteria, deliverable,
// verification and files as the model claimed them (an empty or absent
// evidence_ref is no claim, "hypothesis" an explicit mark).
func stepSpec(t decomposedTask, repoID, repoName string, policyChecks []evidence.CheckKind) plan.TaskSpecWithFiles {
	spec := plan.TaskSpec{
		SchemaVersion: plan.SchemaVersion,
		CriterionIDs:  t.CriterionIDs,
		Deliverable:   t.Deliverable,
		Verification:  t.Verification,
	}
	for _, c := range t.claims() {
		p := strings.TrimPrefix(strings.TrimSpace(c.Path), "./")
		if p == "" {
			continue
		}
		ev := plan.EvidenceRef{}
		if strings.TrimSpace(c.EvidenceRef) != "" {
			if kind, ref, err := parseEvidenceClaim(c.EvidenceRef); err == nil {
				ev = plan.EvidenceRef{Kind: kind, Ref: ref}
			}
		}
		spec.Files = append(spec.Files, plan.FileRef{Path: p, Evidence: ev})
	}
	return plan.TaskSpecWithFiles{
		Index: t.index, Title: t.Title, RepositoryID: repoID, Repository: repoName,
		Spec: spec, PolicyChecks: policyChecks,
	}
}

// policyCheckKinds are the check kinds the repository's verification policy
// (M3-03) requires, normalised. A policy that cannot be read is the
// fail-closed default (require test), as at the gate. The no-test-runner
// exemption is not applied here: the validator applies it to the policy's
// kinds and to the criteria's required checks alike (NoTestRunner).
func policyCheckKinds(ctx context.Context, q verifypolicy.Querier, repoID uuid.UUID) []evidence.CheckKind {
	p, _, err := verifypolicy.LoadVerificationPolicy(ctx, q, repoID)
	if err != nil {
		log.Printf("implementation-planner: verification policy for repository %s: %v — using the default", repoID, err)
	}
	var out []evidence.CheckKind
	for _, k := range p.Normalize().RequiredChecks {
		out = append(out, plan.NormalizeCheckKind(k))
	}
	return out
}

// statRootFor is where a repository's paths are stat'ed: the snapshot
// worktree at the parent's pin (<clone>/.git/ae-snapshots/<sha12>, created by
// research-agent, M2-03) when it exists, basis snapshot; otherwise the clone,
// basis clone. The planner never creates a snapshot (its repo-cache mount is
// read-only).
func statRootFor(clonePath string, pin snapshot.Pin) (string, string) {
	if clonePath == "" {
		return "", plan.BasisClone
	}
	if snapshot.ValidSHA(pin.BaseSHA) {
		gitDir := filepath.Join(clonePath, ".git")
		if st, err := os.Stat(gitDir); err == nil && st.IsDir() {
			wt := snapshot.WorktreePathIn(gitDir, pin.BaseSHA)
			if st, err := os.Stat(wt); err == nil && st.IsDir() {
				if _, err := os.Stat(filepath.Join(wt, ".git")); err == nil {
					return wt, plan.BasisSnapshot
				}
			}
		}
	}
	return clonePath, plan.BasisClone
}

// snapshotStat is the validator's view of the repositories: whether path
// exists under the repository's stat root. A path that escapes the
// repository does not exist. A repository with no stat root (no clone path)
// cannot be checked, and its paths are taken to exist: stale_evidence then
// never fires there, and an unclaimed path must still be marked.
func (b *planBuild) snapshotStat(repoID, path string) bool {
	root, ok := b.statRoots[repoID]
	if !ok || root == "" {
		return true
	}
	rel, inside := containedPath(path, root)
	if !inside {
		return false
	}
	_, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
	return err == nil
}

// validate runs the plan validator over this attempt.
func (b *planBuild) validate() []plan.Violation {
	brief := plan.BriefCriteria{SchemaVersion: plan.SchemaVersion}
	if b.criteria != nil {
		brief = *b.criteria
	}
	stat := b.snapshotStat
	if b.skipStat {
		stat = nil
	}
	return b.unownedViolations(plan.ValidatePlan(brief, b.specs, b.graph, stat))
}

// build is phase 1 of the decomposition for one attempt: fold the criteria,
// drop non-implementation steps, group the rest into one subtask per
// repository (dropping test-only steps where there is no test runner),
// ground every file, and resolve the dependency graph. No database writes.
func (in *planInputs) build(ctx context.Context, raw []decomposedTask) *planBuild {
	numberSteps(raw)
	b := &planBuild{raw: raw, dropped: map[int]string{}, statRoots: map[string]string{}}
	b.criteria, b.folds = foldPlanCriteria(in.workID, in.structured.Criteria, in.briefFolds, raw)

	pool := in.pool
	workID := in.workID
	structured := in.structured
	codexPatterns := in.codexPatterns
	codexVersion := in.codexVersion
	parentItem := in.parentItem
	repoIndex := in.repoIndex
	parentPins := in.parentPins
	productIDPtr := in.productIDPtr
	payload := struct{ ProductID string }{ProductID: in.productID}

	tasks := filterInvalidTasks(raw)
	kept := map[int]bool{}
	for _, t := range tasks {
		kept[t.index] = true
	}
	for _, t := range raw {
		if !kept[t.index] {
			b.dropped[t.index] = nonImplementationDropReason
		}
	}
	log.Printf("implementation-planner: decomposed %d tasks (%d after filtering) for work_id=%s", len(raw), len(tasks), workID)
	if len(tasks) == 0 && len(raw) > 0 {
		b.markAllNonCode()
		return b
	}

	resolveRepoID := func(t decomposedTask) *uuid.UUID {
		if parentItem != nil && parentItem.RepositoryID != nil {
			return parentItem.RepositoryID
		}
		if parentItem != nil && parentItem.ProductID != nil {
			var productRepoID uuid.UUID
			if err := pool.QueryRow(ctx,
				`SELECT id FROM repositories WHERE product_id = $1 ORDER BY created_at LIMIT 1`,
				*parentItem.ProductID,
			).Scan(&productRepoID); err == nil {
				return &productRepoID
			}
		}
		repoID := t.RepositoryID
		if repoID == "" && len(t.Files) > 0 && len(repoIndex) > 0 {
			repoID = inferRepositoryID(t.Files, repoIndex)
		}
		if repoID != "" {
			if rid, parseErr := uuid.Parse(repoID); parseErr == nil {
				var exists bool
				if err := pool.QueryRow(ctx,
					`SELECT EXISTS(SELECT 1 FROM repositories WHERE id = $1)`,
					rid,
				).Scan(&exists); err == nil && exists {
					return &rid
				}
			}
		}
		return nil
	}

	type repoGroup struct {
		tasks []decomposedTask
	}
	groups := make(map[uuid.UUID]*repoGroup)
	groupOrder := []uuid.UUID{} // preserve deterministic iteration order
	unassigned := []string{}
	for _, t := range tasks {
		rid := resolveRepoID(t)
		if rid == nil {
			unassigned = append(unassigned, t.Title)
			b.dropped[t.index] = "no repository_id that maps to a repository of this product"
			continue
		}
		g, ok := groups[*rid]
		if !ok {
			g = &repoGroup{}
			groups[*rid] = g
			groupOrder = append(groupOrder, *rid)
		}
		g.tasks = append(g.tasks, t)
	}
	if len(unassigned) > 0 {
		log.Printf("implementation-planner: %d task(s) had no resolvable repository_id and were dropped: %v", len(unassigned), unassigned)
	}
	if len(groups) == 0 {
		b.noRepository = true
		return b
	}

	b.planned = make([]plannedSubtask, 0, len(groupOrder))
	for _, rid := range groupOrder {
		group := groups[rid]
		ridCopy := rid // loop-var capture safety for the pointer below
		taskID := uuid.New().String()

		var repoName, repoClonePath string
		_ = pool.QueryRow(ctx,
			`SELECT name, clone_path FROM repositories WHERE id = $1`, rid,
		).Scan(&repoName, &repoClonePath)
		if repoName == "" {
			repoName = rid.String()
		}
		// M2-05: paths are stat'ed at the pinned snapshot when research left
		// its snapshot worktree, otherwise in the clone (legacy-unpinned).
		statRoot, statBasis := statRootFor(repoClonePath, parentPins[rid])
		b.statRoots[rid.String()] = statRoot

		// Test-infrastructure gate: if the repo has no test runner, drop
		// test-only tasks from the consolidated description and flag the
		// subtask so the implementation-agent's prompt override fires. The
		// implementation-agent also performs this check, but filtering
		// here keeps the consolidated Step list clean instead of telling
		// the agent to "Write Test Cases" and then telling it 'OVERRIDE —
		// no tests.' That contradiction wastes iterations.
		//
		// Exception: if the work item ITSELF is a request to add testing
		// (title/description says "add tests", "set up testing", etc.),
		// the filter must NOT fire — the user explicitly asked for tests
		// and the agent needs to create both the infra and the test files.
		hasTestInfra := true
		if repoClonePath != "" {
			if cmd := testdetect.DetectTestCommand(repoClonePath); cmd == "" {
				hasTestInfra = false
			}
		}
		userAskedForTests := false
		if parentItem != nil {
			userAskedForTests = testdetect.FeatureRequestsTests(parentItem.Title, parentItem.Description)
		}
		if !hasTestInfra && userAskedForTests {
			log.Printf("implementation-planner: %s has no test runner but the feature request is about adding tests — NOT filtering test tasks", repoName)
		}
		if !hasTestInfra && !userAskedForTests {
			filtered := group.tasks[:0]
			var droppedTitles []string
			for _, t := range group.tasks {
				if isTestOnlyTask(t) {
					b.dropped[t.index] = fmt.Sprintf("test-only, and repository %s has no test runner", repoName)
					droppedTitles = append(droppedTitles, t.Title)
					if len(t.CriterionIDs) > 0 {
						// M2-04: the criteria this step served lose a task here;
						// coverage is M2-05's check, so name them now.
						log.Printf("implementation-planner: %s: test-only step %q dropped (no test runner) — its criteria %v lose this task", repoName, t.Title, t.CriterionIDs)
					}
					continue
				}
				filtered = append(filtered, t)
			}
			group.tasks = filtered
			if len(droppedTitles) > 0 {
				log.Printf("implementation-planner: %s has no test runner — dropped %d test-only tasks: %v", repoName, len(droppedTitles), droppedTitles)
			}
			if len(group.tasks) == 0 {
				log.Printf("implementation-planner: all tasks for %s were test-only and the repo has no test runner — nothing to implement for this repo", repoName)
				continue
			}
		}

		// M2-05: the validator's view of each kept step, with the kinds the
		// repository's verification policy requires that this plan can carry.
		policyChecks := policyCheckKinds(ctx, pool, rid)
		for _, t := range group.tasks {
			s := stepSpec(t, rid.String(), repoName, policyChecks)
			s.NoTestRunner = !hasTestInfra && !userAskedForTests
			b.specs = append(b.specs, s)
		}

		combinedTitle := group.tasks[0].Title
		if len(group.tasks) > 1 {
			combinedTitle = fmt.Sprintf("Implement in %s: %s (+%d more steps)", repoName, group.tasks[0].Title, len(group.tasks)-1)
		}

		var desc strings.Builder
		if len(group.tasks) > 1 {
			fmt.Fprintf(&desc, "Implement the following %d coordinated steps in repository %s on a single branch. All changes land in one commit sequence on one branch — you are the single implementer for this repo.\n\n",
				len(group.tasks), repoName)
		}
		for i, t := range group.tasks {
			fmt.Fprintf(&desc, "## Step %d: %s\n\n%s\n", i+1, t.Title, t.Description)
			if len(t.Files) > 0 {
				fmt.Fprintf(&desc, "Files: %v\n", t.Files)
			}
			if len(t.DependsOn) > 0 {
				fmt.Fprintf(&desc, "Depends on (within this repo): %v\n", t.DependsOn)
			}
			desc.WriteString(renderStepSpec(t))
			desc.WriteString("\n")
		}

		// Grounding gate: validate every file against the actual repo
		// filesystem AND against reachability. Two failure modes we've
		// seen:
		//   1. Path doesn't exist on disk (fictional). Drop it.
		//   2. Path exists but is never imported by any other file
		//      (orphan component). The agent will edit it successfully
		//      but the edit has no effect on the live site — the PR #9
		//      failure mode. Demote it and tell the model to use
		//      codex_find_references / search_code to locate the real
		//      entry point.
		//
		// M2-04: the stat result is evidence (groundTaskFiles). An existing
		// path is repo evidence (or research's codex/lsp/repo evidence); a
		// missing one stays out of files_to_change as before but is kept in
		// the TaskSpec as a hypothesis with the grounding note; a path the
		// model marked "new" is neither dropped nor reported missing.
		var briefEvidence map[string]plan.EvidenceRef
		if structured.Criteria != nil {
			briefEvidence = structured.Criteria.FileEvidence
		}
		grounded := groundTaskFilesAt(repoClonePath, statRoot, statBasis, groupFileClaims(group.tasks), briefEvidence)
		validatedFiles := make([]string, 0, len(grounded))
		var droppedNonexistent []string
		var orphanPaths []string
		for _, g := range grounded {
			f := g.ref.Path
			switch g.status {
			case fileUnverified:
				// No clone path — can't validate. Trust the hint, the
				// agent's read-before-edit gate will catch bad paths.
				validatedFiles = append(validatedFiles, f)
			case fileMissing:
				droppedNonexistent = append(droppedNonexistent, f)
			case fileExists:
				if isLikelyOrphan(repoClonePath, f) {
					orphanPaths = append(orphanPaths, f)
					// Still keep the file in the list — the model may want
					// to delete it or wire it up. The warning below biases
					// the model toward investigating first.
				}
				validatedFiles = append(validatedFiles, f)
			}
		}
		taskSpec := buildTaskSpec(group.tasks, grounded)
		specCriteria := relevantCriteria(taskSpec, b.criteria, nil)
		desc.WriteString(renderSubtaskSpec(taskSpec, specCriteria))
		if len(droppedNonexistent) > 0 {
			log.Printf("implementation-planner: %s: dropped %d file paths that don't exist on disk: %v", repoName, len(droppedNonexistent), droppedNonexistent)
			fmt.Fprintf(&desc, "\n## Grounding note — missing files\n\nResearch suggested these file paths but they DO NOT exist in the repository: %v\n\nDo NOT recreate them under made-up paths. Use `codex_find_module`, `codex_workspace_symbols`, or `search_code` to locate the real files that implement this area of the app, `read_file` to confirm their structure, and THEN edit them.\n", droppedNonexistent)
		}
		if len(orphanPaths) > 0 {
			log.Printf("implementation-planner: %s: %d file(s) in scope appear orphan (not imported anywhere): %v", repoName, len(orphanPaths), orphanPaths)
			fmt.Fprintf(&desc, "\n## Grounding note — orphan files\n\nThese files exist but are NOT imported anywhere in the repository: %v\n\nEditing them has NO EFFECT on the live site — they ship as dead code. Before editing, use `codex_find_references` or `search_code` to locate the file that is actually rendered on the affected page (for Next.js projects: start from `app/**/page.tsx` and follow the `use client` component it renders, or from `app/api/**/route.ts` for API endpoints). Edit THAT file, not this one. If you conclude an orphan file should genuinely be wired up, do it explicitly by editing the importing file as well.\n", orphanPaths)
		}
		files := validatedFiles

		subItem := &models.WorkItem{
			ID:           uuid.MustParse(taskID),
			Type:         models.WorkItemTypeSubtask,
			State:        models.WorkItemStateImplementation,
			Title:        combinedTitle,
			Description:  desc.String(),
			Priority:     models.PriorityMedium,
			CodexVersion: codexVersion,
			ParentID:     &workID,
			ProductID:    productIDPtr,
			RepositoryID: &ridCopy,
		}

		filesToChange := plannedFilesToChange(grounded, structured.AffectedFiles, desc.String(), &taskSpec)
		log.Printf("implementation-planner: %s: files from LLM=%v (%d), from research=%v (%d), files_to_change=%v (%d)",
			repoName, files, len(files), structured.AffectedFiles, len(structured.AffectedFiles), filesToChange, len(filesToChange))

		taskMsg := implementationTaskMessage{
			TaskID:               taskID,
			WorkID:               workID.String(),
			ProductID:            payload.ProductID,
			RepositoryID:         rid.String(),
			Title:                combinedTitle,
			Description:          desc.String(),
			FilesToChange:        filesToChange,
			Dependencies:         nil, // set by applyPlanDependencies once the whole plan is known
			CodexVersion:         codexVersion,
			TechnicalBrief:       structured.proseContent(),
			CodexPatterns:        codexPatterns,
			AffectedModules:      structured.AffectedModules,
			RelevantPatterns:     structured.RelevantPatterns,
			RelevantAntiPatterns: structured.RelevantAntiPatterns,
			CrossRepoTouchpoints: structured.CrossRepoTouchpoints,
			TaskSpec:             &taskSpec,
			Criteria:             specCriteria,
			LineSeeds:            seedsForTask(structured.LineSeeds, filesToChange, &taskSpec),
			PRRequirements:       structured.PRRequirements,
		}
		if pin, ok := parentPins[rid]; ok {
			taskMsg.BaseSHA = pin.BaseSHA
			taskMsg.BaseRef = pin.BaseRef
		} else {
			snapshot.NoteUnpinned(agentID, workID, "no parent pin for repository "+repoName)
		}
		applyCodexPin(&taskMsg, in.codexPin)

		// Granular titles that resolve to this repo-scoped subtask, and the
		// titles those steps depend on: the input to dependency resolution,
		// which needs the whole plan and so runs after this loop.
		titles := make([]string, 0, len(group.tasks))
		var dependsOnTitles []string
		for _, t := range group.tasks {
			titles = append(titles, t.Title)
			dependsOnTitles = append(dependsOnTitles, t.DependsOn...)
		}

		b.planned = append(b.planned, plannedSubtask{
			repoName:        repoName,
			steps:           len(group.tasks),
			titles:          titles,
			dependsOnTitles: dependsOnTitles,
			item:            subItem,
			msg:             taskMsg,
			stepTasks:       group.tasks,
			criteria:        b.criteria,
		})
	}

	for _, t := range raw {
		if reason, ok := b.dropped[t.index]; ok {
			s := stepSpec(t, "", "", nil)
			s.Dropped = reason
			s.NonCode = reason == nonImplementationDropReason
			b.specs = append(b.specs, s)
		}
	}
	sort.SliceStable(b.specs, func(i, j int) bool { return b.specs[i].Index < b.specs[j].Index })
	b.graph = planDependencyGraph(b.planned, planSteps(raw))
	return b
}

// planOutcome is how decomposition ended.
type planOutcome struct {
	build *planBuild
	// violations are the last attempt's (errors and warnings).
	violations []plan.Violation
	attempts   int
	// decomposeErr: the model could not produce a task list (block with
	// decompositionBlockReason, through blockForDecompositionFailure).
	decomposeErr error
	// previous are the first attempt's violations when the outcome comes
	// from the repair round, so a repair that ends for another reason (a
	// decomposition failure, no repository, nothing to implement) can say
	// why it was asked for.
	previous []plan.Violation
	// orphanNote is a sentence for the block reason when the plan was left
	// blocked because it had more orphaned criteria than the planner
	// re-homes or records as gaps (R8).
	orphanNote string
	// roundNote is a sentence for the block reason when a further repair
	// round was earned by progress but not tried (R9 P2: the spend limit).
	roundNote string
}

// blockNote is what the plan-validation block reason adds to the
// violations: the orphan threshold (R8) and the skipped round (R9).
func (o planOutcome) blockNote() string { return o.orphanNote + o.roundNote }

// previousNote is a sentence for a block or completion reason when the
// repair round ended for a reason other than validation: what the previous
// plan was rejected for. "" when there was no repair round.
func (o planOutcome) previousNote() string {
	errs := plan.Errors(o.previous)
	if len(errs) == 0 {
		return ""
	}
	return fmt.Sprintf(" This was the plan-validation repair round: the previous plan had been rejected with %d violation(s): %s.",
		len(errs), plan.SummarizeViolations(errs, planValidationBlockShown))
}

// attemptsPhrase says what the attempts were: "the decomposition and one
// repair round", "the decomposition and 2 repair rounds".
func attemptsPhrase(attempts int) string {
	switch {
	case attempts <= 1:
		return "the decomposition"
	case attempts == 2:
		return "the decomposition and one repair round"
	}
	return fmt.Sprintf("the decomposition and %d repair rounds", attempts-1)
}

// rejected reports whether the plan failed validation after the repair round.
func (o planOutcome) rejected() bool { return plan.HasErrors(o.violations) }

// planWithRepair is decompose → build → validate, with ONE repair round: the
// first attempt's violations are appended to the prompt verbatim and the
// model plans again; the second attempt's verdict is final (planWithPolicy
// may allow one more round, R9 P2). A plan with no
// repository, or nothing to implement, is not validated (nothing would be
// published).
//
// R9 P1: it dispatches on the remediation policy (internal/plan
// remediation.go). A block code ends planning at once. Before a re-prompt,
// the normalise codes are normalised (rehomeOrphans), so no re-prompt is
// spent on what the planner fixes itself; the rest goes to the model with a
// hint per violation (repairHints). After the last round, the orphans left
// are re-homed or recorded as gaps within the R8 thresholds
// (resolveOrphans); gap codes are warnings and never block.
//
// planWithRepair allows no further round (repairBudget{}); the handler uses
// planWithPolicy with the work item's spend.
func planWithRepair(workID uuid.UUID, decompose func(repair string) ([]decomposedTask, error), build func([]decomposedTask) *planBuild) planOutcome {
	return planWithPolicy(workID, ignoreReserved(decompose), build, repairBudget{})
}

// ignoreReserved adapts a decomposition that numbers no derived criteria
// itself (a test's canned rounds) to planWithPolicy's.
func ignoreReserved(decompose func(repair string) ([]decomposedTask, error)) func(repair string, reserved []string) ([]decomposedTask, error) {
	return func(repair string, _ []string) ([]decomposedTask, error) { return decompose(repair) }
}

// extraRoundMaxSpendPercent: a further repair round (R9 P2) may start only
// while the work item has spent at most this share of its token budget
// (D4), so a planning round never eats the implementation's budget.
const extraRoundMaxSpendPercent = 60

// repairBudget is what the further repair round (R9 P2) is bounded by.
type repairBudget struct {
	// budget is the work item's token budget (D4, tools.TokenBudget()).
	budget int64
	// spent reads the work item's token spend so far; nil means unknown,
	// and an unknown spend allows no further round.
	spent func() (int64, error)
}

// workItemTokensSpent is the work item's token spend so far: the same sum
// the tool loop checks the budget against (internal/llm/tools RunToolLoop)
// and research-coordinator's budget hint reads (workItemTokensSpent there).
func workItemTokensSpent(ctx context.Context, pool *pgxpool.Pool, workID uuid.UUID) (int64, error) {
	if pool == nil {
		return 0, errors.New("no database")
	}
	var spent int64
	err := pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(input_tokens + output_tokens), 0) FROM inference_logs WHERE work_item_id = $1`,
		workID).Scan(&spent)
	return spent, err
}

// extraRound decides the further repair round (R9 P2): the repair round
// strictly reduced the violations the model was asked to fix (by
// plan.Violation.Key; plan.StrictlyFewer), what is left still needs the
// model (plan.NeedsModel: not only normalise or gap codes), and the work
// item has spent at most extraRoundMaxSpendPercent of its budget. note is a
// sentence for the block reason when the round was earned but not tried.
func extraRound(workID uuid.UUID, previous, current []plan.Violation, rb repairBudget) (ok bool, note string) {
	if !plan.NeedsModel(current) || !plan.StrictlyFewer(modelViolations(previous), modelViolations(current)) {
		return false, ""
	}
	fixed := len(plan.Errors(modelViolations(previous))) - len(plan.Errors(modelViolations(current)))
	lead := fmt.Sprintf(" The repair round fixed %d of the %d violation(s) it was asked to fix, but a second repair round was not tried: ",
		fixed, len(plan.Errors(modelViolations(previous))))
	if rb.spent == nil || rb.budget <= 0 {
		return false, lead + "could not read the work item's token spend."
	}
	spent, err := rb.spent()
	if err != nil {
		log.Printf("implementation-planner: work_id=%s: read token spend: %v — no second repair round", workID, err)
		return false, lead + "could not read the work item's token spend."
	}
	if spent*100 > rb.budget*extraRoundMaxSpendPercent {
		return false, fmt.Sprintf("%sthe work item has used %d of its %d-token budget, more than the %d%% a further round may start from.",
			lead, spent, rb.budget, extraRoundMaxSpendPercent)
	}
	log.Printf("implementation-planner: work_id=%s: the repair round made progress (%d fixed, nothing new) and the item has used %d of %d tokens — one more repair round", workID, fixed, spent, rb.budget)
	return true, ""
}

// modelViolations are the violations only the model can fix (reprompt
// codes, and normalise codes the normaliser left that fall back to the
// model); progress is measured on them, since the rest are
// the planner's to settle.
func modelViolations(vs []plan.Violation) []plan.Violation {
	var out []plan.Violation
	for _, v := range vs {
		if plan.ModelFixes(v) {
			out = append(out, v)
		}
	}
	return out
}

// hasRepromptCode reports whether any rejecting violation is of a reprompt
// code (not a normalise code the planner may settle).
func hasRepromptCode(vs []plan.Violation) bool {
	for _, v := range plan.Errors(vs) {
		if plan.RemediationOf(v) == plan.RemediationReprompt {
			return true
		}
	}
	return false
}

// planWithPolicy is planWithRepair with the further repair round (R9 P2)
// bounded by rb. decompose receives, besides the repair prompt, the ids the
// earlier rounds used (R13: carriedDerived.reservedIDs), which a fresh
// derived id must skip (decomposeIntoTasks).
func planWithPolicy(workID uuid.UUID, decompose func(repair string, reserved []string) ([]decomposedTask, error), build func([]decomposedTask) *planBuild, rb repairBudget) planOutcome {
	repair := ""
	var previous []plan.Violation
	limit := maxPlanAttempts
	roundNote := ""
	// R13 D2: the derived criteria of earlier rounds stay known.
	carried := newCarriedDerived()
	for attempt := 1; ; attempt++ {
		raw, err := decompose(repair, carried.reservedIDs())
		if err != nil {
			return planOutcome{attempts: attempt, decomposeErr: err, previous: previous}
		}
		for _, note := range carried.restore(raw) {
			log.Printf("implementation-planner: work_id=%s: plan attempt %d: %s", workID, attempt, note)
		}
		b := build(raw)
		carried.remember(b)
		if b.noRepository || len(b.planned) == 0 {
			return planOutcome{build: b, attempts: attempt, previous: previous}
		}
		vs := b.validate()
		countViolations(vs)
		for _, v := range vs {
			log.Printf("implementation-planner: work_id=%s: plan validation attempt %d: %s [%s] %s", workID, attempt, v.Severity, v.Code, v.Message)
		}
		if !plan.HasErrors(vs) {
			return planOutcome{build: b, violations: vs, attempts: attempt, previous: previous}
		}
		if blocks := plan.Blocks(vs); len(blocks) > 0 {
			log.Printf("implementation-planner: work_id=%s: %d violation(s) cannot be repaired — blocking without a repair round", workID, len(blocks))
			return planOutcome{build: b, violations: vs, attempts: attempt, previous: previous}
		}
		// R9 P1: the normalise codes' normalisers run before any re-prompt
		// and before the last round's verdict; they work on a copy, so the
		// model's plan (b, vs) is what a block records.
		nb, nvs := normalisePlan(workID, b, vs, build)
		if !plan.HasErrors(nvs) {
			return planOutcome{build: nb, violations: nvs, attempts: attempt, previous: previous}
		}
		if attempt == maxPlanAttempts && limit == maxPlanAttempts {
			ok, note := extraRound(workID, previous, nvs, rb)
			if ok {
				limit++
			}
			roundNote = note
		}
		if attempt >= limit {
			// R8: orphans the repair round left are re-homed by file
			// overlap or recorded as gaps, within the thresholds.
			fb, fvs, note := resolveOrphans(workID, nb, nvs, build)
			if plan.HasErrors(fvs) && hasRepromptCode(vs) {
				// The model's plan had problems only it could fix: the
				// block records that plan, not the planner's edits (R8:
				// an orphan next to another error is not resolved).
				return planOutcome{build: b, violations: vs, attempts: attempt, previous: previous, orphanNote: note, roundNote: roundNote}
			}
			return planOutcome{build: fb, violations: fvs, attempts: attempt, previous: previous, orphanNote: note, roundNote: roundNote}
		}
		previous = nvs
		repair = repairPrompt(nvs, nb.raw, repairHints(nb, nvs))
		log.Printf("implementation-planner: work_id=%s: plan rejected with %d violation(s) — repair round %d", workID, len(plan.Errors(nvs)), attempt)
	}
}

// repairPlanMaxBytes bounds the previous plan quoted in the repair prompt.
const repairPlanMaxBytes = 16 << 10

// repairPrompt is the repair round's addition to the decomposition prompt:
// every rejecting violation's message, verbatim, each followed by its hint
// (repairHints, keyed by plan.Violation.Key: for an orphan, the paths it
// names and the tasks that change them, R8; for a missing check, the owners
// and a known command; for a missing deliverable, the criterion the task
// cites; R9), then the warnings that have a hint, as not rejected, then the
// rejected plan itself (bounded to repairPlanMaxBytes) so "task 2" in a
// message can be resolved — the tasks are numbered from 1 in the array's
// order.
func repairPrompt(vs []plan.Violation, previous []decomposedTask, hints map[string]string) string {
	var b strings.Builder
	b.WriteString("Your previous plan was rejected by the plan validator. Fix every problem below and return the complete corrected JSON array (every task, not only the changed ones):\n")
	hinted := map[string]bool{}
	hint := func(v plan.Violation) {
		k := v.Key()
		if h := hints[k]; h != "" && !hinted[k] {
			hinted[k] = true
			fmt.Fprintf(&b, "  %s\n", h)
		}
	}
	for _, v := range plan.Errors(vs) {
		fmt.Fprintf(&b, "- %s\n", v.Message)
		hint(v)
	}
	var warned bool
	for _, v := range plan.Warnings(vs) {
		if hints[v.Key()] == "" {
			continue
		}
		if !warned {
			b.WriteString("\nNot rejected, but fix it if you can:\n")
			warned = true
		}
		fmt.Fprintf(&b, "- %s\n", v.Message)
		hint(v)
	}
	if len(previous) > 0 {
		if data, err := json.MarshalIndent(previous, "", " "); err == nil {
			b.WriteString("\nYour previous plan (task 1 is the first element):\n")
			if len(data) > repairPlanMaxBytes {
				b.Write(data[:repairPlanMaxBytes])
				fmt.Fprintf(&b, "\n... (truncated: %d of %d bytes shown)", repairPlanMaxBytes, len(data))
			} else {
				b.Write(data)
			}
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// planValidationReason is the block reason: the first five violations and
// the count.
func planValidationReason(errs []plan.Violation, attempts int) string {
	return fmt.Sprintf(
		"Plan validation failed after %d attempt(s) (%s): %d violation(s): %s. "+
			"Nothing was created and no implementation.tasks event was queued; the full list is in the plan-validation artifact. "+
			"Retry from technical_brief state to re-decompose.",
		attempts, attemptsPhrase(attempts), len(errs), plan.SummarizeViolations(errs, planValidationBlockShown))
}

// planValidationContent is the plan-validation artifact's content.
type planValidationContent struct {
	Name          string           `json:"name"`
	SchemaVersion int              `json:"schema_version"`
	Attempt       int              `json:"attempt"`
	Violations    []plan.Violation `json:"violations"`
	// Note is the orphan-threshold sentence of the block reason (R8).
	Note string `json:"note,omitempty"`
}

// addArtifactTx is the seam the integration test uses to fail the artifact
// insert after the block; production never reassigns it.
var addArtifactTx = func(ctx context.Context, tx pgx.Tx, a *models.WorkItemArtifact) error {
	return workgraph.New(nil).AddArtifactTx(ctx, tx, a)
}

// blockForPlanValidation blocks the parent with the violations as the reason
// and records them in a technical_brief artifact named plan-validation, in
// one transaction (guards.BlockWorkItemTx enqueues work.blocked with it). db
// is idempotency.Wrap(msgCtx, pool), so the delivery's guard row commits
// with the block. An already-blocked parent gets the artifact only. note
// (planOutcome.orphanNote, usually "") is appended to the reason.
func blockForPlanValidation(ctx context.Context, db workgraph.TxBeginner, workID uuid.UUID, violations []plan.Violation, attempts int, note string) error {
	content, err := json.Marshal(planValidationContent{
		Name: planValidationArtifactName, SchemaVersion: plan.SchemaVersion, Attempt: attempts, Violations: violations,
		Note: strings.TrimSpace(note),
	})
	if err != nil {
		return fmt.Errorf("encode plan-validation artifact: %w", err)
	}
	reason := planValidationReason(plan.Errors(violations), attempts) + note
	err = workgraph.WithTx(ctx, db, func(tx pgx.Tx) error {
		if err := guards.BlockWorkItemTx(ctx, tx, workID, workID, agentID, reason); err != nil {
			return err
		}
		return addArtifactTx(ctx, tx, &models.WorkItemArtifact{
			WorkItemID: workID,
			Type:       models.ArtifactTypeTechnicalBrief,
			Content:    content,
			AgentID:    agentID,
		})
	})
	if err != nil {
		return err
	}
	planValidationBlocks.Inc()
	return nil
}

// decompositionOutputError marks a decomposition that failed because of what
// the model returned (unparseable, or rejected by the strict pass after the
// one correction), as opposed to the provider (no LLM, a failed or empty
// call) or the brief.
type decompositionOutputError struct{ err error }

func (e *decompositionOutputError) Error() string { return e.err.Error() }
func (e *decompositionOutputError) Unwrap() error { return e.err }

// emptyBriefError marks a decomposition refused because the technical brief
// has no prose: neither the model's fault nor the provider's.
type emptyBriefError struct{ err error }

func (e *emptyBriefError) Error() string { return e.err.Error() }
func (e *emptyBriefError) Unwrap() error { return e.err }

// decompositionBlockReason is the block reason for a decomposition failure.
// A model-output failure says so and that Retry re-runs planning; an empty
// brief points at the brief; anything else keeps the provider wording.
func decompositionBlockReason(err error) string {
	var out *decompositionOutputError
	if errors.As(err, &out) {
		return fmt.Sprintf("Task decomposition failed: the model's task decomposition was invalid after one correction: %s. Retry re-runs planning.", out.Error())
	}
	var empty *emptyBriefError
	if errors.As(err, &empty) {
		return fmt.Sprintf("Task decomposition failed: the technical brief has no prose to decompose (%s). Research must produce a brief with a narrative; Retry re-runs planning against the current technical brief.", empty.Error())
	}
	return fmt.Sprintf("Task decomposition failed: %s. Retry from technical_brief state once the LLM provider is healthy.", err.Error())
}

// blockForDecompositionFailure blocks the parent with reason and enqueues
// work.blocked in one transaction (guards.BlockWorkItemTx), as
// blockForPlanValidation does: status-orchestrator's work.blocked consumer
// then raises the escalation prompt, so the dashboard shows an actionable
// prompt rather than a bare blocked item. db is idempotency.Wrap(msgCtx,
// pool), so the delivery's guard row commits with the block.
func blockForDecompositionFailure(ctx context.Context, db workgraph.TxBeginner, workID uuid.UUID, reason string) error {
	return workgraph.WithTx(ctx, db, func(tx pgx.Tx) error {
		return guards.BlockWorkItemTx(ctx, tx, workID, workID, agentID, reason)
	})
}

// attachNonCodeCriteria marks the plan's non-code criteria (R3: owned by no
// kept task, cited only by steps dropped as non-code work) and its unowned
// ones (R8: owned by no task after the repair round, and no step's files
// cover them), and records them on every subtask: in the message's
// non_code_criteria, which review's verifier reads from plan_tasks as
// recorded gaps, and as a section of the description that tells the
// implementer they are not its work. It also records the criteria re-homed
// by file overlap (R8) on every subtask (attachRehomedCriteria).
func attachNonCodeCriteria(b *planBuild) {
	if b == nil {
		return
	}
	attachAttachedChecks(b)
	if b.criteria == nil {
		return
	}
	attachRehomedCriteria(b)
	marks := plan.NonCodeCriteria(*b.criteria, b.specs)
	unowned := map[string]bool{}
	for _, id := range b.unowned {
		if _, ok := marks[id]; !ok {
			unowned[id] = true
			marks[id] = "owned by no task after planning"
		}
	}
	if len(marks) == 0 {
		return
	}
	marked := *b.criteria
	marked.Criteria = plan.MarkNonCode(b.criteria.Criteria, marks)
	for i := range marked.Criteria {
		if unowned[marked.Criteria[i].ID] {
			marked.Criteria[i].Unowned = true
		}
	}
	b.criteria = &marked
	var nonCode []plan.Criterion
	var lines []string
	for _, c := range marked.Criteria {
		if c.NonCode {
			nonCode = append(nonCode, c)
			lines = append(lines, "- "+plan.NonCodeGap(c))
		}
	}
	heading := "Non-code acceptance criteria"
	if len(unowned) > 0 {
		heading = "Unowned and non-code acceptance criteria"
	}
	section := "\n## " + heading + " (not this task's work; review verifies them manually)\n\n" + strings.Join(lines, "\n") + "\n"
	for i := range b.planned {
		b.planned[i].criteria = b.criteria
		b.planned[i].msg.NonCodeCriteria = append([]plan.Criterion(nil), nonCode...)
		b.planned[i].msg.Description += section
		if b.planned[i].item != nil {
			b.planned[i].item.Description += section
		}
	}
}

// attachRehomedCriteria records the criteria re-homed by file overlap (R8)
// in every subtask's rehomed_criteria, and adds a note to the subtask whose
// step received one. The criterion itself is already in that subtask's
// task_spec and criteria (the plan was rebuilt after re-homing).
func attachRehomedCriteria(b *planBuild) {
	if len(b.rehomed) == 0 {
		return
	}
	for i := range b.planned {
		b.planned[i].msg.RehomedCriteria = append([]rehomedCriterion(nil), b.rehomed...)
		var lines []string
		for _, r := range b.rehomed {
			for _, st := range b.planned[i].stepTasks {
				if st.index != r.TaskIndex {
					continue
				}
				text := r.CriterionID
				if c, ok := b.criterionByID(r.CriterionID); ok {
					text = fmt.Sprintf("%s (%q)", c.ID, c.Text)
				}
				lines = append(lines, fmt.Sprintf("- %s was owned by no step after planning; the planner added it to step %q because that step changes %s. Satisfy it in that step; review verifies it as usual.",
					text, st.Title, strings.Join(r.Files, ", ")))
			}
		}
		if len(lines) == 0 {
			continue
		}
		section := "\n## Re-homed acceptance criteria\n\n" + strings.Join(lines, "\n") + "\n"
		b.planned[i].msg.Description += section
		if b.planned[i].item != nil {
			b.planned[i].item.Description += section
		}
	}
}

// attachPlanWarnings adds the warnings about a subtask's steps to its
// description (work item and message): a hypothesis-only step is planned,
// and said to be one.
func attachPlanWarnings(planned []plannedSubtask, vs []plan.Violation) {
	for i := range planned {
		var lines []string
		for _, v := range plan.Warnings(vs) {
			for _, st := range planned[i].stepTasks {
				if v.TaskIndex == st.index {
					lines = append(lines, "- "+v.Message)
					break
				}
			}
		}
		if len(lines) == 0 {
			continue
		}
		section := "\n## Plan validation warnings\n\n" + strings.Join(lines, "\n") + "\n"
		planned[i].msg.Description += section
		if planned[i].item != nil {
			planned[i].item.Description += section
		}
	}
}

// Metrics: ae_plan_validation_violations_total{code} (every violation of
// every validation, warnings included) and ae_plan_validation_blocks_total.
var (
	violationCountersMu  sync.Mutex
	violationCounters    = map[plan.ViolationCode]*metrics.Counter{}
	planValidationBlocks = metrics.NewCounter("ae_plan_validation_blocks_total",
		"Parents blocked because their plan failed validation after the repair round")
)

func init() {
	for _, c := range plan.ViolationCodes {
		violationCounter(c)
	}
}

func violationCounter(c plan.ViolationCode) *metrics.Counter {
	violationCountersMu.Lock()
	defer violationCountersMu.Unlock()
	if m, ok := violationCounters[c]; ok {
		return m
	}
	m := metrics.NewLabeledCounter("ae_plan_validation_violations_total",
		"Plan validation violations found, by rule", "code", string(c))
	violationCounters[c] = m
	return m
}

func countViolations(vs []plan.Violation) {
	for _, v := range vs {
		violationCounter(v.Code).Inc()
	}
}
