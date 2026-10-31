package credmonitor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ── ProbeGitHub ──────────────────────────────────────────────────────────────

func TestProbeGitHub_Valid(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ghp_valid" {
			t.Errorf("expected Bearer ghp_valid, got %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"login": "testuser"})
	}))
	defer srv.Close()

	res := probeGitHubAt(context.Background(), srv.URL, "ghp_valid", 5*time.Second)
	if res.Status != StatusValid {
		t.Fatalf("want valid, got %s (%s)", res.Status, res.Error)
	}
}

func TestProbeGitHub_ExpiryHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("github-authentication-token-expiration", "2030-01-15 12:00:00 UTC")
		_, _ = w.Write([]byte(`{"login":"x"}`))
	}))
	defer srv.Close()

	res := probeGitHubAt(context.Background(), srv.URL, "ghp_valid", 5*time.Second)
	if res.Status != StatusValid {
		t.Fatalf("want valid, got %s", res.Status)
	}
	if res.ExpiresAt == nil {
		t.Fatal("expected ExpiresAt to be parsed from header")
	}
	if res.ExpiresAt.Year() != 2030 {
		t.Fatalf("want 2030, got %v", res.ExpiresAt)
	}
}

func TestProbeGitHub_Unauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	res := probeGitHubAt(context.Background(), srv.URL, "bad", 5*time.Second)
	if res.Status != StatusInvalid {
		t.Fatalf("want invalid, got %s", res.Status)
	}
	if res.Error == "" {
		t.Fatal("expected Error message")
	}
}

func TestProbeGitHub_NetworkError(t *testing.T) {
	res := probeGitHubAt(context.Background(), "http://127.0.0.1:1/not-a-server", "x", 500*time.Millisecond)
	if res.Status != StatusNetworkError {
		t.Fatalf("want network_error, got %s", res.Status)
	}
}

func TestProbeGitHub_NeverLogsToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()
	token := "ghp_secret_do_not_leak_into_errors"
	res := probeGitHubAt(context.Background(), srv.URL, token, 1*time.Second)
	if res.Error == "" {
		t.Fatal("expected error")
	}
	if strings.Contains(res.Error, token) {
		t.Fatalf("token leaked into error: %s", res.Error)
	}
}

// ── ProbeJira ────────────────────────────────────────────────────────────────

func TestProbeJira_Valid(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rest/api/3/myself" {
			t.Errorf("wrong path: %s", r.URL.Path)
		}
		u, p, ok := r.BasicAuth()
		if !ok || u != "e@x.com" || p != "tok" {
			t.Errorf("basic auth wrong: u=%q p=%q ok=%v", u, p, ok)
		}
		_, _ = w.Write([]byte(`{"accountId":"abc"}`))
	}))
	defer srv.Close()

	res := ProbeJira(context.Background(), srv.URL, "e@x.com", "tok", 5*time.Second)
	if res.Status != StatusValid {
		t.Fatalf("want valid, got %s (%s)", res.Status, res.Error)
	}
}

func TestProbeJira_Unauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "denied", http.StatusUnauthorized)
	}))
	defer srv.Close()
	res := ProbeJira(context.Background(), srv.URL, "e@x.com", "bad", 5*time.Second)
	if res.Status != StatusInvalid {
		t.Fatalf("want invalid, got %s", res.Status)
	}
}

func TestProbeJira_NeverLogsToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "denied", http.StatusUnauthorized)
	}))
	defer srv.Close()
	secret := "secret_jira_token_should_never_leak"
	res := ProbeJira(context.Background(), srv.URL, "e@x.com", secret, 1*time.Second)
	if strings.Contains(res.Error, secret) {
		t.Fatalf("token leaked: %s", res.Error)
	}
}
