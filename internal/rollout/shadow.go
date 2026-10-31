package rollout

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/autonomousengine/autonomous-engine/internal/configsnap"
)

// Intake metadata keys of a shadow intake.
const (
	MetaConfigSnapshotID = "config_snapshot_id"
	MetaReplay           = "replay"
	MetaReplayOf         = "replay_of"
	MetaRolloutID        = "rollout_id"
)

// SourceBenchmark is the intake source of the benchmark wrapper (AE_SOURCE
// default). It is a client-supplied string and grants nothing by itself.
const SourceBenchmark = "benchmark"

// ShadowIntake is what a gateway intake asked for beyond normal routing.
type ShadowIntake struct {
	// SnapshotID is the requested configuration snapshot ("" = route
	// normally). AgentDefinitionID is that snapshot's definition (nil
	// when the snapshot was captured without one).
	SnapshotID        string
	AgentDefinitionID *uuid.UUID
	Replay            bool
	ReplayOf          *uuid.UUID
	RolloutID         *uuid.UUID
}

// Active reports whether the intake needs shadow handling.
func (s *ShadowIntake) Active() bool {
	return s != nil && (s.SnapshotID != "" || s.Replay || s.RolloutID != nil)
}

// IntakeError is a refused shadow intake with its HTTP status.
type IntakeError struct {
	Status  int
	Code    string
	Message string
}

func (e *IntakeError) Error() string { return e.Message }

func refuse(status int, code, format string, args ...any) error {
	return &IntakeError{Status: status, Code: code, Message: fmt.Sprintf(format, args...)}
}

// shadowRolloutStatuses are the statuses in which a rollout may still run
// shadow work.
var shadowRolloutStatuses = map[string]bool{StatusProposed: true, StatusShadow: true, StatusEvaluating: true}

