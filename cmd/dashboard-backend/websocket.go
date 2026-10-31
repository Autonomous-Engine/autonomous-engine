package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/sharedstate"
	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

// defaultChannels are the channels every new WebSocket client is subscribed to.
var defaultChannels = []string{"pipeline", "agents", "queues", "alerts", "prompts", "indexing", "service_health"}

// upgrader configures the WebSocket upgrader. CheckOrigin permits all origins
// so the React dev server can connect without CORS issues.
var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

// wsMessage is the JSON structure used for subscribe/unsubscribe control frames
// sent by clients.
type wsMessage struct {
	Action  string `json:"action"` // "subscribe" | "unsubscribe"
	Channel string `json:"channel"`
}

// broadcastPayload is the JSON wrapper written to clients on Broadcast.
type broadcastPayload struct {
	Channel string          `json:"channel"`
	Data    json.RawMessage `json:"data"`
}

// WSHub manages active WebSocket connections and fan-out broadcasting.
//
// Without Redis, Broadcast writes straight to this replica's clients. With
// EnableRedisFanout, Broadcast publishes to Redis instead and every replica,
// this one included, writes the event to its clients when it comes back on
// its subscription. A replica normally delivers an event either directly or
// via Redis, not both: the publisher writes locally only when the publish
// failed or Redis reports that no subscription received it.
//
// One rare case delivers twice on the publishing replica: the publish
// reached Redis but its reply timed out, so the hub treats it as failed and
// delivers locally while its subscription also delivers it. The reverse
// also exists: if other replicas are subscribed but this one's own
// subscription is still reconnecting, its clients miss the event.
type WSHub struct {
	mu      sync.RWMutex
	clients map[*websocket.Conn]map[string]bool
	db      *pgxpool.Pool

	// relay and fallback are set by EnableRedisFanout; nil means direct
	// delivery only.
	relay    *sharedstate.Relay
	fallback *sharedstate.Fallback
}

// wsPublishTimeout bounds one Redis publish; past it the event is delivered
// to this replica's clients only.
const wsPublishTimeout = 500 * time.Millisecond

// NewWSHub creates and returns an empty WSHub.
func NewWSHub(db *pgxpool.Pool) *WSHub {
	return &WSHub{
		clients: make(map[*websocket.Conn]map[string]bool),
		db:      db,
	}
}

// HandleConnect upgrades the HTTP connection to WebSocket, registers the client
// with the default channel set, and spawns a goroutine to handle incoming
// subscribe/unsubscribe control frames. When the client disconnects it is
// removed from the hub.
func (h *WSHub) HandleConnect(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("ws: upgrade error: %v", err)
		return
	}

	// Register client with default subscriptions.
	subs := make(map[string]bool, len(defaultChannels))
	for _, ch := range defaultChannels {
		subs[ch] = true
	}

	h.mu.Lock()
	h.clients[conn] = subs
	h.mu.Unlock()

	log.Printf("ws: client connected (%s)", conn.RemoteAddr())

	// Read loop — handles control messages and detects disconnection.
	go func() {
		defer func() {
			h.mu.Lock()
			delete(h.clients, conn)
			h.mu.Unlock()
			conn.Close()
			log.Printf("ws: client disconnected (%s)", conn.RemoteAddr())
		}()

		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
					log.Printf("ws: read error (%s): %v", conn.RemoteAddr(), err)
				}
				return
			}

			var msg wsMessage
			if err := json.Unmarshal(raw, &msg); err != nil {
				// Ignore malformed control frames.
				continue
			}

			if msg.Channel == "" {
				continue
			}

			h.mu.Lock()
			switch msg.Action {
			case "subscribe":
				h.clients[conn][msg.Channel] = true
			case "unsubscribe":
				delete(h.clients[conn], msg.Channel)
			}
			h.mu.Unlock()
		}
	}()
}

