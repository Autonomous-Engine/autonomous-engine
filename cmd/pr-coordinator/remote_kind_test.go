package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/gitremote"
	"github.com/autonomousengine/autonomous-engine/internal/idempotency"
	"github.com/autonomousengine/autonomous-engine/internal/interactions"
	"github.com/autonomousengine/autonomous-engine/internal/products"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
)

func TestManualGateReason(t *testing.T) {
	cases := []struct {
		mode     string
		url      string
		readable bool
		want     string
	}{
		{"auto", "o/r", true, ""},
		{"auto", "https://github.com/o/r.git", true, ""},
		{"auto", "git@github.com:o/r.git", true, ""},
		{"auto", "/repo-cache/mirror.git", true, gateReasonNoGitHub},
		{"auto", "file:///repo-cache/mirror.git", true, gateReasonNoGitHub},
		{"auto", "https://gitlab.example/o/r.git", true, gateReasonNoGitHub},
		{"", "/repo-cache/mirror.git", true, gateReasonNoGitHub},
		{"manual", "o/r", true, gateReasonManual},
		{"manual", "/repo-cache/mirror.git", true, gateReasonManual},
		// The repository row could not be read: behaviour is unchanged.
		{"auto", "", false, ""},
	}
	for _, tc := range cases {
		kind, _ := gitremote.Classify(tc.url)
		noGitHub := tc.readable && !kind.IsGitHub()
		if got := manualGateReason(tc.mode, kind, noGitHub); got != tc.want {
			t.Errorf("manualGateReason(%q, %q) = %q, want %q", tc.mode, tc.url, got, tc.want)
		}
	}
}

// The gate decision is taken before github-service's PR creation, from the
// repository's github_url.
func TestPRLoop_ChecksRemoteKindBeforeGitHubAPI(t *testing.T) {
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	src := strings.ReplaceAll(string(b), "\r\n", "\n")
	sel := strings.Index(src, "SELECT base_branch, clone_path, name, github_url FROM repositories")
	gate := strings.Index(src, "gateReason := manualGateReason(prGateMode, repoKind, repoNoGitHub)")
	call := strings.Index(src, "prURL, prNumber := createGitHubPR(")
	if sel < 0 || gate < 0 || call < 0 || !(sel < gate && gate < call) {
		t.Fatalf("want the github_url lookup, then the gate decision, then createGitHubPR (positions %d, %d, %d)", sel, gate, call)
	}
}

// G1, against PostgreSQL: an auto-gate work item whose repository remote is
// not on GitHub (a local mirror, another host) never calls github-service;
// its branch becomes a pending gated change instead. Env-gated
// (TEST_POSTGRES_DSN).
func TestIntegration_NonGitHubRemoteRoutesThroughManualGate(t *testing.T) {
	pool := integrationPool(t)
	for _, remote := range []string{"/repo-cache/g1-mirror.git", "https://example.invalid/g1.git"} {
		t.Run(remote, func(t *testing.T) {
			ctx := context.Background()
			workID, taskID := seedGatedParent(t, pool, "implementation")
			if _, err := pool.Exec(ctx, `UPDATE work_items SET pr_gate_mode = 'auto' WHERE id = $1`, workID); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `UPDATE repositories SET github_url = $2 WHERE id = (SELECT repository_id FROM work_items WHERE id = $1)`, taskID, remote); err != nil {
				t.Fatal(err)
			}
			var githubCalls int32
			github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&githubCalls, 1)
				_, _ = w.Write([]byte(`{"pr_url":"https://example.invalid/pr/1","pr_number":1}`))
			}))
			defer github.Close()
			h := idempotency.NewLedger(pool).Handler(idempotencyConsumer, buildHandler(workgraph.New(pool),
				interactions.NewPublisher(nil), newTracker(), github.URL,
				products.NewStore(pool), evidence.NewStore(pool), evidence.CompatWindowLegacy))
			msg := completionMessage(t, pool, workID, taskID)
			msg.Value = verifiedCompletion(t, pool, workID, taskID)
			if err := h(ctx, msg); err != nil {
				t.Fatalf("delivery: %v", err)
			}
			if n := atomic.LoadInt32(&githubCalls); n != 0 {
				t.Errorf("github-service calls = %d, want 0", n)
			}
			if n := count(t, pool, `SELECT count(*) FROM pending_pr_changes WHERE work_item_id = $1`, workID); n != 1 {
				t.Errorf("pending PR rows = %d, want 1", n)
			}
		})
	}
}
