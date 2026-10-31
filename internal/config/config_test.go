package config

import (
	"os"
	"testing"
)

// setEnv is a helper that sets multiple environment variables at once and
// returns a cleanup function that unsets them all.
func setEnv(t *testing.T, pairs map[string]string) func() {
	t.Helper()
	for k, v := range pairs {
		if err := os.Setenv(k, v); err != nil {
			t.Fatalf("setenv %s: %v", k, err)
		}
	}
	return func() {
		for k := range pairs {
			os.Unsetenv(k)
		}
	}
}

// TestLoadFromEnv verifies that Load correctly reads all fields from
// environment variables when every variable is explicitly set.
func TestLoadFromEnv(t *testing.T) {
	cleanup := setEnv(t, map[string]string{
		// Required
		"DATABASE_URL":  "postgres://user:pass@localhost:5432/testdb",
		"KAFKA_BROKERS": "broker1:9092,broker2:9092, broker3:9092",
		// Model
		"MODEL_PROVIDER":          "openai",
		"MODEL_ENDPOINT":          "https://api.openai.com/v1",
		"MODEL_NAME":              "gpt-4",
		"MODEL_API_KEY":           "sk-testkey",
		"MODEL_FALLBACK_PROVIDER": "anthropic",
		"MODEL_FALLBACK_ENDPOINT": "https://api.anthropic.com",
		"MODEL_FALLBACK_API_KEY":  "anth-testkey",
		// GitHub
		"GITHUB_TOKEN":          "ghp_token",
		"GITHUB_REPO":           "org/repo",
		"GITHUB_WEBHOOK_SECRET": "webhooksecret",
		"GITHUB_BASE_BRANCH":    "develop",
		"GITHUB_MERGE_STRATEGY": "merge",
		// Jira
		"JIRA_URL":              "https://example.atlassian.net",
		"JIRA_EMAIL":            "user@example.com",
		"JIRA_API_TOKEN":        "jira-api-token",
		"JIRA_PROJECT_KEY":      "PROJ",
		"JIRA_WORKFLOW_MAPPING": "todo:In Progress,done:Done",
		// Qdrant
		"QDRANT_URL": "http://qdrant:6333",
		// System
		"RESEARCH_POOL_SIZE":    "10",
		"IMPL_POOL_SIZE":        "6",
		"QA_POOL_SIZE":          "4",
		"MAX_RETRY_COUNT":       "5",
		"HUMAN_REVIEW_REQUIRED": "false",
		"DASHBOARD_PORT":        "9080",
		"WEBHOOK_PORT":          "9081",
		"GATEWAY_PORT":          "9082",
	})
	defer cleanup()

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}

	// Database
	if cfg.Database.URL != "postgres://user:pass@localhost:5432/testdb" {
		t.Errorf("Database.URL = %q; want %q", cfg.Database.URL, "postgres://user:pass@localhost:5432/testdb")
	}

	// Kafka brokers — white-space trimming should be applied
	wantBrokers := []string{"broker1:9092", "broker2:9092", "broker3:9092"}
	if len(cfg.Kafka.Brokers) != len(wantBrokers) {
		t.Fatalf("Kafka.Brokers len = %d; want %d", len(cfg.Kafka.Brokers), len(wantBrokers))
	}
	for i, b := range wantBrokers {
		if cfg.Kafka.Brokers[i] != b {
			t.Errorf("Kafka.Brokers[%d] = %q; want %q", i, cfg.Kafka.Brokers[i], b)
		}
	}

	// Model
	if cfg.Model.Provider != "openai" {
		t.Errorf("Model.Provider = %q; want %q", cfg.Model.Provider, "openai")
	}
	if cfg.Model.Endpoint != "https://api.openai.com/v1" {
		t.Errorf("Model.Endpoint = %q; want %q", cfg.Model.Endpoint, "https://api.openai.com/v1")
	}
	if cfg.Model.Name != "gpt-4" {
		t.Errorf("Model.Name = %q; want %q", cfg.Model.Name, "gpt-4")
	}
	if cfg.Model.APIKey != "sk-testkey" {
		t.Errorf("Model.APIKey = %q; want %q", cfg.Model.APIKey, "sk-testkey")
	}
	if cfg.Model.FallbackProvider != "anthropic" {
		t.Errorf("Model.FallbackProvider = %q; want %q", cfg.Model.FallbackProvider, "anthropic")
	}
	if cfg.Model.FallbackEndpoint != "https://api.anthropic.com" {
		t.Errorf("Model.FallbackEndpoint = %q; want %q", cfg.Model.FallbackEndpoint, "https://api.anthropic.com")
	}
	if cfg.Model.FallbackAPIKey != "anth-testkey" {
		t.Errorf("Model.FallbackAPIKey = %q; want %q", cfg.Model.FallbackAPIKey, "anth-testkey")
	}

	// GitHub
	if cfg.GitHub.Token != "ghp_token" {
		t.Errorf("GitHub.Token = %q; want %q", cfg.GitHub.Token, "ghp_token")
	}
	if cfg.GitHub.Repo != "org/repo" {
		t.Errorf("GitHub.Repo = %q; want %q", cfg.GitHub.Repo, "org/repo")
	}
	if cfg.GitHub.WebhookSecret != "webhooksecret" {
		t.Errorf("GitHub.WebhookSecret = %q; want %q", cfg.GitHub.WebhookSecret, "webhooksecret")
	}
	if cfg.GitHub.BaseBranch != "develop" {
		t.Errorf("GitHub.BaseBranch = %q; want %q", cfg.GitHub.BaseBranch, "develop")
	}
	if cfg.GitHub.MergeStrategy != "merge" {
		t.Errorf("GitHub.MergeStrategy = %q; want %q", cfg.GitHub.MergeStrategy, "merge")
	}

	// Jira
	if cfg.Jira.URL != "https://example.atlassian.net" {
		t.Errorf("Jira.URL = %q; want %q", cfg.Jira.URL, "https://example.atlassian.net")
	}
	if cfg.Jira.Email != "user@example.com" {
		t.Errorf("Jira.Email = %q; want %q", cfg.Jira.Email, "user@example.com")
	}
	if cfg.Jira.APIToken != "jira-api-token" {
		t.Errorf("Jira.APIToken = %q; want %q", cfg.Jira.APIToken, "jira-api-token")
	}
	if cfg.Jira.ProjectKey != "PROJ" {
		t.Errorf("Jira.ProjectKey = %q; want %q", cfg.Jira.ProjectKey, "PROJ")
	}
	if cfg.Jira.WorkflowMapping != "todo:In Progress,done:Done" {
		t.Errorf("Jira.WorkflowMapping = %q; want %q", cfg.Jira.WorkflowMapping, "todo:In Progress,done:Done")
	}

	// Qdrant
	if cfg.Qdrant.URL != "http://qdrant:6333" {
		t.Errorf("Qdrant.URL = %q; want %q", cfg.Qdrant.URL, "http://qdrant:6333")
	}

	// System
	if cfg.System.ResearchPoolSize != 10 {
		t.Errorf("System.ResearchPoolSize = %d; want 10", cfg.System.ResearchPoolSize)
	}
	if cfg.System.ImplPoolSize != 6 {
		t.Errorf("System.ImplPoolSize = %d; want 6", cfg.System.ImplPoolSize)
	}
	if cfg.System.QAPoolSize != 4 {
		t.Errorf("System.QAPoolSize = %d; want 4", cfg.System.QAPoolSize)
	}
	if cfg.System.MaxRetryCount != 5 {
		t.Errorf("System.MaxRetryCount = %d; want 5", cfg.System.MaxRetryCount)
	}
	if cfg.System.HumanReviewRequired != false {
		t.Errorf("System.HumanReviewRequired = %v; want false", cfg.System.HumanReviewRequired)
	}
	if cfg.System.DashboardPort != 9080 {
		t.Errorf("System.DashboardPort = %d; want 9080", cfg.System.DashboardPort)
	}
	if cfg.System.WebhookPort != 9081 {
		t.Errorf("System.WebhookPort = %d; want 9081", cfg.System.WebhookPort)
	}
	if cfg.System.GatewayPort != 9082 {
		t.Errorf("System.GatewayPort = %d; want 9082", cfg.System.GatewayPort)
	}
}

