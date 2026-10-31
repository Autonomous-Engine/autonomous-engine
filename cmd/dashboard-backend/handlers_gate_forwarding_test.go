package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/google/uuid"
)

// The manual approval paths (dashboard approve-pr, the pr_gate prompt
// answer, dashboard approve, the human_review prompt answer) hand review and
// QA the branch a person approved. Under EVIDENCE_COMPAT_WINDOW=strict the
// review-agent and qa-agent refuse a message without the verified head SHA
// (D1.5), so these paths must forward the SHA that was stored with the
// pending change (migration 046) or written into the review artifact.

// fakeRow is a pgx.Row over fixed values.
type fakeRow struct {
	vals []any
	err  error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != len(r.vals) {
		return fmt.Errorf("scan: %d destinations for %d values", len(dest), len(r.vals))
	}
	for i, d := range dest {
		switch p := d.(type) {
		case *string:
			*p = r.vals[i].(string)
		case *[]string:
			*p = r.vals[i].([]string)
		default:
			return fmt.Errorf("scan: unsupported destination %T", d)
		}
	}
	return nil
}

// fakePendingChanges serves one pending_pr_changes row. With headSHAColumn
// false it behaves like a database where migration 046 has not run: any
// statement naming head_sha fails with undefined_column.
type fakePendingChanges struct {
	row           pendingPRChange
	headSHAColumn bool
	// err, when set, is what every Scan returns (a database without the
	// row, a connection failure), regardless of the statement.
	err  error
	sqls []string
}

func (q *fakePendingChanges) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	q.sqls = append(q.sqls, sql)
	if q.err != nil {
		return fakeRow{err: q.err}
	}
	vals := []any{q.row.ID, q.row.WorkItemID, q.row.RepositoryID, q.row.Branch, q.row.BaseBranch, q.row.PRTitle, q.row.PRDescription, q.row.Status, q.row.FilesChanged}
	if strings.Contains(sql, "head_sha") {
		if !q.headSHAColumn {
			return fakeRow{err: &pgconn.PgError{Code: pgUndefinedColumn, Message: `column "head_sha" does not exist`}}
		}
		vals = append(vals, q.row.HeadSHA)
	}
	return fakeRow{vals: vals}
}

const (
	fwdHeadSHA = "3333333333333333333333333333333333333333"
	fwdWorkID  = "0f1e2d3c-0000-4000-8000-000000000011"
	fwdRepoID  = "0f1e2d3c-0000-4000-8000-000000000012"
)

func storedChange() pendingPRChange {
	return pendingPRChange{
		ID:            "0f1e2d3c-0000-4000-8000-000000000013",
		WorkItemID:    fwdWorkID,
		RepositoryID:  fwdRepoID,
		Branch:        "agent/w/t",
		BaseBranch:    "main",
		PRTitle:       "[repo] Add widget",
		PRDescription: "## Summary\n\nAdd widget\n",
		Status:        "pending",
		FilesChanged:  []string{"internal/real.go"},
		HeadSHA:       fwdHeadSHA,
	}
}

// prReadyWire mirrors what review-agent decodes from pr.ready.
type prReadyWire struct {
	WorkID            string   `json:"work_id"`
	ProductID         string   `json:"product_id"`
	RepositoryID      string   `json:"repository_id"`
	PRNumber          int      `json:"pr_number"`
	PRURL             string   `json:"pr_url"`
	PRDescription     string   `json:"pr_description"`
	Branch            string   `json:"branch"`
	FilesChanged      []string `json:"files_changed"`
	Timestamp         string   `json:"timestamp"`
	HeadSHA           string   `json:"head_sha"`
	PartiallyVerified bool     `json:"partially_verified"`
}

func TestLoadPendingPRChange_ReadsStoredHeadSHA(t *testing.T) {
	for _, tail := range []string{pendingChangeByWorkItem, pendingChangeByID} {
		q := &fakePendingChanges{row: storedChange(), headSHAColumn: true}
		got, err := loadPendingPRChange(context.Background(), q, tail, fwdWorkID)
		if err != nil {
			t.Fatalf("%s: %v", tail, err)
		}
		if got.HeadSHA != fwdHeadSHA {
			t.Errorf("%s: head sha = %q, want %q", tail, got.HeadSHA, fwdHeadSHA)
		}
		if got.ID != storedChange().ID || got.Branch != "agent/w/t" || got.Status != "pending" || len(got.FilesChanged) != 1 {
			t.Errorf("%s: row = %+v", tail, got)
		}
		if len(q.sqls) != 1 || !strings.Contains(q.sqls[0], "head_sha") || !strings.HasSuffix(strings.TrimSpace(q.sqls[0]), tail) {
			t.Errorf("%s: sqls = %q", tail, q.sqls)
		}
	}
}

