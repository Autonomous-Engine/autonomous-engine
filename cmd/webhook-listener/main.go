package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/config"
	"github.com/autonomousengine/autonomous-engine/internal/db"
	"github.com/autonomousengine/autonomous-engine/internal/health"
	"github.com/autonomousengine/autonomous-engine/internal/outbox"
	"github.com/autonomousengine/autonomous-engine/internal/outcomes"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const agentID = "webhook-listener"

// ─── Dead-Letter Queue ────────────────────────────────────────────────────────

// saveToDeadLetter persists a webhook event that could not be enqueued (or
// could not be parsed) to the dead-letter table so it can be retried later.
// The error is returned so a handler can tell the sender the delivery failed
// when neither the outbox nor the dead-letter table has it.
func saveToDeadLetter(ctx context.Context, db webhookDB, source, eventType string, payload []byte, errMsg string) error {
	_, err := db.Exec(ctx,
		`INSERT INTO webhook_dead_letters (source, event_type, payload, error_message, next_retry_at)
		 VALUES ($1, $2, $3, $4, NOW() + INTERVAL '5 minutes')`,
		source, eventType, payload, errMsg,
	)
	if err != nil {
		log.Printf("webhook-listener: dead-letter insert (%s/%s): %v", source, eventType, err)
	}
	return err
}

// startDeadLetterWorker launches a background goroutine that periodically
// queries unresolved dead-letter entries whose next_retry_at is due and
// re-enqueues them in the outbox. On success the entry is marked resolved in
// the same transaction; on failure retry_count is incremented with
// exponential backoff.
func startDeadLetterWorker(ctx context.Context, pool *pgxpool.Pool) {
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				processDeadLetters(ctx, pool)
			}
		}
	}()
}

// processDeadLetters fetches and attempts to re-enqueue pending dead-letter entries.
func processDeadLetters(ctx context.Context, pool *pgxpool.Pool) {
	rows, err := pool.Query(ctx,
		`SELECT id, source, event_type, payload, retry_count, max_retries
		 FROM webhook_dead_letters
		 WHERE NOT resolved
		   AND retry_count < max_retries
		   AND next_retry_at <= NOW()
		 ORDER BY next_retry_at
		 LIMIT 50`,
	)
	if err != nil {
		log.Printf("webhook-listener: dead-letter query: %v", err)
		return
	}
	defer rows.Close()

	var entries []deadLetter
	for rows.Next() {
		var e deadLetter
		if err := rows.Scan(&e.id, &e.source, &e.eventType, &e.payload, &e.retryCount, &e.maxRetries); err != nil {
			log.Printf("webhook-listener: dead-letter scan: %v", err)
			continue
		}
		entries = append(entries, e)
	}
	rows.Close()

	for _, e := range entries {
		retryErr := retryDeadLetter(ctx, pool, e)
		if retryErr == nil {
			log.Printf("webhook-listener: dead-letter %s re-enqueued successfully", e.id)
			continue
		}
		// Failure — increment retry count, apply exponential backoff.
		backoffMinutes := 5 * (1 << e.retryCount) // 5, 10, 20, 40 …
		if backoffMinutes > 480 {
			backoffMinutes = 480 // cap at 8 hours
		}
		if _, err := pool.Exec(ctx,
			`UPDATE webhook_dead_letters
			 SET retry_count = retry_count + 1,
			     next_retry_at = NOW() + ($1 * INTERVAL '1 minute'),
			     error_message = $2
			 WHERE id = $3`,
			backoffMinutes, retryErr.Error(), e.id,
		); err != nil {
			log.Printf("webhook-listener: dead-letter backoff update %s: %v", e.id, err)
		}
		log.Printf("webhook-listener: dead-letter %s retry %d failed: %v", e.id, e.retryCount+1, retryErr)
	}
}

