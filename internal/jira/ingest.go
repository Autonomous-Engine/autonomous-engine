package jira

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/outbox"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Ingester polls Jira on a configurable interval and creates work items for
// newly discovered tickets matching the configured JQL.
//
// ProductID (when non-nil) scopes every ingested work item, recorded ticket
// row, and intake event to that product. Without it the pipeline
// receives productless work items and every downstream product-scoped
// guard silently skips them — see audit findings A.5 and A.6. Operators
// configuring Jira ingest for a multi-product deployment must supply one
// Ingester per product.
//
// The intake event is enqueued in the transactional outbox together with the
// work item (see commitIngest); the host service must run an outbox
// dispatcher (cmd/jira-sync does).
type Ingester struct {
	client    *JiraClient
	db        *pgxpool.Pool
	workgraph *workgraph.WorkGraph
	mapping   *WorkflowMapping
	productID *uuid.UUID
	cancel    context.CancelFunc
}

// NewIngester creates a new Ingester. productID may be nil for legacy/global
// deployments but the caller should log a warning; see ingestIssue which
// blocks the work item when productID is absent.
func NewIngester(
	client *JiraClient,
	db *pgxpool.Pool,
	wg *workgraph.WorkGraph,
	mapping *WorkflowMapping,
	productID *uuid.UUID,
) *Ingester {
	return &Ingester{
		client:    client,
		db:        db,
		workgraph: wg,
		mapping:   mapping,
		productID: productID,
	}
}

// Start spawns a goroutine that polls Jira on the given interval using the
// provided JQL query to discover new tickets.
func (ing *Ingester) Start(ctx context.Context, jql string, interval time.Duration) {
	childCtx, cancel := context.WithCancel(ctx)
	ing.cancel = cancel

	go func() {
		log.Printf("jira-ingest: starting poll loop (interval=%s, jql=%q)", interval, jql)
		// Run immediately on startup, then on every tick.
		ing.poll(childCtx, jql)

		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-childCtx.Done():
				log.Println("jira-ingest: poll loop stopped")
				return
			case <-ticker.C:
				ing.poll(childCtx, jql)
			}
		}
	}()
}

// Stop cancels the background poll loop.
func (ing *Ingester) Stop() {
	if ing.cancel != nil {
		ing.cancel()
	}
}

// IngestSingle ingests a single Jira ticket by key.
func (ing *Ingester) IngestSingle(ctx context.Context, jiraKey string) error {
	body, err := ing.client.doRequest(ctx, http.MethodGet, "/rest/api/3/issue/"+jiraKey)
	if err != nil {
		return fmt.Errorf("fetch issue %s: %w", jiraKey, err)
	}

	var issue jiraIssue
	if err := json.Unmarshal(body, &issue); err != nil {
		return fmt.Errorf("parse issue %s: %w", jiraKey, err)
	}

	return ing.ingestIssue(ctx, issue)
}

// ─── Internal helpers ─────────────────────────────────────────────────────────

// jiraIssue is a minimal representation of a Jira issue from the search API.
type jiraIssue struct {
	Key    string `json:"key"`
	Fields struct {
		Summary     string `json:"summary"`
		Description string `json:"description"`
		IssueType   struct {
			Name string `json:"name"`
		} `json:"issuetype"`
		Priority struct {
			Name string `json:"name"`
		} `json:"priority"`
		Labels []string `json:"labels"`
	} `json:"fields"`
}

