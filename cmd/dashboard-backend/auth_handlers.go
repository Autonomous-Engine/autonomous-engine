package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/auth"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

// AuthHandlers exposes HTTP handlers for authentication endpoints. It delegates
// credential validation and session management to the auth.Manager.
type AuthHandlers struct {
	manager *auth.Manager
	db      *pgxpool.Pool
}

// NewAuthHandlers creates an AuthHandlers using the provided Manager and pool.
func NewAuthHandlers(manager *auth.Manager, db *pgxpool.Pool) *AuthHandlers {
	return &AuthHandlers{manager: manager, db: db}
}

// ─── helpers ──────────────────────────────────────────────────────────────────

// hashToken returns the hex-encoded SHA-256 digest of a JWT string. This is
// stored in the sessions table so that the raw token never touches the DB.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// extractBearerToken returns the token string from an "Authorization: Bearer …"
// header, or "" if the header is absent or malformed.
func extractBearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return ""
	}
	return strings.TrimPrefix(h, "Bearer ")
}

// loginSuccessResponse is the JSON body returned on a successful login.
type loginSuccessResponse struct {
	Token string          `json:"token"`
	User  loginUserFields `json:"user"`
}

type loginUserFields struct {
	ID                     string `json:"id"`
	Email                  string `json:"email"`
	Name                   string `json:"name"`
	Role                   string `json:"role"`
	PasswordChangeRequired bool   `json:"password_change_required"`
}

// issueSessionAndRespond is shared logic after a provider returns an AuthResult:
// upsert the user, issue a JWT, create a session, log the attempt, and write
// the success response.
func (h *AuthHandlers) issueSessionAndRespond(w http.ResponseWriter, r *http.Request, result *auth.AuthResult) {
	ctx := r.Context()

	userID, err := h.manager.UpsertUser(ctx, result)
	if err != nil {
		http.Error(w, `{"error":"user upsert failed"}`, http.StatusInternalServerError)
		return
	}

	token, err := h.manager.IssueToken(result)
	if err != nil {
		http.Error(w, `{"error":"token issuance failed"}`, http.StatusInternalServerError)
		return
	}

	tokenHash := hashToken(token)
	expiresAt := time.Now().Add(8 * time.Hour)

	if err := h.manager.CreateSession(ctx, userID, tokenHash, r.RemoteAddr, expiresAt); err != nil {
		// The token was just issued but CreateSession failed, so server-side
		// session validation will reject every subsequent request with a 401
		// and no trace. Returning the token would be a silent-failure lie.
		// Log the real error and return 500 so the client sees an honest
		// failure and does not persist a useless token.
		log.Printf("auth: create session for %s (%s): %v", result.Email, userID, err)
		h.manager.LogAttempt(
			ctx,
			result.Email,
			result.Provider,
			false,
			"session creation failed: "+err.Error(),
			r.RemoteAddr,
			r.UserAgent(),
			nil,
		)
		http.Error(w, `{"error":"session creation failed"}`, http.StatusInternalServerError)
		return
	}

	uid, _ := uuid.Parse(userID)
	h.manager.LogAttempt(
		ctx,
		result.Email,
		result.Provider,
		true,
		"",
		r.RemoteAddr,
		r.UserAgent(),
		&uid,
	)

	writeJSON(w, http.StatusOK, loginSuccessResponse{
		Token: token,
		User: loginUserFields{
			ID:                     userID,
			Email:                  result.Email,
			Name:                   result.Name,
			Role:                   result.Role,
			PasswordChangeRequired: result.PasswordChangeRequired,
		},
	})
}

// ─── Public auth endpoints ─────────────────────────────────────────────────────

// HandleLogin handles POST /api/auth/login using the primary configured provider.
func (h *AuthHandlers) HandleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	provider := h.manager.GetPrimary()
	if provider == nil {
		http.Error(w, `{"error":"no auth provider configured"}`, http.StatusInternalServerError)
		return
	}

	result, err := provider.Authenticate(r.Context(), r)
	if err != nil {
		h.manager.LogAttempt(
			r.Context(),
			"",
			provider.Type(),
			false,
			err.Error(),
			r.RemoteAddr,
			r.UserAgent(),
			nil,
		)
		http.Error(w, `{"error":"invalid credentials"}`, http.StatusUnauthorized)
		return
	}

	h.issueSessionAndRespond(w, r, result)
}

