package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/codex"
	"github.com/autonomousengine/autonomous-engine/internal/config"
	"github.com/autonomousengine/autonomous-engine/internal/configstore"
	"github.com/autonomousengine/autonomous-engine/internal/crypto"
	"github.com/autonomousengine/autonomous-engine/internal/db"
	"github.com/autonomousengine/autonomous-engine/internal/health"
	"github.com/autonomousengine/autonomous-engine/internal/idempotency"
	"github.com/autonomousengine/autonomous-engine/internal/llm"
	"github.com/autonomousengine/autonomous-engine/internal/llm/strategy"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/outbox"
	"github.com/autonomousengine/autonomous-engine/internal/prompts"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Codex coherence Part 1 removed the reconciler's blockAffectedWork: 24 h
// after a moderate or major external change it blocked EVERY work item in
// research, planning, implementation, verification or review, across all
// products, from an in-memory timer that a restart lost. An external change
// now reaches in-flight work only through indexing: knowledge-indexer
// consumes external-change.detected (and the codex.updated this service
// enqueues for a minor change), its incremental index writes a codex version
// with the modules that changed and announces it on codex.indexed, and
// status-orchestrator re-plans exactly the items pinned to those modules on
// that product and branch (cmd/status-orchestrator/codex_replan.go). What
// remains here: the quiet-period batching, the classification, the
// external-change record for moderate/major changes (dashboard visibility)
// and the codex section plus codex.updated for minor ones.
const (
	agentID       = "change-reconciler"
	consumerGroup = "change-reconciler"
	healthPort    = 8100
	quietPeriod   = 15 * time.Minute
)

// externalChangeEvent is received from external-change.detected.
type externalChangeEvent struct {
	Source    string `json:"source"`
	Branch    string `json:"branch"`
	CommitSHA string `json:"commit_sha"`
	IsAgent   bool   `json:"is_agent"`
	Timestamp string `json:"timestamp"`
	// Jira event fields.
	IssueKey  string `json:"issue_key,omitempty"`
	EventType string `json:"event_type,omitempty"`
	Field     string `json:"field,omitempty"`
	From      string `json:"from,omitempty"`
	To        string `json:"to,omitempty"`
}

// codexUpdatedMessage is published to codex.updated for minor changes.
type codexUpdatedMessage struct {
	Source    string `json:"source"`
	CommitSHA string `json:"commit_sha"`
	Timestamp string `json:"timestamp"`
	// ProductID is the product the change was resolved to (the event's
	// aggregate; additive, F3), so the knowledge-indexer treats the message
	// as an index request rather than a notification. Omitted when the
	// change resolved to no product.
	ProductID string `json:"product_id,omitempty"`
}

// pendingChange tracks a batched external change.
type pendingChange struct {
	events    []externalChangeEvent
	timer     *time.Timer
	firstSeen time.Time
}

// reconciler manages pending changes and coordinates Codex updates.
type reconciler struct {
	pending map[string]*pendingChange // key: source+branch
	mu      sync.Mutex
	cx      *codex.Codex
	wg      *workgraph.WorkGraph
	llmSvc  *llm.Service
	pool    *pgxpool.Pool
	ctx     context.Context
}

func newReconciler(ctx context.Context, cx *codex.Codex, wg *workgraph.WorkGraph, llmSvc *llm.Service, pool *pgxpool.Pool) *reconciler {
	return &reconciler{
		pending: make(map[string]*pendingChange),
		cx:      cx,
		wg:      wg,
		llmSvc:  llmSvc,
		pool:    pool,
		ctx:     ctx,
	}
}

