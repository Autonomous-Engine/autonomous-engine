package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/verifypolicy"
	"github.com/google/uuid"
)

type fakeRepoPolicyBackend struct {
	repos   map[uuid.UUID]string
	stored  map[uuid.UUID]verifypolicy.Policy
	lookErr error
	saveErr error
}

func (f *fakeRepoPolicyBackend) ClonePath(_ context.Context, id uuid.UUID) (string, error) {
	if f.lookErr != nil {
		return "", f.lookErr
	}
	p, ok := f.repos[id]
	if !ok {
		return "", errRepositoryNotFound
	}
	return p, nil
}

func (f *fakeRepoPolicyBackend) Load(_ context.Context, id uuid.UUID) (verifypolicy.Policy, bool, error) {
	p, ok := f.stored[id]
	if !ok {
		return verifypolicy.Default(id), false, nil
	}
	return p, true, nil
}

func (f *fakeRepoPolicyBackend) Save(_ context.Context, p verifypolicy.Policy) (verifypolicy.Policy, error) {
	if f.saveErr != nil {
		return verifypolicy.Policy{}, f.saveErr
	}
	f.stored[p.RepositoryID] = p
	return p, nil
}

func (f *fakeRepoPolicyBackend) Detect(string) (string, bool, string) {
	return "npm test", true, "node"
}

func repoPolicyRequest(t *testing.T, h *AdminHandlers, method, query string, body any, userID string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if s, ok := body.(string); ok {
			buf.WriteString(s)
		} else if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, "/api/admin/repositories/verification-policy"+query, &buf)
	if userID != "" {
		req = req.WithContext(context.WithValue(req.Context(), contextKeyUserID, userID))
	}
	rec := httptest.NewRecorder()
	h.RepositoryVerificationPolicy(rec, req)
	return rec
}

func TestRepositoryVerificationPolicy_GetShowsFailClosedDefault(t *testing.T) {
	id := uuid.New()
	be := &fakeRepoPolicyBackend{repos: map[uuid.UUID]string{id: "/repo-cache/p/r/"}, stored: map[uuid.UUID]verifypolicy.Policy{}}
	h := &AdminHandlers{repoPolicy: be}

	rec := repoPolicyRequest(t, h, http.MethodGet, "?id="+id.String(), nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d %s", rec.Code, rec.Body.String())
	}
	var view verifypolicy.View
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Found || view.Policy.NoChecksPolicy != verifypolicy.NoChecksBlock || view.DetectedTestCommand != "npm test" {
		t.Fatalf("view = %+v, want unstored block default with detection", view)
	}

	for _, tc := range []struct {
		query string
		want  int
	}{
		{"", http.StatusBadRequest},
		{"?id=nope", http.StatusBadRequest},
		{"?id=" + uuid.NewString(), http.StatusNotFound},
	} {
		if rec := repoPolicyRequest(t, h, http.MethodGet, tc.query, nil, ""); rec.Code != tc.want {
			t.Errorf("GET %q = %d, want %d", tc.query, rec.Code, tc.want)
		}
	}
	be.lookErr = errors.New("db down")
	if rec := repoPolicyRequest(t, h, http.MethodGet, "?id="+id.String(), nil, ""); rec.Code != http.StatusInternalServerError {
		t.Errorf("GET with lookup error = %d, want 500", rec.Code)
	}
}

func TestRepositoryVerificationPolicy_PutStoresAdminPolicy(t *testing.T) {
	id := uuid.New()
	be := &fakeRepoPolicyBackend{repos: map[uuid.UUID]string{id: ""}, stored: map[uuid.UUID]verifypolicy.Policy{}}
	h := &AdminHandlers{repoPolicy: be}
	user := uuid.NewString()

	if rec := repoPolicyRequest(t, h, http.MethodPut, "?id="+id.String(), map[string]any{}, user); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "no_checks_policy is required") {
		t.Fatalf("PUT without no_checks_policy = %d %s, want 400", rec.Code, rec.Body.String())
	}
	if rec := repoPolicyRequest(t, h, http.MethodPut, "?id="+id.String(), map[string]any{"no_checks_policy": "block", "required_checks": []string{"diagnostic"}}, user); rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT with diagnostic required = %d, want 400", rec.Code)
	}
	if rec := repoPolicyRequest(t, h, http.MethodPut, "?id="+id.String(), "{", user); rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT malformed = %d, want 400", rec.Code)
	}
	if len(be.stored) != 0 {
		t.Fatalf("invalid PUTs stored %v", be.stored)
	}

	rec := repoPolicyRequest(t, h, http.MethodPut, "?id="+id.String(), map[string]any{
		"test_command":     "npm run test:ci",
		"required_checks":  []string{"test", "build"},
		"no_checks_policy": "partial_with_human_review",
	}, user)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d %s", rec.Code, rec.Body.String())
	}
	got := be.stored[id]
	if got.Source != verifypolicy.SourceAdmin || got.SetBy != "user:"+user {
		t.Fatalf("source/set_by = %q/%q", got.Source, got.SetBy)
	}
	if !reflect.DeepEqual(got.RequiredChecks, []evidence.CheckKind{"test", "build"}) || *got.TestCommand != "npm run test:ci" {
		t.Fatalf("stored = %+v", got)
	}
	if !reflect.DeepEqual(got.SecurityReviewPaths, verifypolicy.GenericSecurityReviewPaths) {
		t.Fatalf("omitted security paths stored as %v, want the generic defaults", got.SecurityReviewPaths)
	}

	be.saveErr = errors.New("db down")
	if rec := repoPolicyRequest(t, h, http.MethodPut, "?id="+id.String(), map[string]any{"no_checks_policy": "block"}, ""); rec.Code != http.StatusInternalServerError {
		t.Fatalf("PUT with save error = %d, want 500", rec.Code)
	}
	if rec := repoPolicyRequest(t, h, http.MethodDelete, "?id="+id.String(), nil, ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE = %d, want 405", rec.Code)
	}
}