// HandleLDAPLogin handles POST /api/auth/login/ldap using the LDAP provider.
func (h *AuthHandlers) HandleLDAPLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	provider, ok := h.manager.GetProvider("ldap")
	if !ok {
		http.Error(w, `{"error":"ldap provider not configured"}`, http.StatusServiceUnavailable)
		return
	}

	result, err := provider.Authenticate(r.Context(), r)
	if err != nil {
		h.manager.LogAttempt(
			r.Context(),
			"",
			"ldap",
			false,
			err.Error(),
			r.RemoteAddr,
			r.UserAgent(),
			nil,
		)
		http.Error(w, `{"error":"invalid credentials"}`, http.StatusUnauthorized)
		return
	}

	h.issueSessionAndRespond(w, r, result)
}

// HandleSAMLLogin handles GET /api/auth/saml/login — redirects to the IdP.
func (h *AuthHandlers) HandleSAMLLogin(w http.ResponseWriter, r *http.Request) {
	provider, ok := h.manager.GetProvider("saml")
	if !ok {
		http.Error(w, `{"error":"saml provider not configured"}`, http.StatusServiceUnavailable)
		return
	}

	loginURL := provider.LoginURL(r)
	if loginURL == "" {
		http.Error(w, `{"error":"could not generate SAML redirect URL"}`, http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, loginURL, http.StatusFound)
}

// HandleSAMLCallback handles POST /api/auth/saml/callback — the IdP posts the
// SAML assertion here. On success, the user is redirected to the dashboard with
// the JWT token as a query parameter so the frontend can pick it up.
func (h *AuthHandlers) HandleSAMLCallback(w http.ResponseWriter, r *http.Request) {
	provider, ok := h.manager.GetProvider("saml")
	if !ok {
		http.Error(w, `{"error":"saml provider not configured"}`, http.StatusServiceUnavailable)
		return
	}

	callbackFn := provider.CallbackHandler()
	if callbackFn == nil {
		http.Error(w, `{"error":"saml callback not available"}`, http.StatusInternalServerError)
		return
	}

	// Capture the AuthResult that the SAML callback writes as JSON into a
	// response recorder, then reuse it to issue a proper session.
	rr := &responseRecorder{header: make(http.Header), code: http.StatusOK}
	callbackFn(rr, r)

	if rr.code != http.StatusOK {
		w.WriteHeader(rr.code)
		w.Write(rr.body) //nolint:errcheck
		return
	}

	var result auth.AuthResult
	if err := json.Unmarshal(rr.body, &result); err != nil {
		http.Error(w, `{"error":"saml callback returned invalid data"}`, http.StatusInternalServerError)
		return
	}

	ctx := r.Context()

	userID, err := h.manager.UpsertUser(ctx, &result)
	if err != nil {
		http.Error(w, `{"error":"user upsert failed"}`, http.StatusInternalServerError)
		return
	}

	token, err := h.manager.IssueToken(&result)
	if err != nil {
		http.Error(w, `{"error":"token issuance failed"}`, http.StatusInternalServerError)
		return
	}

	tokenHash := hashToken(token)
	expiresAt := time.Now().Add(8 * time.Hour)
	_ = h.manager.CreateSession(ctx, userID, tokenHash, r.RemoteAddr, expiresAt)

	uid, _ := uuid.Parse(userID)
	h.manager.LogAttempt(ctx, result.Email, "saml", true, "", r.RemoteAddr, r.UserAgent(), &uid)

	http.Redirect(w, r, "/dashboard?token="+token, http.StatusFound)
}

// responseRecorder is a minimal http.ResponseWriter used to capture the output
// of an inner handler without writing it directly to the client.
type responseRecorder struct {
	header http.Header
	code   int
	body   []byte
}

func (rr *responseRecorder) Header() http.Header        { return rr.header }
func (rr *responseRecorder) WriteHeader(code int)       { rr.code = code }
func (rr *responseRecorder) Write(b []byte) (int, error) {
	rr.body = append(rr.body, b...)
	return len(b), nil
}

// HandleLogout handles POST /api/auth/logout. Requires a valid JWT (enforced by
// Middleware). Deletes the session so the token can no longer be used.
func (h *AuthHandlers) HandleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	token := extractBearerToken(r)
	if token == "" {
		http.Error(w, `{"error":"missing token"}`, http.StatusUnauthorized)
		return
	}

	tokenHash := hashToken(token)
	if err := h.manager.RevokeSession(r.Context(), tokenHash); err != nil {
		http.Error(w, `{"error":"could not revoke session"}`, http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "logged_out"})
}

