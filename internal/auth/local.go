package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

const (
	lockoutDuration = 15 * time.Minute
	maxFailedLogins = 5
)

// LocalProvider authenticates users stored in the database using bcrypt-hashed
// passwords. It enforces a 15-minute account lockout after 5 consecutive
// failed attempts.
type LocalProvider struct {
	db *pgxpool.Pool
}

// NewLocalProvider creates a LocalProvider backed by the given connection pool.
func NewLocalProvider(db *pgxpool.Pool) *LocalProvider {
	return &LocalProvider{db: db}
}

// Type implements Provider.
func (l *LocalProvider) Type() string { return "local" }

// loginRequest is the JSON body expected by the local login endpoint.
type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// localUser holds the columns we need from the users table.
type localUser struct {
	id                     string
	passwordHash           string
	role                   string
	name                   string
	passwordChangeRequired bool
	lockedAt               *time.Time
	failedLoginCount       int
}

// Authenticate reads {email, password} from the request body, validates the
// credentials, and returns an AuthResult on success.
//
// Lockout logic:
//   - If locked_at is set and less than 15 minutes ago → return lockout error.
//   - If locked_at is set but 15 or more minutes ago → clear the lockout.
//   - On bcrypt mismatch → increment failed_login_count; lock when it reaches 5.
//   - On success → reset failed_login_count and locked_at.
func (l *LocalProvider) Authenticate(ctx context.Context, r *http.Request) (*AuthResult, error) {
	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return nil, fmt.Errorf("decode login request: %w", err)
	}

	var u localUser
	err := l.db.QueryRow(ctx,
		`SELECT id, password_hash, role, name,
		        password_change_required, locked_at, failed_login_count
		   FROM users
		  WHERE email = $1 AND auth_provider = 'local'`,
		req.Email,
	).Scan(
		&u.id, &u.passwordHash, &u.role, &u.name,
		&u.passwordChangeRequired, &u.lockedAt, &u.failedLoginCount,
	)
	if err != nil {
		return nil, fmt.Errorf("user not found: %w", err)
	}

	// Lockout check.
	if u.lockedAt != nil {
		elapsed := time.Since(*u.lockedAt)
		if elapsed < lockoutDuration {
			remaining := lockoutDuration - elapsed
			return nil, fmt.Errorf("account locked: try again in %s",
				remaining.Round(time.Second))
		}

		// Lockout period has passed — clear it.
		_, _ = l.db.Exec(ctx,
			`UPDATE users
			    SET locked_at = NULL, failed_login_count = 0, updated_at = NOW()
			  WHERE id = $1`,
			u.id,
		)
		u.lockedAt = nil
		u.failedLoginCount = 0
	}

	// Password validation.
	if err := bcrypt.CompareHashAndPassword([]byte(u.passwordHash), []byte(req.Password)); err != nil {
		// Increment failure counter and potentially lock the account.
		u.failedLoginCount++
		if u.failedLoginCount >= maxFailedLogins {
			_, _ = l.db.Exec(ctx,
				`UPDATE users
				    SET failed_login_count = $1, locked_at = NOW(), updated_at = NOW()
				  WHERE id = $2`,
				u.failedLoginCount, u.id,
			)
			return nil, fmt.Errorf("account locked after %d failed attempts", maxFailedLogins)
		}
		_, _ = l.db.Exec(ctx,
			`UPDATE users
			    SET failed_login_count = $1, updated_at = NOW()
			  WHERE id = $2`,
			u.failedLoginCount, u.id,
		)
		return nil, fmt.Errorf("invalid credentials")
	}

	// Success — reset failure counters.
	_, _ = l.db.Exec(ctx,
		`UPDATE users
		    SET failed_login_count = 0, locked_at = NULL, updated_at = NOW()
		  WHERE id = $1`,
		u.id,
	)

	return &AuthResult{
		UserID:                 u.id,
		Email:                  req.Email,
		Name:                   u.name,
		Role:                   u.role,
		Provider:               "local",
		SSOId:                  "",
		PasswordChangeRequired: u.passwordChangeRequired,
	}, nil
}

// LoginURL implements Provider. Local authentication has no redirect URL.
func (l *LocalProvider) LoginURL(_ *http.Request) string { return "" }

// CallbackHandler implements Provider. Local authentication has no callback.
func (l *LocalProvider) CallbackHandler() http.HandlerFunc { return nil }

// Configure implements Provider. Local authentication requires no external
// configuration.
func (l *LocalProvider) Configure(_ json.RawMessage) error { return nil }
