package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/guards"
	"github.com/autonomousengine/autonomous-engine/internal/snapshot"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// M2-03: research reads the repository snapshot its work item was pinned to,
// not the shared clone's working tree (which knowledge-indexer and
// github-service move underneath in-flight work). Each pinned repository is
// read from a shared, detached snapshot worktree
// <clone_path>/.git/ae-snapshots/<sha12> (snapshot.Worktrees); the executor, its
// file tools and its LSP are rooted there. A legacy-unpinned item (no
// base_sha on the repo_list entry and no pin row) keeps reading the clone and
// is logged once.
//
// Codex and Qdrant reads stay branch-keyed in this ticket (known
// limitation): the research result records the base_sha per repository and
// the codex_version the findings were produced from, so the brief can say
// which snapshot and which Codex it rests on.

// defaultRepoPath is where a task without a repo_list reads (legacy).
const defaultRepoPath = "/workspace/repo"

// selectResearchRepos is the repositories a task reads: the task's own
// repository when it names one (and it is in the list), otherwise the whole
// list, otherwise the legacy default path.
func selectResearchRepos(task researchTaskMessage) []repoRef {
	if task.RepositoryID != "" {
		for _, r := range task.RepoList {
			if r.ID == task.RepositoryID {
				return []repoRef{r}
			}
		}
		return nil
	}
	if len(task.RepoList) > 0 {
		return append([]repoRef(nil), task.RepoList...)
	}
	return []repoRef{{ClonePath: defaultRepoPath}}
}

// snapshotEnsurer is the slice of *snapshot.Worktrees research needs.
type snapshotEnsurer interface {
	Ensure(ctx context.Context, clonePath, baseRef, sha string) (snapshot.Snapshot, error)
}

// pinReader reads a work item's pin (*snapshot.Store satisfies it).
type pinReader interface {
	Get(ctx context.Context, workID, repositoryID uuid.UUID) (snapshot.Pin, bool, error)
}

// snapshotDeps are research's snapshot collaborators; tests inject fakes.
type snapshotDeps struct {
	worktrees snapshotEnsurer
	pins      pinReader
	// baseBranch returns repositories.base_branch, used to fetch a pinned
	// commit the clone lacks when the pin row cannot be read.
	baseBranch func(ctx context.Context, repositoryID string) string
}

// pinnedRead is where research reads each selected repository.
type pinnedRead struct {
	// Paths parallels the selected repos: the snapshot worktree for a
	// pinned repository, the clone for a legacy-unpinned one.
	Paths []string
	// BaseSHAs is repository id → the pinned commit actually read; nil
	// when nothing was pinned.
	BaseSHAs map[string]string
}

