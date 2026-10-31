// Package auth provides the authentication provider interface, JWT issuance,
// session management, and audit logging for the dashboard.
package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AuthResult holds the normalised identity returned by any auth provider after
// a successful authentication attempt.
type AuthResult struct {
	UserID                 string `json:"user_id"`
	Email                  string `json:"email"`
	Name                   string `json:"name"`
	Role                   string `json:"role"`
	Provider               string `json:"provider"`
	SSOId                  string `json:"sso_id"`
	PasswordChangeRequired bool   `json:"password_change_required"`
}

// Provider is the interface that every authentication backend must implement.
// A provider handles both credential validation and (for SSO providers) the
// OAuth/SAML redirect and callback lifecycle.
type Provider interface {
	// Type returns the unique provider type identifier (e.g. "local", "saml").
	Type() string

	// Authenticate validates the credentials in the HTTP request and returns
	// an AuthResult on success.
	Authenticate(ctx context.Context, r *http.Request) (*AuthResult, error)

	// LoginURL returns the SSO redirect URL. Local providers return "".
	LoginURL(r *http.Request) string

	// CallbackHandler returns an http.HandlerFunc for the SSO callback
	// endpoint. Local providers return nil.
	CallbackHandler() http.HandlerFunc

	// Configure deserialises provider-specific JSON configuration from the
	// auth_providers.config column.
	Configure(config json.RawMessage) error
}

// Manager coordinates registered auth providers, JWT issuance, session
// lifecycle, and audit logging.
type Manager struct {
	db        *pgxpool.Pool
	jwtSecret []byte
	providers map[string]Provider
	primary   string
}

// NewManager creates an empty Manager. Providers must be registered with
// RegisterProvider before use, and LoadFromDB should be called to configure
// them from the database.
func NewManager(db *pgxpool.Pool, jwtSecret []byte) *Manager {
	return &Manager{
		db:        db,
		jwtSecret: jwtSecret,
		providers: make(map[string]Provider),
	}
}

// RegisterProvider adds a provider to the manager's registry. It does not
// activate it; activation happens in LoadFromDB.
func (m *Manager) RegisterProvider(p Provider) {
	m.providers[p.Type()] = p
}

// LoadFromDB reads the auth_providers table and configures every enabled
// provider. The first enabled provider marked is_primary becomes the primary.
func (m *Manager) LoadFromDB(ctx context.Context) error {
	rows, err := m.db.Query(ctx,
		`SELECT provider_type, config, is_primary
		   FROM auth_providers
		  WHERE enabled = true`)
	if err != nil {
		return fmt.Errorf("query auth_providers: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var providerType string
		var rawConfig []byte
		var isPrimary bool

		if err := rows.Scan(&providerType, &rawConfig, &isPrimary); err != nil {
			return fmt.Errorf("scan auth_providers row: %w", err)
		}

		p, ok := m.providers[providerType]
		if !ok {
			log.Printf("auth: no registered provider for type %q, skipping", providerType)
			continue
		}

		if err := p.Configure(json.RawMessage(rawConfig)); err != nil {
			return fmt.Errorf("configure provider %q: %w", providerType, err)
		}

		if isPrimary && m.primary == "" {
			m.primary = providerType
		}
	}

	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate auth_providers: %w", err)
	}

	return nil
}

// GetPrimary returns the primary provider. If no primary was set during
// LoadFromDB, it falls back to the local provider.
func (m *Manager) GetPrimary() Provider {
	if m.primary != "" {
		if p, ok := m.providers[m.primary]; ok {
			return p
		}
	}
	if local, ok := m.providers["local"]; ok {
		return local
	}
	return nil
}

// GetProvider returns the named provider and a boolean indicating whether it
// was found.
func (m *Manager) GetProvider(providerType string) (Provider, bool) {
	p, ok := m.providers[providerType]
	return p, ok
}

// claims extends the standard JWT registered claims with application-specific
// fields.
type claims struct {
	Email    string `json:"email"`
	Role     string `json:"role"`
	Provider string `json:"provider"`
	jwt.RegisteredClaims
}

// IssueToken creates a signed JWT for the given AuthResult with an 8-hour
// expiry. The token carries sub, email, role, and provider claims.
func (m *Manager) IssueToken(result *AuthResult) (string, error) {
	now := time.Now()
	c := claims{
		Email:    result.Email,
		Role:     result.Role,
		Provider: result.Provider,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   result.UserID,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(8 * time.Hour)),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, c)
	signed, err := token.SignedString(m.jwtSecret)
	if err != nil {
		return "", fmt.Errorf("sign jwt: %w", err)
	}
	return signed, nil
}

