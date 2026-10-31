package main

import "testing"

// A payload with no trigger and no repository_id / product_id is a
// notification, not an index request: handleKafkaMessage skips it. Every
// payload that sets a trigger or names a repository or product indexes.
func TestTriggerlessNotificationsDoNotIndex(t *testing.T) {
	cases := []struct {
		name        string
		payload     string
		wantIndexed bool
	}{
		{
			// onboarding approve-codex notification.
			name:        "approve-codex notification is skipped",
			payload:     `{"session_id":"s-1","version_number":3,"source":"onboarding"}`,
			wantIndexed: false,
		},
		{
			// webhook-listener retryDeadLetter of a raw GitHub push body.
			name:        "dead-letter replay of a raw push is skipped",
			payload:     `{"ref":"refs/heads/main","after":"abc123","commits":[{"id":"abc123","message":"fix","author":{"name":"a"}}],"repository":{"full_name":"x/app"},"pusher":{"name":"a"},"head_commit":{"id":"abc123","message":"fix","author":{"name":"a"}}}`,
			wantIndexed: false,
		},
		{
			// webhook-listener pushEvents for a registered repository: the
			// externalChangeEvent with repository_id, plus the event_id.
			name:        "webhook push of a registered repository is indexed",
			payload:     `{"source":"github","branch":"main","commit_sha":"abc123","is_agent":false,"timestamp":"2026-10-03T10:00:00Z","repository_id":"1b4e28ba-2fa1-11d2-883f-0016d3cca427","event_id":"6f1c0a52-58a4-4e43-9a43-1f2b9b8f7b21"}`,
			wantIndexed: true,
		},
		{
			// The same push of an unregistered repository: no repository_id.
			name:        "webhook push of an unregistered repository is skipped",
			payload:     `{"source":"github","branch":"main","commit_sha":"abc123","is_agent":true,"timestamp":"2026-10-03T10:00:00Z","event_id":"6f1c0a52-58a4-4e43-9a43-1f2b9b8f7b21"}`,
			wantIndexed: false,
		},
		{
			// webhook-listener jiraChangeEvent: no repository context. Before
			// F3 it reached the default branch and indexed the legacy
			// REPO_PATH; it is now a notification.
			name:        "jira external-change is skipped",
			payload:     `{"issue_key":"AE-12","event_type":"jira:issue_updated","field":"priority","from":"Low","to":"High","timestamp":"2026-10-03T10:00:00Z","event_id":"0e7a0c51-2c8b-4d3a-9b1e-6d0f3a2b1c4d"}`,
			wantIndexed: false,
		},
		{
			// change-reconciler codexUpdatedMessage resolved to a product.
			name:        "change-reconciler codex.updated with product_id is indexed",
			payload:     `{"source":"github","commit_sha":"abc123","timestamp":"2026-10-03T10:00:00Z","product_id":"8d0b5b8e-2b8f-4a55-9a77-0d6f4a1d9e10","event_id":"0e7a0c51-2c8b-4d3a-9b1e-6d0f3a2b1c4d"}`,
			wantIndexed: true,
		},
		{
			// change-reconciler codexUpdatedMessage that resolved to no product.
			name:        "change-reconciler codex.updated without a product is skipped",
			payload:     `{"source":"github","commit_sha":"abc123","timestamp":"2026-10-03T10:00:00Z"}`,
			wantIndexed: false,
		},
		{
			name:        "status-orchestrator post_merge is indexed",
			payload:     `{"trigger":"post_merge","repository_id":"1b4e28ba-2fa1-11d2-883f-0016d3cca427","since":"HEAD~1"}`,
			wantIndexed: true,
		},
		{
			name:        "onboarding initial scan is indexed",
			payload:     `{"session_id":"s-1","trigger":"onboarding_initial_scan","product_id":"8d0b5b8e-2b8f-4a55-9a77-0d6f4a1d9e10"}`,
			wantIndexed: true,
		},
		{
			name:        "product_id alone is an index request",
			payload:     `{"product_id":"8d0b5b8e-2b8f-4a55-9a77-0d6f4a1d9e10"}`,
			wantIndexed: true,
		},
		{
			name:        "non-string repository_id does not count as repository context",
			payload:     `{"repository_id":42}`,
			wantIndexed: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr, err := decodeIndexTrigger([]byte(tc.payload))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got := !tr.notification(); got != tc.wantIndexed {
				t.Fatalf("indexed = %v, want %v (%+v)", got, tc.wantIndexed, tr)
			}
		})
	}
}
