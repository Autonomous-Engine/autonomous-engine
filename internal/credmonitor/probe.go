package credmonitor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const GitHubAPI = "https://api.github.com"

// ProbeGitHub hits GET /user with the PAT and classifies the response.
// The token never appears in the returned Error string.
func ProbeGitHub(ctx context.Context, token string, timeout time.Duration) CheckResult {
	return probeGitHubAt(ctx, GitHubAPI, token, timeout)
}

// ProbeGitHubAt is ProbeGitHub against the API root base (a test server in
// another package's tests, or a GitHub Enterprise API root).
func ProbeGitHubAt(ctx context.Context, base, token string, timeout time.Duration) CheckResult {
	return probeGitHubAt(ctx, base, token, timeout)
}

// probeGitHubAt is the test-injectable form with a configurable base URL.
func probeGitHubAt(ctx context.Context, base, token string, timeout time.Duration) CheckResult {
	now := time.Now()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/user", nil)
	if err != nil {
		return CheckResult{Status: StatusNetworkError, Error: "request build: " + err.Error(), CheckedAt: now}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return CheckResult{Status: StatusNetworkError, Error: "network: " + err.Error(), CheckedAt: now}
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusOK:
		var exp *time.Time
		if raw := resp.Header.Get("github-authentication-token-expiration"); raw != "" {
			// GitHub format: "2030-01-15 12:00:00 UTC"
			if t, err := time.Parse("2006-01-02 15:04:05 MST", raw); err == nil {
				exp = &t
			}
		}
		return CheckResult{Status: StatusValid, ExpiresAt: exp, CheckedAt: now}
	case resp.StatusCode == http.StatusUnauthorized:
		return CheckResult{Status: StatusInvalid, Error: "HTTP 401 Unauthorized", CheckedAt: now}
	case resp.StatusCode >= 500:
		return CheckResult{Status: StatusNetworkError, Error: fmt.Sprintf("HTTP %d", resp.StatusCode), CheckedAt: now}
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		var msg struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(body, &msg)
		return CheckResult{Status: StatusInvalid, Error: fmt.Sprintf("HTTP %d: %s", resp.StatusCode, msg.Message), CheckedAt: now}
	}
}

// ProbeJira hits GET {base}/rest/api/3/myself with basic auth (email + API
// token). Classifies the response the same way ProbeGitHub does. The token
// never appears in the returned Error string.
func ProbeJira(ctx context.Context, base, email, apiToken string, timeout time.Duration) CheckResult {
	now := time.Now()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/rest/api/3/myself", nil)
	if err != nil {
		return CheckResult{Status: StatusNetworkError, Error: "request build: " + err.Error(), CheckedAt: now}
	}
	req.SetBasicAuth(email, apiToken)
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return CheckResult{Status: StatusNetworkError, Error: "network: " + err.Error(), CheckedAt: now}
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusOK:
		return CheckResult{Status: StatusValid, CheckedAt: now}
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return CheckResult{Status: StatusInvalid, Error: fmt.Sprintf("HTTP %d", resp.StatusCode), CheckedAt: now}
	case resp.StatusCode >= 500:
		return CheckResult{Status: StatusNetworkError, Error: fmt.Sprintf("HTTP %d", resp.StatusCode), CheckedAt: now}
	default:
		return CheckResult{Status: StatusInvalid, Error: fmt.Sprintf("HTTP %d", resp.StatusCode), CheckedAt: now}
	}
}
