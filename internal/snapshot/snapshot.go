// Package snapshot pins the repository snapshot a work item is built against
// (roadmap 2.1, tickets M2-01/M2-02): one base commit per (work item,
// repository), resolved once at intake by product-agent and copied to each
// repo-scoped subtask by the planner.
//
// Tables (migration 050): work_item_base_shas holds the pins; base_sha_events
// is their audit trail (resolved, drift_detected, repinned). An item with no
// row predates pinning: readers treat it as legacy-unpinned (NoteUnpinned)
// and keep their branch-name behaviour. Nothing here repins; what happens on
// drift is M2-03's decision.
package snapshot

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Pin is the base commit of one repository for one work item.
type Pin struct {
	WorkItemID   uuid.UUID `json:"work_item_id"`
	RepositoryID uuid.UUID `json:"repository_id"`
	// BaseRef is the branch the SHA was resolved from (repositories.base_branch
	// at resolution time).
	BaseRef string `json:"base_ref"`
	// BaseSHA is the full 40-character lower-case commit id.
	BaseSHA    string    `json:"base_sha"`
	ResolvedAt time.Time `json:"resolved_at"`
}

// EventKind is a base_sha_events.kind value.
type EventKind string

const (
	// EventResolved: a pin was created (resolved at intake, or inherited by
	// a subtask from its parent).
	EventResolved EventKind = "resolved"
	// EventDriftDetected: a comparison found the base moved away from the
	// pin. The pin is kept.
	EventDriftDetected EventKind = "drift_detected"
	// EventRepinned: the pin was replaced (M2-03; not written by M2-01/02).
	EventRepinned EventKind = "repinned"
)

// Event is one base_sha_events row.
type Event struct {
	ID           uuid.UUID `json:"id"`
	WorkItemID   uuid.UUID `json:"work_item_id"`
	RepositoryID uuid.UUID `json:"repository_id"`
	Kind         EventKind `json:"kind"`
	OldSHA       string    `json:"old_sha"`
	NewSHA       string    `json:"new_sha"`
	Detail       string    `json:"detail"`
	CreatedAt    time.Time `json:"created_at"`
}

var shaPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// ValidSHA reports whether s is a full lower-case hex commit id, the only form
// work_item_base_shas accepts.
func ValidSHA(s string) bool { return shaPattern.MatchString(s) }

// Querier is the pooled read surface (*pgxpool.Pool satisfies it).
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Store reads and writes pins. The *Tx methods touch only the transaction
// they are given, so a Store with a nil Querier is enough for them (as with
// workgraph.New(nil)).
type Store struct {
	db Querier
}

// NewStore returns a Store reading through db.
func NewStore(db Querier) *Store { return &Store{db: db} }

func (p Pin) validate() error {
	if p.WorkItemID == uuid.Nil || p.RepositoryID == uuid.Nil {
		return errors.New("snapshot: pin needs a work item id and a repository id")
	}
	if p.BaseRef == "" {
		return fmt.Errorf("snapshot: pin for repository %s has no base ref", p.RepositoryID)
	}
	if !ValidSHA(p.BaseSHA) {
		return fmt.Errorf("snapshot: pin for repository %s: %q is not a full commit id", p.RepositoryID, p.BaseSHA)
	}
	return nil
}

const upsertSQL = `
INSERT INTO work_item_base_shas (work_item_id, repository_id, base_ref, base_sha, resolved_at)
VALUES ($1, $2, $3, $4, COALESCE($5, now()))
ON CONFLICT (work_item_id, repository_id)
DO UPDATE SET base_ref = EXCLUDED.base_ref, base_sha = EXCLUDED.base_sha, resolved_at = EXCLUDED.resolved_at`

// UpsertTx writes pins, replacing any existing pin for the same (work item,
// repository). It records no event: callers that create pins at intake use
// PinTx, which never replaces one; replacing a pin is a repin (M2-03), whose
// caller records EventRepinned itself.
func (s *Store) UpsertTx(ctx context.Context, tx pgx.Tx, pins ...Pin) error {
	for _, p := range pins {
		if err := p.validate(); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, upsertSQL, p.WorkItemID, p.RepositoryID, p.BaseRef, p.BaseSHA, nullTime(p.ResolvedAt)); err != nil {
			return fmt.Errorf("snapshot: upsert pin for repository %s: %w", p.RepositoryID, err)
		}
	}
	return nil
}

// PinResult is what PinTx did.
type PinResult string

