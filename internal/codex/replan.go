package codex

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Codex re-plan (codex coherence Part 1, migration 062).
//
// status-orchestrator consumes codex.indexed and schedules a sweep of the
// event's product and branch in codex_replan_pending (ScheduleSweepTx): a
// durable debounce, so a burst of versions collapses into one sweep a quiet
// window after the last of them, and a restart loses nothing. Its ticker
// claims due sweeps (ClaimDueSweep: FOR UPDATE SKIP LOCKED, a claim token and
// a lease, so replicas never sweep one row twice and a crashed sweep is
// retried when its lease expires) and removes a sweep once it is done
// (CompleteSweep; a sweep re-armed by a newer event meanwhile stays).
//
// A sweep compares each pinned work item against the codex: the versions
// after the item's pin (or after the newest version an earlier sweep already
// acted on for it, ReplanHistory.AcknowledgedVersion) up to the current one,
// judged by StalenessFor. What it did is recorded per item in
// work_item_codex_events (RecordItemEventTx): the dashboard's activity entry,
// the loop guard's count of automatic re-plans, and the starting version of
// the next sweep.

// PlanRelevantSections are the codex section keys whose change makes every
// pinned item stale, whatever modules it relies on: the onboarding answers
// that bound what the agents may do and when a human must look, and the zones
// (Part 2 writes section:zones).
var PlanRelevantSections = []string{"autonomous_scope", "review_triggers", "risk_zones", "zones"}

// Staleness is what a codex change means for one pin: the pinned modules
// that changed and the plan-relevant section keys that changed (sorted,
// distinct; nil when none).
type Staleness struct {
	Modules  []string `json:"modules"`
	Sections []string `json:"sections"`
}

// Stale reports whether the change touches the plan.
func (s Staleness) Stale() bool { return len(s.Modules) > 0 || len(s.Sections) > 0 }

// Describe is the human-readable change, as the re-plan reason carries it:
// "modules auth, billing; sections risk_zones (v12 → v14)".
func (s Staleness) Describe(from, to int) string {
	var parts []string
	if len(s.Modules) > 0 {
		parts = append(parts, "modules "+strings.Join(s.Modules, ", "))
	}
	if len(s.Sections) > 0 {
		parts = append(parts, "sections "+strings.Join(s.Sections, ", "))
	}
	return fmt.Sprintf("%s (v%d → v%d)", strings.Join(parts, "; "), from, to)
}

// StalenessFor judges changed (typed sections_changed entries; legacy bare
// values are modules) against a pin's modules (bare names).
func StalenessFor(pinnedModules, changed []string) Staleness {
	pinned := make(map[string]struct{}, len(pinnedModules))
	for _, m := range pinnedModules {
		pinned[m] = struct{}{}
	}
	var s Staleness
	for _, m := range ChangedModules(changed) {
		if _, ok := pinned[m]; ok {
			s.Modules = append(s.Modules, m)
		}
	}
	relevant := make(map[string]struct{}, len(PlanRelevantSections))
	for _, k := range PlanRelevantSections {
		relevant[k] = struct{}{}
	}
	for _, k := range ChangedSectionKeys(changed) {
		if _, ok := relevant[k]; ok {
			s.Sections = append(s.Sections, k)
		}
	}
	return s
}

// ── Debounce ────────────────────────────────────────────────────────────────

// PendingSweep is a claimed codex_replan_pending row.
type PendingSweep struct {
	ProductID       uuid.UUID
	Branch          string
	LatestVersion   int
	SectionsChanged []string
	EventCount      int
	ClaimToken      uuid.UUID
}

func seconds(d time.Duration) float64 {
	if d < 0 {
		return 0
	}
	return d.Seconds()
}

