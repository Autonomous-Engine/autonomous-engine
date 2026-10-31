package rollout

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// outcomeAggregates is the aggregate list over work_item_outcomes o. A
// review_defects value that is not a number counts as 0 (never an error).
const outcomeAggregates = `count(*),
	count(*) FILTER (WHERE o.release_decision = 'release'),
	count(*) FILTER (WHERE o.false_success),
	count(*) FILTER (WHERE o.review_defects IS NOT NULL),
	COALESCE(sum(CASE WHEN jsonb_typeof(o.review_defects->'critical') = 'number'
		THEN (o.review_defects->>'critical')::numeric ELSE 0 END), 0)::bigint,
	count(*) FILTER (WHERE jsonb_typeof(o.review_defects->'major') = 'number'
		AND (o.review_defects->>'major')::numeric > 0)`

// ProductionOutcomesSQL aggregates the production outcomes of one snapshot.
// Shadow items (benchmark intakes that asked for a snapshot, replay items,
// items run for a rollout) are excluded: they are evidence about a
// candidate, not production.
//
// The match is exact snapshot-id equality, not Equivalent. Since migration
// 060 a snapshot's codex_version is always 0 (configsnap), so items of one
// configuration share its id whatever codex version they ran against.
const ProductionOutcomesSQL = `SELECT ` + outcomeAggregates + `
FROM work_item_outcomes o
JOIN work_items w ON w.id = o.work_item_id
WHERE o.config_snapshot_id = $1
  AND NOT w.replay AND w.shadow_rollout_id IS NULL AND w.requested_config_snapshot_id IS NULL`

// ReplayOutcomesSQL aggregates the outcomes of a rollout's replay items.
const ReplayOutcomesSQL = `SELECT ` + outcomeAggregates + `
FROM work_item_outcomes o
JOIN work_items w ON w.id = o.work_item_id
WHERE w.shadow_rollout_id = $1 AND w.replay`

// ShadowOutcomesSQL aggregates the outcomes of a rollout's benchmark shadow
// items that asked for one side's snapshot (AE_CONFIG_SNAPSHOT_ID): the
// defect gates' data path for both sides (review runs on them; their
// structured defects arrive with M3-07).
const ShadowOutcomesSQL = `SELECT ` + outcomeAggregates + `
FROM work_item_outcomes o
JOIN work_items w ON w.id = o.work_item_id
WHERE w.shadow_rollout_id = $1 AND NOT w.replay AND w.requested_config_snapshot_id = $2`

func scanOutcomes(row interface{ Scan(...any) error }, out *ProdOutcomes) (int, error) {
	var items, released, falseSuccess, defectItems, critical, major int64
	if err := row.Scan(&items, &released, &falseSuccess, &defectItems, &critical, &major); err != nil {
		return 0, err
	}
	out.Items += int(items)
	out.Released += int(released)
	out.FalseSuccess += int(falseSuccess)
	out.DefectDataItems += int(defectItems)
	out.CriticalDefects += int(critical)
	out.ItemsWithMajorDefect += int(major)
	return int(items), nil
}

// ProductionOutcomes returns the production outcomes of snapshotID.
func ProductionOutcomes(ctx context.Context, q DB, snapshotID string) (ProdOutcomes, error) {
	out := ProdOutcomes{SnapshotID: snapshotID, Source: "production"}
	if snapshotID == "" {
		return out, nil
	}
	if _, err := scanOutcomes(q.QueryRow(ctx, ProductionOutcomesSQL, snapshotID), &out); err != nil {
		return out, fmt.Errorf("rollout: production outcomes: %w", err)
	}
	return out, nil
}

// sideOutcomes adds the rollout's shadow items for snapshotID (and, for the
// candidate, its replay items) to its production outcomes. Source names
// every part that contributed rows.
func sideOutcomes(ctx context.Context, q DB, snapshotID string, rolloutID uuid.UUID, withReplay bool) (ProdOutcomes, error) {
	out := ProdOutcomes{SnapshotID: snapshotID}
	var parts []string
	add := func(name, sql string, args ...any) error {
		n, err := scanOutcomes(q.QueryRow(ctx, sql, args...), &out)
		if err != nil {
			return fmt.Errorf("rollout: %s outcomes: %w", name, err)
		}
		if n > 0 {
			parts = append(parts, name)
		}
		return nil
	}
	if snapshotID != "" {
		if err := add("production", ProductionOutcomesSQL, snapshotID); err != nil {
			return out, err
		}
		if err := add("shadow", ShadowOutcomesSQL, rolloutID, snapshotID); err != nil {
			return out, err
		}
	}
	if withReplay {
		if err := add("replay", ReplayOutcomesSQL, rolloutID); err != nil {
			return out, err
		}
	}
	out.Source = strings.Join(parts, "+")
	if out.Source == "" {
		out.Source = "none"
	}
	return out, nil
}

// CandidateOutcomes returns the candidate's production outcomes (items that
// ran under it outside a shadow run, normally none), its benchmark shadow
// items of this rollout and the rollout's replay items.
func CandidateOutcomes(ctx context.Context, q DB, snapshotID string, rolloutID uuid.UUID) (ProdOutcomes, error) {
	return sideOutcomes(ctx, q, snapshotID, rolloutID, true)
}

// BaselineOutcomes returns the baseline's production outcomes plus its
// benchmark shadow items of this rollout.
func BaselineOutcomes(ctx context.Context, q DB, snapshotID string, rolloutID uuid.UUID) (ProdOutcomes, error) {
	return sideOutcomes(ctx, q, snapshotID, rolloutID, false)
}
