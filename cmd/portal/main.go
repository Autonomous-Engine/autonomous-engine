package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/config"
	"github.com/autonomousengine/autonomous-engine/internal/db"
	"github.com/autonomousengine/autonomous-engine/internal/guards"
	"github.com/autonomousengine/autonomous-engine/internal/health"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/outbox"
	"github.com/autonomousengine/autonomous-engine/internal/products"
	"github.com/autonomousengine/autonomous-engine/internal/ratelimit"
	"github.com/autonomousengine/autonomous-engine/internal/sharedstate"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ─── Customer-facing status map ──────────────────────────────────────────────

var customerStatusMap = map[string]string{
	"intake":         "Request received",
	"prd":            "Being analyzed by our team",
	"research":       "Investigating the best approach",
	"planning":       "Planning the implementation",
	"implementation": "Engineers are working on it",
	"verification":   "Checking the change against your requirements",
	"review":         "Code review in progress",
	"human_review":   "Awaiting final approval",
	"qa":             "Quality assurance in progress",
	"passed":         "Complete — ready for release",
	"released":       "Deployed",
	"blocked":        "On hold — we'll update you soon",
	"failed":         "Being re-investigated",
	"rejected":       "Request could not be fulfilled",
}

// customerStatus maps an internal state to a customer-friendly label.
func customerStatus(state string) string {
	if s, ok := customerStatusMap[state]; ok {
		return s
	}
	return state
}

// ─── Timeline descriptions ────────────────────────────────────────────────────

var stateDescriptions = map[string]string{
	"intake":         "Your request was submitted",
	"prd":            "A product analyst is reviewing your request",
	"research":       "Our team is investigating the best approach",
	"planning":       "Implementation plan is being prepared",
	"implementation": "Implementation has started",
	"verification":   "The change is being verified against your requirements",
	"review":         "Code is being reviewed by our team",
	"human_review":   "A team member is performing a final review",
	"qa":             "Quality checks are underway",
	"passed":         "All checks passed — awaiting release",
	"released":       "Your request has been deployed",
	"blocked":        "Work has been paused — we will update you soon",
	"failed":         "Work is being re-investigated",
	"rejected":       "Your request could not be fulfilled at this time",
}

func stateDescription(state string) string {
	if d, ok := stateDescriptions[state]; ok {
		return d
	}
	return fmt.Sprintf("Status changed to %s", state)
}

// ─── Request / Response types ─────────────────────────────────────────────────

type submitRequest struct {
	ProductID    string `json:"product_id"`
	Title        string `json:"title"`
	Description  string `json:"description"`
	Category     string `json:"category"`
	Priority     string `json:"priority"`
	ContactEmail string `json:"contact_email"`
}

type submitResponse struct {
	TrackingID string `json:"tracking_id"`
	ProductID  string `json:"product_id"`
	Status     string `json:"status"`
	Message    string `json:"message"`
}