// ScheduleSweepTx records ev in tx: the product and branch get one pending
// sweep, due debounce after now (pushed back by every later event), with the
// event's sections added to the row's. A claimed sweep is re-armed (its claim
// cleared), so the sweep in flight cannot remove it.
func ScheduleSweepTx(ctx context.Context, tx pgx.Tx, ev IndexedEvent, debounce time.Duration) error {
	if err := checkScope(ev.ProductID, ev.Branch); err != nil {
		return err
	}
	sections := NormalizeChanges(ev.SectionsChanged)
	_, err := tx.Exec(ctx, `
		INSERT INTO codex_replan_pending (product_id, branch, due_at, first_event_at, last_event_at, latest_version, sections_changed, event_count, claim_token)
		VALUES ($1, $2, now() + make_interval(secs => $3), now(), now(), $4, $5, 1, NULL)
		ON CONFLICT (product_id, branch) DO UPDATE
		   SET due_at = EXCLUDED.due_at,
		       last_event_at = now(),
		       latest_version = GREATEST(codex_replan_pending.latest_version, EXCLUDED.latest_version),
		       sections_changed = ARRAY(SELECT DISTINCT s FROM unnest(codex_replan_pending.sections_changed || EXCLUDED.sections_changed) AS s ORDER BY s),
		       event_count = codex_replan_pending.event_count + 1,
		       claim_token = NULL
	`, ev.ProductID, ev.Branch, seconds(debounce), ev.ProductVersion, sections)
	if err != nil {
		return fmt.Errorf("schedule codex re-plan sweep for %s/%s: %w", ev.ProductID, ev.Branch, err)
	}
	return nil
}

// ClaimDueSweep claims the oldest due sweep (restricted to the products in
// only, when given) for lease: the row is skipped by every other claimer
// until the lease runs out. It returns nil when nothing is due.
func ClaimDueSweep(ctx context.Context, q Querier, lease time.Duration, only ...uuid.UUID) (*PendingSweep, error) {
	s := PendingSweep{ClaimToken: uuid.New()}
	if only == nil {
		only = []uuid.UUID{}
	}
	err := q.QueryRow(ctx, `
		WITH due AS (
			SELECT product_id, branch FROM codex_replan_pending
			 WHERE due_at <= now()
			   AND (cardinality($3::uuid[]) = 0 OR product_id = ANY($3::uuid[]))
			 ORDER BY due_at
			 LIMIT 1
			 FOR UPDATE SKIP LOCKED
		)
		UPDATE codex_replan_pending p
		   SET claim_token = $1, due_at = now() + make_interval(secs => $2)
		  FROM due
		 WHERE p.product_id = due.product_id AND p.branch = due.branch
		RETURNING p.product_id, p.branch, p.latest_version, p.sections_changed, p.event_count
	`, s.ClaimToken, seconds(lease), only).Scan(&s.ProductID, &s.Branch, &s.LatestVersion, &s.SectionsChanged, &s.EventCount)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claim codex re-plan sweep: %w", err)
	}
	return &s, nil
}

// CompleteSweep removes a swept row if it is still the claim s holds;
// removed is false when a newer event re-armed it meanwhile.
func CompleteSweep(ctx context.Context, q Querier, s PendingSweep) (removed bool, err error) {
	var n int
	err = q.QueryRow(ctx, `
		WITH d AS (
			DELETE FROM codex_replan_pending
			 WHERE product_id = $1 AND branch = $2 AND claim_token = $3
			RETURNING 1
		)
		SELECT count(*) FROM d
	`, s.ProductID, s.Branch, s.ClaimToken).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("complete codex re-plan sweep for %s/%s: %w", s.ProductID, s.Branch, err)
	}
	return n > 0, nil
}

// ── Item events ─────────────────────────────────────────────────────────────

// ItemEventKind is a work_item_codex_events.kind.
type ItemEventKind string

const (
	// EventReplanned: the item was sent back to planning (Automatic: by a
	// sweep; otherwise by the operator's "Re-plan" answer).
	EventReplanned ItemEventKind = "replanned"
	// EventEscalated: the automatic re-plan cap or the budget was reached;
	// the operator was asked (PromptID).
	EventEscalated ItemEventKind = "escalated"
	// EventNoted: the codex changed since planning but the item is past
	// planning (verification, review, an open PR, …) and was left alone.
	EventNoted ItemEventKind = "noted"
	// EventContinued: the operator chose "Continue as is".
	EventContinued ItemEventKind = "continued"
)

