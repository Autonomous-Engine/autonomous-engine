package main

import (
	"context"
	"log"

	"github.com/autonomousengine/autonomous-engine/internal/codex"
	"github.com/autonomousengine/autonomous-engine/internal/snapshot"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Repository snapshot pins (M2-02). product-agent pinned each repository's
// base commit at intake; research-coordinator forwards them on the research
// task's repo_list and records them in the technical brief. It does not yet
// change how research reads the repository (M2-03): Codex and Qdrant stay
// branch-keyed until then.

// applyRepoPins sets BaseSHA on every repoRef that has a pin. With no pin at
// all the item is legacy-unpinned (logged once, counted). A read error is
// logged and leaves the list unpinned: the pin is informational here.
func applyRepoPins(ctx context.Context, db snapshot.Querier, workID uuid.UUID, repos []repoRef) {
	if len(repos) == 0 {
		return
	}
	pins, err := snapshot.NewStore(db).GetAll(ctx, workID)
	if err != nil {
		log.Printf("research-coordinator: work_id=%s: read base pins: %v — research dispatched unpinned", workID, err)
		return
	}
	if len(pins) == 0 {
		snapshot.NoteUnpinned(agentID, workID, "research dispatch")
		return
	}
	byRepo := make(map[string]string, len(pins))
	for _, p := range pins {
		byRepo[p.RepositoryID.String()] = p.BaseSHA
	}
	for i := range repos {
		repos[i].BaseSHA = byRepo[repos[i].ID]
	}
}

// briefBaseSHAs is the technical brief's base_shas: repository id → pinned
// SHA, nil (omitted) for a legacy-unpinned item.
func briefBaseSHAs(ctx context.Context, db snapshot.Querier, workID uuid.UUID) map[string]string {
	pins, err := snapshot.NewStore(db).GetAll(ctx, workID)
	if err != nil {
		log.Printf("research-coordinator: work_id=%s: read base pins for the brief: %v", workID, err)
		return nil
	}
	if len(pins) == 0 {
		snapshot.NoteUnpinned(agentID, workID, "technical brief")
		return nil
	}
	out := make(map[string]string, len(pins))
	for _, p := range pins {
		out[p.RepositoryID.String()] = p.BaseSHA
	}
	return out
}

// briefCodexPin is the codex pin of the brief (migration 060): the codex
// the brief, and so the plan built from it, rests on.
//
//   - Branch: the codex_branch every successful result reports; when they
//     disagree or none reports one (a pre-060 research-agent), the item's
//     codex branch (itemBranch: the work item's branch, else the product's
//     default branch).
//   - ProductVersion: the highest codex_version a successful result on that
//     branch reported. A codex_version without a codex_branch is a pre-060
//     global version_number and is ignored; with none left, the branch's
//     current product_version (current).
//   - Modules: the brief's affected modules.
//
// ok is false (no pin, logged) without a product, a branch or a version.
func briefCodexPin(workID, productID uuid.UUID, results []researchResultMessage, modules []string, itemBranch func() (string, error), current func(branch string) (int, error)) (codex.Pin, bool) {
	if productID == uuid.Nil {
		return codex.Pin{}, false
	}
	branch := ""
	agree := true
	for _, r := range results {
		if r.Status != ResearchStatusOK || r.CodexBranch == "" {
			continue
		}
		if branch == "" {
			branch = r.CodexBranch
		} else if branch != r.CodexBranch {
			agree = false
		}
	}
	if branch == "" || !agree {
		b, err := itemBranch()
		if err != nil || b == "" {
			log.Printf("research-coordinator: work_id=%s: no codex branch for the brief's codex pin: %v — not pinned", workID, err)
			return codex.Pin{}, false
		}
		branch = b
	}
	version, reported := 0, false
	for _, r := range results {
		if r.Status == ResearchStatusOK && r.CodexBranch == branch {
			reported = true
			if r.CodexVersion > version {
				version = r.CodexVersion
			}
		}
	}
	if !reported {
		v, err := current(branch)
		if err != nil {
			log.Printf("research-coordinator: work_id=%s: codex version of %s/%s for the brief's codex pin: %v — not pinned", workID, productID, branch, err)
			return codex.Pin{}, false
		}
		version = v
	}
	return codex.Pin{WorkItemID: workID, ProductID: productID, Branch: branch, ProductVersion: version, Modules: dedupeSorted(modules)}, true
}

// codexPins writes the brief's codex pin inside the brief's transaction; the
// *Tx methods never use a pool.
var codexPins = codex.NewPinStore(nil)

// codexBranchFor is the work item's codex branch, resolved as research
// resolves it: the item's branch, else the product's default branch.
func codexBranchFor(ctx context.Context, pool *pgxpool.Pool, wg *workgraph.WorkGraph, workID, productID uuid.UUID) func() (string, error) {
	return func() (string, error) {
		if wi, err := wg.GetWorkItem(ctx, workID); err == nil && wi != nil && wi.Branch != nil && *wi.Branch != "" {
			return *wi.Branch, nil
		}
		return codex.ResolveDefaultBranch(ctx, pool, productID)
	}
}

// checkResultBases logs every successful research result that read a
// different commit than the work item's pin for a repository (M2-03). It
// should never happen — research reads the snapshot at the pin — so a line
// here means a research-agent build or a repin raced the research.
func checkResultBases(workID uuid.UUID, pins map[string]string, results []researchResultMessage) int {
	mismatches := 0
	for _, r := range results {
		if r.Status != ResearchStatusOK {
			continue
		}
		for repo, sha := range r.BaseSHAs {
			if pin, ok := pins[repo]; ok && pin != sha {
				mismatches++
				log.Printf("research-coordinator: work_id=%s task=%s read repository %s at %s but the pin is %s", workID, r.TaskID, repo, sha, pin)
			}
		}
	}
	return mismatches
}

// The unpinned series reads 0 from startup rather than being absent.
func init() { snapshot.RegisterUnpinned(agentID) }
