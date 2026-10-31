package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/agentregistry"
	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/config"
	"github.com/autonomousengine/autonomous-engine/internal/db"
	"github.com/autonomousengine/autonomous-engine/internal/guards"
	"github.com/autonomousengine/autonomous-engine/internal/health"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/outbox"
	"github.com/autonomousengine/autonomous-engine/internal/products"
	"github.com/autonomousengine/autonomous-engine/internal/rollout"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// intakeRequest is the JSON body accepted by POST /api/intake.
type intakeRequest struct {
	ProductID   string                 `json:"product_id"`
	Description string                 `json:"description"`
	Source      string                 `json:"source"`
	Metadata    map[string]interface{} `json:"metadata"`
	Tags        []string               `json:"tags"`
	Complexity  string                 `json:"complexity"`
}

// intakeResponse is the JSON body returned by a successful POST /api/intake.
type intakeResponse struct {
	WorkID    string `json:"work_id"`
	ProductID string `json:"product_id"`
	Status    string `json:"status"`
}

// intakeMessage is the payload published to the "intake" Kafka topic.
type intakeMessage struct {
	WorkID      string                 `json:"work_id"`
	ProductID   string                 `json:"product_id"`
	Description string                 `json:"description"`
	Source      string                 `json:"source"`
	Metadata    map[string]interface{} `json:"metadata"`
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Load configuration.
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("gateway: load config: %v", err)
	}

	// Connect to PostgreSQL.
	pool, err := db.NewPool(ctx, cfg.Database.URL)
	if err != nil {
		log.Fatalf("gateway: connect db: %v", err)
	}
	defer pool.Close()

	// Connect to Kafka.
	kb, err := bus.NewKafkaBus(cfg.Kafka.Brokers)
	if err != nil {
		log.Fatalf("gateway: connect kafka: %v", err)
	}
	defer kb.Close() //nolint:errcheck

	// Create work graph, product store, and agent registry.
	wg := workgraph.New(pool)
	ps := products.NewStore(pool)
	registry := agentregistry.New(pool)

	// Set up health checker with DB ping check.
	checker := health.New("gateway")
	checker.AddCheck("database", func() error {
		return pool.Ping(ctx)
	})
	checker.AddCheck("kafka", kb.HealthCheck())

	// Outbox dispatcher (M1-04). Intake no longer publishes directly: the
	// intake event is committed to outbox_events with the work item row, and
	// this loop carries it to Kafka. It runs on its own context, cancelled
	// only after the HTTP server has drained, so an intake committed by an
	// in-flight request is still dispatched before the process exits.
	dispatchCtx, stopDispatch := context.WithCancel(context.Background())
	defer stopDispatch()
	dispatcher, err := outbox.StartForService(dispatchCtx, pool, kb, log.Default(), "gateway")
	if err != nil {
		log.Fatalf("gateway: start outbox dispatcher: %v", err)
	}
	log.Printf("gateway: outbox dispatcher started claimed_by=%s", dispatcher.Name())

	// Register HTTP handlers.
	mux := http.NewServeMux()
	mux.HandleFunc("/api/intake", intakeHandler(pool, ps, wg, registry))
	mux.HandleFunc("/health", checker.Handler())

	addr := fmt.Sprintf(":%d", cfg.System.GatewayPort)
	srv := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	// Start server in background.
	go func() {
		log.Printf("gateway: listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("gateway: server error: %v", err)
		}
	}()

	// Wait for shutdown signal.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("gateway: shutting down...")
	cancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("gateway: shutdown error: %v", err)
	}

	stopDispatch()
	dispatcher.Wait()
	log.Println("gateway: outbox dispatcher stopped")
	log.Println("gateway: stopped")
}

