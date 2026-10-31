package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/auth"
	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/codex"
	"github.com/autonomousengine/autonomous-engine/internal/config"
	"github.com/autonomousengine/autonomous-engine/internal/configstore"
	"github.com/autonomousengine/autonomous-engine/internal/crypto"
	"github.com/autonomousengine/autonomous-engine/internal/db"
	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/health"
	"github.com/autonomousengine/autonomous-engine/internal/idempotency"
	"github.com/autonomousengine/autonomous-engine/internal/interactions"
	"github.com/autonomousengine/autonomous-engine/internal/outbox"
	"github.com/autonomousengine/autonomous-engine/internal/sharedstate"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
)

var version = "dev"

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// ── Configuration ─────────────────────────────────────────────────────────

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("dashboard-backend: load config: %v", err)
	}

	// More than one replica needs Redis to fan WebSocket events out to the
	// clients of every replica (replicas.go); refuse to start without it.
	replicas, err := checkReplicaConfig(os.Getenv)
	if err != nil {
		log.Fatalf("dashboard-backend: %v", err)
	}
	redisClient, err := sharedstate.Open(cfg.Redis.URL)
	if err != nil {
		log.Fatalf("dashboard-backend: %v", err)
	}

	secret, err := loadJWTSecret(os.Getenv)
	if err != nil {
		log.Fatalf("dashboard-backend: %v", err)
	}
	jwtSecret = secret

	// ── Database ──────────────────────────────────────────────────────────────

	pool, err := db.NewPool(ctx, cfg.Database.URL)
	if err != nil {
		log.Fatalf("dashboard-backend: connect db: %v", err)
	}
	defer pool.Close()

	// ── Master encryption key ─────────────────────────────────────────────────

	masterKeyStr := os.Getenv("MASTER_ENCRYPTION_KEY")
	if masterKeyStr == "" {
		_, encoded, _ := crypto.GenerateMasterKey()
		log.Printf("WARNING: No MASTER_ENCRYPTION_KEY set. Generated: %s", encoded)
		log.Fatal("Set MASTER_ENCRYPTION_KEY in .env and restart")
	}
	masterKey, err := crypto.LoadMasterKey(masterKeyStr)
	if err != nil {
		log.Fatalf("load master key: %v", err)
	}

	// ── ConfigStore ───────────────────────────────────────────────────────────

	configStore := configstore.New(pool, masterKey)
	configStore.StartWatcher(ctx)

	// Credentials (LLM, GitHub, Jira, etc.) are configured exclusively through
	// the admin dashboard. The registry starts empty on first boot; an admin
	// uses Admin → Providers / Admin → Credentials (or the Onboarding Wizard) to
	// add them. No env-var import is performed.

	// ── Kafka ─────────────────────────────────────────────────────────────────

	kb, err := bus.NewKafkaBus(cfg.Kafka.Brokers)
	if err != nil {
		log.Fatalf("dashboard-backend: connect kafka: %v", err)
	}
	defer kb.Close() //nolint:errcheck

	// ── Auth Manager ──────────────────────────────────────────────────────────

	authManager := auth.NewManager(pool, jwtSecret)
	authManager.RegisterProvider(auth.NewLocalProvider(pool))
	authManager.RegisterProvider(auth.NewSAMLProvider(pool))
	authManager.RegisterProvider(auth.NewLDAPProvider(pool))
	if err := authManager.LoadFromDB(ctx); err != nil {
		log.Printf("dashboard-backend: load auth providers: %v", err)
	}

	// ── Domain services ───────────────────────────────────────────────────────

	wg := workgraph.New(pool)
	cx := codex.New(pool)
	interactionsStore := interactions.NewStore(pool)
	interactionsPublisher := interactions.NewPublisher(kb)
	evidenceStore := evidence.NewStore(pool)
	handlers := NewHandlers(wg, cx, kb, interactionsStore, interactionsPublisher, evidenceStore)
	authSvc := NewAuthService(pool)
	wsHub := NewWSHub(pool)
	adminHandlers := NewAdminHandlers(configStore, pool)
	authHandlers := NewAuthHandlers(authManager, pool)
	userHandlers := NewUserHandlers(pool)
	ollamaHandlers := NewOllamaHandlers(pool, configStore)
	providerHandlers := NewProviderHandlers(pool, configStore)
	embeddingProviderHandlers := NewEmbeddingProviderHandlers(pool, configStore)
	reindexHandlers := NewReindexHandlers(pool)
	promptsHandlers := NewPromptsHandlers(interactionsStore, interactionsPublisher, "admin@localhost")
	promptsHandlers.SetOnPRDAnswer(handlers.HandlePRDPromptAnswer)
	promptsHandlers.SetOnHumanReviewAnswer(handlers.HandleHumanReviewPromptAnswer)
	promptsHandlers.SetOnPRGateAnswer(handlers.HandlePRGatePromptAnswer)
	promptsHandlers.SetOnBlockedAnswer(handlers.HandleBlockedPromptAnswer)
	promptsHandlers.SetWSHub(wsHub)
	gitHandlers := NewGitHandlers(pool)

	// ── Health monitor ────────────────────────────────────────────────────────

	healthMonitor := NewHealthMonitor(pool, wsHub)
	go healthMonitor.Start(ctx)

	// ── WebSocket Kafka fanout ─────────────────────────────────────────────────

	// With Redis, events are relayed so every replica's clients get them; the
	// relay must be subscribed before the Kafka consumers start.
	if redisClient != nil {
		if err := wsHub.EnableRedisFanout(ctx, redisClient); err != nil {
			if replicas > 1 {
				log.Fatalf("dashboard-backend: %d replicas need the Redis WebSocket fan-out: %v", replicas, err)
			}
			log.Printf("dashboard-backend: Redis WebSocket fan-out unavailable (%v); delivering events directly", err)
		} else {
			log.Printf("dashboard-backend: WebSocket fan-out via Redis (%d replica(s))", replicas)
		}
	}

	if err := wsHub.StartKafkaFanout(kb); err != nil {
		log.Printf("dashboard-backend: start kafka fanout: %v", err)
	}

	// The outbox dispatcher delivers the events the manual approval paths
	// enqueue with their transitions (prd.ready, qa.ready, intake,
	// implementation.tasks, pr.ready). Without it those events are committed
	// to a table nobody drains while the approvals report success, so a
	// failure to start is fatal. Migration 047 must be applied first
	// (status-orchestrator).
	outboxDispatcher, err := outbox.StartForService(ctx, pool, kb, log.Default(), "dashboard-backend")
	if err != nil {
		log.Fatalf("dashboard-backend: start outbox dispatcher: %v", err)
	}
	log.Printf("dashboard-backend: outbox dispatcher started claimed_by=%s", outboxDispatcher.Name())

	// Start the Kafka consumer that turns user_prompt.requested events into
	// Postgres rows + WebSocket pushes. Task 9 of the interaction-surface plan.
	// M1-05: deduplicated on the processed-event ledger (consumer
	// "dashboard-backend"). The service has no /metrics endpoint, so its
	// ae_idempotency_* counters are visible in the log lines only.
	promptsConsumer := NewPromptsConsumer(interactionsStore, wsHub).WithLedger(idempotency.NewLedger(pool))
	if err := promptsConsumer.Start(ctx, kb); err != nil {
		log.Fatalf("start prompts consumer: %v", err)
	}

	// ── Health checker ────────────────────────────────────────────────────────

	checker := health.New("dashboard-backend")
	checker.AddCheck("database", func() error {
		return pool.Ping(ctx)
	})
	checker.AddCheck("kafka", kb.HealthCheck())

	// ── HTTP mux ──────────────────────────────────────────────────────────────

	mux := http.NewServeMux()

	// Health
	mux.HandleFunc("/health", checker.Handler())
	mux.HandleFunc("/api/system/health", authSvc.Middleware(healthMonitor.HandleGetHealth))
	mux.HandleFunc("/api/system/health/events", authSvc.Middleware(healthMonitor.HandleGetHealthEvents))

	// Public auth routes — no middleware required
	mux.HandleFunc("/api/auth/login", authHandlers.HandleLogin)
	mux.HandleFunc("/api/auth/login/ldap", authHandlers.HandleLDAPLogin)
	mux.HandleFunc("/api/auth/saml/login", authHandlers.HandleSAMLLogin)
	mux.HandleFunc("/api/auth/saml/callback", authHandlers.HandleSAMLCallback)
	mux.HandleFunc("/api/auth/providers", authHandlers.HandleListProviders)

	// Protected auth routes (valid JWT required, any role)
	mux.HandleFunc("/api/auth/logout", authSvc.Middleware(authHandlers.HandleLogout))
	mux.HandleFunc("/api/auth/change-password", authSvc.Middleware(authHandlers.HandleChangePassword))
	mux.HandleFunc("/api/auth/me", authSvc.Middleware(authHandlers.HandleMe))

	// Admin-only auth management routes
	mux.HandleFunc("/api/admin/auth/providers", authSvc.Middleware(AdminOnly(authHandlers.HandleListAuthProviders)))
	mux.HandleFunc("/api/admin/auth/providers/update", authSvc.Middleware(AdminOnly(authHandlers.HandleUpdateAuthProvider)))
	mux.HandleFunc("/api/admin/auth/providers/enable", authSvc.Middleware(AdminOnly(authHandlers.HandleEnableProvider)))
	mux.HandleFunc("/api/admin/auth/providers/disable", authSvc.Middleware(AdminOnly(authHandlers.HandleDisableProvider)))
	mux.HandleFunc("/api/admin/auth/providers/primary", authSvc.Middleware(AdminOnly(authHandlers.HandleSetPrimary)))
	mux.HandleFunc("/api/admin/auth/login-audit", authSvc.Middleware(AdminOnly(authHandlers.HandleLoginAudit)))

	// Admin-only user management routes
	mux.HandleFunc("/api/admin/users", authSvc.Middleware(AdminOnly(userHandlers.HandleListUsers)))
	mux.HandleFunc("/api/admin/users/create", authSvc.Middleware(AdminOnly(userHandlers.HandleCreateUser)))
	mux.HandleFunc("/api/admin/users/update", authSvc.Middleware(AdminOnly(userHandlers.HandleUpdateUser)))
	mux.HandleFunc("/api/admin/users/delete", authSvc.Middleware(AdminOnly(userHandlers.HandleDeleteUser)))
	mux.HandleFunc("/api/admin/users/reset-password", authSvc.Middleware(AdminOnly(userHandlers.HandleResetPassword)))
	mux.HandleFunc("/api/admin/users/unlock", authSvc.Middleware(AdminOnly(userHandlers.HandleUnlockUser)))

	// WebSocket — no auth required (auth can be enforced at the WS frame level
	// if needed, but the spec requests open access for the frontend)
	mux.HandleFunc("/ws", wsHub.HandleConnect)

	// Protected REST endpoints
	mux.HandleFunc("/api/work-items", authSvc.Middleware(handlers.ListWorkItems))
	mux.HandleFunc("/api/work-items/detail", authSvc.Middleware(handlers.GetWorkItem))
	mux.HandleFunc("/api/work-items/evidence", authSvc.Middleware(handlers.ListWorkItemEvidence))
	mux.HandleFunc("/api/work-items/approve", authSvc.Middleware(handlers.ApproveWorkItem))
	mux.HandleFunc("/api/work-items/approve-prd", authSvc.Middleware(handlers.ApprovePRD))
	mux.HandleFunc("/api/work-items/reject-prd", authSvc.Middleware(handlers.RejectPRD))
	// Admin-prefixed aliases — the dashboard UI's WorkItemDetail + ApprovalQueue
	// components were built against /admin/work-items/... per an earlier spec.
	// Keep both routes active so the UI works without a re-release of the frontend.
	mux.HandleFunc("/api/admin/work-items/approve-prd", authSvc.Middleware(AdminOnly(handlers.ApprovePRD)))
	mux.HandleFunc("/api/admin/work-items/reject-prd", authSvc.Middleware(AdminOnly(handlers.RejectPRD)))
	mux.HandleFunc("/api/work-items/cancel", authSvc.Middleware(handlers.CancelWorkItem))
	mux.HandleFunc("/api/work-items/retry", authSvc.Middleware(handlers.RetryWorkItem))
	mux.HandleFunc("/api/work-items/update-and-retry", authSvc.Middleware(handlers.UpdateAndRetry))
	mux.HandleFunc("/api/work-items/cost", authSvc.Middleware(handlers.WorkItemCost))
	mux.HandleFunc("/api/work-items/diff", authSvc.Middleware(handlers.WorkItemDiff))

	// PR gate routes — let users gate PR creation and review pending changes
	mux.HandleFunc("/api/work-items/pending-changes", authSvc.Middleware(handlers.ListPendingChanges))
	mux.HandleFunc("/api/work-items/pending-change", authSvc.Middleware(handlers.GetPendingChange))
	mux.HandleFunc("/api/work-items/approve-pr", authSvc.Middleware(handlers.ApprovePendingChange))
	mux.HandleFunc("/api/work-items/reject-pr", authSvc.Middleware(handlers.RejectPendingChange))
	mux.HandleFunc("/api/work-items/pr-gate", authSvc.Middleware(handlers.SetWorkItemPRGate))

	mux.HandleFunc("/api/queue/counts", authSvc.Middleware(handlers.GetQueueCounts))
	mux.HandleFunc("/api/codex/patterns", authSvc.Middleware(handlers.ListCodexPatterns))
	mux.HandleFunc("/api/codex/modules", authSvc.Middleware(handlers.ListCodexModules))
	mux.HandleFunc("/api/codex/sections", authSvc.Middleware(handlers.ListCodexSections))
	mux.HandleFunc("/api/codex/search", authSvc.Middleware(handlers.SearchCodex))
	mux.HandleFunc("/api/codex/version", authSvc.Middleware(handlers.GetCodexVersion))
	// Graph endpoint is auth-only (not AdminOnly) — any authenticated user can
	// read the codex graph for a product.
	mux.HandleFunc("/api/codex/graph", authSvc.Middleware(handlers.GraphForProduct))
	mux.HandleFunc("POST /api/codex/backfill", authSvc.Middleware(handlers.BackfillCodex))
	mux.HandleFunc("GET /api/codex/diff", authSvc.Middleware(handlers.CodexDiff))
	mux.HandleFunc("GET /api/codex/contract-edges", authSvc.Middleware(handlers.ContractEdges))
	mux.HandleFunc("GET /api/codex/topology", authSvc.Middleware(handlers.Topology))
	mux.HandleFunc("GET /api/codex/watched-branches", authSvc.Middleware(handlers.ListWatchedBranches))
	mux.HandleFunc("POST /api/codex/watched-branches", authSvc.Middleware(handlers.AddWatchedBranch))
	mux.HandleFunc("POST /api/codex/watched-branches/disable", authSvc.Middleware(handlers.DisableWatchedBranch))
	mux.HandleFunc("GET /api/codex/git-strategy", authSvc.Middleware(handlers.GitStrategy))
	mux.HandleFunc("GET /api/codex/snapshots", authSvc.Middleware(handlers.ListSnapshots))
	mux.HandleFunc("GET /api/codex/snapshot", authSvc.Middleware(handlers.GetSnapshot))
	mux.HandleFunc("/api/agents/status", authSvc.Middleware(handlers.AgentStatus))
	mux.HandleFunc("/api/metrics/summary", authSvc.Middleware(handlers.MetricsSummary))

	// Admin-only routes (require valid JWT with role == "admin")
	mux.HandleFunc("/api/admin/credentials", authSvc.Middleware(AdminOnly(adminHandlers.ListCredentials)))
	mux.HandleFunc("/api/admin/credentials/detail", authSvc.Middleware(AdminOnly(adminHandlers.GetCredential)))
	mux.HandleFunc("/api/admin/credentials/reveal", authSvc.Middleware(AdminOnly(adminHandlers.RevealCredential)))
	mux.HandleFunc("/api/admin/credentials/create", authSvc.Middleware(AdminOnly(adminHandlers.CreateCredential)))
	mux.HandleFunc("/api/admin/credentials/update", authSvc.Middleware(AdminOnly(adminHandlers.UpdateCredential)))
	mux.HandleFunc("/api/admin/credentials/delete", authSvc.Middleware(AdminOnly(adminHandlers.DeleteCredential)))
	mux.HandleFunc("/api/admin/credentials/rotate", authSvc.Middleware(AdminOnly(adminHandlers.RotateCredential)))
	mux.HandleFunc("/api/admin/credentials/test", authSvc.Middleware(AdminOnly(adminHandlers.TestConnection)))
	mux.HandleFunc("/api/admin/credentials/resolve", authSvc.Middleware(AdminOnly(adminHandlers.ResolveCredentialPreview)))
	mux.HandleFunc("/api/admin/config", authSvc.Middleware(AdminOnly(adminHandlers.ListConfig)))
	mux.HandleFunc("/api/admin/config/update", authSvc.Middleware(AdminOnly(adminHandlers.UpdateConfig)))
	mux.HandleFunc("/api/admin/audit", authSvc.Middleware(AdminOnly(adminHandlers.ListAudit)))

	// Admin-only product management routes
	mux.HandleFunc("/api/admin/products", authSvc.Middleware(AdminOnly(adminHandlers.ListProducts)))
	mux.HandleFunc("/api/admin/products/create", authSvc.Middleware(AdminOnly(adminHandlers.CreateProduct)))
	mux.HandleFunc("/api/admin/products/update", authSvc.Middleware(AdminOnly(adminHandlers.UpdateProduct)))
	mux.HandleFunc("/api/admin/products/delete", authSvc.Middleware(AdminOnly(adminHandlers.DeleteProduct)))
	mux.HandleFunc("/api/admin/products/pr-gate", authSvc.Middleware(AdminOnly(adminHandlers.SetProductPRGate)))
	mux.HandleFunc("/api/admin/products/partial-verification", authSvc.Middleware(AdminOnly(adminHandlers.SetProductPartialVerification)))
	mux.HandleFunc("/api/admin/products/contracts-strict", authSvc.Middleware(AdminOnly(adminHandlers.SetProductContractsStrict)))

	// Admin-only per-branch re-index routes (proxy to knowledge-indexer).
	mux.HandleFunc("/api/admin/reindex", authSvc.Middleware(AdminOnly(reindexHandlers.ReindexNow)))
	mux.HandleFunc("/api/admin/reindex/status", authSvc.Middleware(AdminOnly(reindexHandlers.ReindexStatus)))
	mux.HandleFunc("/api/admin/products/branches", authSvc.Middleware(AdminOnly(reindexHandlers.ProductBranches)))

	// Admin-only repository management routes
	mux.HandleFunc("/api/admin/products/reindex", authSvc.Middleware(AdminOnly(adminHandlers.ReindexProduct)))
	mux.HandleFunc("/api/admin/products/codex-stats", authSvc.Middleware(AdminOnly(adminHandlers.CodexStatsForProduct)))
	mux.HandleFunc("/api/admin/products/validate", authSvc.Middleware(AdminOnly(adminHandlers.ValidateProduct)))
	mux.HandleFunc("/api/admin/repositories", authSvc.Middleware(AdminOnly(adminHandlers.ListRepositories)))
	mux.HandleFunc("/api/admin/repositories/create", authSvc.Middleware(AdminOnly(adminHandlers.AddRepository)))
	mux.HandleFunc("/api/admin/repositories/update", authSvc.Middleware(AdminOnly(adminHandlers.UpdateRepository)))
	mux.HandleFunc("/api/admin/repositories/verification-policy", authSvc.Middleware(AdminOnly(adminHandlers.RepositoryVerificationPolicy)))
	mux.HandleFunc("/api/admin/repositories/zones", authSvc.Middleware(AdminOnly(adminHandlers.RepositoryZones)))
	mux.HandleFunc("/api/admin/zones", authSvc.Middleware(AdminOnly(adminHandlers.Zone)))
	mux.HandleFunc("/api/admin/zones/freeze", authSvc.Middleware(AdminOnly(adminHandlers.FreezeZone)))
	mux.HandleFunc("/api/admin/zones/thaw", authSvc.Middleware(AdminOnly(adminHandlers.ThawZone)))
	mux.HandleFunc("/api/admin/repositories/delete", authSvc.Middleware(AdminOnly(adminHandlers.DeleteRepository)))
	mux.HandleFunc("/api/admin/repositories/pull", authSvc.Middleware(AdminOnly(adminHandlers.PullRepository)))
	mux.HandleFunc("/api/admin/repositories/clone-status", authSvc.Middleware(AdminOnly(adminHandlers.RepoCloneStatus)))
	mux.HandleFunc("/api/admin/repositories/test-access", authSvc.Middleware(AdminOnly(adminHandlers.TestRepoAccess)))
	mux.HandleFunc("/api/admin/repositories/analyze", authSvc.Middleware(AdminOnly(adminHandlers.AnalyzeRepository)))
	mux.HandleFunc("/api/admin/repositories/analyze/status", authSvc.Middleware(AdminOnly(adminHandlers.AnalyzeRepositoryStatus)))

	// Admin-only dependency management routes
	mux.HandleFunc("/api/admin/dependencies", authSvc.Middleware(AdminOnly(adminHandlers.ListDependencies)))
	mux.HandleFunc("/api/admin/dependencies/create", authSvc.Middleware(AdminOnly(adminHandlers.CreateDependency)))
	mux.HandleFunc("/api/admin/dependencies/delete", authSvc.Middleware(AdminOnly(adminHandlers.DeleteDependency)))

	// Admin-only model evaluation routes
	mux.HandleFunc("/api/admin/models/evaluate", authSvc.Middleware(AdminOnly(adminHandlers.EvaluateModel)))
	mux.HandleFunc("/api/admin/models/profiles", authSvc.Middleware(AdminOnly(adminHandlers.ListModelProfiles)))

	// M4-05: configuration versions, outcome rollups and model prices.
	registerConfigVersionRoutes(mux, authSvc.Middleware, NewConfigVersionHandlers(pool, configStore))
	// M4-06: rollouts (shadow evaluation decisions, approve/block, override audit, replay opt-in).
	registerRolloutRoutes(mux, authSvc.Middleware, NewRolloutHandlers(pool))

	// Admin-only GitHub repo browser
	mux.HandleFunc("/api/admin/github/repos", authSvc.Middleware(AdminOnly(adminHandlers.ListGitHubRepos)))

	// GitHub account routes
	mux.HandleFunc("/api/admin/github/accounts", authSvc.Middleware(AdminOnly(adminHandlers.ListGitHubAccounts)))
	mux.HandleFunc("/api/admin/github/accounts/create", authSvc.Middleware(AdminOnly(adminHandlers.AddGitHubAccount)))
	mux.HandleFunc("/api/admin/github/accounts/update", authSvc.Middleware(AdminOnly(adminHandlers.UpdateGitHubAccount)))
	mux.HandleFunc("/api/admin/github/accounts/delete", authSvc.Middleware(AdminOnly(adminHandlers.DeleteGitHubAccount)))
	mux.HandleFunc("/api/admin/github/accounts/test", authSvc.Middleware(AdminOnly(adminHandlers.TestGitHubAccount)))
	mux.HandleFunc("/api/admin/github/accounts/repos", authSvc.Middleware(AdminOnly(adminHandlers.ListGitHubAccountRepos)))
	mux.HandleFunc("/api/admin/github/accounts/migrate-legacy", authSvc.Middleware(AdminOnly(adminHandlers.MigrateLegacyGitHubToken)))

	// Admin-only Jira routes
	mux.HandleFunc("/api/admin/jira/ingested-count", authSvc.Middleware(AdminOnly(adminHandlers.JiraIngestedCount)))

	// Admin-only webhook dead-letter queue routes
	mux.HandleFunc("/api/admin/webhook/dead-letters", authSvc.Middleware(AdminOnly(adminHandlers.ListWebhookDeadLetters)))
	mux.HandleFunc("/api/admin/webhook/dead-letters/retry", authSvc.Middleware(AdminOnly(adminHandlers.RetryWebhookDeadLetter)))
	mux.HandleFunc("/api/admin/webhook/dead-letters/resolve", authSvc.Middleware(AdminOnly(adminHandlers.ResolveWebhookDeadLetter)))

	// Admin-only Ollama management routes
	mux.HandleFunc("/api/admin/ollama/models", authSvc.Middleware(AdminOnly(ollamaHandlers.ListModels)))
	mux.HandleFunc("/api/admin/ollama/pull", authSvc.Middleware(AdminOnly(ollamaHandlers.PullModel)))
	mux.HandleFunc("/api/admin/ollama/delete", authSvc.Middleware(AdminOnly(ollamaHandlers.DeleteModel)))
	mux.HandleFunc("/api/admin/ollama/status", authSvc.Middleware(AdminOnly(ollamaHandlers.OllamaStatus)))
	mux.HandleFunc("/api/admin/ollama/running", authSvc.Middleware(AdminOnly(ollamaHandlers.RunningModels)))

	// Admin-only provider management routes
	mux.HandleFunc("/api/admin/providers", authSvc.Middleware(AdminOnly(providerHandlers.ListProviders)))
	mux.HandleFunc("/api/admin/providers/create", authSvc.Middleware(AdminOnly(providerHandlers.CreateProvider)))
	mux.HandleFunc("/api/admin/providers/update", authSvc.Middleware(AdminOnly(providerHandlers.UpdateProvider)))
	mux.HandleFunc("/api/admin/providers/delete", authSvc.Middleware(AdminOnly(providerHandlers.DeleteProvider)))
	mux.HandleFunc("/api/admin/providers/set-default", authSvc.Middleware(AdminOnly(providerHandlers.SetDefault)))
	mux.HandleFunc("/api/admin/providers/test", authSvc.Middleware(AdminOnly(providerHandlers.TestConnection)))
	mux.HandleFunc("/api/admin/providers/models", authSvc.Middleware(AdminOnly(providerHandlers.ListModels)))
	mux.HandleFunc("/api/admin/providers/pull", authSvc.Middleware(AdminOnly(providerHandlers.PullModel)))
	mux.HandleFunc("/api/admin/providers/models/delete", authSvc.Middleware(AdminOnly(providerHandlers.DeleteModel)))

	// Admin-only embedding provider routes. Parallel to /api/admin/providers
	// but for the embedding_providers table — knowledge-indexer and the
	// codex_similar_code tool read the default row from this table on boot.
	mux.HandleFunc("/api/admin/embedding-providers", authSvc.Middleware(AdminOnly(embeddingProviderHandlers.ListEmbeddingProviders)))
	mux.HandleFunc("/api/admin/embedding-providers/create", authSvc.Middleware(AdminOnly(embeddingProviderHandlers.CreateEmbeddingProvider)))
	mux.HandleFunc("/api/admin/embedding-providers/update", authSvc.Middleware(AdminOnly(embeddingProviderHandlers.UpdateEmbeddingProvider)))
	mux.HandleFunc("/api/admin/embedding-providers/delete", authSvc.Middleware(AdminOnly(embeddingProviderHandlers.DeleteEmbeddingProvider)))
	mux.HandleFunc("/api/admin/embedding-providers/test", authSvc.Middleware(AdminOnly(embeddingProviderHandlers.TestEmbeddingProvider)))
	mux.HandleFunc("/api/admin/embedding-providers/default", authSvc.Middleware(AdminOnly(embeddingProviderHandlers.SetDefaultEmbeddingProvider)))

	// Products (authenticated, not admin-only)
	mux.HandleFunc("GET /api/products", authSvc.Middleware(handlers.HandleListProducts))

	// Chat sessions
	mux.HandleFunc("POST /api/chat/sessions", authSvc.Middleware(handlers.HandleCreateChatSession))
	mux.HandleFunc("GET /api/chat/sessions", authSvc.Middleware(handlers.HandleListChatSessions))
	mux.HandleFunc("GET /api/chat/sessions/{id}", authSvc.Middleware(handlers.HandleGetChatSession))
	mux.HandleFunc("DELETE /api/chat/sessions/{id}", authSvc.Middleware(handlers.HandleDeleteChatSession))
	mux.HandleFunc("PATCH /api/chat/sessions/{id}", authSvc.Middleware(handlers.HandleUpdateChatSession))
	mux.HandleFunc("POST /api/chat/sessions/{id}/archive", authSvc.Middleware(handlers.HandleArchiveChatSession))
	mux.HandleFunc("POST /api/chat/sessions/{id}/unarchive", authSvc.Middleware(handlers.HandleUnarchiveChatSession))
	mux.HandleFunc("PUT /api/chat/sessions/{id}/product", authSvc.Middleware(handlers.HandleSetChatSessionProduct))
	mux.HandleFunc("PUT /api/chat/sessions/{id}/provider", authSvc.Middleware(handlers.HandleSetChatSessionProvider))
	mux.HandleFunc("GET /api/llm-providers", authSvc.Middleware(handlers.HandleListLLMProviders))
	mux.HandleFunc("POST /api/chat/sessions/{id}/messages", authSvc.Middleware(handlers.HandleSendChatMessage))
	mux.HandleFunc("GET /api/chat/sessions/{id}/messages", authSvc.Middleware(handlers.HandleGetChatMessages))
	mux.HandleFunc("POST /api/chat/sessions/{id}/proposals/{msgId}/approve", authSvc.Middleware(handlers.HandleApproveProposal))
	mux.HandleFunc("POST /api/chat/sessions/{id}/proposals/{msgId}/reject", authSvc.Middleware(handlers.HandleRejectProposal))

	// Interactions layer — unified prompt surface (Task 7).
	mux.HandleFunc("GET /api/prompts", promptsHandlers.ListPrompts)
	mux.HandleFunc("GET /api/prompts/{id}", promptsHandlers.GetPrompt)
	mux.HandleFunc("POST /api/prompts/{id}/answer", promptsHandlers.AnswerPrompt)
	mux.HandleFunc("POST /api/prompts/{id}/revoke", promptsHandlers.RevokePrompt)
	mux.HandleFunc("POST /api/codex/entries/revoke", promptsHandlers.RevokeCodexEntry)

	// Admin-only prompt management routes
	mux.HandleFunc("/api/admin/prompts", authSvc.Middleware(AdminOnly(adminHandlers.ListPrompts)))
	mux.HandleFunc("/api/admin/prompts/detail", authSvc.Middleware(AdminOnly(adminHandlers.GetPrompt)))
	mux.HandleFunc("/api/admin/prompts/create", authSvc.Middleware(AdminOnly(adminHandlers.CreatePrompt)))
	mux.HandleFunc("/api/admin/prompts/update", authSvc.Middleware(AdminOnly(adminHandlers.UpdatePrompt)))
	mux.HandleFunc("/api/admin/prompts/delete", authSvc.Middleware(AdminOnly(adminHandlers.DeletePrompt)))
	mux.HandleFunc("/api/admin/prompts/resolve", authSvc.Middleware(AdminOnly(adminHandlers.PreviewResolution)))
	mux.HandleFunc("/api/admin/prompts/intents", authSvc.Middleware(AdminOnly(adminHandlers.ListIntentTypes)))

	// Admin-only agent definition routes
	mux.HandleFunc("/api/admin/agents", authSvc.Middleware(AdminOnly(adminHandlers.ListAgentDefinitions)))
	mux.HandleFunc("/api/admin/agents/detail", authSvc.Middleware(AdminOnly(adminHandlers.GetAgentDefinition)))
	mux.HandleFunc("/api/admin/agents/create", authSvc.Middleware(AdminOnly(adminHandlers.CreateAgentDefinition)))
	mux.HandleFunc("/api/admin/agents/update", authSvc.Middleware(AdminOnly(adminHandlers.UpdateAgentDefinition)))
	mux.HandleFunc("/api/admin/agents/delete", authSvc.Middleware(AdminOnly(adminHandlers.DeleteAgentDefinition)))
	mux.HandleFunc("/api/admin/agents/set-default", authSvc.Middleware(AdminOnly(adminHandlers.SetDefaultAgent)))
	mux.HandleFunc("/api/admin/agents/prompts", authSvc.Middleware(AdminOnly(adminHandlers.ListAgentPrompts)))
	mux.HandleFunc("/api/admin/agents/prompts/set", authSvc.Middleware(AdminOnly(adminHandlers.SetAgentPrompt)))
	mux.HandleFunc("/api/admin/agents/prompts/delete", authSvc.Middleware(AdminOnly(adminHandlers.DeleteAgentPrompt)))

	// Version endpoint
	mux.HandleFunc("/api/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"version": version}) //nolint:errcheck
	})

	// Tool events (historical)
	mux.HandleFunc("/api/work-items/tool-events", authSvc.Middleware(handlers.ListToolEvents))

	// Git repository browser (any authenticated user can browse code)
	mux.HandleFunc("/api/repos/tree", authSvc.Middleware(gitHandlers.RepoTree))
	mux.HandleFunc("/api/repos/blob", authSvc.Middleware(gitHandlers.RepoBlob))
	mux.HandleFunc("/api/repos/commits", authSvc.Middleware(gitHandlers.RepoCommits))
	mux.HandleFunc("/api/repos/commit", authSvc.Middleware(gitHandlers.RepoCommit))
	mux.HandleFunc("/api/repos/branches", authSvc.Middleware(gitHandlers.RepoBranches))

	// ── CORS middleware ────────────────────────────────────────────────────────

	corsHandler := corsMiddleware(mux)

	// ── HTTP server ───────────────────────────────────────────────────────────

	addr := fmt.Sprintf(":%d", cfg.System.DashboardPort)
	srv := &http.Server{
		Addr:         addr,
		Handler:      corsHandler,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		log.Printf("dashboard-backend: listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("dashboard-backend: server error: %v", err)
		}
	}()

	// ── Graceful shutdown ─────────────────────────────────────────────────────

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("dashboard-backend: shutting down...")
	cancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("dashboard-backend: shutdown error: %v", err)
	}
	// Let an in-flight outbox publish finish being recorded before exit.
	outboxDispatcher.Wait()
	log.Println("dashboard-backend: stopped")
}

// ─── CORS middleware ──────────────────────────────────────────────────────────

// corsMiddleware wraps the given handler, adding CORS headers to every response
// and short-circuiting OPTIONS preflight requests.
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Accept, X-Requested-With")
		w.Header().Set("Access-Control-Max-Age", "86400")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}