const (
	// PinCreated: no pin existed; it was written with an EventResolved row.
	PinCreated PinResult = "created"
	// PinUnchanged: the existing pin already has this SHA; nothing written.
	PinUnchanged PinResult = "unchanged"
	// PinDrifted: a different pin exists (an item re-entering intake after
	// its base moved). The existing pin is KEPT and an EventDriftDetected row
	// records old and new SHAs; M2-03 decides whether to repin.
	PinDrifted PinResult = "drifted"
)

const insertIfAbsentSQL = `
INSERT INTO work_item_base_shas (work_item_id, repository_id, base_ref, base_sha, resolved_at)
VALUES ($1, $2, $3, $4, COALESCE($5, now()))
ON CONFLICT (work_item_id, repository_id) DO NOTHING`

// PinTx creates pin unless the item already has one for the repository, and
// records the audit event either way something happened (see PinResult).
// detail is stored on the event (how the SHA was resolved).
func (s *Store) PinTx(ctx context.Context, tx pgx.Tx, pin Pin, detail string) (PinResult, error) {
	if err := pin.validate(); err != nil {
		return "", err
	}
	tag, err := tx.Exec(ctx, insertIfAbsentSQL, pin.WorkItemID, pin.RepositoryID, pin.BaseRef, pin.BaseSHA, nullTime(pin.ResolvedAt))
	if err != nil {
		return "", fmt.Errorf("snapshot: pin repository %s: %w", pin.RepositoryID, err)
	}
	if tag.RowsAffected() == 1 {
		err := s.RecordEventTx(ctx, tx, Event{
			WorkItemID: pin.WorkItemID, RepositoryID: pin.RepositoryID,
			Kind: EventResolved, NewSHA: pin.BaseSHA, Detail: detail,
		})
		return PinCreated, err
	}
	var existing string
	if err := tx.QueryRow(ctx,
		`SELECT base_sha FROM work_item_base_shas WHERE work_item_id = $1 AND repository_id = $2 FOR SHARE`,
		pin.WorkItemID, pin.RepositoryID).Scan(&existing); err != nil {
		return "", fmt.Errorf("snapshot: read existing pin for repository %s: %w", pin.RepositoryID, err)
	}
	if existing == pin.BaseSHA {
		return PinUnchanged, nil
	}
	err = s.RecordEventTx(ctx, tx, Event{
		WorkItemID: pin.WorkItemID, RepositoryID: pin.RepositoryID,
		Kind: EventDriftDetected, OldSHA: existing, NewSHA: pin.BaseSHA,
		Detail: fmt.Sprintf("%s moved since the pin was resolved; the pin is kept (%s)", pin.BaseRef, detail),
	})
	return PinDrifted, err
}

const copyToChildSQL = `
INSERT INTO work_item_base_shas (work_item_id, repository_id, base_ref, base_sha, resolved_at)
SELECT $2, repository_id, base_ref, base_sha, now()
  FROM work_item_base_shas
 WHERE work_item_id = $1 AND repository_id = $3
ON CONFLICT (work_item_id, repository_id) DO NOTHING
RETURNING base_ref, base_sha, resolved_at`

// CopyToChildTx gives childID the parent's pin for repositoryID and records an
// EventResolved row naming the parent. copied is false, with nothing written,
// when the parent has no pin for that repository (legacy-unpinned) or the
// child already has one.
func (s *Store) CopyToChildTx(ctx context.Context, tx pgx.Tx, parentID, childID, repositoryID uuid.UUID) (pin Pin, copied bool, err error) {
	pin = Pin{WorkItemID: childID, RepositoryID: repositoryID}
	err = tx.QueryRow(ctx, copyToChildSQL, parentID, childID, repositoryID).Scan(&pin.BaseRef, &pin.BaseSHA, &pin.ResolvedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Pin{}, false, nil
	}
	if err != nil {
		return Pin{}, false, fmt.Errorf("snapshot: copy pin of %s to subtask %s for repository %s: %w", parentID, childID, repositoryID, err)
	}
	if err := s.RecordEventTx(ctx, tx, Event{
		WorkItemID: childID, RepositoryID: repositoryID, Kind: EventResolved,
		NewSHA: pin.BaseSHA, Detail: "inherited from parent work item " + parentID.String(),
	}); err != nil {
		return Pin{}, false, err
	}
	return pin, true, nil
}

