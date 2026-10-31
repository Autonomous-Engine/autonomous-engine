package codex

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/outbox"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Codex versions (migrations 002, 009, 021, 060).
//
// A version is one row of codex_versions for a (product_id, branch). Its
// product_version counts that product and branch's versions from 1 and is
// what every reader compares; version_number is the row's global id
// (one SERIAL across all products) and is kept only as an identifier.
//
// Every version belongs to a product. RecordVersion / RecordVersionTx
// refuse uuid.Nil (ErrNoProduct), and every reader takes the product and
// branch it asks about and refuses uuid.Nil too. Rows written before
// products existed (product_id NULL, product_version NULL) are seen by no
// reader; a caller without a product has no codex version to read or write
// (the legacy single-repo index path writes codex rows but no version).

// ErrNoProduct is returned when a version is written or read without a
// product.
var ErrNoProduct = errors.New("codex version: no product")

// NewVersion is one codex change to record.
type NewVersion struct {
	ProductID uuid.UUID
	// Branch the change was indexed or written on. RecordVersion resolves an
	// empty Branch to the product's default branch (ResolveDefaultBranch);
	// RecordVersionTx requires it.
	Branch string
	// SectionsChanged are typed entries (sections.go). They are normalised
	// (legacy values prefixed, deduplicated, sorted) before they are stored.
	SectionsChanged []string
	Summary         string
	Source          models.CodexChangeSource
	// Diff is stored as JSONB; nil stores {}.
	Diff []byte
	// Trigger names the writer on the codex.indexed event (Trigger*
	// constants).
	Trigger string
}

// RecordedVersion is the version a write created.
type RecordedVersion struct {
	ID              uuid.UUID
	VersionNumber   int
	ProductVersion  int
	ProductID       uuid.UUID
	Branch          string
	SectionsChanged []string
	CreatedAt       time.Time
}

// RecordVersion writes v in a transaction of its own (RecordVersionTx).
func (c *Codex) RecordVersion(ctx context.Context, v NewVersion) (RecordedVersion, error) {
	if v.ProductID == uuid.Nil {
		return RecordedVersion{}, ErrNoProduct
	}
	if v.Branch == "" {
		branch, err := c.resolveDefault(ctx, v.ProductID)
		if err != nil {
			return RecordedVersion{}, fmt.Errorf("record codex version: %w", err)
		}
		v.Branch = branch
	}
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return RecordedVersion{}, fmt.Errorf("record codex version: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once committed
	rec, err := RecordVersionTx(ctx, tx, v)
	if err != nil {
		return RecordedVersion{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return RecordedVersion{}, fmt.Errorf("record codex version: commit: %w", err)
	}
	return rec, nil
}

const versionLockSQL = `SELECT pg_advisory_xact_lock(hashtextextended('codex_versions:' || $1::text || ':' || $2, 0))`

// RecordVersionTx inserts a codex_versions row for (v.ProductID, v.Branch)
// in tx, numbered one past that product and branch's highest
// product_version, and enqueues its codex.indexed event (IndexedEvent)
// through the outbox in the same transaction.
//
// Concurrent writers for the same product and branch are serialised by a
// transaction-scoped advisory lock keyed by both, taken before the number is
// read; writers for other products or branches do not wait. The lock is
// held until tx ends, so callers should commit promptly. The unique index
// codex_versions_product_branch_version_key is the backstop.
func RecordVersionTx(ctx context.Context, tx pgx.Tx, v NewVersion) (RecordedVersion, error) {
	if v.ProductID == uuid.Nil {
		return RecordedVersion{}, ErrNoProduct
	}
	if v.Branch == "" {
		return RecordedVersion{}, fmt.Errorf("record codex version: empty branch for product %s; caller must resolve branch", v.ProductID)
	}
	diff := v.Diff
	if len(diff) == 0 {
		diff = []byte(`{}`)
	}
	rec := RecordedVersion{
		ID:              uuid.New(),
		ProductID:       v.ProductID,
		Branch:          v.Branch,
		SectionsChanged: NormalizeChanges(v.SectionsChanged),
	}
	if _, err := tx.Exec(ctx, versionLockSQL, v.ProductID, v.Branch); err != nil {
		return RecordedVersion{}, fmt.Errorf("record codex version: lock %s/%s: %w", v.ProductID, v.Branch, err)
	}
	err := tx.QueryRow(ctx, `
		INSERT INTO codex_versions (id, product_id, branch, product_version, sections_changed, change_summary, change_source, diff, created_at)
		SELECT $1, $2, $3, COALESCE(MAX(product_version), 0) + 1, $4, $5, $6, $7, now()
		  FROM codex_versions
		 WHERE product_id = $2 AND branch = $3
		RETURNING version_number, product_version, created_at
	`, rec.ID, v.ProductID, v.Branch, rec.SectionsChanged, v.Summary, v.Source, diff).Scan(&rec.VersionNumber, &rec.ProductVersion, &rec.CreatedAt)
	if err != nil {
		return RecordedVersion{}, fmt.Errorf("record codex version: insert: %w", err)
	}
	ev, err := indexedOutboxEvent(IndexedEvent{
		ProductID:       rec.ProductID,
		Branch:          rec.Branch,
		ProductVersion:  rec.ProductVersion,
		VersionNumber:   rec.VersionNumber,
		SectionsChanged: rec.SectionsChanged,
		Trigger:         v.Trigger,
	})
	if err != nil {
		return RecordedVersion{}, fmt.Errorf("record codex version: build codex.indexed: %w", err)
	}
	if err := outbox.Enqueue(ctx, tx, ev); err != nil {
		return RecordedVersion{}, fmt.Errorf("record codex version: enqueue codex.indexed: %w", err)
	}
	return rec, nil
}

func checkScope(productID uuid.UUID, branch string) error {
	if productID == uuid.Nil {
		return ErrNoProduct
	}
	if branch == "" {
		return fmt.Errorf("codex version: empty branch for product %s", productID)
	}
	return nil
}

// GetCurrentVersion returns the highest product_version of productID on
// branch, 0 when the branch has no version yet.
func (c *Codex) GetCurrentVersion(ctx context.Context, productID uuid.UUID, branch string) (int, error) {
	if err := checkScope(productID, branch); err != nil {
		return 0, err
	}
	var version int
	err := c.pool.QueryRow(ctx, `
		SELECT COALESCE(MAX(product_version), 0) FROM codex_versions
		 WHERE product_id = $1 AND branch = $2
	`, productID, branch).Scan(&version)
	if err != nil {
		return 0, fmt.Errorf("get current version: %w", err)
	}
	return version, nil
}

const versionColumns = `id, version_number, sections_changed, change_summary, change_source, diff, created_at, product_id, branch, product_version`

func scanVersion(row pgx.Row) (*models.CodexVersion, error) {
	v := &models.CodexVersion{}
	var pv *int
	if err := row.Scan(&v.ID, &v.VersionNumber, &v.SectionsChanged, &v.ChangeSummary, &v.ChangeSource, &v.Diff, &v.CreatedAt, &v.ProductID, &v.Branch, &pv); err != nil {
		return nil, err
	}
	if pv != nil {
		v.ProductVersion = *pv
	}
	return v, nil
}

// GetLatestVersion returns the newest version of productID on branch, or
// nil (and no error) when there is none.
func (c *Codex) GetLatestVersion(ctx context.Context, productID uuid.UUID, branch string) (*models.CodexVersion, error) {
	if err := checkScope(productID, branch); err != nil {
		return nil, err
	}
	v, err := scanVersion(c.pool.QueryRow(ctx, `
		SELECT `+versionColumns+` FROM codex_versions
		 WHERE product_id = $1 AND branch = $2
		 ORDER BY product_version DESC
		 LIMIT 1
	`, productID, branch))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get latest version: %w", err)
	}
	return v, nil
}

// GetVersion retrieves a single codex version by its global version_number
// (the row id; not a per-product number).
func (c *Codex) GetVersion(ctx context.Context, versionNumber int) (*models.CodexVersion, error) {
	v, err := scanVersion(c.pool.QueryRow(ctx, `
		SELECT `+versionColumns+` FROM codex_versions WHERE version_number = $1
	`, versionNumber))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("codex version %d not found", versionNumber)
	}
	if err != nil {
		return nil, fmt.Errorf("get version: %w", err)
	}
	return v, nil
}

