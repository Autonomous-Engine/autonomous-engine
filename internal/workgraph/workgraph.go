package workgraph

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// WorkGraph provides CRUD operations for work items and their related entities.
type WorkGraph struct {
	pool *pgxpool.Pool
}

// New creates a new WorkGraph backed by the provided connection pool.
func New(pool *pgxpool.Pool) *WorkGraph {
	return &WorkGraph{pool: pool}
}

// DB returns the underlying pgxpool.Pool for direct queries.
func (wg *WorkGraph) DB() *pgxpool.Pool {
	return wg.pool
}

// ─── Write Operations ────────────────────────────────────────────────────────

// execer is the write surface shared by *pgxpool.Pool and pgx.Tx, so one
// statement can be run either on its own or inside a caller's transaction
// without the SQL being written twice.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// insertWorkItemSQL is the single definition of the work_items insert, used by
// both CreateWorkItem and CreateWorkItemTx. Keeping one string means the
// pooled and transactional paths cannot drift in their column list.
const insertWorkItemSQL = `
		INSERT INTO work_items
			(id, external_ref, type, state, title, description, priority, codex_version, parent_id, product_id, repository_id, branch, created_at, updated_at)
		VALUES
			($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
	`

// setAgentDefinitionSQL assigns an agent definition to an existing work item.
const setAgentDefinitionSQL = `UPDATE work_items SET agent_definition_id = $1 WHERE id = $2`

// CreateWorkItem inserts a new work item into the database.
// If the item's ID is the zero UUID, a new one is generated.
// CreatedAt and UpdatedAt are set to now if zero.
func (wg *WorkGraph) CreateWorkItem(ctx context.Context, item *models.WorkItem) error {
	return createWorkItem(ctx, wg.pool, item)
}

// CreateWorkItemTx is CreateWorkItem inside the caller's transaction.
//
// It exists for producers that must commit a work item and the event
// announcing it together — see internal/outbox: the point of the outbox is
// that the row and its event share one commit, and a pool-based insert cannot
// participate in that. Behaviour is otherwise identical to CreateWorkItem,
// including the id and timestamp defaulting, which it mutates on item in
// place. If the caller's transaction rolls back, the row is gone but item
// keeps the id it was given — callers should not treat a populated item.ID as
// proof the insert committed.
func (wg *WorkGraph) CreateWorkItemTx(ctx context.Context, tx pgx.Tx, item *models.WorkItem) error {
	if tx == nil {
		return fmt.Errorf("create work item: nil transaction")
	}
	return createWorkItem(ctx, tx, item)
}

// createWorkItem is the shared body; ex is either the pool or a transaction.
func createWorkItem(ctx context.Context, ex execer, item *models.WorkItem) error {
	if item.ID == uuid.Nil {
		item.ID = uuid.New()
	}
	now := time.Now().UTC()
	if item.CreatedAt.IsZero() {
		item.CreatedAt = now
	}
	if item.UpdatedAt.IsZero() {
		item.UpdatedAt = now
	}

	_, err := ex.Exec(ctx, insertWorkItemSQL,
		item.ID,
		item.ExternalRef,
		item.Type,
		item.State,
		item.Title,
		item.Description,
		item.Priority,
		item.CodexVersion,
		item.ParentID,
		item.ProductID,
		item.RepositoryID,
		item.Branch,
		item.CreatedAt,
		item.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("create work item: %w", err)
	}
	return nil
}

// SetAgentDefinitionTx points a work item at an agent definition inside the
// caller's transaction.
//
// agent_definition_id is not part of models.WorkItem, so it is written by a
// follow-up UPDATE rather than by the insert. A subtask that inherits its
// parent's definition must inherit it in the same transaction that creates the
// subtask: committing the row first and the inheritance afterwards leaves a
// window in which a consumer picks the subtask up under the wrong agent
// configuration, and leaves it permanently mis-configured if the second write
// fails.
func (wg *WorkGraph) SetAgentDefinitionTx(ctx context.Context, tx pgx.Tx, itemID, agentDefinitionID uuid.UUID) error {
	if tx == nil {
		return fmt.Errorf("set agent definition: nil transaction")
	}
	if itemID == uuid.Nil || agentDefinitionID == uuid.Nil {
		return fmt.Errorf("set agent definition: work item id and agent definition id are both required")
	}
	if _, err := tx.Exec(ctx, setAgentDefinitionSQL, agentDefinitionID, itemID); err != nil {
		return fmt.Errorf("set agent definition on work item %s: %w", itemID, err)
	}
	return nil
}

