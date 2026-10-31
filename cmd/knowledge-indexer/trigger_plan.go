package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/autonomousengine/autonomous-engine/internal/products"
	"github.com/google/uuid"
)

// Incremental index planning for a Kafka trigger (codex.updated /
// external-change.detected) that is not a full index
// (onboarding_initial_scan) and not self-triggered.
//
//   - post_merge: the repository's clone when repository_id resolves (else
//     the legacy REPO_PATH), refreshed first (git pull) — unchanged.
//   - repository_id (no trigger, e.g. a registered webhook push): that
//     repository's clone, refreshed first the same way post_merge is; a
//     failed refresh is logged and what is there is indexed.
//   - product_id only (no trigger, no repository_id, e.g. change-reconciler):
//     every enabled repository of the product that has a clone, never the
//     legacy REPO_PATH; skipped with a log when the product has none.
//   - any other trigger without ids keeps the legacy REPO_PATH (messages
//     with no trigger and no id are notifications, skipped earlier).

// triggerRepoSource is the products surface the planner needs
// (*products.ProductStore).
type triggerRepoSource interface {
	GetRepository(ctx context.Context, id uuid.UUID) (*products.Repository, error)
	GetRepositoriesForProduct(ctx context.Context, productID uuid.UUID) ([]*products.Repository, error)
}

// indexStep is one incremental index run.
type indexStep struct {
	path string
	// refresh: pull the clone before indexing it.
	refresh bool
	// productID / repoID are the clone's repository and its product when
	// the trigger resolved one (uuid.Nil for the legacy REPO_PATH). With a
	// product the run writes product-scoped codex rows and a codex version
	// (migration 060); without one it writes unscoped rows and no version.
	productID uuid.UUID
	repoID    uuid.UUID
}

// triggerPlan is what a trigger indexes; skip, when set, says why nothing
// is indexed.
type triggerPlan struct {
	steps []indexStep
	skip  string
}

// hasClone reports whether path is a git clone.
func hasClone(path string) bool {
	if path == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(path, ".git"))
	return err == nil
}

// planIncrementalTrigger resolves the clones t indexes. repos is nil when the
// indexer has no product store; cloneExists is hasClone in production.
func planIncrementalTrigger(ctx context.Context, t indexTrigger, repos triggerRepoSource, legacyPath string, cloneExists func(string) bool) triggerPlan {
	refresh := t.Trigger == "post_merge"
	if t.RepositoryID != "" {
		step := indexStep{path: legacyPath}
		if repos != nil {
			if id, err := uuid.Parse(t.RepositoryID); err == nil {
				if repo, err := repos.GetRepository(ctx, id); err == nil && repo != nil && repo.ClonePath != "" {
					step = indexStep{path: repo.ClonePath, productID: repo.ProductID, repoID: repo.ID}
					// A trigger-less message naming a repository is a
					// change notice (a registered push): refresh first.
					refresh = refresh || t.Trigger == ""
				}
			}
		}
		step.refresh = refresh
		return triggerPlan{steps: []indexStep{step}}
	}
	if t.ProductID != "" && t.Trigger == "" {
		if repos == nil {
			return triggerPlan{skip: "product " + t.ProductID + ": no product store to resolve its repositories"}
		}
		pid, err := uuid.Parse(t.ProductID)
		if err != nil {
			return triggerPlan{skip: fmt.Sprintf("product_id %q is not a uuid", t.ProductID)}
		}
		list, err := repos.GetRepositoriesForProduct(ctx, pid)
		if err != nil {
			return triggerPlan{skip: fmt.Sprintf("product %s: list repositories: %v", pid, err)}
		}
		var plan triggerPlan
		for _, r := range list {
			if r == nil || !r.Enabled || !cloneExists(r.ClonePath) {
				continue
			}
			// Intentionally no refresh: the product-only message is the
			// change-reconciler's, fired after its quiet period (about 15
			// minutes) following the push, whose own repository_id trigger
			// already pulled the clone.
			plan.steps = append(plan.steps, indexStep{path: r.ClonePath, productID: pid, repoID: r.ID})
		}
		if len(plan.steps) == 0 {
			plan.skip = fmt.Sprintf("product %s has no enabled repository with a clone", pid)
		}
		return plan
	}
	return triggerPlan{steps: []indexStep{{path: legacyPath, refresh: refresh}}}
}

// runTriggerPlan runs every step: refresh (when asked; a failure is logged
// and the clone indexed as it is), then index. All steps run; the errors
// are joined.
func runTriggerPlan(plan triggerPlan, refresh func(path string) error, index func(st indexStep) error) error {
	var errs []error
	for _, st := range plan.steps {
		if st.refresh {
			log.Printf("knowledge-indexer: refreshing clone path=%s before indexing", st.path)
			if err := refresh(st.path); err != nil {
				log.Printf("knowledge-indexer: refresh %s: %v — indexing the clone as it is", st.path, err)
			}
		}
		if err := index(st); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", st.path, err))
		}
	}
	return errors.Join(errs...)
}