// HandleChangePassword handles POST /api/auth/change-password. Requires auth.
// Body: {current_password, new_password}.
func (h *AuthHandlers) HandleChangePassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	userID, _ := r.Context().Value(contextKeyUserID).(string)
	if userID == "" {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	var body struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid JSON body"}`, http.StatusBadRequest)
		return
	}

	if len(body.NewPassword) < 12 {
		http.Error(w, `{"error":"new_password must be at least 12 characters"}`, http.StatusBadRequest)
		return
	}
	if body.CurrentPassword == body.NewPassword {
		http.Error(w, `{"error":"new_password must differ from current_password"}`, http.StatusBadRequest)
		return
	}

	// Fetch the stored password hash.
	var storedHash string
	err := h.db.QueryRow(r.Context(),
		`SELECT password_hash FROM users WHERE id = $1`,
		userID,
	).Scan(&storedHash)
	if err != nil {
		http.Error(w, `{"error":"user not found"}`, http.StatusNotFound)
		return
	}

	// Verify the current password.
	if err := bcrypt.CompareHashAndPassword([]byte(storedHash), []byte(body.CurrentPassword)); err != nil {
		http.Error(w, `{"error":"current password is incorrect"}`, http.StatusUnauthorized)
		return
	}

	// Hash the new password.
	newHash, err := bcrypt.GenerateFromPassword([]byte(body.NewPassword), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, `{"error":"failed to hash new password"}`, http.StatusInternalServerError)
		return
	}

	_, err = h.db.Exec(r.Context(),
		`UPDATE users
		    SET password_hash = $1, password_change_required = false, updated_at = NOW()
		  WHERE id = $2`,
		string(newHash), userID,
	)
	if err != nil {
		http.Error(w, `{"error":"failed to update password"}`, http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "changed"})
}

// HandleMe handles GET /api/auth/me. Returns the authenticated user's profile.
func (h *AuthHandlers) HandleMe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	userID, _ := r.Context().Value(contextKeyUserID).(string)
	if userID == "" {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	var u struct {
		ID                     string     `json:"id"`
		Email                  string     `json:"email"`
		Name                   string     `json:"name"`
		Role                   string     `json:"role"`
		AuthProvider           string     `json:"auth_provider"`
		LastLogin              *time.Time `json:"last_login"`
		PasswordChangeRequired bool       `json:"password_change_required"`
	}

	err := h.db.QueryRow(r.Context(),
		`SELECT id, email, name, role, auth_provider, last_login, password_change_required
		   FROM users WHERE id = $1`,
		userID,
	).Scan(&u.ID, &u.Email, &u.Name, &u.Role, &u.AuthProvider, &u.LastLogin, &u.PasswordChangeRequired)
	if err != nil {
		http.Error(w, `{"error":"user not found"}`, http.StatusNotFound)
		return
	}

	writeJSON(w, http.StatusOK, u)
}