// SQL for the remaining writes. Each statement is spelled once and shared by
// the pooled method and its *Tx twin, so the two cannot drift.
const (
	// selectStateForUpdateSQL locks the work item row for the duration of the
	// transition, so two concurrent transitions serialise on it.
	selectStateForUpdateSQL = `SELECT state FROM work_items WHERE id = $1 FOR UPDATE`

	updateStateSQL = `
		UPDATE work_items SET state = $1, updated_at = $2 WHERE id = $3
	`

	insertTransitionSQL = `
		INSERT INTO work_item_transitions (id, work_item_id, from_state, to_state, agent_id, reason, timestamp)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`

	insertAssignmentSQL = `
		INSERT INTO agent_assignments (id, work_item_id, agent_type, agent_instance_id, assigned_at, status)
		VALUES ($1, $2, $3, $4, $5, $6)
	`

	completeAssignmentSQL = `
		UPDATE agent_assignments SET status = $1, completed_at = $2 WHERE id = $3
	`

	insertArtifactSQL = `
		INSERT INTO work_item_artifacts (id, work_item_id, artifact_type, content, created_at, agent_id)
		VALUES ($1, $2, $3, $4, $5, $6)
	`
)

// ErrInvalidTransition is wrapped by TransitionState and TransitionStateTx
// when the state machine forbids the move. The message keeps its historical
// form, "invalid transition from <from> to <to>". When it is returned,
// TransitionStateTx's from-state is the state the row is actually in, and
// nothing has been written, so the caller's transaction is still usable.
var ErrInvalidTransition = errors.New("invalid transition")

