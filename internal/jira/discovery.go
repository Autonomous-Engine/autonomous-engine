package jira

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// JiraProject holds all discovered metadata for a Jira project.
type JiraProject struct {
	Key        string          `json:"key"`
	Name       string          `json:"name"`
	IssueTypes []JiraIssueType `json:"issue_types"`
	Statuses   []JiraStatus    `json:"statuses"`
	Priorities []JiraPriority  `json:"priorities"`
	Fields     []JiraField     `json:"fields"`
	Workflows  []JiraWorkflow  `json:"workflows"`
}

// JiraIssueType represents a Jira issue type (Story, Bug, Task, etc.).
type JiraIssueType struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Subtask     bool   `json:"subtask"`
}

// JiraStatus represents a Jira workflow status.
type JiraStatus struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Category string `json:"category"` // "new", "indeterminate", "done"
}

// JiraPriority represents a Jira priority level.
type JiraPriority struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// JiraField represents a Jira issue field (standard or custom).
type JiraField struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Custom   bool   `json:"custom"`
	Required bool   `json:"required"`
	Type     string `json:"type"`
}

// JiraWorkflow represents a Jira workflow with its transitions.
type JiraWorkflow struct {
	Name        string           `json:"name"`
	Transitions []JiraTransition `json:"transitions"`
}

// JiraTransition represents a transition between two statuses in a workflow.
type JiraTransition struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	FromStatus string `json:"from_status"`
	ToStatus   string `json:"to_status"`
}

// JiraClient is a minimal REST client for the Jira Cloud API v3.
type JiraClient struct {
	baseURL    string
	email      string
	token      string
	httpClient *http.Client
}

// NewClient creates a new JiraClient with Basic auth credentials.
func NewClient(baseURL, email, token string) *JiraClient {
	return &JiraClient{
		baseURL:    baseURL,
		email:      email,
		token:      token,
		httpClient: &http.Client{},
	}
}

// doRequest performs an authenticated GET request and returns the response body bytes.
func (c *JiraClient) doRequest(ctx context.Context, method, path string) ([]byte, error) {
	url := c.baseURL + path
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request %s %s: %w", method, path, err)
	}

	creds := base64.StdEncoding.EncodeToString([]byte(c.email + ":" + c.token))
	req.Header.Set("Authorization", "Basic "+creds)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do request %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body %s %s: %w", method, path, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("jira API %s %s returned %d: %s", method, path, resp.StatusCode, string(body))
	}

	return body, nil
}