// Before migration 046 the column does not exist; the approval must still
// work, just without a SHA (the agents then apply their compat window).
func TestLoadPendingPRChange_FallsBackWithoutHeadSHAColumn(t *testing.T) {
	q := &fakePendingChanges{row: storedChange(), headSHAColumn: false}
	got, err := loadPendingPRChange(context.Background(), q, pendingChangeByID, storedChange().ID)
	if err != nil {
		t.Fatalf("fallback: %v", err)
	}
	if got.HeadSHA != "" {
		t.Errorf("head sha = %q, want empty when the column is missing", got.HeadSHA)
	}
	if got.Branch != "agent/w/t" || got.PRTitle != "[repo] Add widget" {
		t.Errorf("row = %+v", got)
	}
	if len(q.sqls) != 2 || !strings.Contains(q.sqls[0], "head_sha") || strings.Contains(q.sqls[1], "head_sha") {
		t.Errorf("expected one retry without head_sha, sqls = %q", q.sqls)
	}
}

// Only the missing-column error is retried: a row that does not exist
// (pgx.ErrNoRows) or any other failure is returned after exactly one
// statement, so the handler's "pending change not found" path is reached
// without a second round trip.
func TestLoadPendingPRChange_OtherErrorsAreNotRetried(t *testing.T) {
	for _, dbErr := range []error{pgx.ErrNoRows, errors.New("connection reset")} {
		q := &fakePendingChanges{row: storedChange(), headSHAColumn: true, err: dbErr}
		_, err := loadPendingPRChange(context.Background(), q, pendingChangeByID, storedChange().ID)
		if !errors.Is(err, dbErr) {
			t.Fatalf("%v: err = %v, want the database error", dbErr, err)
		}
		if len(q.sqls) != 1 {
			t.Fatalf("%v: %d statements issued, want exactly 1 (no retry for a non-column error): %q", dbErr, len(q.sqls), q.sqls)
		}
		if !strings.Contains(q.sqls[0], "head_sha") {
			t.Fatalf("%v: the one statement must be the head_sha read, got %q", dbErr, q.sqls[0])
		}
	}
}

func TestEnqueueGatedPRReady_ForwardsHeadSHAAndPartialVerification(t *testing.T) {
	cases := []struct {
		name        string
		mutate      func(*pendingPRChange)
		wantSHA     string
		wantPartial bool
	}{
		{
			name:    "verified change forwards the stored head sha",
			wantSHA: fwdHeadSHA,
		},
		{
			name:        "partially verified title marker is forwarded",
			mutate:      func(pc *pendingPRChange) { pc.PRTitle = partialVerificationTitlePrefix + pc.PRTitle },
			wantSHA:     fwdHeadSHA,
			wantPartial: true,
		},
		{
			name: "partially verified body marker is forwarded",
			mutate: func(pc *pendingPRChange) {
				pc.PRDescription = "> **" + partialVerificationBodyMarker + "**\n>\n" + pc.PRDescription
			},
			wantSHA:     fwdHeadSHA,
			wantPartial: true,
		},
		{
			name:    "legacy row without head sha publishes without the field",
			mutate:  func(pc *pendingPRChange) { pc.HeadSHA = "" },
			wantSHA: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pc := storedChange()
			if tc.mutate != nil {
				tc.mutate(&pc)
			}
			pub := &outboxTx{}
			now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
			if err := enqueueGatedPRReady(context.Background(), pub, pc, "prod-1", 42, "https://github.com/o/r/pull/42", now); err != nil {
				t.Fatalf("enqueue: %v", err)
			}
			if pub.calls != 1 || pub.topic != bus.TopicPRReady || pub.key != fwdWorkID || pub.aggregate.String() != fwdWorkID {
				t.Fatalf("publish calls=%d topic=%q key=%q", pub.calls, pub.topic, pub.key)
			}
			var raw map[string]json.RawMessage
			if err := json.Unmarshal(pub.payload, &raw); err != nil {
				t.Fatalf("payload: %v", err)
			}
			var got prReadyWire
			if err := json.Unmarshal(pub.payload, &got); err != nil {
				t.Fatalf("payload: %v", err)
			}
			if got.HeadSHA != tc.wantSHA {
				t.Errorf("head_sha = %q, want %q", got.HeadSHA, tc.wantSHA)
			}
			if _, present := raw["head_sha"]; present != (tc.wantSHA != "") {
				t.Errorf("head_sha key present = %v, want %v (additive, omitempty)", present, tc.wantSHA != "")
			}
			if got.PartiallyVerified != tc.wantPartial {
				t.Errorf("partially_verified = %v, want %v", got.PartiallyVerified, tc.wantPartial)
			}
			if got.WorkID != fwdWorkID || got.RepositoryID != fwdRepoID || got.ProductID != "prod-1" || got.PRNumber != 42 ||
				got.PRURL != "https://github.com/o/r/pull/42" || got.Branch != "agent/w/t" || got.PRDescription != pc.PRDescription ||
				len(got.FilesChanged) != 1 || got.FilesChanged[0] != "internal/real.go" || got.Timestamp != "2026-09-10T12:00:00Z" {
				t.Errorf("pre-existing fields changed: %+v", got)
			}
		})
	}
}

