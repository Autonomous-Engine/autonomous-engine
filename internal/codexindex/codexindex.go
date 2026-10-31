// Package codexindex asks knowledge-indexer for a full index of one product
// (POST /api/index/full). dashboard-backend's Re-index (the admin page and
// the answer to a codex refusal) and status-orchestrator's re-post of a
// Re-index the indexer was too busy to take both use it.
package codexindex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
)

// DefaultURL is knowledge-indexer's in-network base URL.
const DefaultURL = "http://knowledge-indexer:8087"

// URLEnv overrides DefaultURL.
const URLEnv = "KNOWLEDGE_INDEXER_URL"

// URLFromEnv is URLEnv or DefaultURL.
func URLFromEnv() string {
	if v := strings.TrimSpace(os.Getenv(URLEnv)); v != "" {
		return strings.TrimRight(v, "/")
	}
	return DefaultURL
}

// Outcome classifies the indexer's answer to a full-index request.
type Outcome int

const (
	// Accepted: the index of the product started (it ends with a
	// codex.indexed event per version it writes).
	Accepted Outcome = iota
	// Busy: 409 — the indexer runs one index at a time for every product,
	// so nothing is indexing this product yet; post again later.
	Busy
	// Failed: any other answer, or no answer.
	Failed
)

// Result is one request's outcome, the HTTP status to report and the
// indexer's body or the transport error. Reached is false when the request
// never got an answer (Status is then 502, or 500 for a bad request).
type Result struct {
	Outcome Outcome
	Status  int
	Detail  string
	Reached bool
}

// RequestFull posts a full index of productID on branch ("" = each clone's
// checked-out branch) to baseURL.
func RequestFull(ctx context.Context, baseURL string, productID uuid.UUID, branch string) Result {
	params := url.Values{"product_id": {productID.String()}}
	if branch != "" {
		params.Set("branch", branch)
	}
	body, _ := json.Marshal(map[string]string{"product_id": productID.String(), "branch": branch})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+"/api/index/full?"+params.Encode(), bytes.NewReader(body))
	if err != nil {
		return Result{Outcome: Failed, Status: http.StatusInternalServerError, Detail: "build index request: " + err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return Result{Outcome: Failed, Status: http.StatusBadGateway, Detail: "knowledge-indexer unreachable: " + err.Error()}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	res := Result{Status: resp.StatusCode, Detail: strings.TrimSpace(string(raw)), Reached: true}
	switch {
	case resp.StatusCode == http.StatusConflict:
		res.Outcome = Busy
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		res.Outcome = Accepted
	default:
		res.Outcome = Failed
	}
	return res
}

// Error describes a Failed result.
func (r Result) Error() string {
	if !r.Reached {
		return r.Detail
	}
	return fmt.Sprintf("knowledge-indexer error %d: %s", r.Status, r.Detail)
}