// DiscoverProject fetches all metadata for the given Jira project key and returns
// a fully populated JiraProject. It makes several API calls to collect issue types,
// statuses, priorities, fields, and assembles a synthetic workflow from statuses.
func (c *JiraClient) DiscoverProject(ctx context.Context, projectKey string) (*JiraProject, error) {
	project := &JiraProject{Key: projectKey}

	// 1. GET project name
	{
		body, err := c.doRequest(ctx, http.MethodGet, "/rest/api/3/project/"+projectKey)
		if err != nil {
			return nil, fmt.Errorf("discover project info: %w", err)
		}
		var p struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(body, &p); err != nil {
			return nil, fmt.Errorf("parse project info: %w", err)
		}
		project.Name = p.Name
	}

	// 2. GET project statuses (per issue type) → deduplicate into flat list
	{
		body, err := c.doRequest(ctx, http.MethodGet, "/rest/api/3/project/"+projectKey+"/statuses")
		if err != nil {
			return nil, fmt.Errorf("discover project statuses: %w", err)
		}

		var issueTypeStatuses []struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			Statuses []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"statuses"`
		}
		if err := json.Unmarshal(body, &issueTypeStatuses); err != nil {
			return nil, fmt.Errorf("parse project statuses: %w", err)
		}

		seen := make(map[string]bool)
		for _, it := range issueTypeStatuses {
			for _, s := range it.Statuses {
				if !seen[s.ID] {
					seen[s.ID] = true
					project.Statuses = append(project.Statuses, JiraStatus{
						ID:   s.ID,
						Name: s.Name,
					})
				}
			}
		}
	}

	// 3. GET all issue types
	{
		body, err := c.doRequest(ctx, http.MethodGet, "/rest/api/3/issuetype")
		if err != nil {
			return nil, fmt.Errorf("discover issue types: %w", err)
		}

		var raw []struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			Description string `json:"description"`
			Subtask     bool   `json:"subtask"`
		}
		if err := json.Unmarshal(body, &raw); err != nil {
			return nil, fmt.Errorf("parse issue types: %w", err)
		}

		for _, r := range raw {
			project.IssueTypes = append(project.IssueTypes, JiraIssueType{
				ID:          r.ID,
				Name:        r.Name,
				Description: r.Description,
				Subtask:     r.Subtask,
			})
		}
	}

	// 4. GET all priorities
	{
		body, err := c.doRequest(ctx, http.MethodGet, "/rest/api/3/priority")
		if err != nil {
			return nil, fmt.Errorf("discover priorities: %w", err)
		}

		var raw []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		if err := json.Unmarshal(body, &raw); err != nil {
			return nil, fmt.Errorf("parse priorities: %w", err)
		}

		for _, r := range raw {
			project.Priorities = append(project.Priorities, JiraPriority{
				ID:   r.ID,
				Name: r.Name,
			})
		}
	}

	// 5. GET all fields, mark custom ones
	{
		body, err := c.doRequest(ctx, http.MethodGet, "/rest/api/3/field")
		if err != nil {
			return nil, fmt.Errorf("discover fields: %w", err)
		}

		var raw []struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			Custom bool   `json:"custom"`
			Schema *struct {
				Type string `json:"type"`
			} `json:"schema,omitempty"`
		}
		if err := json.Unmarshal(body, &raw); err != nil {
			return nil, fmt.Errorf("parse fields: %w", err)
		}

		for _, r := range raw {
			f := JiraField{
				ID:     r.ID,
				Name:   r.Name,
				Custom: r.Custom,
			}
			if r.Schema != nil {
				f.Type = r.Schema.Type
			}
			project.Fields = append(project.Fields, f)
		}
	}

	// 6. GET all statuses with category names (enriches the flat status list)
	{
		body, err := c.doRequest(ctx, http.MethodGet, "/rest/api/3/status")
		if err != nil {
			return nil, fmt.Errorf("discover all statuses: %w", err)
		}

		var raw []struct {
			ID             string `json:"id"`
			Name           string `json:"name"`
			StatusCategory struct {
				Key string `json:"key"`
			} `json:"statusCategory"`
		}
		if err := json.Unmarshal(body, &raw); err != nil {
			return nil, fmt.Errorf("parse all statuses: %w", err)
		}

		// Build lookup map from ID to category key
		catByID := make(map[string]string, len(raw))
		for _, r := range raw {
			catByID[r.ID] = r.StatusCategory.Key
		}

		// Enrich the statuses already collected from project/statuses
		enriched := make([]JiraStatus, 0, len(project.Statuses))
		for _, s := range project.Statuses {
			s.Category = catByID[s.ID]
			enriched = append(enriched, s)
		}
		project.Statuses = enriched

		// If the project/statuses call returned nothing, fall back to all statuses
		if len(project.Statuses) == 0 {
			for _, r := range raw {
				project.Statuses = append(project.Statuses, JiraStatus{
					ID:       r.ID,
					Name:     r.Name,
					Category: r.StatusCategory.Key,
				})
			}
		}
	}

	// 7. Assemble a synthetic workflow from the collected statuses
	workflow := JiraWorkflow{
		Name: projectKey + " Workflow",
	}
	for _, s := range project.Statuses {
		workflow.Transitions = append(workflow.Transitions, JiraTransition{
			ID:       s.ID,
			Name:     "Transition to " + s.Name,
			ToStatus: s.Name,
		})
	}
	project.Workflows = []JiraWorkflow{workflow}

	return project, nil
}
