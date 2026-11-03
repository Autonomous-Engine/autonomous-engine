package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

// jwtSecret signs and verifies session tokens. main sets it from JWT_SECRET
// via loadJWTSecret before serving; there is no built-in default.
var jwtSecret []byte

// minJWTSecretBytes is the shortest JWT_SECRET the backend accepts.
const minJWTSecretBytes = 32

// loadJWTSecret reads JWT_SECRET and rejects a missing, short or placeholder
// value, so the backend never starts with a guessable signing key.
func loadJWTSecret(getenv func(string) string) ([]byte, error) {
	s := getenv("JWT_SECRET")
	switch {
	case s == "":
		return nil, fmt.Errorf("JWT_SECRET is not set; set it to a random value of at least %d bytes (for example `openssl rand -hex 32`)", minJWTSecretBytes)
	case len(s) < minJWTSecretBytes:
		return nil, fmt.Errorf("JWT_SECRET is %d bytes; it must be at least %d bytes", len(s), minJWTSecretBytes)
	case strings.HasPrefix(strings.ToLower(s), "change-me"):
		return nil, fmt.Errorf("JWT_SECRET still holds the example placeholder; set it to a random value of at least %d bytes", minJWTSecretBytes)
	}
	return []byte(s), nil
}

// contextKey is an unexported type for context value keys set by this package.
type contextKey string

const (
	contextKeyUserID   contextKey = "user_id"
	contextKeyUserRole contextKey = "user_role"
	// contextKeyRole is an alias for contextKeyUserRole used by admin middleware.
	contextKeyRole contextKey = "user_role"
)

// AuthService handles user authentication and JWT issuance.
type AuthService struct {
	db *pgxpool.Pool

	// lookup answers "does this user still exist, is it locked, what is
	// its role now" for Middleware; cacheTTL bounds how long an answer is
	// reused per user (AUTH_USER_CACHE_TTL, default 30s; 0 asks every
	// request). A token used to carry the role for its whole 8-hour life,
	// so a deleted, locked or demoted user kept their old access (F3).
	lookup   userLookup
	cacheTTL time.Duration
	mu       sync.Mutex
	cache    map[string]cachedUser
}

// userRecord is what the middleware needs to know about the user now.
type userRecord struct {
	Role               string
	Locked             bool
	MustChangePassword bool
}

// userLookup reports the user's current record. found is false when the
// user no longer exists.
type userLookup func(ctx context.Context, userID string) (rec userRecord, found bool, err error)

type cachedUser struct {
	rec   userRecord
	found bool
	at    time.Time
}

const defaultUserCacheTTL = 30 * time.Second

func userCacheTTL() time.Duration {
	if s := os.Getenv("AUTH_USER_CACHE_TTL"); s != "" {
		if d, err := time.ParseDuration(s); err == nil && d >= 0 {
			return d
		}
	}
	return defaultUserCacheTTL
}

// NewAuthService creates a new AuthService backed by the given connection pool.
func NewAuthService(db *pgxpool.Pool) *AuthService {
	a := &AuthService{db: db, cacheTTL: userCacheTTL()}
	a.lookup = a.dbLookup
	return a
}

func (a *AuthService) dbLookup(ctx context.Context, userID string) (userRecord, bool, error) {
	if a.db == nil {
		return userRecord{}, false, errors.New("auth: no database for user lookup")
	}
	id, err := uuid.Parse(userID)
	if err != nil {
		return userRecord{}, false, nil
	}
	var rec userRecord
	err = a.db.QueryRow(ctx, `SELECT role::text, locked_at IS NOT NULL, COALESCE(password_change_required, false) FROM users WHERE id = $1`, id).
		Scan(&rec.Role, &rec.Locked, &rec.MustChangePassword)
	if errors.Is(err, pgx.ErrNoRows) {
		return userRecord{}, false, nil
	}
	if err != nil {
		return userRecord{}, false, err
	}
	return rec, true, nil
}

// lookupCached is lookup with a per-user cache of cacheTTL.
func (a *AuthService) lookupCached(ctx context.Context, userID string) (userRecord, bool, error) {
	a.mu.Lock()
	if a.lookup == nil {
		a.lookup = a.dbLookup
	}
	lookup := a.lookup
	var cached cachedUser
	hit := false
	if a.cacheTTL > 0 {
		cached, hit = a.cache[userID]
	}
	a.mu.Unlock()
	if hit && time.Since(cached.at) < a.cacheTTL {
		return cached.rec, cached.found, nil
	}
	rec, found, err := lookup(ctx, userID)
	if err != nil {
		return userRecord{}, false, err
	}
	if a.cacheTTL > 0 {
		a.mu.Lock()
		if a.cache == nil {
			a.cache = make(map[string]cachedUser)
		}
		a.cache[userID] = cachedUser{rec: rec, found: found, at: time.Now()}
		a.mu.Unlock()
	}
	return rec, found, nil
}

// Login looks up the user by email, verifies the password with bcrypt, and
// returns a signed JWT containing the user's ID and role. The token expires
// after 24 hours.
func (a *AuthService) Login(ctx context.Context, email, password string) (string, error) {
	var (
		userID       string
		role         string
		passwordHash string
	)

	err := a.db.QueryRow(ctx, `
		SELECT id, role, password_hash
		FROM users
		WHERE email = $1
	`, email).Scan(&userID, &role, &passwordHash)
	if err != nil {
		return "", fmt.Errorf("invalid credentials")
	}

	if err := bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(password)); err != nil {
		return "", fmt.Errorf("invalid credentials")
	}

	now := time.Now().UTC()
	claims := jwt.MapClaims{
		"sub":  userID,
		"role": role,
		"iat":  now.Unix(),
		"exp":  now.Add(24 * time.Hour).Unix(),
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString(jwtSecret)
	if err != nil {
		return "", fmt.Errorf("sign token: %w", err)
	}
	return signed, nil
}

// Middleware wraps an http.HandlerFunc, requiring a valid Bearer JWT in the
// Authorization header. On success it injects user_id and user_role into the
// request context before calling next.
func (a *AuthService) Middleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" || !strings.HasPrefix(authHeader, "Bearer ") {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		tokenStr := strings.TrimPrefix(authHeader, "Bearer ")

		token, err := jwt.Parse(tokenStr, func(t *jwt.Token) (interface{}, error) {
			if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
			}
			return jwtSecret, nil
		}, jwt.WithExpirationRequired())
		if err != nil || !token.Valid {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		claims, ok := token.Claims.(jwt.MapClaims)
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		userID, _ := claims["sub"].(string)
		// The role comes from the database, not the token: a user removed,
		// locked or demoted after the token was issued loses access at once
		// (within the lookup cache TTL).
		rec, found, err := a.lookupCached(r.Context(), userID)
		if err != nil {
			http.Error(w, "authentication unavailable", http.StatusServiceUnavailable)
			return
		}
		if !found || rec.Locked {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// The first-run password change is enforced here, not only by the
		// login page's redirect (F2): until it is done, only the auth
		// routes (change-password, me, logout) answer.
		if rec.MustChangePassword && !strings.HasPrefix(r.URL.Path, "/api/auth/") {
			http.Error(w, `{"error":"password change required"}`, http.StatusForbidden)
			return
		}
		userRole := rec.Role

		ctx := context.WithValue(r.Context(), contextKeyUserID, userID)
		ctx = context.WithValue(ctx, contextKeyUserRole, userRole)

		next(w, r.WithContext(ctx))
	}
}
