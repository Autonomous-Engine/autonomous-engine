package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

// fakeProductPolicyStore records SetPartialVerificationOptIn calls.
type fakeProductPolicyStore struct {
	err    error
	calls  []fakePolicyCall
	exists map[uuid.UUID]bool
}

type fakePolicyCall struct {
	productID uuid.UUID
	optIn     bool
}

func (s *fakeProductPolicyStore) SetPartialVerificationOptIn(_ context.Context, productID uuid.UUID, optIn bool) error {
	s.calls = append(s.calls, fakePolicyCall{productID, optIn})
	if s.err != nil {
		return s.err
	}
	if s.exists != nil && !s.exists[productID] {
		return errProductNotFound
	}
	return nil
}

func postPartialVerification(t *testing.T, h *AdminHandlers, method string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	switch b := body.(type) {
	case string:
		buf.WriteString(b)
	default:
		if err := json.NewEncoder(&buf).Encode(b); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, "/api/admin/products/partial-verification", &buf)
	rec := httptest.NewRecorder()
	h.SetProductPartialVerification(rec, req)
	return rec
}

func TestSetProductPartialVerification_FlipsFlag(t *testing.T) {
	productID := uuid.New()
	store := &fakeProductPolicyStore{exists: map[uuid.UUID]bool{productID: true}}
	h := &AdminHandlers{productPolicy: store}

	for _, optIn := range []bool{true, false} {
		rec := postPartialVerification(t, h, http.MethodPost, map[string]any{"product_id": productID.String(), "opt_in": optIn})
		if rec.Code != http.StatusOK {
			t.Fatalf("opt_in=%v: status = %d, body = %s", optIn, rec.Code, rec.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body["status"] != "ok" || body["product_id"] != productID.String() || body["partial_verification_opt_in"] != optIn {
			t.Fatalf("opt_in=%v: unexpected body %v", optIn, body)
		}
	}
	if len(store.calls) != 2 || store.calls[0] != (fakePolicyCall{productID, true}) || store.calls[1] != (fakePolicyCall{productID, false}) {
		t.Fatalf("store calls = %+v", store.calls)
	}
}

func TestSetProductPartialVerification_Errors(t *testing.T) {
	productID := uuid.New()
	cases := []struct {
		name      string
		method    string
		body      any
		store     ProductPolicyStore
		want      int
		wantCalls int
	}{
		{"method not allowed", http.MethodGet, map[string]any{"product_id": productID.String(), "opt_in": true}, &fakeProductPolicyStore{}, http.StatusMethodNotAllowed, 0},
		{"invalid json", http.MethodPost, "{not json", &fakeProductPolicyStore{}, http.StatusBadRequest, 0},
		{"missing product_id", http.MethodPost, map[string]any{"opt_in": true}, &fakeProductPolicyStore{}, http.StatusBadRequest, 0},
		{"invalid product_id", http.MethodPost, map[string]any{"product_id": "nope", "opt_in": true}, &fakeProductPolicyStore{}, http.StatusBadRequest, 0},
		{"missing opt_in is not a silent opt-out", http.MethodPost, map[string]any{"product_id": productID.String()}, &fakeProductPolicyStore{}, http.StatusBadRequest, 0},
		{"unknown product", http.MethodPost, map[string]any{"product_id": productID.String(), "opt_in": true}, &fakeProductPolicyStore{exists: map[uuid.UUID]bool{}}, http.StatusNotFound, 1},
		{"column missing before migration 045", http.MethodPost, map[string]any{"product_id": productID.String(), "opt_in": true}, &fakeProductPolicyStore{err: errPolicyColumnMissing}, http.StatusServiceUnavailable, 1},
		{"store failure", http.MethodPost, map[string]any{"product_id": productID.String(), "opt_in": true}, &fakeProductPolicyStore{err: errors.New("boom")}, http.StatusInternalServerError, 1},
		{"store not configured", http.MethodPost, map[string]any{"product_id": productID.String(), "opt_in": true}, nil, http.StatusInternalServerError, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &AdminHandlers{}
			if tc.store != nil {
				h.productPolicy = tc.store
			}
			rec := postPartialVerification(t, h, tc.method, tc.body)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.want, rec.Body.String())
			}
			var body map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["error"] == "" {
				t.Fatalf("expected error body, got %s", rec.Body.String())
			}
			if fs, ok := tc.store.(*fakeProductPolicyStore); ok && len(fs.calls) != tc.wantCalls {
				t.Fatalf("store calls = %d, want %d", len(fs.calls), tc.wantCalls)
			}
		})
	}
}

func TestSetProductPartialVerification_ColumnMissingMessageNamesMigration(t *testing.T) {
	h := &AdminHandlers{productPolicy: &fakeProductPolicyStore{err: errPolicyColumnMissing}}
	rec := postPartialVerification(t, h, http.MethodPost, map[string]any{"product_id": uuid.New().String(), "opt_in": true})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", rec.Code)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("migration 045")) {
		t.Fatalf("error should tell the operator which migration is missing: %s", rec.Body.String())
	}
}
