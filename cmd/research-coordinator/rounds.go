package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Research rounds (R2, migration 058). A round is one dispatch of research
// tasks for a work item. It is persisted with the PRD hash it was dispatched
// for and its whole task set, in the dispatch transaction, so:
//
//   - convergence is judged against the persisted set, not against the
//     results this process has seen (a restart used to declare a round done
//     on its first result and synthesize from one finding with an empty
//     PRD: B2);
//   - a retry with the same PRD hash reuses the ok results of earlier rounds
//     and dispatches only the failed or missing research types.

// Round statuses.
const (
	roundOpen         = "open"
	roundSynthesizing = "synthesizing" // claimed by the result that completed it
	roundCompleted    = "completed"
	roundBlocked      = "blocked"
	roundSuperseded   = "superseded" // a newer round for the item replaced it
)

// taskPending is a round task with no result yet. Recorded tasks carry the
// result's status (ok, limit_hit, timed_out, error, or "missing").
const taskPending = "pending"

type roundTask struct {
	TaskID       string
	ResearchType string
	Status       string
	Result       *researchResultMessage
	// ReusedFrom is the task whose ok result this one carries (a reuse on
	// retry); empty for a dispatched task.
	ReusedFrom string
	Followup   bool
}

type researchRound struct {
	ID           uuid.UUID
	WorkID       uuid.UUID
	PRDHash      string
	WorkType     string
	ProductID    string
	PRDContent   string
	AggregateID  uuid.UUID
	AssignmentID uuid.UUID
	RepoList     []repoRef
	Status       string
	ClaimedBy    string
	CreatedAt    time.Time
	Tasks        []roundTask
}

// resultDecision is what recording one result means for its round.
type resultDecision struct {
	Known        bool // the task belongs to a persisted round
	Converged    bool // no task of the round is pending
	TimeExceeded bool
	// Claimed: this result moved the round to synthesis; only the claimant
	// synthesizes (a redelivery of the claimant may claim again).
	Claimed bool
	Pending int
	Total   int
}

// prdHash fingerprints the PRD a round researched. It is the PRD alone, not
// the approval guidance: the manual Retry rebuilds prd.ready without
// user_guidance, so a guidance-inclusive hash would never match on retry.
func prdHash(prd string) string {
	sum := sha256.Sum256([]byte(strings.ReplaceAll(prd, "\r\n", "\n")))
	return hex.EncodeToString(sum[:])
}

func (r *researchRound) task(id string) *roundTask {
	for i := range r.Tasks {
		if r.Tasks[i].TaskID == id {
			return &r.Tasks[i]
		}
	}
	return nil
}

func (r *researchRound) pending() int {
	n := 0
	for _, t := range r.Tasks {
		if t.Status == taskPending {
			n++
		}
	}
	return n
}

// gateResults are the results the honest-failure gate judges: every
// recorded result plus, for each task still pending (the round was claimed
// on the time budget), a failure in its place — a task that never answered
// is a failed task, not one that did not exist.
func (r *researchRound) gateResults() []researchResultMessage {
	out := r.results()
	for _, t := range r.Tasks {
		if t.Status == taskPending {
			out = append(out, researchResultMessage{
				TaskID:      t.TaskID,
				WorkID:      r.WorkID.String(),
				Status:      taskPending,
				FindingType: t.ResearchType,
				ErrorDetail: "no result before the research time budget ran out",
			})
		}
	}
	return out
}

// results are the recorded results of the round, reused ones included, in
// task order.
func (r *researchRound) results() []researchResultMessage {
	var out []researchResultMessage
	for _, t := range r.Tasks {
		if t.Status != taskPending && t.Result != nil {
			out = append(out, *t.Result)
		}
	}
	return out
}

