package guards

// Codex coherence Part 3: no planning or implementation on an empty codex.
//
// CheckCodexGrounded is the one grounding gate of product-agent,
// research-coordinator and implementation-planner. A product and branch are
// grounded when the codex has at least one module for them, after the
// branch fallback of the context builder (codex.GroundingBranch: the
// product's resolved default branch, never the literal "main"). Patterns or
// anti-patterns alone do not ground a product.
//
// A stage that refuses calls RefuseUngroundedTx: in the stage's own
// transaction the item is blocked, work.blocked is enqueued with kind
// codex_ungrounded (status-orchestrator raises the Re-index / Retry /
// Cancel escalation from it), and the refusal is recorded in
// codex_grounding_refusals (migration 061) with the message the stage was
// handling. That row is the durable re-run: Re-index (dashboard-backend)
// marks it, status-orchestrator replays it (ReplayRefusalTx) when
// codex.indexed reports the product's codex grounded, and Retry replays it
// at once. No refusal leaves an item blocked without an escalation.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/codex"
	"github.com/autonomousengine/autonomous-engine/internal/interactions"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/outbox"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MissingCodexGrounding is the ValidationResult.Missing entry of an
// ungrounded product and branch.
const MissingCodexGrounding = "codex_grounding"

// BlockKindCodexUngrounded is WorkBlockedPayload.Kind for a refusal on an
// ungrounded codex: status-orchestrator's escalation then offers Re-index.
const BlockKindCodexUngrounded = "codex_ungrounded"

// Escalation actions of a codex refusal (the prompt's action ids).
const (
	ActionReindex = "reindex"
	ActionRetry   = "retry"
	ActionCancel  = "cancel"
)

// CheckCodexGrounded reports whether productID's codex grounds branch (""
// means the product's default branch): at least one module on the branch
// codex.GroundingBranch resolves to. A product without a configured default
// branch, or a failed lookup, is not grounded and says why.
func CheckCodexGrounded(ctx context.Context, db codex.Querier, productID uuid.UUID, branch string) ValidationResult {
	effective, modules, err := codex.GroundingBranch(ctx, db, productID, branch)
	if err != nil {
		return ValidationResult{
			Valid:   false,
			Missing: []string{MissingCodexGrounding},
			Message: fmt.Sprintf("Cannot check the codex of product %s: %v. Fix the product's repositories (default branch), then re-index the product.", productID, err),
		}
	}
	if modules > 0 {
		return ValidationResult{Valid: true}
	}
	return ValidationResult{
		Valid:   false,
		Missing: []string{MissingCodexGrounding},
		Message: ungroundedMessage(productID, branch, effective),
	}
}

func ungroundedMessage(productID uuid.UUID, requested, effective string) string {
	where := fmt.Sprintf("branch %s", effective)
	if requested != "" && requested != effective {
		where = fmt.Sprintf("branch %s (nor on the default branch %s)", requested, effective)
	}
	return fmt.Sprintf("The codex of product %s has no module on %s, so no stage can ground this work item: "+
		"patterns and anti-patterns alone are not enough. Re-index the product (the item re-runs once the index has modules), "+
		"or onboard it in Admin → Products.", productID, where)
}

// ContextErrorReason is the refusal reason when a stage could not build the
// codex context (codex.BuildContextForIntentBranch failed): the stage stops
// instead of running without the codex.
func ContextErrorReason(productID uuid.UUID, err error) string {
	return fmt.Sprintf("Could not build the codex context for product %s: %v. The stage stopped rather than run without the codex. "+
		"Fix the cause, then Retry (or Re-index the product).", productID, err)
}

// Refusal is one stage's refusal of a work item on an ungrounded codex.
type Refusal struct {
	ProductID uuid.UUID
	// Branch is the branch the stage checked ("" = the default branch).
	Branch string
	// Stage is the refusing service (product-agent, research-coordinator,
	// implementation-planner).
	Stage  string
	Reason string
	// RerunState is the state the item re-enters on a re-run (intake for
	// product-agent, research for research-coordinator and the planner);
	// blocked must have an edge to it.
	RerunState models.WorkItemState
	// RerunTopic, RerunKey and RerunPayload are the message the stage was
	// handling, replayed through the outbox on a re-run (its event_id is
	// dropped, so the replay is a new event for the stage's ledger).
	// RerunAggregateID orders the replay (default: the work item).
	RerunTopic       string
	RerunKey         string
	RerunPayload     []byte
	RerunAggregateID uuid.UUID
}

