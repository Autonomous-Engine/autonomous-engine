package auth

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	ldap "github.com/go-ldap/ldap/v3"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ldapRoleMapping holds the group-to-role mapping configuration for LDAP.
// It intentionally mirrors samlRoleMapping so both providers use the same
// JSON shape, but they are separate types to keep them independently evolvable.
type ldapRoleMapping struct {
	AdminGroups    []string `json:"admin_groups"`
	OperatorGroups []string `json:"operator_groups"`
	ViewerGroups   []string `json:"viewer_groups"`
}

// ldapConfig is the JSON structure stored in auth_providers.config for the
// LDAP provider.
type ldapConfig struct {
	Server         string          `json:"server"`
	UseTLS         bool            `json:"use_tls"`
	BindDN         string          `json:"bind_dn"`
	BindPassword   string          `json:"bind_password"`
	SearchBase     string          `json:"search_base"`
	SearchFilter   string          `json:"search_filter"`
	GroupAttribute string          `json:"group_attribute"`
	RoleMapping    ldapRoleMapping `json:"role_mapping"`
	DefaultRole    string          `json:"default_role"`
}

// ldapLoginRequest is the JSON body expected by the LDAP authenticate endpoint.
type ldapLoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// LDAPProvider implements Provider using LDAP / Active Directory. It binds
// with a service account to locate the user's DN, then re-binds with the
// user's credentials to verify the password. Group membership is read from the
// configured groupAttribute on the user's LDAP entry.
type LDAPProvider struct {
	db             *pgxpool.Pool
	server         string
	useTLS         bool
	bindDN         string
	bindPassword   string
	searchBase     string
	searchFilter   string
	groupAttribute string
	roleMapping    ldapRoleMapping
	defaultRole    string
}

// NewLDAPProvider creates an unconfigured LDAPProvider. Call Configure before
// using the provider.
func NewLDAPProvider(db *pgxpool.Pool) *LDAPProvider {
	return &LDAPProvider{db: db}
}

// Type implements Provider.
func (l *LDAPProvider) Type() string { return "ldap" }

// Configure parses the LDAP-specific JSON configuration and stores all fields
// for use during authentication.
func (l *LDAPProvider) Configure(config json.RawMessage) error {
	var cfg ldapConfig
	if err := json.Unmarshal(config, &cfg); err != nil {
		return fmt.Errorf("ldap: parse config: %w", err)
	}

	if cfg.Server == "" {
		return errors.New("ldap: server is required")
	}

	l.server = cfg.Server
	l.useTLS = cfg.UseTLS
	l.bindDN = cfg.BindDN
	l.bindPassword = cfg.BindPassword
	l.searchBase = cfg.SearchBase
	l.searchFilter = cfg.SearchFilter
	l.groupAttribute = cfg.GroupAttribute
	l.roleMapping = cfg.RoleMapping
	l.defaultRole = cfg.DefaultRole

	if l.groupAttribute == "" {
		l.groupAttribute = "memberOf"
	}
	if l.searchFilter == "" {
		l.searchFilter = "(sAMAccountName={{username}})"
	}
	if l.defaultRole == "" {
		l.defaultRole = "viewer"
	}

	return nil
}

// Authenticate reads {username, password} from the request body, connects to
// the LDAP server, locates the user entry via the service-account bind, then
// verifies credentials by re-binding as the user. On success it returns an
// AuthResult with role derived from group membership.
func (l *LDAPProvider) Authenticate(ctx context.Context, r *http.Request) (*AuthResult, error) {
	var req ldapLoginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return nil, fmt.Errorf("ldap: decode request: %w", err)
	}

	if req.Username == "" || req.Password == "" {
		return nil, errors.New("ldap: username and password are required")
	}

	// Connect to LDAP server.
	conn, err := ldap.DialURL(l.server)
	if err != nil {
		return nil, fmt.Errorf("ldap: connect to %q: %w", l.server, err)
	}
	defer conn.Close()

	// Upgrade to TLS if requested (STARTTLS, distinct from ldaps://).
	if l.useTLS {
		if err := conn.StartTLS(&tls.Config{InsecureSkipVerify: false}); err != nil { //nolint:gosec
			return nil, fmt.Errorf("ldap: StartTLS: %w", err)
		}
	}

	// Bind with the service account to perform the user search.
	if err := conn.Bind(l.bindDN, l.bindPassword); err != nil {
		return nil, fmt.Errorf("ldap: service-account bind: %w", err)
	}

	// Build and execute the search request.
	filter := strings.ReplaceAll(l.searchFilter, "{{username}}", ldap.EscapeFilter(req.Username))

	searchReq := ldap.NewSearchRequest(
		l.searchBase,
		ldap.ScopeWholeSubtree,
		ldap.NeverDerefAliases,
		0, // size limit (0 = unlimited)
		0, // time limit (0 = unlimited)
		false,
		filter,
		[]string{"dn", "mail", "displayName", "cn", l.groupAttribute},
		nil,
	)

	searchRes, err := conn.Search(searchReq)
	if err != nil {
		return nil, fmt.Errorf("ldap: search: %w", err)
	}

	if len(searchRes.Entries) == 0 {
		return nil, errors.New("ldap: user not found")
	}

	entry := searchRes.Entries[0]
	userDN := entry.DN

	// Verify the user's credentials by attempting a second bind.
	if err := conn.Bind(userDN, req.Password); err != nil {
		return nil, errors.New("ldap: invalid credentials")
	}

	// Collect group memberships.
	groups := entry.GetAttributeValues(l.groupAttribute)

	// Extract display attributes; fall back gracefully when absent.
	email := entry.GetAttributeValue("mail")
	if email == "" {
		// Some directories store the UPN or sAMAccountName instead of mail.
		email = req.Username
	}

	name := entry.GetAttributeValue("displayName")
	if name == "" {
		name = entry.GetAttributeValue("cn")
	}

	role := l.mapRole(groups)

	return &AuthResult{
		Email:    email,
		Name:     name,
		Role:     role,
		Provider: "ldap",
		SSOId:    userDN,
	}, nil
}

// LoginURL implements Provider. LDAP authentication is credential-based and
// does not use a redirect URL.
func (l *LDAPProvider) LoginURL(_ *http.Request) string { return "" }

// CallbackHandler implements Provider. LDAP authentication has no SSO callback.
func (l *LDAPProvider) CallbackHandler() http.HandlerFunc { return nil }

// mapRole returns the highest-privilege role that matches any of the provided
// LDAP group DNs or names. Priority order: admin > operator > viewer. Falls
// back to defaultRole when no group matches.
func (l *LDAPProvider) mapRole(groups []string) string {
	groupSet := make(map[string]struct{}, len(groups))
	for _, g := range groups {
		groupSet[g] = struct{}{}
	}

	for _, g := range l.roleMapping.AdminGroups {
		if _, ok := groupSet[g]; ok {
			return "admin"
		}
	}
	for _, g := range l.roleMapping.OperatorGroups {
		if _, ok := groupSet[g]; ok {
			return "operator"
		}
	}
	for _, g := range l.roleMapping.ViewerGroups {
		if _, ok := groupSet[g]; ok {
			return "viewer"
		}
	}

	return l.defaultRole
}

// Ensure LDAPProvider satisfies the Provider interface at compile time.
var _ Provider = (*LDAPProvider)(nil)
