//go:build go1.23
// +build go1.23

package chi_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

// Held-out acceptance tests: handlers see the matched chi route pattern in
// http.Request.Pattern (Go 1.23+).

func benchServe(t *testing.T, r http.Handler, method, path string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func benchEchoPattern(w http.ResponseWriter, r *http.Request) {
	_, _ = w.Write([]byte(r.Pattern))
}

func TestBenchRequestPatternSetForMatchedRoute(t *testing.T) {
	cases := []struct {
		register func(chi.Router)
		method   string
		path     string
		want     string
	}{
		{func(r chi.Router) { r.Get("/orders/{orderID}", benchEchoPattern) }, "GET", "/orders/77", "/orders/{orderID}"},
		{func(r chi.Router) { r.Post("/teams/{team}/members/{member}", benchEchoPattern) }, "POST", "/teams/red/members/ann", "/teams/{team}/members/{member}"},
		{func(r chi.Router) { r.Get("/static/*", benchEchoPattern) }, "GET", "/static/css/site.css", "/static/*"},
		{func(r chi.Router) { r.Get("/health", benchEchoPattern) }, "GET", "/health", "/health"},
		{func(r chi.Router) { r.Get("/v/{id:[0-9]+}", benchEchoPattern) }, "GET", "/v/12", "/v/{id:[0-9]+}"},
	}
	for _, tc := range cases {
		r := chi.NewRouter()
		tc.register(r)
		code, body := benchServe(t, r, tc.method, tc.path)
		if code != http.StatusOK {
			t.Errorf("%s %s: status %d, want 200", tc.method, tc.path, code)
			continue
		}
		if body != tc.want {
			t.Errorf("%s %s: r.Pattern = %q, want %q", tc.method, tc.path, body, tc.want)
		}
	}
}

func TestBenchRequestPatternMatchesRoutePatternWithMiddleware(t *testing.T) {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { next.ServeHTTP(w, req) })
	})
	r.Get("/users/{userID}/posts/{postID}", func(w http.ResponseWriter, req *http.Request) {
		rp := chi.RouteContext(req.Context()).RoutePattern()
		if req.Pattern != rp {
			t.Errorf("r.Pattern = %q, chi RoutePattern() = %q; want them equal", req.Pattern, rp)
		}
		_, _ = w.Write([]byte(req.Pattern))
	})
	if _, body := benchServe(t, r, "GET", "/users/5/posts/9"); body != "/users/{userID}/posts/{postID}" {
		t.Errorf("r.Pattern = %q, want %q", body, "/users/{userID}/posts/{postID}")
	}
}

func TestBenchRequestPatternUnmatchedLeavesNotFound(t *testing.T) {
	r := chi.NewRouter()
	r.Get("/known", benchEchoPattern)
	if code, _ := benchServe(t, r, "GET", "/unknown"); code != http.StatusNotFound {
		t.Errorf("GET /unknown: status %d, want 404", code)
	}
}
