package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/products"
	"github.com/autonomousengine/autonomous-engine/internal/verifypolicy"
	"github.com/google/uuid"
)

// fakePolicyBackend is an in-memory policyBackend.
type fakePolicyBackend struct {
	repos    map[uuid.UUID]*products.Repository
	stored   map[uuid.UUID]verifypolicy.Policy
	loadErr  error
	saveErr  error
	detected string
	clone    bool
	language string
}

func newFakePolicyBackend(repo *products.Repository) *fakePolicyBackend {
	return &fakePolicyBackend{
		repos:  map[uuid.UUID]*products.Repository{repo.ID: repo},
		stored: map[uuid.UUID]verifypolicy.Policy{},
	}
}

func (f *fakePolicyBackend) Repository(_ context.Context, id uuid.UUID) (*products.Repository, error) {
	r, ok := f.repos[id]
	if !ok {
		return nil, errors.New("not found")
	}
	return r, nil
}

func (f *fakePolicyBackend) Load(_ context.Context, id uuid.UUID) (verifypolicy.Policy, bool, error) {
	if f.loadErr != nil {
		return verifypolicy.Default(id), false, f.loadErr
	}
	p, ok := f.stored[id]
	if !ok {
		return verifypolicy.Default(id), false, nil
	}
	return p, true, nil
}

func (f *fakePolicyBackend) Save(_ context.Context, p verifypolicy.Policy) (verifypolicy.Policy, error) {
	if f.saveErr != nil {
		return verifypolicy.Policy{}, f.saveErr
	}
	// Mirror UpsertOnboardingTx's ownership rule.
	if cur, ok := f.stored[p.RepositoryID]; ok && cur.Source != verifypolicy.SourceOnboarding {
		return verifypolicy.Policy{}, verifypolicy.ErrNotOnboardingOwned
	}
	f.stored[p.RepositoryID] = p
	return p, nil
}

func (f *fakePolicyBackend) Detect(string) (string, bool, string) {
	return f.detected, f.clone, f.language
}

func policyRequest(t *testing.T, s *server, method, target string, body any) (*httptest.ResponseRecorder, verifypolicy.View) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if str, ok := body.(string); ok {
			buf.WriteString(str)
		} else if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	rec := httptest.NewRecorder()
	s.verificationPolicyHandler(rec, httptest.NewRequest(method, target, &buf))
	var view verifypolicy.View
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
			t.Fatalf("decode view: %v: %s", err, rec.Body.String())
		}
	}
	return rec, view
}

// TestVerificationPolicyStepProposesBlock: before the operator answers,
// the step shows the detected command and proposes the fail-closed policy
// with "block" preselected and language-seeded security paths.
func TestVerificationPolicyStepProposesBlock(t *testing.T) {
	repo := &products.Repository{ID: uuid.New(), Name: "svc", ClonePath: "/repo-cache/x/svc/"}
	be := newFakePolicyBackend(repo)
	be.detected, be.clone, be.language = "go test ./... -count=1", true, "go"
	s := &server{policyStore: be}

	rec, view := policyRequest(t, s, http.MethodGet, "/api/onboarding/verification-policy?repository_id="+repo.ID.String(), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d %s", rec.Code, rec.Body.String())
	}
	if view.Found || view.Policy.NoChecksPolicy != verifypolicy.NoChecksBlock || view.Policy.Source != verifypolicy.SourceDefault {
		t.Fatalf("proposal = %+v, want unstored default with block preselected", view)
	}
	if view.DetectedTestCommand != "go test ./... -count=1" || !view.CloneAvailable || view.Language != "go" {
		t.Fatalf("detection = (%q, %v, %q)", view.DetectedTestCommand, view.CloneAvailable, view.Language)
	}
	if !reflect.DeepEqual(view.Policy.SecurityReviewPaths, verifypolicy.SuggestedSecurityReviewPaths("go")) {
		t.Fatalf("proposed security paths = %v, want the go suggestions", view.Policy.SecurityReviewPaths)
	}
	if !reflect.DeepEqual(view.SelectableChecks, []string{"test", "build", "lint"}) {
		t.Fatalf("selectable checks = %v", view.SelectableChecks)
	}

	if rec, _ := policyRequest(t, s, http.MethodGet, "/api/onboarding/verification-policy?repository_id=nope", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("GET bad id = %d, want 400", rec.Code)
	}
	if rec, _ := policyRequest(t, s, http.MethodGet, "/api/onboarding/verification-policy?repository_id="+uuid.NewString(), nil); rec.Code != http.StatusNotFound {
		t.Fatalf("GET unknown repo = %d, want 404", rec.Code)
	}
	be.loadErr = errors.New("db down")
	if rec, _ := policyRequest(t, s, http.MethodGet, "/api/onboarding/verification-policy?repository_id="+repo.ID.String(), nil); rec.Code != http.StatusInternalServerError {
		t.Fatalf("GET with load error = %d, want 500", rec.Code)
	}
}

