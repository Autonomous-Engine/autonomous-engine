package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/guards"
	"github.com/autonomousengine/autonomous-engine/internal/outbox"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Persisted plan execution state (M1-06, migration 048).
//
// The planner used to keep its held tasks, its completed set and its
// title -> task id map in process memory. A restart lost them, replicas could
// not share them, and since decomposition always sent Dependencies = nil the
// release path never actually ran. All of it now lives in plan_tasks and
// plan_task_dependencies, and every change to it commits in the same
// transaction as the outbox events it implies:
//
//   - decomposition writes the rows and edges with the subtasks
//     (commitDecomposition) and enqueues only the tasks with no dependencies;
//   - a completion with a release decision marks the task completed and
//     releases every held task whose dependencies are now all completed
//     (completePlanTask);
//   - a completion with a block decision marks the task failed and blocks
//     the parent (failPlanTask) — it never releases anything (D1);
//   - on boot, releaseReadyOnStartup releases whatever is ready, so nothing
//     depends on the process that planned it still being alive.

// planTaskStatus is plan_tasks.status.
type planTaskStatus string

const (
	planTaskHeld      planTaskStatus = "held"
	planTaskReleased  planTaskStatus = "released"
	planTaskCompleted planTaskStatus = "completed"
	planTaskFailed    planTaskStatus = "failed"
)

// initialStatus is the status a planned subtask is committed with: held when
// it waits on another subtask of the plan, released (and enqueued in the same
// transaction) otherwise.
func (ps plannedSubtask) initialStatus() planTaskStatus {
	if len(ps.dependsOn) > 0 {
		return planTaskHeld
	}
	return planTaskReleased
}

// encodeStoredMessage is the implementation.tasks payload as plan_tasks keeps
// it: the message a held task is released with, after any restart.
func encodeStoredMessage(msg implementationTaskMessage) (json.RawMessage, error) {
	raw, err := json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("encode implementation.tasks message for %s: %w", msg.TaskID, err)
	}
	return raw, nil
}

// releaseEventNamespace scopes the name-based release event ids. Changing it
// would let a task released before the change be released a second time.
var releaseEventNamespace = uuid.MustParse("6d1f7c2e-4b8a-4e0f-9a35-8c2b1e7d4f60")

// releaseEventID is the outbox event id for releasing taskID: a name-based
// UUID of the task id plus a "release" suffix. A task is released at most
// once, so the id is stable across retries, restarts and racing releasers,
// and outbox insert's ON CONFLICT (id) DO NOTHING turns a second release
// into a no-op even if the status guard were ever bypassed.
func releaseEventID(taskID uuid.UUID) uuid.UUID {
	return uuid.NewSHA1(releaseEventNamespace, []byte(taskID.String()+":release"))
}

// releaseEvent builds the implementation.tasks event that releases a held
// task: the stored message, unchanged, plus event_id = releaseEventID.
// Addressed like every other task event: aggregate = the parent work item,
// key = the task id.
func releaseEvent(parentWorkID, taskID uuid.UUID, stored json.RawMessage) (outbox.Event, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(stored, &fields); err != nil || fields == nil {
		return outbox.Event{}, fmt.Errorf("stored message for task %s is not a JSON object: %v", taskID, err)
	}
	id, err := json.Marshal(releaseEventID(taskID).String())
	if err != nil {
		return outbox.Event{}, err
	}
	fields[outbox.FieldEventID] = id
	payload, err := json.Marshal(fields)
	if err != nil {
		return outbox.Event{}, fmt.Errorf("encode release event for task %s: %w", taskID, err)
	}
	return outbox.NewEvent("work_item", parentWorkID, bus.TopicImplementationTasks, taskID.String(), json.RawMessage(payload))
}

const insertPlanTaskSQL = `
	INSERT INTO plan_tasks (id, parent_work_id, title, repository_id, status, message, created_at, released_at)
	VALUES ($1, $2, $3, $4, $5, $6::jsonb, clock_timestamp(), CASE WHEN $7::boolean THEN clock_timestamp() END)`

const insertPlanTaskDependencySQL = `
	INSERT INTO plan_task_dependencies (task_id, depends_on_task_id) VALUES ($1, $2)`