// EnableRedisFanout makes Broadcast reach the clients of every replica
// sharing c: events are published to Redis channel "ae:ws:<channel>" and
// this hub delivers what it receives on "ae:ws:*" to its own clients. It
// returns once the subscription is confirmed, so call it before
// StartKafkaFanout and before serving clients. The subscription ends with
// ctx.
func (h *WSHub) EnableRedisFanout(ctx context.Context, c *redis.Client) error {
	relay := sharedstate.NewRelay(c)
	if err := relay.Subscribe(ctx, h.deliverLocal); err != nil {
		return err
	}
	h.mu.Lock()
	h.relay = relay
	h.fallback = sharedstate.NewFallback("dashboard-ws", 0)
	h.mu.Unlock()
	return nil
}

// encodeBroadcast wraps data in the channel envelope clients receive.
func encodeBroadcast(channel string, data interface{}) ([]byte, bool) {
	dataBytes, err := json.Marshal(data)
	if err != nil {
		log.Printf("ws: marshal broadcast data for channel %q: %v", channel, err)
		return nil, false
	}
	payload, err := json.Marshal(broadcastPayload{
		Channel: channel,
		Data:    json.RawMessage(dataBytes),
	})
	if err != nil {
		log.Printf("ws: marshal broadcast payload for channel %q: %v", channel, err)
		return nil, false
	}
	return payload, true
}

// Broadcast serialises data as JSON, wraps it in a channel envelope, and sends
// it to every client subscribed to channel on every replica (on this replica
// only when Redis fan-out is not enabled). Use it for events that exist once
// in the deployment: Kafka events and REST actions. When Redis is
// unreachable the event reaches this replica's clients only; the fallback
// is logged and counted in ae_redis_fallback_total{component="dashboard-ws"}.
func (h *WSHub) Broadcast(channel string, data interface{}) {
	payload, ok := encodeBroadcast(channel, data)
	if !ok {
		return
	}
	h.mu.RLock()
	relay, fallback := h.relay, h.fallback
	h.mu.RUnlock()
	if relay == nil {
		h.deliverLocal(channel, payload)
		return
	}
	if !fallback.Available() {
		fallback.Skipped()
		h.deliverLocal(channel, payload)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), wsPublishTimeout)
	defer cancel()
	receivers, err := relay.Publish(ctx, channel, payload)
	if err != nil {
		fallback.Fail(err)
		h.deliverLocal(channel, payload)
		return
	}
	fallback.Recover()
	if receivers == 0 {
		// No subscription, not even ours (e.g. still resubscribing after a
		// Redis restart): Redis dropped it, so deliver here at least.
		h.deliverLocal(channel, payload)
	}
	// Otherwise this replica's subscription delivers it.
}

// BroadcastLocal sends data to this replica's subscribed clients only. Use
// it for events every replica produces for itself (the health monitor), which
// relaying would deliver once per replica.
func (h *WSHub) BroadcastLocal(channel string, data interface{}) {
	if payload, ok := encodeBroadcast(channel, data); ok {
		h.deliverLocal(channel, payload)
	}
}

// deliverLocal writes an encoded envelope to this replica's clients
// subscribed to channel. Write errors cause the offending connection to be
// closed and removed.
func (h *WSHub) deliverLocal(channel string, payload []byte) {
	h.mu.RLock()
	// Collect targets before releasing the read lock to keep the critical section short.
	targets := make([]*websocket.Conn, 0)
	for conn, subs := range h.clients {
		if subs[channel] {
			targets = append(targets, conn)
		}
	}
	h.mu.RUnlock()

	var failed []*websocket.Conn
	for _, conn := range targets {
		if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
			log.Printf("ws: write error (%s): %v — removing client", conn.RemoteAddr(), err)
			failed = append(failed, conn)
		}
	}

	if len(failed) > 0 {
		h.mu.Lock()
		for _, conn := range failed {
			delete(h.clients, conn)
			conn.Close()
		}
		h.mu.Unlock()
	}
}

