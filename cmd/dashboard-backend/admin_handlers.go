package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/agentregistry"
	"github.com/autonomousengine/autonomous-engine/internal/configstore"
	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
	"github.com/autonomousengine/autonomous-engine/internal/llm/profile"
	"github.com/autonomousengine/autonomous-engine/internal/products"
	"github.com/autonomousengine/autonomous-engine/internal/prompts"
	"github.com/autonomousengine/autonomous-engine/internal/rollout"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AdminHandlers groups all HTTP handlers for the /api/admin/* routes. It
// wraps a *configstore.ConfigStore to access credential and config operations.
type AdminHandlers struct {
	store         *configstore.ConfigStore
	db            *pgxpool.Pool
	promptStore   *prompts.PromptStore
	agentRegistry *agentregistry.Registry
	// productPolicy persists per-product verification policy (D2 opt-in).
	productPolicy ProductPolicyStore
	// contractsPolicy persists products.contracts_strict (M2-06).
	contractsPolicy ContractsStrictStore
	// repoPolicy overrides the repository verification-policy backend
	// (tests); nil means the database-backed one (M3-03).
	repoPolicy repositoryPolicyBackend
}

// NewAdminHandlers returns an AdminHandlers backed by the given ConfigStore and
// database pool.
func NewAdminHandlers(store *configstore.ConfigStore, db *pgxpool.Pool) *AdminHandlers {
	return &AdminHandlers{
		store:           store,
		db:              db,
		promptStore:     prompts.NewStore(db),
		agentRegistry:   agentregistry.New(db),
		productPolicy:   &pgProductPolicyStore{db: db},
		contractsPolicy: &pgProductPolicyStore{db: db},
	}
}

// ─── helpers ──────────────────────────────────────────────────────────────────

// guardCredential runs the M4-06 guard for a credential write that sets
// the global model implementation-agent resolves (llm provider /
// model_name / endpoint at global/default or agent_type/
// implementation-agent, unless a more specific row shadows it). The value
// is encrypted, so the "after" configuration is captured with the new value
// overlaid. F2: write is the credential write on the guard's transaction
// (configstore SetCredentialTx / RotateCredentialTx), so the credential,
// its credential_audit_log row and any override audit row commit together
// under the guard's advisory lock, or none of them does.
//
// guarded reports whether the guard handled the write; ok whether the
// caller may go on. guarded=false, ok=true: the credential does not shape
// the unit and the caller performs the plain (unguarded) write. ok=false:
// the response has been written (refused, or the write failed and nothing
// was changed).
func (h *AdminHandlers) guardCredential(w http.ResponseWriter, r *http.Request, action, scope, target, category, key, value string, write func(ctx context.Context, tx pgx.Tx) error) (guarded, ok bool) {
	overlay := rollout.OverlayForCredential(category, key, value)
	if overlay == nil {
		return false, true
	}
	if h.db == nil {
		writeError(w, http.StatusInternalServerError, "configuration guard unavailable")
		return false, false
	}
	shapes, err := rollout.CredentialShapesUnit(r.Context(), h.db, scope, target, category, key)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return false, false
	}
	if !shapes {
		return false, true
	}
	if !guardedChange(w, r, h.guardDB(), credentialChange(action, scope, target, category, key, overlay, write)) {
		return true, false
	}
	// The write committed: drop the local cache entry again (F2). The one
	// dropped inside the transaction can be refilled with the old value by
	// a read before the commit, and a LISTEN outage loses config_changed.
	if h.store != nil {
		h.store.InvalidateCached(scope, target, key, category)
	}
	return true, true
}

// credentialChange is the guarded change for a credential write: write is
// Apply (the real write, on the guard's transaction) and overlay stands in
// for the encrypted value in the "after" capture.
func credentialChange(action, scope, target, category, key string, overlay *rollout.GlobalModelOverlay, write func(ctx context.Context, tx pgx.Tx) error) rollout.DefaultChange {
	return rollout.DefaultChange{
		Action: action, Apply: write, Overlay: overlay,
		Details: map[string]any{"scope": scope, "scope_target": target, "category": category, "key_name": key},
	}
}

// promptNotFound maps the prompt store's not-found error to a 404.
func promptNotFound(err error) error {
	if errors.Is(err, prompts.ErrNotFound) {
		return notFound("prompt template not found")
	}
	return err
}

// guardDB is the pool as the M4-06 configuration guard's database (nil
// when there is none, which the guard refuses).
func (h *AdminHandlers) guardDB() guardBeginner {
	if h.db == nil {
		return nil
	}
	return h.db
}

// actorIDFromCtx extracts the user ID placed in ctx by auth.Middleware and
// returns it as a *uuid.UUID. Returns nil if absent or unparseable.
func actorIDFromCtx(r *http.Request) *uuid.UUID {
	raw, _ := r.Context().Value(contextKeyUserID).(string)
	if raw == "" {
		return nil
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return nil
	}
	return &id
}

// ─── Credential handlers ──────────────────────────────────────────────────────

// ListCredentials handles GET /api/admin/credentials
// Optional query params: ?category= and ?scope=
func (h *AdminHandlers) ListCredentials(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	category := r.URL.Query().Get("category")
	scope := r.URL.Query().Get("scope")

	creds, err := h.store.ListCredentials(r.Context(), category, scope)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Return an empty JSON array rather than null when there are no results.
	if creds == nil {
		creds = []*configstore.CredentialEntry{}
	}
	writeJSON(w, http.StatusOK, creds)
}

// GetCredential handles GET /api/admin/credentials/detail?id=<uuid>
func (h *AdminHandlers) GetCredential(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	idStr := r.URL.Query().Get("id")
	if idStr == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	credID, err := uuid.Parse(idStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}

	// ListCredentials with no filter, then find by ID. This reuses the existing
	// masked-listing path so secret values are never exposed.
	all, err := h.store.ListCredentials(r.Context(), "", "")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	for _, c := range all {
		if c.ID == credID {
			writeJSON(w, http.StatusOK, c)
			return
		}
	}
	writeError(w, http.StatusNotFound, "credential not found")
}

// revealRequest is the JSON body accepted by RevealCredential.
type revealRequest struct {
	Password string `json:"password"`
}

// RevealCredential handles POST /api/admin/credentials/reveal?id=<uuid>
// Body: {"password":"..."}  (re-auth check — password field is required but
// validation against the user's stored hash is deferred to future work; for
// now we require the field to be non-empty as a basic gate).
func (h *AdminHandlers) RevealCredential(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	idStr := r.URL.Query().Get("id")
	if idStr == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	credID, err := uuid.Parse(idStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}

	var req revealRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Password == "" {
		writeError(w, http.StatusBadRequest, "password is required for reveal")
		return
	}

	actorID := actorIDFromCtx(r)
	plaintext, err := h.store.RevealCredential(r.Context(), credID, actorID, r.RemoteAddr)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"value": plaintext})
}

// createCredentialRequest is the JSON body accepted by CreateCredential.
type createCredentialRequest struct {
	Scope       string `json:"scope"`
	ScopeTarget string `json:"scope_target"`
	Category    string `json:"category"`
	KeyName     string `json:"key_name"`
	Value       string `json:"value"`
	IsSecret    bool   `json:"is_secret"`
	Description string `json:"description"`
}

// CreateCredential handles POST /api/admin/credentials/create
func (h *AdminHandlers) CreateCredential(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req createCredentialRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	// Validate required fields.
	if req.Scope == "" {
		writeError(w, http.StatusBadRequest, "scope is required")
		return
	}
	if req.Category == "" {
		writeError(w, http.StatusBadRequest, "category is required")
		return
	}
	if req.KeyName == "" {
		writeError(w, http.StatusBadRequest, "key_name is required")
		return
	}
	if req.Value == "" {
		writeError(w, http.StatusBadRequest, "value is required")
		return
	}

	actorID := actorIDFromCtx(r)
	guarded, ok := h.guardCredential(w, r, "credentials.create", req.Scope, req.ScopeTarget, req.Category, req.KeyName, req.Value,
		func(ctx context.Context, tx pgx.Tx) error {
			if h.store == nil {
				return errors.New("credential store unavailable")
			}
			return h.store.SetCredentialTx(ctx, tx,
				req.Scope, req.ScopeTarget, req.Category, req.KeyName, req.Value,
				req.IsSecret, req.Description, actorID)
		})
	if !ok {
		return
	}
	if !guarded {
		err := h.store.SetCredential(
			r.Context(),
			req.Scope, req.ScopeTarget, req.Category, req.KeyName, req.Value,
			req.IsSecret, req.Description,
			actorID,
		)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	writeJSON(w, http.StatusCreated, map[string]string{"status": "created"})
}

// updateCredentialRequest is the JSON body accepted by UpdateCredential.
type updateCredentialRequest struct {
	Value       string `json:"value"`
	Description string `json:"description"`
}

// UpdateCredential handles PUT /api/admin/credentials/update?id=<uuid>
// Body: {"value":"...", "description":"..."}
// If value is provided, the credential is rotated (re-encrypted).
func (h *AdminHandlers) UpdateCredential(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	idStr := r.URL.Query().Get("id")
	if idStr == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	credID, err := uuid.Parse(idStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}

	var req updateCredentialRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	if req.Value != "" {
		actorID := actorIDFromCtx(r)
		guarded := false
		if h.db != nil {
			var scope, target, category, key string
			err := h.db.QueryRow(r.Context(), `SELECT scope, scope_target, category, key_name FROM credential_registry WHERE id = $1`, credID).
				Scan(&scope, &target, &category, &key)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
			if err == nil {
				var ok bool
				guarded, ok = h.guardCredential(w, r, "credentials.update", scope, target, category, key, req.Value,
					func(ctx context.Context, tx pgx.Tx) error {
						if h.store == nil {
							return errors.New("credential store unavailable")
						}
						err := h.store.RotateCredentialTx(ctx, tx, credID, req.Value, actorID)
						if errors.Is(err, configstore.ErrCredentialNotFound) {
							return notFound("credential not found")
						}
						return err
					})
				if !ok {
					return
				}
			}
		}
		if !guarded {
			if err := h.store.RotateCredential(r.Context(), credID, req.Value, actorID); err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

// DeleteCredential handles DELETE /api/admin/credentials/delete?id=<uuid>
func (h *AdminHandlers) DeleteCredential(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	idStr := r.URL.Query().Get("id")
	if idStr == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	credID, err := uuid.Parse(idStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}

	actorID := actorIDFromCtx(r)
	if err := h.store.DeleteCredential(r.Context(), credID, actorID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// rotateCredentialRequest is the JSON body accepted by RotateCredential.
type rotateCredentialRequest struct {
	Value string `json:"value"`
}

// RotateCredential handles POST /api/admin/credentials/rotate?id=<uuid>
// Body: {"value":"..."}
func (h *AdminHandlers) RotateCredential(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	idStr := r.URL.Query().Get("id")
	if idStr == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	credID, err := uuid.Parse(idStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}

	var req rotateCredentialRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Value == "" {
		writeError(w, http.StatusBadRequest, "value is required")
		return
	}

	actorID := actorIDFromCtx(r)
	if err := h.store.RotateCredential(r.Context(), credID, req.Value, actorID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "rotated"})
}

// testConnectionRequest is the JSON body accepted by TestConnection.
type testConnectionRequest struct {
	Category    string `json:"category"`
	ScopeTarget string `json:"scope_target"`
}

// TestConnection handles POST /api/admin/credentials/test
// Tests real connectivity for LLM, GitHub, and Jira integrations.
func (h *AdminHandlers) TestConnection(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req testConnectionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	ctx := r.Context()

	switch req.Category {
	case "llm":
		h.testLLMConnection(w, ctx, req.ScopeTarget)
	case "github":
		h.testGitHubConnection(w, ctx, req.ScopeTarget)
	case "jira":
		h.testJiraConnection(w, ctx, req.ScopeTarget)
	default:
		writeError(w, http.StatusBadRequest, "unsupported category: "+req.Category)
	}
}

// testLLMConnection resolves LLM credentials for the given scope target, creates
// an adapter, calls HealthCheck, and returns reachability + latency.
func (h *AdminHandlers) testLLMConnection(w http.ResponseWriter, ctx context.Context, scopeTarget string) {
	apiKey, _ := h.store.ResolveCredential(ctx, "llm", "api_key", "", scopeTarget)
	endpoint, _ := h.store.ResolveCredential(ctx, "llm", "endpoint", "", scopeTarget)
	provider, _ := h.store.ResolveCredential(ctx, "llm", "provider", "", scopeTarget)
	modelName, _ := h.store.ResolveCredential(ctx, "llm", "model_name", "", scopeTarget)

	if provider == "" {
		provider = "openai"
	}

	adp, err := adapter.NewFromConfig(provider, endpoint, apiKey, modelName)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"reachable":  false,
			"latency_ms": 0,
			"error":      "failed to build adapter: " + err.Error(),
		})
		return
	}

	start := time.Now()
	healthErr := adp.HealthCheck(ctx)
	latencyMS := time.Since(start).Milliseconds()

	if healthErr != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"reachable":  false,
			"latency_ms": latencyMS,
			"error":      healthErr.Error(),
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"reachable":  true,
		"latency_ms": latencyMS,
		"error":      nil,
	})
}

// testGitHubConnection resolves the GitHub token, calls /user, and returns
// reachability + the authenticated username.
func (h *AdminHandlers) testGitHubConnection(w http.ResponseWriter, ctx context.Context, scopeTarget string) {
	token, err := h.store.ResolveCredential(ctx, "github", "token", "", scopeTarget)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"reachable": false,
			"username":  "",
			"error":     "could not resolve github token: " + err.Error(),
		})
		return
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/user", nil)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"reachable": false,
			"username":  "",
			"error":     "build request: " + err.Error(),
		})
		return
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("Accept", "application/vnd.github+json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"reachable": false,
			"username":  "",
			"error":     err.Error(),
		})
		return
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		writeJSON(w, http.StatusOK, map[string]any{
			"reachable": false,
			"username":  "",
			"error":     "GitHub returned status " + resp.Status + ": " + string(body),
		})
		return
	}

	var ghUser struct {
		Login string `json:"login"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&ghUser); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"reachable": true,
			"username":  "",
			"error":     "decode response: " + err.Error(),
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"reachable": true,
		"username":  ghUser.Login,
		"error":     nil,
	})
}

// testJiraConnection resolves Jira credentials, calls /rest/api/3/myself, and
// returns reachability + the authenticated user's display name.
func (h *AdminHandlers) testJiraConnection(w http.ResponseWriter, ctx context.Context, scopeTarget string) {
	jiraURL, err := h.store.ResolveCredential(ctx, "jira", "url", "", scopeTarget)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"reachable":    false,
			"display_name": "",
			"error":        "could not resolve jira url: " + err.Error(),
		})
		return
	}
	email, err := h.store.ResolveCredential(ctx, "jira", "email", "", scopeTarget)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"reachable":    false,
			"display_name": "",
			"error":        "could not resolve jira email: " + err.Error(),
		})
		return
	}
	apiToken, err := h.store.ResolveCredential(ctx, "jira", "api_token", "", scopeTarget)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"reachable":    false,
			"display_name": "",
			"error":        "could not resolve jira api_token: " + err.Error(),
		})
		return
	}

	target := jiraURL + "/rest/api/3/myself"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"reachable":    false,
			"display_name": "",
			"error":        "build request: " + err.Error(),
		})
		return
	}

	creds := base64.StdEncoding.EncodeToString([]byte(email + ":" + apiToken))
	httpReq.Header.Set("Authorization", "Basic "+creds)
	httpReq.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"reachable":    false,
			"display_name": "",
			"error":        err.Error(),
		})
		return
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		writeJSON(w, http.StatusOK, map[string]any{
			"reachable":    false,
			"display_name": "",
			"error":        "Jira returned status " + resp.Status + ": " + string(body),
		})
		return
	}

	var jiraUser struct {
		DisplayName string `json:"displayName"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&jiraUser); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"reachable":    true,
			"display_name": "",
			"error":        "decode response: " + err.Error(),
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"reachable":    true,
		"display_name": jiraUser.DisplayName,
		"error":        nil,
	})
}

// ─── Products handlers ────────────────────────────────────────────────────────

// productRow is the JSON shape returned by the products endpoints.
type productRow struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
	RepoCount   int    `json:"repo_count"`
	// PartialVerificationOptIn is the D2 opt-in: when true, partially
	// verified implementations may advance to mandatory human review instead
	// of being blocked. Defaults to false.
	PartialVerificationOptIn bool `json:"partial_verification_opt_in"`
	// ContractsStrict (M2-06): when true, a cross-repository contract the
	// PR-time checkers cannot evaluate blocks the consumer's PR.
	ContractsStrict bool `json:"contracts_strict"`
}

// ListProducts handles GET /api/admin/products
func (h *AdminHandlers) ListProducts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	// partial_verification_opt_in is read through to_jsonb(p) so this query
	// still runs on a database that predates migration 045 (the key is
	// simply absent and COALESCEs to false).
	rows, err := h.db.Query(r.Context(),
		`SELECT p.id, p.name, p.description, p.created_at, p.updated_at,
		        (SELECT COUNT(*) FROM repositories WHERE product_id = p.id) AS repo_count,
		        COALESCE((to_jsonb(p) ->> 'partial_verification_opt_in')::boolean, false) AS partial_verification_opt_in,
		        COALESCE((to_jsonb(p) ->> 'contracts_strict')::boolean, false) AS contracts_strict
		 FROM products p ORDER BY p.name`)
	if err != nil {
		log.Printf("admin: list products: %v", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	defer rows.Close()

	products := []productRow{}
	for rows.Next() {
		var p productRow
		var createdAt, updatedAt time.Time
		if err := rows.Scan(&p.ID, &p.Name, &p.Description, &createdAt, &updatedAt, &p.RepoCount, &p.PartialVerificationOptIn, &p.ContractsStrict); err != nil {
			log.Printf("admin: list products scan: %v", err)
			continue
		}
		p.CreatedAt = createdAt.UTC().Format(time.RFC3339)
		p.UpdatedAt = updatedAt.UTC().Format(time.RFC3339)
		products = append(products, p)
	}
	if err := rows.Err(); err != nil {
		log.Printf("admin: list products rows error: %v", err)
	}
	writeJSON(w, http.StatusOK, products)
}

// createProductRequest is the JSON body for CreateProduct.
type createProductRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// CreateProduct handles POST /api/admin/products/create
func (h *AdminHandlers) CreateProduct(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req createProductRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}

	id := uuid.New()
	now := time.Now().UTC()
	_, err := h.db.Exec(r.Context(),
		`INSERT INTO products (id, name, description, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5)`,
		id, req.Name, req.Description, now, now)
	if err != nil {
		log.Printf("admin: create product: %v", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": id.String(), "status": "created"})
}

// updateProductRequest is the JSON body for UpdateProduct.
type updateProductRequest struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// UpdateProduct handles PUT /api/admin/products/update
func (h *AdminHandlers) UpdateProduct(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req updateProductRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.ID == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	productID, err := uuid.Parse(req.ID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}

	_, err = h.db.Exec(r.Context(),
		`UPDATE products SET name = $1, description = $2, updated_at = $3 WHERE id = $4`,
		req.Name, req.Description, time.Now().UTC(), productID)
	if err != nil {
		log.Printf("admin: update product: %v", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

// DeleteProduct handles DELETE /api/admin/products/delete?id=<uuid>
func (h *AdminHandlers) DeleteProduct(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	idStr := r.URL.Query().Get("id")
	if idStr == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	productID, err := uuid.Parse(idStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}

	_, err = h.db.Exec(r.Context(), `DELETE FROM products WHERE id = $1`, productID)
	if err != nil {
		log.Printf("admin: delete product: %v", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// ReindexProduct handles POST /api/admin/products/reindex?product_id=<uuid>
// Triggers a full re-index of all repositories belonging to the product.
func (h *AdminHandlers) ReindexProduct(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	// Accept product_id from query param or extract from path.
	productIDStr := r.URL.Query().Get("product_id")
	if productIDStr == "" {
		// Try to extract from the request body.
		var body struct {
			ProductID string `json:"product_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil && body.ProductID != "" {
			productIDStr = body.ProductID
		}
	}
	if productIDStr == "" {
		writeError(w, http.StatusBadRequest, "product_id is required")
		return
	}
	productID, err := uuid.Parse(productIDStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid product_id")
		return
	}

	// Look up all repos for this product and trigger indexing for each.
	rows, qErr := h.db.Query(r.Context(),
		"SELECT id, clone_path FROM repositories WHERE product_id = $1 AND enabled = true", productID)
	if qErr != nil {
		writeError(w, http.StatusInternalServerError, "failed to list repositories")
		return
	}
	defer rows.Close()

	indexerURL := "http://knowledge-indexer:8087"
	indexed := 0
	var lastErr string
	for rows.Next() {
		var repoID, clonePath string
		if err := rows.Scan(&repoID, &clonePath); err != nil {
			continue
		}
		body, _ := json.Marshal(map[string]string{
			"product_id":    productID.String(),
			"repository_id": repoID,
			"repo_path":     clonePath,
		})
		indexReq, reqErr := http.NewRequestWithContext(r.Context(), "POST", indexerURL+"/api/index/full", bytes.NewReader(body))
		if reqErr != nil {
			lastErr = reqErr.Error()
			continue
		}
		indexReq.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(indexReq)
		if err != nil {
			lastErr = err.Error()
			continue
		}
		resp.Body.Close()
		indexed++
	}

	result := map[string]interface{}{
		"status":             "triggered",
		"repositories_count": indexed,
	}
	if lastErr != "" {
		result["last_error"] = lastErr
	}
	writeJSON(w, http.StatusOK, result)
}

// ─── Repositories handlers ────────────────────────────────────────────────────

// repositoryRow is the JSON shape returned by the repositories endpoints.
type repositoryRow struct {
	ID                string  `json:"id"`
	ProductID         string  `json:"product_id"`
	Name              string  `json:"name"`
	GitHubURL         string  `json:"github_url"`
	BaseBranch        string  `json:"base_branch"`
	MergeStrategy     string  `json:"merge_strategy"`
	BranchPrefix      string  `json:"branch_prefix"`
	ClonePath         string  `json:"clone_path"`
	LastIndexedCommit string  `json:"last_indexed_commit"`
	Enabled           bool    `json:"enabled"`
	CreatedAt         string  `json:"created_at"`
}

// ListRepositories handles GET /api/admin/repositories?product_id=<uuid>
func (h *AdminHandlers) ListRepositories(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	productIDStr := r.URL.Query().Get("product_id")
	if productIDStr == "" {
		writeError(w, http.StatusBadRequest, "product_id is required")
		return
	}
	productID, err := uuid.Parse(productIDStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid product_id")
		return
	}

	rows, err := h.db.Query(r.Context(),
		`SELECT id, product_id, name, github_url, base_branch, merge_strategy, branch_prefix, clone_path, last_indexed_commit, enabled, created_at
		 FROM repositories WHERE product_id = $1 ORDER BY name`, productID)
	if err != nil {
		log.Printf("admin: list repositories: %v", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	defer rows.Close()

	repos := []repositoryRow{}
	for rows.Next() {
		var repo repositoryRow
		var createdAt time.Time
		if err := rows.Scan(&repo.ID, &repo.ProductID, &repo.Name, &repo.GitHubURL, &repo.BaseBranch, &repo.MergeStrategy, &repo.BranchPrefix, &repo.ClonePath, &repo.LastIndexedCommit, &repo.Enabled, &createdAt); err != nil {
			log.Printf("admin: list repositories scan: %v", err)
			continue
		}
		repo.CreatedAt = createdAt.UTC().Format(time.RFC3339)
		repos = append(repos, repo)
	}
	if err := rows.Err(); err != nil {
		log.Printf("admin: list repositories rows error: %v", err)
	}
	writeJSON(w, http.StatusOK, repos)
}

// addRepositoryRequest is the JSON body for AddRepository.
type addRepositoryRequest struct {
	ProductID      string `json:"product_id"`
	Name           string `json:"name"`
	URL            string `json:"url"`
	GitHubURL      string `json:"github_url"`
	BaseBranch     string `json:"base_branch"`
	MergeStrategy  string `json:"merge_strategy"`
	GitHubAccountID string `json:"github_account_id"`
}

// AddRepository handles POST /api/admin/repositories/create
func (h *AdminHandlers) AddRepository(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req addRepositoryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.ProductID == "" {
		writeError(w, http.StatusBadRequest, "product_id is required")
		return
	}
	productID, err := uuid.Parse(req.ProductID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid product_id")
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}

	// Accept github_url or url (backward compat)
	repoURL := req.GitHubURL
	if repoURL == "" {
		repoURL = req.URL
	}
	// Classify the remote before it is stored or handed to git (G1): an
	// invalid value is a 400; a remote that is not on github.com never gets
	// a PAT attached.
	repoURL, remoteKind, err := products.ClassifyRemote(repoURL)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	baseBranch := req.BaseBranch
	if baseBranch == "" {
		baseBranch = "main"
	}
	mergeStrategy := req.MergeStrategy
	if mergeStrategy == "" {
		mergeStrategy = "squash"
	}

	id := uuid.New()
	now := time.Now().UTC()
	clonePath := fmt.Sprintf("/repo-cache/%s/%s/", productID, req.Name)
	var githubAccountID *uuid.UUID
	tokenOverride := ""
	if req.GitHubAccountID != "" && !remoteKind.IsGitHub() {
		log.Printf("admin: add repository %q: %s remote, not attaching GitHub account %s (no PAT is ever attached to a non-GitHub remote)", req.Name, remoteKind, req.GitHubAccountID)
	}
	if req.GitHubAccountID != "" && remoteKind.IsGitHub() {
		gaID, err := uuid.Parse(req.GitHubAccountID)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid github_account_id")
			return
		}
		githubAccountID = &gaID
		var credentialID uuid.UUID
		err = h.db.QueryRow(r.Context(), `
			SELECT credential_id FROM github_accounts WHERE id = $1
		`, gaID).Scan(&credentialID)
		if err != nil {
			writeError(w, http.StatusBadRequest, "github account not found")
			return
		}
		tokenOverride, err = h.store.RevealCredential(r.Context(), credentialID, nil, "")
		if err != nil {
			writeError(w, http.StatusInternalServerError, "reveal credential: "+err.Error())
			return
		}
	}

	_, err = h.db.Exec(r.Context(),
		`INSERT INTO repositories (id, product_id, name, github_url, base_branch, merge_strategy, clone_path, token_override, github_account_id, created_at, remote_kind)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		id, productID, req.Name, repoURL, baseBranch, mergeStrategy, clonePath, tokenOverride, githubAccountID, now, string(remoteKind))
	if err != nil {
		log.Printf("admin: add repository: %v", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	// Immediately trigger clone via github-service.
	githubSvcURL := os.Getenv("GITHUB_SERVICE_URL")
	if githubSvcURL == "" {
		githubSvcURL = "http://github-service:8085"
	}
	cloneBody, _ := json.Marshal(map[string]string{
		"repository_id": id.String(),
	})
	cloneReq, cloneReqErr := http.NewRequestWithContext(r.Context(), "POST", githubSvcURL+"/clone", bytes.NewReader(cloneBody))
	cloneSuccess := false
	cloneMessage := ""
	if cloneReqErr != nil {
		cloneMessage = "Clone request error: " + cloneReqErr.Error()
		log.Printf("admin: add repository clone request build error: %v", cloneReqErr)
	} else {
		cloneReq.Header.Set("Content-Type", "application/json")
		cloneResp, cloneErr := http.DefaultClient.Do(cloneReq)
		if cloneErr != nil {
			cloneMessage = "Clone failed: " + cloneErr.Error()
			log.Printf("admin: add repository clone error for %s: %v", req.Name, cloneErr)
		} else {
			defer cloneResp.Body.Close()
			var cr map[string]interface{}
			json.NewDecoder(cloneResp.Body).Decode(&cr) //nolint:errcheck
			if s, ok := cr["success"].(bool); ok && s {
				cloneSuccess = true
				cloneMessage = "Cloned successfully"
				log.Printf("admin: add repository cloned %s to %s", req.Name, clonePath)
			} else {
				msg, _ := cr["message"].(string)
				if msg == "" {
					msg = "unknown error"
				}
				cloneMessage = "Clone failed: " + msg
				log.Printf("admin: add repository clone failed for %s: %s", req.Name, msg)
			}
		}
	}

	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"id":            id.String(),
		"name":          req.Name,
		"github_url":    repoURL,
		"base_branch":   baseBranch,
		"merge_strategy": mergeStrategy,
		"clone_path":    clonePath,
		"product_id":    productID.String(),
		"enabled":       true,
		"created_at":    now.Format(time.RFC3339),
		"status":        "created",
		"clone_success": cloneSuccess,
		"clone_message": cloneMessage,
		"remote_kind":   string(remoteKind),
	})
}

// UpdateRepository handles PUT /api/admin/repositories/update?id=<uuid>
// Body: JSON with optional fields: base_branch, merge_strategy, branch_prefix, enabled.
func (h *AdminHandlers) UpdateRepository(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	idStr := r.URL.Query().Get("id")
	if idStr == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	repoID, err := uuid.Parse(idStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}

	var body struct {
		BaseBranch    *string `json:"base_branch"`
		MergeStrategy *string `json:"merge_strategy"`
		BranchPrefix  *string `json:"branch_prefix"`
		Enabled       *bool   `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	ctx := r.Context()
	if body.BaseBranch != nil {
		h.db.Exec(ctx, `UPDATE repositories SET base_branch = $1, updated_at = NOW() WHERE id = $2`, *body.BaseBranch, repoID) //nolint:errcheck
	}
	if body.MergeStrategy != nil {
		h.db.Exec(ctx, `UPDATE repositories SET merge_strategy = $1, updated_at = NOW() WHERE id = $2`, *body.MergeStrategy, repoID) //nolint:errcheck
	}
	if body.BranchPrefix != nil {
		h.db.Exec(ctx, `UPDATE repositories SET branch_prefix = $1, updated_at = NOW() WHERE id = $2`, *body.BranchPrefix, repoID) //nolint:errcheck
	}
	if body.Enabled != nil {
		h.db.Exec(ctx, `UPDATE repositories SET enabled = $1, updated_at = NOW() WHERE id = $2`, *body.Enabled, repoID) //nolint:errcheck
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

// DeleteRepository handles DELETE /api/admin/repositories/delete?id=<uuid>
func (h *AdminHandlers) DeleteRepository(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	idStr := r.URL.Query().Get("id")
	if idStr == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	repoID, err := uuid.Parse(idStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}

	_, err = h.db.Exec(r.Context(), `DELETE FROM repositories WHERE id = $1`, repoID)
	if err != nil {
		log.Printf("admin: delete repository: %v", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// PullRepository handles POST /api/admin/repositories/pull?id=<uuid>
// Proxies to github-service /pull to run git pull on the repo's clone path.
func (h *AdminHandlers) PullRepository(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	repoID := r.URL.Query().Get("id")
	if repoID == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	if _, err := uuid.Parse(repoID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}

	githubSvcURL := os.Getenv("GITHUB_SERVICE_URL")
	if githubSvcURL == "" {
		githubSvcURL = "http://github-service:8085"
	}

	pullBody, _ := json.Marshal(map[string]string{
		"repository_id": repoID,
	})
	pullReq, err := http.NewRequestWithContext(r.Context(), "POST", githubSvcURL+"/pull", bytes.NewReader(pullBody))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to build pull request")
		return
	}
	pullReq.Header.Set("Content-Type", "application/json")

	pullResp, err := http.DefaultClient.Do(pullReq)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "github-service unreachable: "+err.Error())
		return
	}
	defer pullResp.Body.Close()

	var result map[string]interface{}
	json.NewDecoder(pullResp.Body).Decode(&result) //nolint:errcheck
	if result == nil {
		result = map[string]interface{}{"success": false, "message": "empty response from github-service"}
	}
	writeJSON(w, pullResp.StatusCode, result)
}

// RepoCloneStatus handles GET /api/admin/repositories/clone-status?id=<uuid>
// Returns whether the repo at clone_path appears to be cloned (non-empty path in DB).
func (h *AdminHandlers) RepoCloneStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	repoID := r.URL.Query().Get("id")
	if repoID == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}

	var clonePath, name string
	err := h.db.QueryRow(r.Context(),
		"SELECT clone_path, name FROM repositories WHERE id = $1",
		repoID,
	).Scan(&clonePath, &name)
	if err != nil {
		writeError(w, http.StatusNotFound, "repository not found")
		return
	}

	// Ask github-service to verify the path actually exists by attempting a /pull dry-run.
	// For now, use clone_path non-empty as a proxy (set on creation) combined with
	// last_indexed_commit to determine true clone status.
	var lastIndexedCommit string
	h.db.QueryRow(r.Context(), //nolint:errcheck
		"SELECT COALESCE(last_indexed_commit, '') FROM repositories WHERE id = $1",
		repoID,
	).Scan(&lastIndexedCommit)

	cloned := strings.TrimSpace(clonePath) != "" && strings.TrimSpace(lastIndexedCommit) != ""
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"cloned":               cloned,
		"clone_path":           clonePath,
		"last_indexed_commit":  lastIndexedCommit,
		"name":                 name,
	})
}

// ValidateProduct handles POST /api/admin/products/validate?product_id=<uuid>
// Runs pre-go-live checks that tell the operator whether the product is
// actually ready for autonomous engineering. Each check returns
// {check, passed, message} — never silently skips.
func (h *AdminHandlers) ValidateProduct(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	productIDStr := r.URL.Query().Get("product_id")
	if productIDStr == "" {
		writeError(w, http.StatusBadRequest, "product_id is required")
		return
	}
	productID, err := uuid.Parse(productIDStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid product_id: "+err.Error())
		return
	}

	type result struct {
		Check   string `json:"check"`
		Passed  bool   `json:"passed"`
		Message string `json:"message"`
	}
	out := []result{}

	// 1. Product exists
	var productName string
	err = h.db.QueryRow(r.Context(),
		`SELECT name FROM products WHERE id = $1`, productID).Scan(&productName)
	if err != nil {
		writeJSON(w, http.StatusOK, []result{{Check: "Product exists", Passed: false, Message: "product not found: " + err.Error()}})
		return
	}
	out = append(out, result{Check: "Product exists", Passed: true, Message: productName})

	// 2. >=1 enabled repo
	var enabledRepos int
	_ = h.db.QueryRow(r.Context(),
		`SELECT COUNT(*) FROM repositories WHERE product_id=$1 AND enabled=true`, productID).Scan(&enabledRepos)
	if enabledRepos == 0 {
		out = append(out, result{Check: "At least one enabled repository", Passed: false, Message: "no enabled repositories — add one in the Repositories step"})
	} else {
		out = append(out, result{Check: "At least one enabled repository", Passed: true, Message: fmt.Sprintf("%d enabled", enabledRepos)})
	}

	// 3. Every enabled repo has a clone path
	var reposMissingClone int
	_ = h.db.QueryRow(r.Context(),
		`SELECT COUNT(*) FROM repositories WHERE product_id=$1 AND enabled=true AND (clone_path IS NULL OR clone_path = '')`,
		productID).Scan(&reposMissingClone)
	if reposMissingClone > 0 {
		out = append(out, result{Check: "Every enabled repo is cloned", Passed: false, Message: fmt.Sprintf("%d enabled repos lack a clone_path", reposMissingClone)})
	} else if enabledRepos > 0 {
		out = append(out, result{Check: "Every enabled repo is cloned", Passed: true, Message: "all repos have clone paths"})
	}

	// 4. Codex has modules
	var moduleCount int
	_ = h.db.QueryRow(r.Context(),
		`SELECT COUNT(*) FROM codex_modules WHERE product_id=$1`, productID).Scan(&moduleCount)
	if moduleCount == 0 {
		out = append(out, result{Check: "Codex has modules", Passed: false, Message: "0 modules — run Analysis in the previous step, then retry"})
	} else {
		out = append(out, result{Check: "Codex has modules", Passed: true, Message: fmt.Sprintf("%d modules indexed", moduleCount)})
	}

	// 5. Onboarding candidates generated
	var candidateCount int
	_ = h.db.QueryRow(r.Context(),
		`SELECT COUNT(*) FROM onboarding_candidates WHERE product_id=$1`, productID).Scan(&candidateCount)
	if candidateCount == 0 {
		out = append(out, result{Check: "Onboarding candidates generated", Passed: false, Message: "pre-fill has not run yet — the knowledge-indexer may still be scanning"})
	} else {
		out = append(out, result{Check: "Onboarding candidates generated", Passed: true, Message: fmt.Sprintf("%d candidate answers across the question set", candidateCount)})
	}

	// 6. At least one candidate reviewed
	if candidateCount > 0 {
		var reviewedCount int
		_ = h.db.QueryRow(r.Context(),
			`SELECT COUNT(*) FROM onboarding_candidates WHERE product_id=$1 AND user_status IN ('confirmed','edited')`,
			productID).Scan(&reviewedCount)
		if reviewedCount == 0 {
			out = append(out, result{Check: "Operator reviewed candidates", Passed: false, Message: "no candidates confirmed or edited — complete the Conversation step"})
		} else {
			out = append(out, result{Check: "Operator reviewed candidates", Passed: true, Message: fmt.Sprintf("%d of %d candidates reviewed", reviewedCount, candidateCount)})
		}
	}

	writeJSON(w, http.StatusOK, out)
}

// CodexStatsForProduct handles GET /api/admin/products/codex-stats?product_id=<uuid>
// Returns the count of modules and patterns attached to the given product,
// plus the last updated timestamp. Used by the onboarding wizard's Codex
// Review step and the /admin/products detail page to show whether indexing
// actually produced data for that product.
//
// IMPORTANT: this endpoint MUST fail honestly. Return real counts, or a
// specific error message — never silently return zero on failure.
func (h *AdminHandlers) CodexStatsForProduct(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	productIDStr := r.URL.Query().Get("product_id")
	if productIDStr == "" {
		writeError(w, http.StatusBadRequest, "product_id is required")
		return
	}
	productID, err := uuid.Parse(productIDStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid product_id: "+err.Error())
		return
	}

	var modules, patterns int
	var lastUpdated *time.Time
	if err := h.db.QueryRow(r.Context(),
		`SELECT COUNT(*), MAX(updated_at) FROM codex_modules WHERE product_id = $1`,
		productID,
	).Scan(&modules, &lastUpdated); err != nil {
		writeError(w, http.StatusInternalServerError, "count modules: "+err.Error())
		return
	}
	if err := h.db.QueryRow(r.Context(),
		`SELECT COUNT(*) FROM codex_patterns WHERE product_id = $1`,
		productID,
	).Scan(&patterns); err != nil {
		writeError(w, http.StatusInternalServerError, "count patterns: "+err.Error())
		return
	}

	resp := map[string]interface{}{
		"product_id": productID.String(),
		"modules":    modules,
		"patterns":   patterns,
	}
	if lastUpdated != nil {
		resp["last_updated"] = lastUpdated.UTC().Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, resp)
}

// AnalyzeRepository handles POST /api/admin/repositories/analyze?id=<uuid>
// Proxies to the knowledge-indexer service to scan a repository.
func (h *AdminHandlers) AnalyzeRepository(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	repoID := r.URL.Query().Get("id")
	if repoID == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}

	// Look up repo details
	var name, clonePath, productID string
	err := h.db.QueryRow(r.Context(),
		"SELECT name, clone_path, product_id FROM repositories WHERE id = $1", repoID,
	).Scan(&name, &clonePath, &productID)
	if err != nil {
		writeError(w, http.StatusNotFound, "repository not found")
		return
	}

	// Call knowledge-indexer /api/index/full
	indexerURL := "http://knowledge-indexer:8087"
	body, _ := json.Marshal(map[string]string{
		"product_id":    productID,
		"repository_id": repoID,
		"repo_path":     clonePath,
	})
	indexReq, _ := http.NewRequestWithContext(r.Context(), "POST", indexerURL+"/api/index/full", bytes.NewReader(body))
	indexReq.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(indexReq)
	if err != nil {
		// Knowledge indexer might not be reachable — return a simulated result
		// since the repo structure can be read directly
		log.Printf("admin: analyze repository: knowledge-indexer unreachable: %v", err)
		writeError(w, http.StatusServiceUnavailable,
			"Knowledge indexer service is not reachable. Cannot analyze repository. "+
				"Check that the knowledge-indexer container is running and healthy.")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		writeError(w, resp.StatusCode,
			"Knowledge indexer returned an error: "+string(bodyBytes))
		return
	}

	// Forward the indexer response
	var indexResp map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&indexResp)
	if indexResp == nil {
		indexResp = map[string]interface{}{"files_scanned": 0, "modules_found": 0}
	}
	writeJSON(w, http.StatusOK, indexResp)
}

// AnalyzeRepositoryStatus handles GET /api/admin/repositories/analyze/status
// Proxies to the knowledge-indexer /api/index/status endpoint.
func (h *AdminHandlers) AnalyzeRepositoryStatus(w http.ResponseWriter, r *http.Request) {
	indexerURL := "http://knowledge-indexer:8087"
	statusReq, _ := http.NewRequestWithContext(r.Context(), "GET", indexerURL+"/api/index/status", nil)
	resp, err := http.DefaultClient.Do(statusReq)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "Knowledge indexer not reachable")
		return
	}
	defer resp.Body.Close()

	var statusResp map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&statusResp)
	if statusResp == nil {
		statusResp = map[string]interface{}{"phase": "unknown"}
	}
	writeJSON(w, http.StatusOK, statusResp)
}

// ─── Dependencies handlers ───────────────────────────────────────────────────

// ListDependencies handles GET /api/admin/dependencies?product_id=<uuid>
func (ah *AdminHandlers) ListDependencies(w http.ResponseWriter, r *http.Request) {
	productID := r.URL.Query().Get("product_id")
	if productID == "" {
		writeError(w, http.StatusBadRequest, "product_id required")
		return
	}
	rows, err := ah.db.Query(r.Context(),
		`SELECT d.id, d.source_repo_id, d.target_repo_id, d.dependency_type, d.description, d.discovered_by
         FROM repo_dependencies d
         JOIN repositories r ON d.source_repo_id = r.id OR d.target_repo_id = r.id
         WHERE r.product_id = $1
         GROUP BY d.id, d.source_repo_id, d.target_repo_id, d.dependency_type, d.description, d.discovered_by`, productID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query failed")
		return
	}
	defer rows.Close()
	var deps []map[string]interface{}
	for rows.Next() {
		var id, srcID, tgtID, depType, desc, discoveredBy string
		if err := rows.Scan(&id, &srcID, &tgtID, &depType, &desc, &discoveredBy); err != nil {
			log.Printf("admin: list dependencies scan: %v", err)
			continue
		}
		deps = append(deps, map[string]interface{}{
			"id": id, "source_repo_id": srcID, "target_repo_id": tgtID,
			"dependency_type": depType, "description": desc, "discovered_by": discoveredBy,
		})
	}
	if deps == nil {
		deps = []map[string]interface{}{}
	}
	writeJSON(w, http.StatusOK, deps)
}

// CreateDependency handles POST /api/admin/dependencies/create
func (ah *AdminHandlers) CreateDependency(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SourceRepoID   string `json:"source_repo_id"`
		TargetRepoID   string `json:"target_repo_id"`
		DependencyType string `json:"dependency_type"`
		Description    string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	var id string
	err := ah.db.QueryRow(r.Context(),
		`INSERT INTO repo_dependencies (source_repo_id, target_repo_id, dependency_type, description, discovered_by)
         VALUES ($1, $2, $3, $4, 'manual')
         ON CONFLICT (source_repo_id, target_repo_id)
         DO UPDATE SET dependency_type = EXCLUDED.dependency_type,
                       description = EXCLUDED.description
         RETURNING id`,
		body.SourceRepoID, body.TargetRepoID, body.DependencyType, body.Description).Scan(&id)
	if err != nil {
		log.Printf("admin: create dependency: %v", err)
		writeError(w, http.StatusInternalServerError, "create failed")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": id})
}

// DeleteDependency handles DELETE /api/admin/dependencies/delete?id=<uuid>
func (ah *AdminHandlers) DeleteDependency(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "id required")
		return
	}
	if _, err := ah.db.Exec(r.Context(), "DELETE FROM repo_dependencies WHERE id = $1", id); err != nil {
		log.Printf("admin: delete dependency %s: %v", id, err)
		writeError(w, http.StatusInternalServerError, "delete failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// ─── Jira handlers ────────────────────────────────────────────────────────────

// JiraIngestedCount handles GET /api/admin/jira/ingested-count
func (h *AdminHandlers) JiraIngestedCount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var count int
	if err := h.db.QueryRow(r.Context(), `SELECT COUNT(*) FROM jira_ingested_tickets`).Scan(&count); err != nil {
		log.Printf("admin: jira ingested count: %v", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"count": count})
}

// ResolveCredentialPreview handles GET /api/admin/credentials/resolve
// Query params: ?category=&key_name=&agent_type=&instance_id=
// Returns {resolved: bool, value_length: int} — never returns the plaintext.
func (h *AdminHandlers) ResolveCredentialPreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	q := r.URL.Query()
	category := q.Get("category")
	keyName := q.Get("key_name")
	agentType := q.Get("agent_type")
	instanceID := q.Get("instance_id")

	if category == "" || keyName == "" {
		writeError(w, http.StatusBadRequest, "category and key_name are required")
		return
	}

	plaintext, err := h.store.ResolveCredential(r.Context(), category, keyName, agentType, instanceID)
	if err != nil {
		// Could not resolve — credential simply not found.
		writeJSON(w, http.StatusOK, map[string]any{"resolved": false, "value_length": 0})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"resolved": true, "value_length": len(plaintext)})
}

// ─── Config handlers ──────────────────────────────────────────────────────────

// ListConfig handles GET /api/admin/config
// Optional query param: ?category=
func (h *AdminHandlers) ListConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	category := r.URL.Query().Get("category")
	entries, err := h.store.ListConfig(r.Context(), category)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if entries == nil {
		entries = []*configstore.ConfigEntry{}
	}
	writeJSON(w, http.StatusOK, entries)
}

// updateConfigRequest is the JSON body accepted by UpdateConfig.
type updateConfigRequest struct {
	Scope       string `json:"scope"`
	ScopeTarget string `json:"scope_target"`
	KeyName     string `json:"key_name"`
	Value       string `json:"value"`
	ValueType   string `json:"value_type"`
	Category    string `json:"category"`
	Description string `json:"description"`
}

// UpdateConfig handles PUT or POST /api/admin/config/update
// Accepts either a single config object or an array of config objects.
func (h *AdminHandlers) UpdateConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut && r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	// Read the body to determine if it's an array or a single object.
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read body")
		return
	}

	var reqs []updateConfigRequest
	if len(bodyBytes) > 0 && bodyBytes[0] == '[' {
		// Array of config entries.
		if err := json.Unmarshal(bodyBytes, &reqs); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
	} else {
		// Single config entry.
		var single updateConfigRequest
		if err := json.Unmarshal(bodyBytes, &single); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		reqs = []updateConfigRequest{single}
	}

	actorID := actorIDFromCtx(r)

	for _, req := range reqs {
		if req.KeyName == "" {
			writeError(w, http.StatusBadRequest, "key_name is required")
			return
		}
		if req.Scope == "" {
			req.Scope = "global"
		}

		if err := h.store.SetConfig(
			r.Context(),
			req.Scope, req.ScopeTarget, req.KeyName, req.Value,
			req.ValueType, req.Category, req.Description,
			actorID,
		); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

// ─── Audit handler ────────────────────────────────────────────────────────────

// ListAudit handles GET /api/admin/audit
// Optional query params: ?action=&category=&actor_id=&limit=&offset=
func (h *AdminHandlers) ListAudit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	q := r.URL.Query()
	action := q.Get("action")
	category := q.Get("category")
	actorIDStr := q.Get("actor_id")

	var actorID *uuid.UUID
	if actorIDStr != "" {
		id, err := uuid.Parse(actorIDStr)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid actor_id")
			return
		}
		actorID = &id
	}

	limit := 50
	offset := 0
	if ls := q.Get("limit"); ls != "" {
		if v, err := strconv.Atoi(ls); err == nil && v > 0 {
			limit = v
		}
	}
	if os := q.Get("offset"); os != "" {
		if v, err := strconv.Atoi(os); err == nil && v >= 0 {
			offset = v
		}
	}

	entries, err := h.store.ListAudit(r.Context(), action, category, actorID, limit, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if entries == nil {
		entries = []*configstore.AuditEntry{}
	}
	writeJSON(w, http.StatusOK, entries)
}

// ─── Repository access handler ────────────────────────────────────────────────

// TestRepoAccess handles POST /api/admin/repositories/test-access
// Body: {"github_url":"org/repo"}
// Tests whether the configured GitHub token can access the given repository.
func (ah *AdminHandlers) TestRepoAccess(w http.ResponseWriter, r *http.Request) {
	var body struct {
		GithubURL string `json:"github_url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	// The GitHub API only knows GitHub remotes (G1).
	if _, kind, err := products.ClassifyRemote(body.GithubURL); err != nil || !kind.IsGitHub() {
		msg := "repository has no GitHub remote; access is not checked through the GitHub API"
		if err != nil {
			msg = err.Error()
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"accessible":  false,
			"remote_kind": string(kind),
			"error":       msg,
		})
		return
	}

	// Resolve GitHub token from credential registry
	token, err := ah.store.ResolveCredential(r.Context(), "github", "token", "", "")
	if err != nil || token == "" {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"accessible": false,
			"error":      "No GitHub token configured",
		})
		return
	}

	// Test access: GET https://api.github.com/repos/{owner}/{repo}
	req, _ := http.NewRequestWithContext(r.Context(), "GET",
		"https://api.github.com/repos/"+body.GithubURL, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"accessible": false,
			"error":      err.Error(),
		})
		return
	}
	defer resp.Body.Close()

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"accessible":  resp.StatusCode == 200,
		"status_code": resp.StatusCode,
	})
}

// ─── Model evaluation handlers ────────────────────────────────────────────────

// EvaluateModel handles POST /api/admin/models/evaluate
// Resolves LLM credentials, runs the evaluation suite, and persists the result.
func (ah *AdminHandlers) EvaluateModel(w http.ResponseWriter, r *http.Request) {
	// Resolve LLM credentials
	provider, _ := ah.store.ResolveCredential(r.Context(), "llm", "provider", "", "")
	endpoint, _ := ah.store.ResolveCredential(r.Context(), "llm", "endpoint", "", "")
	apiKey, _ := ah.store.ResolveCredential(r.Context(), "llm", "api_key", "", "")
	modelName, _ := ah.store.ResolveCredential(r.Context(), "llm", "model_name", "", "")

	if provider == "" || apiKey == "" {
		writeError(w, http.StatusBadRequest, "LLM credentials not configured")
		return
	}

	// Create adapter and run evaluation
	a, err := adapter.NewFromConfig(provider, endpoint, apiKey, modelName)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to create model adapter: "+err.Error())
		return
	}

	profiler := profile.NewProfiler(a)
	result, err := profiler.Evaluate(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Evaluation failed: "+err.Error())
		return
	}

	// Save to DB
	if _, dbErr := ah.db.Exec(r.Context(),
		`INSERT INTO model_profiles (id, provider, model_id, effective_context_window, instruction_following, code_generation, code_understanding, reasoning, synthesis, avg_inference_speed, concurrent_capacity, operating_mode, evaluated_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		result.ID, result.Provider, result.ModelID, result.EffectiveContextWindow,
		result.InstructionFollowing, result.CodeGeneration, result.CodeUnderstanding,
		result.Reasoning, result.Synthesis, result.AvgInferenceSpeed,
		result.ConcurrentCapacity, result.OperatingMode, result.EvaluatedAt,
	); dbErr != nil {
		log.Printf("admin: save model profile: %v", dbErr)
	}

	writeJSON(w, http.StatusOK, result)
}

// ListModelProfiles handles GET /api/admin/models/profiles
// Returns all model profiles ordered by evaluation date descending.
func (ah *AdminHandlers) ListModelProfiles(w http.ResponseWriter, r *http.Request) {
	rows, err := ah.db.Query(r.Context(),
		`SELECT id, provider, model_id, effective_context_window, instruction_following,
		        code_generation, code_understanding, reasoning, synthesis,
		        avg_inference_speed, concurrent_capacity, operating_mode, evaluated_at
		 FROM model_profiles ORDER BY evaluated_at DESC`)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query failed")
		return
	}
	defer rows.Close()

	var profiles []map[string]interface{}
	for rows.Next() {
		var id, provider, modelID, opMode string
		var ctxWindow, instrFollow, codeGen, codeUnder, reasoning, synthesis, concurrent int
		var speed float64
		var evalAt time.Time
		if err := rows.Scan(&id, &provider, &modelID, &ctxWindow, &instrFollow, &codeGen, &codeUnder,
			&reasoning, &synthesis, &speed, &concurrent, &opMode, &evalAt); err != nil {
			log.Printf("admin: list model profiles scan: %v", err)
			continue
		}
		profiles = append(profiles, map[string]interface{}{
			"id": id, "provider": provider, "model_id": modelID,
			"effective_context_window": ctxWindow,
			"instruction_following": instrFollow, "code_generation": codeGen,
			"code_understanding": codeUnder, "reasoning": reasoning, "synthesis": synthesis,
			"avg_inference_speed": speed, "concurrent_capacity": concurrent,
			"operating_mode": opMode, "evaluated_at": evalAt,
		})
	}
	if profiles == nil {
		profiles = []map[string]interface{}{}
	}
	writeJSON(w, http.StatusOK, profiles)
}

// ─── Prompt Management Handlers ───────────────────────────────────────────────

// ListPrompts handles GET /api/admin/prompts?intent_type=&agent_type=
// Returns all prompt templates matching the optional filter parameters.
func (h *AdminHandlers) ListPrompts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	intentType := r.URL.Query().Get("intent_type")
	agentType := r.URL.Query().Get("agent_type")

	templates, err := h.promptStore.List(r.Context(), intentType, agentType)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if templates == nil {
		templates = []*prompts.PromptTemplate{}
	}
	writeJSON(w, http.StatusOK, templates)
}

// GetPrompt handles GET /api/admin/prompts/detail?id=<uuid>
func (h *AdminHandlers) GetPrompt(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	idStr := r.URL.Query().Get("id")
	if idStr == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	promptID, err := uuid.Parse(idStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}

	t, err := h.promptStore.Get(r.Context(), promptID)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, t)
}

// createPromptRequest is the JSON body accepted by CreatePrompt.
type createPromptRequest struct {
	IntentType    string `json:"intent_type"`
	AgentType     string `json:"agent_type"`
	Provider      string `json:"provider"`
	ModelID       string `json:"model_id"`
	OperatingMode string `json:"operating_mode"`
	PromptType    string `json:"prompt_type"`
	Content       string `json:"content"`
	Description   string `json:"description"`
}

// CreatePrompt handles POST /api/admin/prompts/create
func (h *AdminHandlers) CreatePrompt(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req createPromptRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.IntentType == "" {
		writeError(w, http.StatusBadRequest, "intent_type is required")
		return
	}
	if req.Content == "" {
		writeError(w, http.StatusBadRequest, "content is required")
		return
	}
	if req.PromptType == "" {
		req.PromptType = "system"
	}

	actorID := actorIDFromCtx(r)
	t := &prompts.PromptTemplate{
		IntentType:    req.IntentType,
		AgentType:     req.AgentType,
		Provider:      req.Provider,
		ModelID:       req.ModelID,
		OperatingMode: req.OperatingMode,
		PromptType:    req.PromptType,
		Content:       req.Content,
		Description:   req.Description,
		CreatedBy:     actorID,
	}
	if !guardConfigChange(w, r, h.guardDB(), "prompts.create", map[string]any{"intent_type": t.IntentType, "agent_type": t.AgentType},
		func(ctx context.Context, tx pgx.Tx) error { return prompts.CreateTx(ctx, tx, t) }) {
		return
	}
	writeJSON(w, http.StatusCreated, t)
}

// updatePromptRequest is the JSON body accepted by UpdatePrompt.
type updatePromptRequest struct {
	Content     string `json:"content"`
	Description string `json:"description"`
}

// UpdatePrompt handles PUT /api/admin/prompts/update?id=<uuid>
func (h *AdminHandlers) UpdatePrompt(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	idStr := r.URL.Query().Get("id")
	if idStr == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	promptID, err := uuid.Parse(idStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}

	var req updatePromptRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Content == "" {
		writeError(w, http.StatusBadRequest, "content is required")
		return
	}

	if !guardConfigChange(w, r, h.guardDB(), "prompts.update", map[string]any{"prompt_template_id": promptID},
		func(ctx context.Context, tx pgx.Tx) error {
			return promptNotFound(prompts.UpdateTx(ctx, tx, promptID, req.Content, req.Description))
		}) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

// DeletePrompt handles DELETE /api/admin/prompts/delete?id=<uuid>
func (h *AdminHandlers) DeletePrompt(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	idStr := r.URL.Query().Get("id")
	if idStr == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	promptID, err := uuid.Parse(idStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}

	if !guardConfigChange(w, r, h.guardDB(), "prompts.delete", map[string]any{"prompt_template_id": promptID},
		func(ctx context.Context, tx pgx.Tx) error { return promptNotFound(prompts.DeleteTx(ctx, tx, promptID)) }) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// PreviewResolution handles GET /api/admin/prompts/resolve
// Query params: intent_type, agent_type, provider, model_id, operating_mode
// Returns the resolution chain showing which template wins.
func (h *AdminHandlers) PreviewResolution(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	q := r.URL.Query()
	intentType := q.Get("intent_type")
	if intentType == "" {
		writeError(w, http.StatusBadRequest, "intent_type is required")
		return
	}
	agentType := q.Get("agent_type")
	provider := q.Get("provider")
	modelID := q.Get("model_id")
	operatingMode := q.Get("operating_mode")
	promptType := q.Get("prompt_type")
	if promptType == "" {
		promptType = "system"
	}

	chain, err := h.promptStore.GetResolutionChain(
		r.Context(), intentType, agentType, provider, modelID, operatingMode, promptType,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if chain == nil {
		chain = []*prompts.PromptTemplate{}
	}

	type chainEntry struct {
		Template *prompts.PromptTemplate `json:"template"`
		Wins     bool                    `json:"wins"`
	}

	entries := make([]chainEntry, len(chain))
	for i, t := range chain {
		entries[i] = chainEntry{Template: t, Wins: i == 0}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"chain":  entries,
		"winner": func() *prompts.PromptTemplate {
			if len(chain) > 0 {
				return chain[0]
			}
			return nil
		}(),
	})
}

// ListIntentTypes handles GET /api/admin/prompts/intents
// Returns distinct intent_type values in the prompt_templates table.
func (h *AdminHandlers) ListIntentTypes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	types, err := h.promptStore.ListIntentTypes(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if types == nil {
		types = []string{}
	}
	writeJSON(w, http.StatusOK, types)
}

// ─── Agent Definition handlers ────────────────────────────────────────────────

// ListAgentDefinitions handles GET /api/admin/agents
func (h *AdminHandlers) ListAgentDefinitions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	defs, err := h.agentRegistry.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if defs == nil {
		defs = []*agentregistry.AgentDefinition{}
	}
	writeJSON(w, http.StatusOK, defs)
}

// GetAgentDefinition handles GET /api/admin/agents/detail?id=
func (h *AdminHandlers) GetAgentDefinition(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id, err := uuidFromQuery(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	def, err := h.agentRegistry.Get(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "agent definition not found")
		return
	}
	writeJSON(w, http.StatusOK, def)
}

// CreateAgentDefinition handles POST /api/admin/agents/create
func (h *AdminHandlers) CreateAgentDefinition(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var def agentregistry.AgentDefinition
	if err := json.NewDecoder(r.Body).Decode(&def); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if def.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	// M4-06: an enabled (routable) or default definition is production;
	// create a candidate disabled and roll it out.
	if !guardConfigChange(w, r, h.guardDB(), "agents.create", map[string]any{"name": def.Name, "enabled": def.Enabled, "is_default": def.IsDefault},
		func(ctx context.Context, tx pgx.Tx) error { return agentregistry.CreateTx(ctx, tx, &def) }) {
		return
	}
	writeJSON(w, http.StatusCreated, def)
}

// UpdateAgentDefinition handles PUT /api/admin/agents/update?id=
func (h *AdminHandlers) UpdateAgentDefinition(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id, err := uuidFromQuery(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var def agentregistry.AgentDefinition
	if err := json.NewDecoder(r.Body).Decode(&def); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if !guardConfigChange(w, r, h.guardDB(), "agents.update", map[string]any{"agent_definition_id": id},
		func(ctx context.Context, tx pgx.Tx) error {
			err := agentregistry.UpdateTx(ctx, tx, id, &def)
			if errors.Is(err, pgx.ErrNoRows) {
				return notFound("agent definition not found")
			}
			return err
		}) {
		return
	}
	writeJSON(w, http.StatusOK, def)
}

// DeleteAgentDefinition handles DELETE /api/admin/agents/delete?id=
func (h *AdminHandlers) DeleteAgentDefinition(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id, err := uuidFromQuery(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !guardConfigChange(w, r, h.guardDB(), "agents.delete", map[string]any{"agent_definition_id": id},
		func(ctx context.Context, tx pgx.Tx) error { return agentregistry.DeleteTx(ctx, tx, id) }) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// SetDefaultAgent handles POST /api/admin/agents/set-default?id=
func (h *AdminHandlers) SetDefaultAgent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id, err := uuidFromQuery(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !guardConfigChange(w, r, h.guardDB(), "agents.set_default", map[string]any{"agent_definition_id": id},
		func(ctx context.Context, tx pgx.Tx) error { return agentregistry.SetDefaultTx(ctx, tx, id) }) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "default set"})
}

// ListAgentPrompts handles GET /api/admin/agents/prompts?agent_id=
func (h *AdminHandlers) ListAgentPrompts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	agentID, err := uuidFromQuery(r, "agent_id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	prompts, err := h.agentRegistry.GetPrompts(r.Context(), agentID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if prompts == nil {
		prompts = []*agentregistry.AgentDefinitionPrompt{}
	}
	writeJSON(w, http.StatusOK, prompts)
}

// setAgentPromptRequest is the request body for SetAgentPrompt.
type setAgentPromptRequest struct {
	AgentDefinitionID string `json:"agent_definition_id"`
	IntentType        string `json:"intent_type"`
	PromptType        string `json:"prompt_type"`
	Content           string `json:"content"`
}

// SetAgentPrompt handles POST /api/admin/agents/prompts/set
func (h *AdminHandlers) SetAgentPrompt(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req setAgentPromptRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	agentID, err := uuid.Parse(req.AgentDefinitionID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid agent_definition_id")
		return
	}
	if req.IntentType == "" {
		writeError(w, http.StatusBadRequest, "intent_type is required")
		return
	}
	promptType := req.PromptType
	if promptType == "" {
		promptType = "system"
	}
	if !guardConfigChange(w, r, h.guardDB(), "agents.prompts.set", map[string]any{"agent_definition_id": agentID, "intent_type": req.IntentType},
		func(ctx context.Context, tx pgx.Tx) error {
			return agentregistry.SetPromptTx(ctx, tx, agentID, req.IntentType, promptType, req.Content)
		}) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
}

// DeleteAgentPrompt handles DELETE /api/admin/agents/prompts/delete?id=
// The id query param is the agent_definition_id; also requires intent_type.
func (h *AdminHandlers) DeleteAgentPrompt(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	agentID, err := uuidFromQuery(r, "agent_id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	intentType := r.URL.Query().Get("intent_type")
	if intentType == "" {
		writeError(w, http.StatusBadRequest, "intent_type is required")
		return
	}
	if !guardConfigChange(w, r, h.guardDB(), "agents.prompts.delete", map[string]any{"agent_definition_id": agentID, "intent_type": intentType},
		func(ctx context.Context, tx pgx.Tx) error {
			return agentregistry.DeletePromptTx(ctx, tx, agentID, intentType)
		}) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// uuidFromQuery parses a UUID from a query parameter.
func uuidFromQuery(r *http.Request, param string) (uuid.UUID, error) {
	s := r.URL.Query().Get(param)
	if s == "" {
		return uuid.Nil, &missingParamError{param}
	}
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, &invalidParamError{param}
	}
	return id, nil
}

type missingParamError struct{ param string }

func (e *missingParamError) Error() string { return e.param + " is required" }

type invalidParamError struct{ param string }

func (e *invalidParamError) Error() string { return "invalid " + e.param }

// ─── Webhook Dead-Letter Queue ────────────────────────────────────────────────

// deadLetterEntry is the shape returned by the dead-letter list endpoint.
type deadLetterEntry struct {
	ID           string     `json:"id"`
	Source       string     `json:"source"`
	EventType    string     `json:"event_type"`
	Payload      any        `json:"payload"`
	ErrorMessage string     `json:"error_message"`
	RetryCount   int        `json:"retry_count"`
	MaxRetries   int        `json:"max_retries"`
	NextRetryAt  *time.Time `json:"next_retry_at,omitempty"`
	Resolved     bool       `json:"resolved"`
	CreatedAt    time.Time  `json:"created_at"`
}

// ListWebhookDeadLetters handles GET /api/admin/webhook/dead-letters.
// Returns unresolved dead-letter entries ordered by creation time (newest first).
func (h *AdminHandlers) ListWebhookDeadLetters(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	includeResolved := r.URL.Query().Get("resolved") == "true"

	var rows []deadLetterEntry

	query := `SELECT id, source, event_type, payload, error_message, retry_count, max_retries,
	                 next_retry_at, resolved, created_at
	          FROM webhook_dead_letters`
	if !includeResolved {
		query += ` WHERE NOT resolved`
	}
	query += ` ORDER BY created_at DESC LIMIT 200`

	dbRows, err := h.db.Query(r.Context(), query)
	if err != nil {
		log.Printf("admin: list dead letters: %v", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	defer dbRows.Close()

	for dbRows.Next() {
		var e deadLetterEntry
		var payload []byte
		if err := dbRows.Scan(
			&e.ID, &e.Source, &e.EventType, &payload,
			&e.ErrorMessage, &e.RetryCount, &e.MaxRetries,
			&e.NextRetryAt, &e.Resolved, &e.CreatedAt,
		); err != nil {
			log.Printf("admin: dead letters scan: %v", err)
			continue
		}
		if err := json.Unmarshal(payload, &e.Payload); err != nil {
			e.Payload = string(payload)
		}
		rows = append(rows, e)
	}
	if rows == nil {
		rows = []deadLetterEntry{}
	}
	writeJSON(w, http.StatusOK, rows)
}

// RetryWebhookDeadLetter handles POST /api/admin/webhook/dead-letters/retry?id=<uuid>.
// Resets the next_retry_at to NOW() and clears any exhausted state so the
// background worker picks it up on its next cycle.
func (h *AdminHandlers) RetryWebhookDeadLetter(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	idStr := r.URL.Query().Get("id")
	if idStr == "" {
		writeError(w, http.StatusBadRequest, "id parameter is required")
		return
	}
	id, err := uuid.Parse(idStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id format")
		return
	}

	_, err = h.db.Exec(r.Context(),
		`UPDATE webhook_dead_letters
		 SET next_retry_at = NOW(), retry_count = 0
		 WHERE id = $1 AND NOT resolved`,
		id,
	)
	if err != nil {
		log.Printf("admin: retry dead letter %s: %v", id, err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "scheduled"})
}

// ResolveWebhookDeadLetter handles POST /api/admin/webhook/dead-letters/resolve?id=<uuid>.
// Marks the dead-letter entry as manually resolved so it no longer appears in the queue.
func (h *AdminHandlers) ResolveWebhookDeadLetter(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	idStr := r.URL.Query().Get("id")
	if idStr == "" {
		writeError(w, http.StatusBadRequest, "id parameter is required")
		return
	}
	id, err := uuid.Parse(idStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id format")
		return
	}

	_, err = h.db.Exec(r.Context(),
		`UPDATE webhook_dead_letters SET resolved = true WHERE id = $1`,
		id,
	)
	if err != nil {
		log.Printf("admin: resolve dead letter %s: %v", id, err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "resolved"})
}

// ─── GitHub repo browser ──────────────────────────────────────────────────────

// ListGitHubRepos handles GET /api/admin/github/repos
// Returns all repos accessible with the configured GitHub token, paginating
// through the GitHub API automatically (up to 1 000 repos).
func (h *AdminHandlers) ListGitHubRepos(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	// Resolve GitHub token from credential registry.
	token, err := h.store.ResolveCredential(r.Context(), "github", "token", "", "")
	if err != nil || token == "" {
		writeError(w, http.StatusBadRequest, "GitHub token not configured. Add it in Admin → Credentials.")
		return
	}

	var allRepos []map[string]interface{}
	page := 1
	for {
		ghURL := fmt.Sprintf("https://api.github.com/user/repos?per_page=100&sort=updated&type=all&page=%d", page)
		req, err := http.NewRequestWithContext(r.Context(), "GET", ghURL, nil)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to build request: "+err.Error())
			return
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/vnd.github.v3+json")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			writeError(w, http.StatusBadGateway, "GitHub API error: "+err.Error())
			return
		}
		defer resp.Body.Close() //nolint:gocritic

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			writeError(w, http.StatusBadGateway, "GitHub API returned "+resp.Status+": "+string(body))
			return
		}

		var ghRepos []struct {
			FullName      string `json:"full_name"`
			Name          string `json:"name"`
			Description   string `json:"description"`
			Private       bool   `json:"private"`
			Language      string `json:"language"`
			UpdatedAt     string `json:"updated_at"`
			HTMLURL       string `json:"html_url"`
			DefaultBranch string `json:"default_branch"`
			Size          int    `json:"size"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&ghRepos); err != nil {
			writeError(w, http.StatusBadGateway, "failed to decode GitHub response: "+err.Error())
			return
		}

		for _, gr := range ghRepos {
			allRepos = append(allRepos, map[string]interface{}{
				"full_name":      gr.FullName,
				"name":           gr.Name,
				"description":    gr.Description,
				"private":        gr.Private,
				"language":       gr.Language,
				"updated_at":     gr.UpdatedAt,
				"html_url":       gr.HTMLURL,
				"default_branch": gr.DefaultBranch,
				"size_kb":        gr.Size,
			})
		}

		// Stop when the page was not full (no more pages) or we've hit the safety limit.
		if len(ghRepos) < 100 {
			break
		}
		page++
		if page > 10 {
			break // safety cap: 1 000 repos maximum
		}
	}

	if allRepos == nil {
		allRepos = []map[string]interface{}{}
	}
	writeJSON(w, http.StatusOK, allRepos)
}

// ─── GitHub Account handlers ─────────────────────────────────────────────────

func resolveGitHubUser(ctx context.Context, token string) (username, avatarURL string, err error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/user", nil)
	if err != nil {
		return "", "", fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("Accept", "application/vnd.github+json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", "", fmt.Errorf("GitHub returned %s: %s", resp.Status, string(body))
	}

	var ghUser struct {
		Login     string `json:"login"`
		AvatarURL string `json:"avatar_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&ghUser); err != nil {
		return "", "", fmt.Errorf("decode: %w", err)
	}
	return ghUser.Login, ghUser.AvatarURL, nil
}

func (h *AdminHandlers) ListGitHubAccounts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	rows, err := h.db.Query(r.Context(), `
		SELECT id, username, avatar_url, credential_id, last_validated_at, valid, created_at, label
		FROM github_accounts ORDER BY created_at
	`)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query failed: "+err.Error())
		return
	}
	defer rows.Close()

	type account struct {
		ID              string  `json:"id"`
		Username        string  `json:"username"`
		AvatarURL       string  `json:"avatar_url"`
		CredentialID    string  `json:"credential_id"`
		LastValidatedAt *string `json:"last_validated_at"`
		Valid           bool    `json:"valid"`
		CreatedAt       string  `json:"created_at"`
		Label           string  `json:"label"`
	}
	var accounts []account
	for rows.Next() {
		var a account
		var lastVal *time.Time
		var createdAt time.Time
		var credID uuid.UUID
		var id uuid.UUID
		if err := rows.Scan(&id, &a.Username, &a.AvatarURL, &credID, &lastVal, &a.Valid, &createdAt, &a.Label); err != nil {
			writeError(w, http.StatusInternalServerError, "scan: "+err.Error())
			return
		}
		a.ID = id.String()
		a.CredentialID = credID.String()
		a.CreatedAt = createdAt.Format(time.RFC3339)
		if lastVal != nil {
			s := lastVal.Format(time.RFC3339)
			a.LastValidatedAt = &s
		}
		accounts = append(accounts, a)
	}
	if accounts == nil {
		accounts = []account{}
	}
	writeJSON(w, http.StatusOK, accounts)
}

func (h *AdminHandlers) AddGitHubAccount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		Token string `json:"token"`
		Label string `json:"label"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if req.Token == "" {
		writeError(w, http.StatusBadRequest, "token is required")
		return
	}

	username, avatarURL, err := resolveGitHubUser(r.Context(), req.Token)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid token: "+err.Error())
		return
	}

	label := strings.TrimSpace(req.Label)

	var exists bool
	_ = h.db.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM github_accounts WHERE username = $1 AND label = $2)`, username, label).Scan(&exists)
	if exists {
		writeError(w, http.StatusConflict, fmt.Sprintf("account already exists: %s (label: %s)", username, label))
		return
	}

	id := uuid.New()
	scopeTarget := "github_account:" + id.String()
	if err := h.store.SetCredential(r.Context(),
		"github_account", scopeTarget, "github", "token", req.Token,
		true, fmt.Sprintf("GitHub PAT for %s (%s)", username, label), nil,
	); err != nil {
		writeError(w, http.StatusInternalServerError, "store credential: "+err.Error())
		return
	}

	var credentialID uuid.UUID
	err = h.db.QueryRow(r.Context(), `
		SELECT id FROM credential_registry
		WHERE scope = 'github_account' AND scope_target = $1 AND key_name = 'token' AND category = 'github'
	`, scopeTarget).Scan(&credentialID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "lookup credential: "+err.Error())
		return
	}

	now := time.Now().UTC()
	_, err = h.db.Exec(r.Context(), `
		INSERT INTO github_accounts (id, username, avatar_url, credential_id, label, last_validated_at, valid, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, true, $7, $7)
	`, id, username, avatarURL, credentialID, label, now, now)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "create account: "+err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"id":                id.String(),
		"username":          username,
		"label":             label,
		"avatar_url":        avatarURL,
		"credential_id":     credentialID.String(),
		"last_validated_at": now.Format(time.RFC3339),
		"valid":             true,
		"created_at":        now.Format(time.RFC3339),
	})
}

// MigrateLegacyGitHubToken converts the legacy global GitHub token (from the
// single-token era) into a github_accounts row. It reveals the legacy token,
// validates it against GitHub, creates the account-scoped credential, inserts
// the github_accounts row, and links any existing repositories that have no
// github_account_id yet. Idempotent: returns 409 if the account already exists.
func (h *AdminHandlers) MigrateLegacyGitHubToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	// 1. Find the legacy credential.
	var legacyCredID uuid.UUID
	err := h.db.QueryRow(r.Context(), `
		SELECT id FROM credential_registry
		WHERE scope = 'global' AND key_name = 'token' AND category = 'github'
		LIMIT 1
	`).Scan(&legacyCredID)
	if err != nil {
		writeError(w, http.StatusNotFound, "no legacy GitHub token found")
		return
	}

	// 2. Reveal the token plaintext.
	token, err := h.store.RevealCredential(r.Context(), legacyCredID, nil, "")
	if err != nil || token == "" {
		writeError(w, http.StatusInternalServerError, "could not reveal legacy token")
		return
	}

	// 3. Validate against GitHub API.
	username, avatarURL, err := resolveGitHubUser(r.Context(), token)
	if err != nil {
		writeError(w, http.StatusBadRequest, "legacy token is invalid: "+err.Error())
		return
	}

	// 4. Check if this account already exists.
	var exists bool
	_ = h.db.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM github_accounts WHERE username = $1)`, username).Scan(&exists)
	if exists {
		writeError(w, http.StatusConflict, "account already migrated: "+username)
		return
	}

	// 5. Create the account-scoped credential.
	scopeTarget := "github_account:" + username
	if err := h.store.SetCredential(r.Context(),
		"github_account", scopeTarget, "github", "token", token,
		true, "Migrated from legacy global GitHub token", nil,
	); err != nil {
		writeError(w, http.StatusInternalServerError, "store credential: "+err.Error())
		return
	}

	var credentialID uuid.UUID
	err = h.db.QueryRow(r.Context(), `
		SELECT id FROM credential_registry
		WHERE scope = 'github_account' AND scope_target = $1 AND key_name = 'token' AND category = 'github'
	`, scopeTarget).Scan(&credentialID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "lookup credential: "+err.Error())
		return
	}

	// 6. Create the github_accounts row.
	accountID := uuid.New()
	now := time.Now().UTC()
	_, err = h.db.Exec(r.Context(), `
		INSERT INTO github_accounts (id, username, avatar_url, credential_id, last_validated_at, valid, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, true, $6, $6)
	`, accountID, username, avatarURL, credentialID, now, now)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "create account: "+err.Error())
		return
	}

	// 7. Link existing repos that have no github_account_id and set token_override.
	// A repository recorded as a non-GitHub remote (migration 057) never
	// gets the PAT; rows from before 057 (NULL) keep the legacy behaviour,
	// and gitremote still refuses to send the token to a non-GitHub host.
	tag, err := h.db.Exec(r.Context(), `
		UPDATE repositories
		SET github_account_id = $1, token_override = $2
		WHERE github_account_id IS NULL
		  AND (remote_kind IS NULL OR remote_kind IN ('github_shorthand', 'github_https', 'github_ssh'))
	`, accountID, token)
	linkedRepos := int64(0)
	if err == nil {
		linkedRepos = tag.RowsAffected()
	}

	log.Printf("admin: migrated legacy GitHub token → account %s (%s), linked %d repos", accountID, username, linkedRepos)

	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"id":                accountID.String(),
		"username":          username,
		"avatar_url":        avatarURL,
		"credential_id":    credentialID.String(),
		"last_validated_at": now.Format(time.RFC3339),
		"valid":             true,
		"created_at":        now.Format(time.RFC3339),
		"linked_repos":      linkedRepos,
	})
}

func (h *AdminHandlers) UpdateGitHubAccount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	idStr := r.URL.Query().Get("id")
	accountID, err := uuid.Parse(idStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	var req struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if req.Token == "" {
		writeError(w, http.StatusBadRequest, "token is required")
		return
	}

	username, avatarURL, err := resolveGitHubUser(r.Context(), req.Token)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid token: "+err.Error())
		return
	}

	var credentialID uuid.UUID
	var oldUsername string
	err = h.db.QueryRow(r.Context(), `
		SELECT credential_id, username FROM github_accounts WHERE id = $1
	`, accountID).Scan(&credentialID, &oldUsername)
	if err != nil {
		writeError(w, http.StatusNotFound, "account not found")
		return
	}

	if err := h.store.RotateCredential(r.Context(), credentialID, req.Token, nil); err != nil {
		writeError(w, http.StatusInternalServerError, "rotate credential: "+err.Error())
		return
	}

	now := time.Now().UTC()
	_, err = h.db.Exec(r.Context(), `
		UPDATE github_accounts SET username = $1, avatar_url = $2, valid = true,
		last_validated_at = $3, updated_at = $3 WHERE id = $4
	`, username, avatarURL, now, accountID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "update account: "+err.Error())
		return
	}

	_, err = h.db.Exec(r.Context(), `
		UPDATE repositories SET token_override = $1, updated_at = NOW()
		WHERE github_account_id = $2
	`, req.Token, accountID)
	if err != nil {
		log.Printf("admin: bulk update token_override for account %s: %v", accountID, err)
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"id":                accountID.String(),
		"username":          username,
		"avatar_url":        avatarURL,
		"valid":             true,
		"last_validated_at": now.Format(time.RFC3339),
	})
}

func (h *AdminHandlers) DeleteGitHubAccount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete && r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	idStr := r.URL.Query().Get("id")
	accountID, err := uuid.Parse(idStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	var credentialID uuid.UUID
	err = h.db.QueryRow(r.Context(), `
		SELECT credential_id FROM github_accounts WHERE id = $1
	`, accountID).Scan(&credentialID)
	if err != nil {
		writeError(w, http.StatusNotFound, "account not found")
		return
	}
	if err := h.store.DeleteCredential(r.Context(), credentialID, nil); err != nil {
		writeError(w, http.StatusInternalServerError, "delete credential: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"deleted": true})
}

func (h *AdminHandlers) TestGitHubAccount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	idStr := r.URL.Query().Get("id")
	accountID, err := uuid.Parse(idStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	var credentialID uuid.UUID
	err = h.db.QueryRow(r.Context(), `
		SELECT credential_id FROM github_accounts WHERE id = $1
	`, accountID).Scan(&credentialID)
	if err != nil {
		writeError(w, http.StatusNotFound, "account not found")
		return
	}
	token, err := h.store.RevealCredential(r.Context(), credentialID, nil, "")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "reveal credential: "+err.Error())
		return
	}
	username, _, testErr := resolveGitHubUser(r.Context(), token)
	now := time.Now().UTC()
	valid := testErr == nil
	errMsg := ""
	if testErr != nil {
		errMsg = testErr.Error()
	}
	_, _ = h.db.Exec(r.Context(), `
		UPDATE github_accounts SET valid = $1, last_validated_at = $2, updated_at = $2 WHERE id = $3
	`, valid, now, accountID)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"valid":    valid,
		"username": username,
		"error":    errMsg,
	})
}

func (h *AdminHandlers) ListGitHubAccountRepos(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	idStr := r.URL.Query().Get("id")
	accountID, err := uuid.Parse(idStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	var credentialID uuid.UUID
	err = h.db.QueryRow(r.Context(), `
		SELECT credential_id FROM github_accounts WHERE id = $1
	`, accountID).Scan(&credentialID)
	if err != nil {
		writeError(w, http.StatusNotFound, "account not found")
		return
	}
	token, err := h.store.RevealCredential(r.Context(), credentialID, nil, "")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "reveal credential: "+err.Error())
		return
	}

	type ghRepo struct {
		FullName      string `json:"full_name"`
		Name          string `json:"name"`
		Description   string `json:"description"`
		Private       bool   `json:"private"`
		Language      string `json:"language"`
		UpdatedAt     string `json:"updated_at"`
		HTMLURL       string `json:"html_url"`
		DefaultBranch string `json:"default_branch"`
		Size          int    `json:"size"`
	}

	seen := map[string]bool{}
	var allRepos []map[string]interface{}

	addRepo := func(gr ghRepo) {
		if seen[gr.FullName] {
			return
		}
		seen[gr.FullName] = true
		allRepos = append(allRepos, map[string]interface{}{
			"full_name":      gr.FullName,
			"name":           gr.Name,
			"description":    gr.Description,
			"private":        gr.Private,
			"language":       gr.Language,
			"updated_at":     gr.UpdatedAt,
			"html_url":       gr.HTMLURL,
			"default_branch": gr.DefaultBranch,
			"size_kb":        gr.Size,
		})
	}

	doGET := func(url string) ([]byte, int, error) {
		ghReq, err := http.NewRequestWithContext(r.Context(), "GET", url, nil)
		if err != nil {
			return nil, 0, err
		}
		ghReq.Header.Set("Authorization", "Bearer "+token)
		ghReq.Header.Set("Accept", "application/vnd.github.v3+json")
		resp, err := http.DefaultClient.Do(ghReq)
		if err != nil {
			return nil, 0, err
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return body, resp.StatusCode, nil
	}

	// 1. Try /installation/repositories (fine-grained PATs scoped to an org)
	for page := 1; page <= 10; page++ {
		body, status, err := doGET(fmt.Sprintf("https://api.github.com/installation/repositories?per_page=100&page=%d", page))
		if err != nil || status != http.StatusOK {
			break
		}
		var wrapper struct {
			Repositories []ghRepo `json:"repositories"`
		}
		if json.Unmarshal(body, &wrapper) != nil {
			break
		}
		for _, gr := range wrapper.Repositories {
			addRepo(gr)
		}
		if len(wrapper.Repositories) < 100 {
			break
		}
	}

	// 2. Also fetch /user/repos (classic PATs and user-scoped fine-grained PATs)
	for page := 1; page <= 10; page++ {
		body, status, err := doGET(fmt.Sprintf("https://api.github.com/user/repos?per_page=100&sort=updated&type=all&page=%d", page))
		if err != nil || status != http.StatusOK {
			break
		}
		var repos []ghRepo
		if json.Unmarshal(body, &repos) != nil {
			break
		}
		for _, gr := range repos {
			addRepo(gr)
		}
		if len(repos) < 100 {
			break
		}
	}

	if allRepos == nil {
		allRepos = []map[string]interface{}{}
	}
	writeJSON(w, http.StatusOK, allRepos)
}
