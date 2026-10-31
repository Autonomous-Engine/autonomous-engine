package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
)

// repoURLRow answers the repository github_url lookup.
type repoURLRow struct {
	url string
	err error
}

func (q repoURLRow) QueryRow(context.Context, string, ...any) pgx.Row {
	if q.err != nil {
		return fakeRow{err: q.err}
	}
	return fakeRow{vals: []any{q.url}}
}

func TestPendingChangeRemoteCheck(t *testing.T) {
	pc := storedChange()
	for _, tc := range []struct {
		q        repoURLRow
		noGitHub bool
	}{
		{repoURLRow{url: "o/r"}, false},
		{repoURLRow{url: "https://github.com/o/r.git"}, false},
		{repoURLRow{url: "/repo-cache/mirror.git"}, true},
		{repoURLRow{url: "https://gitlab.example/o/r.git"}, true},
		{repoURLRow{err: errors.New("no rows")}, false}, // unknown: github-service decides
	} {
		err := pendingChangeRemoteCheck(context.Background(), tc.q, pc)
		var ng *noGitHubRemoteError
		if got := errors.As(err, &ng); got != tc.noGitHub {
			t.Errorf("%+v: err = %v, want noGitHub=%v", tc.q, err, tc.noGitHub)
		}
		if tc.noGitHub && !strings.Contains(err.Error(), pc.Branch) {
			t.Errorf("message does not name the branch: %v", err)
		}
	}
}

type execRecorder struct{ sqls []string }

func (e *execRecorder) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	e.sqls = append(e.sqls, sql)
	return pgconn.CommandTag{}, nil
}

func TestRevertApproval_OnlyAnApprovedRowGoesBackToPending(t *testing.T) {
	e := &execRecorder{}
	revertApproval(context.Background(), e, uuid.New())
	if len(e.sqls) != 1 || !strings.Contains(e.sqls[0], "status = 'pending'") || !strings.Contains(e.sqls[0], "AND status = 'approved'") {
		t.Fatalf("sql = %q", e.sqls)
	}
}

// ─── Integration (TEST_POSTGRES_DSN) ─────────────────────────────────────────