func TestEnqueueGatedPRReady_EnqueueErrorIsReturned(t *testing.T) {
	pub := &outboxTx{err: errors.New("outbox insert failed")}
	err := enqueueGatedPRReady(context.Background(), pub, storedChange(), "", 0, "", time.Now())
	if err == nil || !strings.Contains(err.Error(), "outbox insert failed") {
		t.Fatalf("err = %v, want the enqueue error", err)
	}
}

// ─── qa.ready ────────────────────────────────────────────────────────────────

func reviewArtifact(t *testing.T, fields map[string]string) *models.WorkItemArtifact {
	t.Helper()
	b, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return &models.WorkItemArtifact{Type: models.ArtifactTypeReviewComments, Content: b}
}

func TestReviewedHead_PicksBranchAndSHAFromTheSameArtifact(t *testing.T) {
	shaA, shaB := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	cases := []struct {
		name       string
		artifacts  []*models.WorkItemArtifact // oldest first, as GetArtifacts returns them
		wantBranch string
		wantSHA    string
	}{
		{
			name:       "no artifacts",
			wantBranch: "", wantSHA: "",
		},
		{
			name: "newest approved review wins, with its own sha",
			artifacts: []*models.WorkItemArtifact{
				reviewArtifact(t, map[string]string{"decision": "approve", "branch": "b1", "head_sha": shaA}),
				reviewArtifact(t, map[string]string{"decision": "request_changes", "branch": "b2", "head_sha": shaB}),
			},
			wantBranch: "b1", wantSHA: shaA,
		},
		{
			name: "no approved review: newest with a branch, with its own sha",
			artifacts: []*models.WorkItemArtifact{
				reviewArtifact(t, map[string]string{"decision": "request_changes", "branch": "b1", "head_sha": shaA}),
				reviewArtifact(t, map[string]string{"decision": "request_changes", "branch": "b2", "head_sha": shaB}),
			},
			wantBranch: "b2", wantSHA: shaB,
		},
		{
			name: "legacy review without head_sha forwards no sha, never a sibling's",
			artifacts: []*models.WorkItemArtifact{
				reviewArtifact(t, map[string]string{"decision": "request_changes", "branch": "b1", "head_sha": shaA}),
				reviewArtifact(t, map[string]string{"decision": "approve", "branch": "b2"}),
			},
			wantBranch: "b2", wantSHA: "",
		},
		{
			name: "malformed and branchless artifacts are skipped",
			artifacts: []*models.WorkItemArtifact{
				reviewArtifact(t, map[string]string{"decision": "approve", "branch": "b1", "head_sha": shaA}),
				{Type: models.ArtifactTypeReviewComments, Content: json.RawMessage(`{"decision":`)},
				reviewArtifact(t, map[string]string{"decision": "approve", "head_sha": shaB}),
				nil,
			},
			wantBranch: "b1", wantSHA: shaA,
		},
		{
			// M3-07: cycle 1 approved head A (verifier deferred, second pass
			// approved); cycle 2's verifier escalated head B to a human. The
			// human approves B, so B is forwarded, never the stale A.
			name: "verifier escalation after an older approval forwards the escalated head",
			artifacts: []*models.WorkItemArtifact{
				reviewArtifact(t, map[string]string{"kind": "verifier", "decision": "approve_pending_second_pass", "branch": "b1", "head_sha": shaA}),
				reviewArtifact(t, map[string]string{"kind": "second_pass", "decision": "approve", "branch": "b1", "head_sha": shaA}),
				reviewArtifact(t, map[string]string{"kind": "verifier", "decision": "escalate", "branch": "b1", "head_sha": shaB}),
			},
			wantBranch: "b1", wantSHA: shaB,
		},
		{
			// No clone: the verifier escalated without examining a tree but
			// recorded the pr.ready's verified head.
			name: "no-clone verifier escalation forwards the pr.ready head",
			artifacts: []*models.WorkItemArtifact{
				reviewArtifact(t, map[string]string{"kind": "verifier", "decision": "escalate", "branch": "b1", "head_sha": shaA, "reviewed_sha": ""}),
			},
			wantBranch: "b1", wantSHA: shaA,
		},
		{
			name: "a deferred verifier approval is not an approval",
			artifacts: []*models.WorkItemArtifact{
				reviewArtifact(t, map[string]string{"decision": "approve", "branch": "b1", "head_sha": shaA}),
				reviewArtifact(t, map[string]string{"kind": "verifier", "decision": "approve_pending_second_pass", "branch": "b1", "head_sha": shaB}),
			},
			wantBranch: "b1", wantSHA: shaA,
		},
		{
			name: "whitespace-only sha is treated as absent",
			artifacts: []*models.WorkItemArtifact{
				reviewArtifact(t, map[string]string{"decision": "approve", "branch": "b1", "head_sha": "  "}),
			},
			wantBranch: "b1", wantSHA: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			branch, sha := reviewedHead(tc.artifacts)
			if branch != tc.wantBranch || sha != tc.wantSHA {
				t.Errorf("reviewedHead = (%q, %q), want (%q, %q)", branch, sha, tc.wantBranch, tc.wantSHA)
			}
		})
	}
}