// pinnedReadPaths resolves the read path of every selected repository. A
// pinned repository whose clone is on disk gets its snapshot worktree; if that
// cannot be created the task fails (err) rather than silently reading the
// moving clone. A repository whose clone is not on disk keeps its clone path
// (the caller's availability check decides about it).
func pinnedReadPaths(ctx context.Context, deps snapshotDeps, workID uuid.UUID, repos []repoRef) (pinnedRead, error) {
	out := pinnedRead{Paths: make([]string, len(repos))}
	unpinned := false
	for i, r := range repos {
		out.Paths[i] = r.ClonePath
		sha, ref := r.BaseSHA, ""
		repoID, idErr := uuid.Parse(r.ID)
		if idErr == nil && deps.pins != nil {
			if p, found, err := deps.pins.Get(ctx, workID, repoID); err != nil {
				log.Printf("research-agent: work_id=%s repo=%s: read pin: %v", workID, r.ID, err)
			} else if found {
				if sha != "" && sha != p.BaseSHA {
					log.Printf("research-agent: work_id=%s repo=%s: repo_list base_sha %s differs from the pin %s — reading the pin", workID, r.ID, snapshot.Short(sha), snapshot.Short(p.BaseSHA))
				}
				sha, ref = p.BaseSHA, p.BaseRef
			}
		}
		if sha == "" {
			unpinned = true
			continue
		}
		if r.ClonePath == "" || !guards.CheckRepoAvailable(r.ClonePath).Valid {
			continue
		}
		if ref == "" && deps.baseBranch != nil {
			ref = deps.baseBranch(ctx, r.ID)
		}
		snap, err := deps.worktrees.Ensure(ctx, r.ClonePath, ref, sha)
		if err != nil {
			return out, fmt.Errorf("repository %s (%s): pinned snapshot %s unavailable: %w", r.Name, r.ID, snapshot.Short(sha), err)
		}
		how := "reused"
		if snap.Created {
			how = "created"
		}
		if snap.Fetched {
			how += " after fetching origin " + ref
		}
		log.Printf("research-agent: work_id=%s repo=%s reading pinned snapshot %s (base %s, %s)", workID, r.Name, snap.Path, snapshot.Short(sha), how)
		out.Paths[i] = snap.Path
		if out.BaseSHAs == nil {
			out.BaseSHAs = map[string]string{}
		}
		out.BaseSHAs[r.ID] = sha
	}
	if unpinned {
		snapshot.NoteUnpinned(agentID, workID, "research reads the shared clone")
	}
	return out, nil
}

// snapshotPromptLine tells the model which commit it is reading: the primary
// repository's pinned base, or nothing for a legacy-unpinned read.
func snapshotPromptLine(repos []repoRef, paths []string, primaryPath string, baseSHAs map[string]string) string {
	for i, p := range paths {
		if p != primaryPath || i >= len(repos) {
			continue
		}
		if sha := baseSHAs[repos[i].ID]; sha != "" {
			return fmt.Sprintf("\nRepository snapshot: base %s (pinned; read-only checkout of the commit this work item was approved against)", snapshot.Short(sha))
		}
	}
	return ""
}

// newSnapshotDeps wires the production collaborators.
func newSnapshotDeps(pool *pgxpool.Pool) snapshotDeps {
	return snapshotDeps{
		worktrees: researchWorktrees,
		pins:      snapshot.NewStore(pool),
		baseBranch: func(ctx context.Context, repositoryID string) string {
			var b string
			_ = pool.QueryRow(ctx, `SELECT COALESCE(base_branch, '') FROM repositories WHERE id = $1`, repositoryID).Scan(&b)
			return b
		},
	}
}

// researchWorktrees creates and reuses snapshot worktrees. A first fetch and
// checkout of a large repository can take minutes.
var researchWorktrees = &snapshot.Worktrees{Timeout: 5 * time.Minute}

// codexVersions is the part of *codex.Codex codexVersionFor reads.
type codexVersions interface {
	GetCurrentVersion(ctx context.Context, productID uuid.UUID, branch string) (int, error)
}

// codexVersionFor is the product's current Codex version on branch (its
// product_version, migration 060): the Codex research was grounded on. 0
// when unknown.
func codexVersionFor(ctx context.Context, cx codexVersions, productID uuid.UUID, branch string) int {
	if cx == nil {
		return 0
	}
	v, err := cx.GetCurrentVersion(ctx, productID, branch)
	if err != nil {
		log.Printf("research-agent: codex version for product %s branch %s: %v", productID, branch, err)
		return 0
	}
	return v
}

// pruneSnapshotsOnStartup removes snapshot worktrees no non-terminal pin
// references and nobody used for 7 days (snapshot.PruneRepositories). It
// runs once, in the background, bounded.
func pruneSnapshotsOnStartup(ctx context.Context, pool *pgxpool.Pool) {
	pctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	if _, err := snapshot.PruneRepositories(pctx, pool, researchWorktrees, agentID); err != nil {
		log.Printf("research-agent: snapshot prune: %v", err)
	}
}

// The unpinned series reads 0 from startup rather than being absent.
func init() { snapshot.RegisterUnpinned(agentID) }
