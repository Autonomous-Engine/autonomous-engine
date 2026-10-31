package credmonitor

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/configstore"
)

// Runner orchestrates periodic credential probing + expiry sweeping.
//
//   ProbeTicker: every Interval, probe every GitHub + Jira credential.
//                Update DB row; if status changed, emit a state-change alert.
//   SweepTicker: every 1h, fire 14d / 7d / 1d / 0d threshold alerts for any
//                credential with expires_at set (deduped via the
//                credential_expiry_alerts_fired table).
type Runner struct {
	CS        *configstore.ConfigStore
	Pub       *Publisher
	Interval  time.Duration // probe period (default 15m)
	ProbeTO   time.Duration // per-probe timeout (default 10s)
	JiraBase  string        // base URL for Jira probes (from ConfigStore or env)
	JiraEmail string        // used only if no paired ConfigStore jira/email entry
}

// Start blocks until ctx is cancelled.
func (r *Runner) Start(ctx context.Context) {
	if r.Interval <= 0 {
		r.Interval = 15 * time.Minute
	}
	if r.ProbeTO <= 0 {
		r.ProbeTO = 10 * time.Second
	}
	probeTick := time.NewTicker(r.Interval)
	sweepTick := time.NewTicker(1 * time.Hour)
	defer probeTick.Stop()
	defer sweepTick.Stop()

	// Run both immediately on startup so state is fresh before the first tick.
	r.runProbes(ctx)
	r.runExpirySweep(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-probeTick.C:
			r.runProbes(ctx)
		case <-sweepTick.C:
			r.runExpirySweep(ctx)
		}
	}
}

func (r *Runner) runProbes(ctx context.Context) {
	for _, cat := range []string{"github", "jira"} {
		creds, err := r.CS.CredentialsByCategory(ctx, cat)
		if err != nil {
			log.Printf("credmonitor: list %s: %v", cat, err)
			continue
		}
		for _, c := range creds {
			r.probeOne(ctx, cat, c, creds)
		}
	}
}

func (r *Runner) probeOne(ctx context.Context, cat string, c *configstore.CredentialEntry, siblings []*configstore.CredentialEntry) {
	if cat == "github" && c.KeyName != "token" {
		return
	}
	if cat == "jira" && c.KeyName != "api_token" {
		return
	}

	var res CheckResult
	if cat == "github" {
		res = ProbeGitHub(ctx, c.Value, r.ProbeTO)
	} else {
		email := r.JiraEmail
		for _, other := range siblings {
			if other.KeyName == "email" {
				email = other.Value
				break
			}
		}
		if email == "" {
			log.Printf("credmonitor: jira credential %s has no paired email; skipping", c.ID)
			return
		}
		base := r.JiraBase
		for _, other := range siblings {
			if other.KeyName == "base_url" {
				base = other.Value
				break
			}
		}
		if base == "" {
			log.Printf("credmonitor: jira credential %s has no base URL; skipping", c.ID)
			return
		}
		res = ProbeJira(ctx, base, email, c.Value, r.ProbeTO)
	}

	prev, err := r.CS.UpdateValidationStatus(ctx, c.ID, res.Status, res.Error, res.ExpiresAt)
	if err != nil {
		log.Printf("credmonitor: update status %s/%s: %v", cat, c.KeyName, err)
		return
	}

	// Sync github_accounts table if this is an account-level credential.
	if cat == "github" && c.Scope == "github_account" {
		valid := res.Status == StatusValid
		_, syncErr := r.CS.DB().Exec(ctx, `
			UPDATE github_accounts SET valid = $1, last_validated_at = $2, updated_at = $2
			WHERE credential_id = $3
		`, valid, res.CheckedAt, c.ID)
		if syncErr != nil {
			log.Printf("credmonitor: sync github_accounts for credential %s: %v", c.ID, syncErr)
		}
	}

	if prev != res.Status {
		r.emitStateChange(ctx, c, prev, res)
	}
}

