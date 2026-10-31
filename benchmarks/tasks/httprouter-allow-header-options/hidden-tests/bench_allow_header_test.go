package httprouter

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Held-out acceptance tests: the Allow header only advertises OPTIONS when the
// router actually handles OPTIONS requests.

func benchNoopHandle(http.ResponseWriter, *http.Request, Params) {}

func benchNewRouter(handleOptions bool) *Router {
	r := New()
	r.HandleOPTIONS = handleOptions
	r.GET("/path", benchNoopHandle)
	r.POST("/path", benchNoopHandle)
	return r
}

func benchDo(r *Router, method, target string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(method, target, nil))
	return w
}

func TestBenchAllowHeaderOmitsOptionsWhenNotHandled(t *testing.T) {
	r := benchNewRouter(false)

	w := benchDo(r, http.MethodDelete, "/path")
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE /path: status %d, want 405", w.Code)
	}
	if got := w.Header().Get("Allow"); got != "GET, POST" {
		t.Fatalf("DELETE /path: Allow = %q, want %q", got, "GET, POST")
	}

	// An OPTIONS request is itself not handled, so it is a 405 with the same list.
	w = benchDo(r, http.MethodOptions, "/path")
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("OPTIONS /path: status %d, want 405", w.Code)
	}
	if got := w.Header().Get("Allow"); got != "GET, POST" {
		t.Fatalf("OPTIONS /path: Allow = %q, want %q", got, "GET, POST")
	}
}

func TestBenchServerWideAllowHeaderOmitsOptionsWhenNotHandled(t *testing.T) {
	r := benchNewRouter(false)

	w := benchDo(r, http.MethodOptions, "*")
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("OPTIONS *: status %d, want 405", w.Code)
	}
	if got := w.Header().Get("Allow"); got != "GET, POST" {
		t.Fatalf("OPTIONS *: Allow = %q, want %q", got, "GET, POST")
	}
}

func TestBenchAllowHeaderIncludesOptionsWhenHandled(t *testing.T) {
	r := benchNewRouter(true)

	w := benchDo(r, http.MethodDelete, "/path")
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE /path: status %d, want 405", w.Code)
	}
	if got := w.Header().Get("Allow"); got != "GET, OPTIONS, POST" {
		t.Fatalf("DELETE /path: Allow = %q, want %q", got, "GET, OPTIONS, POST")
	}

	w = benchDo(r, http.MethodOptions, "/path")
	if w.Code != http.StatusOK && w.Code != http.StatusNoContent {
		t.Fatalf("OPTIONS /path: status %d, want automatic OPTIONS response", w.Code)
	}
	if got := w.Header().Get("Allow"); got != "GET, OPTIONS, POST" {
		t.Fatalf("OPTIONS /path: Allow = %q, want %q", got, "GET, OPTIONS, POST")
	}

	w = benchDo(r, http.MethodOptions, "*")
	if got := w.Header().Get("Allow"); got != "GET, OPTIONS, POST" {
		t.Fatalf("OPTIONS *: Allow = %q, want %q", got, "GET, OPTIONS, POST")
	}
}
