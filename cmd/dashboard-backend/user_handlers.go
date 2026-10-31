package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

// UserHandlers exposes admin-only HTTP handlers for user CRUD operations.
type UserHandlers struct {
	db *pgxpool.Pool
}

// NewUserHandlers creates a UserHandlers backed by the given connection pool.
func NewUserHandlers(db *pgxpool.Pool) *UserHandlers {
	return &UserHandlers{db: db}
}

// HandleListUsers handles GET /api/admin/users. Returns all users ordered by
// created_at. The password_hash column is never included.
func (h *UserHandlers) HandleListUsers(w http.ResponseWriter, r *http.Request) {
	rows, err := h.db.Query(r.Context(),
		`SELECT id, email, name, role, auth_provider, last_login,
		        locked_at, password_change_required, created_at
		   FROM users
		  ORDER BY created_at`,
	)
	if err != nil {
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type userRow struct {
		ID                     string     `json:"id"`
		Email                  string     `json:"email"`
		Name                   string     `json:"name"`
		Role                   string     `json:"role"`
		AuthProvider           string     `json:"auth_provider"`
		LastLogin              *time.Time `json:"last_login"`
		LockedAt               *time.Time `json:"locked_at"`
		PasswordChangeRequired bool       `json:"password_change_required"`
		CreatedAt              time.Time  `json:"created_at"`
	}

	users := []userRow{}
	for rows.Next() {
		var u userRow
		if err := rows.Scan(
			&u.ID, &u.Email, &u.Name, &u.Role, &u.AuthProvider,
			&u.LastLogin, &u.LockedAt, &u.PasswordChangeRequired, &u.CreatedAt,
		); err != nil {
			continue
		}
		users = append(users, u)
	}

	writeJSON(w, http.StatusOK, users)
}

// HandleCreateUser handles POST /api/admin/users/create.
// Body: {email, name, role, password}.
func (h *UserHandlers) HandleCreateUser(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var body struct {
		Email    string `json:"email"`
		Name     string `json:"name"`
		Role     string `json:"role"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid JSON body"}`, http.StatusBadRequest)
		return
	}

	if body.Email == "" {
		http.Error(w, `{"error":"email is required"}`, http.StatusBadRequest)
		return
	}
	if len(body.Password) < 12 {
		http.Error(w, `{"error":"password must be at least 12 characters"}`, http.StatusBadRequest)
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(body.Password), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, `{"error":"failed to hash password"}`, http.StatusInternalServerError)
		return
	}

	var newID string
	err = h.db.QueryRow(r.Context(),
		`INSERT INTO users (email, name, role, password_hash, auth_provider)
		 VALUES ($1, $2, $3, $4, 'local')
		 RETURNING id`,
		body.Email, body.Name, body.Role, string(hash),
	).Scan(&newID)
	if err != nil {
		http.Error(w, `{"error":"failed to create user"}`, http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"id":    newID,
		"email": body.Email,
		"name":  body.Name,
		"role":  body.Role,
	})
}

// HandleUpdateUser handles PUT /api/admin/users/update.
// Query param: ?id=. Body: {name, role}.
// An admin cannot change their own role via this endpoint.
func (h *UserHandlers) HandleUpdateUser(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	targetID := r.URL.Query().Get("id")
	if targetID == "" {
		http.Error(w, `{"error":"id query parameter is required"}`, http.StatusBadRequest)
		return
	}

	callerID, _ := r.Context().Value(contextKeyUserID).(string)

	var body struct {
		Name string `json:"name"`
		Role string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid JSON body"}`, http.StatusBadRequest)
		return
	}

	// Prevent an admin from changing their own role (could lock themselves out).
	if callerID == targetID && body.Role != "" {
		// Fetch the caller's current role to compare.
		var currentRole string
		if err := h.db.QueryRow(r.Context(),
			`SELECT role FROM users WHERE id = $1`, callerID,
		).Scan(&currentRole); err == nil {
			if body.Role != currentRole {
				http.Error(w, `{"error":"cannot change your own role"}`, http.StatusForbidden)
				return
			}
		}
	}

	_, err := h.db.Exec(r.Context(),
		`UPDATE users SET name = $1, role = $2, updated_at = NOW() WHERE id = $3`,
		body.Name, body.Role, targetID,
	)
	if err != nil {
		http.Error(w, `{"error":"failed to update user"}`, http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

// HandleDeleteUser handles DELETE /api/admin/users/delete.
// Query param: ?id=. Users cannot delete themselves.
func (h *UserHandlers) HandleDeleteUser(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	targetID := r.URL.Query().Get("id")
	if targetID == "" {
		http.Error(w, `{"error":"id query parameter is required"}`, http.StatusBadRequest)
		return
	}

	callerID, _ := r.Context().Value(contextKeyUserID).(string)
	if callerID == targetID {
		http.Error(w, `{"error":"cannot delete your own account"}`, http.StatusForbidden)
		return
	}

	_, err := h.db.Exec(r.Context(),
		`DELETE FROM users WHERE id = $1`,
		targetID,
	)
	if err != nil {
		http.Error(w, `{"error":"failed to delete user"}`, http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// HandleResetPassword handles POST /api/admin/users/reset-password.
// Query param: ?id=. Generates a 16-char random password, hashes it, updates
// the user record, and returns the plaintext password once.
func (h *UserHandlers) HandleResetPassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	targetID := r.URL.Query().Get("id")
	if targetID == "" {
		http.Error(w, `{"error":"id query parameter is required"}`, http.StatusBadRequest)
		return
	}

	// Generate 16 random hex characters (8 random bytes → 16 hex chars).
	rawBytes := make([]byte, 8)
	if _, err := rand.Read(rawBytes); err != nil {
		http.Error(w, `{"error":"failed to generate password"}`, http.StatusInternalServerError)
		return
	}
	password := hex.EncodeToString(rawBytes)

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, `{"error":"failed to hash password"}`, http.StatusInternalServerError)
		return
	}

	_, err = h.db.Exec(r.Context(),
		`UPDATE users
		    SET password_hash = $1, password_change_required = true, updated_at = NOW()
		  WHERE id = $2`,
		string(hash), targetID,
	)
	if err != nil {
		http.Error(w, `{"error":"failed to reset password"}`, http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"password": password})
}

// HandleUnlockUser handles POST /api/admin/users/unlock.
// Query param: ?id=. Clears locked_at and resets failed_login_count.
func (h *UserHandlers) HandleUnlockUser(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	targetID := r.URL.Query().Get("id")
	if targetID == "" {
		http.Error(w, `{"error":"id query parameter is required"}`, http.StatusBadRequest)
		return
	}

	_, err := h.db.Exec(r.Context(),
		`UPDATE users
		    SET locked_at = NULL, failed_login_count = 0, updated_at = NOW()
		  WHERE id = $1`,
		targetID,
	)
	if err != nil {
		http.Error(w, `{"error":"failed to unlock user"}`, http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "unlocked"})
}
