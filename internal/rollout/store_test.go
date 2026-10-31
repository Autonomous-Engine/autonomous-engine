package rollout

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// fakeDB is an in-memory rollouts row for the store's approval paths. It
// applies the status UPDATEs (honouring the verdict re-check of the plain
// approval) and records every Exec.
type fakeDB struct {
	r      Rollout
	execs  []string
	args   [][]any
	update []string
}

func (f *fakeDB) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	f.execs = append(f.execs, sql)
	f.args = append(f.args, args)
	return pgconn.NewCommandTag("INSERT 0 1"), nil
}

func (f *fakeDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("fakeDB: Query unused")
}

func (f *fakeDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	s := strings.TrimSpace(sql)
	switch {
	case strings.HasPrefix(s, "UPDATE rollouts SET status = 'approved'"):
		f.update = append(f.update, s)
		if f.r.Status != StatusEvaluating || f.r.Verdict() != VerdictApproved {
			return fakeRow{err: pgx.ErrNoRows}
		}
		f.r.Status = StatusApproved
	case strings.HasPrefix(s, "UPDATE rollouts SET status = $2"):
		f.update = append(f.update, s)
		to := args[1].(string)
		allowed := false
		for _, from := range args[6].([]string) {
			if from == f.r.Status {
				allowed = true
			}
		}
		if !allowed {
			return fakeRow{err: pgx.ErrNoRows}
		}
		f.r.Status = to
	case strings.HasPrefix(s, "SELECT status FROM rollouts"):
		return fakeRow{status: &f.r.Status}
	}
	return fakeRow{r: f.r}
}

type fakeRow struct {
	r      Rollout
	status *string
	err    error
}

func (row fakeRow) Scan(dest ...any) error {
	if row.err != nil {
		return row.err
	}
	if row.status != nil {
		*dest[0].(*string) = *row.status
		return nil
	}
	r := row.r
	*dest[0].(*uuid.UUID) = r.ID
	*dest[1].(**string) = r.CandidateSnapshotID
	*dest[2].(**string) = r.BaselineSnapshotID
	*dest[3].(*string) = r.Scope
	*dest[4].(**uuid.UUID) = r.ProductID
	*dest[5].(*string) = r.Status
	*dest[6].(*string) = r.CreatedBy
	*dest[7].(*time.Time) = r.CreatedAt
	*dest[8].(*time.Time) = r.UpdatedAt
	*dest[9].(**time.Time) = r.DecidedAt
	*dest[10].(**string) = r.DecidedBy
	*dest[11].(*[]byte) = []byte(r.DecisionReport)
	*dest[12].(*string) = r.Notes
	return nil
}

func fakeRollout(status, verdict string, missing ...string) *fakeDB {
	c, b := "c", "b"
	d := map[string]any{"verdict": verdict}
	if len(missing) > 0 {
		d["not_evaluated"] = missing
	}
	raw, _ := json.Marshal(d)
	return &fakeDB{r: Rollout{ID: uuid.New(), CandidateSnapshotID: &c, BaselineSnapshotID: &b, Scope: ScopeGlobal,
		Status: status, DecisionReport: raw}}
}

func TestStoreApprove_OverridePath(t *testing.T) {
	ctx := context.Background()

	// MORE_TRIALS without a reason: refused, nothing written.
	f := fakeRollout(StatusEvaluating, VerdictMoreTrials)
	if _, err := NewStore(f).Approve(ctx, f.r.ID, "admin", ""); !errors.Is(err, ErrNotApprovable) {
		t.Fatalf("MORE_TRIALS without reason: %v", err)
	}
	if len(f.execs) != 0 || len(f.update) != 0 {
		t.Errorf("refusal wrote %v / %v", f.execs, f.update)
	}

	// INCOMPLETE without a reason names the missing gates.
	f = fakeRollout(StatusEvaluating, VerdictIncomplete, GateCriticalDefect, GateMajorDefectRate)
	_, err := NewStore(f).Approve(ctx, f.r.ID, "admin", "")
	if !errors.Is(err, ErrNotApprovable) || !strings.Contains(err.Error(), GateMajorDefectRate) {
		t.Fatalf("INCOMPLETE without reason: %v", err)
	}

	// INCOMPLETE with a reason: audited (with the missing gates) and approved.
	got, err := NewStore(f).Approve(ctx, f.r.ID, "admin", "M3-07 not landed; reviewed replay diffs by hand")
	if err != nil || got.Status != StatusApproved {
		t.Fatalf("override approve: %+v %v", got, err)
	}
	if len(f.execs) != 1 || !strings.Contains(f.execs[0], "INSERT INTO rollout_overrides") {
		t.Fatalf("audit execs: %v", f.execs)
	}
	details := string(f.args[0][8].([]byte))
	if !strings.Contains(details, `"missing_gates":["critical_escaped_defect","major_defect_rate"]`) || !strings.Contains(details, `"verdict":"INCOMPLETE"`) {
		t.Errorf("audit details %s", details)
	}
	if f.args[0][6] != "M3-07 not landed; reviewed replay diffs by hand" || f.args[0][1] != "approve_rollout" {
		t.Errorf("audit args %v", f.args[0])
	}

	// A proposed rollout approved by override passes through evaluating.
	f = fakeRollout(StatusProposed, "")
	if got, err := NewStore(f).Approve(ctx, f.r.ID, "admin", "reason"); err != nil || got.Status != StatusApproved || len(f.update) != 2 {
		t.Fatalf("proposed override: %+v %v updates=%d", got, err, len(f.update))
	}

	// APPROVED: no reason, no audit, and the UPDATE re-checks the verdict.
	f = fakeRollout(StatusEvaluating, VerdictApproved)
	if got, err := NewStore(f).Approve(ctx, f.r.ID, "admin", ""); err != nil || got.Status != StatusApproved {
		t.Fatalf("approved: %+v %v", got, err)
	}
	if len(f.execs) != 0 || len(f.update) != 1 || !strings.Contains(f.update[0], "decision_report->>'verdict' = 'APPROVED'") {
		t.Errorf("plain approval: execs %v update %v", f.execs, f.update)
	}

	// Blocked stays blocked, even with a reason.
	f = fakeRollout(StatusBlocked, VerdictBlocked)
	if _, err := NewStore(f).Approve(ctx, f.r.ID, "admin", "please"); !errors.Is(err, ErrBadTransition) {
		t.Fatalf("blocked: %v", err)
	}
	if len(f.execs) != 0 {
		t.Errorf("blocked approval audited: %v", f.execs)
	}
}
