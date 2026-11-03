package main

import "net/http"

// jiraIngestKeys are the system_config keys (category jira) the
// Integrations page edits through /api/admin/config/update.
var jiraIngestKeys = []string{"ingest_enabled", "ingest_jql", "polling_interval", "ingest_label"}

// GetJiraIngestConfig handles GET /api/admin/config/jira-ingest: the four
// ingest settings as strings, "" when unset. The page loaded this on every
// visit and got a 404 because the route did not exist (F11).
func (h *AdminHandlers) GetJiraIngestConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	out := map[string]string{}
	for _, k := range jiraIngestKeys {
		out[k] = ""
	}
	if h.store != nil {
		entries, err := h.store.ListConfig(r.Context(), "jira")
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		for _, e := range entries {
			if _, want := out[e.KeyName]; want && e.Scope == "global" {
				out[e.KeyName] = e.Value
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}
