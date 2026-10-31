// Package outcomes maintains work_item_outcomes (migration 053, M4-05): one
// row per top-level work item recording what happened to the work in
// production — the verified head, the release decision, review defects,
// human review and human edits, QA, the merge, a later revert, tokens and
// cost — keyed so it can be rolled up by configuration snapshot.
//
// status-orchestrator calls UpsertTx inside the transaction of the state
// change that produced a fact, so the outcome and the transition commit or
// roll back together. Every derivable column is recomputed from the
// database on each upsert (idempotent under redelivery); event-only facts
// (QA result, merge identity) are passed in and never cleared by a later
// upsert that lacks them. The row holds no user text: no titles, feedback
// or PR bodies.
package outcomes

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// Release decisions (work_item_outcomes.release_decision). The column is
// the worst over each subtask's latest decision: block > human_review >
// legacy_release > release.
const (
	DecisionRelease       = "release"
	DecisionLegacyRelease = "legacy_release"
	DecisionHumanReview   = "human_review"
	DecisionBlock         = "block"
)

// QA results.
const (
	QAPass = "pass"
	QAFail = "fail"
)

// RevertDetectionHeuristic marks reverted_at as set by the title/body
// heuristic (RevertedPRNumbers), not by an authoritative signal.
const RevertDetectionHeuristic = "heuristic"

// Merge is the merge identity from a pr.merged event.
type Merge struct {
	PRNumber   int
	Repository string // owner/name
	HeadSHA    string // the PR head at merge time
	MergeSHA   string // merge_commit_sha
	MergedAt   *time.Time
}

// Facts are the event-only inputs of one upsert. The zero value recomputes
// the derived columns only.
type Facts struct {
	// TaskID and Decision record one subtask's release decision.
	TaskID   string
	Decision string
	// QAResult is QAPass or QAFail.
	QAResult string
	Merge    *Merge
}

// Execer is the write surface (pgx.Tx, *pgxpool.Pool).
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

const worstDecisionExpr = `CASE WHEN bool_or(value = 'block') THEN 'block'
	WHEN bool_or(value = 'human_review') THEN 'human_review'
	WHEN bool_or(value = 'legacy_release') THEN 'legacy_release'
	WHEN bool_or(value = 'release') THEN 'release' END`

