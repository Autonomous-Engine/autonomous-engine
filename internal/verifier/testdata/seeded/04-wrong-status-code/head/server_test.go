package items

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCreateItem(t *testing.T) {
	s := &Server{}
	rec := httptest.NewRecorder()
	s.CreateItem(rec, httptest.NewRequest(http.MethodPost, "/items", strings.NewReader(`{"name":"pen"}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rec.Code)
	}
	if len(s.items) != 1 {
		t.Fatalf("items = %v", s.items)
	}
}
