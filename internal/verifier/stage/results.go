package stage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/verifier"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Result is one verification_results row (migration 059): the decision the
// stage announced for a subtask's head.
type Result struct {
	ID                uuid.UUID
	WorkID            uuid.UUID
	TaskID            uuid.UUID
	HeadSHA           string
	Decision          Decision
	Reason            string
	Legacy            bool
	VerdictArtifactID *uuid.UUID
	VerifierVersion   string
	CreatedAt         time.Time
}

// Execer is the write side of a pgx.Tx.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

const insertResultSQL = `INSERT INTO verification_results
	(id, work_item_id, task_id, head_sha, decision, reason, legacy, verdict_artifact_id, verifier_version, created_at)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`

// InsertResultTx writes r in the caller's transaction, defaulting its id and
// timestamp.
func InsertResultTx(ctx context.Context, tx Execer, r *Result) error {
	if !r.Decision.Valid() {
		return fmt.Errorf("verification result: unknown decision %q", r.Decision)
	}
	if r.ID == uuid.Nil {
		r.ID = uuid.New()
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now().UTC()
	}
	if _, err := tx.Exec(ctx, insertResultSQL, r.ID, r.WorkID, r.TaskID, r.HeadSHA, string(r.Decision), r.Reason, r.Legacy, r.VerdictArtifactID, r.VerifierVersion, r.CreatedAt); err != nil {
		return fmt.Errorf("insert verification result: %w", err)
	}
	return nil
}

const resultColumns = `id, work_item_id, task_id, head_sha, decision, reason, legacy, verdict_artifact_id, verifier_version, created_at`

func scanResult(row pgx.Row) (Result, error) {
	var r Result
	var decision string
	err := row.Scan(&r.ID, &r.WorkID, &r.TaskID, &r.HeadSHA, &decision, &r.Reason, &r.Legacy, &r.VerdictArtifactID, &r.VerifierVersion, &r.CreatedAt)
	r.Decision = Decision(decision)
	return r, err
}

// LatestForTask is the newest result for the subtask; ok is false when
// there is none.
func LatestForTask(ctx context.Context, q verifier.Querier, taskID uuid.UUID) (Result, bool, error) {
	r, err := scanResult(q.QueryRow(ctx, `SELECT `+resultColumns+` FROM verification_results
		WHERE task_id = $1 ORDER BY created_at DESC, id DESC LIMIT 1`, taskID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Result{}, false, nil
	}
	if err != nil {
		return Result{}, false, fmt.Errorf("read verification result of %s: %w", taskID, err)
	}
	return r, true, nil
}

// LatestByTask is the newest result of every subtask of workID that has
// one, keyed by subtask id.
func LatestByTask(ctx context.Context, q verifier.Querier, workID uuid.UUID) (map[uuid.UUID]Result, error) {
	rows, err := q.Query(ctx, `SELECT DISTINCT ON (task_id) `+resultColumns+` FROM verification_results
		WHERE work_item_id = $1 ORDER BY task_id, created_at DESC, id DESC`, workID)
	if err != nil {
		return nil, fmt.Errorf("read verification results of %s: %w", workID, err)
	}
	defer rows.Close()
	out := map[uuid.UUID]Result{}
	for rows.Next() {
		r, err := scanResult(rows)
		if err != nil {
			return nil, fmt.Errorf("scan verification result: %w", err)
		}
		out[r.TaskID] = r
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read verification results of %s: %w", workID, err)
	}
	return out, nil
}

// StoredVerdict is a verdict recorded for a head, with its artifact.
type StoredVerdict struct {
	Result  Result
	Content ArtifactContent
}

// ReusableVerdict is the newest verdict of the current verifier contract
// (verifier.Version) recorded for workID's subtask taskID at headSHA: the
// stage reuses it for a repeated implementation.complete of the same
// subtask and head instead of verifying the same commit again. A verdict
// is judged against one subtask's requirements, so a sibling with the same
// head never inherits it. taskID uuid.Nil matches any subtask: review-agent
// on pr.ready, whose message names the PR's verified head but not its
// subtask (the head is the subtask's branch tip). An empty headSHA never
// matches (a legacy message has no verified head); ok is false when there
// is none.
func ReusableVerdict(ctx context.Context, q verifier.Querier, workID, taskID uuid.UUID, headSHA string) (StoredVerdict, bool, error) {
	if headSHA == "" {
		return StoredVerdict{}, false, nil
	}
	var sv StoredVerdict
	var decision string
	var raw []byte
	err := q.QueryRow(ctx, `SELECT r.id, r.work_item_id, r.task_id, r.head_sha, r.decision, r.reason, r.legacy,
			r.verdict_artifact_id, r.verifier_version, r.created_at, a.content
		FROM verification_results r
		JOIN work_item_artifacts a ON a.id = r.verdict_artifact_id
		WHERE r.work_item_id = $1 AND r.head_sha = $2 AND r.verifier_version = $3
		  AND ($4::uuid IS NULL OR r.task_id = $4)
		  AND r.decision IN ('approve', 'approve_pending_second_pass', 'request_changes', 'escalate')
		ORDER BY r.created_at DESC, r.id DESC LIMIT 1`, workID, headSHA, verifier.Version, nullableUUID(taskID)).
		Scan(&sv.Result.ID, &sv.Result.WorkID, &sv.Result.TaskID, &sv.Result.HeadSHA, &decision, &sv.Result.Reason, &sv.Result.Legacy,
			&sv.Result.VerdictArtifactID, &sv.Result.VerifierVersion, &sv.Result.CreatedAt, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return StoredVerdict{}, false, nil
	}
	if err != nil {
		return StoredVerdict{}, false, fmt.Errorf("read stored verdict of %s at %s: %w", workID, headSHA, err)
	}
	sv.Result.Decision = Decision(decision)
	if err := json.Unmarshal(raw, &sv.Content); err != nil || sv.Content.Kind != ArtifactKindVerifier {
		if err == nil {
			err = fmt.Errorf("artifact kind %q", sv.Content.Kind)
		}
		return StoredVerdict{}, false, fmt.Errorf("stored verdict artifact %v of %s is unreadable: %w", sv.Result.VerdictArtifactID, workID, err)
	}
	return sv, true, nil
}

func nullableUUID(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}

// LoadArtifactContent reads a verdict artifact by id.
func LoadArtifactContent(ctx context.Context, q verifier.Querier, id uuid.UUID) (ArtifactContent, error) {
	var raw []byte
	if err := q.QueryRow(ctx, `SELECT content FROM work_item_artifacts WHERE id = $1`, id).Scan(&raw); err != nil {
		return ArtifactContent{}, fmt.Errorf("read verdict artifact %s: %w", id, err)
	}
	var c ArtifactContent
	if err := json.Unmarshal(raw, &c); err != nil {
		return ArtifactContent{}, fmt.Errorf("decode verdict artifact %s: %w", id, err)
	}
	return c, nil
}