// intakeHandler handles POST /api/intake.
func intakeHandler(pool *pgxpool.Pool, ps *products.ProductStore, wg *workgraph.WorkGraph, registry *agentregistry.Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req intakeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}

		if req.Description == "" {
			http.Error(w, "description is required", http.StatusBadRequest)
			return
		}

		// Resolve product_id.
		productID, err := resolveProductID(r.Context(), ps, req.ProductID)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()}) //nolint:errcheck
			return
		}

		// Guard: product must be onboarded.
		productCheck := guards.CheckProductOnboarded(r.Context(), pool, productID)
		if !productCheck.Valid {
			log.Printf("gateway: rejecting intake — product not onboarded: %s", productCheck.Message)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]string{ //nolint:errcheck
				"error": "Product has not been onboarded. " + productCheck.Message,
				"code":  "not_onboarded",
			})
			return
		}

		// Guard: refuse work submissions until global onboarding is also complete.
		onboardingCheck := guards.CheckOnboardingComplete(r.Context(), pool, nil)
		if !onboardingCheck.Valid {
			log.Printf("gateway: rejecting intake — system not onboarded: %s", onboardingCheck.Message)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]string{ //nolint:errcheck
				"error": "System has not been onboarded. Please complete onboarding before submitting work items.",
				"code":  "not_onboarded",
			})
			return
		}

		workID := uuid.New()

		// Auto-assign repository_id for single-repo products
		var repoID *uuid.UUID
		if ps != nil {
			repos, _ := ps.GetRepositoriesForProduct(r.Context(), productID)
			if len(repos) == 1 {
				repoID = &repos[0].ID
			}
		}

		// Use the first sentence (up to 100 chars) as the title, not the full description.
		title := req.Description
		if idx := strings.IndexAny(title, ".!\n"); idx > 0 && idx < 100 {
			title = title[:idx]
		} else if len(title) > 100 {
			title = title[:100]
		}

		item := &models.WorkItem{
			ID:           workID,
			Type:         models.WorkItemTypeFeature,
			State:        models.WorkItemStateIntake,
			Title:        title,
			Description:  req.Description,
			Priority:     models.PriorityMedium,
			ProductID:    &productID,
			RepositoryID: repoID,
		}

		msg := intakeMessage{
			WorkID:      workID.String(),
			ProductID:   productID.String(),
			Description: req.Description,
			Source:      req.Source,
			Metadata:    req.Metadata,
		}

		// Resolve agent definition based on tags and complexity — before the
		// transaction, because it is a read and must not hold the commit open.
		// If the registry has no definitions yet (table empty, before SeedDefaults runs),
		// ResolveForWorkItem returns an error. We log a warning but do NOT block the work
		// item — a NULL agent_definition_id means "use full pipeline", which is the safe default.
		// M4-06 shadow intake: a configuration snapshot (AE_CONFIG_SNAPSHOT_ID)
		// is honoured only for a rollout's candidate/baseline, while the
		// rollout is in shadow, on a benchmark scratch product; a replay
		// must come from a product that opted in and target a scratch
		// product. The source string grants nothing. Refused shadow intakes
		// fail here (logged), before anything is written.
		shadow, shadowErr := rollout.ResolveShadowIntake(r.Context(), pool, req.Source, req.Metadata, productID)
		if shadowErr != nil {
			var ie *rollout.IntakeError
			if errors.As(shadowErr, &ie) {
				log.Printf("gateway: rejecting shadow intake (source %q, product %s): %s", req.Source, productID, ie.Message)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(ie.Status)
				json.NewEncoder(w).Encode(map[string]string{"error": ie.Message, "code": ie.Code}) //nolint:errcheck
				return
			}
			log.Printf("gateway: resolve shadow intake for %s: %v", workID, shadowErr)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}

		var agentDefID *uuid.UUID
		var def *agentregistry.AgentDefinition
		var defErr error
		if shadow != nil && shadow.SnapshotID != "" {
			// The requested snapshot's definition replaces tag/complexity
			// routing (nil: the snapshot ran without one).
			if shadow.AgentDefinitionID != nil {
				def, defErr = registry.Get(r.Context(), *shadow.AgentDefinitionID)
				if defErr != nil {
					log.Printf("gateway: load agent definition of snapshot %s for %s: %v", shadow.SnapshotID, workID, defErr)
					http.Error(w, "internal server error", http.StatusInternalServerError)
					return
				}
				agentDefID = &def.ID
			}
			log.Printf("gateway: shadow intake %s runs config snapshot %s (replay=%t)", workID, shadow.SnapshotID, shadow.Replay)
		} else {
			def, defErr = registry.ResolveForWorkItem(r.Context(), req.Tags, req.Complexity)
			if defErr != nil {
				log.Printf("gateway: resolve agent definition for %s (warning, non-fatal): %v — using full pipeline", workID, defErr)
			} else if def != nil {
				agentDefID = &def.ID
			}
		}

		// M4-05: the pipeline configuration snapshot the item is routed
		// under. The snapshot row is content-addressed and written once (it
		// may already exist), so it is stored before the transaction; the
		// work item's link to it commits with the item. Best-effort: an item
		// without a snapshot is still accepted.
		snapshotID, snapErr := registry.CaptureIntakeSnapshot(r.Context(), def)
		if snapErr != nil {
			log.Printf("gateway: capture config snapshot for %s (warning, non-fatal): %v", workID, snapErr)
			snapshotID = ""
		}

		// The work item, its agent definition and the intake event commit
		// together. Before the outbox a failed publish left a work item in
		// intake that nothing would ever pick up; now either all of it lands
		// or the request fails and none of it does.
		if err := commitIntake(r.Context(), pool, wg, item, agentDefID, snapshotID, shadow, msg); err != nil {
			log.Printf("gateway: commit intake for %s: %v", workID, err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}

		resp := intakeResponse{
			WorkID:    workID.String(),
			ProductID: productID.String(),
			Status:    "accepted",
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(resp) //nolint:errcheck
	}
}