// seedPendingChange creates a product, work item, repository with remote and
// one pending change for it, and returns the change id and work item id.
func seedPendingChange(t *testing.T, pool *pgxpool.Pool, remote string) (changeID, workID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	productID, repoID := uuid.New(), uuid.New()
	workID, changeID = uuid.New(), uuid.New()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO products (id, name) VALUES ($1, $2)`, []any{productID, "g1-gate-" + productID.String()[:8]}},
		{`INSERT INTO work_items (id, type, state, title, product_id) VALUES ($1, 'feature', 'implementation', 'G1 gate', $2)`, []any{workID, productID}},
		{`INSERT INTO repositories (id, product_id, name, github_url, base_branch) VALUES ($1, $2, $3, $4, 'main')`, []any{repoID, productID, "g1-" + repoID.String()[:8], remote}},
		{`INSERT INTO pending_pr_changes (id, work_item_id, repository_id, branch, base_branch, pr_title) VALUES ($1, $2, $3, 'agent/g1/x', 'main', 'G1')`, []any{changeID, workID, repoID}},
	} {
		if _, err := pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("seed (%s): %v", q.sql, err)
		}
	}
	t.Cleanup(func() {
		c := context.Background()
		for _, s := range []struct {
			sql string
			id  uuid.UUID
		}{
			{`DELETE FROM pending_pr_changes WHERE work_item_id = $1`, workID},
			{`DELETE FROM work_item_transitions WHERE work_item_id = $1`, workID},
			{`DELETE FROM work_items WHERE id = $1`, workID},
			{`DELETE FROM repositories WHERE id = $1`, repoID},
			{`DELETE FROM products WHERE id = $1`, productID},
		} {
			_, _ = pool.Exec(c, s.sql, s.id)
		}
	})
	return changeID, workID
}

// fakeGitHubService counts /pr calls and answers each with status and body.
func fakeGitHubService(t *testing.T, status int, body string) *int32 {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("GITHUB_SERVICE_URL", srv.URL)
	return &calls
}

func changeStatus(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(context.Background(), `SELECT status FROM pending_pr_changes WHERE id = $1`, id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

const g1Conflict = `{"success":false,"message":"repository has no GitHub remote (remote kind local_path)"}`

func approveRequest(id uuid.UUID) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/work-items/approve-pr", bytes.NewBufferString(`{"id":"`+id.String()+`"}`))
	return req.WithContext(context.WithValue(req.Context(), contextKeyUserID, uuid.New().String()))
}

// G1: approving a gated change of a repository without a GitHub remote is
// a 409 while the row is still pending; github-service is not called, and
// the operator can reject the change after merging it by hand.
func TestIntegration_ApprovePendingChange_NonGitHubRemoteStaysPending(t *testing.T) {
	pool := runsIntegrationPool(t)
	calls := fakeGitHubService(t, http.StatusConflict, g1Conflict)
	h := &Handlers{wg: workgraph.New(pool)}
	id, _ := seedPendingChange(t, pool, "/repo-cache/g1-mirror.git")

	rec := httptest.NewRecorder()
	h.ApprovePendingChange(rec, approveRequest(id))
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "no GitHub remote") {
		t.Fatalf("approve: %d %q, want 409 no GitHub remote", rec.Code, rec.Body.String())
	}
	if n := atomic.LoadInt32(calls); n != 0 {
		t.Errorf("github-service calls = %d, want 0", n)
	}
	if s := changeStatus(t, pool, id); s != "pending" {
		t.Fatalf("status = %q, want pending", s)
	}
	rec = httptest.NewRecorder()
	h.RejectPendingChange(rec, httptest.NewRequest(http.MethodPost, "/api/work-items/reject-pr", bytes.NewBufferString(`{"id":"`+id.String()+`","reason":"merged by hand"}`)))
	if rec.Code != http.StatusOK || changeStatus(t, pool, id) != "rejected" {
		t.Fatalf("reject after a manual merge: %d %q", rec.Code, rec.Body.String())
	}
}

// Belt and braces: github-service answers 409 for a repository the dashboard
// still thinks is on GitHub; the approval is put back to pending.
func TestIntegration_ApprovePendingChange_GitHubService409RevertsToPending(t *testing.T) {
	pool := runsIntegrationPool(t)
	calls := fakeGitHubService(t, http.StatusConflict, g1Conflict)
	h := &Handlers{wg: workgraph.New(pool)}
	id, _ := seedPendingChange(t, pool, "o/g1-"+uuid.New().String()[:8])

	rec := httptest.NewRecorder()
	h.ApprovePendingChange(rec, approveRequest(id))
	if rec.Code != http.StatusConflict {
		t.Fatalf("approve: %d %q, want 409", rec.Code, rec.Body.String())
	}
	if n := atomic.LoadInt32(calls); n != 1 {
		t.Errorf("github-service calls = %d, want 1", n)
	}
	if s := changeStatus(t, pool, id); s != "pending" {
		t.Fatalf("status = %q, want pending", s)
	}
}

func TestIntegration_PRGatePromptAnswer_NonGitHubRemoteStaysPending(t *testing.T) {
	pool := runsIntegrationPool(t)
	calls := fakeGitHubService(t, http.StatusConflict, g1Conflict)
	h := &Handlers{wg: workgraph.New(pool)}
	id, workID := seedPendingChange(t, pool, "https://gitlab.example/o/r.git")

	err := h.HandlePRGatePromptAnswer(context.Background(), workID, "approve", "", uuid.New().String())
	var ng *noGitHubRemoteError
	if !errors.As(err, &ng) {
		t.Fatalf("err = %v, want noGitHubRemoteError", err)
	}
	if n := atomic.LoadInt32(calls); n != 0 {
		t.Errorf("github-service calls = %d, want 0", n)
	}
	if s := changeStatus(t, pool, id); s != "pending" {
		t.Fatalf("status = %q, want pending", s)
	}
}

func TestIntegration_PRGatePromptAnswer_GitHubService409RevertsToPending(t *testing.T) {
	pool := runsIntegrationPool(t)
	calls := fakeGitHubService(t, http.StatusConflict, g1Conflict)
	h := &Handlers{wg: workgraph.New(pool)}
	id, workID := seedPendingChange(t, pool, "https://github.com/o/g1.git")

	if err := h.HandlePRGatePromptAnswer(context.Background(), workID, "approve", "", uuid.New().String()); err == nil {
		t.Fatal("want an error from the 409")
	}
	if n := atomic.LoadInt32(calls); n != 1 {
		t.Errorf("github-service calls = %d, want 1", n)
	}
	if s := changeStatus(t, pool, id); s != "pending" {
		t.Fatalf("status = %q, want pending", s)
	}
}
