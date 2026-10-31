// Package codexgap records mid-run empty codex lookups (codex coherence
// Part 3) and says when a run was ungrounded.
//
// Producer: the tool executor's maybeEmitGap (internal/llm/tools) publishes
// codex.gap when an agent falls back to a filesystem tool after a codex_*
// tool returned nothing; each such event is one empty codex lookup. The
// planner-scope events on the same topic (reason "planner_missed_file") are
// not lookups and are ignored.
//
// Consumer: status-orchestrator (its own consumer group) calls RecordTx for
// every event. A lookup is counted against one run of its work item:
//
//	run = (work item, stage, run_started_at)
//
// A run starts with a transition into research or implementation that is
// not a prompt restore (awaiting_user → the state the item was parked in),
// or with the item's creation in one of those states, and lasts until the
// item moves to any state other than that stage or awaiting_user. A lookup
// counts for the run whose interval contains the event's own timestamp
// (the payload's "timestamp"; the consume time when it has none), so a
// lagging event counts for the run it happened in, a prompt round-trip
// mid-run does not split the run, and a lookup made outside any run (the
// verifier's, during planning) counts for none. A re-run (the item
// re-enters research or implementation, e.g. after a block) is a new run
// with its own count. Timestamps come from the agents' clocks and the
// transitions from the database's; both run on the same hosts.
// The agents key their events on the parent work item, so the subtasks of
// one implementation round share the parent's run. A run with more than the
// threshold (CODEX_GAP_THRESHOLD, default 5) lookups is marked ungrounded.
//
// Reader: the verification stage asks Ungrounded for the parent and the
// subtask; only each stage's latest run counts, so a clean re-run clears an
// earlier ungrounded one.
package codexgap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ThresholdEnv names the number of empty codex lookups one run may record
// before it is ungrounded (more than this many marks it).
const ThresholdEnv = "CODEX_GAP_THRESHOLD"

// DefaultThreshold is ThresholdEnv's default.
const DefaultThreshold = 5

// ThresholdFromEnv reads ThresholdEnv; an unset, malformed or negative
// value gives DefaultThreshold.
func ThresholdFromEnv() int {
	v := strings.TrimSpace(os.Getenv(ThresholdEnv))
	if v == "" {
		return DefaultThreshold
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return DefaultThreshold
	}
	return n
}

// Event is the codex.gap payload (internal/llm/tools maybeEmitGap and
// maybeEmitPlannerGap).
type Event struct {
	GapID        string `json:"gap_id,omitempty"`
	WorkItemID   string `json:"work_item_id"`
	ProductID    string `json:"product_id,omitempty"`
	CodexTool    string `json:"codex_tool,omitempty"`
	Query        string `json:"query,omitempty"`
	FallbackTool string `json:"fallback_tool,omitempty"`
	FoundVia     string `json:"found_via,omitempty"`
	Reason       string `json:"reason,omitempty"`
	Timestamp    string `json:"timestamp,omitempty"`
}

// EmptyLookup reports whether the event is an empty codex lookup (not a
// planner-scope gap).
func (e Event) EmptyLookup() bool { return e.CodexTool != "" && e.Reason == "" }

// ErrMalformed wraps a codex.gap payload RecordTx cannot read; a consumer
// drops such an event rather than retry it.
var ErrMalformed = errors.New("malformed codex.gap payload")

// Result is what RecordTx did with one event.
type Result struct {
	// Recorded: the event was counted (false for a duplicate, a non-lookup
	// event, or an item outside a research or implementation run).
	Recorded bool
	Stage    models.WorkItemState
	Count    int
	// Ungrounded: the run is (now or already) ungrounded; Newly: this
	// event crossed the threshold.
	Ungrounded bool
	Newly      bool
}

// countedStages are the runs a lookup is counted against.
var countedStages = map[models.WorkItemState]bool{
	models.WorkItemStateResearch:       true,
	models.WorkItemStateImplementation: true,
}

// entriesCTE lists the item's run starts: transitions into research or
// implementation that are not prompt restores (awaiting_user → the same
// state the item was parked in). An item that never transitioned into a
// counted state but was created in one has its creation as the start.
const entriesCTE = `
	entries AS (
		SELECT t.work_item_id, t.to_state::text AS stage, t.timestamp AS started
		  FROM work_item_transitions t
		 WHERE t.to_state::text IN ('research', 'implementation')
		   AND (t.from_state IS NULL OR t.from_state::text <> 'awaiting_user')
		UNION ALL
		SELECT w.id, w.state::text, w.created_at
		  FROM work_items w
		 WHERE w.state::text IN ('research', 'implementation')
		   AND NOT EXISTS (
		       SELECT 1 FROM work_item_transitions t
		        WHERE t.work_item_id = w.id AND t.to_state::text IN ('research', 'implementation'))
	)`

// runAtSQL is the run that contains time $2 for item $1: the latest run
// start at or before $2, provided the item had not left that stage (to a
// state other than the stage itself or awaiting_user) between the start
// and $2. No row: $2 falls in no research or implementation run.
const runAtSQL = `
	WITH ` + entriesCTE + `
	SELECT e.stage, e.started
	  FROM entries e
	 WHERE e.work_item_id = $1 AND e.started <= $2
	   AND NOT EXISTS (
	       SELECT 1 FROM work_item_transitions x
	        WHERE x.work_item_id = $1 AND x.timestamp > e.started AND x.timestamp <= $2
	          AND x.to_state::text NOT IN (e.stage, 'awaiting_user'))
	 ORDER BY e.started DESC
	 LIMIT 1`

