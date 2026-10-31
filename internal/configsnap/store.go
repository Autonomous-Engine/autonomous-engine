package configsnap

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrNotFound is returned by Get for an unknown id.
var ErrNotFound = errors.New("config snapshot not found")

// Store writes and reads config_snapshots (migration 053). It caches the
// ids it has written or seen, so resolving the same configuration again
// costs a hash and no write.
type Store struct {
	db Execer

	mu    sync.Mutex
	known map[string]struct{}
}

// NewStore returns a store over db.
func NewStore(db Execer) *Store {
	return &Store{db: db, known: map[string]struct{}{}}
}

const insertSQL = `INSERT INTO config_snapshots (id, schema_version, agent_definition_id, agent_type, provider, model_id, document)
	VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb)
	ON CONFLICT (id) DO NOTHING`

// GetOrCreate stores doc under its content hash (once; an existing row is
// never touched) and returns the id.
func (s *Store) GetOrCreate(ctx context.Context, doc Document) (string, error) {
	id, canonical, err := Hash(doc)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	_, ok := s.known[id]
	s.mu.Unlock()
	if ok {
		return id, nil
	}
	defID, agentType, provider, modelID := scalars(doc)
	if _, err := s.db.Exec(ctx, insertSQL, id, SchemaVersion, defID, agentType, provider, modelID, string(canonical)); err != nil {
		return "", fmt.Errorf("configsnap: insert snapshot: %w", err)
	}
	s.mu.Lock()
	s.known[id] = struct{}{}
	s.mu.Unlock()
	return id, nil
}

// CaptureAndStore captures in and stores it, returning the id.
func (s *Store) CaptureAndStore(ctx context.Context, in Input) (string, error) {
	doc, err := Capture(ctx, s.db, in)
	if err != nil {
		return "", err
	}
	return s.GetOrCreate(ctx, doc)
}

// Snapshot is one stored row.
type Snapshot struct {
	ID                string    `json:"id"`
	SchemaVersion     int       `json:"schema_version"`
	AgentDefinitionID *string   `json:"agent_definition_id"`
	AgentType         string    `json:"agent_type"`
	Provider          string    `json:"provider"`
	ModelID           string    `json:"model_id"`
	CreatedAt         time.Time `json:"created_at"`
	Document          Document  `json:"document"`
}

// Get returns the stored snapshot with id.
func (s *Store) Get(ctx context.Context, id string) (*Snapshot, error) {
	if !ValidID(id) {
		return nil, ErrNotFound
	}
	var out Snapshot
	var defID *uuid.UUID
	var raw []byte
	err := s.db.QueryRow(ctx, `SELECT id, schema_version, agent_definition_id, agent_type, provider, model_id, created_at, document
		FROM config_snapshots WHERE id = $1`, id).Scan(&out.ID, &out.SchemaVersion, &defID, &out.AgentType, &out.Provider, &out.ModelID, &out.CreatedAt, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("configsnap: get snapshot: %w", err)
	}
	if defID != nil {
		s := defID.String()
		out.AgentDefinitionID = &s
	}
	if err := decodeJSON(raw, &out.Document); err != nil {
		return nil, fmt.Errorf("configsnap: decode snapshot %s: %w", id, err)
	}
	return &out, nil
}

// scalars extracts the denormalised columns from a document.
func scalars(doc Document) (defID *uuid.UUID, agentType, provider, modelID string) {
	agentType, _ = doc["agent_type"].(string)
	if m, ok := doc["model"].(map[string]any); ok {
		provider, _ = m["provider"].(string)
		modelID, _ = m["model_id"].(string)
	}
	if d, ok := doc["agent_definition"].(map[string]any); ok {
		if s, ok := d["id"].(string); ok {
			if id, err := uuid.Parse(s); err == nil {
				defID = &id
			}
		}
	}
	return defID, agentType, provider, modelID
}

// PruneUnreferencedSQL deletes snapshots no row references. It is the
// documented retention helper (spec: "Privacy and retention"); nothing
// schedules it. Snapshots are small next to the inference logs that point
// at them, and a pruned id can be recreated byte-identically from the same
// configuration.
const PruneUnreferencedSQL = `DELETE FROM config_snapshots s
WHERE s.created_at < $1
  AND NOT EXISTS (SELECT 1 FROM work_items w WHERE w.config_snapshot_id = s.id)
  AND NOT EXISTS (SELECT 1 FROM implementation_runs r WHERE r.config_snapshot_id = s.id)
  AND NOT EXISTS (SELECT 1 FROM implementation_verifications v WHERE v.config_snapshot_id = s.id)
  AND NOT EXISTS (SELECT 1 FROM inference_logs l WHERE l.config_snapshot_id = s.id)
  AND NOT EXISTS (SELECT 1 FROM work_item_outcomes o WHERE o.config_snapshot_id = s.id)
  AND NOT EXISTS (SELECT 1 FROM work_items w WHERE w.requested_config_snapshot_id = s.id)
  AND NOT EXISTS (SELECT 1 FROM rollouts ro WHERE ro.candidate_snapshot_id = s.id OR ro.baseline_snapshot_id = s.id)`

// PruneUnreferenced deletes snapshots older than before that nothing
// references and returns how many were removed. Not scheduled; see
// PruneUnreferencedSQL.
func (s *Store) PruneUnreferenced(ctx context.Context, before time.Time) (int64, error) {
	tag, err := s.db.Exec(ctx, PruneUnreferencedSQL, before)
	if err != nil {
		return 0, fmt.Errorf("configsnap: prune: %w", err)
	}
	s.mu.Lock()
	s.known = map[string]struct{}{}
	s.mu.Unlock()
	return tag.RowsAffected(), nil
}