// deadLetterTopic maps a source + eventType back to the appropriate Kafka topic.
func deadLetterTopic(source, eventType string) string {
	switch source {
	case "github":
		switch eventType {
		case "push":
			return bus.TopicExternalChangeDetected
		case "pull_request":
			return bus.TopicPRMerged
			// pull_request_review / issue_comment are no longer forwarded
			// to Kafka (no subscriber exists) — they fall through to the
			// default and land in external-change topic on retry, which is
			// the safe choice since no handler will act on them.
		}
	case "jira":
		return bus.TopicExternalChangeDetected
	}
	// Default fallback — use external-change topic.
	return bus.TopicExternalChangeDetected
}

// ─── GitHub payload types ─────────────────────────────────────────────────────

type githubCommit struct {
	ID      string `json:"id"`
	Message string `json:"message"`
	Author  struct {
		Name string `json:"name"`
	} `json:"author"`
}

type githubPushPayload struct {
	Ref        string         `json:"ref"`
	After      string         `json:"after"`
	Commits    []githubCommit `json:"commits"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	Pusher struct {
		Name string `json:"name"`
	} `json:"pusher"`
	HeadCommit *githubCommit `json:"head_commit"`
}

type githubPRPayload struct {
	Action      string `json:"action"`
	PullRequest struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		Body   string `json:"body"`
		Merged bool   `json:"merged"`
		// M4-05: merge identity for work_item_outcomes.
		MergeCommitSHA string `json:"merge_commit_sha"`
		MergedAt       string `json:"merged_at"`
		Head           struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
		User struct {
			Login string `json:"login"`
		} `json:"user"`
	} `json:"pull_request"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

type githubPRReviewPayload struct {
	Action string `json:"action"`
	Review struct {
		ID   int    `json:"id"`
		Body string `json:"body"`
		User struct {
			Login string `json:"login"`
		} `json:"user"`
		State string `json:"state"`
	} `json:"review"`
	PullRequest struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		Head   struct {
			Ref string `json:"ref"`
		} `json:"head"`
	} `json:"pull_request"`
}

type githubIssueCommentPayload struct {
	Action  string `json:"action"`
	Comment struct {
		ID   int    `json:"id"`
		Body string `json:"body"`
		User struct {
			Login string `json:"login"`
		} `json:"user"`
	} `json:"comment"`
	Issue struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
	} `json:"issue"`
}

// ─── Jira payload types ───────────────────────────────────────────────────────

type jiraWebhookPayload struct {
	WebhookEvent string `json:"webhookEvent"`
	Timestamp    int64  `json:"timestamp"` // ms since epoch; stable across redeliveries
	Issue        struct {
		ID     string `json:"id"`
		Key    string `json:"key"`
		Fields struct {
			Summary string `json:"summary"`
			Status  struct {
				Name string `json:"name"`
			} `json:"status"`
			Priority struct {
				Name string `json:"name"`
			} `json:"priority"`
			Comment struct {
				Comments []struct {
					ID     string `json:"id"`
					Body   string `json:"body"`
					Author struct {
						DisplayName string `json:"displayName"`
					} `json:"author"`
				} `json:"comments"`
			} `json:"comment"`
		} `json:"fields"`
	} `json:"issue"`
	ChangeLog struct {
		Items []struct {
			Field   string `json:"field"`
			FromStr string `json:"fromString"`
			ToStr   string `json:"toString"`
		} `json:"items"`
	} `json:"changelog"`
}

// ─── Published event types ────────────────────────────────────────────────────

type externalChangeEvent struct {
	Source    string `json:"source"`
	Branch    string `json:"branch"`
	CommitSHA string `json:"commit_sha"`
	IsAgent   bool   `json:"is_agent"`
	Timestamp string `json:"timestamp"`
	// RepositoryID is the registered repositories.id (additive, F3), so the
	// knowledge-indexer can tell an index request from a notification.
	// Omitted for an unregistered repository.
	RepositoryID string `json:"repository_id,omitempty"`
}