// TestLoadDefaults verifies that Load applies all documented defaults when
// only the required environment variables are set.
func TestLoadDefaults(t *testing.T) {
	cleanup := setEnv(t, map[string]string{
		"DATABASE_URL":  "postgres://localhost/defaultdb",
		"KAFKA_BROKERS": "localhost:9092",
	})
	defer cleanup()

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}

	// GitHub defaults
	if cfg.GitHub.BaseBranch != "main" {
		t.Errorf("GitHub.BaseBranch = %q; want %q", cfg.GitHub.BaseBranch, "main")
	}
	if cfg.GitHub.MergeStrategy != "squash" {
		t.Errorf("GitHub.MergeStrategy = %q; want %q", cfg.GitHub.MergeStrategy, "squash")
	}

	// Qdrant default
	if cfg.Qdrant.URL != "http://localhost:6333" {
		t.Errorf("Qdrant.URL = %q; want %q", cfg.Qdrant.URL, "http://localhost:6333")
	}

	// System defaults
	if cfg.System.ResearchPoolSize != 5 {
		t.Errorf("System.ResearchPoolSize = %d; want 5", cfg.System.ResearchPoolSize)
	}
	if cfg.System.ImplPoolSize != 3 {
		t.Errorf("System.ImplPoolSize = %d; want 3", cfg.System.ImplPoolSize)
	}
	if cfg.System.QAPoolSize != 2 {
		t.Errorf("System.QAPoolSize = %d; want 2", cfg.System.QAPoolSize)
	}
	if cfg.System.MaxRetryCount != 3 {
		t.Errorf("System.MaxRetryCount = %d; want 3", cfg.System.MaxRetryCount)
	}
	if cfg.System.HumanReviewRequired != true {
		t.Errorf("System.HumanReviewRequired = %v; want true", cfg.System.HumanReviewRequired)
	}
	if cfg.System.DashboardPort != 8080 {
		t.Errorf("System.DashboardPort = %d; want 8080", cfg.System.DashboardPort)
	}
	if cfg.System.WebhookPort != 8081 {
		t.Errorf("System.WebhookPort = %d; want 8081", cfg.System.WebhookPort)
	}
	if cfg.System.GatewayPort != 8082 {
		t.Errorf("System.GatewayPort = %d; want 8082", cfg.System.GatewayPort)
	}

	// Optional fields should be empty strings when not set
	if cfg.Model.Provider != "" {
		t.Errorf("Model.Provider = %q; want empty string", cfg.Model.Provider)
	}
	if cfg.GitHub.Token != "" {
		t.Errorf("GitHub.Token = %q; want empty string", cfg.GitHub.Token)
	}
	if cfg.Jira.URL != "" {
		t.Errorf("Jira.URL = %q; want empty string", cfg.Jira.URL)
	}
}