// StartKafkaFanout subscribes to Kafka topics and broadcasts events to the
// appropriate WebSocket channels. It runs asynchronously; errors from the first
// subscription are returned immediately, subsequent ones are logged.
func (h *WSHub) StartKafkaFanout(msgBus bus.MessageBus) error {
	if err := msgBus.Subscribe(bus.TopicStateChanged, "dashboard-backend-fanout", func(ctx context.Context, msg *bus.Message) error {
		h.Broadcast("pipeline", json.RawMessage(msg.Value))
		return nil
	}); err != nil {
		return err
	}

	if err := msgBus.Subscribe(bus.TopicToolExecuted, "dashboard-ws", func(ctx context.Context, msg *bus.Message) error {
		var event interface{}
		if jsonErr := json.Unmarshal(msg.Value, &event); jsonErr != nil {
			log.Printf("ws: unmarshal tool.executed event: %v", jsonErr)
			return nil
		}
		h.Broadcast("tool_calls", event)
		return nil
	}); err != nil {
		log.Printf("ws: subscribe tool.executed: %v", err)
	}

	// Forward console.stream → "console" WS channel (full-fidelity tool output)
	// and persist to DB for historical retrieval.
	if err := msgBus.Subscribe(bus.TopicConsoleStream, "dashboard-ws-console", func(ctx context.Context, msg *bus.Message) error {
		var event consoleToolEvent // handlers_runs.go: carries phase / exit_code (M3-02)
		if jsonErr := json.Unmarshal(msg.Value, &event); jsonErr != nil {
			log.Printf("ws: unmarshal console.stream event: %v", jsonErr)
			return nil
		}
		h.Broadcast("console", event)
		if h.db != nil {
			if dbErr := persistToolEvent(ctx, h.db, event); dbErr != nil {
				log.Printf("ws: persist tool_event: %v", dbErr)
			}
		}
		return nil
	}); err != nil {
		log.Printf("ws: subscribe console.stream: %v", err)
	}

	// Forward chat.response → "chat" WS channel for real-time chat responses.
	// Not in defaultChannels — clients subscribe when they open the chat panel.
	if err := msgBus.Subscribe(bus.TopicChatResponse, "dashboard-ws-chat", func(ctx context.Context, msg *bus.Message) error {
		var event interface{}
		if jsonErr := json.Unmarshal(msg.Value, &event); jsonErr != nil {
			log.Printf("ws: unmarshal chat.response event: %v", jsonErr)
			return nil
		}
		h.Broadcast("chat", event)
		return nil
	}); err != nil {
		log.Printf("ws: subscribe chat.response: %v", err)
	}

	// Forward onboarding.prefill.completed → alerts WS channel so the wizard
	// knows when a pre-fill finished and how many candidates/patterns/drifts
	// were produced.
	if err := msgBus.Subscribe(bus.TopicOnboardingPrefillCompleted, "dashboard-backend-onboarding-completed", func(ctx context.Context, msg *bus.Message) error {
		var payload struct {
			ProductID     string          `json:"product_id"`
			ModulesFound  int             `json:"modules_found"`
			PatternsFound int             `json:"patterns_found"`
			Drifts        json.RawMessage `json:"drifts"`
		}
		if err := json.Unmarshal(msg.Value, &payload); err != nil {
			log.Printf("ws: unmarshal onboarding.prefill.completed: %v", err)
			return nil
		}
		drifts := payload.Drifts
		if len(drifts) == 0 {
			drifts = json.RawMessage("[]")
		}
		h.Broadcast("alerts", map[string]interface{}{
			"kind":           "onboarding_prefill_completed",
			"product_id":     payload.ProductID,
			"modules_found":  payload.ModulesFound,
			"patterns_found": payload.PatternsFound,
			"drifts":         drifts,
		})
		return nil
	}); err != nil {
		log.Printf("ws: subscribe onboarding.prefill.completed: %v", err)
	}

	// Forward onboarding.prefill.drift_detected → alerts WS channel. Drives the
	// CredentialAlertBanner / drift-banner UI.
	if err := msgBus.Subscribe(bus.TopicOnboardingPrefillDriftDetected, "dashboard-backend-onboarding-drift", func(ctx context.Context, msg *bus.Message) error {
		var payload struct {
			ProductID string          `json:"product_id"`
			Drifts    json.RawMessage `json:"drifts"`
		}
		if err := json.Unmarshal(msg.Value, &payload); err != nil {
			log.Printf("ws: unmarshal onboarding.prefill.drift_detected: %v", err)
			return nil
		}
		drifts := payload.Drifts
		if len(drifts) == 0 {
			drifts = json.RawMessage("[]")
		}
		h.Broadcast("alerts", map[string]interface{}{
			"kind":       "onboarding_drift_detected",
			"product_id": payload.ProductID,
			"drifts":     drifts,
		})
		return nil
	}); err != nil {
		log.Printf("ws: subscribe onboarding.prefill.drift_detected: %v", err)
	}

	// Forward onboarding.prefill.progress → alerts WS channel so the wizard
	// can show a live progress indicator while pre-fill is running.
	if err := msgBus.Subscribe(bus.TopicOnboardingPrefillProgress, "dashboard-backend-onboarding-progress", func(ctx context.Context, msg *bus.Message) error {
		var payload struct {
			ProductID string `json:"product_id"`
			Phase     string `json:"phase"`
			Message   string `json:"message"`
			Percent   int    `json:"percent"`
		}
		if err := json.Unmarshal(msg.Value, &payload); err != nil {
			log.Printf("ws: unmarshal onboarding.prefill.progress: %v", err)
			return nil
		}
		h.Broadcast("alerts", map[string]interface{}{
			"kind":       "onboarding_prefill_progress",
			"product_id": payload.ProductID,
			"phase":      payload.Phase,
			"message":    payload.Message,
			"percent":    payload.Percent,
		})
		return nil
	}); err != nil {
		log.Printf("ws: subscribe onboarding.prefill.progress: %v", err)
	}

	// Forward Kafka "alerts" topic → alerts WS channel. Publishers are
	// internal/alerts/alerts.go (metric threshold trips) and
	// internal/credmonitor/alert.go (credential expiry). Without this
	// subscription, alert events were committed to Kafka and then never
	// surfaced to operators — the dashboard drift banner stayed silent
	// even when credentials had expired. Payload shape varies slightly by
	// publisher, so we forward the raw JSON wrapped in a "kind=alert"
	// envelope and let the frontend render whatever keys are present.
	if err := msgBus.Subscribe(bus.TopicAlerts, "dashboard-backend-alerts", func(ctx context.Context, msg *bus.Message) error {
		var raw json.RawMessage
		if err := json.Unmarshal(msg.Value, &raw); err != nil {
			// Not JSON — fall back to the raw bytes as a string so we
			// still surface something rather than dropping the event.
			raw = json.RawMessage(msg.Value)
		}
		h.Broadcast("alerts", map[string]interface{}{
			"kind":    "alert",
			"payload": raw,
		})
		return nil
	}); err != nil {
		log.Printf("ws: subscribe alerts: %v", err)
	}

	// Forward indexing.progress → "indexing" WS channel so the dashboard can
	// show real-time indexing activity for every phase change.
	if err := msgBus.Subscribe(bus.TopicIndexingProgress, "dashboard-backend-indexing", func(ctx context.Context, msg *bus.Message) error {
		var raw json.RawMessage
		if err := json.Unmarshal(msg.Value, &raw); err != nil {
			return nil
		}
		h.Broadcast("indexing", raw)
		return nil
	}); err != nil {
		log.Printf("ws: subscribe indexing.progress: %v", err)
	}

	return nil
}

// PushPromptEvent broadcasts a prompt lifecycle event to the "prompts"
// WebSocket channel. Callers: the Kafka user_prompt.requested consumer
// (Task 9), and the REST answer/revoke handlers (could fan-out via events
// from the Kafka consumer instead — for v1, REST handlers publish to Kafka
// and the consumer does the WS push).
func (h *WSHub) PushPromptEvent(eventType string, payload map[string]any) {
	body := map[string]any{"type": eventType}
	for k, v := range payload {
		body[k] = v
	}
	b, err := json.Marshal(body)
	if err != nil {
		log.Printf("PushPromptEvent marshal: %v", err)
		return
	}
	h.Broadcast("prompts", json.RawMessage(b))
}