type prMergedEvent struct {
	WorkID    string `json:"work_id"`
	PRNumber  int    `json:"pr_number"`
	Title     string `json:"title"`
	Branch    string `json:"branch"`
	IsAgent   bool   `json:"is_agent"`
	Timestamp string `json:"timestamp"`

	// M4-05 (additive): the merge identity status-orchestrator records on
	// work_item_outcomes, and the revert heuristic's result.
	Repository     string `json:"repository,omitempty"`
	HeadSHA        string `json:"head_sha,omitempty"`
	MergeCommitSHA string `json:"merge_commit_sha,omitempty"`
	MergedAt       string `json:"merged_at,omitempty"`
	// RevertsPRNumbers lists the PRs of the same repository this merged PR
	// reverts (outcomes.RevertedPRNumbers over its title and body). The
	// body itself is not forwarded.
	RevertsPRNumbers []int `json:"reverts_pr_numbers,omitempty"`
}

// prMergedEventFor builds the pr.merged event for a merged pull request.
// workID is "" for a non-agent branch.
func prMergedEventFor(p githubPRPayload, workID, now string) prMergedEvent {
	branch := p.PullRequest.Head.Ref
	return prMergedEvent{
		WorkID:           workID,
		PRNumber:         p.PullRequest.Number,
		Title:            p.PullRequest.Title,
		Branch:           branch,
		IsAgent:          strings.HasPrefix(branch, "agent/"),
		Timestamp:        now,
		Repository:       p.Repository.FullName,
		HeadSHA:          p.PullRequest.Head.SHA,
		MergeCommitSHA:   p.PullRequest.MergeCommitSHA,
		MergedAt:         p.PullRequest.MergedAt,
		RevertsPRNumbers: outcomes.RevertedPRNumbers(p.PullRequest.Title, p.PullRequest.Body, p.PullRequest.Number),
	}
}

type reviewCommentEvent struct {
	PRNumber  int    `json:"pr_number"`
	Reviewer  string `json:"reviewer"`
	Body      string `json:"body"`
	State     string `json:"state"`
	Timestamp string `json:"timestamp"`
}

type issueCommentEvent struct {
	IssueNumber int    `json:"issue_number"`
	Author      string `json:"author"`
	Body        string `json:"body"`
	Timestamp   string `json:"timestamp"`
}

type jiraChangeEvent struct {
	IssueKey  string `json:"issue_key"`
	EventType string `json:"event_type"`
	Field     string `json:"field,omitempty"`
	From      string `json:"from,omitempty"`
	To        string `json:"to,omitempty"`
	Timestamp string `json:"timestamp"`
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("webhook-listener: load config: %v", err)
	}

	pool, err := db.NewPool(ctx, cfg.Database.URL)
	if err != nil {
		log.Fatalf("webhook-listener: connect db: %v", err)
	}
	defer pool.Close()

	kb, err := bus.NewKafkaBus(cfg.Kafka.Brokers)
	if err != nil {
		log.Fatalf("webhook-listener: connect kafka: %v", err)
	}
	defer kb.Close() //nolint:errcheck

	checker := health.New(agentID)
	checker.AddCheck("database", func() error {
		return pool.Ping(ctx)
	})
	checker.AddCheck("kafka", kb.HealthCheck())

	// Every accepted webhook event is enqueued in the outbox; this dispatcher
	// is what delivers it. Without it events would be committed to a table
	// nobody drains, so a start failure is fatal.
	dispatcher, err := outbox.StartForService(ctx, pool, kb, log.Default(), agentID)
	if err != nil {
		log.Fatalf("webhook-listener: start outbox dispatcher: %v", err)
	}

	// Start the dead-letter background retry worker.
	startDeadLetterWorker(ctx, pool)

	mux := http.NewServeMux()
	mux.HandleFunc("/webhook/github", githubWebhookHandler(ctx, pool, newGitHubSecretSource(ctx, pool, cfg.GitHub.WebhookSecret)))
	mux.HandleFunc("/webhook/jira", jiraWebhookHandler(ctx, pool))
	mux.HandleFunc("/health", checker.Handler())

	addr := fmt.Sprintf(":%d", cfg.System.WebhookPort)
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		log.Printf("webhook-listener: listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("webhook-listener: server error: %v", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("webhook-listener: shutting down...")

	// Drain first, cancel second: the handlers enqueue on ctx, so cancelling
	// it before srv.Shutdown would make every in-flight webhook fail its
	// enqueue (and its dead-letter fallback) during the graceful drain —
	// losing deliveries that cannot be replayed. The dispatcher keeps
	// publishing while the drain runs; cancel then stops it and the
	// dead-letter worker, and Wait lets an in-flight publish be recorded.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("webhook-listener: shutdown error: %v", err)
	}
	cancel()
	dispatcher.Wait()
	log.Println("webhook-listener: stopped")
}