// poll fetches tickets from Jira matching the JQL and ingests any that are new.
func (ing *Ingester) poll(ctx context.Context, jql string) {
	body, err := ing.client.doRequest(ctx, http.MethodGet,
		"/rest/api/3/search?jql="+url.QueryEscape(jql)+"&maxResults=50")
	if err != nil {
		log.Printf("jira-ingest: search error: %v", err)
		return
	}

	var result struct {
		Issues []jiraIssue `json:"issues"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		log.Printf("jira-ingest: parse search result: %v", err)
		return
	}

	ingested := 0
	for _, issue := range result.Issues {
		if err := ing.ingestIssue(ctx, issue); err != nil {
			log.Printf("jira-ingest: ingest issue %s: %v", issue.Key, err)
			continue
		}
		ingested++
	}

	if len(result.Issues) > 0 {
		log.Printf("jira-ingest: polled %d issues, ingested %d new", len(result.Issues), ingested)
	}
}

// ingestIssue creates a work item from a single Jira issue, skipping it if it
// has already been ingested.
func (ing *Ingester) ingestIssue(ctx context.Context, issue jiraIssue) error {
	// a. Check if already ingested — skip if a row exists
	var existingID uuid.UUID
	scanErr := ing.db.QueryRow(ctx,
		`SELECT work_item_id FROM jira_ingested_tickets WHERE jira_key = $1`,
		issue.Key,
	).Scan(&existingID)
	if scanErr == nil {
		// Row found: already ingested
		return nil
	}

	// a2. Block ingest when no product is configured. The audit found that
	// Jira-ingested work items with NULL product_id bypass every per-product
	// guard (CheckOnboardingComplete, codex lookups, dashboard filters).
	// Refusing here makes the failure loud rather than silently polluting
	// the pipeline — see finding A.6.
	if ing.productID == nil {
		log.Printf("jira-ingest: refusing to ingest %s — Ingester has no product_id configured; set one per Jira project or connector", issue.Key)
		return fmt.Errorf("jira-ingest: no product_id configured for Jira ticket %s", issue.Key)
	}
	productID := *ing.productID

	// b. Extract fields
	summary := issue.Fields.Summary
	description := issue.Fields.Description
	jiraIssueType := issue.Fields.IssueType.Name
	jiraPriority := issue.Fields.Priority.Name

	// c. Map Jira issue type → internal work item type (reverse lookup)
	internalType := models.WorkItemTypeTask // default
	for internalT, jiraT := range ing.mapping.IssueTypeMap {
		if jiraT == jiraIssueType {
			internalType = models.WorkItemType(internalT)
			break
		}
	}

	// d. Map Jira priority → internal priority (reverse lookup)
	internalPriority := models.PriorityMedium // default
	for internalP, jiraP := range ing.mapping.PriorityMap {
		if jiraP == jiraPriority {
			internalPriority = models.Priority(internalP)
			break
		}
	}

	// e. Create the work item, scoped to the configured product so it
	// surfaces on per-product dashboards and runs through product-scoped
	// guards like every other intake source.
	workID := uuid.New()
	pid := productID
	item := &models.WorkItem{
		ID:          workID,
		ExternalRef: issue.Key,
		Type:        internalType,
		State:       models.WorkItemStateIntake,
		Title:       summary,
		Description: description,
		Priority:    internalPriority,
		ProductID:   &pid,
	}
	// f/g. The work item, its intake event and the jira_ingested_tickets
	// row commit together. Before the outbox a failed intake publish was
	// logged and ignored (the work item sat in intake with nothing on the bus)
	// and a failed ticket insert left a work item the next poll would
	// duplicate; now either all three land or none does and the next poll
	// retries the ticket.
	if err := commitIngest(ctx, ing.db, ing.workgraph, item, issue.Key, productID, description); err != nil {
		return fmt.Errorf("ingest %s: %w", issue.Key, err)
	}

	// h. Add ingest label to Jira ticket
	ingestLabel := ing.mapping.IngestLabel
	if ingestLabel == "" {
		ingestLabel = "autonomous-ingest"
	}
	if err := ing.addLabel(ctx, issue.Key, ingestLabel, issue.Fields.Labels); err != nil {
		log.Printf("jira-ingest: add label to %s: %v", issue.Key, err)
		// Non-fatal
	}

	// i. Add comment to Jira ticket
	comment := fmt.Sprintf("Picked up by Autonomous Engineering System. Tracking ID: %s", workID.String())
	if err := ing.addComment(ctx, issue.Key, comment); err != nil {
		log.Printf("jira-ingest: add comment to %s: %v", issue.Key, err)
		// Non-fatal
	}

	log.Printf("jira-ingest: ingested %s as work_item %s", issue.Key, workID)
	return nil
}

// insertIngestedTicketSQL records that a Jira ticket was ingested (product-scoped).
const insertIngestedTicketSQL = `INSERT INTO jira_ingested_tickets (jira_key, work_item_id, product_id) VALUES ($1, $2, $3)`

// commitIngest writes the work item, enqueues intake and records the
// ingested ticket in one transaction.
//
// The intake payload is the canonical intakeMessage shape (work_id,
// product_id, description, source, metadata) — the previous bespoke {work_id}
// payload made product-agent read intake.ProductID == "" and silently skip
// product-scoped checks (audit finding A.6). Keep it in lockstep with the
// intakeMessage struct in cmd/product-agent/main.go.
func commitIngest(ctx context.Context, db workgraph.TxBeginner, wg *workgraph.WorkGraph, item *models.WorkItem, issueKey string, productID uuid.UUID, description string) error {
	payload := map[string]interface{}{
		"work_id":     item.ID.String(),
		"product_id":  productID.String(),
		"description": description,
		"source":      "jira",
		"metadata": map[string]string{
			"jira_key": issueKey,
		},
	}
	return workgraph.WithTx(ctx, db, func(tx pgx.Tx) error {
		if err := wg.CreateWorkItemTx(ctx, tx, item); err != nil {
			return fmt.Errorf("create work item: %w", err)
		}
		if err := outbox.EnqueueJSON(ctx, tx, item.ID, bus.TopicIntake, item.ID.String(), payload); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, insertIngestedTicketSQL, issueKey, item.ID, productID); err != nil {
			return fmt.Errorf("record ingested ticket: %w", err)
		}
		return nil
	})
}

// addLabel updates the Jira issue's labels to include the given label.
func (ing *Ingester) addLabel(ctx context.Context, issueKey, label string, existing []string) error {
	for _, l := range existing {
		if l == label {
			return nil // already labelled
		}
	}
	newLabels := append(existing, label)

	body, err := json.Marshal(map[string]interface{}{
		"update": map[string]interface{}{
			"labels": []map[string]interface{}{
				{"set": newLabels},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("marshal label update: %w", err)
	}

	return ing.doWriteRequest(ctx, http.MethodPut, "/rest/api/3/issue/"+issueKey, body)
}

// addComment posts a comment on a Jira issue using the Atlassian Document Format.
func (ing *Ingester) addComment(ctx context.Context, issueKey, text string) error {
	body, err := json.Marshal(map[string]interface{}{
		"body": map[string]interface{}{
			"type":    "doc",
			"version": 1,
			"content": []map[string]interface{}{
				{
					"type": "paragraph",
					"content": []map[string]interface{}{
						{"type": "text", "text": text},
					},
				},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("marshal comment body: %w", err)
	}

	return ing.doWriteRequest(ctx, http.MethodPost, "/rest/api/3/issue/"+issueKey+"/comment", body)
}

// doWriteRequest performs an authenticated PUT or POST request with a JSON body.
func (ing *Ingester) doWriteRequest(ctx context.Context, method, path string, body []byte) error {
	reqURL := ing.client.baseURL + path
	req, err := http.NewRequestWithContext(ctx, method, reqURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build %s request %s: %w", method, path, err)
	}

	creds := base64.StdEncoding.EncodeToString([]byte(ing.client.email + ":" + ing.client.token))
	req.Header.Set("Authorization", "Basic "+creds)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := ing.client.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("do %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body) // drain body

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("jira API %s %s returned %d", method, path, resp.StatusCode)
	}
	return nil
}
