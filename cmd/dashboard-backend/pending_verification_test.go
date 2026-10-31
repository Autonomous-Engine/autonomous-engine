package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

type pendingVerificationRow struct {
	err     error
	verdict []byte
}

func (r pendingVerificationRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*dest[0].(*string) = "approve_pending_second_pass"
	*dest[1].(*string) = "every criterion met; no critical or major defect"
	*dest[2].(*time.Time) = time.Date(2026, 10, 5, 1, 2, 3, 0, time.UTC)
	*dest[3].(*[]byte) = r.verdict
	return nil
}

type pendingVerificationQuerier struct {
	row  pendingVerificationRow
	sql  string
	args []any
}

func (q *pendingVerificationQuerier) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	q.sql, q.args = sql, args
	return q.row
}

// V5: the gate card's detail carries the verdict recorded for the gated head.
func TestLoadPendingVerification(t *testing.T) {
	q := &pendingVerificationQuerier{row: pendingVerificationRow{verdict: []byte(`{"kind":"verifier","decision":"approve_pending_second_pass"}`)}}
	v := loadPendingVerification(context.Background(), q, "w", "abc")
	if v == nil || v.Decision != "approve_pending_second_pass" || v.RecordedAt != "2026-10-05T01:02:03Z" || !strings.Contains(string(v.Verdict), `"kind":"verifier"`) {
		t.Fatalf("verification = %+v", v)
	}
	if !strings.Contains(q.sql, "FROM verification_results") || q.args[0] != "w" || q.args[1] != "abc" {
		t.Errorf("query %q args %v", q.sql, q.args)
	}
	if v := loadPendingVerification(context.Background(), &pendingVerificationQuerier{row: pendingVerificationRow{err: pgx.ErrNoRows}}, "w", "abc"); v != nil {
		t.Errorf("no row → nil, got %+v", v)
	}
	if v := loadPendingVerification(context.Background(), q, "w", ""); v != nil {
		t.Errorf("no head → nil, got %+v", v)
	}
}