// StoredRefusal is a codex_grounding_refusals row.
type StoredRefusal struct {
	WorkItemID         uuid.UUID
	ProductID          uuid.UUID
	Branch             string
	IndexBranch        string
	Stage              string
	Reason             string
	RerunState         string
	RerunTopic         string
	RerunKey           string
	RerunAggregateID   uuid.UUID
	RerunPayload       json.RawMessage
	ReindexRequestedAt *time.Time
	ReindexRequestedBy string
	CreatedAt          time.Time
	// BlockTransitionID is the transition of the block this refusal created
	// (migration 061); nil skips the latest-block check.
	BlockTransitionID *uuid.UUID
	// IndexPostedAt is when knowledge-indexer accepted the Re-index
	// request; nil while it is still to be posted (the indexer was busy).
	IndexPostedAt *time.Time
}

const upsertRefusalSQL = `
	INSERT INTO codex_grounding_refusals
		(work_item_id, product_id, branch, index_branch, stage, reason, rerun_state, rerun_topic, rerun_key, rerun_aggregate_id, rerun_payload, block_transition_id)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11::jsonb, $12)
	ON CONFLICT (work_item_id) DO UPDATE SET
		product_id = EXCLUDED.product_id,
		branch = EXCLUDED.branch,
		index_branch = EXCLUDED.index_branch,
		stage = EXCLUDED.stage,
		reason = EXCLUDED.reason,
		rerun_state = EXCLUDED.rerun_state,
		rerun_topic = EXCLUDED.rerun_topic,
		rerun_key = EXCLUDED.rerun_key,
		rerun_aggregate_id = EXCLUDED.rerun_aggregate_id,
		rerun_payload = EXCLUDED.rerun_payload,
		block_transition_id = EXCLUDED.block_transition_id,
		reindex_requested_at = NULL,
		reindex_requested_by = NULL,
		index_posted_at = NULL,
		created_at = now()`

const refusalColumns = `work_item_id, product_id, branch, index_branch, stage, reason, rerun_state, rerun_topic, rerun_key,
	rerun_aggregate_id, rerun_payload, reindex_requested_at, COALESCE(reindex_requested_by, ''), created_at,
	block_transition_id, index_posted_at`

func scanRefusal(row pgx.Row) (StoredRefusal, error) {
	var r StoredRefusal
	err := row.Scan(&r.WorkItemID, &r.ProductID, &r.Branch, &r.IndexBranch, &r.Stage, &r.Reason, &r.RerunState,
		&r.RerunTopic, &r.RerunKey, &r.RerunAggregateID, &r.RerunPayload, &r.ReindexRequestedAt, &r.ReindexRequestedBy, &r.CreatedAt,
		&r.BlockTransitionID, &r.IndexPostedAt)
	return r, err
}

// latestBlockSQL is the item's latest block: its newest transition to
// blocked that is not a prompt restore (awaiting_user → blocked), so parking
// the item on its escalation prompt and restoring it keeps the block.
const latestBlockSQL = `
	SELECT id FROM work_item_transitions
	 WHERE work_item_id = $1 AND to_state = 'blocked'
	   AND (from_state IS NULL OR from_state <> 'awaiting_user')
	 ORDER BY timestamp DESC, id DESC
	 LIMIT 1`

// latestBlock reads latestBlockSQL; ok is false when the item was never
// blocked.
func latestBlock(ctx context.Context, q Querier, workID uuid.UUID) (uuid.UUID, bool, error) {
	var id uuid.UUID
	err := q.QueryRow(ctx, latestBlockSQL, workID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("read the latest block of %s: %w", workID, err)
	}
	return id, true, nil
}

