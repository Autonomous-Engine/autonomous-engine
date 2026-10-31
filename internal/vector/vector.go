package vector

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// Document represents a single item to be indexed in the vector store.
type Document struct {
	ID       string            `json:"id"`
	Content  string            `json:"content"`
	Metadata map[string]string `json:"metadata"`
	Vector   []float32         `json:"vector"`
}

// SearchResult is a single result returned from a vector similarity search.
type SearchResult struct {
	ID       string            `json:"id"`
	Score    float32           `json:"score"`
	Content  string            `json:"content"`
	Metadata map[string]string `json:"metadata"`
}

// VectorStore is a client for a Qdrant-compatible vector store REST API.
type VectorStore struct {
	endpoint   string
	collection string
	httpClient *http.Client
	embedding  *EmbeddingService // optional; nil means no auto-embedding
}

// New creates a new VectorStore client targeting the given endpoint and collection.
func New(endpoint, collection string) *VectorStore {
	return &VectorStore{
		endpoint:   endpoint,
		collection: collection,
		httpClient: &http.Client{},
	}
}

// NewWithEmbeddings creates a VectorStore that automatically generates
// embeddings for documents whose Vector field is nil, using the provided
// EmbeddingService. embeddingService may be nil, in which case behaviour is
// identical to New.
func NewWithEmbeddings(endpoint, collection string, embeddingService *EmbeddingService) *VectorStore {
	return &VectorStore{
		endpoint:   endpoint,
		collection: collection,
		httpClient: &http.Client{},
		embedding:  embeddingService,
	}
}

// EmbeddingService returns the EmbeddingService configured on this VectorStore,
// or nil if none was provided.
func (vs *VectorStore) EmbeddingService() *EmbeddingService {
	return vs.embedding
}

// ─── Collection Management ────────────────────────────────────────────────────

// CollectionInfo describes the vector configuration of an existing Qdrant
// collection. Exists == false means the collection has not been created yet.
type CollectionInfo struct {
	Exists     bool
	VectorSize int
}

