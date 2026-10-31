package auth

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/crewjam/saml"
	"github.com/jackc/pgx/v5/pgxpool"
)

// samlRoleMapping holds the group-to-role mapping configuration for SAML.
type samlRoleMapping struct {
	AdminGroups    []string `json:"admin_groups"`
	OperatorGroups []string `json:"operator_groups"`
	ViewerGroups   []string `json:"viewer_groups"`
}

// samlConfig is the JSON structure stored in auth_providers.config for the
// SAML provider.
type samlConfig struct {
	MetadataURL string          `json:"metadata_url"`
	EntityID    string          `json:"entity_id"`
	ACSURL      string          `json:"acs_url"`
	RoleMapping samlRoleMapping `json:"role_mapping"`
	DefaultRole string          `json:"default_role"`
}

// SAMLProvider implements Provider using SAML 2.0 via the crewjam/saml library.
// It acts as a Service Provider (SP) and delegates authentication to the
// Identity Provider (IdP) whose metadata is fetched at configuration time.
type SAMLProvider struct {
	db          *pgxpool.Pool
	sp          *saml.ServiceProvider
	roleMapping samlRoleMapping
	defaultRole string
}

// NewSAMLProvider creates an unconfigured SAMLProvider. Call Configure before
// using the provider.
func NewSAMLProvider(db *pgxpool.Pool) *SAMLProvider {
	return &SAMLProvider{db: db}
}

// Type implements Provider.
func (s *SAMLProvider) Type() string { return "saml" }

