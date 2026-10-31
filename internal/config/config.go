package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// ModelConfig holds LLM provider settings.
type ModelConfig struct {
	Provider         string
	Endpoint         string
	Name             string
	APIKey           string
	FallbackProvider string
	FallbackEndpoint string
	FallbackAPIKey   string
}

// GitHubConfig holds GitHub integration settings.
type GitHubConfig struct {
	Token         string
	Repo          string
	WebhookSecret string
	BaseBranch    string
	MergeStrategy string
}

// JiraConfig holds Jira integration settings.
type JiraConfig struct {
	URL             string
	Email           string
	APIToken        string
	ProjectKey      string
	WorkflowMapping string
}

// DatabaseConfig holds database connection settings.
type DatabaseConfig struct {
	URL string
}

// KafkaConfig holds Kafka broker settings.
type KafkaConfig struct {
	Brokers []string
}

// QdrantConfig holds Qdrant vector store settings.
type QdrantConfig struct {
	URL string
}

// RedisConfig holds the optional Redis connection. Redis is used only for
// state that must be shared between replicas of a service and may be lost
// on restart: the dashboard WebSocket fan-out and the shared rate limits.
// An empty URL disables it, and every user falls back to its in-process
// behaviour, so a single-node install never needs Redis.
//
// A malformed REDIS_URL is handled in one of two ways, by design:
//   - dashboard-backend and portal refuse to start (sharedstate.Open error
//     is fatal): an operator who set the URL asked for shared fan-out and
//     limits, and silently running per-process would be wrong with several
//     replicas.
//   - services that only use it through the LLM router (sharedstate.FromEnv)
//     log the error once and run with in-process rate limits, so a typo
//     cannot stop the pipeline agents.
type RedisConfig struct {
	URL string
}

// Enabled reports whether a Redis URL is configured.
func (r RedisConfig) Enabled() bool { return r.URL != "" }

// RedisURL returns the trimmed REDIS_URL environment variable; empty means
// Redis is disabled. Packages that build their own clients (the LLM router,
// the rate limiters) read it through here so the variable has one name.
func RedisURL() string {
	return strings.TrimSpace(os.Getenv("REDIS_URL"))
}

// SystemConfig holds system-level operational settings.
type SystemConfig struct {
	ResearchPoolSize    int
	ImplPoolSize        int
	QAPoolSize          int
	MaxRetryCount       int
	HumanReviewRequired bool
	DashboardPort       int
	WebhookPort         int
	GatewayPort         int
}

// Config is the top-level configuration structure for the autonomous engine.
type Config struct {
	Model    ModelConfig
	GitHub   GitHubConfig
	Jira     JiraConfig
	Database DatabaseConfig
	Kafka    KafkaConfig
	Qdrant   QdrantConfig
	Redis    RedisConfig
	System   SystemConfig
}

// Load reads configuration from environment variables and returns a populated
// Config. DATABASE_URL and KAFKA_BROKERS are required; all other fields have
// sensible defaults.
func Load() (*Config, error) {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		return nil, fmt.Errorf("required environment variable DATABASE_URL is not set")
	}

	kafkaBrokersRaw := os.Getenv("KAFKA_BROKERS")
	if kafkaBrokersRaw == "" {
		return nil, fmt.Errorf("required environment variable KAFKA_BROKERS is not set")
	}

	cfg := &Config{
		Model: ModelConfig{
			Provider:         getEnv("MODEL_PROVIDER", ""),
			Endpoint:         getEnv("MODEL_ENDPOINT", ""),
			Name:             getEnv("MODEL_NAME", ""),
			APIKey:           getEnv("MODEL_API_KEY", ""),
			FallbackProvider: getEnv("MODEL_FALLBACK_PROVIDER", ""),
			FallbackEndpoint: getEnv("MODEL_FALLBACK_ENDPOINT", ""),
			FallbackAPIKey:   getEnv("MODEL_FALLBACK_API_KEY", ""),
		},
		GitHub: GitHubConfig{
			Token:         getEnv("GITHUB_TOKEN", ""),
			Repo:          getEnv("GITHUB_REPO", ""),
			WebhookSecret: getEnv("GITHUB_WEBHOOK_SECRET", ""),
			BaseBranch:    getEnv("GITHUB_BASE_BRANCH", "main"),
			MergeStrategy: getEnv("GITHUB_MERGE_STRATEGY", "squash"),
		},
		Jira: JiraConfig{
			URL:             getEnv("JIRA_URL", ""),
			Email:           getEnv("JIRA_EMAIL", ""),
			APIToken:        getEnv("JIRA_API_TOKEN", ""),
			ProjectKey:      getEnv("JIRA_PROJECT_KEY", ""),
			WorkflowMapping: getEnv("JIRA_WORKFLOW_MAPPING", ""),
		},
		Database: DatabaseConfig{
			URL: dbURL,
		},
		Kafka: KafkaConfig{
			Brokers: splitBrokers(kafkaBrokersRaw),
		},
		Qdrant: QdrantConfig{
			URL: getEnv("QDRANT_URL", "http://localhost:6333"),
		},
		Redis: RedisConfig{
			URL: RedisURL(),
		},
		System: SystemConfig{
			ResearchPoolSize:    getEnvInt("RESEARCH_POOL_SIZE", 5),
			ImplPoolSize:        getEnvInt("IMPL_POOL_SIZE", 3),
			QAPoolSize:          getEnvInt("QA_POOL_SIZE", 2),
			MaxRetryCount:       getEnvInt("MAX_RETRY_COUNT", 3),
			HumanReviewRequired: getEnvBool("HUMAN_REVIEW_REQUIRED", true),
			DashboardPort:       getEnvInt("DASHBOARD_PORT", 8080),
			WebhookPort:         getEnvInt("WEBHOOK_PORT", 8081),
			GatewayPort:         getEnvInt("GATEWAY_PORT", 8082),
		},
	}

	return cfg, nil
}

// getEnv returns the value of the named environment variable, or defaultVal if
// the variable is not set or is empty.
func getEnv(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

// getEnvInt returns the integer value of the named environment variable, or
// defaultVal if the variable is not set, is empty, or cannot be parsed as an
// integer.
func getEnvInt(key string, defaultVal int) int {
	v := os.Getenv(key)
	if v == "" {
		return defaultVal
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return defaultVal
	}
	return n
}

// getEnvBool returns the boolean value of the named environment variable, or
// defaultVal if the variable is not set, is empty, or cannot be parsed as a
// boolean. Accepted true values: "1", "t", "T", "TRUE", "true", "True".
func getEnvBool(key string, defaultVal bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return defaultVal
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return defaultVal
	}
	return b
}

// splitBrokers splits a comma-separated broker string into a slice of trimmed,
// non-empty broker addresses.
func splitBrokers(s string) []string {
	parts := strings.Split(s, ",")
	brokers := make([]string, 0, len(parts))
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			brokers = append(brokers, trimmed)
		}
	}
	return brokers
}