func (r *Runner) emitStateChange(ctx context.Context, c *configstore.CredentialEntry, prev string, res CheckResult) {
	var severity, status, msg string
	switch res.Status {
	case StatusInvalid:
		severity, status = "critical", "invalid"
		msg = fmt.Sprintf("%s credential %q is invalid: %s. Rotate it under Dashboard → Admin → Credentials.", c.Category, c.KeyName, res.Error)
	case StatusExpired:
		severity, status = "critical", "expired"
		msg = fmt.Sprintf("%s credential %q has expired. Rotate it under Dashboard → Admin → Credentials.", c.Category, c.KeyName)
	case StatusValid:
		if prev == StatusInvalid || prev == StatusExpired {
			severity, status = "info", "recovered"
			msg = fmt.Sprintf("%s credential %q is valid again.", c.Category, c.KeyName)
			// Clear stale active alerts.
			_ = r.Pub.ResolveAcknowledgment(ctx, c.ID)
		} else {
			return
		}
	default:
		return
	}
	if err := r.Pub.Emit(ctx, AlertPayload{
		Category: c.Category, KeyName: c.KeyName,
		Severity: severity, Status: status, Message: msg,
		CredentialID: c.ID, ExpiresAt: c.ExpiresAt,
	}); err != nil {
		log.Printf("credmonitor: emit alert %s/%s: %v", c.Category, c.KeyName, err)
	}
}

func (r *Runner) runExpirySweep(ctx context.Context) {
	for _, cat := range []string{"github", "jira"} {
		creds, err := r.CS.CredentialsByCategory(ctx, cat)
		if err != nil {
			log.Printf("credmonitor: sweep list %s: %v", cat, err)
			continue
		}
		now := time.Now()
		for _, c := range creds {
			if c.ExpiresAt == nil {
				continue
			}
			rem := c.ExpiresAt.Sub(now)
			var threshold int
			switch {
			case rem <= 0:
				threshold = 0
			case rem <= 24*time.Hour:
				threshold = 1
			case rem <= 7*24*time.Hour:
				threshold = 7
			case rem <= 14*24*time.Hour:
				threshold = 14
			default:
				continue
			}

			var cnt int
			if err := r.CS.DB().QueryRow(ctx,
				`SELECT COUNT(*) FROM credential_expiry_alerts_fired
				  WHERE credential_id = $1 AND threshold_days = $2 AND expires_at_snapshot = $3`,
				c.ID, threshold, *c.ExpiresAt,
			).Scan(&cnt); err != nil {
				log.Printf("credmonitor: dedupe lookup: %v", err)
				continue
			}
			if cnt > 0 {
				continue
			}

			sev := "warning"
			if threshold <= 1 {
				sev = "critical"
			}
			status := "expiring_soon"
			if threshold == 0 {
				status = "expired"
			}
			var msg string
			if threshold == 0 {
				msg = fmt.Sprintf("%s credential %q expired.", c.Category, c.KeyName)
			} else {
				msg = fmt.Sprintf("%s credential %q expires in %d day(s).", c.Category, c.KeyName, threshold)
			}
			if err := r.Pub.Emit(ctx, AlertPayload{
				Category: c.Category, KeyName: c.KeyName,
				Severity: sev, Status: status, Message: msg,
				CredentialID: c.ID, ExpiresAt: c.ExpiresAt,
			}); err != nil {
				log.Printf("credmonitor: emit expiry alert: %v", err)
				continue
			}
			if _, err := r.CS.DB().Exec(ctx,
				`INSERT INTO credential_expiry_alerts_fired (credential_id, threshold_days, expires_at_snapshot)
				 VALUES ($1, $2, $3)`,
				c.ID, threshold, *c.ExpiresAt,
			); err != nil {
				log.Printf("credmonitor: dedupe insert: %v", err)
			}
		}
	}
}