// Configure parses the SAML-specific JSON configuration, fetches IdP metadata
// from metadata_url, and builds the saml.ServiceProvider ready for use.
func (s *SAMLProvider) Configure(config json.RawMessage) error {
	var cfg samlConfig
	if err := json.Unmarshal(config, &cfg); err != nil {
		return fmt.Errorf("saml: parse config: %w", err)
	}

	if cfg.MetadataURL == "" {
		return errors.New("saml: metadata_url is required")
	}

	// Fetch the IdP metadata document.
	resp, err := http.Get(cfg.MetadataURL) //nolint:noctx // metadata fetch at startup
	if err != nil {
		return fmt.Errorf("saml: fetch metadata from %q: %w", cfg.MetadataURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("saml: metadata fetch returned HTTP %d", resp.StatusCode)
	}

	var idpMeta saml.EntityDescriptor
	if err := xml.NewDecoder(resp.Body).Decode(&idpMeta); err != nil {
		return fmt.Errorf("saml: parse IdP metadata: %w", err)
	}

	// Parse the ACS URL for use in the ServiceProvider.
	acsURL, err := url.Parse(cfg.ACSURL)
	if err != nil {
		return fmt.Errorf("saml: parse acs_url %q: %w", cfg.ACSURL, err)
	}

	// Build the ServiceProvider. We use an empty TLS certificate here; a
	// production deployment should supply a real SP signing certificate via
	// saml.ServiceProvider.Certificate and .Key fields.
	sp := &saml.ServiceProvider{
		EntityID:          cfg.EntityID,
		AcsURL:            *acsURL,
		IDPMetadata:       &idpMeta,
		AllowIDPInitiated: false,
	}

	s.sp = sp
	s.roleMapping = cfg.RoleMapping
	s.defaultRole = cfg.DefaultRole

	if s.defaultRole == "" {
		s.defaultRole = "viewer"
	}

	return nil
}

// LoginURL returns a SAML authentication-request redirect URL. Returns "" if
// the ServiceProvider has not been configured.
func (s *SAMLProvider) LoginURL(r *http.Request) string {
	if s.sp == nil {
		return ""
	}

	// Resolve the IdP SSO endpoint that supports the HTTP-Redirect binding.
	idpSSOURL := s.sp.GetSSOBindingLocation(saml.HTTPRedirectBinding)
	if idpSSOURL == "" {
		return ""
	}

	authReq, err := s.sp.MakeAuthenticationRequest(idpSSOURL, saml.HTTPRedirectBinding, saml.HTTPPostBinding)
	if err != nil {
		return ""
	}

	redirectURL, err := authReq.Redirect("", s.sp)
	if err != nil {
		return ""
	}

	return redirectURL.String()
}

// CallbackHandler returns an http.HandlerFunc that processes the IdP's POST
// response at the ACS endpoint. It validates the SAML assertion, extracts the
// NameID and group attributes, maps groups to an application role, and writes
// the resulting AuthResult as JSON.
func (s *SAMLProvider) CallbackHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.sp == nil {
			http.Error(w, "saml: provider not configured", http.StatusInternalServerError)
			return
		}

		if err := r.ParseForm(); err != nil {
			http.Error(w, "saml: parse form: "+err.Error(), http.StatusBadRequest)
			return
		}

		// ParseResponse validates the SAMLResponse POST parameter and returns the
		// parsed assertion. The second argument is an optional list of possible
		// request IDs for SP-initiated flows; nil accepts any.
		assertion, err := s.sp.ParseResponse(r, nil)
		if err != nil {
			http.Error(w, "saml: invalid response: "+err.Error(), http.StatusUnauthorized)
			return
		}

		// Extract NameID as the primary user identifier (typically email).
		email := ""
		name := ""
		if assertion.Subject != nil && assertion.Subject.NameID != nil {
			email = assertion.Subject.NameID.Value
		}

		// Walk attribute statements for email, display name, and group membership.
		var groups []string
		for _, attrStmt := range assertion.AttributeStatements {
			for _, attr := range attrStmt.Attributes {
				switch attr.Name {
				case "mail", "email",
					"http://schemas.xmlsoap.org/ws/2005/05/identity/claims/emailaddress":
					if len(attr.Values) > 0 && email == "" {
						email = attr.Values[0].Value
					}
				case "displayName", "cn",
					"http://schemas.xmlsoap.org/ws/2005/05/identity/claims/name":
					if len(attr.Values) > 0 && name == "" {
						name = attr.Values[0].Value
					}
				case "memberOf", "groups",
					"http://schemas.xmlsoap.org/claims/Group":
					for _, v := range attr.Values {
						groups = append(groups, v.Value)
					}
				}
			}
		}

		if email == "" {
			http.Error(w, "saml: assertion contains no email or NameID", http.StatusUnauthorized)
			return
		}

		role := s.mapRole(groups)

		result := &AuthResult{
			Email:    email,
			Name:     name,
			Role:     role,
			Provider: "saml",
			SSOId:    email,
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(result); err != nil {
			// Response already started; nothing useful we can do.
			return
		}
	}
}

// Authenticate is not supported for SAML because authentication is redirect-
// based. Callers must use LoginURL to begin the flow and CallbackHandler to
// process the IdP response.
func (s *SAMLProvider) Authenticate(_ context.Context, _ *http.Request) (*AuthResult, error) {
	return nil, errors.New("saml: use LoginURL for SAML authentication")
}

// mapRole returns the highest-privilege role that matches any of the provided
// groups. Priority order: admin > operator > viewer. Falls back to defaultRole
// when no group matches.
func (s *SAMLProvider) mapRole(groups []string) string {
	groupSet := make(map[string]struct{}, len(groups))
	for _, g := range groups {
		groupSet[g] = struct{}{}
	}

	for _, g := range s.roleMapping.AdminGroups {
		if _, ok := groupSet[g]; ok {
			return "admin"
		}
	}
	for _, g := range s.roleMapping.OperatorGroups {
		if _, ok := groupSet[g]; ok {
			return "operator"
		}
	}
	for _, g := range s.roleMapping.ViewerGroups {
		if _, ok := groupSet[g]; ok {
			return "viewer"
		}
	}

	return s.defaultRole
}

// Ensure SAMLProvider satisfies the Provider interface at compile time.
var _ Provider = (*SAMLProvider)(nil)