// applyResult records res on its task and decides whether it completes the
// round. The caller holds the round exclusively (row lock or test fake).
func (r *researchRound) applyResult(res researchResultMessage, now time.Time, timeBudget time.Duration) resultDecision {
	t := r.task(res.TaskID)
	if t == nil {
		return resultDecision{}
	}
	status := res.Status
	if status == "" {
		status = "missing"
	}
	t.Status = status
	cp := res
	t.Result = &cp

	d := resultDecision{Known: true, Pending: r.pending(), Total: len(r.Tasks)}
	d.Converged = d.Pending == 0
	d.TimeExceeded = !r.CreatedAt.IsZero() && now.Sub(r.CreatedAt) >= timeBudget
	claimable := r.Status == roundOpen || (r.Status == roundSynthesizing && r.ClaimedBy == res.TaskID)
	if (d.Converged || d.TimeExceeded) && claimable {
		d.Claimed = true
		r.Status = roundSynthesizing
		r.ClaimedBy = res.TaskID
	}
	return d
}

// roundPlan splits the selected research types into those to dispatch and
// those whose earlier ok result is reused.
type roundPlan struct {
	Dispatch []string
	Reused   []roundTask
}

// planRound reuses, per selected type, the earlier ok result when there is
// one; the rest are dispatched. Order follows selected.
func planRound(selected []string, reusable map[string]roundTask) roundPlan {
	var p roundPlan
	for _, typ := range selected {
		prev, ok := reusable[typ]
		if !ok || prev.Result == nil {
			p.Dispatch = append(p.Dispatch, typ)
			continue
		}
		origin := prev.ReusedFrom
		if origin == "" {
			origin = prev.TaskID
		}
		res := *prev.Result
		p.Reused = append(p.Reused, roundTask{
			TaskID:       uuid.NewString(),
			ResearchType: typ,
			Status:       ResearchStatusOK,
			Result:       &res,
			ReusedFrom:   origin,
		})
	}
	return p
}

// roundStore is the persisted side of rounds that the handlers' decisions
// rest on. pgRoundStore is production; tests use an in-memory fake.
type roundStore interface {
	// Reusable returns, per research type, the latest ok (non-follow-up)
	// result recorded for workID under prdHash.
	Reusable(ctx context.Context, workID uuid.UUID, prdHash string) (map[string]roundTask, error)
	// RecordResult records res on its task and returns the decision with
	// the round as it stands after the write. Known=false (nil round) when
	// the task is in no persisted round.
	RecordResult(ctx context.Context, res researchResultMessage, now time.Time, timeBudget time.Duration) (resultDecision, *researchRound, error)
	// Close ends a round that is open or synthesizing.
	Close(ctx context.Context, roundID uuid.UUID, status string) error
}

// prepareRound reads what is reusable and plans the round. A read failure
// is returned with a plan that dispatches everything (no reuse).
func prepareRound(ctx context.Context, store roundStore, workID uuid.UUID, hash string, selected []string) (roundPlan, error) {
	if store == nil {
		return planRound(selected, nil), nil
	}
	reusable, err := store.Reusable(ctx, workID, hash)
	if err != nil {
		return planRound(selected, nil), err
	}
	return planRound(selected, reusable), nil
}

type pgRoundStore struct {
	pool *pgxpool.Pool
}

func nullableUUID(u uuid.UUID) any {
	if u == uuid.Nil {
		return nil
	}
	return u
}