// ResolveShadowIntake validates the shadow fields of an intake. The gateway
// is unauthenticated and `source` is a client string, so nothing is
// honoured on its say-so:
//
//   - config_snapshot_id is honoured only when rollout_id names a rollout
//     in proposed|shadow|evaluating, the snapshot is that rollout's
//     candidate or baseline, and the target product is a benchmark scratch
//     product (products.benchmark_scratch). Any other request carrying a
//     snapshot id is refused (400). The item is routed to the snapshot's
//     agent definition instead of tag/complexity routing.
//   - replay=true needs replay_of, an existing top-level item whose product
//     opted in (products.allow_replay, else 403), and a benchmark scratch
//     target that is not the source product (400): implementation-agent
//     pushes branches to the target's origin.
//   - rollout_id alone must name a rollout.
//
// It returns nil for an ordinary intake.
func ResolveShadowIntake(ctx context.Context, q DB, source string, metadata map[string]any, productID uuid.UUID) (*ShadowIntake, error) {
	_ = source // deliberately not trusted (see above)
	snapshotID := strings.TrimSpace(metaString(metadata, MetaConfigSnapshotID))
	replay := metaBool(metadata, MetaReplay)
	replayOfRaw := strings.TrimSpace(metaString(metadata, MetaReplayOf))
	rolloutRaw := strings.TrimSpace(metaString(metadata, MetaRolloutID))
	if snapshotID == "" && !replay && rolloutRaw == "" {
		return nil, nil
	}
	s := &ShadowIntake{Replay: replay}

	var cand, base *string
	rolloutStatus := ""
	if rolloutRaw != "" {
		id, err := uuid.Parse(rolloutRaw)
		if err != nil {
			return nil, refuse(http.StatusBadRequest, "invalid_rollout", "invalid rollout_id %q", rolloutRaw)
		}
		err = q.QueryRow(ctx, `SELECT status, candidate_snapshot_id, baseline_snapshot_id FROM rollouts WHERE id = $1`, id).Scan(&rolloutStatus, &cand, &base)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, refuse(http.StatusBadRequest, "unknown_rollout", "rollout %s does not exist", id)
		}
		if err != nil {
			return nil, fmt.Errorf("rollout: look up rollout: %w", err)
		}
		s.RolloutID = &id
	}

	scratch, err := isScratch(ctx, q, productID)
	if err != nil {
		return nil, err
	}

	// A bare rollout_id (no snapshot, no replay) still marks the item as
	// shadow, which removes it from production outcomes (and so from the
	// false-success baseline): it gets the same conditions as a snapshot.
	if s.RolloutID != nil && snapshotID == "" && !replay {
		switch {
		case !shadowRolloutStatuses[rolloutStatus]:
			return nil, refuse(http.StatusBadRequest, "rollout_not_in_shadow", "rollout %s is %s; shadow runs need a proposed, shadow or evaluating rollout", s.RolloutID, rolloutStatus)
		case !scratch:
			return nil, refuse(http.StatusBadRequest, "rollout_needs_scratch_product", "rollout_id is honoured only on a benchmark scratch product (products.benchmark_scratch); product %s is not one", productID)
		}
	}

	if replay {
		if replayOfRaw == "" {
			return nil, refuse(http.StatusBadRequest, "replay_of_required", "a replay intake needs replay_of (the production work item it re-submits)")
		}
		id, err := uuid.Parse(replayOfRaw)
		if err != nil {
			return nil, refuse(http.StatusBadRequest, "invalid_replay_of", "invalid replay_of %q", replayOfRaw)
		}
		var sourceProduct *uuid.UUID
		var parent *uuid.UUID
		var allow *bool
		err = q.QueryRow(ctx, `SELECT w.product_id, w.parent_id, p.allow_replay
			FROM work_items w LEFT JOIN products p ON p.id = w.product_id WHERE w.id = $1`, id).Scan(&sourceProduct, &parent, &allow)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, refuse(http.StatusBadRequest, "unknown_replay_of", "replay_of %s does not exist", id)
		}
		if err != nil {
			return nil, fmt.Errorf("rollout: look up replay_of: %w", err)
		}
		if parent != nil {
			return nil, refuse(http.StatusBadRequest, "replay_of_subtask", "replay_of %s is a subtask; replay re-submits top-level work items", id)
		}
		if sourceProduct == nil || allow == nil || !*allow {
			return nil, refuse(http.StatusForbidden, "replay_not_allowed", "the product of work item %s has not opted in to replay (products.allow_replay)", id)
		}
		if *sourceProduct == productID {
			return nil, refuse(http.StatusBadRequest, "replay_needs_scratch_product", "a replay must target a benchmark scratch product, not the product of the work item it re-submits")
		}
		if !scratch {
			return nil, refuse(http.StatusBadRequest, "replay_needs_scratch_product", "a replay must target a benchmark scratch product (products.benchmark_scratch); product %s is not one", productID)
		}
		s.ReplayOf = &id
	}

	if snapshotID != "" {
		switch {
		case !configsnap.ValidID(snapshotID):
			return nil, refuse(http.StatusBadRequest, "invalid_config_snapshot", "invalid config_snapshot_id %q", snapshotID)
		case s.RolloutID == nil:
			return nil, refuse(http.StatusBadRequest, "snapshot_needs_rollout", "config_snapshot_id is honoured only for a rollout's shadow run: rollout_id is required")
		case !shadowRolloutStatuses[rolloutStatus]:
			return nil, refuse(http.StatusBadRequest, "rollout_not_in_shadow", "rollout %s is %s; shadow runs need a proposed, shadow or evaluating rollout", s.RolloutID, rolloutStatus)
		case !(cand != nil && *cand == snapshotID) && !(base != nil && *base == snapshotID):
			return nil, refuse(http.StatusBadRequest, "snapshot_not_in_rollout", "config snapshot %s is neither the candidate nor the baseline of rollout %s", snapshotID, s.RolloutID)
		case !scratch:
			return nil, refuse(http.StatusBadRequest, "snapshot_needs_scratch_product", "config_snapshot_id is honoured only on a benchmark scratch product (products.benchmark_scratch); product %s is not one", productID)
		}
		var defID *uuid.UUID
		err := q.QueryRow(ctx, `SELECT agent_definition_id FROM config_snapshots WHERE id = $1`, snapshotID).Scan(&defID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, refuse(http.StatusBadRequest, "unknown_config_snapshot", "config snapshot %s does not exist", snapshotID)
		}
		if err != nil {
			return nil, fmt.Errorf("rollout: look up config snapshot: %w", err)
		}
		if defID != nil {
			var exists bool
			if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agent_definitions WHERE id = $1)`, *defID).Scan(&exists); err != nil {
				return nil, fmt.Errorf("rollout: look up agent definition: %w", err)
			}
			if !exists {
				return nil, refuse(http.StatusBadRequest, "config_snapshot_definition_gone", "the agent definition of config snapshot %s no longer exists", snapshotID)
			}
		}
		s.SnapshotID = snapshotID
		s.AgentDefinitionID = defID
	}
	return s, nil
}

func isScratch(ctx context.Context, q DB, productID uuid.UUID) (bool, error) {
	var scratch bool
	err := q.QueryRow(ctx, `SELECT benchmark_scratch FROM products WHERE id = $1`, productID).Scan(&scratch)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("rollout: look up product: %w", err)
	}
	return scratch, nil
}

// ApplyTx records the shadow fields on the new work item, inside the intake
// transaction.
func (s *ShadowIntake) ApplyTx(ctx context.Context, tx DB, workID uuid.UUID) error {
	if !s.Active() {
		return nil
	}
	var snapshot any
	if s.SnapshotID != "" {
		snapshot = s.SnapshotID
	}
	if _, err := tx.Exec(ctx, `UPDATE work_items SET replay = $2, replay_of = $3, shadow_rollout_id = $4,
		requested_config_snapshot_id = $5 WHERE id = $1`, workID, s.Replay, s.ReplayOf, s.RolloutID, snapshot); err != nil {
		return fmt.Errorf("record shadow intake: %w", err)
	}
	return nil
}

// ShadowTerminalSQL decides whether a work item gets replay semantics: it
// is a replay item, or it ran a requested snapshot on a product that is not
// a benchmark scratch product and no approved rollout covers that snapshot
// (belt and braces: the gateway refuses such intakes). Such an item never
// gets a PR; it is reviewed on its branch and then cancelled.
const ShadowTerminalSQL = `SELECT w.replay OR (
		w.requested_config_snapshot_id IS NOT NULL
		AND NOT COALESCE(p.benchmark_scratch, false)
		AND NOT EXISTS (SELECT 1 FROM rollouts r WHERE r.candidate_snapshot_id = w.requested_config_snapshot_id AND r.status = 'approved'))
	FROM work_items w LEFT JOIN products p ON p.id = w.product_id WHERE w.id = $1`

// IsShadowTerminal reports whether workID gets replay semantics (see
// ShadowTerminalSQL). A missing item is not.
func IsShadowTerminal(ctx context.Context, q interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}, workID uuid.UUID) (bool, error) {
	var terminal bool
	err := q.QueryRow(ctx, ShadowTerminalSQL, workID).Scan(&terminal)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("rollout: replay flag: %w", err)
	}
	return terminal, nil
}

func metaString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	switch v := m[key].(type) {
	case string:
		return v
	case fmt.Stringer:
		return v.String()
	default:
		return ""
	}
}

func metaBool(m map[string]any, key string) bool {
	if m == nil {
		return false
	}
	switch v := m[key].(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(strings.TrimSpace(v), "true")
	default:
		return false
	}
}