// insertPlanTasksTx writes one plan_tasks row per planned subtask and every
// dependency edge, in the caller's (decomposition) transaction. Rows are
// written in plan order with clock_timestamp(), so created_at preserves it.
func insertPlanTasksTx(ctx context.Context, tx pgx.Tx, parentWorkID uuid.UUID, planned []plannedSubtask, stored []json.RawMessage) error {
	for i, ps := range planned {
		status := ps.initialStatus()
		if _, err := tx.Exec(ctx, insertPlanTaskSQL,
			ps.item.ID, parentWorkID, ps.item.Title, ps.item.RepositoryID,
			string(status), string(stored[i]), status == planTaskReleased,
		); err != nil {
			return fmt.Errorf("plan task for %s: %w", ps.repoName, err)
		}
	}
	for _, ps := range planned {
		for _, dep := range ps.dependsOn {
			if refs := ps.contractRefs[dep]; len(refs) > 0 {
				// M2-06: a contract edge (one row per pair, even when the
				// title dependencies also ordered it).
				raw, err := contractRefJSON(refs)
				if err != nil {
					return fmt.Errorf("plan task contract dependency %s -> %s: %w", ps.item.ID, dep, err)
				}
				if _, err := tx.Exec(ctx, insertContractDependencySQL, ps.item.ID, dep, raw); err != nil {
					return fmt.Errorf("plan task contract dependency %s -> %s: %w", ps.item.ID, dep, err)
				}
				continue
			}
			if _, err := tx.Exec(ctx, insertPlanTaskDependencySQL, ps.item.ID, dep); err != nil {
				return fmt.Errorf("plan task dependency %s -> %s: %w", ps.item.ID, dep, err)
			}
		}
	}
	return nil
}

// insertContractDependencySQL writes a contract edge (migration 056): kind
// 'contract' and the edge's contracts as a JSON array.
const insertContractDependencySQL = `
	INSERT INTO plan_task_dependencies (task_id, depends_on_task_id, kind, contract_ref)
	VALUES ($1, $2, 'contract', $3::jsonb)`

// lockPlanTx finds the plan taskID belongs to and locks all of its rows, in
// id order, for the rest of the transaction. It returns found=false when the
// task has no plan_tasks row (a plan committed before M1-06, or a task this
// planner did not create).
//
// The lock is what makes concurrent completions correct. Two dependencies of
// one held task completing at once would otherwise each commit without seeing
// the other's completion, and neither would release the dependent. With the
// plan locked, the second transaction waits, and its next statement (READ
// COMMITTED takes a fresh snapshot per statement) sees the first's commit.
func lockPlanTx(ctx context.Context, tx pgx.Tx, taskID uuid.UUID) (parentWorkID uuid.UUID, status planTaskStatus, found bool, err error) {
	err = tx.QueryRow(ctx, `SELECT parent_work_id FROM plan_tasks WHERE id = $1`, taskID).Scan(&parentWorkID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, "", false, nil
	}
	if err != nil {
		return uuid.Nil, "", false, fmt.Errorf("find plan of task %s: %w", taskID, err)
	}
	if err := lockPlanRowsTx(ctx, tx, parentWorkID); err != nil {
		return uuid.Nil, "", false, err
	}
	var s string
	if err := tx.QueryRow(ctx, `SELECT status FROM plan_tasks WHERE id = $1`, taskID).Scan(&s); err != nil {
		return uuid.Nil, "", false, fmt.Errorf("read status of task %s: %w", taskID, err)
	}
	return parentWorkID, planTaskStatus(s), true, nil
}

func lockPlanRowsTx(ctx context.Context, tx pgx.Tx, parentWorkID uuid.UUID) error {
	if _, err := tx.Exec(ctx,
		`SELECT id FROM plan_tasks WHERE parent_work_id = $1 ORDER BY id FOR UPDATE`, parentWorkID,
	); err != nil {
		return fmt.Errorf("lock plan %s: %w", parentWorkID, err)
	}
	return nil
}