// qaReadyWire mirrors what the qa-agent decodes from qa.ready (the key is
// status-orchestrator's qaReadyMessage.HeadSHA tag, "head_sha").
type qaReadyWire struct {
	WorkID             string `json:"work_id"`
	Title              string `json:"title"`
	AcceptanceCriteria string `json:"acceptance_criteria"`
	Branch             string `json:"branch"`
	HeadSHA            string `json:"head_sha"`
}

func TestBuildQAReadyMessage_ForwardsReviewedHeadSHA(t *testing.T) {
	workID := uuid.MustParse(fwdWorkID)
	item := &models.WorkItem{ID: workID, Title: "Add widget", Description: "Given ... when ... then ..."}
	artifacts := []*models.WorkItemArtifact{
		reviewArtifact(t, map[string]string{"decision": "approve", "branch": "agent/w/t", "head_sha": fwdHeadSHA}),
	}

	payload, err := json.Marshal(buildQAReadyMessage(workID, item, artifacts))
	if err != nil {
		t.Fatal(err)
	}
	var got qaReadyWire
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if got.HeadSHA != fwdHeadSHA {
		t.Errorf("head_sha = %q, want %q", got.HeadSHA, fwdHeadSHA)
	}
	if got.WorkID != fwdWorkID || got.Branch != "agent/w/t" || got.Title != "Add widget" || got.AcceptanceCriteria != item.Description {
		t.Errorf("pre-existing fields changed: %+v", got)
	}

	// Legacy review (no sha) and no work item: the field is absent, the
	// rest is unchanged, and nothing panics.
	legacy := []*models.WorkItemArtifact{reviewArtifact(t, map[string]string{"decision": "approve", "branch": "agent/w/t"})}
	payload, err = json.Marshal(buildQAReadyMessage(workID, nil, legacy))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(payload, &raw); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if _, present := raw["head_sha"]; present {
		t.Errorf("legacy review must not carry a head_sha key: %s", payload)
	}
	if string(raw["branch"]) != `"agent/w/t"` || string(raw["work_id"]) != `"`+fwdWorkID+`"` {
		t.Errorf("payload = %s", payload)
	}
}
