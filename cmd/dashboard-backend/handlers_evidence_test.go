package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/google/uuid"
)

// fakeEvidenceStore implements EvidenceStore in memory. rows are returned in
// insertion order, so tests insert newest first to mirror the real store.
type fakeEvidenceStore struct {
	rows  map[uuid.UUID][]evidence.Verification
	err   error
	calls int
}

func newFakeEvidenceStore() *fakeEvidenceStore {
	return &fakeEvidenceStore{rows: map[uuid.UUID][]evidence.Verification{}}
}

func (s *fakeEvidenceStore) GetByWorkItem(_ context.Context, workID uuid.UUID) ([]evidence.Verification, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	if workID == uuid.Nil {
		return nil, evidence.ErrNotFound
	}
	out := []evidence.Verification{}
	out = append(out, s.rows[workID]...)
	return out, nil
}

func intPtr(i int) *int { return &i }

// partialEvidence builds a partially_verified record with one passed and one
// skipped check, the case D2 requires the dashboard to make visible.
func partialEvidence(workID uuid.UUID) evidence.ImplementationEvidence {
	started := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	finished := started.Add(90 * time.Second)
	return evidence.ImplementationEvidence{
		SchemaVersion: evidence.SchemaVersion,
		WorkID:        workID.String(),
		TaskID:        "task-1",
		BaseSHA:       "0123456789abcdef0123456789abcdef01234567",
		HeadSHA:       "89abcdef0123456789abcdef0123456789abcdef",
		Branch:        "ae/feature-x",
		ChangedFiles:  []string{"internal/a.go", "internal/a_test.go"},
		Checks: []evidence.CheckResult{
			{Kind: evidence.CheckKindBuild, Command: "go build ./...", ExitCode: intPtr(0), Status: evidence.CheckPassed,
				StartedAt: &started, FinishedAt: &finished, OutputRef: "https://artifacts.example/build.log"},
			{Kind: evidence.CheckKindTest, Command: "go test ./...", Status: evidence.CheckSkipped,
				SkipReason: "no test runner configured for repository"},
		},
		Outcome:      evidence.OutcomePartiallyVerified,
		StartedAt:    started,
		FinishedAt:   finished,
		ArtifactRefs: []string{"artifact:diff-1"},
	}
}

func TestListWorkItemEvidence_ReturnsRowsNewestFirstWithSkipReason(t *testing.T) {
	store := newFakeEvidenceStore()
	workID := uuid.New()
	newest := evidence.Verification{ID: uuid.New(), CreatedAt: time.Date(2026, 9, 10, 13, 0, 0, 0, time.UTC), Evidence: partialEvidence(workID)}
	older := evidence.Verification{ID: uuid.New(), CreatedAt: time.Date(2026, 9, 10, 11, 0, 0, 0, time.UTC), Evidence: partialEvidence(workID)}
	older.Evidence.Outcome = evidence.OutcomeUnverified
	older.Evidence.Checks = nil
	store.rows[workID] = []evidence.Verification{newest, older}

	h := &Handlers{evidence: store}
	req := httptest.NewRequest(http.MethodGet, "/api/work-items/evidence?id="+workID.String(), nil)
	rec := httptest.NewRecorder()
	h.ListWorkItemEvidence(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body) != 2 {
		t.Fatalf("len = %d, want 2", len(body))
	}
	if body[0]["id"] != newest.ID.String() || body[1]["id"] != older.ID.String() {
		t.Fatalf("order not preserved newest-first: %v", body)
	}
	ev, _ := body[0]["evidence"].(map[string]any)
	if ev == nil {
		t.Fatalf("evidence object missing: %v", body[0])
	}
	if ev["outcome"] != "partially_verified" {
		t.Errorf("outcome = %v", ev["outcome"])
	}
	if ev["head_sha"] != newest.Evidence.HeadSHA || ev["base_sha"] != newest.Evidence.BaseSHA {
		t.Errorf("sha fields not exposed verbatim: %v", ev)
	}
	checks, _ := ev["checks"].([]any)
	if len(checks) != 2 {
		t.Fatalf("checks = %v", ev["checks"])
	}
	skipped, _ := checks[1].(map[string]any)
	if skipped["status"] != "skipped" || skipped["skip_reason"] != "no test runner configured for repository" {
		t.Errorf("skipped check not exposed with reason: %v", skipped)
	}
	if skipped["exit_code"] != nil {
		t.Errorf("skipped exit_code should be null, got %v", skipped["exit_code"])
	}
	passed, _ := checks[0].(map[string]any)
	if passed["exit_code"] != float64(0) || passed["output_ref"] != "https://artifacts.example/build.log" {
		t.Errorf("passed check fields: %v", passed)
	}
	if _, ok := body[0]["created_at"]; !ok {
		t.Errorf("created_at missing: %v", body[0])
	}
}

