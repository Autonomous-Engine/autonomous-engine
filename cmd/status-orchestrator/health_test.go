package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/health"
)

// Services queried tables before the migrations had run and their one-shot
// startup jobs failed silently (F1). status-orchestrator applies the
// migrations, so its /health must stay unhealthy until they are applied;
// compose then holds the other services until it is healthy.
func TestHealthIsUnhealthyUntilMigrationsApplied(t *testing.T) {
	var migrated atomic.Bool
	checker := health.New("status-orchestrator")
	checker.AddCheck("migrations", migrationCheckOf(&migrated, new(atomic.Value)))

	status := func() (string, int) {
		rec := httptest.NewRecorder()
		checker.Handler()(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
		var body struct {
			Status string `json:"status"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return body.Status, rec.Code
	}

	if s, code := status(); s == "healthy" || code == http.StatusOK {
		t.Fatalf("before migrations: status=%q code=%d, want unhealthy", s, code)
	}
	migrated.Store(true)
	if s, code := status(); s != "healthy" || code != http.StatusOK {
		t.Fatalf("after migrations: status=%q code=%d, want healthy 200", s, code)
	}
}
