package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func signedToken(t *testing.T, sub, role string) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": sub, "role": role, "exp": time.Now().Add(time.Hour).Unix(),
	})
	s, err := tok.SignedString(jwtSecret)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// The middleware trusted the token's role without looking the user up, so
// a deleted, locked or demoted user kept their old access for the token's
// 8 hours (F3).
func TestMiddlewareUsesTheDatabaseRoleNotTheTokens(t *testing.T) {
	jwtSecret = []byte("test-secret")
	a := &AuthService{cacheTTL: 0, lookup: func(context.Context, string) (string, bool, bool, error) {
		return "viewer", false, true, nil // demoted since the token was issued
	}}
	var seenRole string
	h := a.Middleware(func(w http.ResponseWriter, r *http.Request) {
		seenRole, _ = r.Context().Value(contextKeyUserRole).(string)
	})
	req := httptest.NewRequest(http.MethodGet, "/api/x", nil)
	req.Header.Set("Authorization", "Bearer "+signedToken(t, "u1", "admin"))
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusOK || seenRole != "viewer" {
		t.Fatalf("code=%d role=%q, want 200 with the database role viewer", rec.Code, seenRole)
	}
}

func TestMiddlewareRejectsRemovedAndLockedUsers(t *testing.T) {
	jwtSecret = []byte("test-secret")
	for name, lookup := range map[string]userLookup{
		"removed": func(context.Context, string) (string, bool, bool, error) { return "", false, false, nil },
		"locked":  func(context.Context, string) (string, bool, bool, error) { return "admin", true, true, nil },
	} {
		t.Run(name, func(t *testing.T) {
			a := &AuthService{lookup: lookup}
			called := false
			h := a.Middleware(func(http.ResponseWriter, *http.Request) { called = true })
			req := httptest.NewRequest(http.MethodGet, "/api/x", nil)
			req.Header.Set("Authorization", "Bearer "+signedToken(t, "u1", "admin"))
			rec := httptest.NewRecorder()
			h(rec, req)
			if rec.Code != http.StatusUnauthorized || called {
				t.Fatalf("code=%d called=%v, want 401 and the handler skipped", rec.Code, called)
			}
		})
	}
}

// The lookup is cached per user for cacheTTL; a TTL of 0 asks every time.
func TestMiddlewareCachesTheLookupForTTL(t *testing.T) {
	jwtSecret = []byte("test-secret")
	calls := 0
	a := &AuthService{cacheTTL: time.Minute, lookup: func(context.Context, string) (string, bool, bool, error) {
		calls++
		return "admin", false, true, nil
	}}
	h := a.Middleware(func(http.ResponseWriter, *http.Request) {})
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodGet, "/api/x", nil)
		req.Header.Set("Authorization", "Bearer "+signedToken(t, "u1", "admin"))
		h(httptest.NewRecorder(), req)
	}
	if calls != 1 {
		t.Fatalf("lookup called %d times within the TTL, want 1", calls)
	}
	a.cacheTTL = 0
	req := httptest.NewRequest(http.MethodGet, "/api/x", nil)
	req.Header.Set("Authorization", "Bearer "+signedToken(t, "u1", "admin"))
	h(httptest.NewRecorder(), req)
	if calls != 2 {
		t.Fatalf("lookup called %d times with TTL 0, want a fresh lookup", calls)
	}
}