// GetChangedSectionsBetween returns the typed entries (NormalizeChanges:
// legacy values as module:, distinct, sorted) changed by any version of
// productID on branch with product_version between fromVersion and
// toVersion, inclusive.
func (c *Codex) GetChangedSectionsBetween(ctx context.Context, productID uuid.UUID, branch string, fromVersion, toVersion int) ([]string, error) {
	if err := checkScope(productID, branch); err != nil {
		return nil, err
	}
	rows, err := c.pool.Query(ctx, `
		SELECT DISTINCT unnest(sections_changed) AS section_name
		  FROM codex_versions
		 WHERE product_id = $1 AND branch = $2
		   AND product_version BETWEEN $3 AND $4
	`, productID, branch, fromVersion, toVersion)
	if err != nil {
		return nil, fmt.Errorf("get changed sections between: %w", err)
	}
	defer rows.Close()

	var sections []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan section name: %w", err)
		}
		sections = append(sections, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate changed sections: %w", err)
	}
	return NormalizeChanges(sections), nil
}

// IsWorkItemStale reports whether a version of productID on branch newer
// than pinnedVersion changed any of dependent (typed entries; a bare name
// means module:<name>, see Touches).
func (c *Codex) IsWorkItemStale(ctx context.Context, productID uuid.UUID, branch string, pinnedVersion int, dependent []string) (bool, error) {
	currentVersion, err := c.GetCurrentVersion(ctx, productID, branch)
	if err != nil {
		return false, fmt.Errorf("is work item stale - get current version: %w", err)
	}
	if pinnedVersion >= currentVersion {
		return false, nil
	}
	changed, err := c.GetChangedSectionsBetween(ctx, productID, branch, pinnedVersion+1, currentVersion)
	if err != nil {
		return false, fmt.Errorf("is work item stale - get changed sections: %w", err)
	}
	return Touches(changed, dependent), nil
}