// queryExecer is execer plus QueryRow: what a transition needs to read the
// current state before writing. Satisfied by pgx.Tx.
type queryExecer interface {
	execer
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// TransitionState validates and applies a state transition for a work item inside
// a transaction. It also records a WorkItemTransition row.
func (wg *WorkGraph) TransitionState(ctx context.Context, itemID uuid.UUID, toState models.WorkItemState, agentID, reason string) error {
	return WithTx(ctx, wg.pool, func(tx pgx.Tx) error {
		_, err := transitionState(ctx, tx, itemID, toState, agentID, reason)
		return err
	})
}

// TransitionStateTx is TransitionState inside the caller's transaction, for a
// producer that must commit the transition together with the event announcing
// it (see WithTx and internal/outbox). It returns the state the item was in
// before the transition, read under the same row lock, so the caller can put
// an accurate from_state in that event instead of reading it beforehand and
// racing a concurrent transition.
//
// It neither commits nor rolls back: on error the caller's transaction must be
// rolled back by the caller (WithTx does this).
func (wg *WorkGraph) TransitionStateTx(ctx context.Context, tx pgx.Tx, itemID uuid.UUID, toState models.WorkItemState, agentID, reason string) (models.WorkItemState, error) {
	if tx == nil {
		return "", fmt.Errorf("transition state: nil transaction")
	}
	return transitionState(ctx, tx, itemID, toState, agentID, reason)
}

// transitionState is the shared body of TransitionState and
// TransitionStateTx. q must be a transaction: the SELECT ... FOR UPDATE lock
// is held only until the enclosing transaction ends.
func transitionState(ctx context.Context, q queryExecer, itemID uuid.UUID, toState models.WorkItemState, agentID, reason string) (models.WorkItemState, error) {
	var currentState models.WorkItemState
	err := q.QueryRow(ctx, selectStateForUpdateSQL, itemID).Scan(&currentState)
	if err != nil {
		if err == pgx.ErrNoRows {
			return "", fmt.Errorf("work item %s not found", itemID)
		}
		return "", fmt.Errorf("select current state: %w", err)
	}

	if !IsValidTransition(currentState, toState) {
		return currentState, fmt.Errorf("%w from %s to %s", ErrInvalidTransition, currentState, toState)
	}

	now := time.Now().UTC()

	if _, err := q.Exec(ctx, updateStateSQL, toState, now, itemID); err != nil {
		return currentState, fmt.Errorf("update work item state: %w", err)
	}

	_, err = q.Exec(ctx, insertTransitionSQL,
		uuid.New(),
		itemID,
		currentState,
		toState,
		agentID,
		reason,
		now,
	)
	if err != nil {
		return currentState, fmt.Errorf("insert transition record: %w", err)
	}
	return currentState, nil
}

// AssignAgent inserts an agent assignment record.
// If the assignment's ID is zero, a new one is generated.
func (wg *WorkGraph) AssignAgent(ctx context.Context, assignment *models.AgentAssignment) error {
	return assignAgent(ctx, wg.pool, assignment)
}

// AssignAgentTx is AssignAgent inside the caller's transaction. Like
// CreateWorkItemTx it defaults the id and timestamp on assignment in place,
// so after a rollback assignment.ID names a row that was never committed.
func (wg *WorkGraph) AssignAgentTx(ctx context.Context, tx pgx.Tx, assignment *models.AgentAssignment) error {
	if tx == nil {
		return fmt.Errorf("assign agent: nil transaction")
	}
	return assignAgent(ctx, tx, assignment)
}

func assignAgent(ctx context.Context, ex execer, assignment *models.AgentAssignment) error {
	if assignment.ID == uuid.Nil {
		assignment.ID = uuid.New()
	}
	if assignment.AssignedAt.IsZero() {
		assignment.AssignedAt = time.Now().UTC()
	}

	_, err := ex.Exec(ctx, insertAssignmentSQL,
		assignment.ID,
		assignment.WorkItemID,
		assignment.AgentType,
		assignment.AgentInstanceID,
		assignment.AssignedAt,
		assignment.Status,
	)
	if err != nil {
		return fmt.Errorf("assign agent: %w", err)
	}
	return nil
}

// CompleteAssignment updates an assignment's status and sets completed_at.
func (wg *WorkGraph) CompleteAssignment(ctx context.Context, assignmentID uuid.UUID, status models.AssignmentStatus) error {
	return completeAssignment(ctx, wg.pool, assignmentID, status)
}

// CompleteAssignmentTx is CompleteAssignment inside the caller's transaction.
func (wg *WorkGraph) CompleteAssignmentTx(ctx context.Context, tx pgx.Tx, assignmentID uuid.UUID, status models.AssignmentStatus) error {
	if tx == nil {
		return fmt.Errorf("complete assignment: nil transaction")
	}
	return completeAssignment(ctx, tx, assignmentID, status)
}

func completeAssignment(ctx context.Context, ex execer, assignmentID uuid.UUID, status models.AssignmentStatus) error {
	now := time.Now().UTC()
	if _, err := ex.Exec(ctx, completeAssignmentSQL, status, now, assignmentID); err != nil {
		return fmt.Errorf("complete assignment: %w", err)
	}
	return nil
}

// AddArtifact inserts a work item artifact.
// If the artifact's ID is zero, a new one is generated.
func (wg *WorkGraph) AddArtifact(ctx context.Context, artifact *models.WorkItemArtifact) error {
	return addArtifact(ctx, wg.pool, artifact)
}

// AddArtifactTx is AddArtifact inside the caller's transaction. It defaults
// the id and timestamp on artifact in place; after a rollback artifact.ID
// names a row that does not exist, so a caller must not hand it out (as an
// artifact reference, say) until the transaction has committed.
func (wg *WorkGraph) AddArtifactTx(ctx context.Context, tx pgx.Tx, artifact *models.WorkItemArtifact) error {
	if tx == nil {
		return fmt.Errorf("add artifact: nil transaction")
	}
	return addArtifact(ctx, tx, artifact)
}

func addArtifact(ctx context.Context, ex execer, artifact *models.WorkItemArtifact) error {
	if artifact.ID == uuid.Nil {
		artifact.ID = uuid.New()
	}
	if artifact.CreatedAt.IsZero() {
		artifact.CreatedAt = time.Now().UTC()
	}

	_, err := ex.Exec(ctx, insertArtifactSQL,
		artifact.ID,
		artifact.WorkItemID,
		artifact.Type,
		artifact.Content,
		artifact.CreatedAt,
		artifact.AgentID,
	)
	if err != nil {
		return fmt.Errorf("add artifact: %w", err)
	}
	return nil
}

// ─── Read Operations ─────────────────────────────────────────────────────────

// GetWorkItem retrieves a single work item by ID.
func (wg *WorkGraph) GetWorkItem(ctx context.Context, id uuid.UUID) (*models.WorkItem, error) {
	row := wg.pool.QueryRow(ctx, `
		SELECT id, external_ref, type, state, title, description, priority, codex_version, parent_id, product_id, repository_id, branch, created_at, updated_at
		FROM work_items
		WHERE id = $1
	`, id)

	item := &models.WorkItem{}
	err := row.Scan(
		&item.ID,
		&item.ExternalRef,
		&item.Type,
		&item.State,
		&item.Title,
		&item.Description,
		&item.Priority,
		&item.CodexVersion,
		&item.ParentID,
		&item.ProductID,
		&item.RepositoryID,
		&item.Branch,
		&item.CreatedAt,
		&item.UpdatedAt,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, fmt.Errorf("work item %s not found", id)
		}
		return nil, fmt.Errorf("get work item: %w", err)
	}
	return item, nil
}