// RefuseUngroundedTx refuses workID in the caller's transaction: the item is
// blocked with r.Reason, work.blocked {work_id, reason, kind:
// codex_ungrounded} is enqueued under aggregateID, and the refusal is
// recorded for a re-run. An item that is already blocked is left alone, as
// BlockWorkItemTx does. Any error must roll the caller's transaction back.
func RefuseUngroundedTx(ctx context.Context, tx pgx.Tx, aggregateID, workID uuid.UUID, agentID string, r Refusal) error {
	if r.ProductID == uuid.Nil {
		return fmt.Errorf("refuse work item %s: no product", workID)
	}
	if r.RerunTopic == "" || r.RerunState == "" || len(r.RerunPayload) == 0 {
		return fmt.Errorf("refuse work item %s: no re-run message", workID)
	}
	if !workgraph.IsValidTransition(models.WorkItemStateBlocked, r.RerunState) {
		return fmt.Errorf("refuse work item %s: blocked cannot re-enter %s", workID, r.RerunState)
	}
	payload, err := withoutEventID(r.RerunPayload)
	if err != nil {
		return fmt.Errorf("refuse work item %s: re-run payload: %w", workID, err)
	}
	blocked, err := blockTx(ctx, tx, aggregateID, workID, agentID, WorkBlockedPayload{
		WorkID: workID.String(), Reason: r.Reason, Kind: BlockKindCodexUngrounded,
	})
	if err != nil || !blocked {
		return err
	}
	// The branch a Re-index indexes: the one the guard fell back to (the
	// product's default branch), "" when it cannot be resolved (the indexer
	// then uses each clone's checked-out branch).
	indexBranch, _, gerr := codex.GroundingBranch(ctx, tx, r.ProductID, r.Branch)
	if gerr != nil {
		indexBranch = r.Branch
	}
	agg := r.RerunAggregateID
	if agg == uuid.Nil {
		agg = aggregateID
	}
	key := r.RerunKey
	if key == "" {
		key = workID.String()
	}
	blockID, ok, err := latestBlock(ctx, tx, workID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("refuse work item %s: the block transition is missing", workID)
	}
	if _, err := tx.Exec(ctx, upsertRefusalSQL, workID, r.ProductID, r.Branch, indexBranch, r.Stage, r.Reason,
		string(r.RerunState), r.RerunTopic, key, agg, string(payload), blockID); err != nil {
		return fmt.Errorf("record codex refusal of %s: %w", workID, err)
	}
	return nil
}

// RefuseUngrounded is RefuseUngroundedTx in a transaction of its own (db is
// the stage's idempotency.Wrap'd pool, so the delivery's guard row commits
// with the refusal).
func RefuseUngrounded(ctx context.Context, db workgraph.TxBeginner, aggregateID, workID uuid.UUID, agentID string, r Refusal) error {
	return workgraph.WithTx(ctx, db, func(tx pgx.Tx) error {
		return RefuseUngroundedTx(ctx, tx, aggregateID, workID, agentID, r)
	})
}

// CheckCodexReady is the codex half of every stage's context validation, the
// same in all three: onboarding complete (a codex version exists) and
// CheckCodexGrounded. Any failure of it is a codex refusal
// (IsCodexRefusal): the same kind, Re-index / Retry / Cancel, whichever
// stage refuses.
func CheckCodexReady(ctx context.Context, db *pgxpool.Pool, productID uuid.UUID, branch string) ValidationResult {
	return RunAll(
		CheckOnboardingComplete(ctx, db, &productID),
		CheckCodexGrounded(ctx, db, productID, branch),
	)
}

// IsCodexRefusal reports whether a failed context check is about the codex
// (no module, or no codex version at all), so the refusal offers Re-index.
func IsCodexRefusal(v ValidationResult) bool {
	for _, m := range v.Missing {
		if m == MissingCodexGrounding || m == "codex" {
			return true
		}
	}
	return false
}

// withoutEventID drops the top-level event_id the outbox injected into the
// message, so the replay is enqueued as a new event.
func withoutEventID(raw []byte) (json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	if fields == nil {
		return nil, errors.New("payload is not a JSON object")
	}
	delete(fields, outbox.FieldEventID)
	return json.Marshal(fields)
}

// Querier is the read surface the refusal readers need (*pgxpool.Pool and
// pgx.Tx satisfy it).
type Querier = codex.Querier

// Execer runs a statement (*pgxpool.Pool and pgx.Tx satisfy it).
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// LoadRefusal reads workID's refusal; ok is false when there is none.
func LoadRefusal(ctx context.Context, q Querier, workID uuid.UUID) (StoredRefusal, bool, error) {
	r, err := scanRefusal(q.QueryRow(ctx, `SELECT `+refusalColumns+` FROM codex_grounding_refusals WHERE work_item_id = $1`, workID))
	if errors.Is(err, pgx.ErrNoRows) {
		return StoredRefusal{}, false, nil
	}
	if err != nil {
		return StoredRefusal{}, false, fmt.Errorf("load codex refusal of %s: %w", workID, err)
	}
	return r, true, nil
}