// DescribeCollection fetches the target collection and returns its vector
// size. A 404 response from Qdrant is translated into Exists=false (no
// error), so callers can distinguish "collection missing" from "Qdrant down".
func (vs *VectorStore) DescribeCollection(ctx context.Context) (CollectionInfo, error) {
	url := fmt.Sprintf("%s/collections/%s", vs.endpoint, vs.collection)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return CollectionInfo{}, fmt.Errorf("build describe request: %w", err)
	}

	resp, err := vs.httpClient.Do(req)
	if err != nil {
		return CollectionInfo{}, fmt.Errorf("describe collection: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode == http.StatusNotFound {
		return CollectionInfo{Exists: false}, nil
	}
	if resp.StatusCode != http.StatusOK {
		return CollectionInfo{}, fmt.Errorf("describe collection: unexpected status %d", resp.StatusCode)
	}

	// Qdrant wraps everything in {"result": {...}}. The vector size lives at
	// result.config.params.vectors.size for unnamed-vector collections, or
	// result.config.params.vectors.<name>.size for named-vector ones. We
	// only use the unnamed form (see EnsureCollection) so we read the
	// simpler path and fall back to the named form if needed.
	var raw struct {
		Result struct {
			Config struct {
				Params struct {
					Vectors json.RawMessage `json:"vectors"`
				} `json:"params"`
			} `json:"config"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return CollectionInfo{}, fmt.Errorf("decode describe response: %w", err)
	}

	// Try the unnamed form: {"size": N, "distance": "..."}.
	var unnamed struct {
		Size int `json:"size"`
	}
	if err := json.Unmarshal(raw.Result.Config.Params.Vectors, &unnamed); err == nil && unnamed.Size > 0 {
		return CollectionInfo{Exists: true, VectorSize: unnamed.Size}, nil
	}

	// Fall back to the named form: {"<name>": {"size": N, ...}}. We only
	// expect one entry since we never create multi-vector collections.
	var named map[string]struct {
		Size int `json:"size"`
	}
	if err := json.Unmarshal(raw.Result.Config.Params.Vectors, &named); err == nil {
		for _, v := range named {
			if v.Size > 0 {
				return CollectionInfo{Exists: true, VectorSize: v.Size}, nil
			}
		}
	}

	return CollectionInfo{Exists: true, VectorSize: 0}, nil
}

// EnsureCollection creates the collection if it does not already exist.
// A 409 Conflict response is treated as a no-op (already exists).
func (vs *VectorStore) EnsureCollection(ctx context.Context, vectorSize int) error {
	body, err := json.Marshal(map[string]interface{}{
		"vectors": map[string]interface{}{
			"size":     vectorSize,
			"distance": "Cosine",
		},
	})
	if err != nil {
		return fmt.Errorf("marshal collection request: %w", err)
	}

	url := fmt.Sprintf("%s/collections/%s", vs.endpoint, vs.collection)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build ensure-collection request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := vs.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("ensure collection: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	// 200 = created, 409 = already exists — both are acceptable
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusConflict {
		return fmt.Errorf("ensure collection: unexpected status %d", resp.StatusCode)
	}
	return nil
}

// ─── Indexing ────────────────────────────────────────────────────────────────

// Index upserts the provided documents into the collection. If the VectorStore
// was created with NewWithEmbeddings and a document's Vector is nil, the
// embedding is generated automatically before indexing.
func (vs *VectorStore) Index(ctx context.Context, docs []Document) error {
	// Auto-generate embeddings for documents that don't have pre-computed vectors.
	if vs.embedding != nil {
		for i := range docs {
			if docs[i].Vector == nil {
				vec, err := vs.embedding.Embed(ctx, docs[i].Content)
				if err != nil {
					return fmt.Errorf("generate embedding for document %s: %w", docs[i].ID, err)
				}
				docs[i].Vector = vec
			}
		}
	}

	type point struct {
		ID      string            `json:"id"`
		Vector  []float32         `json:"vector"`
		Payload map[string]string `json:"payload"`
	}

	points := make([]point, len(docs))
	for i, d := range docs {
		payload := make(map[string]string, len(d.Metadata)+1)
		for k, v := range d.Metadata {
			payload[k] = v
		}
		payload["content"] = d.Content
		points[i] = point{
			ID:      d.ID,
			Vector:  d.Vector,
			Payload: payload,
		}
	}

	body, err := json.Marshal(map[string]interface{}{"points": points})
	if err != nil {
		return fmt.Errorf("marshal index request: %w", err)
	}

	url := fmt.Sprintf("%s/collections/%s/points", vs.endpoint, vs.collection)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build index request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := vs.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("index documents: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("index documents: unexpected status %d", resp.StatusCode)
	}
	return nil
}

// ─── Search ───────────────────────────────────────────────────────────────────

// Search performs a vector similarity search. If filters is non-nil, must-match
// key=value filters are added to the query. limit controls the maximum number of
// results returned.
func (vs *VectorStore) Search(ctx context.Context, vector []float32, filters map[string]string, limit int) ([]SearchResult, error) {
	query := map[string]interface{}{
		"vector": vector,
		"limit":  limit,
		"with_payload": true,
	}

	if len(filters) > 0 {
		must := make([]map[string]interface{}, 0, len(filters))
		for k, v := range filters {
			must = append(must, map[string]interface{}{
				"key":   k,
				"match": map[string]string{"value": v},
			})
		}
		query["filter"] = map[string]interface{}{"must": must}
	}

	body, err := json.Marshal(query)
	if err != nil {
		return nil, fmt.Errorf("marshal search request: %w", err)
	}

	url := fmt.Sprintf("%s/collections/%s/points/search", vs.endpoint, vs.collection)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build search request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := vs.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("search: unexpected status %d", resp.StatusCode)
	}

	var raw struct {
		Result []struct {
			ID      string            `json:"id"`
			Score   float32           `json:"score"`
			Payload map[string]string `json:"payload"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode search response: %w", err)
	}

	results := make([]SearchResult, 0, len(raw.Result))
	for _, r := range raw.Result {
		content := r.Payload["content"]
		metadata := make(map[string]string, len(r.Payload))
		for k, v := range r.Payload {
			if k == "content" {
				continue
			}
			metadata[k] = v
		}
		results = append(results, SearchResult{
			ID:       r.ID,
			Score:    r.Score,
			Content:  content,
			Metadata: metadata,
		})
	}
	return results, nil
}

// ─── Text Search ─────────────────────────────────────────────────────────────

// SearchByText embeds the query string using the EmbeddingService and then
// performs a vector similarity search. It requires the VectorStore to have been
// created with NewWithEmbeddings; if no EmbeddingService is configured an error
// is returned.
func (vs *VectorStore) SearchByText(ctx context.Context, query string, filters map[string]string, limit int) ([]SearchResult, error) {
	if vs.embedding == nil {
		return nil, fmt.Errorf("SearchByText requires an EmbeddingService; use NewWithEmbeddings")
	}

	vec, err := vs.embedding.Embed(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}

	return vs.Search(ctx, vec, filters, limit)
}

// ─── Count ────────────────────────────────────────────────────────────────────

// CountPoints returns the number of points currently stored in the collection.
// Returns 0 if the collection does not exist or the endpoint is unreachable.
func (vs *VectorStore) CountPoints(ctx context.Context) (int, error) {
	url := fmt.Sprintf("%s/collections/%s", vs.endpoint, vs.collection)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, fmt.Errorf("build count request: %w", err)
	}

	resp, err := vs.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("count points: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode == http.StatusNotFound {
		return 0, nil
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("count points: unexpected status %d", resp.StatusCode)
	}

	var raw struct {
		Result struct {
			PointsCount int `json:"points_count"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return 0, fmt.Errorf("decode count response: %w", err)
	}
	return raw.Result.PointsCount, nil
}

// ─── Sample (scroll) ─────────────────────────────────────────────────────────

// Sample returns up to limit payloads from the collection, using Qdrant's
// scroll endpoint. Intended for operator-facing sanity checks (e.g. "are
// chunks too small?" on startup). Returns SearchResults with zero scores —
// the method is random-order, not similarity-ranked.
func (vs *VectorStore) Sample(ctx context.Context, limit int) ([]SearchResult, error) {
	if limit <= 0 {
		limit = 16
	}
	body, err := json.Marshal(map[string]interface{}{
		"limit":        limit,
		"with_payload": true,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal scroll request: %w", err)
	}
	url := fmt.Sprintf("%s/collections/%s/points/scroll", vs.endpoint, vs.collection)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build scroll request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := vs.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("scroll: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("scroll: unexpected status %d", resp.StatusCode)
	}
	var raw struct {
		Result struct {
			Points []struct {
				ID      string            `json:"id"`
				Payload map[string]string `json:"payload"`
			} `json:"points"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode scroll response: %w", err)
	}
	out := make([]SearchResult, 0, len(raw.Result.Points))
	for _, p := range raw.Result.Points {
		content := p.Payload["content"]
		md := make(map[string]string, len(p.Payload))
		for k, v := range p.Payload {
			if k == "content" {
				continue
			}
			md[k] = v
		}
		out = append(out, SearchResult{ID: p.ID, Content: content, Metadata: md})
	}
	return out, nil
}

// ─── Delete ───────────────────────────────────────────────────────────────────

// Delete removes the points with the given IDs from the collection.
func (vs *VectorStore) Delete(ctx context.Context, ids []string) error {
	body, err := json.Marshal(map[string]interface{}{
		"points": ids,
	})
	if err != nil {
		return fmt.Errorf("marshal delete request: %w", err)
	}

	url := fmt.Sprintf("%s/collections/%s/points/delete", vs.endpoint, vs.collection)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build delete request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := vs.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("delete points: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("delete points: unexpected status %d", resp.StatusCode)
	}
	return nil
}
