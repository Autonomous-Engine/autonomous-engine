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

	"github.com/autonomousengine/autonomous-engine/internal/agentregistry"
	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/codex"
	"github.com/autonomousengine/autonomous-engine/internal/config"
	"github.com/autonomousengine/autonomous-engine/internal/configstore"
	"github.com/autonomousengine/autonomous-engine/internal/crypto"
	"github.com/autonomousengine/autonomous-engine/internal/db"
	"github.com/autonomousengine/autonomous-engine/internal/health"
	"github.com/autonomousengine/autonomous-engine/internal/llm"
	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
	"github.com/autonomousengine/autonomous-engine/internal/llm/tools"
	"github.com/autonomousengine/autonomous-engine/internal/outbox"
	"github.com/autonomousengine/autonomous-engine/internal/vector"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	agentID       = "chat-agent"
	consumerGroup = "chat-agent"
	healthPort    = 8099
	maxIterations = 15

	vectorCollection = "codebase"
)

// chatMessage is received from chat.message.
type chatMessage struct {
	SessionID   string          `json:"session_id"`
	WorkItemID  *string         `json:"work_item_id"`
	Message     string          `json:"user_message"`
	UserID      string          `json:"user_id"`
	PageContext json.RawMessage `json:"page_context"`
}

// chatResponse is published to chat.response.
type chatResponse struct {
	SessionID        string          `json:"session_id"`
	WorkItemID       *string         `json:"work_item_id"`
	MessageID        string          `json:"message_id"`
	Content          string          `json:"content"`
	ToolCallsSummary []toolCallChip  `json:"tool_calls_summary"`
	Proposal         json.RawMessage `json:"proposal,omitempty"`
	Timestamp        string          `json:"timestamp"`
}

// toolCallChip is a compact summary of a single tool call included in chatResponse.
type toolCallChip struct {
	Tool        string `json:"tool"`
	ArgsPreview string `json:"args_preview"`
}