// MarkReindexRequested records that an operator chose Re-index for workID's
// refusal; status-orchestrator re-runs the item on the product's next
// grounded codex.indexed. It fails when the item has no refusal.
func MarkReindexRequested(ctx context.Context, db Execer, workID uuid.UUID, by string) error {
	tag, err := db.Exec(ctx, `UPDATE codex_grounding_refusals SET reindex_requested_at = now(), reindex_requested_by = $2, index_posted_at = NULL WHERE work_item_id = $1`, workID, by)
	if err != nil {
		return fmt.Errorf("mark re-index of %s: %w", workID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("work item %s has no codex refusal to re-index", workID)
	}
	return nil
}

// MarkIndexPosted records that knowledge-indexer accepted the Re-index of
// workID's refusal (while the request is still pending).
func MarkIndexPosted(ctx context.Context, db Execer, workID uuid.UUID) error {
	if _, err := db.Exec(ctx, `UPDATE codex_grounding_refusals SET index_posted_at = now() WHERE work_item_id = $1 AND reindex_requested_at IS NOT NULL`, workID); err != nil {
		return fmt.Errorf("mark re-index of %s posted: %w", workID, err)
	}
	return nil
}

// CurrentRefusalCondition is the SQL condition (on codex_grounding_refusals
// aliased r) of a refusal that still describes its item, the same rule as
// ReplayRefusalTx: the item is blocked, or parked in awaiting_user from
// blocked, and - when the refusal knows its block - that block is still the
// item's latest (latestBlockSQL: a prompt park and restore does not count).
const CurrentRefusalCondition = `EXISTS (
		SELECT 1 FROM work_items w
		 WHERE w.id = r.work_item_id
		   AND (w.state = 'blocked' OR (w.state = 'awaiting_user' AND w.previous_state = 'blocked')))
	AND (r.block_transition_id IS NULL OR r.block_transition_id = (
		SELECT t.id FROM work_item_transitions t
		 WHERE t.work_item_id = r.work_item_id AND t.to_state = 'blocked'
		   AND (t.from_state IS NULL OR t.from_state <> 'awaiting_user')
		 ORDER BY t.timestamp DESC, t.id DESC
		 LIMIT 1))`

// DropStaleReindex deletes every refusal with a requested Re-index that no
// longer describes its item (CurrentRefusalCondition fails: the item moved
// on, or was blocked again for another reason) and returns their items. The
// Re-index sweep and poster call it first, so a stale refusal is dropped
// instead of being escalated or posted again.
func DropStaleReindex(ctx context.Context, q Querier) ([]uuid.UUID, error) {
	rows, err := q.Query(ctx, `DELETE FROM codex_grounding_refusals r
		WHERE r.reindex_requested_at IS NOT NULL AND NOT (`+CurrentRefusalCondition+`)
		RETURNING r.work_item_id`)
	if err != nil {
		return nil, fmt.Errorf("drop stale re-index requests: %w", err)
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("drop stale re-index requests: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// UnpostedReindex lists up to limit refusals whose Re-index is requested
// but not yet accepted by the indexer (it answered 409), oldest first. Only
// refusals that still describe their item are listed; stale ones are
// dropped first (DropStaleReindex).
func UnpostedReindex(ctx context.Context, q Querier, limit int) ([]StoredRefusal, error) {
	if _, err := DropStaleReindex(ctx, q); err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT `+refusalColumns+` FROM codex_grounding_refusals r
		WHERE r.reindex_requested_at IS NOT NULL AND r.index_posted_at IS NULL AND `+CurrentRefusalCondition+`
		ORDER BY r.reindex_requested_at, r.work_item_id LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("list unposted re-index requests: %w", err)
	}
	defer rows.Close()
	var out []StoredRefusal
	for rows.Next() {
		r, err := scanRefusal(rows)
		if err != nil {
			return nil, fmt.Errorf("scan codex refusal: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CodexRefusalPrompt is the escalation of a refusal on an ungrounded codex:
// Re-index (full index of the product; the item re-runs once the codex has a
// module), Retry (re-run the refused stage now) or Cancel.
func CodexRefusalPrompt(workID, productID uuid.UUID, title, askingAgent, reason string) *interactions.Prompt {
	return &interactions.Prompt{
		ID:          uuid.New(),
		WorkItemID:  workID,
		ProductID:   productID,
		AskingAgent: askingAgent,
		Trigger:     interactions.TriggerBlocked,
		PromptType:  interactions.PromptTypeEscalation,
		Headline:    fmt.Sprintf("No codex to ground: %s", title),
		Question:    reason,
		ContextLink: fmt.Sprintf("/work-items/%s", workID),
		Actions: []interactions.Action{
			{ID: ActionReindex, Label: "Re-index", Semantic: interactions.SemanticCustom},
			{ID: ActionRetry, Label: "Retry", Semantic: interactions.SemanticRetry},
			{ID: ActionCancel, Label: "Cancel", Semantic: interactions.SemanticCancel},
		},
		ContextPayload: map[string]any{"block_kind": BlockKindCodexUngrounded},
		State:          interactions.StatePending,
		RequestedAt:    time.Now().UTC(),
		PromptedVia:    []string{"dashboard"},
	}
}

// EscalateRefusalTx raises workID's codex refusal escalation again in the
// caller's transaction, unless an escalation is already pending for the
// item: every path that settles a Re-index without re-running the item
// (the index failed, finished still empty, or never reported back) calls
// it, so no refused item is left without a pending prompt. raised is false
// when one was pending or the item has no product.
func EscalateRefusalTx(ctx context.Context, tx pgx.Tx, workID uuid.UUID, askingAgent, reason string) (raised bool, err error) {
	var pending bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM user_interactions WHERE work_item_id = $1 AND prompt_type = 'escalation' AND state = 'pending')`, workID).Scan(&pending); err != nil {
		return false, fmt.Errorf("check pending escalation of %s: %w", workID, err)
	}
	if pending {
		return false, nil
	}
	var title string
	var productID, parentID *uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT title, product_id, parent_id FROM work_items WHERE id = $1`, workID).Scan(&title, &productID, &parentID); err != nil {
		return false, fmt.Errorf("read work item %s: %w", workID, err)
	}
	if productID == nil {
		return false, nil
	}
	aggregate := workID
	if parentID != nil {
		aggregate = *parentID
	}
	if err := interactions.NewPublisher(nil).PublishTx(ctx, tx, aggregate, CodexRefusalPrompt(workID, *productID, title, askingAgent, reason)); err != nil {
		return false, fmt.Errorf("raise the codex escalation of %s: %w", workID, err)
	}
	return true, nil
}

// ClearReindexRequested withdraws a Re-index request (the index call failed,
// or the index finished without grounding the product).
func ClearReindexRequested(ctx context.Context, db Execer, workID uuid.UUID) error {
	_, err := db.Exec(ctx, `UPDATE codex_grounding_refusals SET reindex_requested_at = NULL, reindex_requested_by = NULL, index_posted_at = NULL WHERE work_item_id = $1`, workID)
	if err != nil {
		return fmt.Errorf("clear re-index of %s: %w", workID, err)
	}
	return nil
}

// DeleteRefusal drops workID's refusal (the item was cancelled or moved on).
func DeleteRefusal(ctx context.Context, db Execer, workID uuid.UUID) error {
	if _, err := db.Exec(ctx, `DELETE FROM codex_grounding_refusals WHERE work_item_id = $1`, workID); err != nil {
		return fmt.Errorf("delete codex refusal of %s: %w", workID, err)
	}
	return nil
}

// PendingReindex lists productID's refusals waiting for a re-index, oldest
// first.
func PendingReindex(ctx context.Context, q Querier, productID uuid.UUID) ([]StoredRefusal, error) {
	rows, err := q.Query(ctx, `SELECT `+refusalColumns+` FROM codex_grounding_refusals
		WHERE product_id = $1 AND reindex_requested_at IS NOT NULL ORDER BY reindex_requested_at, work_item_id`, productID)
	if err != nil {
		return nil, fmt.Errorf("list pending re-index of product %s: %w", productID, err)
	}
	defer rows.Close()
	var out []StoredRefusal
	for rows.Next() {
		r, err := scanRefusal(rows)
		if err != nil {
			return nil, fmt.Errorf("scan codex refusal: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ReplayMode says which replays ReplayRefusalTx may make.
type ReplayMode int

const (
	// ReplayRetry: the operator chose Retry; any current refusal replays.
	ReplayRetry ReplayMode = iota
	// ReplayReindexed: a codex.indexed settled a Re-index; the refusal
	// replays only while its Re-index is still requested (re-checked under
	// the row lock, so a concurrent sweep that withdrew it wins).
	ReplayReindexed
)

// ReplayRefusalTx re-runs workID from its refusal in the caller's
// transaction: the item goes from blocked to the refusal's state, the
// refused message is enqueued again, and the refusal is deleted. An item
// parked in awaiting_user on its escalation prompt (previous_state blocked)
// is unparked first.
//
// replayed is false, with nothing replayed, when there is no refusal, when
// mode is ReplayReindexed and the Re-index is no longer requested, or when
// the refusal is stale: the item is no longer blocked, or its latest block
// is not the one the refusal created (it moved on and was blocked again for
// another reason). A stale refusal is deleted.
func ReplayRefusalTx(ctx context.Context, tx pgx.Tx, workID uuid.UUID, agentID, reason string, mode ReplayMode) (replayed bool, err error) {
	r, err := scanRefusal(tx.QueryRow(ctx, `SELECT `+refusalColumns+` FROM codex_grounding_refusals WHERE work_item_id = $1 FOR UPDATE`, workID))
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("load codex refusal of %s: %w", workID, err)
	}
	if mode == ReplayReindexed && r.ReindexRequestedAt == nil {
		return false, nil
	}
	var state string
	var previous *string
	if err := tx.QueryRow(ctx, `SELECT state::text, previous_state FROM work_items WHERE id = $1 FOR UPDATE`, workID).Scan(&state, &previous); err != nil {
		return false, fmt.Errorf("lock work item %s: %w", workID, err)
	}
	parked := models.WorkItemState(state) == models.WorkItemStateAwaitingUser && previous != nil && models.WorkItemState(*previous) == models.WorkItemStateBlocked
	stale := models.WorkItemState(state) != models.WorkItemStateBlocked && !parked
	if !stale && r.BlockTransitionID != nil {
		current, ok, err := latestBlock(ctx, tx, workID)
		if err != nil {
			return false, err
		}
		stale = !ok || current != *r.BlockTransitionID
	}
	if stale {
		if _, err := tx.Exec(ctx, `DELETE FROM codex_grounding_refusals WHERE work_item_id = $1`, workID); err != nil {
			return false, fmt.Errorf("drop stale codex refusal of %s: %w", workID, err)
		}
		return false, nil
	}
	wg := workgraph.New(nil)
	if parked {
		if _, err := tx.Exec(ctx, `UPDATE work_items SET previous_state = NULL, awaiting_prompt_id = NULL WHERE id = $1`, workID); err != nil {
			return false, fmt.Errorf("unpark %s: %w", workID, err)
		}
		if _, err := wg.TransitionStateTx(ctx, tx, workID, models.WorkItemStateBlocked, agentID, "escalation answered"); err != nil {
			return false, fmt.Errorf("unpark %s: %w", workID, err)
		}
	}
	if _, err := wg.TransitionStateTx(ctx, tx, workID, models.WorkItemState(r.RerunState), agentID, reason); err != nil {
		return false, fmt.Errorf("re-run %s: %w", workID, err)
	}
	payload, err := withoutEventID(r.RerunPayload)
	if err != nil {
		return false, fmt.Errorf("re-run %s: payload: %w", workID, err)
	}
	if err := outbox.EnqueueJSON(ctx, tx, r.RerunAggregateID, r.RerunTopic, r.RerunKey, payload); err != nil {
		return false, fmt.Errorf("re-run %s: %w", workID, err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM codex_grounding_refusals WHERE work_item_id = $1`, workID); err != nil {
		return false, fmt.Errorf("re-run %s: delete refusal: %w", workID, err)
	}
	return true, nil
}

// blockTx is the body of BlockWorkItemTx with the full work.blocked
// payload. blocked is false (and nothing is written) when the item was
// already blocked.
func blockTx(ctx context.Context, tx pgx.Tx, aggregateID, workID uuid.UUID, agentID string, payload WorkBlockedPayload) (blocked bool, err error) {
	if tx == nil {
		return false, fmt.Errorf("block work item %s: nil transaction", workID)
	}
	if aggregateID == uuid.Nil || workID == uuid.Nil {
		return false, fmt.Errorf("block work item: aggregate id and work item id are both required")
	}
	// TransitionStateTx touches only the transaction it is given, never the
	// WorkGraph's pool, so a pool-less WorkGraph is enough here.
	from, err := workgraph.New(nil).TransitionStateTx(ctx, tx, workID, models.WorkItemStateBlocked, agentID, payload.Reason)
	if err != nil {
		if errors.Is(err, workgraph.ErrInvalidTransition) && from == models.WorkItemStateBlocked {
			return false, nil
		}
		return false, fmt.Errorf("block work item %s: transition to blocked failed: %w", workID, err)
	}
	if err := outbox.EnqueueJSON(ctx, tx, aggregateID, bus.TopicWorkBlocked, workID.String(), payload); err != nil {
		return false, fmt.Errorf("block work item %s: %w", workID, err)
	}
	return true, nil
}