// TestVerificationPolicyStepRequiresExplicitAnswer: the no-checks question
// has no implicit answer; a valid answer is stored with source onboarding.
func TestVerificationPolicyStepRequiresExplicitAnswer(t *testing.T) {
	repo := &products.Repository{ID: uuid.New(), Name: "svc"}
	be := newFakePolicyBackend(repo)
	s := &server{policyStore: be}
	target := "/api/onboarding/verification-policy"

	rec, _ := policyRequest(t, s, http.MethodPost, target, map[string]any{"repository_id": repo.ID.String()})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "no_checks_policy is required") {
		t.Fatalf("POST without no_checks_policy = %d %s, want 400 naming the question", rec.Code, rec.Body.String())
	}
	for _, bad := range []map[string]any{
		{"repository_id": repo.ID.String(), "no_checks_policy": "allow"},
		{"repository_id": repo.ID.String(), "no_checks_policy": "block", "required_checks": []string{"typecheck"}},
		{"repository_id": repo.ID.String(), "no_checks_policy": "block", "security_review_paths": []string{"auth["}},
		{"repository_id": "x", "no_checks_policy": "block"},
	} {
		if rec, _ := policyRequest(t, s, http.MethodPost, target, bad); rec.Code != http.StatusBadRequest {
			t.Fatalf("POST %v = %d, want 400", bad, rec.Code)
		}
	}
	if rec, _ := policyRequest(t, s, http.MethodPost, target, "{"); rec.Code != http.StatusBadRequest {
		t.Fatalf("POST malformed JSON = %d, want 400", rec.Code)
	}
	if rec, _ := policyRequest(t, s, http.MethodPost, target, map[string]any{"repository_id": uuid.NewString(), "no_checks_policy": "block"}); rec.Code != http.StatusNotFound {
		t.Fatalf("POST unknown repo = %d, want 404", rec.Code)
	}
	if len(be.stored) != 0 {
		t.Fatalf("invalid requests stored %v", be.stored)
	}

	rec, view := policyRequest(t, s, http.MethodPost, target, map[string]any{
		"repository_id":         repo.ID.String(),
		"test_command":          "  make test ",
		"required_checks":       []string{"test", "lint"},
		"no_checks_policy":      "partial_with_human_review",
		"security_review_paths": []string{"auth*", "payments/**"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST = %d %s", rec.Code, rec.Body.String())
	}
	got := be.stored[repo.ID]
	if got.Source != verifypolicy.SourceOnboarding || got.SetBy != onboardingPolicySetBy {
		t.Fatalf("stored source/set_by = %q/%q", got.Source, got.SetBy)
	}
	if got.TestCommand == nil || *got.TestCommand != "make test" ||
		!reflect.DeepEqual(got.RequiredChecks, []evidence.CheckKind{"test", "lint"}) ||
		got.NoChecksPolicy != verifypolicy.NoChecksPartialWithHumanReview ||
		!reflect.DeepEqual(got.SecurityReviewPaths, []string{"auth*", "payments/**"}) {
		t.Fatalf("stored = %+v", got)
	}
	if !view.Found || view.Policy.NoChecksPolicy != verifypolicy.NoChecksPartialWithHumanReview {
		t.Fatalf("response view = %+v, want the stored policy", view)
	}

	be.saveErr = errors.New("db down")
	if rec, _ := policyRequest(t, s, http.MethodPost, target, map[string]any{"repository_id": repo.ID.String(), "no_checks_policy": "block"}); rec.Code != http.StatusInternalServerError {
		t.Fatalf("POST with save error = %d, want 500", rec.Code)
	}
	if rec, _ := policyRequest(t, s, http.MethodDelete, target, nil); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE = %d, want 405", rec.Code)
	}
}