// GetChildren returns all work items whose parent_id matches parentID.
func (wg *WorkGraph) GetChildren(ctx context.Context, parentID uuid.UUID) ([]*models.WorkItem, error) {
	rows, err := wg.pool.Query(ctx, `
		SELECT id, external_ref, type, state, title, description, priority, codex_version, parent_id, product_id, repository_id, branch, created_at, updated_at
		FROM work_items
		WHERE parent_id = $1
		ORDER BY created_at
	`, parentID)
	if err != nil {
		return nil, fmt.Errorf("get children: %w", err)
	}
	defer rows.Close()

	return scanWorkItems(rows)
}

// GetByState returns all work items in the given state, ordered by priority then created_at.
func (wg *WorkGraph) GetByState(ctx context.Context, state models.WorkItemState) ([]*models.WorkItem, error) {
	rows, err := wg.pool.Query(ctx, `
		SELECT id, external_ref, type, state, title, description, priority, codex_version, parent_id, product_id, repository_id, branch, created_at, updated_at
		FROM work_items
		WHERE state = $1
		ORDER BY priority, created_at
	`, state)
	if err != nil {
		return nil, fmt.Errorf("get by state: %w", err)
	}
	defer rows.Close()

	return scanWorkItems(rows)
}

// GetArtifacts returns the artifacts for a work item, optionally filtered by type.
// Pass an empty string for artifactType to retrieve all artifact types.
func (wg *WorkGraph) GetArtifacts(ctx context.Context, workItemID uuid.UUID, artifactType models.ArtifactType) ([]*models.WorkItemArtifact, error) {
	var (
		rows pgx.Rows
		err  error
	)

	if artifactType == "" {
		rows, err = wg.pool.Query(ctx, `
			SELECT id, work_item_id, artifact_type, content, created_at, agent_id
			FROM work_item_artifacts
			WHERE work_item_id = $1
			ORDER BY created_at
		`, workItemID)
	} else {
		rows, err = wg.pool.Query(ctx, `
			SELECT id, work_item_id, artifact_type, content, created_at, agent_id
			FROM work_item_artifacts
			WHERE work_item_id = $1 AND artifact_type = $2
			ORDER BY created_at
		`, workItemID, artifactType)
	}
	if err != nil {
		return nil, fmt.Errorf("get artifacts: %w", err)
	}
	defer rows.Close()

	var artifacts []*models.WorkItemArtifact
	for rows.Next() {
		a := &models.WorkItemArtifact{}
		if err := rows.Scan(&a.ID, &a.WorkItemID, &a.Type, &a.Content, &a.CreatedAt, &a.AgentID); err != nil {
			return nil, fmt.Errorf("scan artifact: %w", err)
		}
		artifacts = append(artifacts, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate artifacts: %w", err)
	}
	return artifacts, nil
}

// GetTransitions returns the full transition history for a work item.
func (wg *WorkGraph) GetTransitions(ctx context.Context, workItemID uuid.UUID) ([]*models.WorkItemTransition, error) {
	rows, err := wg.pool.Query(ctx, `
		SELECT id, work_item_id, from_state, to_state, agent_id, reason, timestamp
		FROM work_item_transitions
		WHERE work_item_id = $1
		ORDER BY timestamp
	`, workItemID)
	if err != nil {
		return nil, fmt.Errorf("get transitions: %w", err)
	}
	defer rows.Close()

	var transitions []*models.WorkItemTransition
	for rows.Next() {
		t := &models.WorkItemTransition{}
		if err := rows.Scan(&t.ID, &t.WorkItemID, &t.FromState, &t.ToState, &t.AgentID, &t.Reason, &t.Timestamp); err != nil {
			return nil, fmt.Errorf("scan transition: %w", err)
		}
		transitions = append(transitions, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate transitions: %w", err)
	}
	return transitions, nil
}

// CountByState returns a map of state → count for all work items (used for dashboard).
func (wg *WorkGraph) CountByState(ctx context.Context) (map[models.WorkItemState]int, error) {
	rows, err := wg.pool.Query(ctx, `
		SELECT state, COUNT(*) AS cnt
		FROM work_items
		GROUP BY state
	`)
	if err != nil {
		return nil, fmt.Errorf("count by state: %w", err)
	}
	defer rows.Close()

	result := make(map[models.WorkItemState]int)
	for rows.Next() {
		var state models.WorkItemState
		var count int
		if err := rows.Scan(&state, &count); err != nil {
			return nil, fmt.Errorf("scan state count: %w", err)
		}
		result[state] = count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate state counts: %w", err)
	}
	return result, nil
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

// GetByProduct returns all work items belonging to the given product.
func (wg *WorkGraph) GetByProduct(ctx context.Context, productID uuid.UUID) ([]*models.WorkItem, error) {
	rows, err := wg.pool.Query(ctx, `
		SELECT id, external_ref, type, state, title, description, priority, codex_version, parent_id, product_id, repository_id, branch, created_at, updated_at
		FROM work_items
		WHERE product_id = $1
		ORDER BY priority, created_at
	`, productID)
	if err != nil {
		return nil, fmt.Errorf("get by product: %w", err)
	}
	defer rows.Close()

	return scanWorkItems(rows)
}

// GetByProductAndState returns all work items belonging to the given product in the given state.
func (wg *WorkGraph) GetByProductAndState(ctx context.Context, productID uuid.UUID, state models.WorkItemState) ([]*models.WorkItem, error) {
	rows, err := wg.pool.Query(ctx, `
		SELECT id, external_ref, type, state, title, description, priority, codex_version, parent_id, product_id, repository_id, branch, created_at, updated_at
		FROM work_items
		WHERE product_id = $1 AND state = $2
		ORDER BY priority, created_at
	`, productID, state)
	if err != nil {
		return nil, fmt.Errorf("get by product and state: %w", err)
	}
	defer rows.Close()

	return scanWorkItems(rows)
}

// SetBranch records the git branch (and optionally PR URL) on a work item so
// the dashboard can display it. Called by pr-coordinator after creating a PR.
func (wg *WorkGraph) SetBranch(ctx context.Context, itemID uuid.UUID, branch string) error {
	return setBranch(ctx, wg.pool, itemID, branch)
}

// SetBranchTx is SetBranch inside the caller's transaction, so pr-coordinator
// can record the branch together with the pr.ready event it enqueues. It never
// commits or rolls back.
func (wg *WorkGraph) SetBranchTx(ctx context.Context, tx pgx.Tx, itemID uuid.UUID, branch string) error {
	if tx == nil {
		return fmt.Errorf("set branch: nil transaction")
	}
	return setBranch(ctx, tx, itemID, branch)
}

// setBranchSQL is the single definition of the branch update shared by
// SetBranch and SetBranchTx.
const setBranchSQL = `UPDATE work_items SET branch = $1, updated_at = $2 WHERE id = $3`

func setBranch(ctx context.Context, db execer, itemID uuid.UUID, branch string) error {
	_, err := db.Exec(ctx, setBranchSQL, branch, time.Now().UTC(), itemID)
	return err
}

// scanWorkItems scans a pgx.Rows result set into a slice of WorkItem pointers.
func scanWorkItems(rows pgx.Rows) ([]*models.WorkItem, error) {
	var items []*models.WorkItem
	for rows.Next() {
		item := &models.WorkItem{}
		if err := rows.Scan(
			&item.ID,
			&item.ExternalRef,
			&item.Type,
			&item.State,
			&item.Title,
			&item.Description,
			&item.Priority,
			&item.CodexVersion,
			&item.ParentID,
			&item.ProductID,
			&item.RepositoryID,
			&item.Branch,
			&item.CreatedAt,
			&item.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan work item: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate work items: %w", err)
	}
	return items, nil
}
