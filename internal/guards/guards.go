package guards

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ValidationResult carries the outcome of one or more context checks.
type ValidationResult struct {
	Valid   bool
	Missing []string
	Message string
}

// CheckOnboardingComplete verifies the system has been onboarded
// (Codex has at least one version record). If productID is non-nil, the check
// is scoped to that product; otherwise the global codex_versions table is queried.
func CheckOnboardingComplete(ctx context.Context, db *pgxpool.Pool, productID *uuid.UUID) ValidationResult {
	var version int
	var err error
	if productID != nil {
		err = db.QueryRow(ctx,
			"SELECT COALESCE(MAX(version_number), 0) FROM codex_versions WHERE product_id = $1",
			*productID,
		).Scan(&version)
	} else {
		err = db.QueryRow(ctx,
			"SELECT COALESCE(MAX(version_number), 0) FROM codex_versions",
		).Scan(&version)
	}
	if err != nil || version == 0 {
		return ValidationResult{
			Valid:   false,
			Missing: []string{"codex"},
			Message: "System has not been onboarded. No Codex exists. Run onboarding first.",
		}
	}
	return ValidationResult{Valid: true}
}

// CheckProductExists verifies that a product with the given ID exists in the database.
func CheckProductExists(ctx context.Context, db *pgxpool.Pool, productID uuid.UUID) ValidationResult {
	var count int
	db.QueryRow(ctx, "SELECT COUNT(*) FROM products WHERE id = $1", productID).Scan(&count) //nolint:errcheck
	if count == 0 {
		return ValidationResult{
			Valid:   false,
			Missing: []string{"product"},
			Message: fmt.Sprintf("Product %s does not exist. Create the product first.", productID),
		}
	}
	return ValidationResult{Valid: true}
}

// CheckProductHasRepos verifies that a product has at least one repository registered.
func CheckProductHasRepos(ctx context.Context, db *pgxpool.Pool, productID uuid.UUID) ValidationResult {
	var count int
	db.QueryRow(ctx, "SELECT COUNT(*) FROM repositories WHERE product_id = $1", productID).Scan(&count) //nolint:errcheck
	if count == 0 {
		return ValidationResult{
			Valid:   false,
			Missing: []string{"repositories"},
			Message: fmt.Sprintf("Product %s has no repositories. Add at least one repository before proceeding.", productID),
		}
	}
	return ValidationResult{Valid: true}
}

// CheckProductReposCloned verifies that all enabled repositories for a product have a
// clone_path that exists on disk (i.e. the repo has been cloned).
func CheckProductReposCloned(ctx context.Context, db *pgxpool.Pool, productID uuid.UUID) ValidationResult {
	rows, err := db.Query(ctx,
		"SELECT clone_path FROM repositories WHERE product_id = $1 AND enabled = true",
		productID,
	)
	if err != nil {
		return ValidationResult{
			Valid:   false,
			Missing: []string{"repositories"},
			Message: fmt.Sprintf("Could not query repositories for product %s.", productID),
		}
	}
	defer rows.Close()

	var missing []string
	for rows.Next() {
		var clonePath string
		if err := rows.Scan(&clonePath); err != nil {
			continue
		}
		if clonePath == "" {
			missing = append(missing, clonePath)
			continue
		}
		if _, err := os.Stat(clonePath); os.IsNotExist(err) {
			missing = append(missing, clonePath)
		}
	}
	if len(missing) > 0 {
		return ValidationResult{
			Valid:   false,
			Missing: []string{"clone_path"},
			Message: fmt.Sprintf("One or more repositories for product %s have not been cloned: %s", productID, strings.Join(missing, ", ")),
		}
	}
	return ValidationResult{Valid: true}
}

// CheckProductOnboarded verifies that at least one codex version entry exists for the product,
// indicating the onboarding knowledge-indexing step has completed.
func CheckProductOnboarded(ctx context.Context, db *pgxpool.Pool, productID uuid.UUID) ValidationResult {
	var count int
	db.QueryRow(ctx, "SELECT COUNT(*) FROM codex_versions WHERE product_id = $1", productID).Scan(&count) //nolint:errcheck
	if count == 0 {
		return ValidationResult{
			Valid:   false,
			Missing: []string{"codex_versions"},
			Message: fmt.Sprintf("Product %s has not been onboarded. Run the knowledge indexer for this product first.", productID),
		}
	}
	return ValidationResult{Valid: true}
}

// CheckRepoAvailable verifies that a git repository has been cloned to repoPath.
func CheckRepoAvailable(repoPath string) ValidationResult {
	gitDir := repoPath + "/.git"
	if _, err := os.Stat(gitDir); os.IsNotExist(err) {
		return ValidationResult{
			Valid:   false,
			Missing: []string{"repository"},
			Message: fmt.Sprintf("No git repository found at %s. Connect a repository and run onboarding first.", repoPath),
		}
	}
	return ValidationResult{Valid: true}
}

