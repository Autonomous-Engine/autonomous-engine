package codex

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Codex pins (migration 060, table work_item_codex_pins): the codex a work
// item was planned against.
//
//   - research-coordinator pins the parent when it records the technical
//     brief (PinTx, in the brief's transaction): the product, the branch the
//     research read the codex on, the product_version it was grounded on
//     and the brief's affected modules. A later brief (research re-run)
//     replaces the pin.
//   - implementation-planner copies the parent's pin to each subtask in the
//     plan's transaction (CopyToChildTx) and carries it on the
//     implementation.tasks message.
//   - Both set work_items.codex_version to the pinned product_version, for
//     display (it was 0 for every item before 060).
//
// An item without a pin predates 060 or was planned from a brief that
// carried none; readers treat it as "codex unknown", never as stale.

// Pin is one work item's codex pin.
type Pin struct {
	WorkItemID     uuid.UUID `json:"work_item_id"`
	ProductID      uuid.UUID `json:"product_id"`
	Branch         string    `json:"branch"`
	ProductVersion int       `json:"product_version"`
	// Modules are the codex module names the plan relies on (sorted,
	// distinct; bare names, not module: entries).
	Modules   []string  `json:"modules"`
	CreatedAt time.Time `json:"created_at"`
}

// PinnedItem is a pinned work item with its current state.
type PinnedItem struct {
	Pin
	State    models.WorkItemState `json:"state"`
	ParentID *uuid.UUID           `json:"parent_id,omitempty"`
}

// Querier is the pooled read surface (*pgxpool.Pool and pgx.Tx satisfy it).
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// PinStore reads and writes codex pins. The *Tx methods use only the
// transaction they are given, so NewPinStore(nil) is enough for them.
type PinStore struct {
	db Querier
}

// NewPinStore returns a PinStore reading through db.
func NewPinStore(db Querier) *PinStore { return &PinStore{db: db} }

func (p Pin) validate() error {
	switch {
	case p.WorkItemID == uuid.Nil:
		return errors.New("codex pin: no work item")
	case p.ProductID == uuid.Nil:
		return fmt.Errorf("codex pin for %s: %w", p.WorkItemID, ErrNoProduct)
	case p.Branch == "":
		return fmt.Errorf("codex pin for %s: no branch", p.WorkItemID)
	case p.ProductVersion < 0:
		return fmt.Errorf("codex pin for %s: negative product_version %d", p.WorkItemID, p.ProductVersion)
	}
	return nil
}