// insertRoundTx persists r and its tasks inside the dispatch transaction,
// superseding any earlier round of the item still open or synthesizing.
func insertRoundTx(ctx context.Context, tx pgx.Tx, r *researchRound) error {
	if r == nil {
		return nil
	}
	if _, err := tx.Exec(ctx, `
		UPDATE research_rounds SET status = 'superseded', closed_at = NOW()
		WHERE work_item_id = $1 AND id <> $2 AND status IN ('open', 'synthesizing')`, r.WorkID, r.ID); err != nil {
		return fmt.Errorf("supersede earlier research rounds: %w", err)
	}
	repos, err := json.Marshal(r.RepoList)
	if err != nil {
		return fmt.Errorf("marshal round repo list: %w", err)
	}
	if r.RepoList == nil {
		repos = []byte("[]")
	}
	status := r.Status
	if status == "" {
		status = roundOpen
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO research_rounds
			(id, work_item_id, prd_hash, work_type, product_id, prd_content, aggregate_id, assignment_id, repo_list, status, claimed_by_task_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, NULLIF($11, ''))`,
		r.ID, r.WorkID, r.PRDHash, r.WorkType, r.ProductID, r.PRDContent,
		nullableUUID(r.AggregateID), nullableUUID(r.AssignmentID), repos, status, r.ClaimedBy); err != nil {
		return fmt.Errorf("insert research round: %w", err)
	}
	for _, t := range r.Tasks {
		if err := insertRoundTaskTx(ctx, tx, r.ID, t); err != nil {
			return err
		}
	}
	return nil
}

func insertRoundTaskTx(ctx context.Context, tx pgx.Tx, roundID uuid.UUID, t roundTask) error {
	status := t.Status
	if status == "" {
		status = taskPending
	}
	var result []byte
	if t.Result != nil {
		b, err := json.Marshal(t.Result)
		if err != nil {
			return fmt.Errorf("marshal reused result (%s): %w", t.ResearchType, err)
		}
		result = b
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO research_round_tasks (task_id, round_id, research_type, status, result, reused_from_task_id, followup, completed_at)
		VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), $7, CASE WHEN $4 = 'pending' THEN NULL ELSE NOW() END)`,
		t.TaskID, roundID, t.ResearchType, status, result, t.ReusedFrom, t.Followup); err != nil {
		return fmt.Errorf("insert research round task (%s): %w", t.ResearchType, err)
	}
	return nil
}

