package jira

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewClientCreatesClientWithCorrectFields(t *testing.T) {
	c := NewClient("https://mycompany.atlassian.net", "user@example.com", "api-token-123")
	if c == nil {
		t.Fatal("NewClient returned nil")
	}
	if c.baseURL != "https://mycompany.atlassian.net" {
		t.Errorf("baseURL = %q, want %q", c.baseURL, "https://mycompany.atlassian.net")
	}
	if c.email != "user@example.com" {
		t.Errorf("email = %q, want %q", c.email, "user@example.com")
	}
	if c.token != "api-token-123" {
		t.Errorf("token = %q, want %q", c.token, "api-token-123")
	}
	if c.httpClient == nil {
		t.Error("httpClient is nil")
	}
}

func TestDiscoverProjectParsesAllFields(t *testing.T) {
	// Build a test server that handles all six API calls made by DiscoverProject.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch r.URL.Path {
		case "/rest/api/3/project/TEST":
			json.NewEncoder(w).Encode(map[string]string{"name": "Test Project"})

		case "/rest/api/3/project/TEST/statuses":
			json.NewEncoder(w).Encode([]map[string]interface{}{
				{
					"id":   "10000",
					"name": "Story",
					"statuses": []map[string]string{
						{"id": "1", "name": "To Do"},
						{"id": "2", "name": "In Progress"},
					},
				},
			})

		case "/rest/api/3/issuetype":
			json.NewEncoder(w).Encode([]map[string]interface{}{
				{"id": "10001", "name": "Story", "description": "A user story", "subtask": false},
				{"id": "10002", "name": "Bug", "description": "A bug", "subtask": false},
				{"id": "10003", "name": "Sub-task", "description": "", "subtask": true},
			})

		case "/rest/api/3/priority":
			json.NewEncoder(w).Encode([]map[string]string{
				{"id": "1", "name": "Highest"},
				{"id": "2", "name": "High"},
				{"id": "3", "name": "Medium"},
				{"id": "4", "name": "Low"},
			})

		case "/rest/api/3/field":
			json.NewEncoder(w).Encode([]map[string]interface{}{
				{"id": "summary", "name": "Summary", "custom": false, "schema": map[string]string{"type": "string"}},
				{"id": "customfield_10001", "name": "Story Points", "custom": true, "schema": map[string]string{"type": "number"}},
			})

		case "/rest/api/3/status":
			json.NewEncoder(w).Encode([]map[string]interface{}{
				{"id": "1", "name": "To Do", "statusCategory": map[string]string{"key": "new"}},
				{"id": "2", "name": "In Progress", "statusCategory": map[string]string{"key": "indeterminate"}},
			})

		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "user@test.com", "token")
	project, err := client.DiscoverProject(context.Background(), "TEST")
	if err != nil {
		t.Fatalf("DiscoverProject error: %v", err)
	}

	if project.Key != "TEST" {
		t.Errorf("Key = %q, want %q", project.Key, "TEST")
	}
	if project.Name != "Test Project" {
		t.Errorf("Name = %q, want %q", project.Name, "Test Project")
	}
	if len(project.IssueTypes) != 3 {
		t.Errorf("IssueTypes len = %d, want 3", len(project.IssueTypes))
	}
	if len(project.Priorities) != 4 {
		t.Errorf("Priorities len = %d, want 4", len(project.Priorities))
	}
	if len(project.Fields) != 2 {
		t.Errorf("Fields len = %d, want 2", len(project.Fields))
	}
	// Statuses should be enriched with category from /rest/api/3/status
	if len(project.Statuses) != 2 {
		t.Errorf("Statuses len = %d, want 2", len(project.Statuses))
	}
	// Verify category enrichment
	for _, s := range project.Statuses {
		if s.Category == "" {
			t.Errorf("status %q has empty Category after enrichment", s.Name)
		}
	}
	// Workflows should be assembled
	if len(project.Workflows) != 1 {
		t.Errorf("Workflows len = %d, want 1", len(project.Workflows))
	}
}

func TestDiscoverProjectReturnsErrorOnAPIFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("project not found"))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "user@test.com", "token")
	_, err := client.DiscoverProject(context.Background(), "NOPROJECT")
	if err == nil {
		t.Error("expected error when API returns 404, got nil")
	}
}

func TestDoRequestSetsBasicAuthHeader(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "admin@example.com", "mytoken")
	_, err := client.doRequest(context.Background(), http.MethodGet, "/some/path")
	if err != nil {
		t.Fatalf("doRequest error: %v", err)
	}

	if gotAuth == "" {
		t.Error("Authorization header was not set")
	}
	if len(gotAuth) < 6 || gotAuth[:6] != "Basic " {
		t.Errorf("Authorization header = %q, want 'Basic ...'", gotAuth)
	}
}
