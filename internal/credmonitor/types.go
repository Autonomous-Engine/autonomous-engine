// Package credmonitor probes stored GitHub and Jira credentials on a schedule,
// records the outcome on the credential_registry row, and publishes alerts on
// state changes. It's consumed by cmd/credential-monitor (the daemon) and
// internal/guards (the synchronous check used by github-service).
package credmonitor

import "time"

// Validation status values — must match the CHECK constraint in migration 015.
const (
	StatusUnknown      = "unknown"
	StatusValid        = "valid"
	StatusExpired      = "expired"
	StatusInvalid      = "invalid"
	StatusNetworkError = "network_error"
)

// CheckResult is the output of a single probe attempt. The probe is
// responsible for ensuring the token does NOT appear in Error.
type CheckResult struct {
	Status    string     // one of the Status* constants above
	ExpiresAt *time.Time // populated only when the API tells us the expiry
	Error     string     // short human-readable reason, never contains the token
	CheckedAt time.Time
}