// UpsertSQL is the single statement UpsertTx runs. $1 is the top-level work
// item; $2/$3 a subtask's release decision; $4 the QA result; $5–$9 the
// merge identity. NULL parameters leave the stored value as it is.
//
// verified_head_sha is evidence only: the newest implementation_verifications
// head of the item whose outcome is 'verified'. reviewed_head_sha is the head
// the review examined (review artifact head_sha, D1.5). Reviewed is not
// verified: rollups count an item as verified only when release_decision =
// 'release'.
// human_edits_after_review is NULL until both the merged head and the
// reference head (reviewed, else verified) are known; true when they differ
// (a commit nobody reviewed reached the merge — a human push to the agent
// branch, or a rebase).
// cost_usd is NULL when any call with tokens has no effective price, and
// NULL when there are no calls: never a zero that means "unknown".
const UpsertSQL = `
WITH tree AS (
	SELECT id FROM work_items WHERE id = $1 OR parent_id = $1
), tok AS (
	SELECT COALESCE(SUM(input_tokens), 0)::bigint AS tin,
	       COALESCE(SUM(output_tokens), 0)::bigint AS tout,
	       CASE WHEN COUNT(*) FILTER (WHERE input_tokens + output_tokens > 0) = 0 THEN NULL
	            WHEN COUNT(*) FILTER (WHERE cost_usd IS NULL AND input_tokens + output_tokens > 0) > 0 THEN NULL
	            ELSE SUM(cost_usd) END AS cost
	FROM inference_log_costs WHERE work_item_id IN (SELECT id FROM tree)
), review AS (
	SELECT content FROM work_item_artifacts
	WHERE work_item_id = $1 AND artifact_type = 'review_comments'
	ORDER BY created_at DESC LIMIT 1
), verified AS (
	SELECT (SELECT head_sha FROM implementation_verifications
	        WHERE work_item_id = $1 AND outcome = 'verified' AND head_sha <> ''
	        ORDER BY created_at DESC, id DESC LIMIT 1) AS sha,
	       NULLIF((SELECT content->>'head_sha' FROM review), '') AS reviewed
), decision AS (
	SELECT CASE WHEN $2::text IS NULL OR $3::text IS NULL THEN '{}'::jsonb
	            ELSE jsonb_build_object($2::text, $3::text) END AS d
)
INSERT INTO work_item_outcomes AS o (
	work_item_id, config_snapshot_id, verified_head_sha, reviewed_head_sha, release_decisions, release_decision,
	review_defects, human_review_required, human_edits_after_review, qa_result,
	pr_number, repository, merged_head_sha, merge_sha, merged_at,
	tokens_in, tokens_out, cost_usd, updated_at)
SELECT w.id, w.config_snapshot_id, v.sha, v.reviewed, dc.d,
	(SELECT ` + worstDecisionExpr + ` FROM jsonb_each_text(dc.d)),
	(SELECT CASE WHEN jsonb_typeof(content->'defects') = 'object' THEN content->'defects' END FROM review),
	EXISTS (SELECT 1 FROM work_item_transitions t WHERE t.work_item_id = w.id AND t.to_state = 'human_review'),
	CASE WHEN $7::text IS NULL OR COALESCE(v.reviewed, v.sha) IS NULL THEN NULL
	     ELSE $7::text <> COALESCE(v.reviewed, v.sha) END,
	$4::text, $5::int, $6::text, $7::text, $8::text, $9::timestamptz,
	tok.tin, tok.tout, tok.cost, NOW()
FROM work_items w, verified v, decision dc, tok
WHERE w.id = $1
ON CONFLICT (work_item_id) DO UPDATE SET
	config_snapshot_id       = COALESCE(EXCLUDED.config_snapshot_id, o.config_snapshot_id),
	verified_head_sha        = COALESCE(EXCLUDED.verified_head_sha, o.verified_head_sha),
	reviewed_head_sha        = COALESCE(EXCLUDED.reviewed_head_sha, o.reviewed_head_sha),
	release_decisions        = o.release_decisions || EXCLUDED.release_decisions,
	release_decision         = (SELECT ` + worstDecisionExpr + ` FROM jsonb_each_text(o.release_decisions || EXCLUDED.release_decisions)),
	review_defects           = COALESCE(EXCLUDED.review_defects, o.review_defects),
	human_review_required    = o.human_review_required OR EXCLUDED.human_review_required,
	human_edits_after_review = COALESCE(EXCLUDED.human_edits_after_review, o.human_edits_after_review),
	qa_result                = COALESCE(EXCLUDED.qa_result, o.qa_result),
	pr_number                = COALESCE(EXCLUDED.pr_number, o.pr_number),
	repository               = COALESCE(EXCLUDED.repository, o.repository),
	merged_head_sha          = COALESCE(EXCLUDED.merged_head_sha, o.merged_head_sha),
	merge_sha                = COALESCE(EXCLUDED.merge_sha, o.merge_sha),
	merged_at                = COALESCE(EXCLUDED.merged_at, o.merged_at),
	tokens_in                = EXCLUDED.tokens_in,
	tokens_out               = EXCLUDED.tokens_out,
	cost_usd                 = EXCLUDED.cost_usd,
	updated_at               = NOW()`

// UpsertArgs returns UpsertSQL's arguments for rootID and f. It validates
// the enumerated facts so a bad value fails here, not on a CHECK.
func UpsertArgs(rootID uuid.UUID, f Facts) ([]any, error) {
	if rootID == uuid.Nil {
		return nil, fmt.Errorf("outcomes: nil work item id")
	}
	var taskID, decision, qa any
	if f.TaskID != "" || f.Decision != "" {
		switch f.Decision {
		case DecisionRelease, DecisionLegacyRelease, DecisionHumanReview, DecisionBlock:
		default:
			return nil, fmt.Errorf("outcomes: unknown release decision %q", f.Decision)
		}
		if f.TaskID == "" {
			return nil, fmt.Errorf("outcomes: release decision without a task id")
		}
		taskID, decision = f.TaskID, f.Decision
	}
	if f.QAResult != "" {
		if f.QAResult != QAPass && f.QAResult != QAFail {
			return nil, fmt.Errorf("outcomes: unknown QA result %q", f.QAResult)
		}
		qa = f.QAResult
	}
	var pr, repo, head, merge, mergedAt any
	if m := f.Merge; m != nil {
		if m.PRNumber > 0 {
			pr = m.PRNumber
		}
		repo, head, merge = nullIfEmpty(m.Repository), nullIfEmpty(m.HeadSHA), nullIfEmpty(m.MergeSHA)
		if m.MergedAt != nil && !m.MergedAt.IsZero() {
			mergedAt = m.MergedAt.UTC()
		}
	}
	return []any{rootID, taskID, decision, qa, pr, repo, head, merge, mergedAt}, nil
}