type trackResponse struct {
	TrackingID  string    `json:"tracking_id"`
	Title       string    `json:"title"`
	Status      string    `json:"status"`
	Stage       string    `json:"stage"`
	ProductName string    `json:"product_name,omitempty"`
	SubmittedAt time.Time `json:"submitted_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type updateEntry struct {
	Timestamp   time.Time `json:"timestamp"`
	Status      string    `json:"status"`
	Description string    `json:"description"`
}

type updatesResponse struct {
	TrackingID string        `json:"tracking_id"`
	Updates    []updateEntry `json:"updates"`
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("portal: encode response: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func generateTrackingID() (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "TRK-" + hex.EncodeToString(b), nil
}

func mapCategory(category string) models.WorkItemType {
	switch category {
	case "bug":
		return models.WorkItemTypeBug
	default:
		return models.WorkItemTypeFeature
	}
}

func mapPriority(p string) models.Priority {
	switch p {
	case "low":
		return models.PriorityLow
	case "high":
		return models.PriorityHigh
	case "critical":
		return models.PriorityCritical
	default:
		return models.PriorityMedium
	}
}

// ─── CORS middleware ──────────────────────────────────────────────────────────

func corsMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Accept")
		w.Header().Set("Access-Control-Max-Age", "86400")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next(w, r)
	}
}

// ─── resolveProductID ─────────────────────────────────────────────────────────

// resolveProductID resolves the product to use. If productIDStr is provided it
// is parsed and returned. When omitted, the single product is auto-selected or
// an error is returned when multiple products exist.
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

// ─── Handlers ─────────────────────────────────────────────────────────────────

func handleProducts(ps *products.ProductStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		prods, err := ps.ListProducts(r.Context())
		if err != nil {
			log.Printf("portal: list products: %v", err)
			writeError(w, http.StatusInternalServerError, "failed to list products")
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"products": prods})
	}
}

func handleSubmit(pool *pgxpool.Pool, ps *products.ProductStore, wg *workgraph.WorkGraph) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}

		var req submitRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON")
			return
		}

		if strings.TrimSpace(req.Title) == "" || strings.TrimSpace(req.Description) == "" {
			writeError(w, http.StatusBadRequest, "title and description are required")
			return
		}

		if req.Category == "" {
			req.Category = "feature"
		}
		if req.Priority == "" {
			req.Priority = "medium"
		}

		// Resolve product_id.
		productID, err := resolveProductID(r.Context(), ps, req.ProductID)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}

		// Guard: refuse submissions until onboarding is complete.
		onboardingCheck := guards.CheckOnboardingComplete(r.Context(), pool, &productID)
		if !onboardingCheck.Valid {
			log.Printf("portal: rejecting submission — system not onboarded: %s", onboardingCheck.Message)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"error": "System has not been onboarded. Please complete onboarding before submitting work items.",
				"code":  "not_onboarded",
			})
			return
		}

		trackingID, err := generateTrackingID()
		if err != nil {
			log.Printf("portal: generate tracking id: %v", err)
			writeError(w, http.StatusInternalServerError, "failed to generate tracking ID")
			return
		}

		ctx := r.Context()

		// Create the WorkItem.
		item := &models.WorkItem{
			ID:          uuid.New(),
			Type:        mapCategory(req.Category),
			State:       models.WorkItemStateIntake,
			Title:       req.Title,
			Description: req.Description,
			Priority:    mapPriority(req.Priority),
			ProductID:   &productID,
		}
		// The work item, its portal_submissions row and the intake event
		// commit together: a submission is either fully recorded and on its
		// way into the pipeline, or not recorded at all.
		if err := commitSubmission(ctx, pool, wg, item, trackingID, req, productID); err != nil {
			log.Printf("portal: record submission: %v", err)
			writeError(w, http.StatusInternalServerError, "failed to record submission")
			return
		}

		writeJSON(w, http.StatusCreated, submitResponse{
			TrackingID: trackingID,
			ProductID:  productID.String(),
			Status:     "received",
			Message:    "Your request has been submitted. Use your tracking ID to check progress.",
		})
	}
}

// insertSubmissionSQL records the portal submission. product_id is also
// persisted so per-product dashboard queries (e.g. admin filtering the portal
// queue by product) can find the row — see audit finding A.4.
const insertSubmissionSQL = `
	INSERT INTO portal_submissions
		(tracking_id, work_item_id, title, description, category, priority, contact_email, product_id)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`

// commitSubmission writes the work item, the portal_submissions row and the
// intake event (topic, key and payload unchanged) in one transaction.
func commitSubmission(ctx context.Context, db workgraph.TxBeginner, wg *workgraph.WorkGraph, item *models.WorkItem, trackingID string, req submitRequest, productID uuid.UUID) error {
	payload := map[string]string{
		"work_id":     item.ID.String(),
		"product_id":  productID.String(),
		"tracking_id": trackingID,
		"type":        string(item.Type),
		"priority":    string(item.Priority),
	}
	return workgraph.WithTx(ctx, db, func(tx pgx.Tx) error {
		if err := wg.CreateWorkItemTx(ctx, tx, item); err != nil {
			return fmt.Errorf("create work item: %w", err)
		}
		if _, err := tx.Exec(ctx, insertSubmissionSQL,
			trackingID, item.ID, req.Title, req.Description, req.Category, req.Priority, req.ContactEmail, productID,
		); err != nil {
			return fmt.Errorf("insert submission: %w", err)
		}
		return outbox.EnqueueJSON(ctx, tx, item.ID, bus.TopicIntake, item.ID.String(), payload)
	})
}

func handleTrack(pool *pgxpool.Pool, wg *workgraph.WorkGraph) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}

		// Parse path: /api/track/{tracking_id} or /api/track/{tracking_id}/updates
		path := strings.TrimPrefix(r.URL.Path, "/api/track/")
		path = strings.TrimSuffix(path, "/")

		wantsUpdates := false
		trackingID := path
		if strings.HasSuffix(path, "/updates") {
			wantsUpdates = true
			trackingID = strings.TrimSuffix(path, "/updates")
		}

		if trackingID == "" {
			writeError(w, http.StatusBadRequest, "tracking ID is required")
			return
		}

		ctx := r.Context()

		// Look up submission and join product name for display.
		var workItemID uuid.UUID
		var title string
		var submittedAt time.Time
		err := pool.QueryRow(ctx, `
			SELECT work_item_id, title, created_at
			FROM portal_submissions
			WHERE tracking_id = $1
		`, trackingID).Scan(&workItemID, &title, &submittedAt)
		if err != nil {
			writeError(w, http.StatusNotFound, "tracking ID not found")
			return
		}

		// Fetch current work item state.
		item, err := wg.GetWorkItem(ctx, workItemID)
		if err != nil {
			log.Printf("portal: get work item %s: %v", workItemID, err)
			writeError(w, http.StatusInternalServerError, "failed to fetch status")
			return
		}

		// Resolve the product name for the response (best-effort; empty if unavailable).
		productName := ""
		if item.ProductID != nil {
			var name string
			if qErr := pool.QueryRow(ctx, `SELECT name FROM products WHERE id = $1`, item.ProductID).Scan(&name); qErr == nil {
				productName = name
			}
		}

		if wantsUpdates {
			transitions, err := wg.GetTransitions(ctx, workItemID)
			if err != nil {
				log.Printf("portal: get transitions %s: %v", workItemID, err)
				writeError(w, http.StatusInternalServerError, "failed to fetch updates")
				return
			}

			// Start with the initial "submitted" entry.
			updates := make([]updateEntry, 0, len(transitions)+1)
			updates = append(updates, updateEntry{
				Timestamp:   submittedAt,
				Status:      customerStatus("intake"),
				Description: stateDescription("intake"),
			})

			for _, t := range transitions {
				updates = append(updates, updateEntry{
					Timestamp:   t.Timestamp,
					Status:      customerStatus(string(t.ToState)),
					Description: stateDescription(string(t.ToState)),
				})
			}

			// Reverse so newest is first.
			for i, j := 0, len(updates)-1; i < j; i, j = i+1, j-1 {
				updates[i], updates[j] = updates[j], updates[i]
			}

			writeJSON(w, http.StatusOK, updatesResponse{
				TrackingID: trackingID,
				Updates:    updates,
			})
			return
		}

		writeJSON(w, http.StatusOK, trackResponse{
			TrackingID:  trackingID,
			Title:       title,
			Status:      customerStatus(string(item.State)),
			Stage:       string(item.State),
			ProductName: productName,
			SubmittedAt: submittedAt,
			UpdatedAt:   item.UpdatedAt,
		})
	}
}

// ─── Main ─────────────────────────────────────────────────────────────────────

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// ── Configuration ───────────────────────────────────────────────────────────

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("portal: load config: %v", err)
	}

	// ── Database ────────────────────────────────────────────────────────────────

	pool, err := db.NewPool(ctx, cfg.Database.URL)
	if err != nil {
		log.Fatalf("portal: connect db: %v", err)
	}
	defer pool.Close()

	// ── Kafka ────────────────────────────────────────────────────────────────────

	kb, err := bus.NewKafkaBus(cfg.Kafka.Brokers)
	if err != nil {
		log.Fatalf("portal: connect kafka: %v", err)
	}
	defer kb.Close() //nolint:errcheck

	// ── WorkGraph and ProductStore ───────────────────────────────────────────────

	wg := workgraph.New(pool)
	ps := products.NewStore(pool)

	// ── Rate limiter ─────────────────────────────────────────────────────────────

	// 30 requests per minute per IP for portal submissions. With REDIS_URL
	// set the budget is shared by every portal replica; without it (or while
	// Redis is unreachable) each replica counts on its own.
	redisClient, err := sharedstate.Open(cfg.Redis.URL)
	if err != nil {
		log.Fatalf("portal: %v", err)
	}
	limiter := ratelimit.New(30, time.Minute).UseRedis(redisClient, "portal")
	go limiter.Cleanup()

	// ── Health checker ───────────────────────────────────────────────────────────

	// Submissions enqueue their intake event in the outbox; this dispatcher
	// delivers it. A start failure is fatal.
	dispatcher, err := outbox.StartForService(ctx, pool, kb, log.Default(), "portal")
	if err != nil {
		log.Fatalf("portal: start outbox dispatcher: %v", err)
	}

	checker := health.New("portal")
	checker.AddCheck("database", func() error {
		return pool.Ping(ctx)
	})
	checker.AddCheck("kafka", kb.HealthCheck())

	// ── HTTP mux ─────────────────────────────────────────────────────────────────

	mux := http.NewServeMux()

	// Health
	mux.HandleFunc("/health", checker.Handler())

	// API routes (CORS applied per route; /api/submit also rate-limited)
	mux.HandleFunc("/api/submit", corsMiddleware(limiter.Middleware(handleSubmit(pool, ps, wg))))
	mux.HandleFunc("/api/track/", corsMiddleware(handleTrack(pool, wg)))
	mux.HandleFunc("/api/products", corsMiddleware(handleProducts(ps)))

	// Static file serving with SPA fallback
	staticDir := "/portal-static"
	if _, err := os.Stat(staticDir); os.IsNotExist(err) {
		staticDir = "portal/dist" // local dev fallback
	}
	fs := http.FileServer(http.Dir(staticDir))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r)
			return
		}
		// Serve static file if it exists; otherwise fall back to index.html for SPA.
		fpath := filepath.Join(staticDir, filepath.Clean("/"+r.URL.Path))
		if _, err := os.Stat(fpath); os.IsNotExist(err) {
			http.ServeFile(w, r, filepath.Join(staticDir, "index.html"))
			return
		}
		fs.ServeHTTP(w, r)
	})

	// ── Port ─────────────────────────────────────────────────────────────────────

	port := os.Getenv("PORTAL_PORT")
	if port == "" {
		port = "3100"
	}

	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		log.Printf("portal: listening on :%s", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("portal: server error: %v", err)
		}
	}()

	// ── Graceful shutdown ─────────────────────────────────────────────────────────

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("portal: shutting down...")
	cancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("portal: shutdown error: %v", err)
	}
	dispatcher.Wait()
	log.Println("portal: stopped")
}