// TestVerificationPolicyStepCannotOverwriteAdminPolicy: the unauthenticated
// onboarding route returns 409 for an admin-owned policy and leaves it
// unchanged; it may still update a policy onboarding itself wrote.
func TestVerificationPolicyStepCannotOverwriteAdminPolicy(t *testing.T) {
	repo := &products.Repository{ID: uuid.New(), Name: "svc"}
	be := newFakePolicyBackend(repo)
	cmd := "go test ./..."
	admin := verifypolicy.Policy{RepositoryID: repo.ID, TestCommand: &cmd, RequiredChecks: []evidence.CheckKind{"test"},
		NoChecksPolicy: verifypolicy.NoChecksBlock, SecurityReviewPaths: []string{"auth*"}, Source: verifypolicy.SourceAdmin, SetBy: "user:x"}
	be.stored[repo.ID] = admin
	s := &server{policyStore: be}
	target := "/api/onboarding/verification-policy"

	rec, _ := policyRequest(t, s, http.MethodPost, target, map[string]any{
		"repository_id": repo.ID.String(), "test_command": "true", "no_checks_policy": "partial_with_human_review",
	})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "administrator") {
		t.Fatalf("POST over an admin policy = %d %s, want 409 naming the administrator", rec.Code, rec.Body.String())
	}
	if !reflect.DeepEqual(be.stored[repo.ID], admin) {
		t.Fatalf("admin policy changed: %+v", be.stored[repo.ID])
	}

	own := &products.Repository{ID: uuid.New(), Name: "own"}
	be.repos[own.ID] = own
	for _, policy := range []string{"block", "partial_with_human_review"} {
		rec, _ := policyRequest(t, s, http.MethodPost, target, map[string]any{"repository_id": own.ID.String(), "no_checks_policy": policy})
		if rec.Code != http.StatusOK || string(be.stored[own.ID].NoChecksPolicy) != policy {
			t.Fatalf("POST %s on an onboarding-owned repo = %d, stored %q", policy, rec.Code, be.stored[own.ID].NoChecksPolicy)
		}
	}
}

// TestAddRepositoryWithPolicyRejectsBadAnswerBeforeInsert: an invalid
// verification_policy on add-repo is a 400-class input error and nothing is
// written (the server has no database; reaching it would panic).
func TestAddRepositoryWithPolicyRejectsBadAnswerBeforeInsert(t *testing.T) {
	s := &server{}
	_, err := s.addRepositoryWithPolicy(context.Background(), &verifypolicy.Input{}, uuid.New(), "r", "u", "main", "squash", "ae/")
	var inputErr *policyInputError
	if !errors.As(err, &inputErr) || !errors.Is(err, verifypolicy.ErrNoChecksPolicyRequired) {
		t.Fatalf("err = %v, want a policyInputError wrapping ErrNoChecksPolicyRequired", err)
	}
}

func TestDetectForPolicy(t *testing.T) {
	if cmd, ok, lang := detectForPolicy(""); cmd != "" || ok || lang != "" {
		t.Fatalf("empty path = (%q, %v, %q)", cmd, ok, lang)
	}
	dir := t.TempDir()
	if _, ok, _ := detectForPolicy(dir); ok {
		t.Fatal("empty clone reported available")
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "x_test.go"), []byte("package x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd, ok, lang := detectForPolicy(dir)
	if !ok || lang != "go" || !strings.HasPrefix(cmd, "go test") {
		t.Fatalf("go clone = (%q, %v, %q), want go test / go", cmd, ok, lang)
	}
}