// RecordTx counts one codex.gap event (raw is its payload) in the caller's
// transaction and marks the run ungrounded when its count exceeds
// threshold. Events are deduplicated on gap_id (a hash of the payload when
// the producer sent none), so a redelivery is not counted twice.
func RecordTx(ctx context.Context, tx pgx.Tx, raw []byte, threshold int) (Result, error) {
	var ev Event
	if err := json.Unmarshal(raw, &ev); err != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	if !ev.EmptyLookup() {
		return Result{}, nil
	}
	workID, err := uuid.Parse(ev.WorkItemID)
	if err != nil {
		return Result{}, fmt.Errorf("%w: work_item_id %q: %v", ErrMalformed, ev.WorkItemID, err)
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM work_items WHERE id = $1)`, workID).Scan(&exists); err != nil {
		return Result{}, fmt.Errorf("read work item %s: %w", workID, err)
	}
	if !exists {
		return Result{}, nil
	}
	// The run is the one the lookup happened in (the event's own time),
	// not the item's state when the event is consumed: a lagging event
	// counts for its run, and one from outside any run counts for none.
	at := time.Now().UTC()
	if ts, perr := time.Parse(time.RFC3339Nano, ev.Timestamp); perr == nil {
		at = ts
	}
	var state string
	var started time.Time
	if err := tx.QueryRow(ctx, runAtSQL, workID, at).Scan(&state, &started); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Result{}, nil
		}
		return Result{}, fmt.Errorf("find the run of %s at %s: %w", workID, at.Format(time.RFC3339Nano), err)
	}
	stage := models.WorkItemState(state)
	if !countedStages[stage] {
		return Result{}, nil
	}
	gapID := ev.GapID
	if gapID == "" {
		sum := sha256.Sum256(raw)
		gapID = "sha256:" + hex.EncodeToString(sum[:])
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO codex_gap_events (gap_id, work_item_id, stage, run_started_at, codex_tool, query, fallback_tool, found_via, event_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (gap_id) DO NOTHING`,
		gapID, workID, state, started, ev.CodexTool, ev.Query, ev.FallbackTool, ev.FoundVia, at)
	if err != nil {
		return Result{}, fmt.Errorf("record codex gap: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return Result{}, nil
	}
	res := Result{Recorded: true, Stage: stage}
	var ungroundedAt *time.Time
	if err := tx.QueryRow(ctx, `
		INSERT INTO codex_gap_runs (work_item_id, stage, run_started_at, empty_lookups)
		VALUES ($1, $2, $3, 1)
		ON CONFLICT (work_item_id, stage, run_started_at) DO UPDATE
			SET empty_lookups = codex_gap_runs.empty_lookups + 1, updated_at = now()
		RETURNING empty_lookups, ungrounded_at`, workID, state, started).Scan(&res.Count, &ungroundedAt); err != nil {
		return Result{}, fmt.Errorf("count codex gap: %w", err)
	}
	res.Ungrounded = ungroundedAt != nil
	if !res.Ungrounded && res.Count > threshold {
		if _, err := tx.Exec(ctx, `UPDATE codex_gap_runs SET ungrounded_at = now() WHERE work_item_id = $1 AND stage = $2 AND run_started_at = $3`,
			workID, state, started); err != nil {
			return Result{}, fmt.Errorf("mark run ungrounded: %w", err)
		}
		res.Ungrounded, res.Newly = true, true
	}
	return res, nil
}

// Querier is the read surface Ungrounded needs.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Ungrounded reports whether the latest research or implementation run of
// any of ids is ungrounded, and describes the runs that are.
func Ungrounded(ctx context.Context, q Querier, ids ...uuid.UUID) (bool, string, error) {
	rows, err := q.Query(ctx, `
		WITH `+entriesCTE+`
		SELECT r.work_item_id, r.stage, r.empty_lookups
		  FROM codex_gap_runs r
		 WHERE r.work_item_id = ANY($1)
		   AND r.ungrounded_at IS NOT NULL
		   AND r.run_started_at = (
		         SELECT MAX(e.started) FROM entries e WHERE e.work_item_id = r.work_item_id AND e.stage = r.stage)
		 ORDER BY r.stage, r.work_item_id`, ids)
	if err != nil {
		return false, "", fmt.Errorf("read ungrounded runs: %w", err)
	}
	defer rows.Close()
	var parts []string
	for rows.Next() {
		var id uuid.UUID
		var stage string
		var n int
		if err := rows.Scan(&id, &stage, &n); err != nil {
			return false, "", fmt.Errorf("scan ungrounded run: %w", err)
		}
		parts = append(parts, fmt.Sprintf("the %s run of %s recorded %d empty codex lookups", stage, id, n))
	}
	if err := rows.Err(); err != nil {
		return false, "", err
	}
	return len(parts) > 0, strings.Join(parts, "; "), nil
}
