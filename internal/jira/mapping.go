package jira

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/configstore"
	"github.com/autonomousengine/autonomous-engine/internal/llm"
	"github.com/autonomousengine/autonomous-engine/internal/llm/strategy"
)

// WorkflowMapping holds the bidirectional mapping between internal pipeline
// states/types/priorities and their Jira equivalents.
type WorkflowMapping struct {
	StatusMapping  map[string]string `json:"status_mapping"`   // internal state → Jira status name
	TransitionMap  map[string]string `json:"transition_map"`   // internal state → Jira transition ID
	IssueTypeMap   map[string]string `json:"issue_type_map"`   // internal type → Jira issue type name
	PriorityMap    map[string]string `json:"priority_map"`     // internal priority → Jira priority name
	RequiredFields map[string]string `json:"required_fields"`  // custom field ID → default value
	IngestJQL      string            `json:"ingest_jql"`       // JQL for pulling tickets
	IngestLabel    string            `json:"ingest_label"`     // label added to ingested tickets
}

// ProposeMapping asks the LLM to map a discovered Jira project's workflow to
// internal pipeline states, types, and priorities.
func ProposeMapping(ctx context.Context, llmService *llm.Service, project *JiraProject) (*WorkflowMapping, error) {
	// Marshal the project structure as context for the LLM
	projectJSON, err := json.MarshalIndent(project, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal project for LLM: %w", err)
	}

	intent := strategy.Intent{
		Type: strategy.IntentMapJiraWorkflow,
		Context: map[string]string{
			"jira_project": string(projectJSON),
		},
		OutputFormat: "JSON object with keys: status_mapping, issue_type_map, priority_map, ingest_jql, ingest_label",
		MaxTokens:    2048,
		Temperature:  0.2,
	}

	result, err := llmService.Execute(ctx, intent)
	if err != nil {
		return nil, fmt.Errorf("LLM propose mapping: %w", err)
	}

	// Extract JSON from the LLM response
	raw := extractJSON(result.Content)
	if raw == "" {
		return nil, fmt.Errorf("LLM response contained no parseable JSON")
	}

	var mapping WorkflowMapping
	if err := json.Unmarshal([]byte(raw), &mapping); err != nil {
		return nil, fmt.Errorf("parse LLM mapping response: %w", err)
	}

	// Ensure maps are initialised
	if mapping.StatusMapping == nil {
		mapping.StatusMapping = make(map[string]string)
	}
	if mapping.TransitionMap == nil {
		mapping.TransitionMap = make(map[string]string)
	}
	if mapping.IssueTypeMap == nil {
		mapping.IssueTypeMap = make(map[string]string)
	}
	if mapping.PriorityMap == nil {
		mapping.PriorityMap = make(map[string]string)
	}
	if mapping.RequiredFields == nil {
		mapping.RequiredFields = make(map[string]string)
	}

	return &mapping, nil
}

// SaveMapping persists each field of the WorkflowMapping to system_config with
// scope=global, scope_target="jira", category="jira".
func SaveMapping(ctx context.Context, cs *configstore.ConfigStore, mapping *WorkflowMapping) error {
	statusJSON, err := json.Marshal(mapping.StatusMapping)
	if err != nil {
		return fmt.Errorf("marshal status_mapping: %w", err)
	}
	transitionJSON, err := json.Marshal(mapping.TransitionMap)
	if err != nil {
		return fmt.Errorf("marshal transition_map: %w", err)
	}
	issueTypeJSON, err := json.Marshal(mapping.IssueTypeMap)
	if err != nil {
		return fmt.Errorf("marshal issue_type_map: %w", err)
	}
	priorityJSON, err := json.Marshal(mapping.PriorityMap)
	if err != nil {
		return fmt.Errorf("marshal priority_map: %w", err)
	}
	requiredJSON, err := json.Marshal(mapping.RequiredFields)
	if err != nil {
		return fmt.Errorf("marshal required_fields: %w", err)
	}

	entries := []struct {
		key   string
		value string
	}{
		{"status_mapping", string(statusJSON)},
		{"transition_map", string(transitionJSON)},
		{"issue_type_map", string(issueTypeJSON)},
		{"priority_map", string(priorityJSON)},
		{"required_fields", string(requiredJSON)},
		{"ingest_jql", mapping.IngestJQL},
		{"ingest_label", mapping.IngestLabel},
	}

	for _, e := range entries {
		// GetConfig's resolver chain terminates at scope='global', scope_target='default'.
		// Writing with scope_target='jira' previously left these entries unreachable —
		// LoadMapping silently returned an empty mapping. Use SetGlobalConfig which
		// hard-codes the canonical convention.
		if err := cs.SetGlobalConfig(ctx,
			e.key, e.value,
			"string", "jira",
			"Jira workflow mapping: "+e.key,
			nil,
		); err != nil {
			return fmt.Errorf("save mapping key %s: %w", e.key, err)
		}
	}

	return nil
}