// RecordEventTx writes one base_sha_events row.
func (s *Store) RecordEventTx(ctx context.Context, tx pgx.Tx, ev Event) error {
	switch ev.Kind {
	case EventResolved, EventDriftDetected, EventRepinned:
	default:
		return fmt.Errorf("snapshot: unknown event kind %q", ev.Kind)
	}
	if ev.WorkItemID == uuid.Nil || ev.RepositoryID == uuid.Nil {
		return errors.New("snapshot: event needs a work item id and a repository id")
	}
	if ev.ID == uuid.Nil {
		ev.ID = uuid.New()
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO base_sha_events (id, work_item_id, repository_id, kind, old_sha, new_sha, detail, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, COALESCE($8, now()))`,
		ev.ID, ev.WorkItemID, ev.RepositoryID, string(ev.Kind), ev.OldSHA, ev.NewSHA, ev.Detail, nullTime(ev.CreatedAt))
	if err != nil {
		return fmt.Errorf("snapshot: record %s event for repository %s: %w", ev.Kind, ev.RepositoryID, err)
	}
	return nil
}

const pinColumns = `work_item_id, repository_id, base_ref, base_sha, resolved_at`

// Get returns the pin of workID for repositoryID. found is false (and err
// nil) when there is none: the legacy-unpinned case.
func (s *Store) Get(ctx context.Context, workID, repositoryID uuid.UUID) (Pin, bool, error) {
	var p Pin
	err := s.db.QueryRow(ctx, `SELECT `+pinColumns+` FROM work_item_base_shas WHERE work_item_id = $1 AND repository_id = $2`,
		workID, repositoryID).Scan(&p.WorkItemID, &p.RepositoryID, &p.BaseRef, &p.BaseSHA, &p.ResolvedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Pin{}, false, nil
	}
	if err != nil {
		return Pin{}, false, fmt.Errorf("snapshot: get pin for %s/%s: %w", workID, repositoryID, err)
	}
	return p, true, nil
}

// GetAll returns every pin of workID ordered by repository id; empty when the
// item is legacy-unpinned.
func (s *Store) GetAll(ctx context.Context, workID uuid.UUID) ([]Pin, error) {
	rows, err := s.db.Query(ctx, `SELECT `+pinColumns+` FROM work_item_base_shas WHERE work_item_id = $1 ORDER BY repository_id`, workID)
	if err != nil {
		return nil, fmt.Errorf("snapshot: list pins for %s: %w", workID, err)
	}
	defer rows.Close()
	var out []Pin
	for rows.Next() {
		var p Pin
		if err := rows.Scan(&p.WorkItemID, &p.RepositoryID, &p.BaseRef, &p.BaseSHA, &p.ResolvedAt); err != nil {
			return nil, fmt.Errorf("snapshot: scan pin for %s: %w", workID, err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Events returns the audit trail of workID, oldest first.
func (s *Store) Events(ctx context.Context, workID uuid.UUID) ([]Event, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id, work_item_id, repository_id, kind, old_sha, new_sha, detail, created_at
		  FROM base_sha_events WHERE work_item_id = $1 ORDER BY created_at, id`, workID)
	if err != nil {
		return nil, fmt.Errorf("snapshot: list events for %s: %w", workID, err)
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var ev Event
		var kind string
		if err := rows.Scan(&ev.ID, &ev.WorkItemID, &ev.RepositoryID, &kind, &ev.OldSHA, &ev.NewSHA, &ev.Detail, &ev.CreatedAt); err != nil {
			return nil, fmt.Errorf("snapshot: scan event for %s: %w", workID, err)
		}
		ev.Kind = EventKind(kind)
		out = append(out, ev)
	}
	return out, rows.Err()
}

// SHAFor picks the base SHA to put on a work-item-level message (qa.ready)
// from the item's pins: the pin of repositoryID when one is given ("" when
// that repository is not pinned); otherwise the only pin, or the common SHA
// when every pin agrees. "" when there is no pin or the pins differ (a
// multi-repository item has no single base); callers then omit the field.
func SHAFor(pins []Pin, repositoryID *uuid.UUID) string {
	if repositoryID != nil && *repositoryID != uuid.Nil {
		for _, p := range pins {
			if p.RepositoryID == *repositoryID {
				return p.BaseSHA
			}
		}
		return ""
	}
	sha := ""
	for _, p := range pins {
		if sha != "" && p.BaseSHA != sha {
			return ""
		}
		sha = p.BaseSHA
	}
	return sha
}

// IsCheckViolation reports whether err is a PostgreSQL CHECK violation (the
// base_sha format check, for tests and callers that want to say so).
func IsCheckViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23514"
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}
