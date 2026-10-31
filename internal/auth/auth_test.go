package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// stubProvider is a minimal Provider implementation used purely for unit tests.
type stubProvider struct {
	providerType string
}

func (s *stubProvider) Type() string { return s.providerType }
func (s *stubProvider) Authenticate(_ context.Context, _ *http.Request) (*AuthResult, error) {
	return &AuthResult{UserID: "u1", Email: "test@example.com", Role: "user", Provider: s.providerType}, nil
}
func (s *stubProvider) LoginURL(_ *http.Request) string       { return "" }
func (s *stubProvider) CallbackHandler() http.HandlerFunc      { return nil }
func (s *stubProvider) Configure(_ json.RawMessage) error      { return nil }

func TestNewManagerCreatesEmptyManager(t *testing.T) {
	m := NewManager(nil, []byte("secret"))
	if m == nil {
		t.Fatal("NewManager returned nil")
	}
	if m.providers == nil {
		t.Error("providers map is nil")
	}
	if m.primary != "" {
		t.Errorf("primary should be empty, got %q", m.primary)
	}
}

func TestRegisterAndGetProvider(t *testing.T) {
	m := NewManager(nil, []byte("secret"))
	p := &stubProvider{providerType: "local"}

	m.RegisterProvider(p)

	got, ok := m.GetProvider("local")
	if !ok {
		t.Error("GetProvider('local') returned ok=false")
	}
	if got.Type() != "local" {
		t.Errorf("provider.Type() = %q, want %q", got.Type(), "local")
	}
}

func TestGetProviderNotFound(t *testing.T) {
	m := NewManager(nil, []byte("secret"))
	_, ok := m.GetProvider("nonexistent")
	if ok {
		t.Error("GetProvider on non-existent type returned ok=true")
	}
}

func TestIssueTokenReturnsValidJWT(t *testing.T) {
	secret := []byte("my-test-secret-key")
	m := NewManager(nil, secret)

	result := &AuthResult{
		UserID:   "user-123",
		Email:    "alice@example.com",
		Role:     "admin",
		Provider: "local",
	}

	tokenStr, err := m.IssueToken(result)
	if err != nil {
		t.Fatalf("IssueToken error: %v", err)
	}
	if tokenStr == "" {
		t.Fatal("IssueToken returned empty string")
	}

	// Parse and verify the JWT
	token, err := jwt.ParseWithClaims(tokenStr, &claims{}, func(t *jwt.Token) (interface{}, error) {
		return secret, nil
	})
	if err != nil {
		t.Fatalf("JWT parse error: %v", err)
	}
	if !token.Valid {
		t.Error("parsed JWT is not valid")
	}

	c, ok := token.Claims.(*claims)
	if !ok {
		t.Fatal("claims type assertion failed")
	}
	if c.Email != "alice@example.com" {
		t.Errorf("email claim = %q, want %q", c.Email, "alice@example.com")
	}
	if c.Role != "admin" {
		t.Errorf("role claim = %q, want %q", c.Role, "admin")
	}
	if c.Provider != "local" {
		t.Errorf("provider claim = %q, want %q", c.Provider, "local")
	}
	sub, err := c.GetSubject()
	if err != nil {
		t.Fatalf("GetSubject error: %v", err)
	}
	if sub != "user-123" {
		t.Errorf("subject = %q, want %q", sub, "user-123")
	}
}

func TestIssueTokenExpiryIsSet(t *testing.T) {
	secret := []byte("test-secret")
	m := NewManager(nil, secret)
	result := &AuthResult{UserID: "u1", Email: "b@b.com", Role: "user", Provider: "local"}

	tokenStr, err := m.IssueToken(result)
	if err != nil {
		t.Fatalf("IssueToken error: %v", err)
	}

	token, err := jwt.ParseWithClaims(tokenStr, &claims{}, func(t *jwt.Token) (interface{}, error) {
		return secret, nil
	})
	if err != nil {
		t.Fatalf("JWT parse error: %v", err)
	}

	c := token.Claims.(*claims)
	if c.ExpiresAt == nil {
		t.Fatal("ExpiresAt is nil")
	}
	if c.IssuedAt == nil {
		t.Fatal("IssuedAt is nil")
	}

	// Expiry should be approximately 8 hours after issued (allow ±5s tolerance
	// for clock rounding between IssuedAt / ExpiresAt computation).
	diff := c.ExpiresAt.Time.Sub(c.IssuedAt.Time)
	expected := 8 * time.Hour
	tolerance := 5 * time.Second
	delta := diff - expected
	if delta < -tolerance || delta > tolerance {
		t.Errorf("token expiry duration = %v, expected ~%v (±%v)", diff, expected, tolerance)
	}
}

func TestGetPrimaryFallsBackToLocal(t *testing.T) {
	m := NewManager(nil, []byte("secret"))
	local := &stubProvider{providerType: "local"}
	m.RegisterProvider(local)
	// no primary set

	p := m.GetPrimary()
	if p == nil {
		t.Fatal("GetPrimary returned nil when local provider is registered")
	}
	if p.Type() != "local" {
		t.Errorf("GetPrimary() = %q, want %q", p.Type(), "local")
	}
}

func TestGetPrimaryReturnsNilWhenNothingRegistered(t *testing.T) {
	m := NewManager(nil, []byte("secret"))
	p := m.GetPrimary()
	if p != nil {
		t.Errorf("GetPrimary() should be nil when no providers registered, got %T", p)
	}
}
