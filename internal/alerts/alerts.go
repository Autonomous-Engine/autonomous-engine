// Package alerts provides a background service that periodically evaluates
// alert rules stored in alert_configs and fires alerts when thresholds are
// exceeded.
package alerts

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AlertService evaluates alert rules on a fixed interval and fires alerts by
// inserting into alert_history, publishing to Kafka, and optionally notifying
// Slack or logging email intent.
type AlertService struct {
	db       *pgxpool.Pool
	bus      bus.MessageBus
	interval time.Duration
	cancel   context.CancelFunc
}

// New creates an AlertService. If interval is zero the default of 60 seconds
// is used.
func New(db *pgxpool.Pool, messageBus bus.MessageBus, interval time.Duration) *AlertService {
	if interval <= 0 {
		interval = 60 * time.Second
	}
	return &AlertService{
		db:       db,
		bus:      messageBus,
		interval: interval,
	}
}

// Start spawns a background goroutine that evaluates alert rules every interval.
// It runs until Stop is called or the parent context is cancelled.
func (s *AlertService) Start(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	s.cancel = cancel

	go func() {
		// Run once immediately, then on each tick.
		s.evaluate(ctx)

		ticker := time.NewTicker(s.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.evaluate(ctx)
			}
		}
	}()
}

// Stop cancels the background evaluation goroutine.
func (s *AlertService) Stop() {
	if s.cancel != nil {
		s.cancel()
	}
}

// alertConfig mirrors a row from the alert_configs table.
type alertConfig struct {
	ID        uuid.UUID
	Name      string
	AlertType string
	Threshold float64
	Channels  []string
	Enabled   bool
}

// evaluate loads all enabled alert configs and checks each one.
func (s *AlertService) evaluate(ctx context.Context) {
	configs, err := s.loadConfigs(ctx)
	if err != nil {
		log.Printf("alerts: load configs: %v", err)
		return
	}

	for _, cfg := range configs {
		fired, message, severity := s.checkRule(ctx, cfg)
		if !fired {
			continue
		}

		if err := s.fire(ctx, cfg, message, severity); err != nil {
			log.Printf("alerts: fire alert %s: %v", cfg.Name, err)
		}
	}
}

