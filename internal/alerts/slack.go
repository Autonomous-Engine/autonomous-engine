package alerts

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
)

// SendSlackNotification POSTs a message to a Slack incoming webhook URL.
// The message is formatted as "[SEVERITY] message" in the text field.
func SendSlackNotification(webhookURL, message, severity string) error {
	payload := map[string]string{
		"text": fmt.Sprintf("[%s] %s", severity, message),
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("slack: marshal payload: %w", err)
	}

	resp, err := http.Post(webhookURL, "application/json", bytes.NewReader(body)) //nolint:noctx
	if err != nil {
		return fmt.Errorf("slack: POST webhook: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("slack: webhook returned status %d", resp.StatusCode)
	}
	return nil
}
