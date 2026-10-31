package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	jira "github.com/andygrunwald/go-jira"
	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/config"
	"github.com/autonomousengine/autonomous-engine/internal/db"
	"github.com/autonomousengine/autonomous-engine/internal/health"
	jirapkg "github.com/autonomousengine/autonomous-engine/internal/jira"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/outbox"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
)

const (
	agentID       = "jira-sync"
	consumerGroup = "jira-sync"
	healthPort    = 8099
	rateLimitMs   = 200 // ms between Jira API calls
)

// stateChangedEvent is received from state.changed.
type stateChangedEvent struct {
	WorkID    string `json:"work_id"`
	FromState string `json:"from_state"`
	ToState   string `json:"to_state"`
	Reason    string `json:"reason"`
	Timestamp string `json:"timestamp"`
}

// jiraUpdate is an item in the queue.
type jiraUpdate struct {
	workID    uuid.UUID
	fromState string
	toState   string
	reason    string
}

// syncService manages queued updates and Jira API interactions.
type syncService struct {
	client     *jira.Client
	projectKey string
	wg         *workgraph.WorkGraph
	queue      chan jiraUpdate
	issueCache map[string]string // workID -> jiraIssueKey
	mu         sync.Mutex
}

func newSyncService(client *jira.Client, projectKey string, wg *workgraph.WorkGraph) *syncService {
	return &syncService{
		client:     client,
		projectKey: projectKey,
		wg:         wg,
		queue:      make(chan jiraUpdate, 256),
		issueCache: make(map[string]string),
	}
}

// startWorker processes queued updates in the background.
func (s *syncService) startWorker(ctx context.Context) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case update := <-s.queue:
				s.processUpdate(ctx, update)
				// Rate limit.
				time.Sleep(time.Duration(rateLimitMs) * time.Millisecond)
			}
		}
	}()
}

func (s *syncService) enqueue(update jiraUpdate) {
	select {
	case s.queue <- update:
	default:
		log.Printf("jira-sync: queue full, dropping update for work_id=%s", update.workID)
	}
}

func (s *syncService) processUpdate(ctx context.Context, update jiraUpdate) {
	if s.client == nil {
		log.Printf("jira-sync: no jira client configured, skipping work_id=%s", update.workID)
		return
	}

	s.mu.Lock()
	issueKey, exists := s.issueCache[update.workID.String()]
	s.mu.Unlock()

	if !exists {
		// Get work item from DB.
		item, err := s.wg.GetWorkItem(ctx, update.workID)
		if err != nil {
			log.Printf("jira-sync: get work item %s: %v", update.workID, err)
			return
		}

		// Try to find existing Jira issue by external ref.
		if item.ExternalRef != "" {
			issueKey = item.ExternalRef
		} else {
			// Create a new Jira issue.
			newKey, err := s.createJiraIssue(ctx, item)
			if err != nil {
				log.Printf("jira-sync: create jira issue for %s: %v", update.workID, err)
				return
			}
			issueKey = newKey
		}

		s.mu.Lock()
		s.issueCache[update.workID.String()] = issueKey
		s.mu.Unlock()
	}

	// Map internal state to Jira transition.
	transitionName := mapStateToJiraTransition(models.WorkItemState(update.toState))
	if transitionName == "" {
		return
	}

	if err := s.transitionIssue(ctx, issueKey, transitionName); err != nil {
		log.Printf("jira-sync: transition issue %s to %s: %v", issueKey, transitionName, err)
	}

	// Add comment with context.
	comment := fmt.Sprintf("Autonomous Engine: state changed from %s to %s. Reason: %s", update.fromState, update.toState, update.reason)
	if err := s.addComment(ctx, issueKey, comment); err != nil {
		log.Printf("jira-sync: add comment to %s: %v", issueKey, err)
	}
}

func (s *syncService) createJiraIssue(ctx context.Context, item *models.WorkItem) (string, error) {
	issueType := "Story"
	if item.Type == models.WorkItemTypeBug {
		issueType = "Bug"
	} else if item.Type == models.WorkItemTypeSubtask || item.Type == models.WorkItemTypeTask {
		issueType = "Task"
	}

	newIssue := &jira.Issue{
		Fields: &jira.IssueFields{
			Project: jira.Project{
				Key: s.projectKey,
			},
			Summary:     item.Title,
			Description: item.Description,
			Type: jira.IssueType{
				Name: issueType,
			},
		},
	}

	issue, _, err := s.client.Issue.Create(newIssue)
	if err != nil {
		return "", fmt.Errorf("create issue: %w", err)
	}
	log.Printf("jira-sync: created Jira issue %s for work_id=%s", issue.Key, item.ID)
	return issue.Key, nil
}