// LoadMapping reads a WorkflowMapping from system_config.
func LoadMapping(ctx context.Context, cs *configstore.ConfigStore) (*WorkflowMapping, error) {
	getJSON := func(key string, target interface{}) error {
		raw, err := cs.GetConfig(ctx, key, "", "")
		if err != nil {
			return err
		}
		return json.Unmarshal([]byte(raw), target)
	}

	getString := func(key string) string {
		v, _ := cs.GetConfig(ctx, key, "", "")
		return v
	}

	mapping := &WorkflowMapping{
		StatusMapping:  make(map[string]string),
		TransitionMap:  make(map[string]string),
		IssueTypeMap:   make(map[string]string),
		PriorityMap:    make(map[string]string),
		RequiredFields: make(map[string]string),
	}

	// Best-effort: partial load is acceptable
	_ = getJSON("status_mapping", &mapping.StatusMapping)
	_ = getJSON("transition_map", &mapping.TransitionMap)
	_ = getJSON("issue_type_map", &mapping.IssueTypeMap)
	_ = getJSON("priority_map", &mapping.PriorityMap)
	_ = getJSON("required_fields", &mapping.RequiredFields)
	mapping.IngestJQL = getString("ingest_jql")
	mapping.IngestLabel = getString("ingest_label")

	return mapping, nil
}

// DefaultMapping generates a reasonable fallback mapping without the LLM.
// It uses Jira status categories and string similarity between type/priority names.
func DefaultMapping(project *JiraProject) *WorkflowMapping {
	mapping := &WorkflowMapping{
		StatusMapping:  make(map[string]string),
		TransitionMap:  make(map[string]string),
		IssueTypeMap:   make(map[string]string),
		PriorityMap:    make(map[string]string),
		RequiredFields: make(map[string]string),
		IngestJQL:      "project = " + project.Key + " AND labels = autonomous-ingest ORDER BY created DESC",
		IngestLabel:    "autonomous-ingest",
	}

	// Map internal states using Jira status categories as hints
	// new → intake, indeterminate → implementation, done → released
	for _, s := range project.Statuses {
		switch s.Category {
		case "new":
			if _, ok := mapping.StatusMapping["intake"]; !ok {
				mapping.StatusMapping["intake"] = s.Name
				mapping.TransitionMap["intake"] = s.ID
			}
			if _, ok := mapping.StatusMapping["prd"]; !ok {
				mapping.StatusMapping["prd"] = s.Name
			}
		case "indeterminate":
			if _, ok := mapping.StatusMapping["implementation"]; !ok {
				mapping.StatusMapping["implementation"] = s.Name
				mapping.TransitionMap["implementation"] = s.ID
			}
			if _, ok := mapping.StatusMapping["research"]; !ok {
				mapping.StatusMapping["research"] = s.Name
			}
			if _, ok := mapping.StatusMapping["planning"]; !ok {
				mapping.StatusMapping["planning"] = s.Name
			}
			if _, ok := mapping.StatusMapping["verification"]; !ok {
				mapping.StatusMapping["verification"] = s.Name
			}
			if _, ok := mapping.StatusMapping["review"]; !ok {
				mapping.StatusMapping["review"] = s.Name
			}
			if _, ok := mapping.StatusMapping["qa"]; !ok {
				mapping.StatusMapping["qa"] = s.Name
			}
		case "done":
			if _, ok := mapping.StatusMapping["released"]; !ok {
				mapping.StatusMapping["released"] = s.Name
				mapping.TransitionMap["released"] = s.ID
			}
			if _, ok := mapping.StatusMapping["passed"]; !ok {
				mapping.StatusMapping["passed"] = s.Name
			}
		}
	}

	// Map internal types to Jira issue types by name similarity
	internalTypes := []string{"feature", "bug", "task", "subtask", "research", "qa"}
	jiraTypeByName := make(map[string]string) // lower-name → Jira name
	for _, it := range project.IssueTypes {
		jiraTypeByName[strings.ToLower(it.Name)] = it.Name
	}

	for _, t := range internalTypes {
		// Direct match first
		if jiraName, ok := jiraTypeByName[t]; ok {
			mapping.IssueTypeMap[t] = jiraName
			continue
		}
		// Heuristic fallbacks
		switch t {
		case "feature":
			for _, candidate := range []string{"story", "new feature", "epic"} {
				if jiraName, ok := jiraTypeByName[candidate]; ok {
					mapping.IssueTypeMap[t] = jiraName
					break
				}
			}
			if _, ok := mapping.IssueTypeMap[t]; !ok && len(project.IssueTypes) > 0 {
				mapping.IssueTypeMap[t] = project.IssueTypes[0].Name
			}
		case "bug":
			if jiraName, ok := jiraTypeByName["bug"]; ok {
				mapping.IssueTypeMap[t] = jiraName
			}
		case "task", "research", "qa":
			if jiraName, ok := jiraTypeByName["task"]; ok {
				mapping.IssueTypeMap[t] = jiraName
			}
		case "subtask":
			for _, candidate := range []string{"sub-task", "subtask", "sub task"} {
				if jiraName, ok := jiraTypeByName[candidate]; ok {
					mapping.IssueTypeMap[t] = jiraName
					break
				}
			}
			if _, ok := mapping.IssueTypeMap[t]; !ok {
				if jiraName, ok := jiraTypeByName["task"]; ok {
					mapping.IssueTypeMap[t] = jiraName
				}
			}
		}
	}

	// Map internal priorities to Jira priorities by position
	// Jira typically orders: Highest, High, Medium, Low, Lowest
	internalPriorities := []string{"critical", "high", "medium", "low"}
	for i, p := range internalPriorities {
		if i < len(project.Priorities) {
			mapping.PriorityMap[p] = project.Priorities[i].Name
		} else if len(project.Priorities) > 0 {
			mapping.PriorityMap[p] = project.Priorities[len(project.Priorities)-1].Name
		}
	}

	return mapping
}

// extractJSON finds and returns the first JSON object in a string.
func extractJSON(s string) string {
	start := strings.Index(s, "{")
	if start == -1 {
		return ""
	}
	// Find matching closing brace
	depth := 0
	for i := start; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}