// releaseReadySQL releases every held task of one plan none of whose
// dependencies is anything but completed. A failed dependency is not
// completed, so its dependents stay held (D1).
const releaseReadySQL = `
	UPDATE plan_tasks t
	   SET status = 'released', released_at = clock_timestamp()
	 WHERE t.parent_work_id = $1
	   AND t.status = 'held'
	   AND NOT EXISTS (
	         SELECT 1
	           FROM plan_task_dependencies d
	           JOIN plan_tasks dt ON dt.id = d.depends_on_task_id
	          WHERE d.task_id = t.id
	            AND dt.status <> 'completed')
	RETURNING t.id, t.message, t.created_at`

// releaseReadyTasksTx releases the plan's ready held tasks and enqueues each
// one's implementation.tasks event, all in the caller's transaction. The
// caller must hold the plan lock. It returns the released task ids in plan
// order.
func releaseReadyTasksTx(ctx context.Context, tx pgx.Tx, parentWorkID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := tx.Query(ctx, releaseReadySQL, parentWorkID)
	if err != nil {
		return nil, fmt.Errorf("release ready tasks of plan %s: %w", parentWorkID, err)
	}
	type released struct {
		id        uuid.UUID
		message   []byte
		createdAt time.Time
	}
	var ready []released
	for rows.Next() {
		var r released
		if err := rows.Scan(&r.id, &r.message, &r.createdAt); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan released task: %w", err)
		}
		ready = append(ready, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("release ready tasks of plan %s: %w", parentWorkID, err)
	}
	if len(ready) == 0 {
		return nil, nil
	}
	sort.Slice(ready, func(i, j int) bool {
		if !ready[i].createdAt.Equal(ready[j].createdAt) {
			return ready[i].createdAt.Before(ready[j].createdAt)
		}
		return ready[i].id.String() < ready[j].id.String()
	})

	events := make([]outbox.Event, 0, len(ready))
	ids := make([]uuid.UUID, 0, len(ready))
	for _, r := range ready {
		ev, err := releaseEvent(parentWorkID, r.id, r.message)
		if err != nil {
			return nil, err
		}
		events = append(events, ev)
		ids = append(ids, r.id)
	}
	if err := outboxEnqueueAll(ctx, tx, events...); err != nil {
		return nil, fmt.Errorf("enqueue released tasks of plan %s: %w", parentWorkID, err)
	}
	return ids, nil
}

// planTaskOutcome reports what a completion did to the plan.
type planTaskOutcome struct {
	// tracked is false when the task has no plan_tasks row; nothing changed.
	tracked      bool
	parentWorkID uuid.UUID
	// previous is the task's status before this call.
	previous planTaskStatus
	// changed is false for a no-op (duplicate or out-of-order message).
	changed bool
	// released lists the held tasks this call released, in plan order.
	released []uuid.UUID
}

// planTxHook is a test seam and nil in production. completePlanTask calls it
// with "begin" inside its transaction before taking the plan lock, and with
// "commit" after the release query, just before committing, so a test can
// hold two completions provably in flight at once.
var planTxHook func(stage string, taskID uuid.UUID)

// completePlanTask records a release decision for taskID: the task becomes
// completed and, in the same transaction, every held task of its plan whose
// dependencies are now all completed is released and its event enqueued.
//
// Only a released task can complete. A second completion of a completed task
// is a no-op (a replayed or duplicated implementation.complete). A failed task
// stays failed: failure is terminal for the plan's dependency graph, and
// re-planning creates new tasks. A held task never had its event enqueued, so
// a completion for it is a producer bug and is not trusted to release
// anything.
func completePlanTask(ctx context.Context, db workgraph.TxBeginner, taskID uuid.UUID) (planTaskOutcome, error) {
	var out planTaskOutcome
	err := workgraph.WithTx(ctx, db, func(tx pgx.Tx) error {
		out = planTaskOutcome{}
		if planTxHook != nil {
			planTxHook("begin", taskID)
		}
		parent, status, found, err := lockPlanTx(ctx, tx, taskID)
		if err != nil || !found {
			return err
		}
		out.tracked, out.parentWorkID, out.previous = true, parent, status
		if status != planTaskReleased {
			return nil
		}
		if _, err := tx.Exec(ctx,
			`UPDATE plan_tasks SET status = 'completed', completed_at = clock_timestamp() WHERE id = $1`, taskID,
		); err != nil {
			return fmt.Errorf("mark task %s completed: %w", taskID, err)
		}
		out.changed = true
		released, err := releaseReadyTasksTx(ctx, tx, parent)
		if err != nil {
			return err
		}
		out.released = released
		if planTxHook != nil {
			planTxHook("commit", taskID)
		}
		return nil
	})
	if err != nil {
		return planTaskOutcome{}, err
	}
	return out, nil
}

