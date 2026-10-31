package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// queryErr is an edgeQuerier whose every query fails with err.
type queryErr struct{ err error }

func (q queryErr) Query(context.Context, string, ...any) (pgx.Rows, error) { return nil, q.err }
func (q queryErr) QueryRow(context.Context, string, ...any) pgx.Row        { return nil }

// A pr-coordinator running against a database before migration 056 must
// not block every PR: an undefined column or table is "no contract edges".
// Any other read error still fails closed.
func TestLoadConsumerEdges_PreMigrationDatabaseHasNoEdges(t *testing.T) {
	for _, code := range []string{"42703", "42P01"} {
		edges, err := loadConsumerEdges(context.Background(), queryErr{&pgconn.PgError{Code: code, Message: "column d.kind does not exist"}}, uuid.New())
		if err != nil || edges != nil {
			t.Fatalf("SQLSTATE %s: edges=%v err=%v, want none and no error", code, edges, err)
		}
	}
	if _, err := loadConsumerEdges(context.Background(), queryErr{errors.New("connection reset")}, uuid.New()); err == nil {
		t.Fatal("a real read error must be returned (fail closed)")
	}
	if _, err := loadConsumerEdges(context.Background(), queryErr{&pgconn.PgError{Code: "57014"}}, uuid.New()); err == nil {
		t.Fatal("a query_canceled error must be returned (fail closed)")
	}
}

func TestOrderByProducers_ProducersPrecedeConsumers(t *testing.T) {
	a, b, c, d := &models.WorkItem{ID: uuid.New()}, &models.WorkItem{ID: uuid.New()}, &models.WorkItem{ID: uuid.New()}, &models.WorkItem{ID: uuid.New()}
	// a consumes from c; c consumes from d; b is unrelated.
	got := orderByProducers([]*models.WorkItem{a, b, c, d}, map[uuid.UUID][]uuid.UUID{a.ID: {c.ID}, c.ID: {d.ID}})
	pos := map[uuid.UUID]int{}
	for i, w := range got {
		pos[w.ID] = i
	}
	if len(got) != 4 || pos[d.ID] > pos[c.ID] || pos[c.ID] > pos[a.ID] {
		t.Fatalf("order = %v", ids(got))
	}
	// No edges: unchanged.
	in := []*models.WorkItem{a, b}
	if out := orderByProducers(in, nil); out[0] != a || out[1] != b {
		t.Fatal("order changed without edges")
	}
	// A producer outside the children is ignored.
	if out := orderByProducers([]*models.WorkItem{a, b}, map[uuid.UUID][]uuid.UUID{a.ID: {uuid.New()}}); len(out) != 2 {
		t.Fatalf("order = %v", ids(out))
	}
	// A pre-056 database keeps the order.
	if out := orderProducersFirst(context.Background(), queryErr{&pgconn.PgError{Code: "42703"}}, []*models.WorkItem{a, b}); out[0] != a || out[1] != b {
		t.Fatal("order changed on a pre-056 database")
	}
}

func ids(ws []*models.WorkItem) []string {
	out := make([]string, 0, len(ws))
	for _, w := range ws {
		out = append(out, w.ID.String()[:8])
	}
	return out
}

func TestLinkProducerPRs_PostsTheLinkToGithubService(t *testing.T) {
	var got []linkPRRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/pr/link" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("request %s %s (%s)", r.Method, r.URL.Path, r.Header.Get("Content-Type"))
		}
		b, _ := io.ReadAll(r.Body)
		var req linkPRRequest
		if err := json.Unmarshal(b, &req); err != nil {
			t.Errorf("body %s: %v", b, err)
		}
		got = append(got, req)
		if req.PRNumber == 9 {
			http.Error(w, "boom", http.StatusInternalServerError) // logged, not fatal
			return
		}
		w.Write([]byte(`{"success":true}`)) //nolint:errcheck
	}))
	defer srv.Close()

	linkProducerPRs(context.Background(), srv.URL, []openedPR{
		{RepoID: "repo-api", RepoName: "api", URL: "https://github.com/acme/api/pull/7", Number: 7},
		{RepoID: "repo-lib", RepoName: "lib", URL: "https://github.com/acme/lib/pull/9", Number: 9},
	}, "https://github.com/acme/web/pull/3", "web")

	if len(got) != 2 || got[0].RepositoryID != "repo-api" || got[0].PRNumber != 7 || got[1].PRNumber != 9 {
		t.Fatalf("requests = %+v", got)
	}
	if !strings.Contains(got[0].Append, "**Paired consumer PR:** https://github.com/acme/web/pull/3 (web)") {
		t.Fatalf("append = %q", got[0].Append)
	}
}

// A consumer whose producer got no PR in the run is blocked naming the
// missing producer, not shipped alone.
func TestContractBlockReason_NamesAMissingProducerPR(t *testing.T) {
	reason := contractBlockReason("web", nil, nil, []string{"producer subtask 1234 (repository api) got no pull request in this run"})
	for _, want := range []string{
		"Contract producer missing: the pull request of web was not opened",
		"producer subtask 1234 (repository api) got no pull request in this run — the consumer depends on its contract change and does not ship without it",
	} {
		if !strings.Contains(reason, want) {
			t.Errorf("reason lacks %q:\n%s", want, reason)
		}
	}
}
