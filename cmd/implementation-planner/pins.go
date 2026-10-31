package main

import (
	"context"
	"fmt"

	"github.com/autonomousengine/autonomous-engine/internal/codex"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/snapshot"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Repository snapshot pins (M2-02): the parent's pins were resolved at intake
// by product-agent. Each repo-scoped subtask gets its repository's pin on its
// implementation.tasks message (and so on its stored plan_tasks.message) and
// as its own work_item_base_shas row, copied in the plan's transaction.

// snapshots copies pins inside the plan transaction; the *Tx methods never use
// a pool.
var snapshots = snapshot.NewStore(nil)

// loadParentPins returns the parent's pins by repository. A read error is
// returned, not treated as "no pins": planning a pinned item as unpinned would
// silently drop its snapshot, so the caller fails the delivery and it is
// retried.
func loadParentPins(ctx context.Context, db snapshot.Querier, parentID uuid.UUID) (map[uuid.UUID]snapshot.Pin, error) {
	pins, err := snapshot.NewStore(db).GetAll(ctx, parentID)
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]snapshot.Pin, len(pins))
	for _, p := range pins {
		out[p.RepositoryID] = p
	}
	return out, nil
}

// copyPinTx copies the parent's pin to the subtask in the plan transaction
// when its message carries one. The message was built from the same pin, so
// a missing or different parent row now means the pin changed under the
// plan: the plan is rolled back rather than dispatched with a SHA its row
// does not match.
func copyPinTx(ctx context.Context, tx pgx.Tx, parentWorkID uuid.UUID, ps plannedSubtask) error {
	if ps.msg.BaseSHA == "" || ps.item.RepositoryID == nil {
		return nil
	}
	pin, copied, err := snapshots.CopyToChildTx(ctx, tx, parentWorkID, ps.item.ID, *ps.item.RepositoryID)
	if err != nil {
		return fmt.Errorf("subtask for %s: %w", ps.repoName, err)
	}
	if !copied {
		return fmt.Errorf("subtask for %s: parent %s has no base pin for repository %s to copy", ps.repoName, parentWorkID, *ps.item.RepositoryID)
	}
	if pin.BaseSHA != ps.msg.BaseSHA {
		return fmt.Errorf("subtask for %s: parent pin moved from %s to %s while planning", ps.repoName, ps.msg.BaseSHA, pin.BaseSHA)
	}
	return nil
}

// Codex pins (migration 060): research-coordinator pinned the parent to the
// codex its brief rests on. The planner carries the pin on every subtask's
// implementation.tasks message (applyCodexPin), gives each subtask its own
// copy in the plan's transaction (copyCodexPinTx) and sets the subtasks'
// codex_version from it (plannedCodexVersion). A parent without a pin plans
// as before, with codex_version 0 meaning "unknown".

// codexPins copies codex pins inside the plan transaction; the *Tx methods
// never use a pool.
var codexPins = codex.NewPinStore(nil)

// loadParentCodexPin returns the parent's codex pin, nil when it has none. A
// read error is returned, not treated as "no pin" (see loadParentPins).
func loadParentCodexPin(ctx context.Context, db codex.Querier, parentID uuid.UUID) (*codex.Pin, error) {
	pin, found, err := codex.NewPinStore(db).Get(ctx, parentID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}
	return &pin, nil
}

// plannedCodexVersion is the codex_version a subtask records: the pinned
// product_version, else the parent's codex_version (0 when unknown).
func plannedCodexVersion(pin *codex.Pin, parent *models.WorkItem) int {
	if pin != nil {
		return pin.ProductVersion
	}
	if parent != nil {
		return parent.CodexVersion
	}
	return 0
}

// applyCodexPin puts the pin on the message: codex_version is its
// product_version, codex_branch and codex_modules name the codex it is.
func applyCodexPin(msg *implementationTaskMessage, pin *codex.Pin) {
	if pin == nil {
		return
	}
	msg.CodexVersion = pin.ProductVersion
	msg.CodexBranch = pin.Branch
	msg.CodexModules = pin.Modules
}

// currentVersions is the part of *codex.Codex resolvePlanningCodexPin reads.
type currentVersions interface {
	GetCurrentVersion(ctx context.Context, productID uuid.UUID, branch string) (int, error)
}

// resolvePlanningCodexPin is the codex pin the plan is built on: the
// parent's pin (the brief's product, branch and modules) at the branch's
// current product_version, read before the codex context is. A re-plan
// (codex coherence Part 1) runs with the existing brief, so the version
// must be the one planning sees, not the one research saw. nil when the
// parent has no pin; a read error is returned (the delivery is retried).
func resolvePlanningCodexPin(ctx context.Context, cx currentVersions, db codex.Querier, parentID uuid.UUID) (*codex.Pin, error) {
	pin, err := loadParentCodexPin(ctx, db, parentID)
	if err != nil || pin == nil {
		return pin, err
	}
	current, err := cx.GetCurrentVersion(ctx, pin.ProductID, pin.Branch)
	if err != nil {
		return nil, fmt.Errorf("current codex version of %s/%s: %w", pin.ProductID, pin.Branch, err)
	}
	pin.ProductVersion = current
	return pin, nil
}

// repinParentTx moves the parent's pin, in the plan's transaction and
// before any message is encoded, to the version the plan was built on (the
// messages' codex_version), and puts the pin row's state on every subtask's
// message and row. The row is authoritative: when a second brief replaced
// it while planning, its modules (and, on another branch, its version) are
// what the subtasks carry; nothing is rolled back. If the row is gone the
// messages lose their pin fields and the subtasks stay unpinned.
//
// After a plan commits, the parent's pin equals the codex version planned
// against, and every subtask carries a copy (copyCodexPinTx).
func repinParentTx(ctx context.Context, tx pgx.Tx, parentWorkID uuid.UUID, planned []plannedSubtask) error {
	var branch string
	var version int
	for _, ps := range planned {
		if ps.msg.CodexBranch != "" {
			branch, version = ps.msg.CodexBranch, ps.msg.CodexVersion
			break
		}
	}
	if branch == "" {
		return nil
	}
	pin, found, err := codexPins.RepinTx(ctx, tx, parentWorkID, branch, version)
	if err != nil {
		return err
	}
	for i := range planned {
		if !found {
			planned[i].msg.CodexBranch, planned[i].msg.CodexModules = "", nil
			continue
		}
		applyCodexPin(&planned[i].msg, &pin)
		planned[i].item.CodexVersion = pin.ProductVersion
	}
	return nil
}

// copyCodexPinTx copies the parent's codex pin (as repinParentTx left it)
// to the subtask in the plan transaction when its message carries one.
func copyCodexPinTx(ctx context.Context, tx pgx.Tx, parentWorkID uuid.UUID, ps plannedSubtask) error {
	if ps.msg.CodexBranch == "" {
		return nil
	}
	if _, copied, err := codexPins.CopyToChildTx(ctx, tx, parentWorkID, ps.item.ID); err != nil {
		return fmt.Errorf("subtask for %s: %w", ps.repoName, err)
	} else if !copied {
		return fmt.Errorf("subtask for %s: parent %s lost its codex pin inside the plan transaction", ps.repoName, parentWorkID)
	}
	return nil
}

// The unpinned series reads 0 from startup rather than being absent.
func init() { snapshot.RegisterUnpinned(agentID) }
