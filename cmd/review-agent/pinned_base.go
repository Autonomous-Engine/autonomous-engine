package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/gitref"
	"github.com/autonomousengine/autonomous-engine/internal/snapshot"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// M2-03: the review diffs the PR head against the work item's pinned base
// commit, not against origin/<base_branch> as of the last fetch (which moves
// whenever anyone pushes to the base branch, so the reviewer would see other
// people's commits as part of this change, or miss part of it). A
// legacy-unpinned item keeps origin/<base_branch>.

// baseDrift is pr.ready.base_drift (set by pr-coordinator when the base
// branch moved after the pin but the PR still merges cleanly onto it).
type baseDrift struct {
	Old   string `json:"old"`
	New   string `json:"new"`
	Clean bool   `json:"clean"`
}

// reviewBase is what the review diffs against.
type reviewBase struct {
	// Ref is the diff base handed to git: the pinned SHA, or
	// origin/<base_branch> for a legacy-unpinned item.
	Ref string
	// SHA is the pinned base ("" when legacy-unpinned).
	SHA string
}

// pinReader reads a work item's pin (*snapshot.Store satisfies it).
type pinReader interface {
	Get(ctx context.Context, workID, repositoryID uuid.UUID) (snapshot.Pin, bool, error)
}

// reviewBaseDeps are the collaborators of resolveReviewBase; tests inject
// fakes.
type reviewBaseDeps struct {
	pins pinReader
	// present reports whether the clone has the commit.
	present func(ctx context.Context, sha string) bool
	// fetch fetches origin <ref> into the clone.
	fetch func(ctx context.Context, ref string) error
}

// resolveReviewBase picks the diff base: the message's base_sha, else the
// work item's pin for the repository, else (legacy-unpinned)
// origin/<baseBranch>. A pinned base the clone lacks is fetched with origin
// <baseBranch>; if it is still missing the review cannot honestly say what
// changed and err is returned (the caller blocks).
func resolveReviewBase(ctx context.Context, deps reviewBaseDeps, workID uuid.UUID, repositoryID, payloadBaseSHA, baseBranch string) (reviewBase, error) {
	legacy := reviewBase{Ref: "origin/" + baseBranch}
	sha := payloadBaseSHA
	if sha != "" && !snapshot.ValidSHA(sha) {
		log.Printf("review-agent: work_id=%s: ignoring malformed base_sha %q on pr.ready", workID, sha)
		sha = ""
	}
	if sha == "" && deps.pins != nil {
		if repoID, err := uuid.Parse(repositoryID); err == nil {
			p, found, err := deps.pins.Get(ctx, workID, repoID)
			if err != nil {
				log.Printf("review-agent: work_id=%s repo=%s: read pin: %v — diffing against %s", workID, repositoryID, err, legacy.Ref)
				return legacy, nil
			}
			if found {
				sha = p.BaseSHA
			}
		}
	}
	if sha == "" {
		snapshot.NoteUnpinned(agentID, workID, "review diff base "+legacy.Ref)
		return legacy, nil
	}
	if deps.present != nil && !deps.present(ctx, sha) {
		var ferr error
		if deps.fetch != nil {
			ferr = deps.fetch(ctx, baseBranch)
		}
		if !deps.present(ctx, sha) {
			return reviewBase{}, fmt.Errorf("pinned base %s of repository %s is not in the local clone (fetch origin %s: %v); cannot diff the PR against the commit the work item was built on", sha, repositoryID, baseBranch, ferr)
		}
	}
	return reviewBase{Ref: sha, SHA: sha}, nil
}

// baseBranchLine is the prompt's base line. A pinned review states the
// pinned commit; "(pinned)" tells the model the diff is anchored there.
func baseBranchLine(baseBranch string, rb reviewBase, drift *baseDrift) string {
	line := "Base branch: " + baseBranch
	if rb.SHA != "" {
		line += fmt.Sprintf(" — base %s (pinned)", snapshot.Short(rb.SHA))
	}
	if drift != nil && drift.Old != "" && drift.New != "" {
		state := "merges cleanly"
		if !drift.Clean {
			state = "conflicts"
		}
		line += fmt.Sprintf("\nThe base branch moved from %s to %s after the pin; the change %s onto the new tip. Review it against the pinned base.",
			snapshot.Short(drift.Old), snapshot.Short(drift.New), state)
	}
	return line
}

// newReviewBaseDeps wires the production collaborators over the clone.
func newReviewBaseDeps(pool *pgxpool.Pool, clonePath string) reviewBaseDeps {
	return reviewBaseDeps{
		pins: snapshot.NewStore(pool),
		present: func(ctx context.Context, sha string) bool {
			return gitref.CommitPresent(ctx, clonePath, sha)
		},
		fetch: func(ctx context.Context, ref string) error {
			if ref == "" || strings.HasPrefix(ref, "-") {
				return fmt.Errorf("no usable base ref %q", ref)
			}
			fctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			defer cancel()
			cmd := exec.CommandContext(fctx, "git", "fetch", "--quiet", "origin", ref)
			cmd.Dir = clonePath
			cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
			if out, err := cmd.CombinedOutput(); err != nil {
				return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
			}
			return nil
		},
	}
}

// The unpinned series reads 0 from startup rather than being absent.
func init() { snapshot.RegisterUnpinned(agentID) }