// resolveWorkIDFromBranch extracts the work_item UUID from a branch name that
// follows the pr-coordinator / implementation-agent naming convention:
//
//	agent/<workID>              — parent branch created by the pr-coordinator
//	agent/<workID>/<shortTaskID> — subtask branch created by implementation-agent
//
// Returns an empty string when the branch does not match (e.g. a human-created
// PR, or an agent branch with a malformed UUID). Callers should treat an empty
// result as "cannot correlate this PR to a work_item" and skip publishing.
func resolveWorkIDFromBranch(branch string) string {
	const prefix = "agent/"
	if !strings.HasPrefix(branch, prefix) {
		return ""
	}
	rest := strings.TrimPrefix(branch, prefix)
	// Take only the first segment — the workID is always first.
	if idx := strings.Index(rest, "/"); idx >= 0 {
		rest = rest[:idx]
	}
	// Validate it looks like a UUID by attempting a parse. The status-orchestrator
	// does a strict uuid.Parse itself, so reject anything that would fail there.
	if _, err := parseUUID(rest); err != nil {
		return ""
	}
	return rest
}

// parseUUID is a thin wrapper that lets resolveWorkIDFromBranch avoid a direct
// dependency on the uuid package in its signature, keeping the helper easy to
// unit-test. The underlying parser is github.com/google/uuid.Parse which
// enforces length and hex-format.
var parseUUID = func(s string) (string, error) {
	// Length + hyphen positions of canonical UUID v4: 8-4-4-4-12
	if len(s) != 36 {
		return "", fmt.Errorf("invalid uuid length: %d", len(s))
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return "", fmt.Errorf("invalid uuid separator at %d", i)
			}
		default:
			if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
				return "", fmt.Errorf("invalid uuid hex at %d", i)
			}
		}
	}
	return s, nil
}

func isWatchedBranch(ctx context.Context, db webhookDB, repoID uuid.UUID, branch string) bool {
	var enabled bool
	err := db.QueryRow(ctx, `
		SELECT enabled FROM codex_watched_branches
		WHERE repo_id = $1 AND branch = $2 AND enabled = TRUE
	`, repoID, branch).Scan(&enabled)
	return err == nil && enabled
}

// acceptEvents enqueues evs and, when that fails, keeps the raw body in the
// dead-letter table instead. It reports whether the delivery is safe in one
// of the two; when it is not, the handler answers 503 so the sender records a
// failed delivery that an operator can redeliver.
func acceptEvents(ctx context.Context, db webhookDB, source, eventType string, body []byte, evs []outbox.Event) bool {
	err := enqueueEvents(ctx, db, evs)
	if err == nil {
		return true
	}
	log.Printf("webhook-listener: enqueue %s/%s events: %v", source, eventType, err)
	return saveToDeadLetter(ctx, db, source, eventType, body, err.Error()) == nil
}

