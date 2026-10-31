package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/configstore"
	"github.com/autonomousengine/autonomous-engine/internal/gitremote"
	"github.com/autonomousengine/autonomous-engine/internal/guards"
	"github.com/autonomousengine/autonomous-engine/internal/interactions"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/products"
	"github.com/autonomousengine/autonomous-engine/internal/snapshot"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Repository snapshot pinning at intake (M2-01/M2-02, spec
// 2026-10-03-repository-snapshot-pinning.md). product-agent is the first
// consumer that knows a work item's product and so its repositories: it
// resolves each repository's base branch to a commit BEFORE the PRD
// transaction (network never runs inside a transaction) and commitPRD writes
// the pins and their resolved events with the PRD. A repository that cannot
// be resolved blocks the item, with the escalation prompt, in one
// transaction of its own.

// snapshots writes pins inside the caller's transaction; the *Tx methods never
// use a pool.
var snapshots = snapshot.NewStore(nil)

// repoResolver is the slice of *snapshot.Resolver intake needs.
type repoResolver interface {
	ResolveDetailed(ctx context.Context, repo products.Repository) (snapshot.Resolution, error)
}

// repoLister is the slice of *products.ProductStore intake needs.
type repoLister interface {
	GetRepository(ctx context.Context, id uuid.UUID) (*products.Repository, error)
	GetRepositoriesForProduct(ctx context.Context, productID uuid.UUID) ([]*products.Repository, error)
}

// intakePinner resolves the pins of a work item at intake.
type intakePinner struct {
	repos    repoLister
	resolver repoResolver
}

// resolvedPin is one resolved repository, ready for commitPRD.
type resolvedPin struct {
	pin    snapshot.Pin
	repo   string
	detail string
}

// newResolver is the production resolver: git on PATH, the github-service
// token order (gitremote.ResolveToken) and, after a failure, the GitHub
// credential health check so a missing, expired or rejected token is named
// (the D8 path) rather than reported as a bare git error.
func newResolver(cs *configstore.ConfigStore, envToken string) *snapshot.Resolver {
	var creds gitremote.CredentialResolver
	if cs != nil {
		creds = cs
	}
	r := &snapshot.Resolver{
		Run: snapshot.GitRunner,
		// The source (override | registry | env | none) is named in a
		// failure's block reason; the value never is.
		TokenSource: func(ctx context.Context, repo products.Repository) (string, string) {
			return gitremote.ResolveTokenSource(ctx, creds, repo.TokenOverride, envToken)
		},
	}
	if cs != nil {
		r.CredentialCheck = func(ctx context.Context, repo products.Repository) error {
			if v := guards.CheckGitHubCredential(ctx, cs, repo.TokenOverride); !v.Valid {
				return errors.New(v.Message)
			}
			return nil
		}
	}
	return r
}

// targetRepositories is what intake pins: the work item's own repository
// when it names one, otherwise every enabled repository of the product.
func (p *intakePinner) targetRepositories(ctx context.Context, productID uuid.UUID, wi *models.WorkItem) ([]*products.Repository, error) {
	if wi != nil && wi.RepositoryID != nil && *wi.RepositoryID != uuid.Nil {
		repo, err := p.repos.GetRepository(ctx, *wi.RepositoryID)
		if err != nil {
			return nil, fmt.Errorf("load repository %s of the work item: %w", *wi.RepositoryID, err)
		}
		return []*products.Repository{repo}, nil
	}
	all, err := p.repos.GetRepositoriesForProduct(ctx, productID)
	if err != nil {
		return nil, fmt.Errorf("list repositories of product %s: %w", productID, err)
	}
	var enabled []*products.Repository
	for _, r := range all {
		if r.Enabled {
			enabled = append(enabled, r)
		}
	}
	return enabled, nil
}

// resolve resolves every target repository. Every repository is attempted;
// the error names each one that failed and why.
func (p *intakePinner) resolve(ctx context.Context, workID, productID uuid.UUID, wi *models.WorkItem) ([]resolvedPin, error) {
	repos, err := p.targetRepositories(ctx, productID, wi)
	if err != nil {
		return nil, err
	}
	if len(repos) == 0 {
		log.Printf("product-agent: work_id=%s: product %s has no enabled repository — nothing to pin", workID, productID)
		return nil, nil
	}
	now := time.Now().UTC()
	var pins []resolvedPin
	var failures []string
	for _, repo := range repos {
		res, rerr := p.resolver.ResolveDetailed(ctx, *repo)
		if rerr != nil {
			failures = append(failures, fmt.Sprintf("repository %s (%s): %v", repo.Name, repo.ID, rerr))
			continue
		}
		pins = append(pins, resolvedPin{
			pin: snapshot.Pin{
				WorkItemID: workID, RepositoryID: repo.ID,
				BaseRef: res.Ref, BaseSHA: res.SHA, ResolvedAt: now,
			},
			repo:   repo.Name,
			detail: "intake: " + res.Detail,
		})
		log.Printf("product-agent: work_id=%s pinned %s %s @ %s (%s)", workID, repo.Name, res.Ref, res.SHA, res.Method)
	}
	if len(failures) > 0 {
		return nil, errors.New(strings.Join(failures, "\n"))
	}
	return pins, nil
}

// pinFailureReason is the block reason for a failed resolution.
func pinFailureReason(err error) string {
	return "Could not pin the base commit of the work item's repositories, so it cannot start: every stage must work from one snapshot.\n\n" +
		err.Error() +
		"\n\nFix the repository (base branch, clone, or GitHub credential), then retry the work item."
}

// pinBlockedPrompt is the escalation prompt raised with the block, the way
// status-orchestrator raises one for any blocked item; it commits first, so
// the orchestrator finds it pending and does not raise a second.
func pinBlockedPrompt(workID, productID uuid.UUID, title, reason string) *interactions.Prompt {
	return &interactions.Prompt{
		ID:          uuid.New(),
		WorkItemID:  workID,
		ProductID:   productID,
		AskingAgent: agentID,
		Trigger:     interactions.TriggerBlocked,
		PromptType:  interactions.PromptTypeEscalation,
		Headline:    fmt.Sprintf("Blocked: %s", title),
		Question:    reason,
		ContextLink: fmt.Sprintf("/work-items/%s", workID),
		Actions: []interactions.Action{
			{ID: "retry", Label: "Retry", Semantic: interactions.SemanticRetry},
			{ID: "cancel", Label: "Cancel", Semantic: interactions.SemanticCancel},
		},
		State:       interactions.StatePending,
		RequestedAt: time.Now().UTC(),
		PromptedVia: []string{"dashboard"},
	}
}

// blockWithPromptInOwnTx blocks the work item and raises its escalation
// prompt in one transaction: the transition, work.blocked, and the prompt row
// with user_prompt.requested commit together. A nil prompt (an item without a
// product: user_interactions.product_id is required) blocks only.
func blockWithPromptInOwnTx(ctx context.Context, db workgraph.TxBeginner, aggregateID, workID uuid.UUID, reason string, prompt *interactions.Prompt) error {
	return workgraph.WithTx(ctx, db, func(tx pgx.Tx) error {
		if err := guards.BlockWorkItemTx(ctx, tx, aggregateID, workID, agentID, reason); err != nil {
			return err
		}
		if prompt == nil {
			return nil
		}
		return interactions.NewPublisher(nil).PublishTx(ctx, tx, aggregateID, prompt)
	})
}

// writePinsTx writes the intake pins and their events in the PRD
// transaction. A pin that already exists (the item re-entered intake) is kept;
// a different SHA is recorded as drift (snapshot.PinTx).
func writePinsTx(ctx context.Context, tx pgx.Tx, workID uuid.UUID, pins []resolvedPin) error {
	for _, rp := range pins {
		res, err := snapshots.PinTx(ctx, tx, rp.pin, rp.detail)
		if err != nil {
			return fmt.Errorf("pin %s: %w", rp.repo, err)
		}
		if res == snapshot.PinDrifted {
			log.Printf("product-agent: work_id=%s %s: base moved since the item was first pinned; pin kept, drift recorded", workID, rp.repo)
		}
	}
	return nil
}