func (s *syncService) transitionIssue(ctx context.Context, issueKey, transitionName string) error {
	transitions, _, err := s.client.Issue.GetTransitions(issueKey)
	if err != nil {
		return fmt.Errorf("get transitions: %w", err)
	}

	for _, t := range transitions {
		if t.Name == transitionName {
			_, err := s.client.Issue.DoTransition(issueKey, t.ID)
			if err != nil {
				return fmt.Errorf("do transition %s: %w", transitionName, err)
			}
			return nil
		}
	}
	return fmt.Errorf("transition %q not found for issue %s", transitionName, issueKey)
}

func (s *syncService) addComment(ctx context.Context, issueKey, body string) error {
	comment := &jira.Comment{Body: body}
	_, _, err := s.client.Issue.AddComment(issueKey, comment)
	if err != nil {
		return fmt.Errorf("add comment: %w", err)
	}
	return nil
}

// mapStateToJiraTransition converts an internal work item state to a Jira workflow transition name.
func mapStateToJiraTransition(state models.WorkItemState) string {
	switch state {
	case models.WorkItemStateIntake:
		return "To Do"
	case models.WorkItemStatePRD:
		return "In Progress"
	case models.WorkItemStateResearch:
		return "In Progress"
	case models.WorkItemStatePlanning:
		return "In Progress"
	case models.WorkItemStateImplementation:
		return "In Progress"
	case models.WorkItemStateVerification:
		return "In Review"
	case models.WorkItemStateReview:
		return "In Review"
	case models.WorkItemStateHumanReview:
		return "Awaiting Review"
	case models.WorkItemStateQA:
		return "In QA"
	case models.WorkItemStatePassed:
		return "Done"
	case models.WorkItemStateFailed:
		return "Failed"
	case models.WorkItemStateBlocked:
		return "Blocked"
	case models.WorkItemStateRejected:
		return "Rejected"
	case models.WorkItemStateReleased:
		return "Released"
	default:
		return ""
	}
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("jira-sync: load config: %v", err)
	}

	pool, err := db.NewPool(ctx, cfg.Database.URL)
	if err != nil {
		log.Fatalf("jira-sync: connect db: %v", err)
	}
	defer pool.Close()

	kb, err := bus.NewKafkaBus(cfg.Kafka.Brokers)
	if err != nil {
		log.Fatalf("jira-sync: connect kafka: %v", err)
	}
	defer kb.Close() //nolint:errcheck

	wg := workgraph.New(pool)

	// Set up Jira client.
	// Jira is optional — the system operates fully without it.
	jiraEnabled := false
	var jiraClient *jira.Client
	if cfg.Jira.URL != "" && cfg.Jira.Email != "" && cfg.Jira.APIToken != "" {
		tp := jira.BasicAuthTransport{
			Username: cfg.Jira.Email,
			Password: cfg.Jira.APIToken,
		}
		var err error
		jiraClient, err = jira.NewClient(tp.Client(), cfg.Jira.URL)
		if err != nil {
			log.Printf("jira-sync: create jira client: %v — Jira sync disabled", err)
		} else {
			jiraEnabled = true
			log.Printf("jira-sync: Jira configured for %s — sync enabled", cfg.Jira.URL)
		}
	} else {
		log.Println("jira-sync: Jira not configured — sync disabled. Configure in Admin → Integrations to enable.")
	}

	svc := newSyncService(jiraClient, cfg.Jira.ProjectKey, wg)
	svc.startWorker(ctx)

	checker := health.New(agentID)
	checker.AddCheck("database", func() error {
		return pool.Ping(ctx)
	})
	checker.AddCheck("kafka", kb.HealthCheck())

	// The Jira ingester enqueues intake events in the transactional outbox;
	// this dispatcher delivers them. A start failure is fatal.
	dispatcher, err := outbox.StartForService(ctx, pool, kb, log.Default(), "jira-sync")
	if err != nil {
		log.Fatalf("jira-sync: start outbox dispatcher: %v", err)
	}

	// Subscribe to state.changed — always subscribe so the service is ready when
	// Jira is later configured. When jiraEnabled is false the handler is a no-op.
	handler := buildHandler(svc, jiraEnabled)
	if err := kb.Subscribe("state.changed", consumerGroup, handler); err != nil {
		log.Fatalf("jira-sync: subscribe: %v", err)
	}

	// ── Jira ingest loop ────────────────────────────────────────────────────
	// Ingest only starts when BOTH Jira credentials are configured AND
	// JIRA_INGEST_ENABLED=true is explicitly set. It never starts by default.
	var ingester *jirapkg.Ingester
	ingestEnabledEnv := os.Getenv("JIRA_INGEST_ENABLED")
	ingestExplicitlyEnabled := ingestEnabledEnv == "true" || ingestEnabledEnv == "1"

	if jiraEnabled && ingestExplicitlyEnabled {
		ingestClient := jirapkg.NewClient(cfg.Jira.URL, cfg.Jira.Email, cfg.Jira.APIToken)

		// Load workflow mapping — fall back to a default if discovery has not run.
		var mapping *jirapkg.WorkflowMapping
		if cfg.Jira.ProjectKey != "" {
			// Attempt a quick discovery to build a default mapping; errors are
			// non-fatal so the sync service continues without ingest.
			proj, discoverErr := ingestClient.DiscoverProject(ctx, cfg.Jira.ProjectKey)
			if discoverErr != nil {
				log.Printf("jira-sync: discover project for ingest mapping: %v — using empty mapping", discoverErr)
				mapping = &jirapkg.WorkflowMapping{
					StatusMapping:  make(map[string]string),
					TransitionMap:  make(map[string]string),
					IssueTypeMap:   make(map[string]string),
					PriorityMap:    make(map[string]string),
					RequiredFields: make(map[string]string),
				}
			} else {
				mapping = jirapkg.DefaultMapping(proj)
			}
		} else {
			mapping = &jirapkg.WorkflowMapping{
				StatusMapping:  make(map[string]string),
				TransitionMap:  make(map[string]string),
				IssueTypeMap:   make(map[string]string),
				PriorityMap:    make(map[string]string),
				RequiredFields: make(map[string]string),
			}
		}

		// Allow the JQL and interval to be overridden via environment variables.
		ingestJQL := os.Getenv("JIRA_INGEST_JQL")
		if ingestJQL == "" {
			ingestJQL = mapping.IngestJQL
		}
		if ingestJQL == "" && cfg.Jira.ProjectKey != "" {
			ingestJQL = "project = " + cfg.Jira.ProjectKey + " AND labels = autonomous-ingest ORDER BY created DESC"
		}

		ingestIntervalStr := os.Getenv("JIRA_INGEST_INTERVAL")
		ingestInterval := 5 * time.Minute
		if ingestIntervalStr != "" {
			if d, parseErr := time.ParseDuration(ingestIntervalStr); parseErr == nil {
				ingestInterval = d
			}
		}

		// productID for Jira ingest is not yet threaded through config —
		// until it is, the Ingester's productID is nil and ingestIssue
		// will refuse to create work items with a clear log line. This
		// is intentional: productless Jira work items would bypass every
		// per-product guard downstream (see audit finding A.6). To enable
		// ingest for a specific product, thread its UUID through
		// cfg.Jira.ProductID (new config key) and pass it here.
		var ingesterProductID *uuid.UUID
		log.Printf("jira-sync: WARNING — Jira Ingester started without product_id; tickets will be refused by ingestIssue until a product is configured")

		ingester = jirapkg.NewIngester(ingestClient, pool, wg, mapping, ingesterProductID)
		log.Printf("jira-sync: ingest enabled (jql=%q, interval=%s)", ingestJQL, ingestInterval)
		ingester.Start(ctx, ingestJQL, ingestInterval)
	} else if !jiraEnabled {
		log.Println("jira-sync: ingest not started — Jira not configured")
	} else {
		log.Println("jira-sync: ingest disabled (set JIRA_INGEST_ENABLED=true to enable)")
	}
	// ────────────────────────────────────────────────────────────────────────

	mux := http.NewServeMux()
	mux.HandleFunc("/health", checker.Handler())
	healthAddr := fmt.Sprintf(":%d", healthPort)
	healthSrv := &http.Server{Addr: healthAddr, Handler: mux}

	go func() {
		log.Printf("jira-sync: health server on %s", healthAddr)
		if err := healthSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("jira-sync: health server error: %v", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("jira-sync: shutting down...")
	cancel()

	if ingester != nil {
		ingester.Stop()
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := healthSrv.Shutdown(shutdownCtx); err != nil {
		log.Printf("jira-sync: health server shutdown error: %v", err)
	}
	dispatcher.Wait()
	log.Println("jira-sync: stopped")
}

func buildHandler(svc *syncService, jiraEnabled bool) bus.Handler {
	return func(msgCtx context.Context, msg *bus.Message) error {
		if !jiraEnabled {
			log.Println("jira-sync: Jira not configured, skipping event")
			return nil
		}

		var event stateChangedEvent
		if err := json.Unmarshal(msg.Value, &event); err != nil {
			log.Printf("jira-sync: unmarshal state.changed: %v", err)
			return nil
		}

		workID, err := uuid.Parse(event.WorkID)
		if err != nil {
			log.Printf("jira-sync: parse work_id %q: %v", event.WorkID, err)
			return nil
		}

		svc.enqueue(jiraUpdate{
			workID:    workID,
			fromState: event.FromState,
			toState:   event.ToState,
			reason:    event.Reason,
		})

		return nil
	}
}