// failPlanTask records a block decision for taskID: the task becomes failed,
// and in the same transaction the parent is blocked with the reason (its
// work.blocked event enqueued with it, guards.BlockWorkItemTx). Nothing is
// released — a failed task is never a completed dependency, so every task
// behind it stays held (D1).
//
// A completed task is not failed after the fact (its dependents were already
// released on verified evidence and cannot be recalled); a failed task is not
// failed twice. Both are no-ops.
func failPlanTask(ctx context.Context, db workgraph.TxBeginner, taskID uuid.UUID, reason string) (planTaskOutcome, error) {
	var out planTaskOutcome
	err := workgraph.WithTx(ctx, db, func(tx pgx.Tx) error {
		out = planTaskOutcome{}
		parent, status, found, err := lockPlanTx(ctx, tx, taskID)
		if err != nil || !found {
			return err
		}
		out.tracked, out.parentWorkID, out.previous = true, parent, status
		if status != planTaskReleased && status != planTaskHeld {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE plan_tasks SET status = 'failed' WHERE id = $1`, taskID); err != nil {
			return fmt.Errorf("mark task %s failed: %w", taskID, err)
		}
		out.changed = true

		var held int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM plan_tasks WHERE parent_work_id = $1 AND status = 'held'`, parent,
		).Scan(&held); err != nil {
			return fmt.Errorf("count held tasks of plan %s: %w", parent, err)
		}
		blockReason := fmt.Sprintf(
			"Subtask %s failed: %s. Its plan has %d held task(s); a failed subtask never releases its dependents, so they stay held until the plan is retried.",
			taskID, reason, held)
		if err := guards.BlockWorkItemTx(ctx, tx, parent, parent, agentID, blockReason); err != nil {
			if errors.Is(err, workgraph.ErrInvalidTransition) {
				// The parent is somewhere blocked cannot be reached from
				// (cancelled, passed, rejected). The task is still failed
				// and its dependents still held; that is what matters.
				log.Printf("implementation-planner: task %s failed but parent %s cannot be blocked: %v", taskID, parent, err)
				return nil
			}
			return err
		}
		return nil
	})
	if err != nil {
		return planTaskOutcome{}, err
	}
	return out, nil
}

// releaseReadyOnStartup is startup reconstruction: it scans plan_tasks for
// plans that still have held tasks and releases every held task whose
// dependencies are all completed. Under normal operation there are none — a
// completion releases its dependents in its own transaction — so this is the
// safety net for state that reached the table some other way (an operator
// repair, a planner build that predates the release, a process that died
// between two completions of one plan on another replica). It is idempotent:
// the status guard releases a task once, and the release event id is
// deterministic, so a second release could not enqueue twice anyway.
//
// Each plan is released in its own transaction; one plan's failure is
// returned after the others have been attempted.
func releaseReadyOnStartup(ctx context.Context, db workgraph.TxBeginner) (int, error) {
	var parents []uuid.UUID
	if err := workgraph.WithTx(ctx, db, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT DISTINCT parent_work_id FROM plan_tasks WHERE status = 'held' ORDER BY parent_work_id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				return err
			}
			parents = append(parents, id)
		}
		return rows.Err()
	}); err != nil {
		return 0, fmt.Errorf("scan plans with held tasks: %w", err)
	}

	total := 0
	var firstErr error
	for _, parent := range parents {
		var released []uuid.UUID
		err := workgraph.WithTx(ctx, db, func(tx pgx.Tx) error {
			if err := lockPlanRowsTx(ctx, tx, parent); err != nil {
				return err
			}
			var err error
			released, err = releaseReadyTasksTx(ctx, tx, parent)
			return err
		})
		if err != nil {
			log.Printf("implementation-planner: startup release for plan %s: %v", parent, err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for _, id := range released {
			log.Printf("implementation-planner: startup released held task %s of plan %s", id, parent)
		}
		total += len(released)
	}
	return total, firstErr
}