// UpsertUser ensures a user record exists for the given AuthResult and returns
// the user's UUID string.
//
// For local providers the user must already exist; last_login is updated.
// For SSO providers the user is upserted by email and sso_id is recorded.
func (m *Manager) UpsertUser(ctx context.Context, result *AuthResult) (string, error) {
	if result.Provider == "local" {
		var userID string
		err := m.db.QueryRow(ctx,
			`UPDATE users
			    SET last_login = NOW(), updated_at = NOW()
			  WHERE email = $1 AND auth_provider = 'local'
			RETURNING id`,
			result.Email,
		).Scan(&userID)
		if err != nil {
			return "", fmt.Errorf("update last_login for local user %q: %w", result.Email, err)
		}
		return userID, nil
	}

	// SSO path: upsert by email.
	var userID string
	err := m.db.QueryRow(ctx,
		`INSERT INTO users (email, name, role, auth_provider, sso_id, last_login, updated_at)
		      VALUES ($1, $2, $3, $4, $5, NOW(), NOW())
		 ON CONFLICT (email) DO UPDATE
		        SET name         = EXCLUDED.name,
		            auth_provider = EXCLUDED.auth_provider,
		            sso_id       = EXCLUDED.sso_id,
		            last_login   = NOW(),
		            updated_at   = NOW()
		  RETURNING id`,
		result.Email,
		result.Name,
		result.Role,
		result.Provider,
		result.SSOId,
	).Scan(&userID)
	if err != nil {
		return "", fmt.Errorf("upsert sso user %q: %w", result.Email, err)
	}
	return userID, nil
}

// LogAttempt records a login attempt in the login_audit table. userID may be
// nil when the attempt failed before a user record was identified.
func (m *Manager) LogAttempt(
	ctx context.Context,
	email, provider string,
	success bool,
	reason, ip, ua string,
	userID *uuid.UUID,
) {
	_, err := m.db.Exec(ctx,
		`INSERT INTO login_audit
		       (user_id, email, provider, success, failure_reason, ip_address, user_agent)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		userID, email, provider, success, reason, ip, ua,
	)
	if err != nil {
		log.Printf("auth: log attempt: %v", err)
	}
}

// CreateSession inserts a new session for userID. If the user already has 3 or
// more active non-expired sessions the oldest ones are deleted first to enforce
// the concurrency limit.
func (m *Manager) CreateSession(
	ctx context.Context,
	userID, tokenHash, ip string,
	expiresAt time.Time,
) error {
	const maxSessions = 3

	// Count current active sessions.
	var count int
	if err := m.db.QueryRow(ctx,
		`SELECT COUNT(*) FROM sessions
		  WHERE user_id = $1 AND expires_at > NOW()`,
		userID,
	).Scan(&count); err != nil {
		return fmt.Errorf("count sessions: %w", err)
	}

	// Evict oldest sessions when at or above the limit.
	if count >= maxSessions {
		evict := count - maxSessions + 1
		_, err := m.db.Exec(ctx,
			`DELETE FROM sessions
			  WHERE id IN (
			      SELECT id FROM sessions
			       WHERE user_id = $1
			       ORDER BY created_at ASC
			       LIMIT $2
			  )`,
			userID, evict,
		)
		if err != nil {
			return fmt.Errorf("evict old sessions: %w", err)
		}
	}

	_, err := m.db.Exec(ctx,
		`INSERT INTO sessions (user_id, token, expires_at)
		 VALUES ($1, $2, $3)`,
		userID, tokenHash, expiresAt,
	)
	if err != nil {
		return fmt.Errorf("insert session: %w", err)
	}
	return nil
}

// ValidateSession returns true if a session with the given tokenHash exists and
// has not expired.
func (m *Manager) ValidateSession(ctx context.Context, tokenHash string) (bool, error) {
	var exists bool
	err := m.db.QueryRow(ctx,
		`SELECT EXISTS (
		     SELECT 1 FROM sessions
		      WHERE token = $1 AND expires_at > NOW()
		 )`,
		tokenHash,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("validate session: %w", err)
	}
	return exists, nil
}

// RevokeSession deletes the session identified by tokenHash.
func (m *Manager) RevokeSession(ctx context.Context, tokenHash string) error {
	_, err := m.db.Exec(ctx,
		`DELETE FROM sessions WHERE token = $1`,
		tokenHash,
	)
	if err != nil {
		return fmt.Errorf("revoke session: %w", err)
	}
	return nil
}
