package main

import (
	"encoding/json"
	"testing"
)

// The payloads below copy the shapes the real producers put on
// codex.updated and external-change.detected (see the producer named in each
// case). Several carry non-string values, which the old map[string]string
// decode rejected, so the indexer skipped them.
func TestDecodeIndexTrigger_ProducerShapes(t *testing.T) {
	cases := []struct {
		name       string
		payload    string
		want       indexTrigger
		oldSkipped bool // the map[string]string decode failed on it
	}{
		{
			// webhook-listener pushEvents: externalChangeEvent plus the
			// outbox event_id the listener embeds.
			name:       "webhook push external-change with is_agent bool",
			payload:    `{"source":"github","branch":"main","commit_sha":"abc123","is_agent":true,"timestamp":"2026-10-03T10:00:00Z","event_id":"6f1c0a52-58a4-4e43-9a43-1f2b9b8f7b21"}`,
			want:       indexTrigger{Since: "HEAD~1"},
			oldSkipped: true,
		},
		{
			// webhook-listener jiraChangeEvent (all strings).
			name:    "webhook jira external-change",
			payload: `{"issue_key":"AE-12","event_type":"jira:issue_updated","field":"priority","from":"Low","to":"High","timestamp":"2026-10-03T10:00:00Z","event_id":"0e7a0c51-2c8b-4d3a-9b1e-6d0f3a2b1c4d"}`,
			want:    indexTrigger{Since: "HEAD~1"},
		},
		{
			// webhook-listener retryDeadLetter: the raw GitHub push body,
			// with nested objects and arrays.
			name:       "dead-letter replay of a raw github push",
			payload:    `{"ref":"refs/heads/main","after":"abc123","commits":[{"id":"abc123","message":"fix","author":{"name":"a"}}],"repository":{"full_name":"x/app"},"pusher":{"name":"a"},"head_commit":{"id":"abc123","message":"fix","author":{"name":"a"}}}`,
			want:       indexTrigger{Since: "HEAD~1"},
			oldSkipped: true,
		},
		{
			// onboarding run-analysis trigger.
			name:    "onboarding initial scan",
			payload: `{"session_id":"s-1","trigger":"onboarding_initial_scan","product_id":"8d0b5b8e-2b8f-4a55-9a77-0d6f4a1d9e10"}`,
			want:    indexTrigger{Trigger: "onboarding_initial_scan", Since: "HEAD~1", ProductID: "8d0b5b8e-2b8f-4a55-9a77-0d6f4a1d9e10"},
		},
		{
			// onboarding approve-codex notification: integer version_number.
			name:       "onboarding approve-codex with version_number int",
			payload:    `{"session_id":"s-1","version_number":3,"source":"onboarding"}`,
			want:       indexTrigger{Since: "HEAD~1"},
			oldSkipped: true,
		},
		{
			// change-reconciler codexUpdatedMessage.
			name:    "change-reconciler minor change",
			payload: `{"source":"github","commit_sha":"abc123","timestamp":"2026-10-03T10:00:00Z"}`,
			want:    indexTrigger{Since: "HEAD~1"},
		},
		{
			// status-orchestrator pr.merged follow-up.
			name:    "status-orchestrator post_merge",
			payload: `{"trigger":"post_merge","repository_id":"1b4e28ba-2fa1-11d2-883f-0016d3cca427","since":"HEAD~1"}`,
			want:    indexTrigger{Trigger: "post_merge", Since: "HEAD~1", RepositoryID: "1b4e28ba-2fa1-11d2-883f-0016d3cca427"},
		},
		{
			// knowledge-indexer enqueueCodexUpdated (self-trigger).
			name:    "indexer self-trigger",
			payload: `{"source":"knowledge-indexer","trigger":"incremental_index","timestamp":"2026-10-03T10:00:00Z"}`,
			want:    indexTrigger{Trigger: "incremental_index", Since: "HEAD~1"},
		},
		{
			name:       "explicit since kept, unknown fields of every type ignored",
			payload:    `{"trigger":"post_merge","since":"abc~2","extra":{"a":[1,2]},"n":1.5,"flag":false,"z":null}`,
			want:       indexTrigger{Trigger: "post_merge", Since: "abc~2"},
			oldSkipped: true,
		},
		{
			name:       "known key with a non-string value is treated as absent",
			payload:    `{"trigger":"post_merge","product_id":42,"since":null}`,
			want:       indexTrigger{Trigger: "post_merge", Since: "HEAD~1"},
			oldSkipped: true,
		},
		{
			name:    "json null is the zero trigger",
			payload: `null`,
			want:    indexTrigger{Since: "HEAD~1"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeIndexTrigger([]byte(tc.payload))
			if err != nil {
				t.Fatalf("decodeIndexTrigger: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
			var old map[string]string
			oldErr := json.Unmarshal([]byte(tc.payload), &old)
			if (oldErr != nil) != tc.oldSkipped {
				t.Errorf("fixture drift: old decode err = %v, oldSkipped = %v", oldErr, tc.oldSkipped)
			}
		})
	}
}

func TestDecodeIndexTrigger_RejectsNonObjects(t *testing.T) {
	for _, p := range []string{`[1,2]`, `"post_merge"`, `7`, `{not json`, ``} {
		if _, err := decodeIndexTrigger([]byte(p)); err == nil {
			t.Errorf("decodeIndexTrigger(%q) = nil error, want one", p)
		}
	}
}