// commitIntake writes a new work item, its agent definition (when one was
// resolved) and the intake event in one transaction. The item is new and
// top-level, so it is its own outbox aggregate; topic, key and payload are
// exactly what the gateway used to publish directly. A shadow intake's
// replay/rollout/requested-snapshot columns (M4-06) commit with the item.
func commitIntake(ctx context.Context, db workgraph.TxBeginner, wg *workgraph.WorkGraph, item *models.WorkItem, agentDefID *uuid.UUID, configSnapshotID string, shadow *rollout.ShadowIntake, msg intakeMessage) error {
	return workgraph.WithTx(ctx, db, func(tx pgx.Tx) error {
		if err := wg.CreateWorkItemTx(ctx, tx, item); err != nil {
			return err
		}
		if agentDefID != nil {
			if err := wg.SetAgentDefinitionTx(ctx, tx, item.ID, *agentDefID); err != nil {
				return err
			}
		}
		if configSnapshotID != "" {
			if _, err := tx.Exec(ctx, `UPDATE work_items SET config_snapshot_id = $2 WHERE id = $1`, item.ID, configSnapshotID); err != nil {
				return fmt.Errorf("record config snapshot: %w", err)
			}
		}
		if err := shadow.ApplyTx(ctx, tx, item.ID); err != nil {
			return err
		}
		return outbox.EnqueueJSON(ctx, tx, item.ID, bus.TopicIntake, item.ID.String(), msg)
	})
}

// resolveProductID resolves the product to use for a request. If productIDStr is
// provided it is parsed and returned. If empty:
//   - auto-select when exactly one product exists
//   - return an error when multiple products exist (product_id is then required)
func resolveProductID(ctx context.Context, ps *products.ProductStore, productIDStr string) (uuid.UUID, error) {
	if productIDStr != "" {
		id, err := uuid.Parse(productIDStr)
		if err != nil {
			return uuid.Nil, fmt.Errorf("invalid product_id: %w", err)
		}
		return id, nil
	}

	all, err := ps.ListProducts(ctx)
	if err != nil {
		return uuid.Nil, fmt.Errorf("list products: %w", err)
	}
	switch len(all) {
	case 0:
		return uuid.Nil, fmt.Errorf("no products configured; create a product first")
	case 1:
		return all[0].ID, nil
	default:
		return uuid.Nil, fmt.Errorf("product_id is required when multiple products are configured")
	}
}