// distinctSorted is modules sorted and deduplicated, empty names dropped,
// never nil.
func distinctSorted(modules []string) []string {
	seen := make(map[string]struct{}, len(modules))
	out := make([]string, 0, len(modules))
	for _, m := range modules {
		if _, dup := seen[m]; dup || m == "" {
			continue
		}
		seen[m] = struct{}{}
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

const setDisplayVersionSQL = `UPDATE work_items SET codex_version = $2, updated_at = now() WHERE id = $1`

// PinTx writes pin, replacing an existing pin of the same work item, and sets
// work_items.codex_version to its product_version.
func (s *PinStore) PinTx(ctx context.Context, tx pgx.Tx, pin Pin) error {
	if err := pin.validate(); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO work_item_codex_pins (work_item_id, product_id, branch, product_version, modules, created_at)
		VALUES ($1, $2, $3, $4, $5, now())
		ON CONFLICT (work_item_id) DO UPDATE
		   SET product_id = EXCLUDED.product_id, branch = EXCLUDED.branch,
		       product_version = EXCLUDED.product_version, modules = EXCLUDED.modules,
		       created_at = EXCLUDED.created_at
	`, pin.WorkItemID, pin.ProductID, pin.Branch, pin.ProductVersion, distinctSorted(pin.Modules))
	if err != nil {
		return fmt.Errorf("codex pin for %s: %w", pin.WorkItemID, err)
	}
	if _, err := tx.Exec(ctx, setDisplayVersionSQL, pin.WorkItemID, pin.ProductVersion); err != nil {
		return fmt.Errorf("codex pin for %s: set codex_version: %w", pin.WorkItemID, err)
	}
	return nil
}

const pinColumns = `work_item_id, product_id, branch, product_version, modules, created_at`

func scanPin(row pgx.Row) (Pin, error) {
	var p Pin
	err := row.Scan(&p.WorkItemID, &p.ProductID, &p.Branch, &p.ProductVersion, &p.Modules, &p.CreatedAt)
	if p.Modules == nil {
		p.Modules = []string{}
	}
	return p, err
}

// CopyToChildTx gives childID the parent's pin (replacing any it has) and
// sets the child's work_items.codex_version. copied is false, with nothing
// written, when the parent has no pin.
func (s *PinStore) CopyToChildTx(ctx context.Context, tx pgx.Tx, parentID, childID uuid.UUID) (pin Pin, copied bool, err error) {
	pin, err = scanPin(tx.QueryRow(ctx, `
		INSERT INTO work_item_codex_pins (work_item_id, product_id, branch, product_version, modules, created_at)
		SELECT $2, product_id, branch, product_version, modules, now()
		  FROM work_item_codex_pins
		 WHERE work_item_id = $1
		ON CONFLICT (work_item_id) DO UPDATE
		   SET product_id = EXCLUDED.product_id, branch = EXCLUDED.branch,
		       product_version = EXCLUDED.product_version, modules = EXCLUDED.modules,
		       created_at = EXCLUDED.created_at
		RETURNING `+pinColumns, parentID, childID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Pin{}, false, nil
	}
	if err != nil {
		return Pin{}, false, fmt.Errorf("codex pin: copy %s to subtask %s: %w", parentID, childID, err)
	}
	if _, err := tx.Exec(ctx, setDisplayVersionSQL, childID, pin.ProductVersion); err != nil {
		return Pin{}, false, fmt.Errorf("codex pin: copy %s to subtask %s: set codex_version: %w", parentID, childID, err)
	}
	return pin, true, nil
}

// RepinTx moves the work item's pin to productVersion — the codex version a
// plan was built on — keeping its product, branch and modules, and sets
// work_items.codex_version. The pin row is locked for the rest of tx. A pin
// on another branch than branch (a newer brief) is not moved. It returns the
// row's pin after the update; found is false, with nothing written, when the
// item has no pin.
//
// The planner calls it in the plan's transaction, so after a plan commits
// the parent's pin (and every subtask's copy) is the version planned
// against.
func (s *PinStore) RepinTx(ctx context.Context, tx pgx.Tx, workItemID uuid.UUID, branch string, productVersion int) (pin Pin, found bool, err error) {
	if productVersion < 0 {
		return Pin{}, false, fmt.Errorf("codex pin for %s: negative product_version %d", workItemID, productVersion)
	}
	pin, err = scanPin(tx.QueryRow(ctx, `SELECT `+pinColumns+` FROM work_item_codex_pins WHERE work_item_id = $1 FOR UPDATE`, workItemID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Pin{}, false, nil
	}
	if err != nil {
		return Pin{}, false, fmt.Errorf("codex pin for %s: %w", workItemID, err)
	}
	if pin.Branch != branch || pin.ProductVersion == productVersion {
		return pin, true, nil
	}
	pin, err = scanPin(tx.QueryRow(ctx, `
		UPDATE work_item_codex_pins SET product_version = $2, created_at = now()
		 WHERE work_item_id = $1
		RETURNING `+pinColumns, workItemID, productVersion))
	if err != nil {
		return Pin{}, false, fmt.Errorf("codex pin for %s: repin: %w", workItemID, err)
	}
	if _, err := tx.Exec(ctx, setDisplayVersionSQL, workItemID, productVersion); err != nil {
		return Pin{}, false, fmt.Errorf("codex pin for %s: set codex_version: %w", workItemID, err)
	}
	return pin, true, nil
}

// Get returns the work item's pin; found is false when it has none.
func (s *PinStore) Get(ctx context.Context, workItemID uuid.UUID) (pin Pin, found bool, err error) {
	pin, err = scanPin(s.db.QueryRow(ctx, `SELECT `+pinColumns+` FROM work_item_codex_pins WHERE work_item_id = $1`, workItemID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Pin{}, false, nil
	}
	if err != nil {
		return Pin{}, false, fmt.Errorf("codex pin for %s: %w", workItemID, err)
	}
	return pin, true, nil
}

// ListPinned returns the work items pinned to productID on branch whose
// state is one of states (every state when states is empty), oldest pin
// first. Parents and subtasks are both returned; ParentID tells them apart.
func (s *PinStore) ListPinned(ctx context.Context, productID uuid.UUID, branch string, states []models.WorkItemState) ([]PinnedItem, error) {
	if err := checkScope(productID, branch); err != nil {
		return nil, err
	}
	stateFilter := make([]string, 0, len(states))
	for _, st := range states {
		stateFilter = append(stateFilter, string(st))
	}
	rows, err := s.db.Query(ctx, `
		SELECT p.work_item_id, p.product_id, p.branch, p.product_version, p.modules, p.created_at,
		       w.state::text, w.parent_id
		  FROM work_item_codex_pins p
		  JOIN work_items w ON w.id = p.work_item_id
		 WHERE p.product_id = $1 AND p.branch = $2
		   AND (cardinality($3::text[]) = 0 OR w.state::text = ANY($3::text[]))
		 ORDER BY p.created_at, p.work_item_id
	`, productID, branch, stateFilter)
	if err != nil {
		return nil, fmt.Errorf("list codex pins for %s/%s: %w", productID, branch, err)
	}
	defer rows.Close()
	var out []PinnedItem
	for rows.Next() {
		var it PinnedItem
		var state string
		if err := rows.Scan(&it.WorkItemID, &it.ProductID, &it.Branch, &it.ProductVersion, &it.Modules, &it.CreatedAt, &state, &it.ParentID); err != nil {
			return nil, fmt.Errorf("scan codex pin: %w", err)
		}
		if it.Modules == nil {
			it.Modules = []string{}
		}
		it.State = models.WorkItemState(state)
		out = append(out, it)
	}
	return out, rows.Err()
}
