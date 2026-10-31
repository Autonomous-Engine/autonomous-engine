package evidence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when no verification row matches a lookup.
var ErrNotFound = errors.New("implementation verification not found")

// Verification is one persisted implementation_verifications row (migration
// 044): the evidence plus the database identity it was stored under.
type Verification struct {
	ID        uuid.UUID
	CreatedAt time.Time
	Evidence  ImplementationEvidence
}

// Store owns the implementation_verifications table.
type Store struct {
	db *pgxpool.Pool
	// configSnapshotID, when set, is written to config_snapshot_id
	// (migration 053) on every row this store inserts.
	configSnapshotID string
}

// NewStore constructs a Store bound to the given pool.
func NewStore(db *pgxpool.Pool) *Store { return &Store{db: db} }

// WithConfigSnapshot returns a store that records id as the configuration
// snapshot (internal/configsnap, M4-05) of the evidence it inserts. The
// snapshot is not part of the evidence contract (the JSON document and its
// schema version are unchanged); it is a column beside it. "" returns s.
func (s *Store) WithConfigSnapshot(id string) *Store {
	if s == nil || id == "" {
		return s
	}
	c := *s
	c.configSnapshotID = id
	return &c
}

const insertSQL = `
	INSERT INTO implementation_verifications (
		id, work_item_id, task_id, repository_id, schema_version,
		base_sha, head_sha, branch, outcome, evidence
	) VALUES (
		$1, $2, $3, $4, $5, $6, $7, $8, $9, $10::jsonb
	)`

const selectCols = `SELECT id, created_at, evidence FROM implementation_verifications`

const selectByWorkItemSQL = selectCols + `
	WHERE work_item_id = $1
	ORDER BY created_at DESC, id DESC`

const selectLatestByHeadSHASQL = selectCols + `
	WHERE head_sha = $1
	ORDER BY created_at DESC, id DESC
	LIMIT 1`

// Insert validates ev and persists it, returning the new row id. Invalid
// evidence (unknown schema version, inconsistent outcome, unreadable ids) is
// rejected before touching the database so the table never holds a record
// that the contract itself would refuse.
func (s *Store) Insert(ctx context.Context, ev *ImplementationEvidence) (uuid.UUID, error) {
	return insert(ctx, s.db, ev, s.configSnapshotID)
}

// InsertTx is Insert inside the caller's transaction, for a producer that
// must commit the evidence row together with the event that carries it (see
// internal/outbox). It shares insertSQL and validation with Insert. On error
// the caller must roll its transaction back; on a later rollback the returned
// id names a row that does not exist, so it must not be published until the
// transaction commits.
func (s *Store) InsertTx(ctx context.Context, tx pgx.Tx, ev *ImplementationEvidence) (uuid.UUID, error) {
	if tx == nil {
		return uuid.Nil, errors.New("insert implementation verification: nil transaction")
	}
	return insert(ctx, tx, ev, s.configSnapshotID)
}

// execer is the write surface shared by *pgxpool.Pool and pgx.Tx.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// insertWithSnapshotSQL is insertSQL plus config_snapshot_id (migration
// 053). insertSQL stays the statement for stores without a snapshot, so the
// column is only ever named when there is a value for it.
const insertWithSnapshotSQL = `
	INSERT INTO implementation_verifications (
		id, work_item_id, task_id, repository_id, schema_version,
		base_sha, head_sha, branch, outcome, evidence, config_snapshot_id
	) VALUES (
		$1, $2, $3, $4, $5, $6, $7, $8, $9, $10::jsonb, $11
	)`

func insert(ctx context.Context, ex execer, ev *ImplementationEvidence, configSnapshotID string) (uuid.UUID, error) {
	id := uuid.New()
	args, err := insertArgs(id, ev)
	if err != nil {
		return uuid.Nil, err
	}
	sql := insertSQL
	if configSnapshotID != "" {
		sql = insertWithSnapshotSQL
		args = append(args, configSnapshotID)
	}
	if _, err := ex.Exec(ctx, sql, args...); err != nil {
		return uuid.Nil, fmt.Errorf("insert implementation verification: %w", err)
	}
	return id, nil
}

// GetByWorkItem returns every verification recorded for a work item, newest
// first. An empty (non-nil) slice means none exist. The nil UUID is never a
// real work item and returns ErrNotFound without querying.
func (s *Store) GetByWorkItem(ctx context.Context, workID uuid.UUID) ([]Verification, error) {
	if workID == uuid.Nil {
		return nil, ErrNotFound
	}
	rows, err := s.db.Query(ctx, selectByWorkItemSQL, workID)
	if err != nil {
		return nil, fmt.Errorf("query verifications by work item: %w", err)
	}
	defer rows.Close()

	out := []Verification{}
	for rows.Next() {
		v, err := scanVerification(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *v)
	}
	return out, rows.Err()
}

// GetLatestByHeadSHA returns the most recent verification whose head_sha
// equals sha, or ErrNotFound. An empty sha would match every unverified
// legacy row, so it returns ErrNotFound without querying.
func (s *Store) GetLatestByHeadSHA(ctx context.Context, sha string) (*Verification, error) {
	if sha == "" {
		return nil, ErrNotFound
	}
	row := s.db.QueryRow(ctx, selectLatestByHeadSHASQL, sha)
	return scanVerification(row)
}

// insertArgs validates ev and produces the positional arguments for
// insertSQL, in column order. Kept separate from Insert so the mapping can be
// unit-tested without a database.
func insertArgs(id uuid.UUID, ev *ImplementationEvidence) ([]any, error) {
	if ev == nil {
		return nil, errors.New("evidence is nil")
	}
	if err := ev.Validate(); err != nil {
		return nil, fmt.Errorf("invalid evidence: %w", err)
	}
	workItemID, err := uuid.Parse(ev.WorkID)
	if err != nil {
		return nil, fmt.Errorf("work_id %q is not a UUID: %w", ev.WorkID, err)
	}
	// repository_id is nullable; an empty string is treated as absent because
	// legacy task messages omit it rather than sending null.
	var repositoryID *uuid.UUID
	if ev.RepositoryID != nil && *ev.RepositoryID != "" {
		parsed, err := uuid.Parse(*ev.RepositoryID)
		if err != nil {
			return nil, fmt.Errorf("repository_id %q is not a UUID: %w", *ev.RepositoryID, err)
		}
		repositoryID = &parsed
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		return nil, fmt.Errorf("marshal evidence: %w", err)
	}
	return []any{
		id,
		workItemID,
		ev.TaskID,
		repositoryID,
		ev.SchemaVersion,
		ev.BaseSHA,
		ev.HeadSHA,
		ev.Branch,
		string(ev.Outcome),
		payload,
	}, nil
}

type scannerLike interface {
	Scan(dest ...any) error
}

// scanVerification maps one selectCols row. The stored evidence is re-parsed
// and re-validated on the way out so a row written under a newer, unknown
// schema version surfaces as an error instead of a half-decoded record.
func scanVerification(r scannerLike) (*Verification, error) {
	var v Verification
	var payload []byte
	if err := r.Scan(&v.ID, &v.CreatedAt, &payload); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("scan implementation verification: %w", err)
	}
	ev, err := Parse(payload)
	if err != nil {
		return nil, fmt.Errorf("implementation verification %s: %w", v.ID, err)
	}
	v.Evidence = *ev
	return &v, nil
}