// UpsertTx upserts the outcome row of rootID (a top-level work item) in tx.
func UpsertTx(ctx context.Context, tx Execer, rootID uuid.UUID, f Facts) error {
	args, err := UpsertArgs(rootID, f)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, UpsertSQL, args...); err != nil {
		return fmt.Errorf("upsert work item outcome: %w", err)
	}
	return nil
}

// MarkRevertedSQL marks the outcomes of the given PRs of a repository as
// reverted (first detection wins).
const MarkRevertedSQL = `UPDATE work_item_outcomes
	SET reverted_at = $3, revert_detection = 'heuristic', updated_at = NOW()
	WHERE repository = $1 AND pr_number = ANY($2) AND reverted_at IS NULL`

// MarkRevertedTx records that a merged PR in repository reverted prNumbers.
// It returns how many outcome rows were marked.
func MarkRevertedTx(ctx context.Context, tx Execer, repository string, prNumbers []int, at time.Time) (int64, error) {
	if repository == "" || len(prNumbers) == 0 {
		return 0, nil
	}
	nums := make([]int32, 0, len(prNumbers))
	for _, n := range prNumbers {
		nums = append(nums, int32(n))
	}
	tag, err := tx.Exec(ctx, MarkRevertedSQL, repository, nums, at.UTC())
	if err != nil {
		return 0, fmt.Errorf("mark reverted: %w", err)
	}
	return tag.RowsAffected(), nil
}

// FalseSuccessWindowDays is the D7 false-success window: a verified item
// reverted within this many days of its merge was a false success.
const FalseSuccessWindowDays = 14

// MarkFalseSuccessSQL is the nightly false-success job (M4-06). A verified
// item — released on verified evidence, release_decision = 'release' — that
// was later reverted within the window (reverted_at, from the revert
// heuristic) is marked false_success. The flag is never cleared.
//
// "Or a critical defect filed against it within 14 days" (the brief's other
// trigger) is not implemented: post-merge defects need M3-07's structured
// verdicts and a defect-filing signal, neither of which exists yet; today's
// review_defects are pre-merge review findings, not escaped defects. The
// job keys on reverts only (spec: shadow-evaluation-rollout-gates).
const MarkFalseSuccessSQL = `UPDATE work_item_outcomes
	SET false_success = true, updated_at = NOW()
	WHERE NOT false_success
	  AND release_decision = 'release'
	  AND reverted_at IS NOT NULL
	  AND merged_at IS NOT NULL
	  AND reverted_at <= merged_at + make_interval(days => $1)`

// MarkFalseSuccess runs the false-success job once and returns how many
// outcome rows it marked. Idempotent: a marked row is never touched again.
func MarkFalseSuccess(ctx context.Context, db Execer) (int64, error) {
	tag, err := db.Exec(ctx, MarkFalseSuccessSQL, FalseSuccessWindowDays)
	if err != nil {
		return 0, fmt.Errorf("mark false success: %w", err)
	}
	return tag.RowsAffected(), nil
}

var (
	revertWord = regexp.MustCompile(`(?i)\brevert(s|ed|ing)?\b`)
	prRef      = regexp.MustCompile(`(?:^|[^\w/])(?:[\w.-]+/[\w.-]+)?#(\d+)\b`)
)

// RevertedPRNumbers is the revert heuristic: when a merged PR's title or
// body contains the word "revert" (GitHub's revert button titles the PR
// `Revert "<title>"` and writes `Reverts owner/repo#123` in the body), every
// `#N` / `owner/repo#N` reference in title and body other than the PR's own
// number is a reverted PR. Best effort: a revert written by hand without a
// PR reference is missed, and a PR that mentions "revert" and links an
// unrelated PR is a false positive (spec: "Heuristics and their limits").
func RevertedPRNumbers(title, body string, selfNumber int) []int {
	text := title + "\n" + body
	if !revertWord.MatchString(text) {
		return nil
	}
	seen := map[int]bool{}
	var out []int
	for _, m := range prRef.FindAllStringSubmatch(text, -1) {
		n, err := strconv.Atoi(m[1])
		if err != nil || n <= 0 || n == selfNumber || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	sort.Ints(out)
	return out
}

func nullIfEmpty(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}
