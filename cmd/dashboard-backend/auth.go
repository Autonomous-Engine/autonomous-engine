package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
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
}

// NewAuthService creates a new AuthService backed by the given connection pool.
func NewAuthService(db *pgxpool.Pool) *AuthService {
	return &AuthService{db: db}
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
		userRole, _ := claims["role"].(string)

		ctx := context.WithValue(r.Context(), contextKeyUserID, userID)
		ctx = context.WithValue(ctx, contextKeyUserRole, userRole)

		next(w, r.WithContext(ctx))
	}
}