func githubWebhookHandler(ctx context.Context, db webhookDB, secrets *githubSecretSource) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		body, ok := readWebhookBody(w, r)
		if !ok {
			return
		}
		var err error

		// Every delivery must carry a valid X-Hub-Signature-256; with no
		// secret configured nothing is accepted.
		if !authorizeGitHubDelivery(w, r, secrets, body) {
			return
		}

		eventType := r.Header.Get("X-GitHub-Event")
		// The delivery id is stable across GitHub's redeliveries; event ids
		// are derived from it so a redelivery is a no-op in the outbox.
		delivery := r.Header.Get("X-GitHub-Delivery")
		now := time.Now().UTC().Format(time.RFC3339)

		switch eventType {
		case "push":
			var payload githubPushPayload
			if err := json.Unmarshal(body, &payload); err != nil {
				log.Printf("webhook-listener: github push: unmarshal: %v", err)
				saveToDeadLetter(ctx, db, "github", eventType, body, "unmarshal error: "+err.Error()) //nolint:errcheck
				w.WriteHeader(http.StatusBadRequest)
				return
			}

			// One repository lookup per push: its id is the aggregate of
			// every event and the repo_id of the extract request. A push
			// to a watched branch of a registered repository also gets an
			// extract request (commit-linked codex); both events describe
			// the same push and are enqueued together.
			branch := strings.TrimPrefix(payload.Ref, "refs/heads/")
			repoID, registered := repositoryAggregateID(ctx, db, payload.Repository.FullName)
			watched := registered && isWatchedBranch(ctx, db, repoID, branch)

			// Only pushes to main/master are external changes.
			var evs []outbox.Event
			if payload.Ref == "refs/heads/main" || payload.Ref == "refs/heads/master" || watched {
				evs, err = pushEvents(payload, repoID, registered, watched, now, delivery)
				if err != nil {
					log.Printf("webhook-listener: build push events: %v", err)
					http.Error(w, "failed to build events", http.StatusInternalServerError)
					return
				}
			}
			if !acceptEvents(ctx, db, "github", eventType, body, evs) {
				http.Error(w, "event could not be recorded", http.StatusServiceUnavailable)
				return
			}

		case "pull_request":
			var payload githubPRPayload
			if err := json.Unmarshal(body, &payload); err != nil {
				log.Printf("webhook-listener: github pr: unmarshal: %v", err)
				saveToDeadLetter(ctx, db, "github", eventType, body, "unmarshal error: "+err.Error()) //nolint:errcheck
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if payload.Action == "closed" && payload.PullRequest.Merged {
				branch := payload.PullRequest.Head.Ref
				isAgent := strings.HasPrefix(branch, "agent/")
				// Resolve work_id from the agent branch naming convention:
				//   agent/<workID>           (parent work item branch)
				//   agent/<workID>/<shortTaskID> (subtask branch)
				// Non-agent branches (human-created PRs) have no work_id to
				// resolve — we skip the event rather than emit an empty
				// work_id that the status-orchestrator will reject.
				workID := resolveWorkIDFromBranch(branch)
				event := prMergedEventFor(payload, workID, now)
				if workID == "" && len(event.RevertsPRNumbers) > 0 {
					// M4-05: a human revert PR has no work item, but the
					// outcomes of the PRs it reverts must learn of it.
					log.Printf("webhook-listener: pr.merged: non-agent PR #%d reverts %v (heuristic) — forwarding without work_id",
						payload.PullRequest.Number, event.RevertsPRNumbers)
				}
				if workID == "" && len(event.RevertsPRNumbers) == 0 {
					if isAgent {
						log.Printf("webhook-listener: pr.merged: could not parse work_id from agent branch %q (pr #%d) — skipping",
							branch, payload.PullRequest.Number)
					} else {
						log.Printf("webhook-listener: pr.merged: non-agent branch %q (pr #%d) — skipping (no work_id)",
							branch, payload.PullRequest.Number)
					}
				} else {
					key := fmt.Sprintf("pr-%d", payload.PullRequest.Number)
					aggregate, _ := repositoryAggregateID(ctx, db, payload.Repository.FullName)
					eventID, _ := githubEventID(delivery, bus.TopicPRMerged, 0)
					ev, buildErr := newEventWithID(eventID, aggregateTypeRepository, aggregate, bus.TopicPRMerged, key, event)
					if buildErr != nil {
						log.Printf("webhook-listener: build pr.merged event: %v", buildErr)
						http.Error(w, "failed to build event", http.StatusInternalServerError)
						return
					}
					if !acceptEvents(ctx, db, "github", eventType, body, []outbox.Event{ev}) {
						http.Error(w, "event could not be recorded", http.StatusServiceUnavailable)
						return
					}
				}
			}

		case "pull_request_review":
			// Review comments on GitHub PRs are no longer forwarded to a
			// Kafka topic — the previous `human-review.queue` target had no
			// subscriber, and the dashboard now reads human-review items
			// directly from the DB (state=human_review / blocked). The event
			// is accepted with 200 OK so GitHub doesn't retry.
			log.Printf("webhook-listener: github pr_review received (ignored — DB-driven queue)")

		case "issue_comment":
			// Issue comments are likewise dropped (see pull_request_review).
			log.Printf("webhook-listener: github issue_comment received (ignored — DB-driven queue)")

		default:
			log.Printf("webhook-listener: unhandled github event type: %s", eventType)
		}

		w.WriteHeader(http.StatusOK)
	}
}

