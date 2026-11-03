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

func stubLookup(u userRecord, found bool) userLookup {
	return func(context.Context, string) (userRecord, bool, error) { return u, found, nil }
}

func call(t *testing.T, a *AuthService, path string, next http.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+signedToken(t, "u1", "admin"))
	rec := httptest.NewRecorder()
	a.Middleware(next)(rec, req)
	return rec
}

// The middleware trusted the token's role without looking the user up, so
// a deleted, locked or demoted user kept their old access for the token's
// 8 hours (F3).
func TestMiddlewareUsesTheDatabaseRoleNotTheTokens(t *testing.T) {
	jwtSecret = []byte("test-secret")
	a := &AuthService{cacheTTL: 0, lookup: stubLookup(userRecord{Role: "viewer"}, true)} // demoted since issue
	var seenRole string
	rec := call(t, a, "/api/x", func(w http.ResponseWriter, r *http.Request) {
		seenRole, _ = r.Context().Value(contextKeyUserRole).(string)
	})
	if rec.Code != http.StatusOK || seenRole != "viewer" {
		t.Fatalf("code=%d role=%q, want 200 with the database role viewer", rec.Code, seenRole)
	}
}

func TestMiddlewareRejectsRemovedAndLockedUsers(t *testing.T) {
	jwtSecret = []byte("test-secret")
	for name, lookup := range map[string]userLookup{
		"removed": stubLookup(userRecord{}, false),
		"locked":  stubLookup(userRecord{Role: "admin", Locked: true}, true),
	} {
		t.Run(name, func(t *testing.T) {
			a := &AuthService{lookup: lookup}
			called := false
			rec := call(t, a, "/api/x", func(http.ResponseWriter, *http.Request) { called = true })
			if rec.Code != http.StatusUnauthorized || called {
				t.Fatalf("code=%d called=%v, want 401 and the handler skipped", rec.Code, called)
			}
		})
	}
}

// The first-run password change is enforced server-side (F2, review I4):
// while password_change_required is set, only the auth routes answer.
func TestMiddlewareHoldsEverythingButAuthUntilThePasswordIsChanged(t *testing.T) {
	jwtSecret = []byte("test-secret")
	a := &AuthService{lookup: stubLookup(userRecord{Role: "admin", MustChangePassword: true}, true)}
	if rec := call(t, a, "/api/products", func(http.ResponseWriter, *http.Request) {}); rec.Code != http.StatusForbidden {
		t.Fatalf("/api/products: %d %s, want 403 password change required", rec.Code, rec.Body.String())
	}
	for _, p := range []string{"/api/auth/change-password", "/api/auth/me", "/api/auth/logout"} {
		if rec := call(t, a, p, func(http.ResponseWriter, *http.Request) {}); rec.Code != http.StatusOK {
			t.Fatalf("%s: %d, want 200 (auth routes stay open)", p, rec.Code)
		}
	}
}

// The lookup is cached per user for cacheTTL; a TTL of 0 asks every time.
func TestMiddlewareCachesTheLookupForTTL(t *testing.T) {
	jwtSecret = []byte("test-secret")
	calls := 0
	a := &AuthService{cacheTTL: time.Minute, lookup: func(context.Context, string) (userRecord, bool, error) {
		calls++
		return userRecord{Role: "admin"}, true, nil
	}}
	for i := 0; i < 3; i++ {
		call(t, a, "/api/x", func(http.ResponseWriter, *http.Request) {})
	}
	if calls != 1 {
		t.Fatalf("lookup called %d times within the TTL, want 1", calls)
	}
	a.cacheTTL = 0
	call(t, a, "/api/x", func(http.ResponseWriter, *http.Request) {})
	if calls != 2 {
		t.Fatalf("lookup called %d times with TTL 0, want a fresh lookup", calls)
	}
}