const chatSystemPromptTemplate = `You are a helpful engineering assistant for the Autonomous Engineering System.
You have access to the codebase via read-only tools and the project's codex
(knowledge graph). You can investigate code, explain agent decisions, search
for patterns, create new work items, and propose changes to existing work items.

%s

PRODUCT CONTEXT:
- Codex tools (codex_find_module, codex_similar_code, etc.) only work when a product is selected.
- If no product is set and the user's request involves code or a specific app, call list_products to show available products, then ask the user which one they mean.
- NEVER assume or pick a product yourself. Always let the user choose.
- Once the user confirms, call set_product_context with the product_id.
- When creating work items, call list_products first so you can set the correct product_id. Ask the user which product if unclear.

When the user asks to change an in-flight work item:
1. Use get_work_item_detail to understand its current state
2. Confirm what you understand the change to be
3. State what will be affected (acceptance criteria, agent direction, etc.)
4. Use propose_work_item_change — the user must approve before it executes
5. Never make changes without explicit approval

CITING SOURCES (CRITICAL — the UI renders these as clickable links):
- ALWAYS reference code using the backtick file-path format: ` + "`" + `path/to/file.go:42` + "`" + `
- NEVER mention a function, method, handler, or class name without its file path. BAD: "the HandleSync function". GOOD: "the HandleSync function in ` + "`" + `cmd/dashboard-backend/handlers.go:150` + "`" + `".
- If you used a tool that returned results WITHOUT file paths (e.g. codex_find_module returns module names), you MUST call search_code or read_file to find the actual file path and line number BEFORE writing your response.
- The module name from codex_find_module is often a directory path (e.g. "cmd/dashboard-backend") — use search_code to find the specific file and line for each function you mention.
- Include line numbers whenever possible so the user can jump directly to the relevant code.

Be concise and direct. When you use tools to find information, summarize what
you found rather than dumping raw output. Ask follow-up questions when the
user's request is ambiguous.`

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("chat-agent: load config: %v", err)
	}

	pool, err := db.NewPool(ctx, cfg.Database.URL)
	if err != nil {
		log.Fatalf("chat-agent: connect db: %v", err)
	}
	defer pool.Close()

	kb, err := bus.NewKafkaBus(cfg.Kafka.Brokers)
	if err != nil {
		log.Fatalf("chat-agent: connect kafka: %v", err)
	}
	defer kb.Close() //nolint:errcheck

	wg := workgraph.New(pool)
	registry := agentregistry.New(pool)
	cx := codex.New(pool)

	var cs *configstore.ConfigStore
	masterKeyStr := os.Getenv("MASTER_ENCRYPTION_KEY")
	if masterKeyStr != "" {
		mk, mkErr := crypto.LoadMasterKey(masterKeyStr)
		if mkErr == nil {
			cs = configstore.New(pool, mk)
			// Listen on pg_notify('config_changed') so UI updates to credentials/
			// endpoints flush the in-memory cache without a restart.
			cs.StartWatcher(ctx)
		}
	}

	var vs *vector.VectorStore
	qdrantURL := os.Getenv("QDRANT_ENDPOINT")
	if qdrantURL == "" {
		qdrantURL = os.Getenv("QDRANT_URL")
	}
	if qdrantURL != "" {
		vs = vector.New(qdrantURL, vectorCollection)
	}

	checker := health.New(agentID)
	checker.AddCheck("database", func() error {
		return pool.Ping(ctx)
	})
	checker.AddCheck("kafka", kb.HealthCheck())

	// The create_work_item tool enqueues intake in the outbox together with
	// the work item (internal/llm/tools); this dispatcher delivers it. A
	// start failure is fatal. chat.response stays a direct publish: it is the
	// streamed reply, not a state change.
	dispatcher, err := outbox.StartForService(ctx, pool, kb, log.Default(), agentID)
	if err != nil {
		log.Fatalf("chat-agent: start outbox dispatcher: %v", err)
	}

	handler := buildChatHandler(ctx, pool, wg, registry, cs, kb, cx, vs)
	if err := kb.Subscribe(bus.TopicChatMessage, consumerGroup, handler); err != nil {
		log.Fatalf("chat-agent: subscribe: %v", err)
	}

	proposalHandler := buildProposalHandler(ctx, pool, wg, registry, cs, kb, cx, vs)
	if err := kb.Subscribe(bus.TopicChatProposalResolved, consumerGroup, proposalHandler); err != nil {
		log.Fatalf("chat-agent: subscribe proposal.resolved: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", checker.Handler())
	mux.HandleFunc("/metrics", checker.MetricsHandler())
	healthAddr := fmt.Sprintf(":%d", healthPort)
	healthSrv := &http.Server{Addr: healthAddr, Handler: mux}

	go func() {
		log.Printf("chat-agent: health server on %s", healthAddr)
		if err := healthSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("chat-agent: health server error: %v", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("chat-agent: shutting down...")
	cancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := healthSrv.Shutdown(shutdownCtx); err != nil {
		log.Printf("chat-agent: health server shutdown error: %v", err)
	}
	dispatcher.Wait()
	log.Println("chat-agent: stopped")
}

func buildChatHandler(
	ctx context.Context,
	pool *pgxpool.Pool,
	wg *workgraph.WorkGraph,
	registry *agentregistry.Registry,
	cs *configstore.ConfigStore,
	kb bus.MessageBus,
	cx *codex.Codex,
	vs *vector.VectorStore,
) bus.Handler {
	return func(msgCtx context.Context, msg *bus.Message) error {
		// M4-05: chat's tool-loop inference rows are tagged role=other.
		msgCtx = llm.WithInferenceTags(msgCtx, llm.InferenceTags{Role: llm.RoleOther, AgentType: "chat"})
		var payload chatMessage
		if err := json.Unmarshal(msg.Value, &payload); err != nil {
			log.Printf("chat-agent: unmarshal: %v", err)
			return nil
		}

		sessionID, err := uuid.Parse(payload.SessionID)
		if err != nil {
			log.Printf("chat-agent: parse session_id %q: %v", payload.SessionID, err)
			return nil
		}

		// Resolve work item context (nullable).
		var workItemID *uuid.UUID
		var contextBlock string
		if payload.WorkItemID != nil && *payload.WorkItemID != "" {
			parsed, parseErr := uuid.Parse(*payload.WorkItemID)
			if parseErr == nil {
				workItemID = &parsed
				workItem, wiErr := wg.GetWorkItem(msgCtx, parsed)
				if wiErr == nil {
					contextBlock = fmt.Sprintf(`Current context:
- Active work item: %s
- Title: %s
- State: %s
- User: %s`, workItem.ID, workItem.Title, string(workItem.State), payload.UserID)
				}
			}
		}

		if contextBlock == "" {
			// Build context from page_context.
			var pc struct {
				Page  string `json:"page"`
				Title string `json:"title"`
			}
			json.Unmarshal(payload.PageContext, &pc) //nolint:errcheck
			if pc.Page != "" {
				contextBlock = fmt.Sprintf("Current context:\n- Page: %s\n- User: %s", pc.Page, payload.UserID)
				if pc.Title != "" {
					contextBlock += fmt.Sprintf("\n- Page title: %s", pc.Title)
				}
			} else {
				contextBlock = fmt.Sprintf("Current context:\n- User: %s", payload.UserID)
			}
		}

		// Check if this is a resumed session.
		var sessionTitle *string
		pool.QueryRow(msgCtx,
			`SELECT title FROM chat_sessions WHERE id = $1`, sessionID).Scan(&sessionTitle)
		if sessionTitle != nil && *sessionTitle != "" {
			contextBlock += fmt.Sprintf("\n- Session: %s (resumed)", *sessionTitle)
		}
		contextBlock += fmt.Sprintf("\n- Session ID: %s", sessionID)

		// Resolve LLM adapter.
		// Priority: session-level provider override > work-item agent definition > global default.
		var llmAdapter adapter.ModelAdapter
		var sessionProvID *uuid.UUID
		pool.QueryRow(msgCtx,
			`SELECT llm_provider_id FROM chat_sessions WHERE id = $1`, sessionID).Scan(&sessionProvID)

		if sessionProvID != nil {
			// Session has an explicit provider override — build adapter from llm_providers row.
			var provType, endpoint string
			var modelID *string
			var credID *uuid.UUID
			if pErr := pool.QueryRow(msgCtx,
				`SELECT provider_type, endpoint, settings->>'model', api_key_credential_id
				 FROM llm_providers WHERE id = $1 AND enabled = true`,
				*sessionProvID).Scan(&provType, &endpoint, &modelID, &credID); pErr == nil {
				apiKey := ""
				if credID != nil && cs != nil {
					var encVal, nonce []byte
					if dErr := pool.QueryRow(msgCtx,
						`SELECT encrypted_value, nonce FROM credential_registry WHERE id = $1`,
						*credID).Scan(&encVal, &nonce); dErr == nil {
						apiKey, _ = crypto.Decrypt(cs.MasterKey(), *credID, encVal, nonce)
					}
				}
				model := ""
				if modelID != nil {
					model = *modelID
				}
				a, aErr := adapter.NewFromConfig(provType, endpoint, apiKey, model)
				if aErr != nil {
					log.Printf("chat-agent: session provider %s: %v", sessionProvID, aErr)
				} else {
					llmAdapter = a
					log.Printf("chat-agent: using session provider %s/%s for session=%s",
						provType, model, sessionID)
				}
			}
		}

		if llmAdapter == nil {
			resolveID := uuid.Nil
			if workItemID != nil {
				resolveID = *workItemID
			}
			a, _, aErr := registry.ResolveAdapterForWorkItem(msgCtx, resolveID, cs, "chat", agentID)
			if aErr != nil || a == nil {
				log.Printf("chat-agent: resolve adapter: %v", aErr)
				publishError(ctx, kb, sessionID, "Sorry, I couldn't connect to the language model. Please check provider configuration.")
				return nil
			}
			llmAdapter = a
		}

		// Resolve repo clone path.
		clonePath := "/repo-cache/default"
		if workItemID != nil {
			clonePath = resolveClonePath(msgCtx, pool, wg, *workItemID)
		}

		// Resolve product ID for codex scoping.
		// Priority: work item's product > session's saved product.
		var productID *uuid.UUID
		if workItemID != nil {
			item, wiErr := wg.GetWorkItem(msgCtx, *workItemID)
			if wiErr == nil && item.ProductID != nil {
				productID = item.ProductID
			}
		}
		if productID == nil {
			var sessionProdID *uuid.UUID
			pool.QueryRow(msgCtx,
				`SELECT product_id FROM chat_sessions WHERE id = $1`, sessionID).Scan(&sessionProdID)
			if sessionProdID != nil {
				productID = sessionProdID
			}
		}
		if productID != nil {
			var prodName string
			if err := pool.QueryRow(msgCtx, `SELECT name FROM products WHERE id = $1`, *productID).Scan(&prodName); err == nil {
				contextBlock += fmt.Sprintf("\n- Active product: %s (%s)", prodName, productID)
			}
		}

		executor := tools.NewExecutorWithCodex(clonePath, cx, vs, productID, kb, workItemID)
		executor.SetSessionID(sessionID)

		// Tell the executor what model/provider it's running so get_system_info works.
		if adapterName := llmAdapter.Name(); strings.Contains(adapterName, "/") {
			parts := strings.SplitN(adapterName, "/", 2)
			executor.SetModelInfo(parts[0], parts[1])
		} else {
			executor.SetModelInfo(adapterName, "")
		}

		// The executor runs read-only tool batches in parallel, so the
		// callback can be called concurrently; the append is serialised.
		// chips is read only after RunToolLoop returns.
		var chipsMu sync.Mutex
		var chips []toolCallChip
		executor.SetCallback(func(callID, toolName, arguments, result string, isError bool, _ tools.ResultMeta) {
			preview := arguments
			if len([]rune(preview)) > 120 {
				preview = string([]rune(preview)[:120]) + "…"
			}
			chipsMu.Lock()
			chips = append(chips, toolCallChip{
				Tool:        toolName,
				ArgsPreview: preview,
			})
			chipsMu.Unlock()
		})

		history, err := loadChatHistory(msgCtx, pool, sessionID)
		if err != nil {
			log.Printf("chat-agent: load history for session %s: %v", sessionID, err)
			history = nil
		}

		systemPrompt := fmt.Sprintf(chatSystemPromptTemplate, contextBlock) +
			fmt.Sprintf("\n\nYou are powered by: %s. Use get_system_info if you need to verify.", llmAdapter.Name())

		messages := []adapter.ChatMessage{
			{Role: adapter.RoleSystem, Content: systemPrompt},
		}
		messages = append(messages, history...)
		messages = append(messages, adapter.ChatMessage{
			Role:    adapter.RoleUser,
			Content: payload.Message,
		})

		req := adapter.CompletionRequest{
			Messages:    messages,
			Tools:       tools.ChatTools(),
			ToolChoice:  "auto",
			MaxTokens:   4096,
			Temperature: 0.3,
		}

		resp, err := tools.RunToolLoop(msgCtx, llmAdapter, req, executor, maxIterations, pool, workItemID)
		if err != nil {
			log.Printf("chat-agent: tool loop error for session=%s: %v", sessionID, err)
			publishError(ctx, kb, sessionID,
				fmt.Sprintf("Sorry, I encountered an error: %v", err))
			return nil
		}

		content := ""
		if resp != nil {
			content = resp.Content
		}

		// Check if the response contains a proposal.
		var proposalJSON json.RawMessage
		if strings.HasPrefix(content, "PROPOSAL_PENDING:") {
			// The propose_work_item_change tool returns the proposal as content.
			// Extract and store as structured proposal.
			proposalStr := strings.TrimPrefix(content, "PROPOSAL_PENDING:")
			proposalJSON = json.RawMessage(proposalStr)
			// Rewrite content to be human-readable.
			var prop struct {
				Action           string `json:"action"`
				TargetWorkItemID string `json:"target_work_item_id"`
				Changes          struct {
					Old       string `json:"old"`
					New       string `json:"new"`
					Rationale string `json:"rationale"`
				} `json:"changes"`
			}
			json.Unmarshal(proposalJSON, &prop) //nolint:errcheck
			content = fmt.Sprintf("I'd like to propose a change:\n\n**Action:** %s\n**Work Item:** %s\n**Change:** %s → %s\n**Rationale:** %s\n\nPlease approve or reject this proposal.",
				prop.Action, prop.TargetWorkItemID, prop.Changes.Old, prop.Changes.New, prop.Changes.Rationale)
		}

		// Persist the assistant response.
		toolCallsJSON, _ := json.Marshal(chips)
		msgInsertID := uuid.New()
		if _, dbErr := pool.Exec(msgCtx,
			`INSERT INTO chat_messages (id, session_id, work_item_id, role, content, tool_calls, proposal, created_at)
			 VALUES ($1, $2, $3, 'assistant', $4, $5, $6, NOW())`,
			msgInsertID, sessionID, workItemID, content, toolCallsJSON, proposalJSON,
		); dbErr != nil {
			log.Printf("chat-agent: insert assistant message: %v", dbErr)
		}

		// Update session timestamp.
		pool.Exec(msgCtx,
			`UPDATE chat_sessions SET updated_at = NOW() WHERE id = $1`, sessionID) //nolint:errcheck

		var wiStr *string
		if workItemID != nil {
			s := workItemID.String()
			wiStr = &s
		}

		response := chatResponse{
			SessionID:        sessionID.String(),
			WorkItemID:       wiStr,
			MessageID:        msgInsertID.String(),
			Content:          content,
			ToolCallsSummary: chips,
			Proposal:         proposalJSON,
			Timestamp:        time.Now().UTC().Format(time.RFC3339),
		}
		responsePayload, err := json.Marshal(response)
		if err != nil {
			log.Printf("chat-agent: marshal response: %v", err)
			return nil
		}
		if err := kb.Publish(ctx, bus.TopicChatResponse, sessionID.String(), responsePayload); err != nil {
			log.Printf("chat-agent: publish chat.response: %v", err)
		}

		return nil
	}
}

func buildProposalHandler(
	ctx context.Context,
	pool *pgxpool.Pool,
	wg *workgraph.WorkGraph,
	registry *agentregistry.Registry,
	cs *configstore.ConfigStore,
	kb bus.MessageBus,
	cx *codex.Codex,
	vs *vector.VectorStore,
) bus.Handler {
	return func(msgCtx context.Context, msg *bus.Message) error {
		msgCtx = llm.WithInferenceTags(msgCtx, llm.InferenceTags{Role: llm.RoleOther, AgentType: "chat"})
		var payload struct {
			SessionID  string          `json:"session_id"`
			MessageID  string          `json:"message_id"`
			Proposal   json.RawMessage `json:"proposal"`
			Resolution string          `json:"resolution"`
			ResolvedBy string          `json:"resolved_by"`
		}
		if err := json.Unmarshal(msg.Value, &payload); err != nil {
			log.Printf("chat-agent: unmarshal proposal resolution: %v", err)
			return nil
		}

		if payload.Resolution != "approved" {
			log.Printf("chat-agent: proposal %s was %s, no action needed", payload.MessageID, payload.Resolution)
			return nil
		}

		sessionID, _ := uuid.Parse(payload.SessionID)

		// Look up session to get work item context.
		var activeWI *uuid.UUID
		pool.QueryRow(msgCtx,
			`SELECT active_work_item_id FROM chat_sessions WHERE id = $1`, sessionID).Scan(&activeWI)

		// Resolve LLM adapter via work item or global default.
		resolveID := uuid.Nil
		if activeWI != nil {
			resolveID = *activeWI
		}
		llmAdapter, _, err := registry.ResolveAdapterForWorkItem(msgCtx, resolveID, cs, "chat", agentID)
		if err != nil || llmAdapter == nil {
			log.Printf("chat-agent: resolve adapter for proposal: %v", err)
			return nil
		}

		clonePath := "/repo-cache/default"
		if activeWI != nil {
			clonePath = resolveClonePath(msgCtx, pool, wg, *activeWI)
		}

		var productID *uuid.UUID
		if activeWI != nil {
			item, wiErr := wg.GetWorkItem(msgCtx, *activeWI)
			if wiErr == nil && item.ProductID != nil {
				productID = item.ProductID
			}
		}

		executor := tools.NewExecutorWithCodex(clonePath, cx, vs, productID, kb, activeWI)

		// Execute the approved proposal directly via the tool executor.
		result := executor.Execute(msgCtx, adapter.ToolCall{
			ID:        uuid.New().String(),
			Name:      "execute_approved_proposal",
			Arguments: fmt.Sprintf(`{"message_id":"%s"}`, payload.MessageID),
		})

		// Track the work item link.
		var prop struct {
			TargetWorkItemID string `json:"target_work_item_id"`
		}
		json.Unmarshal(payload.Proposal, &prop) //nolint:errcheck
		if prop.TargetWorkItemID != "" {
			pool.Exec(msgCtx,
				`INSERT INTO chat_session_work_items (session_id, work_item_id, relationship)
				 VALUES ($1, $2, 'modified')
				 ON CONFLICT (session_id, work_item_id) DO UPDATE SET relationship = 'modified'`,
				sessionID, prop.TargetWorkItemID) //nolint:errcheck
		}

		// Publish confirmation to chat.response.
		confirmContent := result.Content
		if result.IsError {
			confirmContent = fmt.Sprintf("Failed to execute proposal: %s", result.Content)
		}

		msgInsertID := uuid.New()
		pool.Exec(msgCtx,
			`INSERT INTO chat_messages (id, session_id, role, content, created_at)
			 VALUES ($1, $2, 'assistant', $3, NOW())`,
			msgInsertID, sessionID, confirmContent) //nolint:errcheck

		var wiStr *string
		if activeWI != nil {
			s := activeWI.String()
			wiStr = &s
		}

		response := chatResponse{
			SessionID:  sessionID.String(),
			WorkItemID: wiStr,
			MessageID:  msgInsertID.String(),
			Content:    confirmContent,
			Timestamp:  time.Now().UTC().Format(time.RFC3339),
		}
		respPayload, _ := json.Marshal(response)
		kb.Publish(ctx, bus.TopicChatResponse, sessionID.String(), respPayload) //nolint:errcheck

		return nil
	}
}

// loadChatHistory retrieves the last 50 messages for a session, ordered oldest-first.
func loadChatHistory(ctx context.Context, pool *pgxpool.Pool, sessionID uuid.UUID) ([]adapter.ChatMessage, error) {
	rows, err := pool.Query(ctx,
		`SELECT role, content FROM chat_messages WHERE session_id = $1 ORDER BY created_at ASC LIMIT 50`,
		sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var messages []adapter.ChatMessage
	for rows.Next() {
		var role, content string
		if err := rows.Scan(&role, &content); err != nil {
			return nil, err
		}
		r := adapter.RoleUser
		if role == "assistant" {
			r = adapter.RoleAssistant
		}
		messages = append(messages, adapter.ChatMessage{Role: r, Content: content})
	}
	return messages, rows.Err()
}

// resolveClonePath looks up the clone_path for a work item's repository.
// Falls back to /repo-cache/default if not found.
func resolveClonePath(ctx context.Context, pool *pgxpool.Pool, wg *workgraph.WorkGraph, workItemID uuid.UUID) string {
	item, err := wg.GetWorkItem(ctx, workItemID)
	if err != nil || item.RepositoryID == nil {
		return "/repo-cache/default"
	}
	var clonePath string
	err = pool.QueryRow(ctx,
		`SELECT clone_path FROM repositories WHERE id = $1 AND clone_path IS NOT NULL`,
		*item.RepositoryID).Scan(&clonePath)
	if err != nil || clonePath == "" {
		return "/repo-cache/default"
	}
	return clonePath
}

// publishError sends an error message to the chat.response topic so the user
// receives feedback rather than a silent failure.
func publishError(ctx context.Context, kb bus.MessageBus, sessionID uuid.UUID, errMsg string) {
	response := chatResponse{
		SessionID: sessionID.String(),
		MessageID: uuid.New().String(),
		Content:   errMsg,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}
	payload, _ := json.Marshal(response)
	kb.Publish(ctx, bus.TopicChatResponse, sessionID.String(), payload) //nolint:errcheck
}