func jiraWebhookHandler(ctx context.Context, db webhookDB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		body, ok := readWebhookBody(w, r)
		if !ok {
			return
		}

		var payload jiraWebhookPayload
		if err := json.Unmarshal(body, &payload); err != nil {
			log.Printf("webhook-listener: jira: unmarshal: %v", err)
			saveToDeadLetter(ctx, db, "jira", "unknown", body, "unmarshal error: "+err.Error()) //nolint:errcheck
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}

		now := time.Now().UTC().Format(time.RFC3339)
		issueKey := payload.Issue.Key
		if issueKey == "" {
			w.WriteHeader(http.StatusOK)
			return
		}

		// Handle changelog items (priority changes, status transitions):
		// one event per item, enqueued together on the issue's aggregate.
		aggregate := syntheticJiraIssueID(issueKey)
		evs := make([]outbox.Event, 0, len(payload.ChangeLog.Items))
		for i, item := range payload.ChangeLog.Items {
			event := jiraChangeEvent{
				IssueKey:  issueKey,
				EventType: payload.WebhookEvent,
				Field:     item.Field,
				From:      item.FromStr,
				To:        item.ToStr,
				Timestamp: now,
			}
			key := fmt.Sprintf("jira-%s-%s", issueKey, item.Field)
			eventID, _ := jiraEventID(payload.Timestamp, issueKey, i)
			ev, buildErr := newEventWithID(eventID, aggregateTypeJiraIssue, aggregate, bus.TopicExternalChangeDetected, key, event)
			if buildErr != nil {
				log.Printf("webhook-listener: build jira change event: %v", buildErr)
				http.Error(w, "failed to build event", http.StatusInternalServerError)
				return
			}
			evs = append(evs, ev)
		}
		if !acceptEvents(ctx, db, "jira", payload.WebhookEvent, body, evs) {
			http.Error(w, "event could not be recorded", http.StatusServiceUnavailable)
			return
		}

		w.WriteHeader(http.StatusOK)
	}
}

// readWebhookBody reads a webhook body capped at maxWebhookBodyBytes. It writes
// 413 when the body is too large and 400 on any other read error.
func readWebhookBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxWebhookBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return nil, false
		}
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return nil, false
	}
	return body, true
}
