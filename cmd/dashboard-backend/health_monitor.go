package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type serviceTarget struct {
	Name     string `json:"name"`
	URL      string `json:"-"`
	Critical bool   `json:"critical"`
}

type ServiceStatus struct {
	Name      string                 `json:"name"`
	Status    string                 `json:"status"`
	Message   string                 `json:"message,omitempty"`
	Details   map[string]interface{} `json:"details,omitempty"`
	Critical  bool                   `json:"critical"`
	LastCheck time.Time              `json:"last_check"`
	SinceLast string                 `json:"since_last,omitempty"`
}

type HealthMonitor struct {
	db       *pgxpool.Pool
	hub      *WSHub
	client   *http.Client
	targets  []serviceTarget
	mu       sync.RWMutex
	statuses map[string]*ServiceStatus
}

var monitoredServices = []serviceTarget{
	{Name: "gateway", URL: "http://gateway:8082/health", Critical: true},
	{Name: "status-orchestrator", URL: "http://status-orchestrator:8090/health", Critical: true},
	{Name: "dashboard-backend", URL: "http://localhost:8080/health", Critical: true},
	{Name: "github-service", URL: "http://github-service:8085/health", Critical: true},
	{Name: "chat-agent", URL: "http://chat-agent:8099/health", Critical: false},
	{Name: "product-agent", URL: "http://product-agent:8091/health", Critical: false},
	{Name: "research-coordinator", URL: "http://research-coordinator:8092/health", Critical: false},
	{Name: "implementation-planner", URL: "http://implementation-planner:8094/health", Critical: false},
	{Name: "pr-coordinator", URL: "http://pr-coordinator:8096/health", Critical: false},
	{Name: "review-agent", URL: "http://review-agent:8097/health", Critical: false},
	{Name: "change-reconciler", URL: "http://change-reconciler:8100/health", Critical: false},
	{Name: "credential-monitor", URL: "http://credential-monitor:8101/health", Critical: false},
	{Name: "knowledge-indexer", URL: "http://knowledge-indexer:8087/health", Critical: false},
	{Name: "onboarding", URL: "http://onboarding:8086/health", Critical: false},
}

func NewHealthMonitor(db *pgxpool.Pool, hub *WSHub) *HealthMonitor {
	return &HealthMonitor{
		db:  db,
		hub: hub,
		client: &http.Client{
			Timeout: 5 * time.Second,
		},
		targets:  monitoredServices,
		statuses: make(map[string]*ServiceStatus),
	}
}

func (hm *HealthMonitor) Start(ctx context.Context) {
	hm.checkAll(ctx)

	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			hm.checkAll(ctx)
		}
	}
}

func (hm *HealthMonitor) checkAll(ctx context.Context) {
	var wg sync.WaitGroup
	results := make([]ServiceStatus, len(hm.targets))

	for i, target := range hm.targets {
		wg.Add(1)
		go func(idx int, t serviceTarget) {
			defer wg.Done()
			results[idx] = hm.checkService(ctx, t)
		}(i, target)
	}
	wg.Wait()

	hm.mu.Lock()
	var changed []ServiceStatus
	for _, result := range results {
		prev, exists := hm.statuses[result.Name]
		if !exists || prev.Status != result.Status {
			changed = append(changed, result)
			if exists {
				result.SinceLast = prev.LastCheck.Format(time.RFC3339)
				hm.recordEvent(ctx, result, prev.Status)
			} else if result.Status != "healthy" {
				hm.recordEvent(ctx, result, "")
			}
		}
		r := result
		hm.statuses[result.Name] = &r
	}
	hm.mu.Unlock()

	if len(changed) > 0 {
		// Every replica runs its own monitor, so its view stays on its
		// own clients rather than being relayed once per replica.
		hm.hub.BroadcastLocal("service_health", map[string]interface{}{
			"services": hm.GetStatuses(),
			"changes":  changed,
		})
	}
}

func (hm *HealthMonitor) checkService(ctx context.Context, target serviceTarget) ServiceStatus {
	status := ServiceStatus{
		Name:      target.Name,
		Critical:  target.Critical,
		LastCheck: time.Now(),
	}

	req, err := http.NewRequestWithContext(ctx, "GET", target.URL, nil)
	if err != nil {
		status.Status = "unhealthy"
		status.Message = "failed to create request"
		return status
	}

	resp, err := hm.client.Do(req)
	if err != nil {
		status.Status = "unhealthy"
		status.Message = fmt.Sprintf("unreachable: %v", err)
		return status
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	if resp.StatusCode != http.StatusOK {
		status.Status = "unhealthy"
		status.Message = fmt.Sprintf("HTTP %d", resp.StatusCode)
		return status
	}

	var healthResp map[string]interface{}
	if json.Unmarshal(body, &healthResp) == nil {
		if s, ok := healthResp["status"].(string); ok && s == "healthy" {
			status.Status = "healthy"
		} else {
			status.Status = "unhealthy"
			status.Message = fmt.Sprintf("reported status: %v", healthResp["status"])
		}
		if checks, ok := healthResp["checks"].(map[string]interface{}); ok {
			status.Details = checks
		}
	} else {
		status.Status = "healthy"
	}

	return status
}

func (hm *HealthMonitor) recordEvent(ctx context.Context, s ServiceStatus, previousStatus string) {
	eventStatus := s.Status
	if previousStatus == "unhealthy" && s.Status == "healthy" {
		eventStatus = "recovered"
	}

	details := map[string]interface{}{
		"previous_status": previousStatus,
	}
	if s.Details != nil {
		details["checks"] = s.Details
	}
	detailsJSON, _ := json.Marshal(details)

	_, err := hm.db.Exec(ctx, `
		INSERT INTO service_health_events (service_name, status, message, details)
		VALUES ($1, $2, $3, $4)
	`, s.Name, eventStatus, s.Message, detailsJSON)
	if err != nil {
		log.Printf("health-monitor: record event for %s: %v", s.Name, err)
	}

	log.Printf("health-monitor: %s → %s (was: %s) %s", s.Name, eventStatus, previousStatus, s.Message)
}

func (hm *HealthMonitor) GetStatuses() []ServiceStatus {
	hm.mu.RLock()
	defer hm.mu.RUnlock()

	out := make([]ServiceStatus, 0, len(hm.statuses))
	for _, s := range hm.statuses {
		out = append(out, *s)
	}
	return out
}

func (hm *HealthMonitor) HandleGetHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	statuses := hm.GetStatuses()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"services": statuses,
	})
}

func (hm *HealthMonitor) HandleGetHealthEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	limit := 50
	rows, err := hm.db.Query(r.Context(), `
		SELECT id, service_name, status, message, details, created_at
		FROM service_health_events
		ORDER BY created_at DESC
		LIMIT $1
	`, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	type event struct {
		ID          string          `json:"id"`
		ServiceName string          `json:"service_name"`
		Status      string          `json:"status"`
		Message     string          `json:"message"`
		Details     json.RawMessage `json:"details"`
		CreatedAt   time.Time       `json:"created_at"`
	}
	var events []event
	for rows.Next() {
		var e event
		if err := rows.Scan(&e.ID, &e.ServiceName, &e.Status, &e.Message, &e.Details, &e.CreatedAt); err != nil {
			continue
		}
		events = append(events, e)
	}
	if events == nil {
		events = []event{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"events": events,
	})
}
