package health

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewCreatesChecker(t *testing.T) {
	c := New("test-service")
	if c == nil {
		t.Fatal("New returned nil")
	}
	if c.serviceName != "test-service" {
		t.Errorf("serviceName = %q, want %q", c.serviceName, "test-service")
	}
	if c.checks == nil {
		t.Error("checks map is nil")
	}
}

func TestAddCheckRegistersCheck(t *testing.T) {
	c := New("svc")
	c.AddCheck("db", func() error { return nil })

	c.mu.RLock()
	_, ok := c.checks["db"]
	c.mu.RUnlock()

	if !ok {
		t.Error("check 'db' was not registered")
	}
}

func TestHandlerReturns200WhenAllHealthy(t *testing.T) {
	c := New("svc")
	c.AddCheck("db", func() error { return nil })
	c.AddCheck("cache", func() error { return nil })

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()

	c.Handler()(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusOK)
	}

	var s Status
	if err := json.NewDecoder(rr.Body).Decode(&s); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if s.Status != "healthy" {
		t.Errorf("Status = %q, want %q", s.Status, "healthy")
	}
	if s.Service != "svc" {
		t.Errorf("Service = %q, want %q", s.Service, "svc")
	}
	if s.Checks["db"] != "healthy" {
		t.Errorf("checks[db] = %q, want %q", s.Checks["db"], "healthy")
	}
}

func TestHandlerReturns503WhenCheckFails(t *testing.T) {
	c := New("svc")
	c.AddCheck("db", func() error { return nil })
	c.AddCheck("kafka", func() error { return errors.New("connection refused") })

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()

	c.Handler()(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusServiceUnavailable)
	}

	var s Status
	if err := json.NewDecoder(rr.Body).Decode(&s); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if s.Status != "unhealthy" {
		t.Errorf("Status = %q, want %q", s.Status, "unhealthy")
	}
	if s.Checks["kafka"] != "connection refused" {
		t.Errorf("checks[kafka] = %q, want %q", s.Checks["kafka"], "connection refused")
	}
}

func TestHandlerNoChecks(t *testing.T) {
	c := New("empty-svc")

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()

	c.Handler()(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusOK)
	}

	var s Status
	if err := json.NewDecoder(rr.Body).Decode(&s); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if s.Status != "healthy" {
		t.Errorf("Status = %q, want %q", s.Status, "healthy")
	}
}

func TestHandlerContentTypeIsJSON(t *testing.T) {
	c := New("svc")
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()
	c.Handler()(rr, req)

	ct := rr.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("Content-Type = %q, want %q", ct, "application/json")
	}
}