// resolveRepoProduct looks up (product_id, repository_id) for an external
// change event by matching event.Source to repositories.github_url. Returns
// (nil, nil) when no mapping is found; callers must treat this as "no
// product scope available" rather than silently scoping to product_id NULL.
func (rec *reconciler) resolveRepoProduct(ctx context.Context, source string) (*uuid.UUID, *uuid.UUID) {
	if rec.pool == nil || source == "" {
		return nil, nil
	}
	var productID, repoID uuid.UUID
	err := rec.pool.QueryRow(ctx, `
		SELECT product_id, id FROM repositories
		WHERE github_url = $1 OR name = $1
		LIMIT 1
	`, source).Scan(&productID, &repoID)
	if err != nil {
		return nil, nil
	}
	return &productID, &repoID
}

func (rec *reconciler) onExternalChange(event externalChangeEvent) {
	// Skip agent-initiated changes.
	if event.IsAgent {
		return
	}

	key := event.Source + ":" + event.Branch
	if event.IssueKey != "" {
		key = "jira:" + event.IssueKey
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()

	pc, exists := rec.pending[key]
	if !exists {
		pc = &pendingChange{firstSeen: time.Now()}
		rec.pending[key] = pc
		// Set quiet period timer.
		pc.timer = time.AfterFunc(quietPeriod, func() {
			rec.processChange(key)
		})
	} else {
		// Reset quiet period timer.
		pc.timer.Reset(quietPeriod)
	}
	pc.events = append(pc.events, event)
}

func (rec *reconciler) processChange(key string) {
	rec.mu.Lock()
	pc, ok := rec.pending[key]
	if !ok {
		rec.mu.Unlock()
		return
	}
	events := make([]externalChangeEvent, len(pc.events))
	copy(events, pc.events)
	delete(rec.pending, key)
	rec.mu.Unlock()

	if len(events) == 0 {
		return
	}

	log.Printf("change-reconciler: processing %d batched events for key=%s", len(events), key)

	ctx := rec.ctx

	// Classify the change.
	classification := "moderate"
	if rec.llmSvc != nil {
		eventSummary := ""
		for _, e := range events {
			eventSummary += fmt.Sprintf("- source=%s branch=%s sha=%s\n", e.Source, e.Branch, e.CommitSHA)
		}
		classifyIntent := strategy.Intent{
			Type: strategy.IntentClassifyChange,
			Context: map[string]string{
				"events":      eventSummary,
				"event_count": fmt.Sprintf("%d", len(events)),
				"key":         key,
			},
			OutputFormat: "single word: minor, moderate, or major",
		}
		result, err := rec.llmSvc.Execute(ctx, classifyIntent)
		if err != nil {
			log.Printf("change-reconciler: classify change: %v", err)
		} else {
			content := result.Content
			if len(content) > 0 {
				// Extract classification from result.
				for _, cls := range []string{"minor", "moderate", "major"} {
					if containsStr(content, cls) {
						classification = cls
						break
					}
				}
			}
		}
	}

	log.Printf("change-reconciler: classification=%s for key=%s", classification, key)

	now := time.Now().UTC().Format(time.RFC3339)
	firstEvent := events[0]

	switch classification {
	case "minor":
		// Auto-update Codex.
		productID := rec.autoUpdateCodex(ctx, firstEvent)
		updatedMsg := codexUpdatedMessage{
			Source:    firstEvent.Source,
			CommitSHA: firstEvent.CommitSHA,
			Timestamp: now,
		}
		// There is no caller to hand a failure to (this runs on the
		// quiet-period timer), so it is logged loudly; the codex row is
		// already written and the next change for this key re-notifies.
		if err := enqueueCodexUpdated(ctx, rec.pool, productID, key, updatedMsg); err != nil {
			log.Printf("change-reconciler: ERROR enqueue codex.updated for key=%s: %v — downstream re-index not triggered", key, err)
		}

	case "moderate", "major":
		// Create external_change record and notify dashboard. In-flight
		// work is not blocked here (see the note on the constants): the
		// change is indexed and re-plans only the items it touches.
		changeID := uuid.New()
		rec.saveExternalChangeRecord(ctx, changeID, key, classification, firstEvent)

	default:
		log.Printf("change-reconciler: unknown classification %q for key=%s", classification, key)
	}
}

// autoUpdateCodex upserts the external-change codex section and returns the
// product it was scoped to (nil when the source maps to no product).
func (rec *reconciler) autoUpdateCodex(ctx context.Context, event externalChangeEvent) *uuid.UUID {
	// Update codex with external change info. Resolve product/repo from
	// the event's Source so the section lands on the right product's
	// dashboard rather than in the legacy/global namespace (finding A.8).
	productID, repoID := rec.resolveRepoProduct(ctx, event.Source)
	contentStr := fmt.Sprintf(`{"description":"External change detected: source=%s branch=%s sha=%s"}`,
		event.Source, event.Branch, event.CommitSHA)
	// Resolve a branch for the codex row. The webhook's own event.Branch
	// is authoritative when present; fall back to the product's default
	// branch (from repositories.base_branch) rather than hardcoding "main".
	branch := strings.TrimSpace(event.Branch)
	if branch == "" && productID != nil {
		if resolved, resolveErr := codex.ResolveDefaultBranch(ctx, rec.pool, *productID); resolveErr == nil {
			branch = resolved
		} else {
			log.Printf("change-reconciler: resolve default branch for product %s: %v — skipping codex upsert", *productID, resolveErr)
			return productID
		}
	}
	section := &models.CodexSection{
		SectionType:  models.CodexSectionTypeDomain,
		Name:         fmt.Sprintf("external-change-%s", event.CommitSHA),
		Content:      []byte(contentStr),
		Version:      1,
		ProductID:    productID,
		RepositoryID: repoID,
		Branch:       branch,
	}
	if err := rec.cx.UpsertSection(ctx, section); err != nil {
		log.Printf("change-reconciler: upsert codex section: %v", err)
	}
	return productID
}

func (rec *reconciler) saveExternalChangeRecord(ctx context.Context, changeID uuid.UUID, key, classification string, event externalChangeEvent) {
	// Resolve product_id + repository_id from the event's Source so the
	// external-change WorkItem surfaces on the correct product's blocked
	// list — finding A.7. Falling back to NULL is acceptable (Jira-only
	// events have no repo source), but the reconciler logs loudly so
	// missing mappings don't hide.
	productID, repoID := rec.resolveRepoProduct(ctx, event.Source)
	if productID == nil && event.Source != "" {
		log.Printf("change-reconciler: no product/repo mapping found for source=%q — work item %s will land productless", event.Source, changeID)
	}

	// Store as a WorkItem of type "research" to track external changes.
	item := &models.WorkItem{
		ID:           changeID,
		Type:         models.WorkItemTypeResearch,
		State:        models.WorkItemStateBlocked,
		Title:        fmt.Sprintf("External change: %s (%s)", key, classification),
		Description:  fmt.Sprintf("External change detected. Source: %s, Branch: %s, SHA: %s, Classification: %s", event.Source, event.Branch, event.CommitSHA, classification),
		Priority:     classificationToPriority(classification),
		ExternalRef:  key,
		ProductID:    productID,
		RepositoryID: repoID,
	}
	if err := rec.wg.CreateWorkItem(ctx, item); err != nil {
		log.Printf("change-reconciler: save external change record: %v", err)
	}
	log.Printf("change-reconciler: saved external change record id=%s classification=%s", changeID, classification)
}

func classificationToPriority(classification string) models.Priority {
	switch classification {
	case "major":
		return models.PriorityCritical
	case "moderate":
		return models.PriorityHigh
	default:
		return models.PriorityMedium
	}
}

func containsStr(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	lowerS := make([]byte, len(s))
	lowerSub := make([]byte, len(sub))
	for i := range s {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 32
		}
		lowerS[i] = c
	}
	for i := range sub {
		c := sub[i]
		if c >= 'A' && c <= 'Z' {
			c += 32
		}
		lowerSub[i] = c
	}
	ls := string(lowerS)
	lsub := string(lowerSub)
	for i := 0; i <= len(ls)-len(lsub); i++ {
		if ls[i:i+len(lsub)] == lsub {
			return true
		}
	}
	return false
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("change-reconciler: load config: %v", err)
	}

	pool, err := db.NewPool(ctx, cfg.Database.URL)
	if err != nil {
		log.Fatalf("change-reconciler: connect db: %v", err)
	}
	defer pool.Close()

	// Wire prompt store so strategy layer resolves prompts from DB.
	strategy.SetPromptStore(prompts.NewStore(pool))

	kb, err := bus.NewKafkaBus(cfg.Kafka.Brokers)
	if err != nil {
		log.Fatalf("change-reconciler: connect kafka: %v", err)
	}
	defer kb.Close() //nolint:errcheck

	wg := workgraph.New(pool)
	cx := codex.New(pool)

	var cs *configstore.ConfigStore
	masterKeyStr := os.Getenv("MASTER_ENCRYPTION_KEY")
	if masterKeyStr != "" {
		mk, err := crypto.LoadMasterKey(masterKeyStr)
		if err == nil {
			cs = configstore.New(pool, mk)
			// Listen on pg_notify('config_changed') so UI updates to credentials/
			// endpoints flush the in-memory cache without a restart.
			cs.StartWatcher(ctx)
		}
	}

	// Build the LLM service from the credential registry. If no provider is
	// configured yet llmSvc will be nil and the service operates in degraded
	// mode until a model is added via the admin dashboard (Admin → Models).
	var llmSvc *llm.Service
	if cs != nil {
		llmSvc, err = llm.NewServiceFromRegistry(cs, "change-reconciler", "instance-1", pool)
		if err != nil {
			log.Println("change-reconciler: LLM not configured. Configure a model provider in the admin dashboard (Admin → Models).")
		}
	} else {
		log.Println("change-reconciler: LLM not configured. Configure a model provider in the admin dashboard (Admin → Models).")
	}

	// The reconciler's codex.updated and work.blocked events are enqueued
	// in the outbox; this dispatcher delivers them. A start failure is fatal.
	dispatcher, err := outbox.StartForService(ctx, pool, kb, log.Default(), agentID)
	if err != nil {
		log.Fatalf("change-reconciler: start outbox dispatcher: %v", err)
	}

	rec := newReconciler(ctx, cx, wg, llmSvc, pool)

	checker := health.New(agentID)
	checker.AddCheck("database", func() error {
		return pool.Ping(ctx)
	})
	checker.AddCheck("kafka", kb.HealthCheck())

	// M1-05: deduplicate on the processed-event ledger. The handler only
	// schedules a debounced reconciliation (its writes run later, on a
	// timer, outside any transaction this delivery owns), so the event is
	// recorded on receipt and a recorded duplicate does not re-arm the
	// debounce. The reconciliation itself is not covered by the guard.
	handler := idempotency.NewLedger(pool).Handler("change-reconciler", buildHandler(rec))
	if err := kb.Subscribe(bus.TopicExternalChangeDetected, consumerGroup, handler); err != nil {
		log.Fatalf("change-reconciler: subscribe: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", checker.Handler())
	healthAddr := fmt.Sprintf(":%d", healthPort)
	healthSrv := &http.Server{Addr: healthAddr, Handler: mux}

	go func() {
		log.Printf("change-reconciler: health server on %s", healthAddr)
		if err := healthSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("change-reconciler: health server error: %v", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("change-reconciler: shutting down...")
	cancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := healthSrv.Shutdown(shutdownCtx); err != nil {
		log.Printf("change-reconciler: health server shutdown error: %v", err)
	}
	dispatcher.Wait()
	log.Println("change-reconciler: stopped")
}

func buildHandler(rec *reconciler) bus.Handler {
	return func(msgCtx context.Context, msg *bus.Message) error {
		var event externalChangeEvent
		if err := json.Unmarshal(msg.Value, &event); err != nil {
			log.Printf("change-reconciler: unmarshal: %v", err)
			return nil
		}

		rec.onExternalChange(event)
		return nil
	}
}
