package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

type fakeContractsStore struct {
	got *bool
	err error
}

func (f *fakeContractsStore) SetContractsStrict(_ context.Context, _ uuid.UUID, strict bool) error {
	f.got = &strict
	return f.err
}

func TestSetProductContractsStrict(t *testing.T) {
	id := uuid.NewString()
	cases := []struct {
		name   string
		method string
		body   any
		err    error
		want   int
	}{
		{"on", http.MethodPost, map[string]any{"product_id": id, "strict": true}, nil, http.StatusOK},
		{"off", http.MethodPost, map[string]any{"product_id": id, "strict": false}, nil, http.StatusOK},
		{"strict omitted", http.MethodPost, map[string]any{"product_id": id}, nil, http.StatusBadRequest},
		{"bad product", http.MethodPost, map[string]any{"product_id": "x", "strict": true}, nil, http.StatusBadRequest},
		{"unknown product", http.MethodPost, map[string]any{"product_id": id, "strict": true}, errProductNotFound, http.StatusNotFound},
		{"pre-056 database", http.MethodPost, map[string]any{"product_id": id, "strict": true}, errContractsColumnMissing, http.StatusServiceUnavailable},
		{"GET", http.MethodGet, nil, nil, http.StatusMethodNotAllowed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeContractsStore{err: tc.err}
			h := &AdminHandlers{contractsPolicy: store}
			var buf bytes.Buffer
			if tc.body != nil {
				_ = json.NewEncoder(&buf).Encode(tc.body)
			}
			rec := httptest.NewRecorder()
			h.SetProductContractsStrict(rec, httptest.NewRequest(tc.method, "/api/admin/products/contracts-strict", &buf))
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
			if tc.want == http.StatusOK && (store.got == nil || *store.got != tc.body.(map[string]any)["strict"]) {
				t.Fatalf("store got %v", store.got)
			}
		})
	}
}

func TestLoadContractEdges_NilDBIsEmpty(t *testing.T) {
	if got := loadContractEdges(context.Background(), nil, uuid.New()); got == nil || len(got) != 0 {
		t.Fatalf("got %v, want an empty array", got)
	}
}