// ItemEvent is one work_item_codex_events row.
type ItemEvent struct {
	ID          uuid.UUID     `json:"id"`
	WorkItemID  uuid.UUID     `json:"work_item_id"`
	ProductID   uuid.UUID     `json:"product_id"`
	Branch      string        `json:"branch"`
	Kind        ItemEventKind `json:"kind"`
	Automatic   bool          `json:"automatic"`
	FromVersion int           `json:"from_version"`
	ToVersion   int           `json:"to_version"`
	Modules     []string      `json:"modules"`
	Sections    []string      `json:"sections"`
	ItemState   string        `json:"item_state"`
	Reason      string        `json:"reason"`
	Cause       string        `json:"cause,omitempty"`
	PromptID    *uuid.UUID    `json:"prompt_id,omitempty"`
	CreatedAt   time.Time     `json:"created_at"`
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

const itemEventColumns = `id, work_item_id, product_id, branch, kind, automatic, from_version, to_version, modules, sections, item_state, reason, cause, prompt_id, created_at`

func scanItemEvent(row pgx.Row) (ItemEvent, error) {
	var e ItemEvent
	var kind string
	err := row.Scan(&e.ID, &e.WorkItemID, &e.ProductID, &e.Branch, &kind, &e.Automatic, &e.FromVersion, &e.ToVersion,
		&e.Modules, &e.Sections, &e.ItemState, &e.Reason, &e.Cause, &e.PromptID, &e.CreatedAt)
	e.Kind = ItemEventKind(kind)
	e.Modules, e.Sections = orEmpty(e.Modules), orEmpty(e.Sections)
	return e, err
}

// RecordItemEventTx inserts ev in tx and returns it as stored (id and
// created_at filled in).
func RecordItemEventTx(ctx context.Context, tx pgx.Tx, ev ItemEvent) (ItemEvent, error) {
	switch {
	case ev.WorkItemID == uuid.Nil:
		return ItemEvent{}, errors.New("codex item event: no work item")
	case ev.ProductID == uuid.Nil:
		return ItemEvent{}, fmt.Errorf("codex item event for %s: %w", ev.WorkItemID, ErrNoProduct)
	}
	if ev.ID == uuid.Nil {
		ev.ID = uuid.New()
	}
	out, err := scanItemEvent(tx.QueryRow(ctx, `
		INSERT INTO work_item_codex_events (id, work_item_id, product_id, branch, kind, automatic, from_version, to_version, modules, sections, item_state, reason, cause, prompt_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		RETURNING `+itemEventColumns,
		ev.ID, ev.WorkItemID, ev.ProductID, ev.Branch, string(ev.Kind), ev.Automatic, ev.FromVersion, ev.ToVersion,
		orEmpty(ev.Modules), orEmpty(ev.Sections), ev.ItemState, ev.Reason, ev.Cause, ev.PromptID))
	if err != nil {
		return ItemEvent{}, fmt.Errorf("record codex %s event for %s: %w", ev.Kind, ev.WorkItemID, err)
	}
	return out, nil
}

// ListItemEvents returns the item's codex events, oldest first.
func ListItemEvents(ctx context.Context, q Querier, workItemID uuid.UUID) ([]ItemEvent, error) {
	rows, err := q.Query(ctx, `SELECT `+itemEventColumns+` FROM work_item_codex_events WHERE work_item_id = $1 ORDER BY created_at, id`, workItemID)
	if err != nil {
		return nil, fmt.Errorf("list codex events for %s: %w", workItemID, err)
	}
	defer rows.Close()
	out := []ItemEvent{}
	for rows.Next() {
		e, err := scanItemEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("scan codex event: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ItemEventByPrompt returns the escalation event that raised promptID.
func ItemEventByPrompt(ctx context.Context, q Querier, promptID uuid.UUID) (ev ItemEvent, found bool, err error) {
	ev, err = scanItemEvent(q.QueryRow(ctx, `SELECT `+itemEventColumns+` FROM work_item_codex_events WHERE prompt_id = $1 AND kind = 'escalated' ORDER BY created_at DESC LIMIT 1`, promptID))
	if errors.Is(err, pgx.ErrNoRows) {
		return ItemEvent{}, false, nil
	}
	if err != nil {
		return ItemEvent{}, false, fmt.Errorf("codex event for prompt %s: %w", promptID, err)
	}
	return ev, true, nil
}

// ReplanHistory is what the sweep needs to know of an item's past events.
type ReplanHistory struct {
	// AutomaticReplans counts the sweep's own re-plans (the loop guard).
	AutomaticReplans int
	// AcknowledgedVersion is the newest to_version of any event: the
	// versions up to it were already acted on (re-planned, noted, escalated
	// or continued) and are not judged again. 0 when there is none.
	AcknowledgedVersion int
	// LastReplanAt is when the item was last re-planned (zero: never).
	LastReplanAt time.Time
}

// LoadReplanHistory reads the item's history.
func LoadReplanHistory(ctx context.Context, q Querier, workItemID uuid.UUID) (ReplanHistory, error) {
	var h ReplanHistory
	var last *time.Time
	err := q.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE kind = 'replanned' AND automatic),
		       COALESCE(MAX(to_version), 0),
		       MAX(created_at) FILTER (WHERE kind = 'replanned')
		  FROM work_item_codex_events
		 WHERE work_item_id = $1
	`, workItemID).Scan(&h.AutomaticReplans, &h.AcknowledgedVersion, &last)
	if err != nil {
		return ReplanHistory{}, fmt.Errorf("codex re-plan history of %s: %w", workItemID, err)
	}
	if last != nil {
		h.LastReplanAt = *last
	}
	return h, nil
}

// DBNow is the database clock (clock_timestamp()), the clock
// work_item_codex_events.created_at is written with.
func DBNow(ctx context.Context, q Querier) (time.Time, error) {
	var now time.Time
	if err := q.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return time.Time{}, fmt.Errorf("read database clock: %w", err)
	}
	return now, nil
}

// LatestReplanTx returns the item's newest replanned event's id and time;
// found is false when it was never re-planned.
func LatestReplanTx(ctx context.Context, q Querier, workItemID uuid.UUID) (id uuid.UUID, at time.Time, found bool, err error) {
	err = q.QueryRow(ctx, `
		SELECT id, created_at FROM work_item_codex_events
		 WHERE work_item_id = $1 AND kind = 'replanned'
		 ORDER BY created_at DESC, id DESC LIMIT 1`, workItemID).Scan(&id, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, time.Time{}, false, nil
	}
	if err != nil {
		return uuid.Nil, time.Time{}, false, fmt.Errorf("latest codex re-plan of %s: %w", workItemID, err)
	}
	return id, at, true, nil
}

// ReplanBriefAllowedTx is the planner's commit rule for a plan built from a
// technical-brief.ready, asked in the plan's transaction after it locked the
// parent row (the row a re-plan's transaction locks for its transition):
//
//   - a re-plan brief (replanEvent, the replanned event that emitted it)
//     commits only when that event is the parent's latest re-plan, so an
//     earlier re-plan's brief (or a duplicate) never adds a second plan;
//   - an ordinary brief (replanEvent uuid.Nil) commits only when no re-plan
//     happened after briefAt, when the brief was produced (on the database
//     clock); a redelivered brief from before a re-plan does not commit. A
//     zero briefAt (unknown age) is allowed.
//
// reason says why a plan is refused.
func ReplanBriefAllowedTx(ctx context.Context, q Querier, workItemID, replanEvent uuid.UUID, briefAt time.Time) (ok bool, reason string, err error) {
	latest, at, found, err := LatestReplanTx(ctx, q, workItemID)
	if err != nil {
		return false, "", err
	}
	if replanEvent != uuid.Nil {
		if !found || latest != replanEvent {
			return false, fmt.Sprintf("the brief is from codex re-plan %s, but the parent's latest re-plan is %s", replanEvent, latest), nil
		}
		return true, "", nil
	}
	if found && !briefAt.IsZero() && at.After(briefAt) {
		return false, fmt.Sprintf("the brief was produced at %s, before the parent's codex re-plan %s at %s", briefAt.Format(time.RFC3339Nano), latest, at.Format(time.RFC3339Nano)), nil
	}
	return true, "", nil
}