func (s *pgRoundStore) Reusable(ctx context.Context, workID uuid.UUID, hash string) (map[string]roundTask, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT ON (t.research_type) t.research_type, t.task_id, t.result, COALESCE(t.reused_from_task_id, '')
		FROM research_round_tasks t
		JOIN research_rounds r ON r.id = t.round_id
		WHERE r.work_item_id = $1 AND r.prd_hash = $2
		  AND t.status = 'ok' AND t.result IS NOT NULL AND NOT t.followup
		ORDER BY t.research_type, t.completed_at DESC NULLS LAST, t.created_at DESC`, workID, hash)
	if err != nil {
		return nil, fmt.Errorf("reusable research results: %w", err)
	}
	defer rows.Close()
	out := map[string]roundTask{}
	for rows.Next() {
		var t roundTask
		var raw []byte
		if err := rows.Scan(&t.ResearchType, &t.TaskID, &raw, &t.ReusedFrom); err != nil {
			return nil, fmt.Errorf("scan reusable research result: %w", err)
		}
		var res researchResultMessage
		if err := json.Unmarshal(raw, &res); err != nil || res.Status != ResearchStatusOK {
			continue
		}
		t.Status = ResearchStatusOK
		t.Result = &res
		out[t.ResearchType] = t
	}
	return out, rows.Err()
}

func (s *pgRoundStore) RecordResult(ctx context.Context, res researchResultMessage, now time.Time, timeBudget time.Duration) (resultDecision, *researchRound, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return resultDecision{}, nil, fmt.Errorf("record research result: begin: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // no-op once committed

	var roundID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT round_id FROM research_round_tasks WHERE task_id = $1`, res.TaskID).Scan(&roundID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return resultDecision{}, nil, nil
		}
		return resultDecision{}, nil, fmt.Errorf("find round of task %s: %w", res.TaskID, err)
	}
	round, err := loadRoundForUpdate(ctx, tx, roundID)
	if err != nil {
		return resultDecision{}, nil, err
	}
	d := round.applyResult(res, now, timeBudget)
	t := round.task(res.TaskID)
	raw, err := json.Marshal(t.Result)
	if err != nil {
		return resultDecision{}, nil, fmt.Errorf("marshal research result: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE research_round_tasks SET status = $2, result = $3, completed_at = NOW() WHERE task_id = $1`,
		res.TaskID, t.Status, raw); err != nil {
		return resultDecision{}, nil, fmt.Errorf("record result of task %s: %w", res.TaskID, err)
	}
	if d.Claimed {
		if _, err := tx.Exec(ctx, `
			UPDATE research_rounds SET status = 'synthesizing', claimed_by_task_id = $2 WHERE id = $1`,
			roundID, res.TaskID); err != nil {
			return resultDecision{}, nil, fmt.Errorf("claim research round %s: %w", roundID, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return resultDecision{}, nil, fmt.Errorf("record research result: commit: %w", err)
	}
	return d, round, nil
}

// loadRoundForUpdate reads a round and its tasks, holding the round's row
// lock until tx ends: results for one round are recorded one at a time.
func loadRoundForUpdate(ctx context.Context, tx pgx.Tx, roundID uuid.UUID) (*researchRound, error) {
	r := &researchRound{ID: roundID}
	var agg, assignment string
	var repos []byte
	if err := tx.QueryRow(ctx, `
		SELECT work_item_id, prd_hash, work_type, product_id, prd_content,
		       COALESCE(aggregate_id::text, ''), COALESCE(assignment_id::text, ''),
		       repo_list, status, COALESCE(claimed_by_task_id, ''), created_at
		FROM research_rounds WHERE id = $1 FOR UPDATE`, roundID).Scan(
		&r.WorkID, &r.PRDHash, &r.WorkType, &r.ProductID, &r.PRDContent,
		&agg, &assignment, &repos, &r.Status, &r.ClaimedBy, &r.CreatedAt); err != nil {
		return nil, fmt.Errorf("load research round %s: %w", roundID, err)
	}
	r.AggregateID, _ = uuid.Parse(agg)
	r.AssignmentID, _ = uuid.Parse(assignment)
	if len(repos) > 0 {
		_ = json.Unmarshal(repos, &r.RepoList)
	}
	rows, err := tx.Query(ctx, `
		SELECT task_id, research_type, status, result, COALESCE(reused_from_task_id, ''), followup
		FROM research_round_tasks WHERE round_id = $1 ORDER BY created_at, task_id`, roundID)
	if err != nil {
		return nil, fmt.Errorf("load tasks of research round %s: %w", roundID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var t roundTask
		var raw []byte
		if err := rows.Scan(&t.TaskID, &t.ResearchType, &t.Status, &raw, &t.ReusedFrom, &t.Followup); err != nil {
			return nil, fmt.Errorf("scan research round task: %w", err)
		}
		if len(raw) > 0 {
			var res researchResultMessage
			if json.Unmarshal(raw, &res) == nil {
				t.Result = &res
			}
		}
		r.Tasks = append(r.Tasks, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load tasks of research round %s: %w", roundID, err)
	}
	return r, nil
}

func (s *pgRoundStore) Close(ctx context.Context, roundID uuid.UUID, status string) error {
	_, err := s.pool.Exec(ctx, closeRoundSQL, roundID, status)
	if err != nil {
		return fmt.Errorf("close research round %s: %w", roundID, err)
	}
	return nil
}

const closeRoundSQL = `
		UPDATE research_rounds SET status = $2, closed_at = NOW()
		WHERE id = $1 AND status IN ('open', 'synthesizing')`

// closeRoundTx closes a round inside a caller's transaction (the technical
// brief's), so a brief and its round's completion commit together: a crash
// after the commit cannot leave the round claimable and re-run synthesis.
func closeRoundTx(ctx context.Context, tx pgx.Tx, roundID uuid.UUID, status string) error {
	if roundID == uuid.Nil {
		return nil
	}
	if _, err := tx.Exec(ctx, closeRoundSQL, roundID, status); err != nil {
		return fmt.Errorf("close research round %s: %w", roundID, err)
	}
	return nil
}

// errRoundNotOpen: a follow-up was not added because its round is no longer
// open (completed, in synthesis, blocked or superseded).
var errRoundNotOpen = errors.New("research round is no longer open")

// lockOpenRoundTx locks the round row and fails with errRoundNotOpen unless
// the round is still open.
func lockOpenRoundTx(ctx context.Context, tx pgx.Tx, roundID uuid.UUID) error {
	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM research_rounds WHERE id = $1 FOR UPDATE`, roundID).Scan(&status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return errRoundNotOpen
		}
		return fmt.Errorf("lock research round %s: %w", roundID, err)
	}
	if status != roundOpen {
		return fmt.Errorf("%w (status %s)", errRoundNotOpen, status)
	}
	return nil
}

// maxRecordAttempts is how many times recording a result is tried in
// process before the work item is blocked (the F2 rule: the bus does not
// redeliver a failed message in process, so returning the error would bury
// it under the next committed offset and leave the round unconverged).
const maxRecordAttempts = 3

// recordRetryBackoff is the wait before the 2nd and 3rd attempt; tests
// shorten it.
var recordRetryBackoff = []time.Duration{200 * time.Millisecond, 800 * time.Millisecond}

// permanentStoreError reports an error a retry cannot fix: a missing row or
// a PostgreSQL data, integrity constraint or schema error (classes 22, 23,
// 42). Anything else (lost connection, serialization failure, deadlock,
// shutdown) is transient. Mirrors pr-coordinator's permanentCommitError.
func permanentStoreError(err error) bool {
	if errors.Is(err, pgx.ErrNoRows) {
		return true
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch {
		case strings.HasPrefix(pgErr.Code, "22"), strings.HasPrefix(pgErr.Code, "23"), strings.HasPrefix(pgErr.Code, "42"):
			return true
		}
	}
	return false
}

// recordOrBlock records res on its round, retrying a transient failure in
// process. When every attempt fails it blocks the work item through block
// with a reason naming the task, and reports handled=true: the caller acks.
// err is non-nil only when the block failed too — then the delivery must
// not be acknowledged.
func recordOrBlock(ctx context.Context, store roundStore, res researchResultMessage, block func(reason string) error) (d resultDecision, round *researchRound, handled bool, err error) {
	var recErr error
	for attempt := 1; attempt <= maxRecordAttempts; attempt++ {
		d, round, recErr = store.RecordResult(ctx, res, time.Now(), defaultTimeBudget)
		if recErr == nil {
			return d, round, false, nil
		}
		if permanentStoreError(recErr) || attempt == maxRecordAttempts {
			break
		}
		wait := recordRetryBackoff[len(recordRetryBackoff)-1]
		if attempt-1 < len(recordRetryBackoff) {
			wait = recordRetryBackoff[attempt-1]
		}
		log.Printf("research-coordinator: record result task=%s work_id=%s failed (attempt %d/%d): %v — retrying in %s",
			res.TaskID, res.WorkID, attempt, maxRecordAttempts, recErr, wait)
		select {
		case <-ctx.Done():
			attempt = maxRecordAttempts
		case <-time.After(wait):
		}
	}
	reason := fmt.Sprintf("Research result of task %s (%s, status %s) could not be recorded: %v. "+
		"Research cannot converge without it; retry the work item.", res.TaskID, res.FindingType, res.Status, recErr)
	log.Printf("research-coordinator: blocking work_id=%s — %s", res.WorkID, reason)
	if bErr := block(reason); bErr != nil {
		log.Printf("research-coordinator: ERROR could not block work_id=%s after a failed result record: %v — delivery not acknowledged", res.WorkID, bErr)
		return resultDecision{}, nil, true, fmt.Errorf("block work_id=%s after a failed result record: %w", res.WorkID, bErr)
	}
	return resultDecision{}, nil, true, nil
}

// budgetHintTokens is each task's share of what is left of the work item's
// budget: (budget - spent) / tasks, never negative.
func budgetHintTokens(budget, spent int64, tasks int) int64 {
	if tasks <= 0 {
		return 0
	}
	left := budget - spent
	if left < 0 {
		left = 0
	}
	return left / int64(tasks)
}

// workItemTokensSpent is the same sum the tool loop checks the budget
// against (internal/llm/tools RunToolLoop).
func workItemTokensSpent(ctx context.Context, pool *pgxpool.Pool, workID uuid.UUID) (int64, error) {
	if pool == nil {
		return 0, errors.New("no database")
	}
	var spent int64
	err := pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(input_tokens + output_tokens), 0) FROM inference_logs WHERE work_item_id = $1`,
		workID).Scan(&spent)
	return spent, err
}