// TestLoadMissingDatabaseURL verifies that Load returns an error when
// DATABASE_URL is absent.
func TestLoadMissingDatabaseURL(t *testing.T) {
	// Ensure the variable is unset for this test regardless of the environment.
	old := os.Getenv("DATABASE_URL")
	os.Unsetenv("DATABASE_URL")
	defer func() {
		if old != "" {
			os.Setenv("DATABASE_URL", old)
		}
	}()

	oldKafka := os.Getenv("KAFKA_BROKERS")
	os.Setenv("KAFKA_BROKERS", "localhost:9092")
	defer func() {
		if oldKafka != "" {
			os.Setenv("KAFKA_BROKERS", oldKafka)
		} else {
			os.Unsetenv("KAFKA_BROKERS")
		}
	}()

	_, err := Load()
	if err == nil {
		t.Fatal("Load() expected error for missing DATABASE_URL, got nil")
	}
}

// TestLoadMissingKafkaBrokers verifies that Load returns an error when
// KAFKA_BROKERS is absent.
func TestLoadMissingKafkaBrokers(t *testing.T) {
	old := os.Getenv("KAFKA_BROKERS")
	os.Unsetenv("KAFKA_BROKERS")
	defer func() {
		if old != "" {
			os.Setenv("KAFKA_BROKERS", old)
		}
	}()

	oldDB := os.Getenv("DATABASE_URL")
	os.Setenv("DATABASE_URL", "postgres://localhost/testdb")
	defer func() {
		if oldDB != "" {
			os.Setenv("DATABASE_URL", oldDB)
		} else {
			os.Unsetenv("DATABASE_URL")
		}
	}()

	_, err := Load()
	if err == nil {
		t.Fatal("Load() expected error for missing KAFKA_BROKERS, got nil")
	}
}

// TestSplitBrokers exercises the broker string parsing helper directly.
func TestSplitBrokers(t *testing.T) {
	tests := []struct {
		input string
		want  []string
	}{
		{"broker1:9092", []string{"broker1:9092"}},
		{"broker1:9092,broker2:9092", []string{"broker1:9092", "broker2:9092"}},
		{" broker1:9092 , broker2:9092 ", []string{"broker1:9092", "broker2:9092"}},
		{"broker1:9092,,broker2:9092", []string{"broker1:9092", "broker2:9092"}},
	}

	for _, tc := range tests {
		got := splitBrokers(tc.input)
		if len(got) != len(tc.want) {
			t.Errorf("splitBrokers(%q) len = %d; want %d", tc.input, len(got), len(tc.want))
			continue
		}
		for i, b := range tc.want {
			if got[i] != b {
				t.Errorf("splitBrokers(%q)[%d] = %q; want %q", tc.input, i, got[i], b)
			}
		}
	}
}

// TestLoadRedisURL verifies that REDIS_URL is optional: empty means Redis is
// disabled, and a set value is passed through trimmed.
func TestLoadRedisURL(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/x")
	t.Setenv("KAFKA_BROKERS", "localhost:9092")

	t.Setenv("REDIS_URL", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Redis.URL != "" || cfg.Redis.Enabled() {
		t.Fatalf("Redis = %+v, want disabled", cfg.Redis)
	}
	if RedisURL() != "" {
		t.Fatalf("RedisURL() = %q, want empty", RedisURL())
	}

	t.Setenv("REDIS_URL", " redis://redis:6379/0 ")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Redis.URL != "redis://redis:6379/0" || !cfg.Redis.Enabled() {
		t.Fatalf("Redis = %+v, want enabled with trimmed URL", cfg.Redis)
	}
}
