package credmonitor

import (
	"context"
	"encoding/json"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AlertPayload is both persisted (into credential_alerts.payload) and
// broadcast over the "alerts" Kafka topic for the dashboard WebSocket hub.
type AlertPayload struct {
	Kind         string     `json:"kind"` // always "credential"
	Category     string     `json:"category"`
	KeyName      string     `json:"key_name"`
	Severity     string     `json:"severity"` // info / warning / critical
	Status       string     `json:"status"`   // expiring_soon / expired / invalid / recovered
	Message      string     `json:"message"`
	CredentialID uuid.UUID  `json:"credential_id"`
	ExpiresAt    *time.Time `json:"expires_at,omitempty"`
	Emitted      time.Time  `json:"emitted"`
}

// Publisher persists credential alerts and fans them out via the message bus.
// If Bus is nil, alerts are only persisted.
type Publisher struct {
	DB  *pgxpool.Pool
	Bus bus.MessageBus
}

// Emit writes the alert to credential_alerts and broadcasts to the "alerts"
// topic. Safe to call concurrently.
func (p *Publisher) Emit(ctx context.Context, a AlertPayload) error {
	a.Kind = "credential"
	if a.Emitted.IsZero() {
		a.Emitted = time.Now()
	}
	body, err := json.Marshal(a)
	if err != nil {
		return err
	}

	if _, err := p.DB.Exec(ctx,
		`INSERT INTO credential_alerts
		    (credential_id, category, key_name, severity, status, message, payload)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		a.CredentialID, a.Category, a.KeyName, a.Severity, a.Status, a.Message, body,
	); err != nil {
		return err
	}

	if p.Bus != nil {
		if err := p.Bus.Publish(ctx, bus.TopicAlerts, a.CredentialID.String(), body); err != nil {
			return err // persisted but not broadcast — caller decides what to do
		}
	}
	return nil
}

// ResolveAcknowledgment marks all active alerts for a credential as
// acknowledged. Called when a probe flips the credential back to valid.
func (p *Publisher) ResolveAcknowledgment(ctx context.Context, credentialID uuid.UUID) error {
	_, err := p.DB.Exec(ctx,
		`UPDATE credential_alerts SET acknowledged = true
		 WHERE credential_id = $1 AND acknowledged = false`,
		credentialID,
	)
	return err
}
