package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/codex/extractors/framework"
	_ "github.com/autonomousengine/autonomous-engine/internal/codex/extractors/git_strategy"
	"github.com/autonomousengine/autonomous-engine/internal/idempotency"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type commitLinkedOrchestrator struct {
	db   *pgxpool.Pool
	kb   bus.MessageBus
	orch *framework.Orchestrator
}

func newCommitLinkedOrchestrator(db *pgxpool.Pool, kb bus.MessageBus) *commitLinkedOrchestrator {
	blobs := framework.NewBlobStore(db)
	snapshots := framework.NewSnapshotStore(db)
	failures := framework.NewFailureStore(db)
	orch := framework.NewOrchestrator(db, blobs, snapshots, failures, framework.GlobalRegistry())
	return &commitLinkedOrchestrator{db: db, kb: kb, orch: orch}
}

type extractRequest struct {
	RepoID    uuid.UUID `json:"repo_id"`
	CommitSHA string    `json:"commit_sha"`
	Branch    string    `json:"branch"`
	ParentSHA string    `json:"parent_sha"`
}

func (c *commitLinkedOrchestrator) Start(ctx context.Context) error {
	// M1-05: deduplicated on the processed-event ledger. The extraction
	// (framework.Orchestrator.RunForCommit) writes through its own stores, not
	// one transaction this handler owns, so the guard cannot sit inside it:
	// a recorded duplicate is acknowledged before any fetch or extraction,
	// and the event is recorded after a successful run. A crash between the
	// two re-runs the extraction, as before.
	handler := idempotency.NewLedger(c.db).Handler(serviceName,
		func(ctx context.Context, m *bus.Message) error { return c.handle(ctx, m) })
	if err := c.kb.Subscribe(bus.TopicCodexExtractRequested, "knowledge-indexer-commit-linked", handler); err != nil {
		return err
	}
	go c.pollLoop(ctx)
	return nil
}

func (c *commitLinkedOrchestrator) handle(ctx context.Context, m *bus.Message) error {
	var req extractRequest
	if err := json.Unmarshal(m.Value, &req); err != nil {
		log.Printf("commit_linked: bad payload: %v", err)
		return nil
	}
	repoPath, err := c.resolveRepoPath(ctx, req.RepoID)
	if err != nil {
		return err
	}
	if err := fetchAndCheckoutCLO(ctx, repoPath, req.CommitSHA); err != nil {
		return err
	}
	diff, _ := framework.ComputeDiff(ctx, repoPath, req.ParentSHA, req.CommitSHA)
	gs, _ := c.loadGitStrategy(ctx, req.RepoID)
	productID, _ := c.resolveProductID(ctx, req.RepoID)
	watched, _ := c.listWatchedBranches(ctx, req.RepoID)
	ec := &framework.ExtractionContext{
		RepoPath: repoPath, RepoID: req.RepoID, ProductID: productID,
		CommitSHA: req.CommitSHA, Branch: req.Branch, ParentSHA: req.ParentSHA,
		Diff: diff, WatchedSet: watched, GitStrategy: gs,
	}
	_, err = c.orch.RunForCommit(ctx, ec)
	return err
}

func (c *commitLinkedOrchestrator) pollLoop(ctx context.Context) {
	tick := time.NewTicker(15 * time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			c.reconcile(ctx)
		}
	}
}

func (c *commitLinkedOrchestrator) reconcile(ctx context.Context) {
	rows, err := c.db.Query(ctx, `
		SELECT wb.repo_id, wb.branch, r.clone_path, COALESCE(latest.commit_sha, '')
		FROM codex_watched_branches wb
		JOIN repositories r ON r.id = wb.repo_id
		LEFT JOIN LATERAL (
		  SELECT commit_sha FROM codex_snapshots
		  WHERE repo_id = wb.repo_id AND branch = wb.branch
		  ORDER BY extracted_at DESC LIMIT 1
		) latest ON TRUE
		WHERE wb.enabled = TRUE
	`)
	if err != nil {
		log.Printf("commit_linked: reconcile query: %v", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var repoID uuid.UUID
		var branch, clonePath, lastSHA string
		if err := rows.Scan(&repoID, &branch, &clonePath, &lastSHA); err != nil {
			continue
		}
		if err := fetchAndCheckoutCLO(ctx, clonePath, "refs/remotes/origin/"+branch); err != nil {
			continue
		}
		currentSHA := headSHACLO(ctx, clonePath)
		if currentSHA == "" || currentSHA == lastSHA {
			continue
		}
		// A failed enqueue is retried by the next sweep: no snapshot was
		// written for currentSHA, so it is still "new" then.
		if err := enqueueExtractRequest(ctx, c.db, extractRequest{
			RepoID: repoID, CommitSHA: currentSHA, Branch: branch, ParentSHA: lastSHA,
		}); err != nil {
			log.Printf("commit_linked: enqueue extract request repo=%s sha=%s: %v", repoID, currentSHA, err)
		}
	}
}

func (c *commitLinkedOrchestrator) resolveRepoPath(ctx context.Context, repoID uuid.UUID) (string, error) {
	var p string
	err := c.db.QueryRow(ctx, `SELECT clone_path FROM repositories WHERE id = $1`, repoID).Scan(&p)
	return p, err
}

func (c *commitLinkedOrchestrator) resolveProductID(ctx context.Context, repoID uuid.UUID) (uuid.UUID, error) {
	var p uuid.UUID
	err := c.db.QueryRow(ctx, `SELECT product_id FROM repositories WHERE id = $1`, repoID).Scan(&p)
	return p, err
}

func (c *commitLinkedOrchestrator) loadGitStrategy(ctx context.Context, repoID uuid.UUID) (*framework.GitStrategy, error) {
	gs := &framework.GitStrategy{RepoID: repoID}
	var long []string
	err := c.db.QueryRow(ctx, `
		SELECT pattern, main_branch, long_lived_branches, merge_strategy, pr_required
		FROM codex_git_strategy WHERE repo_id = $1
	`, repoID).Scan(&gs.Pattern, &gs.MainBranch, &long, &gs.MergeStrategy, &gs.PRRequired)
	if err != nil {
		return nil, nil
	}
	gs.LongLivedBranches = long
	return gs, nil
}

func (c *commitLinkedOrchestrator) listWatchedBranches(ctx context.Context, repoID uuid.UUID) ([]string, error) {
	rows, err := c.db.Query(ctx, `SELECT branch FROM codex_watched_branches WHERE repo_id = $1 AND enabled = TRUE`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var b string
		_ = rows.Scan(&b)
		out = append(out, b)
	}
	return out, nil
}

func fetchAndCheckoutCLO(ctx context.Context, repoPath, ref string) error {
	if err := exec.CommandContext(ctx, "git", "-C", repoPath, "fetch", "origin").Run(); err != nil {
		return fmt.Errorf("git fetch: %w", err)
	}
	if err := exec.CommandContext(ctx, "git", "-C", repoPath, "checkout", ref).Run(); err != nil {
		return fmt.Errorf("git checkout %s: %w", ref, err)
	}
	return nil
}

func headSHACLO(ctx context.Context, repoPath string) string {
	out, err := exec.CommandContext(ctx, "git", "-C", repoPath, "rev-parse", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
