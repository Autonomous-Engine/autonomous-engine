package ratelimit

import (
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

// entry tracks request count within a sliding window for a single IP.
type entry struct {
	count       int
	windowStart time.Time
}

// IPRateLimiter is a simple in-memory, IP-based rate limiter using a fixed
// window algorithm. It is safe for concurrent use.
type IPRateLimiter struct {
	mu          sync.RWMutex
	entries     map[string]*entry
	maxRequests int
	window      time.Duration

	// shared, when set by UseRedis, decides requests from a window shared
	// with other replicas; nil means the in-process window only.
	shared *sharedWindow
}

// New creates an IPRateLimiter that allows up to maxRequests per IP within
// each window duration.
func New(maxRequests int, window time.Duration) *IPRateLimiter {
	return &IPRateLimiter{
		entries:     make(map[string]*entry),
		maxRequests: maxRequests,
		window:      window,
	}
}

// Allow reports whether the given IP is allowed to make another request.
// If the current window has expired, the counter is reset before checking.
// With UseRedis the window is shared with other replicas; when Redis cannot
// answer, the in-process window decides.
func (rl *IPRateLimiter) Allow(ip string) bool {
	if rl.shared != nil {
		if ok, decided := rl.shared.allow(ip, rl.maxRequests, rl.window); decided {
			return ok
		}
	}
	return rl.allowLocal(ip)
}

// allowLocal is the in-process fixed window.
func (rl *IPRateLimiter) allowLocal(ip string) bool {
	now := time.Now()

	rl.mu.Lock()
	defer rl.mu.Unlock()

	e, ok := rl.entries[ip]
	if !ok || now.Sub(e.windowStart) >= rl.window {
		// First request from this IP, or window has expired — start fresh.
		rl.entries[ip] = &entry{
			count:       1,
			windowStart: now,
		}
		return true
	}

	if e.count >= rl.maxRequests {
		return false
	}

	e.count++
	return true
}

// Middleware wraps an http.HandlerFunc with IP-based rate limiting.
// Denied requests receive HTTP 429 Too Many Requests with a Retry-After header.
func (rl *IPRateLimiter) Middleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := realIP(r)
		if !rl.Allow(ip) {
			retryAfter := int(rl.window.Seconds())
			w.Header().Set("Retry-After", fmt.Sprintf("%d", retryAfter))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"rate limit exceeded, please try again later"}`))
			return
		}
		next(w, r)
	}
}

// Cleanup runs indefinitely, evicting expired entries from the rate limiter
// map every window duration. It should be started in a goroutine.
func (rl *IPRateLimiter) Cleanup() {
	ticker := time.NewTicker(rl.window)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now()
		rl.mu.Lock()
		for ip, e := range rl.entries {
			if now.Sub(e.windowStart) >= rl.window {
				delete(rl.entries, ip)
			}
		}
		rl.mu.Unlock()
	}
}

// realIP extracts the client IP from the request, preferring the
// X-Forwarded-For header (first entry) when behind a proxy, falling back to
// RemoteAddr.
func realIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// X-Forwarded-For may contain a comma-separated list; take the first.
		for i := 0; i < len(xff); i++ {
			if xff[i] == ',' {
				xff = xff[:i]
				break
			}
		}
		// Trim spaces.
		for len(xff) > 0 && xff[0] == ' ' {
			xff = xff[1:]
		}
		for len(xff) > 0 && xff[len(xff)-1] == ' ' {
			xff = xff[:len(xff)-1]
		}
		if xff != "" {
			return xff
		}
	}

	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