// loadConfigs fetches all enabled alert_configs rows.
func (s *AlertService) loadConfigs(ctx context.Context) ([]alertConfig, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id, name, alert_type, threshold, channels, enabled
		FROM alert_configs
		WHERE enabled = true
	`)
	if err != nil {
		return nil, fmt.Errorf("query alert_configs: %w", err)
	}
	defer rows.Close()

	var configs []alertConfig
	for rows.Next() {
		var cfg alertConfig
		if err := rows.Scan(
			&cfg.ID, &cfg.Name, &cfg.AlertType, &cfg.Threshold, &cfg.Channels, &cfg.Enabled,
		); err != nil {
			return nil, fmt.Errorf("scan alert_config row: %w", err)
		}
		configs = append(configs, cfg)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate alert_config rows: %w", err)
	}
	return configs, nil
}

// checkRule evaluates a single alert rule. Returns (fired, message, severity).
func (s *AlertService) checkRule(ctx context.Context, cfg alertConfig) (bool, string, string) {
	switch cfg.AlertType {
	case "queue_depth":
		return s.checkQueueDepth(ctx, cfg)
	case "stuck_task":
		return s.checkStuckTask(ctx, cfg)
	case "model_unhealthy":
		return s.checkModelUnhealthy(ctx, cfg)
	case "qa_failure_rate":
		return s.checkQAFailureRate(ctx, cfg)
	case "circuit_breaker":
		return s.checkCircuitBreaker(ctx, cfg)
	default:
		log.Printf("alerts: unknown alert_type %q for config %s — skipping", cfg.AlertType, cfg.Name)
		return false, "", ""
	}
}

// checkQueueDepth counts work_items in the state recorded in cfg.Name (used as
// the target state) and fires if count > threshold.
//
// Convention: the alert config's name encodes the target state to count, e.g.
// "queue_depth:pending". If no colon is present the raw name is used as state.
func (s *AlertService) checkQueueDepth(ctx context.Context, cfg alertConfig) (bool, string, string) {
	// Derive the target state from the alert name (format "queue_depth:<state>")
	// or fall back to the full name.
	targetState := cfg.Name
	for i, c := range cfg.Name {
		if c == ':' {
			targetState = cfg.Name[i+1:]
			break
		}
	}

	var count int
	err := s.db.QueryRow(ctx, `
		SELECT COUNT(*) FROM work_items WHERE state = $1
	`, targetState).Scan(&count)
	if err != nil {
		log.Printf("alerts: checkQueueDepth query: %v", err)
		return false, "", ""
	}

	if float64(count) > cfg.Threshold {
		msg := fmt.Sprintf("Queue depth alert: %d items in state '%s' (threshold %.0f)", count, targetState, cfg.Threshold)
		return true, msg, "warning"
	}
	return false, "", ""
}

// checkStuckTask finds work_items whose updated_at is older than threshold
// minutes and fires if any are found.
func (s *AlertService) checkStuckTask(ctx context.Context, cfg alertConfig) (bool, string, string) {
	thresholdMinutes := cfg.Threshold
	if thresholdMinutes <= 0 {
		thresholdMinutes = 60
	}

	var count int
	err := s.db.QueryRow(ctx, `
		SELECT COUNT(*) FROM work_items
		WHERE state NOT IN ('passed', 'failed', 'blocked')
		  AND updated_at < NOW() - ($1 * INTERVAL '1 minute')
	`, thresholdMinutes).Scan(&count)
	if err != nil {
		log.Printf("alerts: checkStuckTask query: %v", err)
		return false, "", ""
	}

	if count > 0 {
		msg := fmt.Sprintf("Stuck task alert: %d task(s) have not transitioned in over %.0f minutes", count, thresholdMinutes)
		return true, msg, "warning"
	}
	return false, "", ""
}

// checkModelUnhealthy checks whether the error rate of the last 100
// inference_logs exceeds the threshold percentage.
func (s *AlertService) checkModelUnhealthy(ctx context.Context, cfg alertConfig) (bool, string, string) {
	var total, failures int
	err := s.db.QueryRow(ctx, `
		SELECT COUNT(*),
		       COUNT(*) FILTER (WHERE success = false)
		FROM (
		    SELECT success FROM inference_logs
		    ORDER BY created_at DESC
		    LIMIT 100
		) recent
	`).Scan(&total, &failures)
	if err != nil {
		log.Printf("alerts: checkModelUnhealthy query: %v", err)
		return false, "", ""
	}
	if total == 0 {
		return false, "", ""
	}

	errorRate := float64(failures) / float64(total) * 100.0
	if errorRate > cfg.Threshold {
		msg := fmt.Sprintf("Model unhealthy alert: error rate %.1f%% (threshold %.1f%%) over last %d inference logs", errorRate, cfg.Threshold, total)
		return true, msg, "critical"
	}
	return false, "", ""
}

// checkQAFailureRate counts QA failures vs passes in the last 24 hours and
// fires if the failure rate exceeds the threshold percentage.
func (s *AlertService) checkQAFailureRate(ctx context.Context, cfg alertConfig) (bool, string, string) {
	var passes, failures int
	err := s.db.QueryRow(ctx, `
		SELECT
		    COUNT(*) FILTER (WHERE to_state = 'passed') AS passes,
		    COUNT(*) FILTER (WHERE to_state = 'failed') AS failures
		FROM work_item_transitions
		WHERE transitioned_at > NOW() - INTERVAL '24 hours'
		  AND to_state IN ('passed', 'failed')
	`).Scan(&passes, &failures)
	if err != nil {
		log.Printf("alerts: checkQAFailureRate query: %v", err)
		return false, "", ""
	}

	total := passes + failures
	if total == 0 {
		return false, "", ""
	}

	failureRate := float64(failures) / float64(total) * 100.0
	if failureRate > cfg.Threshold {
		msg := fmt.Sprintf("QA failure rate alert: %.1f%% failure rate in last 24h (threshold %.1f%%); %d failed, %d passed", failureRate, cfg.Threshold, failures, passes)
		return true, msg, "warning"
	}
	return false, "", ""
}

// checkCircuitBreaker counts work_items in state 'blocked' and fires if the
// count exceeds the threshold.
func (s *AlertService) checkCircuitBreaker(ctx context.Context, cfg alertConfig) (bool, string, string) {
	var count int
	err := s.db.QueryRow(ctx, `
		SELECT COUNT(*) FROM work_items WHERE state = 'blocked'
	`).Scan(&count)
	if err != nil {
		log.Printf("alerts: checkCircuitBreaker query: %v", err)
		return false, "", ""
	}

	if float64(count) > cfg.Threshold {
		msg := fmt.Sprintf("Circuit breaker alert: %d item(s) in 'blocked' state (threshold %.0f)", count, cfg.Threshold)
		return true, msg, "critical"
	}
	return false, "", ""
}

// alertEvent is the JSON payload published to the "alerts" Kafka topic.
type alertEvent struct {
	AlertConfigID string `json:"alert_config_id"`
	Name          string `json:"name"`
	AlertType     string `json:"alert_type"`
	Message       string `json:"message"`
	Severity      string `json:"severity"`
	FiredAt       string `json:"fired_at"`
}

// fire records the alert in alert_history, publishes it to Kafka, and
// dispatches notifications to configured channels.
func (s *AlertService) fire(ctx context.Context, cfg alertConfig, message, severity string) error {
	// 1. INSERT into alert_history.
	_, err := s.db.Exec(ctx, `
		INSERT INTO alert_history (alert_config_id, message, severity)
		VALUES ($1, $2, $3)
	`, cfg.ID, message, severity)
	if err != nil {
		return fmt.Errorf("insert alert_history: %w", err)
	}

	log.Printf("alerts: fired alert %q (%s): %s", cfg.Name, severity, message)

	// 2. Publish to Kafka "alerts" topic for WebSocket dashboard fanout.
	event := alertEvent{
		AlertConfigID: cfg.ID.String(),
		Name:          cfg.Name,
		AlertType:     cfg.AlertType,
		Message:       message,
		Severity:      severity,
		FiredAt:       time.Now().UTC().Format(time.RFC3339),
	}
	payload, err := json.Marshal(event)
	if err != nil {
		log.Printf("alerts: marshal kafka event: %v", err)
	} else if s.bus != nil {
		if err := s.bus.Publish(ctx, bus.TopicAlerts, cfg.ID.String(), payload); err != nil {
			log.Printf("alerts: publish to kafka: %v", err)
		}
	}

	// 3. Dispatch to notification channels.
	for _, ch := range cfg.Channels {
		switch ch {
		case "slack":
			webhookURL, err := s.resolveSlackWebhook(ctx)
			if err != nil {
				log.Printf("alerts: resolve slack webhook: %v", err)
				continue
			}
			if err := SendSlackNotification(webhookURL, message, severity); err != nil {
				log.Printf("alerts: send slack notification for %q: %v", cfg.Name, err)
			}
		case "email":
			log.Printf("alerts: [email intent] would send email for alert %q (%s): %s", cfg.Name, severity, message)
		default:
			log.Printf("alerts: unknown channel %q for alert %q — skipping", ch, cfg.Name)
		}
	}

	return nil
}

// resolveSlackWebhook looks up the Slack webhook URL from system_config.
func (s *AlertService) resolveSlackWebhook(ctx context.Context) (string, error) {
	var webhookURL string
	err := s.db.QueryRow(ctx, `
		SELECT value FROM system_config
		WHERE key_name = 'slack_webhook_url'
		ORDER BY
		    CASE scope
		        WHEN 'global' THEN 1
		        ELSE 0
		    END DESC
		LIMIT 1
	`).Scan(&webhookURL)
	if err != nil {
		return "", fmt.Errorf("query slack_webhook_url: %w", err)
	}
	if webhookURL == "" {
		return "", fmt.Errorf("slack_webhook_url is empty")
	}
	return webhookURL, nil
}