// HandleListProviders handles GET /api/auth/providers. Public. Returns provider
// types, names, and enabled status — no config details.
func (h *AuthHandlers) HandleListProviders(w http.ResponseWriter, r *http.Request) {
	rows, err := h.db.Query(r.Context(),
		`SELECT provider_type, name, enabled FROM auth_providers ORDER BY provider_type`,
	)
	if err != nil {
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type providerInfo struct {
		ProviderType string `json:"provider_type"`
		Name         string `json:"name"`
		Enabled      bool   `json:"enabled"`
	}

	providers := []providerInfo{}
	for rows.Next() {
		var p providerInfo
		if err := rows.Scan(&p.ProviderType, &p.Name, &p.Enabled); err != nil {
			continue
		}
		providers = append(providers, p)
	}

	writeJSON(w, http.StatusOK, providers)
}

// ─── Admin auth endpoints ──────────────────────────────────────────────────────

// HandleListAuthProviders handles GET /api/admin/auth/providers. Returns full
// auth_providers rows including config JSON.
func (h *AuthHandlers) HandleListAuthProviders(w http.ResponseWriter, r *http.Request) {
	rows, err := h.db.Query(r.Context(),
		`SELECT provider_type, name, enabled, is_primary, config, created_at, updated_at
		   FROM auth_providers ORDER BY provider_type`,
	)
	if err != nil {
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type fullProvider struct {
		ProviderType string          `json:"provider_type"`
		Name         string          `json:"name"`
		Enabled      bool            `json:"enabled"`
		IsPrimary    bool            `json:"is_primary"`
		Config       json.RawMessage `json:"config"`
		CreatedAt    time.Time       `json:"created_at"`
		UpdatedAt    time.Time       `json:"updated_at"`
	}

	providers := []fullProvider{}
	for rows.Next() {
		var p fullProvider
		if err := rows.Scan(&p.ProviderType, &p.Name, &p.Enabled, &p.IsPrimary, &p.Config, &p.CreatedAt, &p.UpdatedAt); err != nil {
			continue
		}
		providers = append(providers, p)
	}

	writeJSON(w, http.StatusOK, providers)
}

// HandleUpdateAuthProvider handles PUT /api/admin/auth/providers/update.
// Query param: ?type=. Body: JSON object which may contain "config" (object)
// and/or "enabled" (bool). Both fields are optional; only provided fields are
// updated.
func (h *AuthHandlers) HandleUpdateAuthProvider(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	providerType := r.URL.Query().Get("type")
	if providerType == "" {
		http.Error(w, `{"error":"type query parameter is required"}`, http.StatusBadRequest)
		return
	}

	// Decode into a flexible struct so we can handle both config and enabled.
	var body struct {
		Config  json.RawMessage `json:"config"`
		Enabled *bool           `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid JSON body"}`, http.StatusBadRequest)
		return
	}

	ctx := r.Context()

	if body.Enabled != nil {
		if _, err := h.db.Exec(ctx,
			`UPDATE auth_providers SET enabled = $1, updated_at = NOW() WHERE provider_type = $2`,
			*body.Enabled, providerType,
		); err != nil {
			http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
			return
		}
	}

	if body.Config != nil && len(body.Config) > 0 && string(body.Config) != "null" {
		if _, err := h.db.Exec(ctx,
			`UPDATE auth_providers SET config = $1, updated_at = NOW() WHERE provider_type = $2`,
			body.Config, providerType,
		); err != nil {
			http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

// HandleEnableProvider handles POST /api/admin/auth/providers/enable.
// Query param: ?type=.
func (h *AuthHandlers) HandleEnableProvider(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	providerType := r.URL.Query().Get("type")
	if providerType == "" {
		http.Error(w, `{"error":"type query parameter is required"}`, http.StatusBadRequest)
		return
	}

	_, err := h.db.Exec(r.Context(),
		`UPDATE auth_providers SET enabled = true, updated_at = NOW() WHERE provider_type = $1`,
		providerType,
	)
	if err != nil {
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "enabled"})
}

// HandleDisableProvider handles POST /api/admin/auth/providers/disable.
// Query param: ?type=. Prevents disabling the last remaining enabled provider.
func (h *AuthHandlers) HandleDisableProvider(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	providerType := r.URL.Query().Get("type")
	if providerType == "" {
		http.Error(w, `{"error":"type query parameter is required"}`, http.StatusBadRequest)
		return
	}

	// Guard: refuse if this is the only enabled provider.
	var enabledCount int
	if err := h.db.QueryRow(r.Context(),
		`SELECT COUNT(*) FROM auth_providers WHERE enabled = true`,
	).Scan(&enabledCount); err != nil {
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}
	if enabledCount <= 1 {
		http.Error(w, `{"error":"cannot disable the only enabled auth provider"}`, http.StatusConflict)
		return
	}

	_, err := h.db.Exec(r.Context(),
		`UPDATE auth_providers SET enabled = false, updated_at = NOW() WHERE provider_type = $1`,
		providerType,
	)
	if err != nil {
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "disabled"})
}

// HandleSetPrimary handles POST or PUT /api/admin/auth/providers/primary.
// Accepts type as a query param (?type=) or from the JSON body ({"type":"..."}).
func (h *AuthHandlers) HandleSetPrimary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodPut {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	providerType := r.URL.Query().Get("type")
	if providerType == "" {
		// Try reading from the JSON body.
		var body struct {
			Type string `json:"type"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
			providerType = body.Type
		}
	}
	if providerType == "" {
		http.Error(w, `{"error":"type is required (query param or JSON body)"}`, http.StatusBadRequest)
		return
	}

	tx, err := h.db.Begin(r.Context())
	if err != nil {
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}
	defer tx.Rollback(r.Context()) //nolint:errcheck

	if _, err := tx.Exec(r.Context(),
		`UPDATE auth_providers SET is_primary = false, updated_at = NOW()`,
	); err != nil {
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}

	if _, err := tx.Exec(r.Context(),
		`UPDATE auth_providers SET is_primary = true, updated_at = NOW() WHERE provider_type = $1`,
		providerType,
	); err != nil {
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}

	if err := tx.Commit(r.Context()); err != nil {
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "primary set"})
}

// HandleLoginAudit handles GET /api/admin/auth/login-audit. Returns a paginated
// list of login_audit rows. Optional query params: email, success, from, to,
// limit (default 50), offset (default 0).
func (h *AuthHandlers) HandleLoginAudit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	limit := 50
	if l := q.Get("limit"); l != "" {
		if v, err := strconv.Atoi(l); err == nil && v > 0 {
			limit = v
		}
	}
	offset := 0
	if o := q.Get("offset"); o != "" {
		if v, err := strconv.Atoi(o); err == nil && v >= 0 {
			offset = v
		}
	}

	// Build a dynamic WHERE clause.
	where := []string{"1=1"}
	args := []interface{}{}
	argIdx := 1

	if email := q.Get("email"); email != "" {
		where = append(where, "email = $"+strconv.Itoa(argIdx))
		args = append(args, email)
		argIdx++
	}
	if success := q.Get("success"); success != "" {
		boolVal := success == "true" || success == "1"
		where = append(where, "success = $"+strconv.Itoa(argIdx))
		args = append(args, boolVal)
		argIdx++
	}
	if from := q.Get("from"); from != "" {
		where = append(where, "timestamp >= $"+strconv.Itoa(argIdx))
		args = append(args, from)
		argIdx++
	}
	if to := q.Get("to"); to != "" {
		where = append(where, "timestamp <= $"+strconv.Itoa(argIdx))
		args = append(args, to)
		argIdx++
	}

	args = append(args, limit, offset)
	limitIdx := strconv.Itoa(argIdx)
	offsetIdx := strconv.Itoa(argIdx + 1)

	query := "SELECT id, user_id, email, provider, success, failure_reason, ip_address, user_agent, timestamp" +
		" FROM login_audit WHERE " + strings.Join(where, " AND ") +
		" ORDER BY timestamp DESC LIMIT $" + limitIdx + " OFFSET $" + offsetIdx

	rows, err := h.db.Query(r.Context(), query, args...)
	if err != nil {
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type auditRow struct {
		ID            string     `json:"id"`
		UserID        *string    `json:"user_id"`
		Email         string     `json:"email"`
		Provider      string     `json:"provider"`
		Success       bool       `json:"success"`
		FailureReason *string    `json:"failure_reason"`
		IPAddress     string     `json:"ip_address"`
		UserAgent     string     `json:"user_agent"`
		CreatedAt     time.Time  `json:"created_at"`
	}

	results := []auditRow{}
	for rows.Next() {
		var a auditRow
		if err := rows.Scan(
			&a.ID, &a.UserID, &a.Email, &a.Provider, &a.Success,
			&a.FailureReason, &a.IPAddress, &a.UserAgent, &a.CreatedAt,
		); err != nil {
			continue
		}
		results = append(results, a)
	}

	writeJSON(w, http.StatusOK, results)
}
