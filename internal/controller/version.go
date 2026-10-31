package controller

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// Version identifies the controller's derivation rules (Derive, the test
// file / test command classifiers). It is recorded in every configuration
// snapshot (internal/configsnap, M4-05), so a change to how the controller
// reads tool evidence is a configuration change like a prompt or model
// change. Bump it whenever a change to this package can move a run to a
// different state for the same tool events.
const Version = "m3-02.1"

// SetConfigSnapshot records the configuration snapshot the run's model
// calls are made under (implementation_runs.config_snapshot_id, migration
// 053). An empty id is a no-op: the run keeps NULL rather than a value that
// names no snapshot.
func (s *Store) SetConfigSnapshot(ctx context.Context, runID uuid.UUID, snapshotID string) error {
	if s == nil || snapshotID == "" || runID == uuid.Nil {
		return nil
	}
	if _, err := s.db.Exec(ctx, `UPDATE implementation_runs SET config_snapshot_id = $2 WHERE id = $1`, runID, snapshotID); err != nil {
		return fmt.Errorf("set run config snapshot: %w", err)
	}
	return nil
}
