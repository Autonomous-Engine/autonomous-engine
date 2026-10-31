package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// failingRoundStore fails RecordResult the first `fail` times with err.
type failingRoundStore struct {
	*fakeRoundStore
	fail  int
	err   error
	calls int
}

func (f *failingRoundStore) RecordResult(ctx context.Context, res researchResultMessage, now time.Time, budget time.Duration) (resultDecision, *researchRound, error) {
	f.calls++
	if f.calls <= f.fail {
		return resultDecision{}, nil, f.err
	}
	return f.fakeRoundStore.RecordResult(ctx, res, now, budget)
}

func fastRecordRetries(t *testing.T) {
	t.Helper()
	orig := recordRetryBackoff
	recordRetryBackoff = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { recordRetryBackoff = orig })
}

// R2 review fix 1 (the F2 rule): a failed record is retried in process, then
// the item is blocked naming the task; the error reaches the bus only when
// the block fails too.
func TestRecordOrBlock(t *testing.T) {
	fastRecordRetries(t)
	ctx := context.Background()
	transient := errors.New("conn reset by peer")
	permanent := &pgconn.PgError{Code: "23503", Message: "fk violation"}

	setup := func(fail int, err error) (*failingRoundStore, researchResultMessage) {
		inner := newFakeRoundStore()
		r, _ := dispatchRound(t, inner, uuid.New(), "p", []string{"code_trace", "impact_assessment"}, time.Now())
		return &failingRoundStore{fakeRoundStore: inner, fail: fail, err: err}, result(r, "code_trace", ResearchStatusOK)
	}

	t.Run("transient then success", func(t *testing.T) {
		store, res := setup(2, transient)
		blocked := 0
		d, r, handled, err := recordOrBlock(ctx, store, res, func(string) error { blocked++; return nil })
		if handled || err != nil || blocked != 0 || !d.Known || r == nil || store.calls != 3 {
			t.Fatalf("handled=%v err=%v blocked=%d known=%v calls=%d, want recorded on the 3rd attempt", handled, err, blocked, d.Known, store.calls)
		}
	})
	t.Run("transient every time blocks naming the task", func(t *testing.T) {
		store, res := setup(99, transient)
		var reason string
		_, _, handled, err := recordOrBlock(ctx, store, res, func(r string) error { reason = r; return nil })
		if !handled || err != nil || store.calls != maxRecordAttempts {
			t.Fatalf("handled=%v err=%v calls=%d, want handled after %d attempts", handled, err, store.calls, maxRecordAttempts)
		}
		if !strings.Contains(reason, res.TaskID) || !strings.Contains(reason, "code_trace") {
			t.Fatalf("block reason %q does not name the task", reason)
		}
	})
	t.Run("permanent error stops early", func(t *testing.T) {
		store, res := setup(99, permanent)
		blocked := 0
		_, _, handled, err := recordOrBlock(ctx, store, res, func(string) error { blocked++; return nil })
		if !handled || err != nil || store.calls != 1 || blocked != 1 {
			t.Fatalf("handled=%v err=%v calls=%d blocked=%d, want one attempt and a block", handled, err, store.calls, blocked)
		}
	})
	t.Run("failed block is returned", func(t *testing.T) {
		store, res := setup(99, transient)
		_, _, handled, err := recordOrBlock(ctx, store, res, func(string) error { return errors.New("db down") })
		if !handled || err == nil {
			t.Fatalf("handled=%v err=%v, want the block failure returned", handled, err)
		}
	})
}

func TestPermanentStoreError(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{errors.New("eof"), false},
		{&pgconn.PgError{Code: "40001"}, false},
		{&pgconn.PgError{Code: "40P01"}, false},
		{&pgconn.PgError{Code: "23505"}, true},
		{&pgconn.PgError{Code: "42P01"}, true},
		{&pgconn.PgError{Code: "22P02"}, true},
		{pgx.ErrNoRows, true},
	} {
		if got := permanentStoreError(c.err); got != c.want {
			t.Errorf("permanentStoreError(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

// R2 review fix 6: when the time budget claims a round, its pending tasks
// reach the honest-failure gate as failures instead of vanishing.
func TestGateResultsCountPendingAsFailed(t *testing.T) {
	store := newFakeRoundStore()
	start := time.Now().Add(-time.Hour)
	round, _ := dispatchRound(t, store, uuid.New(), "p", []string{"code_trace", "impact_assessment", "bug_archaeology"}, start)
	d, r, _ := store.RecordResult(context.Background(), result(round, "code_trace", ResearchStatusOK), time.Now(), defaultTimeBudget)
	if !d.Claimed || !d.TimeExceeded {
		t.Fatalf("decision %+v, want claimed on the time budget", d)
	}
	gate := r.gateResults()
	if len(gate) != 3 || len(r.results()) != 1 {
		t.Fatalf("gate=%d results=%d, want 3 and 1", len(gate), len(r.results()))
	}
	failed := 0
	for _, g := range gate {
		if g.Status != ResearchStatusOK {
			failed++
			if g.Status != taskPending || g.ErrorDetail == "" || g.FindingType == "" {
				t.Errorf("pending task in gate = %+v", g)
			}
		}
	}
	if failed != 2 {
		t.Fatalf("gate failures = %d, want the 2 pending tasks", failed)
	}
	if src := funcSource(t, "main.go", "finishRound"); !strings.Contains(src, "judgeResearchResults(gateResults)") {
		t.Error("finishRound's honest-failure gate does not judge gateResults")
	}
}

// R2 review fix 4: the reuse hash is the PRD alone, so a retry that rebuilds
// prd.ready without the approval guidance still matches.
func TestPRDHashIsThePRDAlone(t *testing.T) {
	if src := funcSource(t, "main.go", "dispatchResearchRound"); !strings.Contains(src, "hash := prdHash(payload.PRDContent)") {
		t.Fatal("the round hash is not the PRD alone")
	}
}