func TestListWorkItemEvidence_EmptyIsEmptyArray(t *testing.T) {
	h := &Handlers{evidence: newFakeEvidenceStore()}
	req := httptest.NewRequest(http.MethodGet, "/api/work-items/evidence?id="+uuid.New().String(), nil)
	rec := httptest.NewRecorder()
	h.ListWorkItemEvidence(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Body.String(); got != "[]\n" {
		t.Fatalf("body = %q, want empty JSON array", got)
	}
}

func TestListWorkItemEvidence_Errors(t *testing.T) {
	cases := []struct {
		name   string
		method string
		query  string
		store  EvidenceStore
		want   int
	}{
		{"method not allowed", http.MethodPost, "?id=" + uuid.New().String(), newFakeEvidenceStore(), http.StatusMethodNotAllowed},
		{"missing id", http.MethodGet, "", newFakeEvidenceStore(), http.StatusBadRequest},
		{"invalid id", http.MethodGet, "?id=not-a-uuid", newFakeEvidenceStore(), http.StatusBadRequest},
		{"nil uuid is not found", http.MethodGet, "?id=" + uuid.Nil.String(), newFakeEvidenceStore(), http.StatusNotFound},
		{"store failure", http.MethodGet, "?id=" + uuid.New().String(), &fakeEvidenceStore{err: errors.New("boom")}, http.StatusInternalServerError},
		{"store not configured", http.MethodGet, "?id=" + uuid.New().String(), nil, http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &Handlers{}
			if tc.store != nil {
				h.evidence = tc.store
			}
			req := httptest.NewRequest(tc.method, "/api/work-items/evidence"+tc.query, nil)
			rec := httptest.NewRecorder()
			h.ListWorkItemEvidence(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.want, rec.Body.String())
			}
			var body map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["error"] == "" {
				t.Fatalf("expected error body, got %s", rec.Body.String())
			}
		})
	}
}

func TestLatestEvidence(t *testing.T) {
	workID := uuid.New()
	newest := evidence.Verification{ID: uuid.New(), CreatedAt: time.Now(), Evidence: partialEvidence(workID)}
	older := evidence.Verification{ID: uuid.New(), CreatedAt: time.Now().Add(-time.Hour), Evidence: partialEvidence(workID)}

	t.Run("returns first row only", func(t *testing.T) {
		store := newFakeEvidenceStore()
		store.rows[workID] = []evidence.Verification{newest, older}
		got := (&Handlers{evidence: store}).latestEvidence(context.Background(), workID)
		if got == nil || got.ID != newest.ID.String() {
			t.Fatalf("got %+v, want newest %s", got, newest.ID)
		}
		if got.Evidence.Outcome != evidence.OutcomePartiallyVerified {
			t.Errorf("outcome = %s", got.Evidence.Outcome)
		}
	})
	t.Run("nil when no rows", func(t *testing.T) {
		if got := (&Handlers{evidence: newFakeEvidenceStore()}).latestEvidence(context.Background(), workID); got != nil {
			t.Fatalf("got %+v, want nil", got)
		}
	})
	t.Run("nil when store unset", func(t *testing.T) {
		if got := (&Handlers{}).latestEvidence(context.Background(), workID); got != nil {
			t.Fatalf("got %+v, want nil", got)
		}
	})
	t.Run("nil and non-fatal on store error", func(t *testing.T) {
		store := &fakeEvidenceStore{err: errors.New("db down")}
		if got := (&Handlers{evidence: store}).latestEvidence(context.Background(), workID); got != nil {
			t.Fatalf("got %+v, want nil", got)
		}
	})
	t.Run("serialises as null when absent", func(t *testing.T) {
		var v *verificationRow
		b, _ := json.Marshal(map[string]any{"evidence": v})
		if string(b) != `{"evidence":null}` {
			t.Fatalf("got %s", b)
		}
	})
}