// qdrantCollectionInfo is the minimal subset of the Qdrant collection info response
// that we need to check whether vectors have been indexed.
type qdrantCollectionInfo struct {
	Result struct {
		VectorsCount *int `json:"vectors_count"`
		PointsCount  *int `json:"points_count"`
	} `json:"result"`
}

// CheckVectorStorePopulated verifies that the named Qdrant collection exists and
// contains at least one indexed point.
func CheckVectorStorePopulated(ctx context.Context, vectorURL, collection string) ValidationResult {
	url := strings.TrimRight(vectorURL, "/") + "/collections/" + collection
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return ValidationResult{
			Valid:   false,
			Missing: []string{"vector_store"},
			Message: fmt.Sprintf("Vector store is not accessible at %s. Run the knowledge indexer first.", vectorURL),
		}
	}

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		return ValidationResult{
			Valid:   false,
			Missing: []string{"vector_store"},
			Message: fmt.Sprintf("Vector store collection %q not found or unreachable at %s. Run the knowledge indexer first.", collection, vectorURL),
		}
	}
	defer resp.Body.Close()

	var info qdrantCollectionInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return ValidationResult{
			Valid:   false,
			Missing: []string{"vector_store"},
			Message: fmt.Sprintf("Could not parse vector store response for collection %q. Run the knowledge indexer first.", collection),
		}
	}

	// Accept either vectors_count or points_count — different Qdrant versions use different fields.
	count := 0
	if info.Result.VectorsCount != nil {
		count = *info.Result.VectorsCount
	} else if info.Result.PointsCount != nil {
		count = *info.Result.PointsCount
	}

	if count == 0 {
		return ValidationResult{
			Valid:   false,
			Missing: []string{"vector_store"},
			Message: fmt.Sprintf("Vector store collection %q exists but has no indexed content. Run the knowledge indexer first.", collection),
		}
	}
	return ValidationResult{Valid: true}
}

// RunAll combines multiple ValidationResults into one. If any check fails the
// combined result is invalid, with all missing items and messages merged.
func RunAll(checks ...ValidationResult) ValidationResult {
	var missing []string
	var messages []string
	for _, c := range checks {
		if !c.Valid {
			missing = append(missing, c.Missing...)
			messages = append(messages, c.Message)
		}
	}
	if len(missing) > 0 {
		return ValidationResult{
			Valid:   false,
			Missing: missing,
			Message: strings.Join(messages, "; "),
		}
	}
	return ValidationResult{Valid: true}
}

// BlockWorkItem transitions a work item to the blocked state and enqueues
// the work.blocked event in the same transaction, exactly as BlockWorkItemTx
// does. Agents call this when a context check fails so that a human can
// take corrective action. The event is what the status-orchestrator turns
// into the "Blocked:" prompt on the Approvals page; a block that only set
// state='blocked' was invisible there (rerun NEW-7). msgBus stays in the
// signature for the call sites; the outbox is the publish path.
func BlockWorkItem(
	ctx context.Context,
	wg *workgraph.WorkGraph,
	msgBus bus.MessageBus,
	workItemID uuid.UUID,
	agentID string,
	reason string,
) error {
	_ = msgBus
	// Already blocked is a no-op inside BlockWorkItemTx: two sibling subtasks
	// that both fail block the parent once, without a second event.
	err := workgraph.WithTx(ctx, wg.DB(), func(tx pgx.Tx) error {
		return BlockWorkItemTx(ctx, tx, workItemID, workItemID, agentID, reason)
	})
	if err != nil {
		log.Printf("guards: BlockWorkItem: %s: %v", workItemID, err)
	}
	return err
}

// WorkBlockedPayload is the work.blocked message: the shape change-reconciler
// publishes and status-orchestrator consumes.
type WorkBlockedPayload struct {
	WorkID string `json:"work_id"`
	Reason string `json:"reason"`
	// Kind classifies the block for the escalation status-orchestrator
	// raises; empty is a generic block (Retry / Cancel).
	// BlockKindCodexUngrounded adds Re-index.
	Kind string `json:"kind,omitempty"`
}

// BlockWorkItemTx is BlockWorkItem inside the caller's transaction, for a
// producer converted to the outbox: it transitions workID to blocked with
// workgraph.TransitionStateTx and enqueues work.blocked ({work_id, reason},
// key = workID) with outbox.EnqueueJSON, so the block and its event commit
// together or not at all.
//
// aggregateID is the work item the event is ordered under: the parent when
// workID is a subtask, otherwise workID itself.
//
// Like BlockWorkItem it is idempotent: an item that is already blocked is
// left alone and no event is enqueued, and the caller's transaction is still
// usable. Any other error must roll the caller's transaction back.
//
// Note for the status-orchestrator consumer of work.blocked: the item is
// already blocked when the event arrives, so its own transition to blocked
// is a no-op there.
func BlockWorkItemTx(ctx context.Context, tx pgx.Tx, aggregateID, workID uuid.UUID, agentID, reason string) error {
	_, err := blockTx(ctx, tx, aggregateID, workID, agentID, WorkBlockedPayload{WorkID: workID.String(), Reason: reason})
	return err
}
