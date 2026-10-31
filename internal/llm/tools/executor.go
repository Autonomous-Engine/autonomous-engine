package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/codex"
	"github.com/autonomousengine/autonomous-engine/internal/interactions"
	"github.com/autonomousengine/autonomous-engine/internal/llm"
	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
	"github.com/autonomousengine/autonomous-engine/internal/llm/tools/lsp"
	"github.com/autonomousengine/autonomous-engine/internal/llm/tools/verify"
	"github.com/autonomousengine/autonomous-engine/internal/metrics"
	"github.com/autonomousengine/autonomous-engine/internal/outbox"
	"github.com/autonomousengine/autonomous-engine/internal/vector"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
)

// ToolCallback is called after each tool execution with its inputs and result.
// It is used to stream tool activity to external observers (e.g. the dashboard).
// meta carries the result's structured fields (run_command's exit code,
// duration and command; zero for every other tool).
type ToolCallback func(callID, toolName, arguments, result string, isError bool, meta ResultMeta)

// ResultMeta is the structured part of a tool result, for observers.
type ResultMeta struct {
	// ExitCode is run_command's exit status; nil when the command did not
	// run to one (rejected, failed to start, killed by its timeout) and
	// for every other tool.
	ExitCode   *int
	DurationMs int64
	Command    string
}

// MetaOf extracts the structured fields of r.
func MetaOf(r adapter.ToolResult) ResultMeta {
	return ResultMeta{ExitCode: r.ExitCode, DurationMs: r.DurationMs, Command: r.Command}
}

// resumptionPersister is the narrow contract request_user_input needs.
// *interactions.Store satisfies this.
type resumptionPersister interface {
	PersistResumption(ctx context.Context, rs *interactions.ResumptionState) error
}

// Executor runs tool calls against a real repository on disk.
//
// When a Codex/VectorStore/product_id are supplied via NewExecutorWithCodex
// the executor also serves the codex_* tools (codex_find_module,
// codex_dependents_of, codex_dependencies_of, codex_similar_code,
// codex_patterns_for, codex_read_file, codex_grep, codex_list_tree,
// codex_verify_imports, codex_contract_edges) and the six LSP-backed tools (codex_find_references,
// codex_goto_definition, codex_workspace_symbols, codex_document_symbols,
// codex_type_info, codex_diagnostics). When any required dependency is
// nil, calling a codex tool returns a descriptive error so the LLM can
// fall back to grep.
//
// codex_read_file and codex_grep resolve filesystem paths from the
// repositories table: they look up enabled repos for the executor's
// productID and read from the repo-cache mount. Services that expose
// these tools must mount the repo-cache volume (read-only is enough).
type Executor struct {
	repoPath string       // absolute path to the repo clone on disk
	onTool   ToolCallback // optional callback invoked after each tool execution
	// annotate, when set, may append a note to a tool result the model
	// sees (SetResultAnnotator).
	annotate ResultAnnotator
	// iterationHook, when set, runs before every LLM call of RunToolLoop
	// (SetIterationHook, M3-06 budgets).
	iterationHook IterationHook
	// promptHook, when set, is told about every user prompt this executor
	// publishes (SetPromptHook, M3-06 active-time clock).
	promptHook PromptHook

	// Codex dependencies. All optional — when nil, codex_* tools return
	// an error and the existing filesystem tools continue to work as before.
	codex     *codex.Codex
	vectors   *vector.VectorStore
	productID *uuid.UUID

	// Kafka gap-tracking. When kb+workItemID are non-nil and an agent
	// calls search_code / list_files AFTER a codex_* tool returned zero
	// results in the same loop, we publish a codex.gap event so a future
	// re-indexing pass can patch the hole.
	kb         bus.MessageBus
	workItemID *uuid.UUID

	// askingAgent is the service name used as the asking_agent value when
	// request_user_input publishes user_prompt.requested. Callers set this
	// via SetAskingAgent. Empty means request_user_input returns an error.
	askingAgent string

	// qdrantEndpoint + embeddingSvc power past_decisions_query, which builds
	// a per-product *vector.VectorStore bound to "product_<id>_user_decisions"
	// at call time. When either is nil the tool returns an error — set via
	// SetQdrantConfig at agent startup.
	qdrantEndpoint string
	embeddingSvc   *vector.EmbeddingService

	// gapState records the most recent "codex returned empty" per (tool,query).
	// We guard it with a mutex because the stuck-detection logic in
	// RunToolLoop may invoke tools sequentially on a single goroutine but
	// callbacks run asynchronously in some agents.
	gapMu         sync.Mutex
	lastEmptyCall *gapPending

	// plannedFiles holds the set of files the caller (typically the
	// implementation-planner-via-implementation-agent) declared in scope
	// for this task. When write_file/edit_file touches a path OUTSIDE
	// this set and it's non-nil, we emit a codex.gap with reason
	// "planner_missed_file" — the planner's structured brief should
	// have listed that file. Nil disables the check.
	plannedFiles map[string]struct{}
	// plannerGapEmitted tracks which out-of-scope paths we've already
	// reported this loop so we emit at most one event per file per task.
	plannerGapEmitted map[string]struct{}

	// lspManager spawns + caches one LSP client per (repo,language). When
	// nil, the six codex LSP tools (codex_find_references,
	// codex_goto_definition, codex_workspace_symbols, codex_document_symbols,
	// codex_type_info, codex_diagnostics) return a descriptive error so the
	// LLM can fall back to grep/codex_similar_code.
	lspManager *lsp.Manager
	// lspRoot* cache the workspace root LSPReady / WarmLSP resolve once.
	// codexWorktreeReads: SetCodexReadsFromWorktree.
	codexWorktreeReads bool
	worktreeRepo       string // SetWorktreeRepo
	lspRootMu          sync.Mutex
	lspRootResolved    bool
	lspRootPath        string
	// readSeeds: per path, the line a default read centres on (R10 W5).
	readSeeds map[string]int
	// lspRootResolver replaces repoCacheRoot in tests.
	lspRootResolver func(ctx context.Context) (string, error)

	// toolOverride, when set, may answer a call instead of its handler
	// (SetToolOverride; offline harnesses only).
	toolOverride ToolOverride

	// observedPaths tracks paths whose on-disk state the model has been
	// told about in this session: either via a successful read_file /
	// codex_read_file (saw real content), or via a failed one that
	// returned "no such file" (saw the file doesn't exist). Writes and
	// edits are gated on this set — the model cannot edit_file or
	// write_file on a path it hasn't observed. This is the enforcement
	// half of the prompt's "read-before-edit" rule; the prompt alone is
	// not enough to stop the model from creating orphan files at plausible-
	// but-wrong paths (the "app/api/contact.ts vs app/api/contact/route.ts"
	// failure the audit surfaced).
	observedMu    sync.Mutex
	observedPaths map[string]struct{}
	// readCov: which lines of each existing file were read (write_file
	// on a file read only in part is refused).
	readCov map[string]*readCoverage

	// Branch scoping for commit-linked codex. When set, codex queries resolve
	// to a materialized snapshot for the given branch+commit instead of the
	// live codex tables. Set via SetWorkItemBranch.
	currentBranch  string
	currentHeadSHA string

	// resumption is an optional persister for agent tool-loop snapshots
	// captured when request_user_input yields. When nil, request_user_input
	// succeeds without persisting — the user's answer still round-trips via
	// codex / past_decisions_query, but auto-resume of the paused LLM
	// conversation is skipped. Set via SetResumption.
	resumption resumptionPersister

	// resumptionSnapshot holds the message history captured by the caller
	// (typically RunToolLoop right before invoking the tool) so the
	// Executor can persist it when request_user_input fires. Guarded by a
	// mutex because runToolCallBatches may exec tools concurrently (reads
	// only; no mutation here, but read-while-write on Executor fields
	// deserves the lock for clarity).
	resumptionSnapshot []byte

	// modelInfo holds the current model/provider identity so the
	// get_system_info tool can report it. Set via SetModelInfo.
	modelProvider string
	modelName     string

	// diffBase is the ref git_diff uses when the LLM calls it with no
	// explicit ref. Typically "origin/main" — set by the review agent so
	// the diff shows only the PR's changes, not the entire working tree.
	diffBase string

	// diffHead overrides HEAD in the three-dot diff. When the review agent
	// runs against the main clone (not a worktree), HEAD points at main,
	// not the PR branch. Set this to "origin/<branch>" so the diff shows
	// the correct range.
	diffHead string

	// commandGuard, when set, can reject a run_command (SetCommandGuard).
	commandGuard func(command string) string

	// db is set by RunToolLoop so recall_context and maybeCheckpoint can
	// read/write conversation_snapshots.
	db *pgxpool.Pool

	// sessionID is set by the chat agent so set_product_context can persist
	// the chosen product on the chat_sessions row.
	sessionID *uuid.UUID
}

// gapPending is set by a codex_* tool that returned zero results. If the
// next filesystem tool call finds something, we emit a codex.gap event.
type gapPending struct {
	codexTool string
	query     string
}

// NewExecutor creates a new Executor rooted at repoPath. Codex tools are
// disabled — use NewExecutorWithCodex to enable them.
func NewExecutor(repoPath string) *Executor {
	return &Executor{repoPath: repoPath}
}

// NewExecutorWithCodex creates an Executor that also serves the codex_*
// tools. Any of cx / vs / productID / kb / workItemID may be nil; the
// executor degrades gracefully (the corresponding tool returns an error
// the LLM can see and react to).
func NewExecutorWithCodex(
	repoPath string,
	cx *codex.Codex,
	vs *vector.VectorStore,
	productID *uuid.UUID,
	kb bus.MessageBus,
	workItemID *uuid.UUID,
) *Executor {
	return &Executor{
		repoPath:   repoPath,
		codex:      cx,
		vectors:    vs,
		productID:  productID,
		kb:         kb,
		workItemID: workItemID,
		lspManager: lsp.NewManager(),
	}
}

// LSPManager returns the LSP manager so callers (agent shutdown hooks) can
// close it on their way out. Nil for executors built without codex wiring.
func (e *Executor) LSPManager() *lsp.Manager {
	return e.lspManager
}

// SetCallback registers a callback that is invoked after every tool execution.
// Pass nil to clear a previously registered callback.
func (e *Executor) SetCallback(cb ToolCallback) {
	e.onTool = cb
}

// ResultAnnotator returns text to append to a tool result the model sees,
// or "". It must be safe for concurrent use: read-only tool batches run in
// parallel.
type ResultAnnotator func(toolName, arguments string, result adapter.ToolResult) string

// SetResultAnnotator registers an annotator (the implementation agent's
// impact-selected fast-check note, M3-04). The SetCallback observer still
// sees the unannotated result.
func (e *Executor) SetResultAnnotator(a ResultAnnotator) {
	e.annotate = a
}

// IterationHook runs before each LLM call of RunToolLoop with the work
// item's cumulative tokens (input + output, all agents, as the loop's own
// token budget counts them) and that budget. A non-nil error stops the
// loop; RunToolLoop returns it wrapped (errors.Is/As see through). The
// implementation agent checks its D4 budgets here (M3-06).
type IterationHook func(ctx context.Context, cumulativeTokens, tokenBudget int64) error

// SetIterationHook registers the iteration hook (nil clears it).
func (e *Executor) SetIterationHook(h IterationHook) { e.iterationHook = h }

// PromptHook is told about every user prompt the executor publishes
// (request_user_input and the stuck / diminishing-returns heuristics): a
// prompt is pending from then until it is answered. The implementation
// agent pauses its active-time clock on it (D4, M3-06).
type PromptHook func(promptID uuid.UUID, trigger interactions.Trigger)

// SetPromptHook registers the prompt hook (nil clears it).
func (e *Executor) SetPromptHook(h PromptHook) { e.promptHook = h }

func (e *Executor) notifyPrompt(id uuid.UUID, trigger interactions.Trigger) {
	if e.promptHook != nil {
		e.promptHook(id, trigger)
	}
}

// ErrTokenBudgetExceeded is returned (wrapped) by RunToolLoop when the work
// item's cumulative tokens exceed TokenBudget().
var ErrTokenBudgetExceeded = errors.New("token budget exceeded")

// TokenBudget is the per-work-item token ceiling RunToolLoop enforces:
// WORK_ITEM_TOKEN_BUDGET, default 500,000 (D4).
func TokenBudget() int64 {
	var tokenBudget int64 = 500_000
	if s := os.Getenv("WORK_ITEM_TOKEN_BUDGET"); s != "" {
		if v, err := strconv.ParseInt(s, 10, 64); err == nil && v > 0 {
			tokenBudget = v
		}
	}
	return tokenBudget
}

// SetAskingAgent records the agent name used when publishing user prompts
// via request_user_input. Pass the service's name (e.g., "implementation-agent").
func (e *Executor) SetAskingAgent(name string) {
	e.askingAgent = name
}

// SetResumption installs the persister used by request_user_input to save
// the current message-history snapshot on yield.
func (e *Executor) SetResumption(p resumptionPersister) {
	e.resumption = p
}

// SetResumptionSnapshot records a message-history snapshot that
// request_user_input should persist if the LLM calls it. RunToolLoop
// threads this in before each iteration.
func (e *Executor) SetResumptionSnapshot(history []byte) {
	e.resumptionSnapshot = history
}

// SetWorkItemBranch records the branch and HEAD commit SHA for the current
// work item. When set, materializeCodex can resolve codex queries to a
// snapshot for the given branch+commit instead of the live codex tables.
func (e *Executor) SetWorkItemBranch(branch, headSHA string) {
	e.currentBranch = branch
	e.currentHeadSHA = headSHA
}

// SetDiffBase sets the default ref for git_diff when the LLM omits the
// ref argument. Typically "origin/main" so the review agent sees only
// the PR's changes instead of the full working tree.
func (e *Executor) SetDiffBase(ref string) { e.diffBase = ref }

// SetModelInfo stores the current provider and model name so the
// get_system_info tool can report the LLM identity to the model itself.
func (e *Executor) SetModelInfo(provider, model string) {
	e.modelProvider = provider
	e.modelName = model
}

func (e *Executor) SetSessionID(id uuid.UUID) { e.sessionID = &id }

// SetDiffHead overrides HEAD in the three-dot diff range. Use this when
// the executor runs in the main clone rather than a checked-out worktree
// so git_diff diffs the actual PR branch, not the clone's current HEAD.
func (e *Executor) SetDiffHead(ref string) { e.diffHead = ref }

// SetCommandGuard adds a caller-specific check run before every
// run_command, after the built-in denylist: a non-empty return rejects the
// command with that reason (M3-07: the verifier refuses git commands that
// write refs, config or remotes).
func (e *Executor) SetCommandGuard(g func(command string) string) { e.commandGuard = g }

// publishHeuristicPrompt fires a user_prompt.requested when a heuristic
// (stuck / diminishing-returns / retrieval-contradiction) trips. Returns
// true iff the publish succeeded AND the Executor has all the context it
// needs (kb + workItemID + productID + askingAgent). When it returns true,
// the caller bails out of the tool loop with FinishReason="awaiting_user";
// when false, the caller falls through to its original error-or-exit path.
//
// This is how heuristics get a user-redirect option rather than silently
// exiting with stall output or returning a hard stuck error.
func (e *Executor) publishHeuristicPrompt(ctx context.Context, trigger interactions.Trigger, headline, question string, contextPayload map[string]any) bool {
	if e.kb == nil || e.workItemID == nil || e.productID == nil || e.askingAgent == "" {
		return false
	}
	prompt := &interactions.Prompt{
		ID:             uuid.New(),
		WorkItemID:     *e.workItemID,
		ProductID:      *e.productID,
		AskingAgent:    e.askingAgent,
		Trigger:        trigger,
		PromptType:     interactions.PromptTypeEscalation,
		Headline:       headline,
		Question:       question,
		ContextPayload: contextPayload,
		Actions: []interactions.Action{
			{ID: "retry", Label: "Retry with different plan", Semantic: interactions.SemanticRestart},
			{ID: "continue", Label: "Keep trying (one more)", Semantic: interactions.SemanticContinue},
			{ID: "cancel", Label: "Cancel this work item", Semantic: interactions.SemanticCancel},
		},
		State:       interactions.StatePending,
		RequestedAt: time.Now().UTC(),
		PromptedVia: []string{"dashboard"},
	}
	pub := interactions.NewPublisher(e.kb)
	if err := pub.PublishRequested(ctx, prompt); err != nil {
		log.Printf("publishHeuristicPrompt (%s): publish failed: %v", trigger, err)
		return false
	}
	if e.resumption != nil {
		history := e.resumptionSnapshot
		if history == nil {
			history = []byte("[]")
		}
		if err := e.resumption.PersistResumption(ctx, &interactions.ResumptionState{
			PromptID:       prompt.ID,
			WorkItemID:     *e.workItemID,
			AskingAgent:    e.askingAgent,
			MessageHistory: history,
			ToolCallStack:  []byte("[]"),
		}); err != nil {
			log.Printf("publishHeuristicPrompt (%s): persist resumption failed: %v", trigger, err)
		}
	}
	e.notifyPrompt(prompt.ID, trigger)
	return true
}

// SetQdrantConfig records qdrant connection info used by past_decisions_query.
func (e *Executor) SetQdrantConfig(endpoint string, svc *vector.EmbeddingService) {
	e.qdrantEndpoint = endpoint
	e.embeddingSvc = svc
}

// SetPlannedFiles records the set of files the implementation-planner
// assigned to the current subtask. When write_file or edit_file later
// touches a path NOT in this set, maybeEmitPlannerGap emits a
// codex.gap event with reason="planner_missed_file" so follow-up passes
// can see which files the structured brief should have flagged. Passing
// an empty / nil slice disables the check — that's the right call for
// legacy briefs or when the planner provided no file scope at all (we
// can't claim the planner missed a file if it never specified a set).
func (e *Executor) SetPlannedFiles(files []string) {
	e.gapMu.Lock()
	defer e.gapMu.Unlock()
	if len(files) == 0 {
		e.plannedFiles = nil
		return
	}
	e.plannedFiles = make(map[string]struct{}, len(files))
	for _, f := range files {
		e.plannedFiles[f] = struct{}{}
	}
}

// Execute dispatches a single tool call and returns its result.
func (e *Executor) Execute(ctx context.Context, call adapter.ToolCall) adapter.ToolResult {
	// Increment the global tool-call counter so Prometheus can track throughput.
	metrics.ToolCallsExecuted.Inc()

	var args map[string]interface{}
	if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil {
		result := adapter.ToolResult{
			ToolCallID: call.ID,
			Content:    "Invalid arguments: " + err.Error(),
			IsError:    true,
		}
		if e.onTool != nil {
			e.onTool(call.ID, call.Name, call.Arguments, result.Content, result.IsError, MetaOf(result))
		}
		return result
	}

	if e.toolOverride != nil {
		if r, ok := e.toolOverride(ctx, call); ok {
			r.ToolCallID = call.ID
			if e.onTool != nil {
				e.onTool(call.ID, call.Name, call.Arguments, r.Content, r.IsError, MetaOf(r))
			}
			return r
		}
	}

	var result adapter.ToolResult
	switch call.Name {
	case "read_file":
		result = e.readFile(call.ID, args)
	case "write_file":
		result = e.writeFile(call.ID, args)
	case "edit_file":
		result = e.editFile(call.ID, args)
	case "insert_after":
		result = e.insertAfter(call.ID, args)
	case "append_to_file":
		result = e.appendToFile(call.ID, args)
	case "search_code":
		result = e.searchCode(ctx, call.ID, args)
	case "list_files":
		result = e.listFiles(call.ID, args)
	case "run_command":
		result = e.runCommand(ctx, call.ID, args)
	case "git_log":
		result = e.gitLog(ctx, call.ID, args)
	case "git_diff":
		result = e.gitDiff(ctx, call.ID, args)
	case "create_branch":
		result = e.createBranch(ctx, call.ID, args)
	case "commit_and_push":
		result = e.commitAndPush(ctx, call.ID, args)
	case "codex_find_module":
		result = e.codexFindModule(ctx, call.ID, args)
	case "codex_dependents_of":
		result = e.codexDependentsOf(ctx, call.ID, args)
	case "codex_dependencies_of":
		result = e.codexDependenciesOf(ctx, call.ID, args)
	case "codex_similar_code":
		result = e.codexSimilarCode(ctx, call.ID, args)
	case "codex_patterns_for":
		result = e.codexPatternsFor(ctx, call.ID, args)
	case "codex_read_file":
		result = e.codexReadFile(ctx, call.ID, args)
	case "codex_grep":
		result = e.codexGrep(ctx, call.ID, args)
	case "codex_find_references":
		result = e.codexFindReferences(ctx, call.ID, args)
	case "codex_goto_definition":
		result = e.codexGotoDefinition(ctx, call.ID, args)
	case "codex_workspace_symbols":
		result = e.codexWorkspaceSymbols(ctx, call.ID, args)
	case "codex_document_symbols":
		result = e.codexDocumentSymbols(ctx, call.ID, args)
	case "codex_type_info":
		result = e.codexTypeInfo(ctx, call.ID, args)
	case "codex_diagnostics":
		result = e.codexDiagnostics(ctx, call.ID, args)
	case "codex_list_tree":
		result = e.codexListTree(ctx, call.ID, args)
	case "codex_verify_imports":
		result = e.codexVerifyImports(ctx, call.ID, args)
	case "codex_contract_edges":
		result = e.codexContractEdges(ctx, call.ID, args)
	case "request_user_input":
		result = e.requestUserInput(ctx, call.ID, args)
	case "past_decisions_query":
		result = e.pastDecisionsQuery(ctx, call.ID, args)
	case "recall_context":
		result = e.recallContext(ctx, call.ID, args)
	case "set_product_context":
		if e.db == nil {
			return adapter.ToolResult{ToolCallID: call.ID, Content: "database not available", IsError: true}
		}
		pidStr, _ := args["product_id"].(string)
		pid, parseErr := uuid.Parse(pidStr)
		if parseErr != nil {
			return adapter.ToolResult{ToolCallID: call.ID, Content: "invalid product_id: " + parseErr.Error(), IsError: true}
		}
		var prodName string
		if err := e.db.QueryRow(ctx, `SELECT name FROM products WHERE id = $1`, pid).Scan(&prodName); err != nil {
			return adapter.ToolResult{ToolCallID: call.ID, Content: "product not found", IsError: true}
		}
		e.productID = &pid
		if e.sessionID != nil {
			e.db.Exec(ctx, `UPDATE chat_sessions SET product_id = $1, updated_at = NOW() WHERE id = $2`, pid, *e.sessionID) //nolint:errcheck
		}
		return adapter.ToolResult{ToolCallID: call.ID, Content: fmt.Sprintf("Product context set to %q (%s). Codex tools are now scoped to this product.", prodName, pid)}

	case "list_products":
		if e.db == nil {
			return adapter.ToolResult{ToolCallID: call.ID, Content: "database not available", IsError: true}
		}
		rows, dbErr := e.db.Query(ctx, `SELECT id, name, description FROM products ORDER BY name`)
		if dbErr != nil {
			return adapter.ToolResult{ToolCallID: call.ID, Content: fmt.Sprintf("query failed: %v", dbErr), IsError: true}
		}
		defer rows.Close()
		var products []map[string]string
		for rows.Next() {
			var id uuid.UUID
			var name, desc string
			if err := rows.Scan(&id, &name, &desc); err != nil {
				continue
			}
			products = append(products, map[string]string{"id": id.String(), "name": name, "description": desc})
		}
		if len(products) == 0 {
			return adapter.ToolResult{ToolCallID: call.ID, Content: "No products configured. Ask the user to create a product in Admin → Products first."}
		}
		productsJSON, _ := json.Marshal(products)
		return adapter.ToolResult{ToolCallID: call.ID, Content: string(productsJSON)}

	case "create_work_item":
		var args struct {
			Title       string `json:"title"`
			Description string `json:"description"`
			ProductID   string `json:"product_id"`
			Type        string `json:"type"`
			Priority    string `json:"priority"`
		}
		if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil {
			return adapter.ToolResult{ToolCallID: call.ID, Content: fmt.Sprintf("invalid arguments: %v", err), IsError: true}
		}
		if args.Title == "" || args.Description == "" {
			return adapter.ToolResult{ToolCallID: call.ID, Content: "title and description are required", IsError: true}
		}
		if args.Type == "" {
			args.Type = "feature"
		}
		if args.Priority == "" {
			args.Priority = "medium"
		}
		if e.db == nil {
			return adapter.ToolResult{ToolCallID: call.ID, Content: "database not available for work item creation", IsError: true}
		}
		var productID *uuid.UUID
		if args.ProductID != "" {
			pid, parseErr := uuid.Parse(args.ProductID)
			if parseErr != nil {
				return adapter.ToolResult{ToolCallID: call.ID, Content: "invalid product_id: " + parseErr.Error(), IsError: true}
			}
			productID = &pid
		}
		newID := uuid.New()
		// The row and its intake event commit together (outbox, M1-04e):
		// before, a failed publish was ignored and the item sat in intake
		// with nothing on the bus to pick it up.
		if dbErr := createWorkItemWithIntake(ctx, e.db, newID, args.Title, args.Description, args.Type, args.Priority, productID); dbErr != nil {
			return adapter.ToolResult{ToolCallID: call.ID, Content: fmt.Sprintf("failed to create work item: %v", dbErr), IsError: true}
		}
		result := fmt.Sprintf("Created work item %s: %s (type=%s, priority=%s, state=intake)", newID, args.Title, args.Type, args.Priority)
		if productID != nil {
			result += fmt.Sprintf(", linked to product %s", productID)
		}
		return adapter.ToolResult{ToolCallID: call.ID, Content: result}
	case "get_system_info":
		info := map[string]string{
			"provider": e.modelProvider,
			"model":    e.modelName,
			"agent":    e.askingAgent,
		}
		infoJSON, _ := json.Marshal(info)
		result = adapter.ToolResult{ToolCallID: call.ID, Content: string(infoJSON)}
	case "create_pull_request":
		result = adapter.ToolResult{
			ToolCallID: call.ID,
			Content:    "PR creation requires GitHub API — use the github-service HTTP endpoint",
			IsError:    true,
		}

	case "propose_work_item_change":
		var propArgs struct {
			WorkItemID string          `json:"work_item_id"`
			ChangeType string          `json:"change_type"`
			Changes    json.RawMessage `json:"changes"`
		}
		if err := json.Unmarshal([]byte(call.Arguments), &propArgs); err != nil {
			return adapter.ToolResult{ToolCallID: call.ID, Content: fmt.Sprintf("invalid arguments: %v", err), IsError: true}
		}
		if propArgs.WorkItemID == "" || propArgs.ChangeType == "" {
			return adapter.ToolResult{ToolCallID: call.ID, Content: "work_item_id and change_type are required", IsError: true}
		}
		proposal := map[string]interface{}{
			"action":              propArgs.ChangeType,
			"target_work_item_id": propArgs.WorkItemID,
			"changes":             json.RawMessage(propArgs.Changes),
			"status":              "pending",
		}
		proposalJSON, _ := json.Marshal(proposal)
		return adapter.ToolResult{
			ToolCallID: call.ID,
			Content:    fmt.Sprintf("PROPOSAL_PENDING:%s", string(proposalJSON)),
		}

	case "execute_approved_proposal":
		var eapArgs struct {
			MessageID string `json:"message_id"`
		}
		if err := json.Unmarshal([]byte(call.Arguments), &eapArgs); err != nil {
			return adapter.ToolResult{ToolCallID: call.ID, Content: fmt.Sprintf("invalid arguments: %v", err), IsError: true}
		}
		if eapArgs.MessageID == "" {
			return adapter.ToolResult{ToolCallID: call.ID, Content: "message_id is required", IsError: true}
		}
		msgID, parseErr := uuid.Parse(eapArgs.MessageID)
		if parseErr != nil {
			return adapter.ToolResult{ToolCallID: call.ID, Content: fmt.Sprintf("invalid message_id: %v", parseErr), IsError: true}
		}
		if e.db == nil {
			return adapter.ToolResult{ToolCallID: call.ID, Content: "database not available", IsError: true}
		}
		var proposalRaw []byte
		err := e.db.QueryRow(ctx,
			`SELECT proposal FROM chat_messages WHERE id = $1 AND proposal IS NOT NULL`,
			msgID).Scan(&proposalRaw)
		if err != nil {
			return adapter.ToolResult{ToolCallID: call.ID, Content: fmt.Sprintf("proposal not found: %v", err), IsError: true}
		}
		var execProposal struct {
			Action           string          `json:"action"`
			TargetWorkItemID string          `json:"target_work_item_id"`
			Changes          json.RawMessage `json:"changes"`
			Status           string          `json:"status"`
		}
		if err := json.Unmarshal(proposalRaw, &execProposal); err != nil {
			return adapter.ToolResult{ToolCallID: call.ID, Content: fmt.Sprintf("invalid proposal data: %v", err), IsError: true}
		}
		if execProposal.Status != "approved" {
			return adapter.ToolResult{ToolCallID: call.ID, Content: fmt.Sprintf("proposal status is %q, not approved", execProposal.Status), IsError: true}
		}
		workItemID, _ := uuid.Parse(execProposal.TargetWorkItemID)
		var changes struct {
			Old       string `json:"old"`
			New       string `json:"new"`
			Rationale string `json:"rationale"`
		}
		json.Unmarshal(execProposal.Changes, &changes) //nolint:errcheck
		switch execProposal.Action {
		case "update_criteria", "update_description":
			_, dbErr := e.db.Exec(ctx,
				`UPDATE work_items SET description = $1, updated_at = NOW() WHERE id = $2`,
				changes.New, workItemID)
			if dbErr != nil {
				return adapter.ToolResult{ToolCallID: call.ID, Content: fmt.Sprintf("failed to update work item: %v", dbErr), IsError: true}
			}
		case "change_priority":
			_, dbErr := e.db.Exec(ctx,
				`UPDATE work_items SET priority = $1::priority_level, updated_at = NOW() WHERE id = $2`,
				changes.New, workItemID)
			if dbErr != nil {
				return adapter.ToolResult{ToolCallID: call.ID, Content: fmt.Sprintf("failed to update priority: %v", dbErr), IsError: true}
			}
		case "add_context":
			_, dbErr := e.db.Exec(ctx,
				`UPDATE work_items SET description = description || E'\n\n--- Additional Context ---\n' || $1, updated_at = NOW() WHERE id = $2`,
				changes.New, workItemID)
			if dbErr != nil {
				return adapter.ToolResult{ToolCallID: call.ID, Content: fmt.Sprintf("failed to add context: %v", dbErr), IsError: true}
			}
		case "pause":
			_, dbErr := e.db.Exec(ctx,
				`UPDATE work_items SET state = 'blocked', updated_at = NOW() WHERE id = $1`,
				workItemID)
			if dbErr != nil {
				return adapter.ToolResult{ToolCallID: call.ID, Content: fmt.Sprintf("failed to pause: %v", dbErr), IsError: true}
			}
		case "resume":
			_, dbErr := e.db.Exec(ctx,
				`UPDATE work_items SET state = 'implementing', updated_at = NOW() WHERE id = $1`,
				workItemID)
			if dbErr != nil {
				return adapter.ToolResult{ToolCallID: call.ID, Content: fmt.Sprintf("failed to resume: %v", dbErr), IsError: true}
			}
		default:
			return adapter.ToolResult{ToolCallID: call.ID, Content: fmt.Sprintf("unknown action: %s", execProposal.Action), IsError: true}
		}
		_, _ = e.db.Exec(ctx,
			`INSERT INTO work_items_timeline (work_item_id, entry_type, agent, summary, created_at)
			 VALUES ($1, 'chat_modification', 'chat-agent', $2, NOW())`,
			workItemID, fmt.Sprintf("Chat proposal executed: %s — %s", execProposal.Action, changes.Rationale))
		return adapter.ToolResult{
			ToolCallID: call.ID,
			Content:    fmt.Sprintf("Proposal executed: %s on work item %s. Change: %s", execProposal.Action, workItemID, changes.Rationale),
		}

	case "get_work_item_detail":
		var wiArgs struct {
			WorkItemID string `json:"work_item_id"`
		}
		if err := json.Unmarshal([]byte(call.Arguments), &wiArgs); err != nil {
			return adapter.ToolResult{ToolCallID: call.ID, Content: fmt.Sprintf("invalid arguments: %v", err), IsError: true}
		}
		if wiArgs.WorkItemID == "" {
			return adapter.ToolResult{ToolCallID: call.ID, Content: "work_item_id is required", IsError: true}
		}
		wiID, parseErr := uuid.Parse(wiArgs.WorkItemID)
		if parseErr != nil {
			return adapter.ToolResult{ToolCallID: call.ID, Content: fmt.Sprintf("invalid work_item_id: %v", parseErr), IsError: true}
		}
		if e.db == nil {
			return adapter.ToolResult{ToolCallID: call.ID, Content: "database not available", IsError: true}
		}
		var wiTitle, wiDesc, wiState, wiPriority, wiType string
		var wiCreatedAt, wiUpdatedAt time.Time
		err := e.db.QueryRow(ctx,
			`SELECT title, description, state, priority, type, created_at, updated_at
			 FROM work_items WHERE id = $1`, wiID).Scan(&wiTitle, &wiDesc, &wiState, &wiPriority, &wiType, &wiCreatedAt, &wiUpdatedAt)
		if err != nil {
			return adapter.ToolResult{ToolCallID: call.ID, Content: fmt.Sprintf("work item not found: %v", err), IsError: true}
		}
		tlRows, _ := e.db.Query(ctx,
			`SELECT entry_type, agent, summary, created_at FROM work_items_timeline
			 WHERE work_item_id = $1 ORDER BY created_at DESC LIMIT 5`, wiID)
		var timeline []string
		if tlRows != nil {
			defer tlRows.Close()
			for tlRows.Next() {
				var et, ag, sm string
				var ca time.Time
				if tlRows.Scan(&et, &ag, &sm, &ca) == nil {
					timeline = append(timeline, fmt.Sprintf("[%s] %s (%s): %s", ca.Format("2006-01-02 15:04"), et, ag, sm))
				}
			}
		}
		wiResult := fmt.Sprintf("Work Item: %s\nTitle: %s\nType: %s\nPriority: %s\nState: %s\nCreated: %s\nUpdated: %s\n\nDescription:\n%s",
			wiID, wiTitle, wiType, wiPriority, wiState, wiCreatedAt.Format(time.RFC3339), wiUpdatedAt.Format(time.RFC3339), wiDesc)
		if len(timeline) > 0 {
			wiResult += "\n\nRecent Activity:\n" + strings.Join(timeline, "\n")
		}
		return adapter.ToolResult{ToolCallID: call.ID, Content: wiResult}

	case "get_board_summary":
		var bsArgs struct {
			StatusFilter   string `json:"status_filter"`
			PriorityFilter string `json:"priority_filter"`
		}
		json.Unmarshal([]byte(call.Arguments), &bsArgs) //nolint:errcheck
		if e.db == nil {
			return adapter.ToolResult{ToolCallID: call.ID, Content: "database not available", IsError: true}
		}
		stateRows, err := e.db.Query(ctx,
			`SELECT state, COUNT(*) FROM work_items GROUP BY state ORDER BY COUNT(*) DESC`)
		if err != nil {
			return adapter.ToolResult{ToolCallID: call.ID, Content: fmt.Sprintf("query failed: %v", err), IsError: true}
		}
		defer stateRows.Close()
		var bsSummary strings.Builder
		bsSummary.WriteString("Pipeline Board Summary\n\nBy State:\n")
		for stateRows.Next() {
			var st string
			var cnt int
			if stateRows.Scan(&st, &cnt) == nil {
				bsSummary.WriteString(fmt.Sprintf("  %s: %d\n", st, cnt))
			}
		}
		bRows, _ := e.db.Query(ctx,
			`SELECT wi.id, wi.title, wt.summary FROM work_items wi
			 LEFT JOIN LATERAL (
			   SELECT summary FROM work_items_timeline
			   WHERE work_item_id = wi.id ORDER BY created_at DESC LIMIT 1
			 ) wt ON true
			 WHERE wi.state = 'blocked' ORDER BY wi.updated_at DESC LIMIT 10`)
		if bRows != nil {
			defer bRows.Close()
			bsSummary.WriteString("\nBlocked Items:\n")
			for bRows.Next() {
				var bID, bTitle string
				var reason *string
				if bRows.Scan(&bID, &bTitle, &reason) == nil {
					r := "no reason recorded"
					if reason != nil {
						r = *reason
					}
					bsSummary.WriteString(fmt.Sprintf("  %s — %s (reason: %s)\n", bID[:8], bTitle, r))
				}
			}
		}
		return adapter.ToolResult{ToolCallID: call.ID, Content: bsSummary.String()}

	case "get_session_history":
		var shArgs struct {
			SessionID string `json:"session_id"`
		}
		json.Unmarshal([]byte(call.Arguments), &shArgs) //nolint:errcheck
		if e.db == nil {
			return adapter.ToolResult{ToolCallID: call.ID, Content: "database not available", IsError: true}
		}
		if shArgs.SessionID == "" {
			return adapter.ToolResult{ToolCallID: call.ID, Content: "session_id is required", IsError: true}
		}
		sessID, parseErr := uuid.Parse(shArgs.SessionID)
		if parseErr != nil {
			return adapter.ToolResult{ToolCallID: call.ID, Content: fmt.Sprintf("invalid session_id: %v", parseErr), IsError: true}
		}
		shRows, err := e.db.Query(ctx,
			`SELECT role, content, created_at FROM chat_messages
			 WHERE session_id = $1 ORDER BY created_at ASC LIMIT 50`, sessID)
		if err != nil {
			return adapter.ToolResult{ToolCallID: call.ID, Content: fmt.Sprintf("query failed: %v", err), IsError: true}
		}
		defer shRows.Close()
		var shSummary strings.Builder
		shSummary.WriteString("Session History:\n\n")
		for shRows.Next() {
			var role, content string
			var ca time.Time
			if shRows.Scan(&role, &content, &ca) == nil {
				preview := content
				if len([]rune(preview)) > 200 {
					preview = string([]rune(preview)[:200]) + "..."
				}
				shSummary.WriteString(fmt.Sprintf("[%s] %s: %s\n\n", ca.Format("15:04"), role, preview))
			}
		}
		return adapter.ToolResult{ToolCallID: call.ID, Content: shSummary.String()}

	default:
		result = adapter.ToolResult{
			ToolCallID: call.ID,
			Content:    "Unknown tool: " + call.Name,
			IsError:    true,
		}
	}

	// Gap tracking: when a filesystem-level tool succeeds AFTER a codex_*
	// tool returned zero results in the same loop, emit a codex.gap event.
	e.maybeEmitGap(ctx, call.Name, args, result)

	// Planner-scope gap: when the agent writes/edits a file that was not
	// in the planner's FilesToChange set, the brief's affected_files list
	// was incomplete. Emit a distinct codex.gap event so downstream
	// consumers (change-reconciler) can learn which files the structured
	// brief should have flagged.
	e.maybeEmitPlannerGap(ctx, call.Name, args, result)

	if e.onTool != nil {
		e.onTool(call.ID, call.Name, call.Arguments, result.Content, result.IsError, MetaOf(result))
	}
	if e.annotate != nil {
		if note := e.annotate(call.Name, call.Arguments, result); note != "" {
			result.Content += "\n\n" + note
		}
	}
	return result
}

// normalizePath strips a leading repo-name directory if the model
// incorrectly prepends it. This happens when codex tools return paths
// scoped by repo name (e.g. "example-app/lib/foo.ts") and the
// model passes that verbatim to read_file / edit_file. In a worktree
// the directory name is "worktree-XXXX", not the repo name, so the
// extra segment produces "file not found".
func (e *Executor) normalizePath(p string) string {
	cleaned := filepath.Clean(p)
	joined := filepath.Join(e.repoPath, cleaned)
	if _, err := os.Stat(joined); err == nil {
		return cleaned
	}
	// Try stripping the first path component.
	parts := strings.SplitN(filepath.ToSlash(cleaned), "/", 2)
	if len(parts) == 2 {
		stripped := filepath.FromSlash(parts[1])
		if _, err := os.Stat(filepath.Join(e.repoPath, stripped)); err == nil {
			return stripped
		}
	}
	return cleaned // unchanged — let the caller report the real error
}

func (e *Executor) readFile(callID string, args map[string]interface{}) adapter.ToolResult {
	path, _ := args["path"].(string)
	path = e.normalizePath(path)
	fullPath := filepath.Join(e.repoPath, filepath.Clean(path))
	// Security: prevent path traversal outside the repo
	if !strings.HasPrefix(fullPath, e.repoPath) {
		return adapter.ToolResult{ToolCallID: callID, Content: "Path traversal not allowed", IsError: true}
	}
	data, err := os.ReadFile(fullPath)
	if err != nil {
		// Mark the path as observed even on miss — the model has seen that
		// this path doesn't exist, which is exactly the evidence needed to
		// satisfy the read-before-edit gate for a legitimate write_file
		// (creating a new file) or to pivot to search_code for the right
		// path. Return the error so the model can react.
		e.markObserved(path)
		return adapter.ToolResult{ToolCallID: callID, Content: "Error reading file: " + err.Error(), IsError: true}
	}
	e.markObserved(path)
	note := ""
	// The cap counts \n-normalised text, as readPlan and codex_read_file do.
	if window, n, ok := e.defaultWindow(path, args, fileLinesOf(strings.ReplaceAll(string(data), "\r\n", "\n"))); ok {
		args, note = window, n
	}
	out, start, end, total := sliceLines(string(data), args)
	e.markRead(path, start, end, total)
	if note != "" {
		// The seeded note replaces sliceLines' own range hint.
		if k := strings.LastIndex(out, "\n[lines "); k >= 0 && strings.HasSuffix(strings.TrimRight(out, "\n"), "for more]") {
			out = out[:k]
		}
		out = strings.TrimRight(out, "\n") + "\n" + note
	}
	return adapter.ToolResult{ToolCallID: callID, Content: out}
}

// readCoverage is which lines of a file the model has read (R6 review: a
// default 400-line read followed by write_file dropped the rest).
type readCoverage struct {
	total  int
	ranges [][2]int // merged, sorted
}

func (c *readCoverage) full() bool {
	return c.total == 0 || (len(c.ranges) == 1 && c.ranges[0][0] <= 1 && c.ranges[0][1] >= c.total)
}

func (c *readCoverage) String() string {
	var parts []string
	for _, r := range c.ranges {
		parts = append(parts, fmt.Sprintf("%d-%d", r[0], r[1]))
	}
	return fmt.Sprintf("lines %s of %d", strings.Join(parts, ", "), c.total)
}

// markRead records that lines start..end of a total-line file were read. A
// read of a different total (the file changed) starts the coverage over.
func (e *Executor) markRead(path string, start, end, total int) {
	if path == "" || end < start {
		return
	}
	e.observedMu.Lock()
	defer e.observedMu.Unlock()
	if e.readCov == nil {
		e.readCov = map[string]*readCoverage{}
	}
	k := filepath.Clean(path)
	c := e.readCov[k]
	if c == nil || c.total != total {
		c = &readCoverage{total: total}
		e.readCov[k] = c
	}
	c.ranges = append(c.ranges, [2]int{start, end})
	sort.Slice(c.ranges, func(i, j int) bool { return c.ranges[i][0] < c.ranges[j][0] })
	merged := c.ranges[:1]
	for _, r := range c.ranges[1:] {
		last := &merged[len(merged)-1]
		if r[0] <= last[1]+1 {
			last[1] = max(last[1], r[1])
		} else {
			merged = append(merged, r)
		}
	}
	c.ranges = merged
}

// shiftReadTotal follows an edit that changed a file's line count by delta:
// a full read stays full; a partial one stays partial (its ranges are
// approximate after the edit, which only matters for the message).
func (e *Executor) shiftReadTotal(path string, delta int) {
	e.observedMu.Lock()
	defer e.observedMu.Unlock()
	c := e.readCov[filepath.Clean(path)]
	if c == nil || delta == 0 {
		return
	}
	wasFull := c.full()
	c.total += delta
	if wasFull {
		c.ranges = [][2]int{{1, c.total}}
	}
}

// markReadFull records the whole file as known (the model just wrote it).
func (e *Executor) markReadFull(path string) {
	e.observedMu.Lock()
	defer e.observedMu.Unlock()
	if e.readCov != nil {
		delete(e.readCov, filepath.Clean(path))
	}
}

// partialRead is the coverage of a path read only in part, or nil.
func (e *Executor) partialRead(path string) *readCoverage {
	e.observedMu.Lock()
	defer e.observedMu.Unlock()
	if c := e.readCov[filepath.Clean(path)]; c != nil && !c.full() {
		return c
	}
	return nil
}

// readDefaultLines is how much of a file a read without a range returns
// (T6: attempt 6 read an 11.6k-char file whole); readMaxLines caps an
// explicit range.
const (
	readDefaultLines = 400
	readMaxLines     = 2000
)

// readWindow resolves a read's start_line/end_line (1-based, inclusive)
// against a file of total lines: no range is the first readDefaultLines,
// an explicit range any slice up to readMaxLines.
func readWindow(args map[string]interface{}, total int) (start, end int, explicit bool) {
	start, end = 1, 0
	if v, ok := args["start_line"].(float64); ok && int(v) > 0 {
		start, explicit = int(v), true
	}
	if v, ok := args["end_line"].(float64); ok && int(v) > 0 {
		end, explicit = int(v), true
	}
	switch {
	case end < start && explicit:
		end = start + readMaxLines - 1
	case end < start:
		end = start + readDefaultLines - 1
	}
	if end-start+1 > readMaxLines {
		end = start + readMaxLines - 1
	}
	if end > total {
		end = total
	}
	return start, end, explicit
}

// sliceLines is read_file's result: the requested lines verbatim and, when
// the file goes on, a hint naming the next range; start..end of total are
// the lines returned.
func sliceLines(content string, args map[string]interface{}) (out string, start, end, total int) {
	lines := strings.SplitAfter(content, "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	total = len(lines)
	start, end, explicit := readWindow(args, total)
	if !explicit && end >= total {
		return content, 1, total, total
	}
	if start > total {
		return fmt.Sprintf("[the file has %d lines; start_line %d is past its end]", total, start), start, start - 1, total
	}
	return sliceBody(lines, start, end, total), start, end, total
}

func sliceBody(lines []string, start, end, total int) string {
	out := strings.Join(lines[start-1:end], "")
	if end < total {
		if !strings.HasSuffix(out, "\n") {
			out += "\n"
		}
		out += fmt.Sprintf("[lines %d-%d of %d; read_file with start_line=%d for more]", start, end, total, end+1)
	}
	return out
}

// markObserved records that the model has seen the on-disk state of path
// (either its contents or that it doesn't exist). Gates edit_file /
// write_file from acting on paths the model has never read.
func (e *Executor) markObserved(path string) {
	if path == "" {
		return
	}
	e.observedMu.Lock()
	defer e.observedMu.Unlock()
	if e.observedPaths == nil {
		e.observedPaths = make(map[string]struct{})
	}
	e.observedPaths[filepath.Clean(path)] = struct{}{}
}

// hasObserved reports whether the model has read this path (or been told
// it doesn't exist) at some point in this session.
func (e *Executor) hasObserved(path string) bool {
	if path == "" {
		return false
	}
	e.observedMu.Lock()
	defer e.observedMu.Unlock()
	_, ok := e.observedPaths[filepath.Clean(path)]
	return ok
}

func (e *Executor) writeFile(callID string, args map[string]interface{}) adapter.ToolResult {
	path, _ := args["path"].(string)
	path = e.normalizePath(path)
	content, _ := args["content"].(string)
	fullPath := filepath.Join(e.repoPath, filepath.Clean(path))
	if !strings.HasPrefix(fullPath, e.repoPath) {
		return adapter.ToolResult{ToolCallID: callID, Content: "Path traversal not allowed", IsError: true}
	}
	// Read-before-write gate: the model must have observed this path's
	// on-disk state before creating/overwriting it, so it knows whether
	// it's creating a new file intentionally or clobbering an unrelated
	// one. If the path is being created fresh, the model should have
	// called read_file on it first and seen "no such file" — that counts
	// as observation. If the model skipped the read, reject with a clear
	// next-step instruction rather than letting an orphan file land.
	if !e.hasObserved(path) {
		return adapter.ToolResult{
			ToolCallID: callID,
			Content: "REJECTED: read_file must be called on this path before write_file. Call read_file(\"" + path + "\") first. " +
				"If it returns 'no such file or directory' you may then write_file to create it — but verify no existing file covers the same concern via codex_find_module or search_code before creating a new one. Orphan files that nothing imports have no effect on the live app.",
			IsError: true,
		}
	}
	// Capture pre-write state so the LSP validator can revert cleanly on
	// failure. For a new file, oldContent stays nil → revert = delete.
	var oldContent []byte
	if prev, rerr := os.ReadFile(fullPath); rerr == nil {
		oldContent = prev
		// An existing file read only in part: write_file would replace
		// it with what the model saw and drop the rest.
		if c := e.partialRead(path); c != nil {
			return adapter.ToolResult{
				ToolCallID: callID,
				Content:    fmt.Sprintf("REJECTED: file was read partially (%s); use edit_file, or read the rest first.", c),
				IsError:    true,
			}
		}
	}
	// R10 W7: content that would leave the file unparsable is refused
	// before it is written.
	if rej := e.syntaxRejection(path, oldContent, oldContent != nil, []byte(content)); rej != "" {
		return adapter.ToolResult{ToolCallID: callID, Content: rej, IsError: true}
	}
	if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
		return adapter.ToolResult{ToolCallID: callID, Content: "Error creating directories: " + err.Error(), IsError: true}
	}
	if err := os.WriteFile(fullPath, []byte(content), 0644); err != nil {
		return adapter.ToolResult{ToolCallID: callID, Content: "Error writing file: " + err.Error(), IsError: true}
	}
	if rejection := e.validateAfterWrite(context.Background(), callID, path, oldContent); rejection != nil {
		return *rejection
	}
	e.markObserved(path)
	e.markReadFull(path) // the model wrote all of it
	return adapter.ToolResult{ToolCallID: callID, Content: "File written successfully: " + path}
}

func (e *Executor) editFile(callID string, args map[string]interface{}) adapter.ToolResult {
	path, _ := args["path"].(string)
	path = e.normalizePath(path)
	oldString, _ := args["old_string"].(string)
	newString, _ := args["new_string"].(string)

	if oldString == "" {
		return adapter.ToolResult{ToolCallID: callID, Content: "old_string is required and cannot be empty", IsError: true}
	}

	// Read-before-edit gate: same reasoning as write_file. edit_file in
	// particular is only meaningful on existing content — if the model
	// hasn't read the file, its old_string is guessed, not grounded. Force
	// a read first.
	if !e.hasObserved(path) {
		return adapter.ToolResult{
			ToolCallID: callID,
			Content:    "REJECTED: read_file must be called on this path before edit_file. Call read_file(\"" + path + "\") first, then pass old_string copied verbatim from the result. If the file does not exist, search_code / codex_find_module will help you locate the right path.",
			IsError:    true,
		}
	}

	fullPath := filepath.Join(e.repoPath, filepath.Clean(path))
	if !strings.HasPrefix(fullPath, e.repoPath) {
		return adapter.ToolResult{ToolCallID: callID, Content: "Path traversal not allowed", IsError: true}
	}

	data, err := os.ReadFile(fullPath)
	if err != nil {
		return adapter.ToolResult{ToolCallID: callID, Content: "Error reading file: " + err.Error(), IsError: true}
	}

	content := string(data)

	// Duplication detection: if old_string appears MORE than once inside
	// new_string the model is repeating the original content — the pattern
	// that produced PR #9's 13× repeated `import * as z`. A single
	// occurrence is legitimate: the model is extending a block (e.g.,
	// adding a line after the matched text). Only reject the true
	// duplication case.
	if oldString != newString && strings.Count(newString, oldString) > 1 {
		msg := "REJECTED: new_string contains old_string multiple times — this would duplicate content. Provide a new_string that contains old_string at most once, with your additions before or after it."
		// R11 S2: name the cheaper call (attempt 11 re-typed a whole test
		// suite around `});` four times).
		if steer := insertSteering(path, content, oldString, newString); steer != "" {
			msg += "\n" + steer
		}
		return adapter.ToolResult{ToolCallID: callID, Content: msg, IsError: true}
	}

	// R10 W2: an edit whose new_string is already where old_string was
	// (or whose old_string is gone and new_string is there once) is not
	// applied again; a missing old_string gets the nearest lines; several
	// matches can be narrowed with near_line.
	nearLine := 0
	if v, ok := args["near_line"].(float64); ok && v > 0 {
		nearLine = int(v)
	}
	occ := occurrences(content, oldString)
	if len(occ) == 0 {
		if replacedTextPresent(content, oldString, newString) {
			return adapter.ToolResult{ToolCallID: callID, Content: alreadyAppliedResult(path, lineAt(content, strings.Index(content, newString)))}
		}
		return adapter.ToolResult{ToolCallID: callID, Content: notFoundMessage(path, content, oldString), IsError: true}
	}
	idx := occ[0]
	if len(occ) > 1 && nearLine > 0 {
		idx = nearestOccurrence(content, occ, nearLine)
		occ = []int{idx}
	}
	if len(occ) == 1 {
		if line, ok := coveredByNew(content, idx, oldString, newString); ok {
			return adapter.ToolResult{ToolCallID: callID, Content: alreadyAppliedResult(path, line)}
		}
	}
	if count := len(occ); count > 1 {
		// Help the model disambiguate by showing WHERE each match is and
		// the surrounding lines. Previously we just returned "found N
		// times — must be unique," and local models (qwen2.5-coder) would
		// retry with the identical short old_string 10+ times because they
		// had no signal about which match to target. With per-match line
		// numbers + 2-line context the model can pick one and expand its
		// old_string to that context.
		msg := fmt.Sprintf("old_string found %d times — must be unique. Here are each match and 2 lines of surrounding context; copy additional context from ONE of them into your next old_string so it matches exactly once, or pass near_line=<its line number>:\n\n", count)
		msg += describeMatches(content, oldString, 3)
		return adapter.ToolResult{ToolCallID: callID, Content: msg, IsError: true}
	}

	// A block re-typed next to the text it keeps (R10 W1): the message
	// names the lines and both copies, so the model fixes the real problem.
	if retyped := detectRetypedBlock(content, idx, oldString, newString); retyped != "" {
		// R11 S2: the steering comes right after the first line, ahead of
		// the quoted lines, so a LoopStuck reason (cut at 700 chars)
		// carries it into the continuation prompt.
		first, rest, _ := strings.Cut(retyped, "\n")
		msg := "REJECTED: edit would duplicate a block. " + first
		if steer := insertSteering(path, content, oldString, newString); steer != "" {
			msg += "\n" + steer
		}
		if rest != "" {
			msg += "\n" + rest
		}
		return adapter.ToolResult{ToolCallID: callID, Content: msg, IsError: true}
	}

	// R11 review: a CRLF file keeps CRLF endings.
	newString = matchLineEndings(content, idx, newString)
	newContent := content[:idx] + newString + content[idx+len(oldString):]

	// Corruption detection: a well-formed edit doesn't introduce exact-line
	// duplicates that weren't there before. When the model emits the same
	// line 3+ times in a row (its "plan accumulated instead of replaced"
	// failure mode), the resulting file won't build and will confuse every
	// downstream reviewer. Reject before writing.
	if corrupt := detectSuspiciousDuplicates(content, newContent); corrupt != "" {
		return adapter.ToolResult{
			ToolCallID: callID,
			Content:    "REJECTED: edit introduces suspicious duplicates that did not exist in the original file. " + corrupt + " This usually means your new_string duplicated lines that should have been replaced once. Re-issue the edit with a clean new_string.",
			IsError:    true,
		}
	}

	// R10 W7: an edit that would leave the file unparsable (a brace, tag
	// or string it deletes or never closes) is refused before it is
	// written, with the parser's line and the lines around it.
	if rej := e.syntaxRejection(path, []byte(content), true, []byte(newContent)); rej != "" {
		return adapter.ToolResult{ToolCallID: callID, Content: rej, IsError: true}
	}

	if err := os.WriteFile(fullPath, []byte(newContent), 0644); err != nil {
		return adapter.ToolResult{ToolCallID: callID, Content: "Error writing file: " + err.Error(), IsError: true}
	}
	// Post-write LSP validation: for supported languages, the edit must
	// not introduce error-severity diagnostics. On failure the validator
	// reverts the file to `content` (pre-edit bytes we already have in
	// hand) and returns a rejection ToolResult for the model.
	if rejection := e.validateAfterWrite(context.Background(), callID, path, []byte(content)); rejection != nil {
		return *rejection
	}

	e.shiftReadTotal(path, strings.Count(newString, "\n")-strings.Count(oldString, "\n"))
	from := lineAt(newContent, idx)
	return adapter.ToolResult{ToolCallID: callID, Content: fmt.Sprintf("Edit applied to %s: replaced 1 occurrence at lines %d-%d", path, from, from+strings.Count(newString, "\n"))}
}

// validateAfterWrite runs the newly-written file through LSP diagnostics
// to catch syntax and semantic errors before the edit is accepted. If
// the LSP reports any error-severity diagnostics, the file is reverted
// to oldContent and an error ToolResult is returned — the model sees the
// diagnostics verbatim and can retry with a corrected edit.
//
// The validator is conservative: it skips silently when
//   - LSP manager isn't configured
//   - codex/productID isn't wired (no way to resolve repo root)
//   - the file extension isn't one we know a language for
//   - the LSP server isn't available for the repo
//   - the LSP doesn't publish diagnostics within the poll window
//
// A skip is NOT a failure. Validation is a best-effort safety net, not
// a hard gate. Write-success without LSP validation is still the
// fallback path, and the existing verify_gate is the backstop.
//
// Only severity=1 (Error) diagnostics trigger a revert. Warnings, info,
// and hints are visible to the model via codex_diagnostics but don't
// block a write — style nits shouldn't cause tool loops to restart.
func (e *Executor) validateAfterWrite(ctx context.Context, callID, relPath string, oldContent []byte) *adapter.ToolResult {
	if e.lspManager == nil || e.codex == nil || e.productID == nil {
		return nil
	}
	language, languageID := lspLanguageForExt(relPath)
	if language == "" {
		return nil
	}

	// Resolve repo root. For the non-codex writer path, e.repoPath is the
	// repo clone root directly, so we use that. codex_read_file uses the
	// product-wide repo cache; writeFile/editFile always scope to
	// e.repoPath.
	root := e.repoPath
	fullPath := filepath.Join(root, filepath.FromSlash(relPath))
	newContent, err := os.ReadFile(fullPath)
	if err != nil {
		// File gone between write and validate — unexpected but not our
		// problem to report; the write itself was successful.
		return nil
	}
	if looksBinary(newContent[:min(len(newContent), 1024)]) {
		return nil
	}

	validateCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	client, _, cerr := e.lspManager.ClientForRepo(validateCtx, root)
	if cerr != nil {
		// No LSP for this project (e.g., gopls/tsserver not present).
		// Skip — we've logged in the manager; don't block writes.
		return nil
	}

	uri := lsp.PathToURI(fullPath)
	if openErr := client.OpenDoc(uri, languageID, string(newContent)); openErr != nil {
		return nil
	}

	// Poll for diagnostics. Servers publish asynchronously after didOpen.
	// We wait up to ~2.5s for Error-severity diagnostics specifically; if
	// nothing shows up, assume clean. Warnings arriving later won't be
	// seen but that's acceptable — they wouldn't have blocked anyway.
	deadline := time.Now().Add(2500 * time.Millisecond)
	var errorDiags []lsp.Diagnostic
	for time.Now().Before(deadline) {
		diags := client.Diagnostics(uri)
		errorDiags = errorDiags[:0]
		for _, d := range diags {
			if d.Severity == 1 {
				errorDiags = append(errorDiags, d)
			}
		}
		if len(errorDiags) > 0 {
			break
		}
		select {
		case <-time.After(150 * time.Millisecond):
		case <-validateCtx.Done():
			deadline = time.Now()
		}
	}

	if len(errorDiags) == 0 {
		return nil
	}

	// Revert the file so the model can try again with a clean baseline.
	// oldContent is nil only when writeFile created a brand-new file; in
	// that case, deleting the file is the correct revert.
	if oldContent == nil {
		_ = os.Remove(fullPath)
	} else {
		_ = os.WriteFile(fullPath, oldContent, 0644)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "REJECTED: the edit introduced %d error-severity diagnostic(s). The file has been reverted to its previous content. Fix the issues below and retry the write:\n\n", len(errorDiags))
	for i, d := range errorDiags {
		if i >= 10 {
			fmt.Fprintf(&sb, "… and %d more\n", len(errorDiags)-10)
			break
		}
		source := d.Source
		if source == "" {
			source = language
		}
		fmt.Fprintf(&sb, "  [%s] line %d col %d: %s\n", source, d.Range.Start.Line+1, d.Range.Start.Character+1, d.Message)
	}
	return &adapter.ToolResult{ToolCallID: callID, Content: sb.String(), IsError: true}
}

// maybeMicrocompact trims the conversation in place when the input-token
// count for the most recent LLM call exceeds a threshold. The strategy
// is surgical: only the content of older tool_result messages is
// shortened (they're the largest payloads — file contents, command
// output, codex graph dumps). Assistant/user messages stay full so the
// model retains its own reasoning and the user's instructions verbatim.
//
// Keeps the last `keepRecent` tool_result messages intact so the model
// still has full context on what it just did. Older ones get truncated
// to a preview + byte-count marker. Returns true if any trimming
// happened (caller may want to log).
//
// This is "microcompact" in the two-tier scheme. The heavier "compact"
// (fork a summarizer LLM to collapse the old half of the conversation
// into a single system message) is intentionally not implemented here;
// microcompact alone buys ~50–70% token reduction on long tool loops
// which is typically enough to keep qwen2.5-coder:32b inside its
// effective 8k context without the complexity of a summarizer fork.
func maybeMicrocompact(messages []adapter.ChatMessage, inputTokenCount int, threshold int, keepRecent int, maxOldContentBytes int) bool {
	if inputTokenCount < threshold {
		return false
	}
	trimmed := false
	toolResultsSeen := 0
	// Walk backwards; first `keepRecent` tool_results stay full. Older
	// ones truncate.
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != adapter.RoleTool {
			continue
		}
		toolResultsSeen++
		if toolResultsSeen <= keepRecent {
			continue
		}
		if len(messages[i].Content) > maxOldContentBytes {
			preview := messages[i].Content[:maxOldContentBytes]
			messages[i].Content = fmt.Sprintf("%s\n\n[… %d bytes truncated by microcompact — full content was available when the model made its next decision]", preview, len(messages[i].Content)-maxOldContentBytes)
			trimmed = true
		}
	}
	return trimmed
}

// maybeCheckpoint replaces maybeMicrocompact. When input tokens exceed
// the threshold, it stores the full conversation in the DB and replaces
// it with: [system prompt, task, evidence ledger (if any) — checkpointPinned —
// checkpoint summary, the last few messages from a turn boundary
// (checkpointTailStartAfter)]. Returns true if a checkpoint was
// created.
func maybeCheckpoint(
	ctx context.Context,
	db *pgxpool.Pool,
	messages *[]adapter.ChatMessage,
	inputTokenCount int,
	threshold int,
	workItemID *uuid.UUID,
	agentType string,
	snapshotSeq *int,
	recallOffered bool,
) bool {
	if db == nil {
		return false
	}
	return checkpoint(ctx, dbCheckpointStore(db), messages, inputTokenCount, threshold, workItemID, agentType, snapshotSeq, recallOffered)
}

// dbCheckpointStore stores snapshots in conversation_snapshots.
func dbCheckpointStore(db *pgxpool.Pool) checkpointStore {
	return func(ctx context.Context, snapshotID, workItemID uuid.UUID, agentType string, seq int, messagesJSON []byte, summary string, turns int) error {
		_, err := db.Exec(ctx,
			`INSERT INTO conversation_snapshots (id, work_item_id, agent_type, snapshot_seq, messages, summary, turn_count)
			 VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7)`,
			snapshotID, workItemID, agentType, seq, string(messagesJSON), summary, turns,
		)
		return err
	}
}

// checkpointStore persists one conversation snapshot (maybeCheckpoint's is
// the conversation_snapshots insert; tests pass a fake).
type checkpointStore func(ctx context.Context, snapshotID, workItemID uuid.UUID, agentType string, seq int, messagesJSON []byte, summary string, turns int) error

// checkpointKeepLast is roughly how many trailing messages a checkpoint
// keeps (about 3 assistant+tool pairs); checkpointTailStart moves the cut to
// a turn boundary.
const checkpointKeepLast = 6

// checkpointTailStart is the index the checkpoint's kept tail starts at, or
// -1 when no cut is possible. The tail is about the last keepLast messages,
// but it always starts at a turn boundary — a non-tool message — so no kept
// tool result is separated from the assistant turn that called it (a
// count-based cut sent attempt 5 "system,user,tool×6,assistant,…", which
// strict providers reject). The cut moves back to the assistant turn that
// owns the first kept tool result; when that turn is the first message
// after the original prompt (nothing would be summarised), it moves forward
// to the next boundary instead; when there is none, -1. The cut is never at
// or below index 2: the checkpoint keeps the system prompt and replaces
// everything else before the tail with one summary message, so a cut at 2
// (just after the original prompt) would shrink nothing.
func checkpointTailStart(msgs []adapter.ChatMessage, keepLast int) int {
	return checkpointTailStartAfter(msgs, keepLast, 2)
}

// checkpointTailStartAfter is checkpointTailStart for a transcript whose
// first pinned messages stay verbatim: the cut is always after pinned+1, so
// at least one message is summarised.
func checkpointTailStartAfter(msgs []adapter.ChatMessage, keepLast, pinned int) int {
	if keepLast < 1 {
		keepLast = 1
	}
	if pinned < 2 {
		pinned = 2
	}
	if len(msgs) <= keepLast+1 {
		return -1
	}
	start := len(msgs) - keepLast
	b := start
	for b >= 0 && msgs[b].Role == adapter.RoleTool {
		b--
	}
	if b > pinned {
		return b
	}
	for f := max(start+1, pinned+1); f < len(msgs); f++ {
		if msgs[f].Role != adapter.RoleTool {
			return f
		}
	}
	return -1
}

// checkpointPinned is how many leading messages a checkpoint keeps
// verbatim: the system prompt, the first user message (the task — a live
// run, attempt 6, lost its 8-step task to 8 checkpoints in a row) and the
// evidence ledger when it follows the task. A user message that is itself
// a checkpoint summary is never pinned: it is summarised again.
func checkpointPinned(msgs []adapter.ChatMessage) int {
	n := 0
	if n < len(msgs) && msgs[n].Role == adapter.RoleSystem {
		n++
	}
	if n < len(msgs) && msgs[n].Role == adapter.RoleUser && !isCheckpointSummary(msgs[n]) {
		n++
		if n < len(msgs) && isLedgerMessage(msgs[n]) {
			n++
		}
	}
	return n
}

// isCheckpointSummary reports a summary message a checkpoint inserted.
func isCheckpointSummary(m adapter.ChatMessage) bool {
	return m.Role == adapter.RoleUser && strings.HasPrefix(m.Content, checkpointHeader)
}

// checkpoint is maybeCheckpoint with the store injected.
func checkpoint(
	ctx context.Context,
	store checkpointStore,
	messages *[]adapter.ChatMessage,
	inputTokenCount int,
	threshold int,
	workItemID *uuid.UUID,
	agentType string,
	snapshotSeq *int,
	recallOffered bool,
) bool {
	return checkpointDropping(ctx, store, messages, inputTokenCount, threshold, workItemID, agentType, snapshotSeq, recallOffered, nil)
}

// checkpointDropping is checkpoint with onDrop called on the span it
// summarises away, before it goes (R10 W6: the loop hands it to the
// evidence ledger, which pins the in-scope reads in it).
func checkpointDropping(
	ctx context.Context,
	store checkpointStore,
	messages *[]adapter.ChatMessage,
	inputTokenCount int,
	threshold int,
	workItemID *uuid.UUID,
	agentType string,
	snapshotSeq *int,
	recallOffered bool,
	onDrop func(span []adapter.ChatMessage),
) bool {
	if inputTokenCount < threshold || store == nil || workItemID == nil {
		return false
	}
	if len(*messages) < 8 {
		return false
	}
	pinned := checkpointPinned(*messages)
	tailStart := checkpointTailStartAfter(*messages, checkpointKeepLast, pinned)
	if tailStart < 0 {
		log.Printf("tool-loop: checkpoint skipped: no turn boundary to cut %d messages at without separating tool results from their assistant turn", len(*messages))
		return false
	}

	snapshotID := uuid.New()
	*snapshotSeq++

	messagesJSON, err := json.Marshal(*messages)
	if err != nil {
		log.Printf("tool-loop: checkpoint marshal failed: %v", err)
		return false
	}

	summary := buildCheckpointSummary((*messages)[pinned:tailStart], snapshotID, recallOffered)

	if err := store(ctx, snapshotID, *workItemID, agentType, *snapshotSeq, messagesJSON, summary, len(*messages)); err != nil {
		log.Printf("tool-loop: checkpoint insert failed: %v", err)
		return false
	}

	if onDrop != nil {
		onDrop((*messages)[pinned:tailStart])
	}

	// Build the new message array: the pinned head (system prompt, the task,
	// the evidence ledger) verbatim, the checkpoint summary, then the tail.
	newMessages := make([]adapter.ChatMessage, 0, pinned+1+len(*messages)-tailStart)
	newMessages = append(newMessages, (*messages)[:pinned]...)

	// Add the checkpoint summary as a user message
	newMessages = append(newMessages, adapter.ChatMessage{
		Role:    adapter.RoleUser,
		Content: summary,
	})

	// Keep the tail from a turn boundary (checkpointTailStart).
	newMessages = append(newMessages, (*messages)[tailStart:]...)

	log.Printf("tool-loop: checkpoint created (id=%s, seq=%d, before=%d messages, after=%d messages, input_tokens=%d)",
		snapshotID, *snapshotSeq, len(*messages), len(newMessages), inputTokenCount)

	*messages = newMessages
	return true
}

// runGuardedToolCalls is runToolCallBatches with the dead-tool guard applied
// inside the turn (B5). A read-only tool called several times in one turn,
// or one that already failed dead-class in an earlier turn, is probed one
// call at a time: after two consecutive "not ready / unavailable" errors
// (deadCounts carries the count from earlier turns) its remaining calls in
// this turn are answered without running; the first success ends the
// probing and the rest run normally. A live run (attempt 6) ran 15 not-ready
// codex_workspace_symbols calls in one turn. deadCounts is not modified:
// the loop counts every result after the turn, as before.
func runGuardedToolCalls(ctx context.Context, executor *Executor, calls []adapter.ToolCall, cache map[string]string, deadCounts map[string]int) []adapter.ToolResult {
	results := make([]adapter.ToolResult, len(calls))
	done := make([]bool, len(calls))
	perTool := map[string][]int{}
	var order []string
	for i, c := range calls {
		if _, ok := perTool[c.Name]; !ok {
			order = append(order, c.Name)
		}
		perTool[c.Name] = append(perTool[c.Name], i)
	}
	for _, name := range order {
		idxs := perTool[name]
		count := deadCounts[name]
		if !isConcurrencySafe(name) || (len(idxs) < 2 && count == 0) {
			continue
		}
		class := "not ready"
		for _, i := range idxs {
			if count >= 2 {
				results[i] = adapter.ToolResult{
					ToolCallID: calls[i].ID,
					Content:    fmt.Sprintf("%s: not run — it failed twice in a row (%s) and is removed from your tools; do not call it again.", name, class),
					IsError:    true,
				}
				done[i] = true
				continue
			}
			r := runToolCallBatches(ctx, executor, calls[i:i+1], cache)[0]
			results[i], done[i] = r, true
			if !r.IsError || !isDeadToolError(r.Content) {
				break // the tool answers: its other calls run below
			}
			class = errorClass(r.Content)
			count++
		}
	}
	var rest []adapter.ToolCall
	var restIdx []int
	for i, c := range calls {
		if !done[i] {
			rest = append(rest, c)
			restIdx = append(restIdx, i)
		}
	}
	for k, r := range runToolCallBatches(ctx, executor, rest, cache) {
		results[restIdx[k]] = r
	}
	return results
}

// toolCallCacheKey returns a dedup key for cacheable read-only tools.
// Returns "" for non-cacheable tools (writes, commands, etc.).
func toolCallCacheKey(call adapter.ToolCall) string {
	switch call.Name {
	case "read_file", "search_code", "list_files":
		return call.Name + ":" + call.Arguments
	}
	if strings.HasPrefix(call.Name, "codex_") {
		return call.Name + ":" + call.Arguments
	}
	return ""
}

// isWriteTool returns true for tools that mutate the filesystem,
// meaning the read-only cache should be invalidated after they run.
func isWriteTool(name string) bool {
	switch name {
	case "edit_file", "write_file", "insert_after", "append_to_file", "run_command", "git_commit", "git_apply_diff":
		return true
	}
	return false
}

// toolCacheAccessHook, when set (tests only), runs on every access to the
// per-loop dedup cache so a test can assert which goroutine touched it.
var toolCacheAccessHook func()

// runToolCallBatches executes a turn's tool calls, parallelizing
// adjacent concurrency-safe calls and serializing the rest. Results
// land in the same index as the input so callers can interleave them
// back into the message history.
//
// cache is a per-RunToolLoop dedup map: read-only tool calls with
// identical name+args return cached results instead of re-executing.
// Any write tool invalidates the entire cache.
//
// The cache is owned by the calling goroutine: it is read, written and
// pruned only here, never inside the per-call goroutines of a parallel
// batch (a concurrent map write is a fatal runtime error that recover()
// cannot catch — a live run, attempt 5, lost research-agent five times
// to it). The goroutines only execute and write their own results[idx].
func runToolCallBatches(ctx context.Context, executor *Executor, calls []adapter.ToolCall, cache map[string]string) []adapter.ToolResult {
	results := make([]adapter.ToolResult, len(calls))
	keys := make([]string, len(calls))
	for i := range calls {
		keys[i] = toolCallCacheKey(calls[i])
	}

	// execute runs one call and touches nothing but results[idx], so it
	// is safe on a batch goroutine.
	execute := func(idx int) {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("tool-executor: panic in %s: %v", calls[idx].Name, r)
				results[idx] = adapter.ToolResult{
					ToolCallID: calls[idx].ID,
					Content:    fmt.Sprintf("internal error executing %s — please try a different approach", calls[idx].Name),
					IsError:    true,
				}
			}
		}()
		results[idx] = executor.Execute(ctx, calls[idx])
	}
	// fromCache answers idx from the cache. Calling goroutine only.
	fromCache := func(idx int) bool {
		if keys[idx] == "" {
			return false
		}
		cached, ok := toolCacheGet(cache, keys[idx])
		if ok {
			results[idx] = adapter.ToolResult{ToolCallID: calls[idx].ID, Content: cached}
			executor.notifyReused(calls[idx], results[idx])
		}
		return ok
	}
	// record stores idx's result and applies write invalidation. Calling
	// goroutine only.
	record := func(idx int) {
		if keys[idx] != "" && !results[idx].IsError {
			toolCachePut(cache, keys[idx], results[idx].Content)
		}
		if isWriteTool(calls[idx].Name) {
			toolCacheClear(cache)
		}
	}

	i := 0
	for i < len(calls) {
		if !isConcurrencySafe(calls[i].Name) {
			if !fromCache(i) {
				execute(i)
				record(i)
			}
			i++
			continue
		}
		// Collect the run of consecutive safe calls.
		j := i
		for j < len(calls) && isConcurrencySafe(calls[j].Name) {
			j++
		}
		// Resolve cache hits and in-batch duplicates before spawning:
		// an identical cacheable call later in the batch reuses the first
		// one's result, as it would have in a serial run.
		var run, dups []int
		dupOf := map[int]int{}
		first := map[string]int{}
		for k := i; k < j; k++ {
			if fromCache(k) {
				continue
			}
			if keys[k] != "" {
				if f, ok := first[keys[k]]; ok {
					dupOf[k] = f
					dups = append(dups, k)
					continue
				}
				first[keys[k]] = k
			}
			run = append(run, k)
		}
		if len(run) == 1 {
			// Single call to run: skip the goroutine overhead.
			execute(run[0])
		} else if len(run) > 1 {
			var wg sync.WaitGroup
			for _, k := range run {
				wg.Add(1)
				go func(k int) {
					defer wg.Done()
					execute(k)
				}(k)
			}
			wg.Wait()
		}
		for _, k := range run {
			record(k)
		}
		// Duplicates, in call order, on the calling goroutine. A duplicate
		// of a successful call shares its result and is reported to the
		// tool callback under its own id (the verifier's grounding and the
		// live stream see every call id). A duplicate of a failed call is
		// not given the error: it is served from the cache when an earlier
		// duplicate succeeded, and executed otherwise.
		for _, k := range dups {
			if f := dupOf[k]; !results[f].IsError {
				r := results[f]
				r.ToolCallID = calls[k].ID
				results[k] = r
				executor.notifyReused(calls[k], r)
				continue
			}
			if !fromCache(k) {
				execute(k)
				record(k)
			}
		}
		i = j
	}
	return results
}

// notifyReused tells the tool callback about a call answered without
// executing it (a cache hit, or an in-batch duplicate sharing another
// call's result), so every call id the model sees in a tool result is
// also observed (the verifier's grounding set, chat chips, the live
// activity stream). Calling goroutine only.
func (e *Executor) notifyReused(call adapter.ToolCall, r adapter.ToolResult) {
	if e == nil || e.onTool == nil {
		return
	}
	e.onTool(call.ID, call.Name, call.Arguments, r.Content, r.IsError, MetaOf(r))
}

// toolCacheGet, toolCachePut and toolCacheClear are the only accessors of
// the per-loop dedup cache; runToolCallBatches calls them from its own
// goroutine only.
func toolCacheGet(cache map[string]string, key string) (string, bool) {
	if toolCacheAccessHook != nil {
		toolCacheAccessHook()
	}
	v, ok := cache[key]
	return v, ok
}

func toolCachePut(cache map[string]string, key, content string) {
	if toolCacheAccessHook != nil {
		toolCacheAccessHook()
	}
	cache[key] = content
}

func toolCacheClear(cache map[string]string) {
	if toolCacheAccessHook != nil {
		toolCacheAccessHook()
	}
	clear(cache)
}

// isConcurrencySafe reports whether a tool is safe to execute in
// parallel with other concurrency-safe tools within the same turn.
// Safe = read-only, no filesystem or shell side effects, no mutation
// of executor-scoped state the next tool might depend on. Conservative
// when uncertain — a false negative costs a tiny bit of latency; a
// false positive would let a write race another call reading the same
// path.
func isConcurrencySafe(toolName string) bool {
	// Pure read-only filesystem + git introspection tools.
	switch toolName {
	case "read_file", "search_code", "list_files", "git_log", "git_diff":
		return true
	}
	// Codex tools are structural lookups against a pre-computed graph +
	// LSP read requests — all safe in parallel. codex_verify_imports is
	// read-only (it checks whether imports resolve; it does not change
	// the file). The full list as of writing: codex_find_module,
	// codex_dependents_of, codex_dependencies_of, codex_workspace_symbols,
	// codex_find_references, codex_patterns_for, codex_similar_code,
	// codex_list_tree, codex_document_symbols, codex_read_file,
	// codex_grep, codex_goto_definition, codex_type_info,
	// codex_diagnostics, codex_verify_imports.
	if strings.HasPrefix(toolName, "codex_") {
		return true
	}
	// write_file, edit_file, run_command, create_branch, commit_and_push,
	// create_pull_request — all side-effectful, must run serially so the
	// model's next tool sees the effect.
	return false
}

// lspLanguageForExt maps a file path's extension to (human-friendly,
// LSP languageId) pair. Empty language means "no LSP support we know
// of" — validator should skip. The languageID is what LSP servers
// expect in didOpen (tsserver wants "typescript"/"typescriptreact",
// gopls wants "go", etc.).
func lspLanguageForExt(relPath string) (string, string) {
	switch strings.ToLower(filepath.Ext(relPath)) {
	case ".go":
		return "go", "go"
	case ".ts":
		return "typescript", "typescript"
	case ".tsx":
		return "typescript", "typescriptreact"
	case ".js":
		return "javascript", "javascript"
	case ".jsx":
		return "javascript", "javascriptreact"
	case ".mjs", ".cjs":
		return "javascript", "javascript"
	case ".py":
		return "python", "python"
	case ".rs":
		return "rust", "rust"
	case ".java":
		return "java", "java"
	case ".cs":
		return "csharp", "csharp"
	case ".cpp", ".cc", ".cxx", ".hpp", ".hxx":
		return "cpp", "cpp"
	case ".c", ".h":
		return "c", "c"
	case ".php":
		return "php", "php"
	case ".html", ".htm":
		return "html", "html"
	case ".css":
		return "css", "css"
	case ".scss":
		return "css", "scss"
	case ".sass":
		return "css", "sass"
	case ".less":
		return "css", "less"
	case ".s", ".asm", ".inc":
		return "assembly", "assembly"
	}
	// `.S` is capital-S assembly (GNU preprocessed). strings.ToLower above
	// folded it to `.s`, so the `.s` case already catches it.
	return "", ""
}

// detectSuspiciousDuplicates returns a non-empty description when the new
// content contains a meaningful line that appears significantly more often
// than in the original — the pattern that produced `import * as z from
// "zod"` thirteen times in PR #9. Meaningful = trimmed length >= 15 chars
// and not pure punctuation. Threshold: any eligible line that went from
// ≤1 occurrences in the original to ≥3 in the new content is suspicious.
func detectSuspiciousDuplicates(oldContent, newContent string) string {
	oldLines := strings.Split(oldContent, "\n")
	oldCount := make(map[string]int, len(oldLines))
	for _, l := range oldLines {
		t := strings.TrimSpace(l)
		if len(t) >= 15 && !isPurePunctuation(t) {
			oldCount[t]++
		}
	}
	newLines := strings.Split(newContent, "\n")
	newCount := make(map[string]int, len(newLines))
	for _, l := range newLines {
		t := strings.TrimSpace(l)
		if len(t) >= 15 && !isPurePunctuation(t) {
			newCount[t]++
		}
	}
	for line, n := range newCount {
		if n >= 3 && oldCount[line] <= 1 {
			// Truncate for the log message; the model doesn't need the
			// full line to figure out what happened.
			preview := line
			if len(preview) > 80 {
				preview = preview[:77] + "..."
			}
			return fmt.Sprintf("Line %q now appears %d times (was %d in original).", preview, n, oldCount[line])
		}
	}
	return ""
}

func isPurePunctuation(s string) bool {
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

// describeMatches returns a human-readable block listing every occurrence
// of needle in content, with the line number of the match and `ctxLines`
// lines of surrounding context. Output is capped to 8 matches to avoid
// flooding the tool result.
func describeMatches(content, needle string, ctxLines int) string {
	lines := strings.Split(content, "\n")
	// Map character offsets back to 1-indexed line numbers.
	lineStart := make([]int, len(lines))
	pos := 0
	for i, l := range lines {
		lineStart[i] = pos
		pos += len(l) + 1 // +1 for the newline
	}
	findLine := func(offset int) int {
		for i := len(lineStart) - 1; i >= 0; i-- {
			if lineStart[i] <= offset {
				return i
			}
		}
		return 0
	}

	var sb strings.Builder
	shown := 0
	search := content
	cursor := 0
	for shown < 8 {
		idx := strings.Index(search, needle)
		if idx < 0 {
			break
		}
		globalIdx := cursor + idx
		lineIdx := findLine(globalIdx)

		fmt.Fprintf(&sb, "Match %d at line %d:\n", shown+1, lineIdx+1)
		start := lineIdx - ctxLines
		if start < 0 {
			start = 0
		}
		end := lineIdx + ctxLines
		if end >= len(lines) {
			end = len(lines) - 1
		}
		for j := start; j <= end; j++ {
			marker := "  "
			if j == lineIdx {
				marker = "> "
			}
			fmt.Fprintf(&sb, "%s%4d: %s\n", marker, j+1, lines[j])
		}
		sb.WriteString("\n")

		// Advance past this match so the next Index call finds the next one.
		cursor = globalIdx + len(needle)
		search = content[cursor:]
		shown++
	}
	return sb.String()
}

// searchCodeMaxHits bounds one search_code result.
const searchCodeMaxHits = 200

// searchCodeMaxFileBytes: larger files are skipped.
const searchCodeMaxFileBytes = 2 << 20

// searchCode searches the executor's tree in Go (B4): it used to shell out
// to grep with GNU's --include, which BusyBox grep in the alpine images
// rejects. Output keeps grep -rn's "path:line:text" shape. The pattern is
// a Go regex; one that does not compile is searched literally. file_glob
// uses matchGlob; a leading "!" excludes. The codex_grep skip list
// (node_modules, .git, dist, …) and binary files are skipped.
func (e *Executor) searchCode(ctx context.Context, callID string, args map[string]interface{}) adapter.ToolResult {
	pattern, _ := args["pattern"].(string)
	glob, _ := args["file_glob"].(string)
	if strings.TrimSpace(pattern) == "" {
		return adapter.ToolResult{ToolCallID: callID, Content: "search_code: pattern is required", IsError: true}
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		re = regexp.MustCompile(regexp.QuoteMeta(pattern))
	}
	glob = strings.TrimSpace(glob)
	exclude := strings.HasPrefix(glob, "!")
	glob = strings.TrimPrefix(glob, "!")

	var out strings.Builder
	hits := 0
	truncated := false
	walkErr := filepath.WalkDir(e.repoPath, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			if _, skip := defaultGrepSkipDirs[d.Name()]; skip && path != e.repoPath {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, relErr := filepath.Rel(e.repoPath, path)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if glob != "" && matchGlob(glob, rel) == exclude {
			return nil
		}
		if info, ierr := d.Info(); ierr != nil || info.Size() > searchCodeMaxFileBytes {
			return nil // bundles, dumps, lockfiles: not source worth reading
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil || looksBinary(data[:min(len(data), 1024)]) {
			return nil
		}
		for i, line := range strings.Split(string(data), "\n") {
			line = strings.TrimRight(line, "\r")
			if !re.MatchString(line) {
				continue
			}
			if len(line) > 300 {
				line = line[:300] + "…"
			}
			fmt.Fprintf(&out, "%s:%d:%s\n", rel, i+1, line)
			hits++
			if hits >= searchCodeMaxHits || out.Len() > 50000 {
				truncated = true
				return fs.SkipAll
			}
		}
		return nil
	})
	if walkErr != nil && walkErr != fs.SkipAll {
		return adapter.ToolResult{ToolCallID: callID, Content: "search_code: " + walkErr.Error(), IsError: true}
	}
	if hits == 0 {
		return adapter.ToolResult{ToolCallID: callID, Content: "No matches found"}
	}
	result := strings.TrimRight(out.String(), "\n")
	if truncated {
		result += fmt.Sprintf("\n... [truncated at %d matches; narrow the pattern or file_glob]", hits)
	}
	return adapter.ToolResult{ToolCallID: callID, Content: result}
}

func (e *Executor) listFiles(callID string, args map[string]interface{}) adapter.ToolResult {
	path, _ := args["path"].(string)
	if path == "" {
		path = "."
	}
	path = e.normalizePath(path)
	fullPath := filepath.Join(e.repoPath, filepath.Clean(path))
	entries, err := os.ReadDir(fullPath)
	if err != nil {
		return adapter.ToolResult{ToolCallID: callID, Content: "Error listing: " + err.Error(), IsError: true}
	}
	var lines []string
	for _, entry := range entries {
		prefix := "  "
		if entry.IsDir() {
			prefix = "d "
		}
		lines = append(lines, prefix+entry.Name())
	}
	return adapter.ToolResult{ToolCallID: callID, Content: strings.Join(lines, "\n")}
}

func (e *Executor) runCommand(ctx context.Context, callID string, args map[string]interface{}) adapter.ToolResult {
	command, _ := args["command"].(string)

	// Permission gate: reject commands that can destroy customer state,
	// exfiltrate data, or escalate privileges before execution. These
	// aren't acceptable from an autonomous agent under any circumstance —
	// even if the model's intent is benign, a malformed arg turns a
	// "clean up build artifacts" request into `rm -rf /`. The allowlist
	// model is too restrictive for a general shell (legitimate commands
	// look infinite) so we run a denylist of patterns that are almost
	// never correct in an automated commit flow. Operators can loosen
	// this later with a per-product rule table.
	reason := dangerousCommandReason(command)
	if reason == "" && e.commandGuard != nil {
		reason = e.commandGuard(command)
	}
	if reason != "" {
		return adapter.ToolResult{
			ToolCallID: callID,
			Content:    "REJECTED: " + reason + ". Rework the command to achieve the intent without this pattern. If you genuinely need this operation, raise it for human review — autonomous agents do not run it.",
			IsError:    true,
			Command:    command,
		}
	}

	timeoutSec := 60
	if t, ok := args["timeout_seconds"].(float64); ok {
		timeoutSec = int(t)
	}
	cmdCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, "sh", "-c", command)
	cmd.Dir = e.repoPath
	started := time.Now()
	out, err := cmd.CombinedOutput()
	durationMs := time.Since(started).Milliseconds()
	result := string(out)
	if err != nil {
		result += "\n[exit code: " + err.Error() + "]"
	}
	// T5: a test runner's output is cut to its failures, totals and last
	// lines unless raw=true.
	raw, _ := args["raw"].(bool)
	result, _ = shapeRunOutput(command, result, raw)
	if len(result) > 50000 {
		result = result[:50000] + "\n... [truncated]"
	}
	return adapter.ToolResult{
		ToolCallID: callID, Content: result,
		ExitCode: commandExitCode(err, cmdCtx.Err()), DurationMs: durationMs, Command: command,
	}
}

// commandExitCode is the exit status of a finished command: 0 on success,
// the process's code when it exited non-zero, nil when it did not exit on
// its own (could not start, killed by a signal or by its timeout).
func commandExitCode(err, ctxErr error) *int {
	if ctxErr != nil {
		return nil // timed out or cancelled: whatever status the kill left is not the command's
	}
	if err == nil {
		code := 0
		return &code
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ProcessState != nil && exitErr.ProcessState.Exited() {
		code := exitErr.ProcessState.ExitCode()
		return &code
	}
	return nil
}

// dangerousCommandReason returns a human-readable rejection reason if
// the command matches a known unsafe pattern, or "" if it's acceptable.
// Patterns are matched case-insensitively against the full shell string;
// they target the commonly-misused destructive + exfiltrating + privilege-
// escalating forms. This is deliberately conservative — the goal is to
// stop the LLM from shipping `rm -rf ~`, `curl | sh`, or `sudo` into a
// customer's repo, not to sandbox arbitrary shell.
func dangerousCommandReason(command string) string {
	lower := strings.ToLower(command)
	// Path-traversing destructive rm. Matches `rm -rf /`, `rm -rf ~`,
	// `rm -fr /etc`, etc. The "tool-loop is running inside a worktree"
	// argument doesn't save us: the agent can `cd /` first.
	if matched := regexp.MustCompile(`(?i)\brm\s+-[rRfF]+[\s]+(/|\~|\*|\$HOME|/\*|\.\.\/)`).FindString(command); matched != "" {
		return "destructive recursive remove on an unsafe path"
	}
	// Piping downloaded scripts into a shell is a classic supply-chain
	// exfil pattern. Matches curl|sh, wget|bash, fetch|zsh, etc.
	if regexp.MustCompile(`(?i)(curl|wget|fetch)\b[^|]*\|\s*(sh|bash|zsh|ash|dash)\b`).MatchString(command) {
		return "piping downloaded content into a shell is not permitted"
	}
	// sudo / su / doas privilege escalation.
	if regexp.MustCompile(`(?i)\b(sudo|su|doas)\b`).MatchString(lower) {
		return "privilege escalation commands (sudo/su/doas) are not permitted"
	}
	// Forkbomb shapes.
	if strings.Contains(command, ":(){:|:&};:") {
		return "forkbomb pattern is not permitted"
	}
	// Disk wipes.
	if regexp.MustCompile(`(?i)\bmkfs\.|\bdd\s+.*\bof=/dev/`).MatchString(command) {
		return "filesystem-wipe commands (mkfs / dd of=/dev/...) are not permitted"
	}
	// Chmod 777 on the whole tree.
	if regexp.MustCompile(`(?i)\bchmod\s+-R\s+777\b`).MatchString(command) {
		return "chmod -R 777 is almost never correct; refuse on safety grounds"
	}
	// git push --force to main/master of the customer remote.
	if regexp.MustCompile(`(?i)\bgit\s+push\s+.*--force[^\s]*\s+origin\s+(main|master)\b`).MatchString(command) {
		return "force-pushing to the customer's default branch is not permitted"
	}
	// Uncommon but real: `> /dev/sda` style writes.
	if regexp.MustCompile(`>\s*/dev/(sd[a-z]|nvme|xvd[a-z])`).MatchString(command) {
		return "raw-device writes are not permitted"
	}
	return ""
}

// optionLikeGitArg reports whether a model-supplied git argument would be
// parsed as an option (leading "-", after optional whitespace).
func optionLikeGitArg(s string) bool {
	return strings.HasPrefix(strings.TrimSpace(s), "-")
}

func rejectOptionLikeGitArg(callID, name, value string) adapter.ToolResult {
	return adapter.ToolResult{ToolCallID: callID, IsError: true,
		Content: fmt.Sprintf("Error: %s %q looks like a git option; pass a commit, branch or path", name, value)}
}

func (e *Executor) gitLog(ctx context.Context, callID string, args map[string]interface{}) adapter.ToolResult {
	maxCommits := 20
	if m, ok := args["max_commits"].(float64); ok {
		maxCommits = int(m)
	}
	cmdArgs := []string{"log", fmt.Sprintf("--max-count=%d", maxCommits), "--oneline", "--no-decorate"}
	if path, ok := args["path"].(string); ok && path != "" {
		if optionLikeGitArg(path) {
			return rejectOptionLikeGitArg(callID, "path", path)
		}
		cmdArgs = append(cmdArgs, "--", path)
	}
	cmd := exec.CommandContext(ctx, "git", cmdArgs...)
	cmd.Dir = e.repoPath
	out, _ := cmd.CombinedOutput()
	return adapter.ToolResult{ToolCallID: callID, Content: string(out)}
}

func (e *Executor) gitDiff(ctx context.Context, callID string, args map[string]interface{}) adapter.ToolResult {
	cmdArgs := []string{"diff"}
	if ref, ok := args["ref"].(string); ok && ref != "" {
		// A model-supplied ref must never be read as an option: e.g.
		// "--output=<path>" makes git diff write a file anywhere the
		// process can write (a hook in another clone of the shared
		// repo-cache). Reject it, and end option parsing before the ref.
		if optionLikeGitArg(ref) {
			return rejectOptionLikeGitArg(callID, "ref", ref)
		}
		cmdArgs = append(cmdArgs, "--end-of-options", ref)
	} else if e.diffBase != "" {
		head := "HEAD"
		if e.diffHead != "" {
			head = e.diffHead
		}
		cmdArgs = append(cmdArgs, "--end-of-options", e.diffBase+"..."+head)
	}
	cmdArgs = append(cmdArgs, "--",
		".", ":(exclude)package-lock.json", ":(exclude)yarn.lock", ":(exclude)pnpm-lock.yaml",
	)
	cmd := exec.CommandContext(ctx, "git", cmdArgs...)
	cmd.Dir = e.repoPath
	out, _ := cmd.CombinedOutput()
	result := string(out)
	if len(result) > 50000 {
		result = result[:50000] + "\n... [truncated]"
	}
	return adapter.ToolResult{ToolCallID: callID, Content: result}
}

func (e *Executor) createBranch(ctx context.Context, callID string, args map[string]interface{}) adapter.ToolResult {
	name, _ := args["name"].(string)
	cmd := exec.CommandContext(ctx, "git", "checkout", "-b", name)
	cmd.Dir = e.repoPath
	out, err := cmd.CombinedOutput()
	if err != nil {
		return adapter.ToolResult{ToolCallID: callID, Content: "Error: " + string(out), IsError: true}
	}
	return adapter.ToolResult{ToolCallID: callID, Content: "Branch created and checked out: " + name}
}

func (e *Executor) commitAndPush(ctx context.Context, callID string, args map[string]interface{}) adapter.ToolResult {
	message, _ := args["message"].(string)

	// Stage all changes
	addCmd := exec.CommandContext(ctx, "git", "add", "-A")
	addCmd.Dir = e.repoPath
	if out, err := addCmd.CombinedOutput(); err != nil {
		return adapter.ToolResult{ToolCallID: callID, Content: "git add failed: " + string(out), IsError: true}
	}

	// Commit
	commitCmd := exec.CommandContext(ctx, "git", "commit", "-m", message)
	commitCmd.Dir = e.repoPath
	if out, err := commitCmd.CombinedOutput(); err != nil {
		return adapter.ToolResult{ToolCallID: callID, Content: "git commit failed: " + string(out), IsError: true}
	}

	// Push
	pushCmd := exec.CommandContext(ctx, "git", "push", "-u", "origin", "HEAD")
	pushCmd.Dir = e.repoPath
	out, err := pushCmd.CombinedOutput()
	if err != nil {
		return adapter.ToolResult{ToolCallID: callID, Content: "git push failed: " + string(out), IsError: true}
	}
	return adapter.ToolResult{ToolCallID: callID, Content: "Committed and pushed: " + message + "\n" + string(out)}
}

// requestUserInput publishes a user_prompt.requested Kafka event and returns
// a ToolResult instructing the model to stop. Task 19 will add the hard
// tool-loop bailout; v1 relies on the model honoring the stop signal in the
// Content string.
func (e *Executor) requestUserInput(ctx context.Context, callID string, args map[string]any) adapter.ToolResult {
	if e.kb == nil || e.workItemID == nil || e.productID == nil {
		return adapter.ToolResult{
			ToolCallID: callID,
			Content:    "request_user_input requires kb + workItemID + productID; this executor was not constructed with the interactions context",
			IsError:    true,
		}
	}
	if e.askingAgent == "" {
		return adapter.ToolResult{
			ToolCallID: callID,
			Content:    "request_user_input requires askingAgent to be set via SetAskingAgent",
			IsError:    true,
		}
	}

	headline, _ := args["headline"].(string)
	question, _ := args["question"].(string)
	promptTypeStr, _ := args["prompt_type"].(string)
	if headline == "" || question == "" {
		return adapter.ToolResult{
			ToolCallID: callID,
			Content:    "request_user_input: headline and question are required",
			IsError:    true,
		}
	}
	switch interactions.PromptType(promptTypeStr) {
	case interactions.PromptTypeClarification,
		interactions.PromptTypeEscalation,
		interactions.PromptTypeBlockQuestion:
		// OK
	default:
		return adapter.ToolResult{
			ToolCallID: callID,
			Content:    fmt.Sprintf("request_user_input: prompt_type must be clarification | escalation | block_question, got %q", promptTypeStr),
			IsError:    true,
		}
	}

	// Parse actions — each must have id + label + semantic.
	var actions []interactions.Action
	if rawActions, ok := args["actions"].([]any); ok {
		for _, raw := range rawActions {
			m, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			a := interactions.Action{
				ID:       stringMapField(m, "id"),
				Label:    stringMapField(m, "label"),
				Semantic: interactions.Semantic(stringMapField(m, "semantic")),
			}
			if a.ID != "" && a.Label != "" {
				actions = append(actions, a)
			}
		}
	}

	var contextPayload map[string]any
	if cp, ok := args["context_payload"].(map[string]any); ok {
		contextPayload = cp
	}

	prompt := &interactions.Prompt{
		ID:             uuid.New(),
		WorkItemID:     *e.workItemID,
		ProductID:      *e.productID,
		AskingAgent:    e.askingAgent,
		Trigger:        interactions.TriggerAgentRequest,
		PromptType:     interactions.PromptType(promptTypeStr),
		Headline:       headline,
		Question:       question,
		ContextPayload: contextPayload,
		Actions:        actions,
		State:          interactions.StatePending,
		RequestedAt:    time.Now().UTC(),
		PromptedVia:    []string{"dashboard"},
	}
	pub := interactions.NewPublisher(e.kb)
	if err := pub.PublishRequested(ctx, prompt); err != nil {
		return adapter.ToolResult{
			ToolCallID: callID,
			Content:    fmt.Sprintf("request_user_input: publish failed: %v", err),
			IsError:    true,
		}
	}

	// Persist resumption state (best-effort). The caller threads in the
	// current message-history snapshot via SetResumptionSnapshot right
	// before RunToolLoop fires; if nothing's been threaded in, we persist
	// an empty history — the downstream agent resume logic still has the
	// prompt_id → work_item_id mapping from the user_interactions table.
	if e.resumption != nil {
		history := e.resumptionSnapshot // may be nil
		if history == nil {
			history = []byte("[]")
		}
		if err := e.resumption.PersistResumption(ctx, &interactions.ResumptionState{
			PromptID:       prompt.ID,
			WorkItemID:     *e.workItemID,
			AskingAgent:    e.askingAgent,
			MessageHistory: history,
			ToolCallStack:  []byte("[]"),
		}); err != nil {
			// Log but don't fail the tool call — the prompt is already
			// published; losing resumption means lossy resume, not broken
			// state. Agents can still resume via past_decisions_query.
			log.Printf("request_user_input: persist resumption failed: %v", err)
		}
	}
	e.notifyPrompt(prompt.ID, interactions.TriggerAgentRequest)

	return adapter.ToolResult{
		ToolCallID: callID,
		Content:    fmt.Sprintf("User input requested (prompt id %s). Work item is now awaiting user answer. STOP emitting tool calls this turn — you will be resumed with the answer.", prompt.ID),
	}
}

// stringMapField extracts a string-typed field from a map; returns "" if missing.
func stringMapField(m map[string]any, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// pastDecisionsQuery searches the per-product user_decisions qdrant collection
// and returns matching past decisions as JSON. Reads metadata populated by
// knowledge-indexer's UserDecisionStore (Task 13).
func (e *Executor) pastDecisionsQuery(ctx context.Context, callID string, args map[string]any) adapter.ToolResult {
	if e.qdrantEndpoint == "" || e.embeddingSvc == nil {
		return adapter.ToolResult{
			ToolCallID: callID,
			Content:    "past_decisions_query: qdrant endpoint + embedding service not configured on this executor; call SetQdrantConfig at agent startup",
			IsError:    true,
		}
	}
	if e.productID == nil {
		return adapter.ToolResult{
			ToolCallID: callID,
			Content:    "past_decisions_query: productID not set on executor",
			IsError:    true,
		}
	}

	query, _ := args["query"].(string)
	scope, _ := args["scope"].(string)
	if query == "" {
		return adapter.ToolResult{
			ToolCallID: callID,
			Content:    "past_decisions_query: query is required",
			IsError:    true,
		}
	}
	limit := 10
	if v, ok := args["limit"].(float64); ok && v > 0 && v <= 50 {
		limit = int(v)
	}

	filters := map[string]string{"revoked": "false"}
	switch scope {
	case "product":
		// no extra filter — the collection is already per-product
	case "work_item":
		if e.workItemID == nil {
			return adapter.ToolResult{
				ToolCallID: callID,
				Content:    "past_decisions_query: scope=work_item requires workItemID on executor",
				IsError:    true,
			}
		}
		filters["work_item_id"] = e.workItemID.String()
	case "global":
		return adapter.ToolResult{
			ToolCallID: callID,
			Content:    "past_decisions_query: scope=global not supported in v1",
			IsError:    true,
		}
	default:
		return adapter.ToolResult{
			ToolCallID: callID,
			Content:    fmt.Sprintf("past_decisions_query: invalid scope %q", scope),
			IsError:    true,
		}
	}

	collection := fmt.Sprintf("product_%s_user_decisions", e.productID.String())
	vs := vector.NewWithEmbeddings(e.qdrantEndpoint, collection, e.embeddingSvc)
	hits, err := vs.SearchByText(ctx, query, filters, limit)
	if err != nil {
		return adapter.ToolResult{
			ToolCallID: callID,
			Content:    fmt.Sprintf("past_decisions_query: search failed: %v", err),
			IsError:    true,
		}
	}

	out := make([]interactions.Decision, 0, len(hits))
	for _, h := range hits {
		wiID, _ := uuid.Parse(h.Metadata["work_item_id"])
		ansAt, _ := time.Parse(time.RFC3339, h.Metadata["answered_at"])
		out = append(out, interactions.Decision{
			Question:         h.Metadata["question"],
			Answer:           h.Metadata["answer"],
			ClassifiedAs:     interactions.ClassifiedAs(h.Metadata["classified_as"]),
			SourceWorkItemID: wiID,
			AnsweredAt:       ansAt,
		})
	}
	b, err := json.Marshal(out)
	if err != nil {
		return adapter.ToolResult{
			ToolCallID: callID,
			Content:    fmt.Sprintf("past_decisions_query: marshal: %v", err),
			IsError:    true,
		}
	}
	return adapter.ToolResult{ToolCallID: callID, Content: string(b)}
}

// recallContext retrieves a conversation checkpoint from the database.
func (e *Executor) recallContext(ctx context.Context, callID string, args map[string]any) adapter.ToolResult {
	if e.db == nil {
		return adapter.ToolResult{
			ToolCallID: callID,
			Content:    "recall_context: database not available on this executor",
			IsError:    true,
		}
	}
	snapshotID := stringMapField(args, "snapshot_id")
	if snapshotID == "" {
		return adapter.ToolResult{
			ToolCallID: callID,
			Content:    "recall_context: snapshot_id is required",
			IsError:    true,
		}
	}
	sid, err := uuid.Parse(snapshotID)
	if err != nil {
		return adapter.ToolResult{
			ToolCallID: callID,
			Content:    "recall_context: invalid snapshot_id: " + err.Error(),
			IsError:    true,
		}
	}
	var messagesJSON []byte
	var summary string
	err = e.db.QueryRow(ctx,
		`SELECT messages, summary FROM conversation_snapshots WHERE id = $1`, sid,
	).Scan(&messagesJSON, &summary)
	if err != nil {
		return adapter.ToolResult{
			ToolCallID: callID,
			Content:    "recall_context: snapshot not found: " + err.Error(),
			IsError:    true,
		}
	}
	var messages []adapter.ChatMessage
	if err := json.Unmarshal(messagesJSON, &messages); err != nil {
		return adapter.ToolResult{
			ToolCallID: callID,
			Content:    "recall_context: failed to parse stored messages: " + err.Error(),
			IsError:    true,
		}
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("=== Recalled Checkpoint %s (%d messages) ===\n\n", sid, len(messages)))
	for _, msg := range messages {
		if msg.Role == adapter.RoleSystem {
			continue
		}
		switch msg.Role {
		case adapter.RoleAssistant:
			content := msg.Content
			if len(content) > 500 {
				content = content[:500] + "…"
			}
			sb.WriteString("ASSISTANT: " + content + "\n\n")
		case adapter.RoleTool:
			content := msg.Content
			if len(content) > 1000 {
				content = content[:1000] + fmt.Sprintf("… [%d bytes truncated]", len(msg.Content)-1000)
			}
			sb.WriteString(fmt.Sprintf("TOOL(%s): %s\n\n", msg.Name, content))
		case adapter.RoleUser:
			content := msg.Content
			if len(content) > 300 {
				content = content[:300] + "…"
			}
			sb.WriteString("USER: " + content + "\n\n")
		}
	}
	return adapter.ToolResult{ToolCallID: callID, Content: sb.String()}
}

// ─── Codex-backed tools ──────────────────────────────────────────────────────

// codexMissingResult is the standard "codex not wired" response. The LLM
// sees the error and falls back to grep, per the codex-first strategy in
// the system prompt.
func (e *Executor) codexMissingResult(callID, tool string) adapter.ToolResult {
	return adapter.ToolResult{
		ToolCallID: callID,
		Content: fmt.Sprintf("%s unavailable: the codex is not configured for this executor. "+
			"Fall back to search_code / read_file for this query.", tool),
		IsError: true,
	}
}

// codexFindModule returns modules whose name or purpose matches the given
// pattern. Scoped to the executor's product_id on that product's configured
// default branch (read from repositories.base_branch — no "main" guess).
func (e *Executor) codexFindModule(ctx context.Context, callID string, args map[string]interface{}) adapter.ToolResult {
	if e.codex == nil || e.productID == nil {
		return e.codexMissingResult(callID, "codex_find_module")
	}
	pattern, _ := args["name_or_pattern"].(string)
	if strings.TrimSpace(pattern) == "" {
		return adapter.ToolResult{ToolCallID: callID, Content: "name_or_pattern is required", IsError: true}
	}

	modules, err := e.codex.GetModulesForProduct(ctx, *e.productID)
	if err != nil {
		return adapter.ToolResult{ToolCallID: callID, Content: "codex_find_module: " + err.Error(), IsError: true}
	}

	needle := strings.ToLower(pattern)
	type moduleHit struct {
		Name            string          `json:"name"`
		Purpose         string          `json:"purpose"`
		PublicInterface json.RawMessage `json:"public_interface"`
		Dependencies    []string        `json:"dependencies"`
		Dependents      []string        `json:"dependents"`
		RiskLevel       string          `json:"risk_level"`
	}
	hits := []moduleHit{}
	for _, m := range modules {
		if strings.Contains(strings.ToLower(m.Name), needle) ||
			strings.Contains(strings.ToLower(m.Purpose), needle) {
			hits = append(hits, moduleHit{
				Name:            m.Name,
				Purpose:         m.Purpose,
				PublicInterface: m.PublicInterface,
				Dependencies:    m.Dependencies,
				Dependents:      m.Dependents,
				RiskLevel:       m.RiskLevel,
			})
		}
	}

	payload := map[string]interface{}{"modules": hits, "count": len(hits), "query": pattern}
	data, _ := json.MarshalIndent(payload, "", "  ")
	if len(hits) == 0 {
		e.markCodexEmpty("codex_find_module", pattern)
		return adapter.ToolResult{ToolCallID: callID, Content: string(data)}
	}
	return adapter.ToolResult{ToolCallID: callID, Content: string(data)}
}

// codexDependentsOf returns the list of modules that depend on the named module.
func (e *Executor) codexDependentsOf(ctx context.Context, callID string, args map[string]interface{}) adapter.ToolResult {
	if e.codex == nil || e.productID == nil {
		return e.codexMissingResult(callID, "codex_dependents_of")
	}
	name, _ := args["module_name"].(string)
	if strings.TrimSpace(name) == "" {
		return adapter.ToolResult{ToolCallID: callID, Content: "module_name is required", IsError: true}
	}

	modules, err := e.codex.GetModulesForProduct(ctx, *e.productID)
	if err != nil {
		return adapter.ToolResult{ToolCallID: callID, Content: "codex_dependents_of: " + err.Error(), IsError: true}
	}
	var target *codexModuleView
	for _, m := range modules {
		if m.Name == name {
			target = &codexModuleView{Name: m.Name, Dependents: m.Dependents, Dependencies: m.Dependencies}
			break
		}
	}
	if target == nil {
		e.markCodexEmpty("codex_dependents_of", name)
		data, _ := json.Marshal(map[string]interface{}{"module": name, "dependents": []string{}, "error": "module not found in codex"})
		return adapter.ToolResult{ToolCallID: callID, Content: string(data)}
	}
	if len(target.Dependents) == 0 {
		e.markCodexEmpty("codex_dependents_of", name)
	}
	data, _ := json.MarshalIndent(map[string]interface{}{"module": name, "dependents": target.Dependents}, "", "  ")
	return adapter.ToolResult{ToolCallID: callID, Content: string(data)}
}

// codexDependenciesOf returns the forward dependencies of the named module.
func (e *Executor) codexDependenciesOf(ctx context.Context, callID string, args map[string]interface{}) adapter.ToolResult {
	if e.codex == nil || e.productID == nil {
		return e.codexMissingResult(callID, "codex_dependencies_of")
	}
	name, _ := args["module_name"].(string)
	if strings.TrimSpace(name) == "" {
		return adapter.ToolResult{ToolCallID: callID, Content: "module_name is required", IsError: true}
	}

	modules, err := e.codex.GetModulesForProduct(ctx, *e.productID)
	if err != nil {
		return adapter.ToolResult{ToolCallID: callID, Content: "codex_dependencies_of: " + err.Error(), IsError: true}
	}
	var target *codexModuleView
	for _, m := range modules {
		if m.Name == name {
			target = &codexModuleView{Name: m.Name, Dependencies: m.Dependencies, Dependents: m.Dependents}
			break
		}
	}
	if target == nil {
		e.markCodexEmpty("codex_dependencies_of", name)
		data, _ := json.Marshal(map[string]interface{}{"module": name, "dependencies": []string{}, "error": "module not found in codex"})
		return adapter.ToolResult{ToolCallID: callID, Content: string(data)}
	}
	if len(target.Dependencies) == 0 {
		e.markCodexEmpty("codex_dependencies_of", name)
	}
	data, _ := json.MarshalIndent(map[string]interface{}{"module": name, "dependencies": target.Dependencies}, "", "  ")
	return adapter.ToolResult{ToolCallID: callID, Content: string(data)}
}

// codexSimilarCode runs a semantic-similarity search over the Qdrant index
// using the same embedding service the knowledge-indexer wrote with.
func (e *Executor) codexSimilarCode(ctx context.Context, callID string, args map[string]interface{}) adapter.ToolResult {
	if e.vectors == nil {
		return adapter.ToolResult{
			ToolCallID: callID,
			Content: "codex_similar_code unavailable: vector store is not configured. " +
				"Fall back to search_code for this query.",
			IsError: true,
		}
	}
	desc, _ := args["description"].(string)
	if strings.TrimSpace(desc) == "" {
		return adapter.ToolResult{ToolCallID: callID, Content: "description is required", IsError: true}
	}
	limit := 5
	if l, ok := args["limit"].(float64); ok && int(l) > 0 {
		limit = int(l)
		if limit > 20 {
			limit = 20
		}
	}

	// Prefer SearchByText — it embeds with the configured EmbeddingService,
	// which is the same service the knowledge-indexer used when writing.
	// If the VectorStore has no embedding service the call returns a
	// descriptive error and the LLM falls back to grep.
	//
	// CRITICAL: scope the search to this product's embeddings so we never
	// leak (or contaminate benchmarks with) another product's code. The
	// knowledge-indexer tags every Qdrant point with payload.product_id;
	// we filter on it here. When productID is nil (e.g. legacy single-repo
	// deployments) we fall back to an unfiltered search so existing
	// installations don't silently break.
	var filters map[string]string
	if e.productID != nil {
		filters = map[string]string{"product_id": e.productID.String()}
	}
	results, err := e.vectors.SearchByText(ctx, desc, filters, limit)
	if err != nil {
		return adapter.ToolResult{
			ToolCallID: callID,
			Content: "codex_similar_code: " + err.Error() +
				" — fall back to search_code.",
			IsError: true,
		}
	}

	type hit struct {
		File     string  `json:"file"`
		Function string  `json:"function"`
		Package  string  `json:"package"`
		Line     string  `json:"line"`
		Score    float32 `json:"score"`
	}
	hits := make([]hit, 0, len(results))
	for _, r := range results {
		hits = append(hits, hit{
			File:     r.Metadata["file"],
			Function: r.Metadata["function"],
			Package:  r.Metadata["package"],
			Line:     r.Metadata["line"],
			Score:    r.Score,
		})
	}
	if len(hits) == 0 {
		e.markCodexEmpty("codex_similar_code", desc)
	}
	data, _ := json.MarshalIndent(map[string]interface{}{"results": hits, "count": len(hits)}, "", "  ")
	return adapter.ToolResult{ToolCallID: callID, Content: string(data)}
}

// codexPatternsFor returns patterns and anti-patterns matching the category
// or keyword. Uses simple ILIKE substring match over name/category/when_to_use.
func (e *Executor) codexPatternsFor(ctx context.Context, callID string, args map[string]interface{}) adapter.ToolResult {
	if e.codex == nil || e.productID == nil {
		return e.codexMissingResult(callID, "codex_patterns_for")
	}
	kw, _ := args["category_or_keyword"].(string)
	if strings.TrimSpace(kw) == "" {
		return adapter.ToolResult{ToolCallID: callID, Content: "category_or_keyword is required", IsError: true}
	}

	patterns, err := e.codex.GetPatternsForProduct(ctx, *e.productID)
	if err != nil {
		return adapter.ToolResult{ToolCallID: callID, Content: "codex_patterns_for (patterns): " + err.Error(), IsError: true}
	}
	antiPatterns, err := e.codex.GetAntiPatternsForProduct(ctx, *e.productID)
	if err != nil {
		return adapter.ToolResult{ToolCallID: callID, Content: "codex_patterns_for (anti-patterns): " + err.Error(), IsError: true}
	}

	needle := strings.ToLower(kw)

	type patternHit struct {
		Name      string   `json:"name"`
		Category  string   `json:"category"`
		WhenToUse string   `json:"when_to_use"`
		Rationale string   `json:"rationale"`
		KeyRules  []string `json:"key_rules"`
	}
	type antiHit struct {
		Name        string `json:"name"`
		WhyWrong    string `json:"why_wrong"`
		WhatInstead string `json:"what_instead"`
	}

	pHits := []patternHit{}
	for _, p := range patterns {
		if strings.Contains(strings.ToLower(p.Name), needle) ||
			strings.Contains(strings.ToLower(p.Category), needle) ||
			strings.Contains(strings.ToLower(p.WhenToUse), needle) {
			pHits = append(pHits, patternHit{
				Name:      p.Name,
				Category:  p.Category,
				WhenToUse: p.WhenToUse,
				Rationale: p.Rationale,
				KeyRules:  p.KeyRules,
			})
		}
	}
	aHits := []antiHit{}
	for _, ap := range antiPatterns {
		if strings.Contains(strings.ToLower(ap.Name), needle) ||
			strings.Contains(strings.ToLower(ap.WhyWrong), needle) ||
			strings.Contains(strings.ToLower(ap.WhatInstead), needle) {
			aHits = append(aHits, antiHit{
				Name:        ap.Name,
				WhyWrong:    ap.WhyWrong,
				WhatInstead: ap.WhatInstead,
			})
		}
	}

	if len(pHits) == 0 && len(aHits) == 0 {
		e.markCodexEmpty("codex_patterns_for", kw)
	}
	payload := map[string]interface{}{
		"query":         kw,
		"patterns":      pHits,
		"anti_patterns": aHits,
	}
	data, _ := json.MarshalIndent(payload, "", "  ")
	return adapter.ToolResult{ToolCallID: callID, Content: string(data)}
}

// codexModuleView is a tiny local view of CodexModule used for the
// dependents / dependencies lookups. We avoid pulling models.CodexModule
// into the dispatch layer to keep the coupling minimal.
type codexModuleView struct {
	Name         string
	Dependencies []string
	Dependents   []string
}

// ─── codex_read_file / codex_grep ────────────────────────────────────────────
//
// These are the fallback primitives. codex_similar_code returns metadata
// (file / function / line / score) — those are pointers, not contents.
// When a model needs to implement against existing code it has to SEE
// the code; codex_read_file is that bridge. codex_grep is the last
// resort when semantic search misses entirely. Both are hard-scoped to
// the executor's productID — they cannot escape the repo-cache root for
// that product, and they refuse path traversal.

// repoCacheRoot returns the filesystem root for the requested repo (or
// the default enabled repo when repoName is empty). Resolution order:
//
//  1. If the repository row has a non-empty clone_path, use it.
//  2. Otherwise fall back to /repo-cache/<productID>/<name>/, which is
//     the onboarding default.
//
// Returned path is cleaned and guaranteed to exist as a directory.
func (e *Executor) repoCacheRoot(ctx context.Context, repoName string) (string, string, error) {
	if e.codex == nil || e.productID == nil {
		return "", "", fmt.Errorf("codex not configured — cannot resolve repo cache root")
	}
	repos, err := e.codex.ListEnabledRepos(ctx, *e.productID)
	if err != nil {
		return "", "", fmt.Errorf("list repos: %w", err)
	}
	if len(repos) == 0 {
		return "", "", fmt.Errorf("no enabled repositories for product %s", e.productID.String())
	}
	var chosen *codex.RepoEntry
	if strings.TrimSpace(repoName) == "" {
		chosen = &repos[0]
	} else {
		for i := range repos {
			if repos[i].Name == repoName {
				chosen = &repos[i]
				break
			}
		}
		if chosen == nil {
			names := make([]string, 0, len(repos))
			for _, r := range repos {
				names = append(names, r.Name)
			}
			return "", "", fmt.Errorf("repo %q not found for this product; available: %s", repoName, strings.Join(names, ", "))
		}
	}
	root := strings.TrimSpace(chosen.ClonePath)
	if root == "" {
		root = fmt.Sprintf("/repo-cache/%s/%s/", e.productID.String(), chosen.Name)
	}
	root = filepath.Clean(root)
	info, err := os.Stat(root)
	if err != nil {
		return "", chosen.Name, fmt.Errorf("repo-cache root %q not accessible: %w — is the repo-cache volume mounted on this service?", root, err)
	}
	if !info.IsDir() {
		return "", chosen.Name, fmt.Errorf("repo-cache root %q is not a directory", root)
	}
	return root, chosen.Name, nil
}

// validateRepoRelPath rejects absolute paths, parent-directory traversal,
// and .git/ references. Returns the cleaned relative path on success.
func validateRepoRelPath(rel string) (string, error) {
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return "", fmt.Errorf("path is required")
	}
	// Reject absolute paths (Unix root, Windows drive, or leading backslash).
	if strings.HasPrefix(rel, "/") || strings.HasPrefix(rel, `\`) ||
		(len(rel) >= 2 && rel[1] == ':') {
		return "", fmt.Errorf("absolute paths are not allowed; provide a repo-relative path")
	}
	// Normalise separators and reject any ".." segment.
	cleaned := filepath.ToSlash(filepath.Clean(rel))
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.Contains(cleaned, "/../") {
		return "", fmt.Errorf("path traversal ('..') is not allowed")
	}
	// Reject references to the .git directory — it's not code and often
	// contains binary pack files.
	lower := strings.ToLower(cleaned)
	if lower == ".git" || strings.HasPrefix(lower, ".git/") || strings.Contains(lower, "/.git/") {
		return "", fmt.Errorf(".git/ is not readable via this tool")
	}
	return cleaned, nil
}

// looksBinary reports whether the first 1 KiB contains a NUL byte. That
// is the classic heuristic git uses and it works well for source trees.
func looksBinary(buf []byte) bool {
	return bytes.IndexByte(buf, 0) >= 0
}

// codexReadFile reads a slice of a file from the productID's repo-cache.
// Returns JSON { path, start_line, end_line, total_lines, content }.
func (e *Executor) codexReadFile(ctx context.Context, callID string, args map[string]interface{}) adapter.ToolResult {
	if !e.codexReadsAvailable() {
		return e.codexMissingResult(callID, "codex_read_file")
	}
	rawPath, _ := args["path"].(string)
	repoName, _ := args["repo"].(string)
	rel, err := validateRepoRelPath(rawPath)
	if err != nil {
		return adapter.ToolResult{ToolCallID: callID, Content: "codex_read_file: " + err.Error(), IsError: true}
	}

	root, resolvedRepo, err := e.codexReadRoot(ctx, repoName)
	if err != nil {
		return adapter.ToolResult{ToolCallID: callID, Content: "codex_read_file: " + err.Error(), IsError: true}
	}

	full := filepath.Join(root, filepath.FromSlash(rel))
	// Belt-and-suspenders: after Join, confirm we're still under root.
	// (filepath.Clean inside Join normalises any "foo/../.." we missed.)
	absFull, _ := filepath.Abs(full)
	absRoot, _ := filepath.Abs(root)
	if !strings.HasPrefix(absFull+string(filepath.Separator), absRoot+string(filepath.Separator)) && absFull != absRoot {
		return adapter.ToolResult{ToolCallID: callID, Content: "codex_read_file: resolved path escapes repo root", IsError: true}
	}

	f, err := os.Open(full)
	if err != nil {
		// Same observation semantics as read_file: the model now knows
		// the path's on-disk state (missing / inaccessible). That's
		// enough to satisfy the write_file gate if the intent was to
		// create a new file at this location — though it should also
		// search_code first to make sure nothing else already covers
		// the concern.
		e.markObserved(rel)
		return adapter.ToolResult{
			ToolCallID: callID,
			Content:    fmt.Sprintf("codex_read_file: cannot open %q (repo=%s): %v", rel, resolvedRepo, err),
			IsError:    true,
		}
	}
	defer f.Close() //nolint:errcheck
	e.markObserved(rel)

	// Binary sniff on the first 1 KiB.
	sniff := make([]byte, 1024)
	n, _ := f.Read(sniff)
	if looksBinary(sniff[:n]) {
		return adapter.ToolResult{
			ToolCallID: callID,
			Content:    fmt.Sprintf("codex_read_file: %q appears to be binary; use codex_grep instead", rel),
			IsError:    true,
		}
	}
	if _, err := f.Seek(0, 0); err != nil {
		return adapter.ToolResult{ToolCallID: callID, Content: "codex_read_file: seek: " + err.Error(), IsError: true}
	}

	seedNote := ""
	if whole, rerr := os.ReadFile(full); rerr == nil {
		if window, n, ok := e.defaultWindow(rel, args, fileLinesOf(strings.ReplaceAll(string(whole), "\r\n", "\n"))); ok {
			args, seedNote = window, n
		}
	}
	startLine := 1
	if v, ok := args["start_line"].(float64); ok && int(v) > 0 {
		startLine = int(v)
	}
	endLine := 0
	if v, ok := args["end_line"].(float64); ok && int(v) > 0 {
		endLine = int(v)
	}
	const hardCap = readMaxLines
	if endLine == 0 || endLine < startLine {
		endLine = startLine + readDefaultLines - 1 // T6
	}
	// Enforce the hard cap regardless of what the caller asked for.
	if endLine-startLine+1 > hardCap {
		endLine = startLine + hardCap - 1
	}

	var picked []string
	scanner := bufio.NewScanner(f)
	// Allow for long minified lines (~1 MiB) without panicking.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNo := 0
	total := 0
	for scanner.Scan() {
		lineNo++
		total++
		if lineNo >= startLine && lineNo <= endLine {
			picked = append(picked, scanner.Text())
		}
	}
	if err := scanner.Err(); err != nil {
		return adapter.ToolResult{ToolCallID: callID, Content: "codex_read_file: scan: " + err.Error(), IsError: true}
	}
	// Recompute actual end based on total lines — makes the response honest
	// when the caller asked for past-EOF ranges.
	actualEnd := endLine
	if actualEnd > total {
		actualEnd = total
	}
	if startLine > total {
		// Empty slice — return empty content but include the totals so the
		// caller knows the file is shorter than they assumed.
		payload := map[string]interface{}{
			"path":        rel,
			"repo":        resolvedRepo,
			"start_line":  startLine,
			"end_line":    startLine - 1,
			"total_lines": total,
			"content":     "",
		}
		data, _ := json.MarshalIndent(payload, "", "  ")
		return adapter.ToolResult{ToolCallID: callID, Content: string(data)}
	}

	payload := map[string]interface{}{
		"path":        rel,
		"repo":        resolvedRepo,
		"start_line":  startLine,
		"end_line":    actualEnd,
		"total_lines": total,
		"content":     strings.Join(picked, "\n"),
	}
	if actualEnd < total {
		payload["note"] = fmt.Sprintf("lines %d-%d of %d; pass start_line=%d for more", startLine, actualEnd, total, actualEnd+1)
	}
	if seedNote != "" {
		payload["note"] = seedNote
	}
	if root == e.repoPath {
		// The executor's own tree: write_file's full-read check counts it.
		e.markRead(rel, startLine, actualEnd, total)
	}
	data, _ := json.MarshalIndent(payload, "", "  ")
	return adapter.ToolResult{ToolCallID: callID, Content: string(data)}
}

// defaultGrepSkipDirs lists directory names that are never worth walking
// for source search. Match on the BASENAME of each directory entry.
var defaultGrepSkipDirs = map[string]struct{}{
	".git":         {},
	"node_modules": {},
	".next":        {},
	"dist":         {},
	"build":        {},
	"vendor":       {},
	".venv":        {},
	"__pycache__":  {},
}

// matchGlob does a best-effort ** / * match against a forward-slash path.
// Supports:
//   - "**/*.ext" (recursive extension match)
//   - "dir/**" (recursive under a prefix)
//   - "dir/**/*.ext"
//   - plain filepath.Match patterns ("*.tsx", "app/api/*/route.ts")
//
// Empty pattern matches everything.
func matchGlob(pattern, rel string) bool {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return true
	}
	rel = filepath.ToSlash(rel)
	// Fast path: leading "**/" means match on tail only.
	if strings.HasPrefix(pattern, "**/") {
		tail := pattern[3:]
		if ok, _ := filepath.Match(tail, filepath.Base(rel)); ok {
			return true
		}
		// Also allow tail to match any suffix (e.g. "**/*.tsx" against "a/b/c.tsx")
		for i := 0; i < len(rel); i++ {
			if rel[i] == '/' {
				if ok, _ := filepath.Match(tail, rel[i+1:]); ok {
					return true
				}
			}
		}
		return false
	}
	// Prefix with "/**" tail: match any path under the prefix.
	if idx := strings.Index(pattern, "/**"); idx >= 0 {
		prefix := pattern[:idx]
		suffix := strings.TrimPrefix(pattern[idx+3:], "/")
		if !strings.HasPrefix(rel, prefix+"/") && rel != prefix {
			return false
		}
		if suffix == "" {
			return true
		}
		// Try the suffix against basename and any tail.
		for i := 0; i < len(rel); i++ {
			if rel[i] == '/' {
				if ok, _ := filepath.Match(suffix, rel[i+1:]); ok {
					return true
				}
			}
		}
		if ok, _ := filepath.Match(suffix, filepath.Base(rel)); ok {
			return true
		}
		return false
	}
	// Plain glob.
	ok, _ := filepath.Match(pattern, rel)
	if ok {
		return true
	}
	// Also allow matching against the basename when the pattern has no "/".
	if !strings.Contains(pattern, "/") {
		if ok, _ := filepath.Match(pattern, filepath.Base(rel)); ok {
			return true
		}
	}
	return false
}

// codexGrep walks the repo-cache root for the productID and collects
// regex matches. Deliberately bounded: caps hits at 200, skips common
// noise dirs, and truncates match text to 200 chars.
func (e *Executor) codexGrep(ctx context.Context, callID string, args map[string]interface{}) adapter.ToolResult {
	if !e.codexReadsAvailable() {
		return e.codexMissingResult(callID, "codex_grep")
	}
	pattern, _ := args["pattern"].(string)
	if strings.TrimSpace(pattern) == "" {
		return adapter.ToolResult{ToolCallID: callID, Content: "codex_grep: pattern is required", IsError: true}
	}
	glob, _ := args["glob"].(string)
	repoName, _ := args["repo"].(string)
	caseInsensitive, _ := args["case_insensitive"].(bool)
	maxHits := 50
	if v, ok := args["max_hits"].(float64); ok && int(v) > 0 {
		maxHits = int(v)
	}
	if maxHits > 200 {
		maxHits = 200
	}

	rx := pattern
	if caseInsensitive && !strings.HasPrefix(rx, "(?i)") {
		rx = "(?i)" + rx
	}
	re, err := regexp.Compile(rx)
	if err != nil {
		return adapter.ToolResult{
			ToolCallID: callID,
			Content:    fmt.Sprintf("codex_grep: invalid regex %q: %v", pattern, err),
			IsError:    true,
		}
	}

	root, resolvedRepo, err := e.codexReadRoot(ctx, repoName)
	if err != nil {
		return adapter.ToolResult{ToolCallID: callID, Content: "codex_grep: " + err.Error(), IsError: true}
	}

	type hit struct {
		File string `json:"file"`
		Line int    `json:"line"`
		Text string `json:"text"`
	}
	hits := make([]hit, 0, maxHits)
	truncated := false

	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			// Skip unreadable entries without aborting the whole walk.
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if len(hits) >= maxHits {
			truncated = true
			return fs.SkipAll
		}
		name := d.Name()
		if d.IsDir() {
			if _, skip := defaultGrepSkipDirs[name]; skip {
				return fs.SkipDir
			}
			return nil
		}
		// Relative path (forward-slash) for glob + reporting.
		relPath, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		relPath = filepath.ToSlash(relPath)
		if !matchGlob(glob, relPath) {
			return nil
		}

		f, openErr := os.Open(path)
		if openErr != nil {
			return nil
		}
		defer f.Close() //nolint:errcheck

		// Binary sniff: skip files with a NUL in the first 1 KiB.
		sniff := make([]byte, 1024)
		n, _ := f.Read(sniff)
		if looksBinary(sniff[:n]) {
			return nil
		}
		if _, err := f.Seek(0, 0); err != nil {
			return nil
		}

		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		lineNo := 0
		for scanner.Scan() {
			lineNo++
			line := scanner.Text()
			if !re.MatchString(line) {
				continue
			}
			if len(line) > 200 {
				line = line[:200] + "…"
			}
			hits = append(hits, hit{File: relPath, Line: lineNo, Text: line})
			if len(hits) >= maxHits {
				truncated = true
				return fs.SkipAll
			}
		}
		return nil
	})
	if walkErr != nil && walkErr != fs.SkipAll {
		return adapter.ToolResult{ToolCallID: callID, Content: "codex_grep: walk: " + walkErr.Error(), IsError: true}
	}

	if len(hits) == 0 {
		e.markCodexEmpty("codex_grep", pattern)
	}

	payload := map[string]interface{}{
		"pattern":    pattern,
		"repo":       resolvedRepo,
		"glob":       glob,
		"total_hits": len(hits),
		"truncated":  truncated,
		"hits":       hits,
	}
	data, _ := json.MarshalIndent(payload, "", "  ")
	return adapter.ToolResult{ToolCallID: callID, Content: string(data)}
}

// markCodexEmpty records that a codex_* tool returned zero results for
// the given query. maybeEmitGap inspects this the next time a filesystem
// tool runs.
func (e *Executor) markCodexEmpty(tool, query string) {
	e.gapMu.Lock()
	defer e.gapMu.Unlock()
	e.lastEmptyCall = &gapPending{codexTool: tool, query: query}
}

// maybeEmitGap publishes a codex.gap event when a filesystem-level tool
// (search_code / list_files / read_file) is invoked AFTER a codex_* tool
// returned empty within the same tool loop. The signal identifies queries
// the codex should have known but didn't.
func (e *Executor) maybeEmitGap(ctx context.Context, tool string, args map[string]interface{}, result adapter.ToolResult) {
	if e.kb == nil || e.workItemID == nil {
		return
	}
	// Only grep-like tools trigger the gap signal. Codex tools themselves
	// and write-side tools are ignored.
	isFallback := tool == "search_code" || tool == "list_files" || tool == "read_file"
	if !isFallback {
		return
	}
	e.gapMu.Lock()
	pending := e.lastEmptyCall
	if pending == nil {
		e.gapMu.Unlock()
		return
	}
	// Consume the pending marker; we only emit one gap per codex miss to
	// avoid flooding the topic.
	e.lastEmptyCall = nil
	e.gapMu.Unlock()

	foundVia := "grep"
	if result.IsError {
		foundVia = "none"
	} else if result.Content == "" || result.Content == "No matches found" {
		foundVia = "none"
	}

	// Extract the source_file argument, if any. search_code has a pattern,
	// read_file/list_files have a path.
	sourceFile := ""
	if p, ok := args["path"].(string); ok {
		sourceFile = p
	}

	productID := ""
	if e.productID != nil {
		productID = e.productID.String()
	}
	event := map[string]interface{}{
		// gap_id deduplicates a redelivery in status-orchestrator's
		// codex.gap consumer (internal/codexgap).
		"gap_id":        uuid.NewString(),
		"work_item_id":  e.workItemID.String(),
		"product_id":    productID,
		"codex_tool":    pending.codexTool,
		"query":         pending.query,
		"fallback_tool": tool,
		"found_via":     foundVia,
		"source_file":   sourceFile,
		"timestamp":     time.Now().UTC().Format(time.RFC3339Nano),
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return
	}
	// Fire-and-forget. Codex-gap publishing must never block the tool loop.
	_ = e.kb.Publish(ctx, bus.TopicCodexGap, e.workItemID.String(), payload)
}

// maybeEmitPlannerGap publishes a codex.gap event when a write/edit tool
// targets a file that was NOT in the planner's FilesToChange set. The
// distinct reason="planner_missed_file" lets downstream consumers
// separate "codex returned empty, fell back to grep" gaps from "planner
// did not anticipate this file" gaps. We emit at most one event per file
// per task (plannerGapEmitted guards the dedup).
//
// The check is a no-op when plannedFiles is nil (no set declared — e.g.
// legacy brief, or tasks that don't use the planner-scope feature).
func (e *Executor) maybeEmitPlannerGap(ctx context.Context, tool string, args map[string]interface{}, result adapter.ToolResult) {
	if e.kb == nil || e.workItemID == nil {
		return
	}
	if result.IsError {
		return
	}
	// Only write-side tools are interesting here. read_file/search_code
	// against an out-of-scope file isn't a planner miss — the agent may
	// just be checking something.
	if !IsFileEditTool(tool) {
		return
	}
	path, _ := args["path"].(string)
	if path == "" {
		return
	}

	e.gapMu.Lock()
	if e.plannedFiles == nil {
		e.gapMu.Unlock()
		return
	}
	if _, ok := e.plannedFiles[path]; ok {
		e.gapMu.Unlock()
		return
	}
	if e.plannerGapEmitted == nil {
		e.plannerGapEmitted = make(map[string]struct{})
	}
	if _, already := e.plannerGapEmitted[path]; already {
		e.gapMu.Unlock()
		return
	}
	e.plannerGapEmitted[path] = struct{}{}
	e.gapMu.Unlock()

	productID := ""
	if e.productID != nil {
		productID = e.productID.String()
	}
	event := map[string]interface{}{
		"work_item_id":  e.workItemID.String(),
		"product_id":    productID,
		"reason":        "planner_missed_file",
		"intended_file": path,
		"fallback_tool": tool,
		"timestamp":     time.Now().UTC().Format(time.RFC3339Nano),
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return
	}
	_ = e.kb.Publish(ctx, bus.TopicCodexGap, e.workItemID.String(), payload)
}

// ─── codex LSP tools / codex_list_tree / codex_verify_imports ────────────────
//
// The six LSP-backed tools (codex_find_references, codex_goto_definition,
// codex_workspace_symbols, codex_document_symbols, codex_type_info,
// codex_diagnostics) all ride on top of the repo-cache root that
// codex_read_file / codex_grep already resolve, and share one lsp.Manager
// that caches a warm client per (repo, language). codex_list_tree is a
// cheap hierarchical directory print; codex_verify_imports is a fast
// regex-based dependency sanity check.

// codexLSPDispatch is the shared LSP plumbing — it performs the repo
// lookup, opens the document, and issues the requested JSON-RPC. The
// public per-tool wrappers below populate `method` and `args` and pass a
// `toolName` so error strings show the tool the LLM actually called.
//
// On first call for a (repo, language) pair, the Manager spawns the
// server, does the initialize handshake, and caches it. Subsequent
// calls reuse the same warm client.
func (e *Executor) codexLSPDispatch(ctx context.Context, toolName, callID, method string, args map[string]interface{}) adapter.ToolResult {
	if e.codex == nil || e.productID == nil {
		return e.codexMissingResult(callID, toolName)
	}
	if e.lspManager == nil {
		return adapter.ToolResult{
			ToolCallID: callID,
			Content:    toolName + ": LSP manager not configured on this executor",
			IsError:    true,
		}
	}

	repoName, _ := args["repo"].(string)
	root, resolvedRepo, err := e.repoCacheRoot(ctx, repoName)
	if err != nil {
		return adapter.ToolResult{ToolCallID: callID, Content: toolName + ": " + err.Error(), IsError: true}
	}

	// 15s per-call hard cap — language servers occasionally hang on
	// project load. The warm-path overhead is tiny.
	callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	client, language, err := e.lspManager.ClientForRepo(callCtx, root)
	if err != nil {
		return adapter.ToolResult{
			ToolCallID: callID,
			Content:    toolName + ": " + err.Error() + " — fall back to codex_similar_code or codex_grep.",
			IsError:    true,
		}
	}

	// workspace_symbol is the only method that doesn't need a path.
	if method == "workspace_symbol" {
		query, _ := args["query"].(string)
		raw, err := client.Request(callCtx, "workspace/symbol", map[string]any{"query": query})
		if err != nil {
			return adapter.ToolResult{ToolCallID: callID, Content: toolName + ": " + err.Error(), IsError: true}
		}
		// Optional client-side limit — some servers return hundreds of
		// matches for short queries.
		if v, ok := args["limit"].(float64); ok && int(v) > 0 {
			raw = truncateWorkspaceSymbols(raw, int(v))
		}
		return adapter.ToolResult{ToolCallID: callID, Content: string(lspResponse(resolvedRepo, language, method, raw))}
	}

	rel, _ := args["path"].(string)
	cleanRel, err := validateRepoRelPath(rel)
	if err != nil {
		return adapter.ToolResult{ToolCallID: callID, Content: toolName + ": " + err.Error(), IsError: true}
	}
	fullPath := filepath.Join(root, filepath.FromSlash(cleanRel))
	data, rerr := os.ReadFile(fullPath)
	if rerr != nil {
		return adapter.ToolResult{
			ToolCallID: callID,
			Content:    fmt.Sprintf("%s: cannot read %q: %v", toolName, cleanRel, rerr),
			IsError:    true,
		}
	}
	// Hard binary sniff — we must not ship NUL-containing text to the server.
	if looksBinary(data[:min(len(data), 1024)]) {
		return adapter.ToolResult{ToolCallID: callID, Content: toolName + ": file appears binary", IsError: true}
	}

	uri := lsp.PathToURI(fullPath)
	if openErr := client.OpenDoc(uri, language, string(data)); openErr != nil {
		return adapter.ToolResult{ToolCallID: callID, Content: toolName + " didOpen: " + openErr.Error(), IsError: true}
	}

	// Convert 1-based line, 0-based character to the LSP zero-based position.
	pos := lsp.Position{}
	if v, ok := args["line"].(float64); ok && int(v) > 0 {
		pos.Line = int(v) - 1
	}
	if v, ok := args["character"].(float64); ok && int(v) >= 0 {
		pos.Character = int(v)
	}

	switch method {
	case "references":
		raw, rpcErr := client.Request(callCtx, "textDocument/references", map[string]any{
			"textDocument": map[string]any{"uri": uri},
			"position":     pos,
			"context":      map[string]any{"includeDeclaration": true},
		})
		if rpcErr != nil {
			return adapter.ToolResult{ToolCallID: callID, Content: toolName + ": " + rpcErr.Error(), IsError: true}
		}
		return adapter.ToolResult{ToolCallID: callID, Content: string(lspResponse(resolvedRepo, language, method, raw))}
	case "definition":
		raw, rpcErr := client.Request(callCtx, "textDocument/definition", map[string]any{
			"textDocument": map[string]any{"uri": uri},
			"position":     pos,
		})
		if rpcErr != nil {
			return adapter.ToolResult{ToolCallID: callID, Content: toolName + ": " + rpcErr.Error(), IsError: true}
		}
		return adapter.ToolResult{ToolCallID: callID, Content: string(lspResponse(resolvedRepo, language, method, raw))}
	case "hover":
		raw, rpcErr := client.Request(callCtx, "textDocument/hover", map[string]any{
			"textDocument": map[string]any{"uri": uri},
			"position":     pos,
		})
		if rpcErr != nil {
			return adapter.ToolResult{ToolCallID: callID, Content: toolName + ": " + rpcErr.Error(), IsError: true}
		}
		return adapter.ToolResult{ToolCallID: callID, Content: string(lspResponse(resolvedRepo, language, method, raw))}
	case "document_symbol":
		raw, rpcErr := client.Request(callCtx, "textDocument/documentSymbol", map[string]any{
			"textDocument": map[string]any{"uri": uri},
		})
		if rpcErr != nil {
			return adapter.ToolResult{ToolCallID: callID, Content: toolName + ": " + rpcErr.Error(), IsError: true}
		}
		return adapter.ToolResult{ToolCallID: callID, Content: string(lspResponse(resolvedRepo, language, method, raw))}
	case "diagnostics":
		// Diagnostics are push-based — the server publishes them after
		// didOpen / didChange. Give it up to ~1s to produce a response
		// before we snapshot whatever's in the cache.
		deadline := time.Now().Add(1 * time.Second)
		var diags []lsp.Diagnostic
		for time.Now().Before(deadline) {
			diags = client.Diagnostics(uri)
			if len(diags) > 0 {
				break
			}
			select {
			case <-time.After(100 * time.Millisecond):
			case <-callCtx.Done():
				deadline = time.Now()
			}
		}
		payload := map[string]interface{}{
			"repo":        resolvedRepo,
			"language":    language,
			"method":      method,
			"uri":         uri,
			"diagnostics": diags,
		}
		data, _ := json.MarshalIndent(payload, "", "  ")
		return adapter.ToolResult{ToolCallID: callID, Content: string(data)}
	default:
		return adapter.ToolResult{
			ToolCallID: callID,
			Content:    fmt.Sprintf("%s: unsupported method %q", toolName, method),
			IsError:    true,
		}
	}
}

// codexFindReferences serves the codex_find_references tool. Accepts
// either (path, line, character) — the direct case — or a free-form
// `symbol` name that is resolved via workspace/symbol first, picking the
// top hit as the reference target.
func (e *Executor) codexFindReferences(ctx context.Context, callID string, args map[string]interface{}) adapter.ToolResult {
	// Direct-position path: if line/character/path are present, forward.
	if path, _ := args["path"].(string); strings.TrimSpace(path) != "" {
		if _, hasLine := args["line"].(float64); hasLine {
			return e.codexLSPDispatch(ctx, "codex_find_references", callID, "references", args)
		}
	}

	// Symbol-name path: resolve via workspace/symbol, then run references
	// on the top hit's declaration location.
	symbol, _ := args["symbol"].(string)
	if strings.TrimSpace(symbol) == "" {
		return adapter.ToolResult{
			ToolCallID: callID,
			Content:    "codex_find_references: either `symbol` or `path`+`line`+`character` is required",
			IsError:    true,
		}
	}
	if e.codex == nil || e.productID == nil {
		return e.codexMissingResult(callID, "codex_find_references")
	}
	if e.lspManager == nil {
		return adapter.ToolResult{ToolCallID: callID, Content: "codex_find_references: LSP manager not configured on this executor", IsError: true}
	}

	repoName, _ := args["repo"].(string)
	root, resolvedRepo, err := e.repoCacheRoot(ctx, repoName)
	if err != nil {
		return adapter.ToolResult{ToolCallID: callID, Content: "codex_find_references: " + err.Error(), IsError: true}
	}
	callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	client, language, err := e.lspManager.ClientForRepo(callCtx, root)
	if err != nil {
		return adapter.ToolResult{
			ToolCallID: callID,
			Content:    "codex_find_references: " + err.Error() + " — fall back to codex_similar_code or codex_grep.",
			IsError:    true,
		}
	}

	raw, err := client.Request(callCtx, "workspace/symbol", map[string]any{"query": symbol})
	if err != nil {
		return adapter.ToolResult{ToolCallID: callID, Content: "codex_find_references workspace/symbol: " + err.Error(), IsError: true}
	}
	var symbols []workspaceSymbolHit
	if jsonErr := json.Unmarshal(raw, &symbols); jsonErr != nil || len(symbols) == 0 {
		return adapter.ToolResult{
			ToolCallID: callID,
			Content:    fmt.Sprintf("codex_find_references: no workspace symbol matched %q — try codex_workspace_symbols directly, or supply path+line+character.", symbol),
			IsError:    true,
		}
	}

	// Prefer an exact-name match if one exists; otherwise take the first.
	target := symbols[0]
	for _, s := range symbols {
		if s.Name == symbol {
			target = s
			break
		}
	}
	if target.Location.URI == "" {
		return adapter.ToolResult{
			ToolCallID: callID,
			Content:    "codex_find_references: resolved symbol had no location — supply path+line+character directly.",
			IsError:    true,
		}
	}
	targetPath := lsp.URIToPath(target.Location.URI)
	data, rerr := os.ReadFile(targetPath)
	if rerr != nil {
		return adapter.ToolResult{ToolCallID: callID, Content: fmt.Sprintf("codex_find_references: cannot read resolved location %q: %v", targetPath, rerr), IsError: true}
	}
	uri := lsp.PathToURI(targetPath)
	if openErr := client.OpenDoc(uri, language, string(data)); openErr != nil {
		return adapter.ToolResult{ToolCallID: callID, Content: "codex_find_references didOpen: " + openErr.Error(), IsError: true}
	}

	refsRaw, rpcErr := client.Request(callCtx, "textDocument/references", map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     target.Location.Range.Start,
		"context":      map[string]any{"includeDeclaration": true},
	})
	if rpcErr != nil {
		return adapter.ToolResult{ToolCallID: callID, Content: "codex_find_references: " + rpcErr.Error(), IsError: true}
	}
	payload := map[string]interface{}{
		"repo":              resolvedRepo,
		"language":          language,
		"method":            "references",
		"resolved_symbol":   target.Name,
		"resolved_location": target.Location,
		"result":            refsRaw,
	}
	out, _ := json.MarshalIndent(payload, "", "  ")
	return adapter.ToolResult{ToolCallID: callID, Content: string(out)}
}

// codexGotoDefinition serves the codex_goto_definition tool.
func (e *Executor) codexGotoDefinition(ctx context.Context, callID string, args map[string]interface{}) adapter.ToolResult {
	return e.codexLSPDispatch(ctx, "codex_goto_definition", callID, "definition", args)
}

// codexWorkspaceSymbols serves the codex_workspace_symbols tool.
func (e *Executor) codexWorkspaceSymbols(ctx context.Context, callID string, args map[string]interface{}) adapter.ToolResult {
	return e.codexLSPDispatch(ctx, "codex_workspace_symbols", callID, "workspace_symbol", args)
}

// codexDocumentSymbols serves the codex_document_symbols tool.
func (e *Executor) codexDocumentSymbols(ctx context.Context, callID string, args map[string]interface{}) adapter.ToolResult {
	return e.codexLSPDispatch(ctx, "codex_document_symbols", callID, "document_symbol", args)
}

// codexTypeInfo serves the codex_type_info tool (LSP hover).
func (e *Executor) codexTypeInfo(ctx context.Context, callID string, args map[string]interface{}) adapter.ToolResult {
	return e.codexLSPDispatch(ctx, "codex_type_info", callID, "hover", args)
}

// codexDiagnostics serves the codex_diagnostics tool.
func (e *Executor) codexDiagnostics(ctx context.Context, callID string, args map[string]interface{}) adapter.ToolResult {
	return e.codexLSPDispatch(ctx, "codex_diagnostics", callID, "diagnostics", args)
}

// workspaceSymbolHit is the subset of the LSP WorkspaceSymbol shape we
// need to resolve a name to a location.
type workspaceSymbolHit struct {
	Name     string `json:"name"`
	Kind     int    `json:"kind"`
	Location struct {
		URI   string    `json:"uri"`
		Range lsp.Range `json:"range"`
	} `json:"location"`
}

// truncateWorkspaceSymbols trims the workspace/symbol JSON array to at
// most `limit` entries while preserving the JSON shape. Best-effort: on
// parse failure it returns the original payload.
func truncateWorkspaceSymbols(raw json.RawMessage, limit int) json.RawMessage {
	if limit <= 0 {
		return raw
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return raw
	}
	if len(arr) <= limit {
		return raw
	}
	trimmed, err := json.Marshal(arr[:limit])
	if err != nil {
		return raw
	}
	return trimmed
}

// lspResponse envelopes the raw LSP result with metadata the LLM will find
// useful (which repo, which language, which method). Keeps the `result`
// key intact so the LLM can feed it through standard LSP documentation.
func lspResponse(repo, language, method string, raw json.RawMessage) []byte {
	if len(raw) == 0 {
		raw = json.RawMessage("null")
	}
	payload := map[string]interface{}{
		"repo":     repo,
		"language": language,
		"method":   method,
		"result":   raw,
	}
	data, _ := json.MarshalIndent(payload, "", "  ")
	return data
}

// codexListTree walks the repo-cache root and emits an indented text tree.
// Exists because "orient me in this directory" was costing the LLM ~4-6
// list_files calls per unfamiliar subtree.
func (e *Executor) codexListTree(ctx context.Context, callID string, args map[string]interface{}) adapter.ToolResult {
	if !e.codexReadsAvailable() {
		return e.codexMissingResult(callID, "codex_list_tree")
	}
	repoName, _ := args["repo"].(string)
	root, resolvedRepo, err := e.codexReadRoot(ctx, repoName)
	if err != nil {
		return adapter.ToolResult{ToolCallID: callID, Content: "codex_list_tree: " + err.Error(), IsError: true}
	}

	startRel, _ := args["path"].(string)
	if strings.TrimSpace(startRel) == "" || startRel == "." {
		startRel = ""
	} else {
		cleaned, vErr := validateRepoRelPath(startRel)
		if vErr != nil {
			return adapter.ToolResult{ToolCallID: callID, Content: "codex_list_tree: " + vErr.Error(), IsError: true}
		}
		startRel = cleaned
	}

	depth := 2
	if v, ok := args["depth"].(float64); ok && int(v) > 0 {
		depth = int(v)
	}
	if depth > 5 {
		depth = 5
	}

	excludeExtra := []string{}
	if arr, ok := args["exclude"].([]interface{}); ok {
		for _, item := range arr {
			if s, ok := item.(string); ok && s != "" {
				excludeExtra = append(excludeExtra, s)
			}
		}
	}

	start := filepath.Join(root, filepath.FromSlash(startRel))
	info, statErr := os.Stat(start)
	if statErr != nil {
		return adapter.ToolResult{ToolCallID: callID, Content: "codex_list_tree: " + statErr.Error(), IsError: true}
	}
	if !info.IsDir() {
		return adapter.ToolResult{ToolCallID: callID, Content: "codex_list_tree: path is not a directory", IsError: true}
	}

	var out strings.Builder
	label := startRel
	if label == "" {
		label = "."
	}
	out.WriteString(label + "/\n")
	walkTree(&out, start, root, 1, depth, excludeExtra)

	payload := map[string]interface{}{
		"repo": resolvedRepo,
		"path": label,
		"tree": out.String(),
	}
	data, _ := json.MarshalIndent(payload, "", "  ")
	return adapter.ToolResult{ToolCallID: callID, Content: string(data)}
}

// walkTree is the recursive helper for codex_list_tree. It formats each
// level with two-space indentation and appends "/" to directory names.
func walkTree(out *strings.Builder, dir, root string, level, maxDepth int, extraExcludes []string) {
	if level > maxDepth {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	// Deterministic order so the LLM's cache keys stay stable.
	for _, entry := range entries {
		name := entry.Name()
		if _, skip := defaultGrepSkipDirs[name]; skip && entry.IsDir() {
			continue
		}
		full := filepath.Join(dir, name)
		rel, _ := filepath.Rel(root, full)
		rel = filepath.ToSlash(rel)
		excluded := false
		for _, pat := range extraExcludes {
			if matchGlob(pat, rel) {
				excluded = true
				break
			}
		}
		if excluded {
			continue
		}
		indent := strings.Repeat("  ", level)
		if entry.IsDir() {
			out.WriteString(indent + name + "/\n")
			walkTree(out, full, root, level+1, maxDepth, extraExcludes)
		} else {
			out.WriteString(indent + name + "\n")
		}
	}
}

// codexVerifyImports runs the fast import sanity check. Shells to nothing —
// everything is in-process regex + filesystem stat.
func (e *Executor) codexVerifyImports(ctx context.Context, callID string, args map[string]interface{}) adapter.ToolResult {
	if !e.codexReadsAvailable() {
		return e.codexMissingResult(callID, "codex_verify_imports")
	}
	rel, _ := args["path"].(string)
	content, _ := args["content"].(string)
	if strings.TrimSpace(rel) == "" {
		return adapter.ToolResult{ToolCallID: callID, Content: "codex_verify_imports: path is required", IsError: true}
	}
	cleanRel, err := validateRepoRelPath(rel)
	if err != nil {
		return adapter.ToolResult{ToolCallID: callID, Content: "codex_verify_imports: " + err.Error(), IsError: true}
	}
	repoName, _ := args["repo"].(string)
	root, resolvedRepo, err := e.codexReadRoot(ctx, repoName)
	if err != nil {
		return adapter.ToolResult{ToolCallID: callID, Content: "codex_verify_imports: " + err.Error(), IsError: true}
	}

	result, vErr := verify.VerifyImports(root, cleanRel, content)
	if vErr != nil {
		return adapter.ToolResult{ToolCallID: callID, Content: "codex_verify_imports: " + vErr.Error(), IsError: true}
	}

	payload := map[string]interface{}{
		"repo":         resolvedRepo,
		"path":         cleanRel,
		"checked":      result.Checked,
		"unresolved":   result.Unresolved,
		"package_json": result.PackageJSON,
	}
	data, _ := json.MarshalIndent(payload, "", "  ")
	return adapter.ToolResult{ToolCallID: callID, Content: string(data)}
}

// ErrToolCallingNotSupported is returned when the configured model does not
// support tool calling and tools are required for the operation.
var ErrToolCallingNotSupported = fmt.Errorf("model does not support tool calling")

// ErrSafetyLimit is returned when the tool loop hits the maxIterations
// safety valve without producing a final response. Callers (agents) must
// treat this as a hard failure — the LLM was stuck in a loop calling the
// same tools repeatedly, so any partial evidence is unreliable and must
// NOT be passed downstream as if it were a successful finding. Research
// agents wrap results with Status="limit_hit" and coordinators block the
// work item honestly rather than converging on junk.
var ErrSafetyLimit = fmt.Errorf("tool loop reached safety limit")

// createWorkItemSQL inserts a chat-created work item in intake.
const createWorkItemSQL = `INSERT INTO work_items (id, title, description, type, priority, product_id, state, codex_version, created_at, updated_at)
			 VALUES ($1, $2, $3, $4::work_item_type, $5::priority_level, $6, 'intake', 0, NOW(), NOW())`

// createWorkItemWithIntake writes the work item and enqueues its intake event
// (topic, key and payload unchanged from the direct publish it replaces) in
// one transaction. The event is delivered by whichever service's outbox
// dispatcher claims it; the chat agent runs one.
func createWorkItemWithIntake(ctx context.Context, db workgraph.TxBeginner, id uuid.UUID, title, description, itemType, priority string, productID *uuid.UUID) error {
	return workgraph.WithTx(ctx, db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, createWorkItemSQL, id, title, description, itemType, priority, productID); err != nil {
			return err
		}
		return outbox.EnqueueJSON(ctx, tx, id, bus.TopicIntake, id.String(), map[string]string{
			"work_id":     id.String(),
			"title":       title,
			"description": description,
		})
	})
}

// RunToolLoop executes a full tool-calling conversation loop.
// It sends the request to the LLM, executes any returned tool calls, appends
// the results to the message history, and repeats until the LLM produces a
// final text response or the safety valve limit / stuck detection triggers.
//
// maxIterations is a safety valve only — stuck detection (same call repeated
// 3 times) will trigger before the limit in practice. Pass 0 to use the
// default safety valve of 200 iterations.
//
// db and workItemID are optional — when both are non-nil each LLM call is
// logged to the inference_logs table so that tool-loop inference is visible.
//
// IMPORTANT: If tools are provided in the request but the model does not
// support tool calling, RunToolLoop returns ErrToolCallingNotSupported
// immediately. Agents MUST handle this by blocking the work item and
// reporting the limitation to the user — never degrade to text-only mode.
func RunToolLoop(
	ctx context.Context,
	llmAdapter adapter.ModelAdapter,
	req adapter.CompletionRequest,
	executor *Executor,
	maxIterations int, // safety valve only — stuck detection triggers earlier
	db *pgxpool.Pool,
	workItemID *uuid.UUID,
) (*adapter.CompletionResponse, error) {
	return RunToolLoopWithOptions(ctx, llmAdapter, req, executor, maxIterations, db, workItemID, LoopOptions{})
}

// RunToolLoopWithOptions is RunToolLoop with the opt-in LoopOptions
// (evidence-ledger compaction, dead-tool guard, refreshed tool lists).
func RunToolLoopWithOptions(
	ctx context.Context,
	llmAdapter adapter.ModelAdapter,
	req adapter.CompletionRequest,
	executor *Executor,
	maxIterations int,
	db *pgxpool.Pool,
	workItemID *uuid.UUID,
	opts LoopOptions,
) (*adapter.CompletionResponse, error) {
	if maxIterations == 0 {
		maxIterations = 200 // safety valve, not the real limit
	}

	caps := llmAdapter.Capabilities()

	// Enforce: if tools are required, the model must support them.
	// We never silently degrade to text-only — that produces hallucinated output.
	if len(req.Tools) > 0 && !caps.SupportsFunctions {
		return nil, fmt.Errorf(
			"%w: model %q does not support function/tool calling. "+
				"This agent requires tool calling to interact with real code. "+
				"Configure a model that supports tools (e.g., GPT-4o, Claude Sonnet/Opus, Llama 3.1+, Qwen 2.5+) "+
				"or assign this work to a different agent definition with a capable model",
			ErrToolCallingNotSupported,
			llmAdapter.Name(),
		)
	}

	executor.db = db

	// Per-work-item token budget: kill the loop when cumulative tokens
	// across all agents exceed the cap. Default 500K tokens (~$5 at
	// Sonnet pricing). Override via WORK_ITEM_TOKEN_BUDGET env var.
	tokenBudget := TokenBudget()
	var cumulativeTokens int64
	if db != nil && workItemID != nil {
		_ = db.QueryRow(ctx,
			`SELECT COALESCE(SUM(input_tokens + output_tokens), 0) FROM inference_logs WHERE work_item_id = $1`,
			*workItemID,
		).Scan(&cumulativeTokens)
	}

	// Stuck detection: track the last tool-call fingerprint and repeat count.
	// Recovery attempts: when stuck, inject a recovery prompt AND physically
	// strip the stuck tool from the available tools list for subsequent
	// iterations. Text-only recovery ("try something else") does not work on
	// smaller local models (gemma4:26b observed ignoring 3 recovery messages
	// and continuing the exact same call 18 times). Removing the tool from
	// the schema the model sees is the only reliable break.
	var lastCallKey string
	var lastStuckTool string // tool name to strip after stuck detection
	repeatCount := 0
	recoveryAttempts := 0
	maxRecoveries := 3 // try 3 different recovery strategies before giving up

	// Sliding-window uniqueness: track the last 8 turns' tool-name sets.
	// If there are ≤2 unique tool-name sets in the window, the agent is
	// stuck in a rut even with varying arguments.
	const slidingWindowSize = 8
	const slidingMinUnique = 3
	var toolNameHistory []string

	// Checkpoint threshold: 40% of context window, capped at 60K.
	// Larger models benefit from more upfront context (fewer iterations),
	// but letting conversations grow to 150K+ (75% of 200K) wastes tokens
	// re-sending stale tool results every iteration.
	microcompactThreshold := caps.MaxContextTokens * 2 / 5
	if microcompactThreshold < 6000 {
		microcompactThreshold = 6000
	}
	if microcompactThreshold > 60000 {
		microcompactThreshold = 60000
	}
	originalTools := append([]adapter.Tool(nil), req.Tools...)
	strippedTools := map[string]bool{}         // tools currently hidden from the model
	strippedWhy := map[string]string{}         // why each was hidden (the refusal names it)
	snapshotSeq := 0                           // incremented per checkpoint
	toolResultCache := make(map[string]string) // dedup cache: "tool:args" → result content
	consecutiveNudges := 0                     // limits prose-nudge retries before accepting response
	totalNudges := 0                           // hard cap on total nudge iterations (prevents infinite loops)
	maxTotalNudges := maxIterations * 3        // absolute ceiling regardless of productive-iteration resets
	toolsRun := 0                              // tool calls executed so far (R18: EvidenceNudge)
	evidenceAnswers := 0                       // tool-less answers before any tool ran (R18)

	allowedTools := make(map[string]bool, len(req.Tools))
	for _, t := range req.Tools {
		allowedTools[t.Name] = true
	}

	// Evidence ledger (R1): opt-in transcript compaction, and the dead-tool
	// guard's per-tool count of not-ready/unavailable errors.
	var ledger *evidenceLedger
	keepRecent := opts.KeepRecent
	if keepRecent <= 0 {
		keepRecent = keepRecentFromEnv()
	}
	if opts.EvidenceLedger {
		ledger = newEvidenceLedger()
		if opts.PinReads {
			ledger.enablePins(opts.PinnedFiles, executor.currentLines)
			ledger.seedOf = executor.readSeed
		}
	}
	deadToolErrors := map[string]int{}
	// Re-read dedup (R12 X2): what each read returned, by tool-call turn.
	var rereads *readMemo
	if opts.DedupReads && ledger != nil {
		rereads = &readMemo{}
	}
	toolTurn := 0
	// The checkpoint's store (conversation_snapshots, or the caller's) and
	// what it does with the span it removes: with the ledger on, its tool
	// results become ledger lines and its reads pins (R10 W6).
	var cpStore checkpointStore
	cpWork := workItemID
	switch {
	case db != nil:
		cpStore = dbCheckpointStore(db)
	case opts.CheckpointStore != nil:
		cpStore = opts.CheckpointStore
		if cpWork == nil {
			cpWork = &uuid.Nil
		}
	}
	cpDrop := func(span []adapter.ChatMessage) {
		if ledger != nil {
			ledger.absorb(span)
		}
		if rereads != nil {
			rereads.forgetTranscript(toolTurn)
		}
	}
	var stuck *stuckTracker
	var cycles *cycleGuard   // R17: whole-turn cycles
	var repeats *readRepeats // R17: repeated reads of a read-only tree
	if opts.StuckControl {
		stuck = newStuckTracker(opts.PriorStuck)
		cycles = newCycleGuard()
		if opts.ReadOnly {
			repeats = newReadRepeats()
		}
	}

	// Diminishing-returns detector: tracks the last N output-token counts
	// per turn. If all of them are below a floor the model is emitting
	// trivially short responses (often "ok" / "continuing..." / empty
	// tool_calls) — a sign it's run out of useful things to do. We stop
	// the loop with a "progress stalled" error instead of burning
	// iterations on no-op turns.
	const diminishingReturnsWindow = 4
	const diminishingReturnsFloor = 30 // output tokens per turn
	outputTokenHistory := make([]int, 0, diminishingReturnsWindow)

	for i := 0; i < maxIterations; i++ {
		// Respect context cancellation / timeout before each LLM call.
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("tool loop aborted after %d iterations: %w", i, err)
		}
		// The caller's budget check (M3-06): between iterations, before the
		// next LLM call is paid for.
		if executor.iterationHook != nil {
			if err := executor.iterationHook(ctx, cumulativeTokens, tokenBudget); err != nil {
				return nil, fmt.Errorf("tool loop stopped after %d iterations: %w", i, err)
			}
		}

		// Rebuild the tool list from originals minus stripped tools. This runs
		// every iteration so recovery-induced strips take effect on the next
		// LLM call. Empty stripped map is the common case — no overhead.
		// RefreshTools (when set) supplies the current list first.
		if opts.RefreshTools != nil {
			if fresh := opts.RefreshTools(); len(fresh) > 0 {
				originalTools = append([]adapter.Tool(nil), fresh...)
				allowedTools = make(map[string]bool, len(originalTools))
				for _, t := range originalTools {
					allowedTools[t.Name] = true
				}
			}
		}
		// A dropped tool stays dropped (R10 W3): the refresh above used to
		// put it back, and a model that emits calls as text kept calling it.
		// Opt-in with StuckControl: without it a stripped tool a model
		// still calls runs, as before R10 (research relies on that).
		if opts.StuckControl {
			for name := range strippedTools {
				delete(allowedTools, name)
			}
		}
		if len(strippedTools) > 0 || opts.RefreshTools != nil {
			filtered := make([]adapter.Tool, 0, len(originalTools))
			for _, t := range originalTools {
				if !strippedTools[t.Name] {
					filtered = append(filtered, t)
				}
			}
			req.Tools = filtered
		}

		// Insert the inference_logs row BEFORE the LLM call so the dashboard
		// can show it as "running" while it's in flight. Update at the end.
		// latency_ms=0 is the dashboard's "still running" marker.
		var logID string
		if db != nil && workItemID != nil {
			// agent_id is a UUID column; passing the literal string "tool-agent"
			// triggered a silent INSERT failure (errors swallowed below). Leave
			// it NULL — the intent_type="tool_loop" is enough to identify these
			// rows for the dashboard.
			// M4-05: the caller's inference tags (role, agent type, config
			// snapshot, resolved model) come from ctx (llm.WithInferenceTags).
			tags := llm.InferenceTagsFrom(ctx)
			provider, modelID := llm.ModelIdentity(llmAdapter.Name(), tags)
			insertErr := db.QueryRow(ctx,
				`INSERT INTO inference_logs (model_provider, model_id, intent_type, input_tokens, output_tokens, latency_ms, success, error_message, work_item_id, agent_id, created_at,
				                             role, agent_type, config_snapshot_id)
				 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, NULL, NOW(), $10, $11, $12)
				 RETURNING id::text`,
				provider, modelID, "tool_loop", 0, 0,
				int64(0), false, "", workItemID,
				llm.NullIfEmpty(tags.Role), llm.NullIfEmpty(tags.AgentType), llm.NullIfEmpty(tags.ConfigSnapshotID),
			).Scan(&logID)
			if insertErr != nil {
				fmt.Printf("tool-executor: insert in-progress inference_logs row failed: %v\n", insertErr)
			}
			if logID != "" {
				_, _ = db.Exec(ctx, "SELECT pg_notify('inference_started', $1)", logID)
			}
		}

		callStart := time.Now()
		var resp *adapter.CompletionResponse
		var err error
		for attempt := 0; attempt < 3; attempt++ {
			resp, err = llmAdapter.Complete(ctx, req)
			if err == nil || !llm.IsTransientLLMError(err) {
				break
			}
			if attempt < 2 {
				delay := time.Duration(5*(1<<uint(attempt))) * time.Second // 5s, 10s
				log.Printf("tool-loop: transient error on LLM call %d attempt %d/3 (%v); retrying in %s", i, attempt+1, err, delay)
				select {
				case <-time.After(delay):
				case <-ctx.Done():
					break
				}
			}
		}
		latency := time.Since(callStart)

		// Update the row with the real outcome + conversation content.
		if db != nil && workItemID != nil && logID != "" {
			errMsg := ""
			if err != nil {
				errMsg = err.Error()
			}
			var inputTokens, outputTokens int
			responseText := ""
			if resp != nil {
				inputTokens = resp.InputTokens
				outputTokens = resp.OutputTokens
				responseText = resp.Content
			}
			messagesJSON := []byte("[]")
			if b, mErr := json.Marshal(req.Messages); mErr == nil {
				messagesJSON = b
			}
			_, _ = db.Exec(ctx,
				`UPDATE inference_logs
				 SET input_tokens = $1, output_tokens = $2, latency_ms = $3,
				     success = $4, error_message = $5,
				     prompt_messages = $6::jsonb, response_content = $7
				 WHERE id = $8::uuid`,
				inputTokens, outputTokens, latency.Milliseconds(),
				err == nil, errMsg, string(messagesJSON), responseText, logID,
			)
			_, _ = db.Exec(ctx, "SELECT pg_notify('inference_completed', $1)", logID)
		}

		if err != nil {
			return nil, fmt.Errorf("LLM call %d failed: %w", i, err)
		}

		// Track LLM call count and token consumption.
		metrics.LLMCallsTotal.Inc()
		if resp.InputTokens > 0 || resp.OutputTokens > 0 {
			metrics.LLMTokensUsed.Add(int64(resp.InputTokens + resp.OutputTokens))
			cumulativeTokens += int64(resp.InputTokens + resp.OutputTokens)
		}

		// Token budget kill switch.
		if cumulativeTokens > tokenBudget {
			log.Printf("tool-loop: token budget exceeded (%d > %d) for work_item=%v — terminating", cumulativeTokens, tokenBudget, workItemID)
			return nil, fmt.Errorf("%w: %d tokens used (limit %d) — work item is too expensive, investigate before retrying", ErrTokenBudgetExceeded, cumulativeTokens, tokenBudget)
		}

		// If no tool calls, the model either finished or gave up.
		if len(resp.ToolCalls) == 0 || resp.FinishReason != "tool_calls" {
			// R18 E1: an answer before any tool ran is not evidence. The
			// first gets EvidenceNudge; the second ends the loop with
			// NoEvidence (judged before INSUFFICIENT and the prose check,
			// which would accept or re-nudge it).
			if opts.EvidenceNudge != "" && toolsRun == 0 {
				evidenceAnswers++
				if evidenceAnswers > 1 || i >= maxIterations-1 {
					ne := &NoEvidence{Answers: evidenceAnswers, Note: boundNote(resp.Content)}
					log.Printf("tool-loop: %v — ending the loop", ne)
					return nil, ne
				}
				log.Printf("tool-loop: answer with no tool call run yet — nudging to gather evidence (iter %d)", i)
				req.Messages = append(req.Messages,
					adapter.ChatMessage{Role: adapter.RoleAssistant, Content: resp.Content},
					adapter.ChatMessage{Role: adapter.RoleUser, Content: opts.EvidenceNudge},
				)
				i-- // the nudge is not a productive iteration
				continue
			}
			// R10 W3: "INSUFFICIENT: …" is the model saying it is stuck.
			if stuck != nil && len(resp.ToolCalls) == 0 && isInsufficient(resp.Content) {
				s := stuck.insufficient(resp.Content)
				log.Printf("tool-loop: model answered INSUFFICIENT — %v", s)
				return nil, s
			}
			// done_reason="length" with 0 tool calls means the model's thinking
			// filled the entire output budget. Nudging won't help — the context
			// is too large. Checkpoint to compact it, then retry.
			if resp.FinishReason == "length" && len(resp.ToolCalls) == 0 {
				if checkpointDropping(ctx, cpStore, &req.Messages, resp.InputTokens, 0, cpWork, executor.askingAgent, &snapshotSeq, allowedTools["recall_context"], cpDrop) {
					log.Printf("tool-loop: done_reason=length with 0 tool calls — forced checkpoint (seq=%d), retrying with compact context (iter %d)", snapshotSeq, i)
					consecutiveNudges = 0
					continue
				}
			}

			// Gave-up detection: if the response reads like the model was
			// planning more work ("let me", "now I'll", "next step") but
			// emitted prose instead of tool calls, it has stalled — usually
			// after a rejected edit or a confusing tool error. Instead of
			// treating this as a final answer, nudge the model once to
			// resume with tool calls. If it gives up again, accept it.
			lc := strings.ToLower(resp.Content)
			gaveUpSignals := []string{
				"let me ", "now let me", "i'll ", "i will ",
				"next step", "now i need to", "continue with",
				"let's ", "remaining step", "moving on to",
			}
			// R18: tool calls written as JSON text, or a first-turn reply
			// that opens with a plan heading, planned work it did not do —
			// judged only in research and implementation loops (review m1:
			// a chat reply may open with "## Plan" or show a call example).
			looksLikeGaveUp := planTriggers(opts) && looksLikeUnexecutedPlan(resp.Content, toolsRun == 0)
			for _, sig := range gaveUpSignals {
				if strings.Contains(lc, sig) {
					looksLikeGaveUp = true
					break
				}
			}
			// Also treat empty responses (thinking-only extraction failures)
			// as needing a nudge rather than accepting as final.
			if lc == "" && resp.FinishReason == "stop" && i < maxIterations-1 {
				looksLikeGaveUp = true
			}
			if looksLikeGaveUp && i < maxIterations-1 {
				consecutiveNudges++
				totalNudges++
				if consecutiveNudges > 4 || totalNudges > maxTotalNudges {
					log.Printf("tool-loop: nudge cap reached (consecutive=%d, total=%d) — accepting response as final (iter %d)", consecutiveNudges, totalNudges, i)
					return resp, nil
				}
				log.Printf("tool-loop: model responded with prose planning future work instead of tool calls — nudging to resume (iter %d)", i)
				req.Messages = append(req.Messages,
					adapter.ChatMessage{Role: adapter.RoleAssistant, Content: resp.Content},
					adapter.ChatMessage{Role: adapter.RoleUser, Content: "You described what you plan to do but did not call any tools. " +
						"Your response MUST be a tool call — call edit_file, write_file, read_file, or another tool NOW. " +
						"Do not explain or plan. Act."},
				)
				i-- // Nudge iterations don't count against the productive-iteration budget
				continue
			}
			return resp, nil
		}

		// Episodic memory: when input tokens approach the context limit,
		// store the full conversation in the DB and replace it with a
		// compact checkpoint summary + the last few messages. The model
		// can call recall_context to retrieve old details if needed.
		if checkpointDropping(ctx, cpStore, &req.Messages, resp.InputTokens, microcompactThreshold, cpWork, executor.askingAgent, &snapshotSeq, allowedTools["recall_context"], cpDrop) {
			log.Printf("tool-loop: conversation checkpointed (seq=%d, input_tokens=%d was approaching context limit)", snapshotSeq, resp.InputTokens)
		}

		// Diminishing-returns check: if the last N consecutive non-tool-call
		// turns produced <= floor output tokens, assume progress has stalled.
		// Tool-call turns are excluded: reasoning models with think=false
		// emit ~30 tokens per tool call (just the call JSON), which is normal
		// productive behavior. Counting those would false-positive after 4
		// tool calls and kill the loop prematurely.
		if len(resp.ToolCalls) > 0 {
			outputTokenHistory = outputTokenHistory[:0]
		} else {
			outputTokenHistory = append(outputTokenHistory, resp.OutputTokens)
		}
		if len(outputTokenHistory) > diminishingReturnsWindow {
			outputTokenHistory = outputTokenHistory[len(outputTokenHistory)-diminishingReturnsWindow:]
		}
		if len(outputTokenHistory) == diminishingReturnsWindow {
			allBelow := true
			for _, n := range outputTokenHistory {
				if n > diminishingReturnsFloor {
					allBelow = false
					break
				}
			}
			if allBelow {
				log.Printf("tool-loop: diminishing returns detected — %d consecutive turns under %d output tokens", diminishingReturnsWindow, diminishingReturnsFloor)
				// Heuristic escalation: let the user redirect rather than
				// silently returning what's probably a stall-response.
				if executor.publishHeuristicPrompt(ctx,
					interactions.TriggerHeuristicDiminishing,
					"Agent is producing diminishing output — clarify?",
					fmt.Sprintf("The last %d responses each emitted ≤ %d output tokens — the agent appears to be acknowledging without substantive progress. Should I retry with a different approach, or is the plan worth rethinking?", diminishingReturnsWindow, diminishingReturnsFloor),
					map[string]any{
						"output_token_history": outputTokenHistory,
						"last_response":        resp.Content,
					}) {
					return &adapter.CompletionResponse{Content: resp.Content, FinishReason: "awaiting_user"}, nil
				}
				return resp, nil
			}
		}

		// R17: whole-turn cycles and repeated reads of a read-only tree,
		// judged before the turn runs. A cycle's warning goes after the
		// turn's results.
		var cycleWarning string
		var turnFPs []turnCall
		var readPrior map[int]int
		if cycles != nil {
			turnFPs = executor.turnCalls(resp.ToolCalls)
			var s *LoopStuck
			if cycleWarning, s = cycles.observe(turnFPs); s != nil {
				log.Printf("tool-loop: %v — ending the loop", s)
				return nil, s
			}
			if cycleWarning != "" {
				log.Printf("tool-loop: cycle detected — %s", cycleWarning)
			}
		}
		if repeats != nil {
			var s *LoopStuck
			if readPrior, s = repeats.check(turnFPs, allowedTools); s != nil {
				log.Printf("tool-loop: %v — ending the loop", s)
				return nil, s
			}
		}

		// Stuck detection: build a fingerprint of this round's tool calls.
		callKey := ""
		for _, call := range resp.ToolCalls {
			callKey += call.Name + ":" + call.Arguments + ";"
		}
		if callKey == lastCallKey {
			repeatCount++
			if repeatCount >= 3 {
				// We're stuck — strip the offending tool AND inject recovery.
				// Text-only nudges have been observed to fail on smaller local
				// models; removing the tool from the schema forces diversity.
				recoveryAttempts++
				if recoveryAttempts > maxRecoveries && stuck != nil {
					// StuckControl: a structured failure, not a prompt.
					s := &LoopStuck{Tool: resp.ToolCalls[0].Name, Reason: fmt.Sprintf("the same call was repeated after %d recovery attempts", maxRecoveries), Attempts: repeatCount + 1}
					s.Fingerprint, s.Path = executor.callFingerprint(resp.ToolCalls[0])
					return nil, s
				}
				if recoveryAttempts > maxRecoveries {
					// Heuristic escalation: ask the user instead of failing
					// silently. The user can retry / restart / cancel the
					// work item — any of which is better than returning
					// unreliable partial evidence.
					if executor.publishHeuristicPrompt(ctx,
						interactions.TriggerHeuristicStuck,
						fmt.Sprintf("Agent stuck on repeated %s call", resp.ToolCalls[0].Name),
						fmt.Sprintf("I called %s with identical arguments %d× across %d recovery attempts and couldn't break the loop. The tool has been stripped but I have no path forward. What would you like me to try?", resp.ToolCalls[0].Name, repeatCount+1, maxRecoveries),
						map[string]any{
							"repeated_tool":     resp.ToolCalls[0].Name,
							"arguments":         resp.ToolCalls[0].Arguments,
							"recovery_attempts": recoveryAttempts,
						}) {
						return &adapter.CompletionResponse{Content: resp.Content, FinishReason: "awaiting_user"}, nil
					}
					return nil, fmt.Errorf("tool loop stuck after %d recovery attempts: same call repeated (%s)", maxRecoveries, resp.ToolCalls[0].Name)
				}

				stuckTool := resp.ToolCalls[0].Name
				lastStuckTool = stuckTool
				strippedTools[stuckTool] = true
				strippedWhy[stuckTool] = "it was called with identical arguments 3 times in a row"
				if opts.StuckControl {
					delete(allowedTools, stuckTool)
				}

				log.Printf("tool-loop: stuck detected (same call %dx on %s), recovery %d/%d — stripping tool from next iterations", repeatCount+1, stuckTool, recoveryAttempts, maxRecoveries)

				// Recovery messages are task-agnostic and name the stripped tool
				// so the model sees concrete feedback about its own behavior.
				var recoveryMsg string
				switch recoveryAttempts {
				case 1:
					recoveryMsg = fmt.Sprintf("STUCK: You called %s with identical arguments 3 times. It has been temporarily removed from your tool list. Choose a different tool, or if you already have enough evidence, write your final answer now with no more tool calls.", stuckTool)
				case 2:
					recoveryMsg = fmt.Sprintf("STILL STUCK: You have now repeated another call. Both offending tools are removed. Write your final answer with whatever evidence you have gathered. Partial answers are acceptable; silent looping is not. If you genuinely cannot answer, respond: 'INSUFFICIENT: <one-line reason>'.")
				case 3:
					recoveryMsg = fmt.Sprintf("FINAL: Produce your answer now with no tool calls. If you cannot, respond: 'INSUFFICIENT: <reason>'. Any further tool call will cause the task to fail.")
				}

				req.Messages = append(req.Messages, adapter.ChatMessage{
					Role:    adapter.RoleUser,
					Content: recoveryMsg,
				})

				// Reset fingerprint tracking so stripping different tools is measured independently.
				repeatCount = 0
				lastCallKey = ""
				_ = lastStuckTool
				continue
			}
		} else {
			repeatCount = 0
			lastCallKey = callKey
		}

		// Sliding-window uniqueness: detect varied-but-repetitive patterns.
		// Build a tool-name-only key for this turn (ignoring arguments).
		nameKey := ""
		for _, call := range resp.ToolCalls {
			nameKey += call.Name + ";"
		}
		toolNameHistory = append(toolNameHistory, nameKey)
		if len(toolNameHistory) > slidingWindowSize {
			toolNameHistory = toolNameHistory[len(toolNameHistory)-slidingWindowSize:]
		}
		if len(toolNameHistory) == slidingWindowSize {
			uniq := make(map[string]bool)
			for _, k := range toolNameHistory {
				uniq[k] = true
			}
			if len(uniq) < slidingMinUnique {
				log.Printf("tool-loop: sliding-window stuck — only %d unique tool patterns in last %d turns", len(uniq), slidingWindowSize)
				// R17: after its warning, a window with no new call ends the
				// loop (StuckControl); one with new calls is not a rut.
				if cycles != nil && cycles.slidingWarned && !cycles.novelSinceSliding {
					s := &LoopStuck{Tool: strings.TrimSuffix(nameKey, ";"), Kind: StuckKindSlidingWindow, Attempts: slidingWindowSize,
						Reason: fmt.Sprintf("only %d distinct tool patterns in the last %d turns, and no new call since the warning", len(uniq), slidingWindowSize)}
					if len(turnFPs) > 0 {
						s.Tool, s.Path, s.Fingerprint = turnFPs[0].call.Name, turnFPs[0].path, turnFPs[0].fp
					}
					log.Printf("tool-loop: %v — ending the loop", s)
					return nil, s
				}
				if cycles != nil {
					cycles.slidingWarned, cycles.novelSinceSliding = true, false
				}
				recoveryAttempts++
				if recoveryAttempts > maxRecoveries && stuck != nil {
					return nil, &LoopStuck{Tool: strings.TrimSuffix(nameKey, ";"), Reason: fmt.Sprintf("only %d distinct tool patterns in the last %d turns after %d recovery attempts", len(uniq), slidingWindowSize, maxRecoveries), Attempts: slidingWindowSize}
				}
				if recoveryAttempts > maxRecoveries {
					if executor.publishHeuristicPrompt(ctx,
						interactions.TriggerHeuristicStuck,
						"Agent stuck in repetitive loop",
						fmt.Sprintf("The last %d tool-call turns used only %d distinct patterns — the agent is cycling without progress. What should I try differently?", slidingWindowSize, len(uniq)),
						map[string]any{"tool_patterns": toolNameHistory, "recovery_attempts": recoveryAttempts}) {
						return &adapter.CompletionResponse{Content: resp.Content, FinishReason: "awaiting_user"}, nil
					}
					return nil, fmt.Errorf("tool loop stuck: sliding-window detected %d unique patterns in %d turns after %d recoveries", len(uniq), slidingWindowSize, maxRecoveries)
				}
				req.Messages = append(req.Messages, adapter.ChatMessage{
					Role:    adapter.RoleUser,
					Content: fmt.Sprintf("WARNING: You have been calling the same tools repeatedly for %d turns without meaningful progress. Step back and try a fundamentally different approach. If you have enough information, write your final answer now.", slidingWindowSize),
				})
				toolNameHistory = toolNameHistory[:0] // reset window after intervention
			}
		}

		// Log progress so operators can observe the tool loop in action.
		for _, call := range resp.ToolCalls {
			log.Printf("tool-loop: iteration %d, calling %s", i+1, call.Name)
		}

		// Append the assistant's message (which contains the tool call requests).
		// ToolCalls must be preserved so adapters can reconstruct tool_use blocks
		// in the conversation history — Anthropic requires each tool_result to
		// reference a tool_use in the preceding assistant message.
		req.Messages = append(req.Messages, adapter.ChatMessage{
			Role:      adapter.RoleAssistant,
			Content:   resp.Content,
			ToolCalls: resp.ToolCalls,
		})

		// Snapshot message history so request_user_input can persist it
		// for resume (Task 20). Marshal the current req.Messages to JSON —
		// each iteration overwrites with the latest.
		if snap, err := json.Marshal(req.Messages); err == nil {
			executor.SetResumptionSnapshot(snap)
		}

		// Reject hallucinated tool calls not in the schema. The executor
		// has handlers for all tools, but agents only get a subset. If the
		// model generates a call outside its schema we return an error
		// result instead of executing it.
		validCalls := make([]adapter.ToolCall, 0, len(resp.ToolCalls))
		var rejectedResults []adapter.ToolResult
		for _, call := range resp.ToolCalls {
			if allowedTools[call.Name] {
				validCalls = append(validCalls, call)
			} else {
				log.Printf("tool-loop: rejected hallucinated tool call %q (not in schema)", call.Name)
				content := fmt.Sprintf("Tool %q is not available in this agent context. Available tools: use only the tools provided in the schema.", call.Name)
				if why := strippedWhy[call.Name]; why != "" {
					content = fmt.Sprintf("Tool %q is no longer available for this task: %s.", call.Name, why)
				}
				rejectedResults = append(rejectedResults, adapter.ToolResult{
					ToolCallID: call.ID,
					Content:    content,
					IsError:    true,
				})
			}
		}

		// Execute tool calls. Adjacent concurrency-safe calls run in
		// parallel (read-only tools like read_file / search_code /
		// codex_*). Side-effectful tools (write_file, edit_file,
		// run_command, git_*) run in a batch of one so subsequent tools
		// in the same turn see their filesystem changes in order.
		// Results are collected by call index so we can append them to
		// the message history in the original order the model emitted
		// them — the conversational protocol requires 1:1 tool_call →
		// tool_result matching.
		// Fingerprints are taken before the calls run: the file state the
		// call acted on.
		var fingerprints, fpPaths []string
		if stuck != nil {
			for _, call := range validCalls {
				fp, p := executor.callFingerprint(call)
				fingerprints, fpPaths = append(fingerprints, fp), append(fpPaths, p)
			}
		}
		toolTurn++
		// R12 X2: reads of lines the model still has, unchanged, are
		// answered with one line; the rest run.
		runCalls := validCalls
		var shortReads map[int]adapter.ToolResult
		if rereads != nil {
			shortReads = rereads.answer(ctx, executor, validCalls, toolTurn, keepRecent, ledger)
			if len(shortReads) > 0 {
				runCalls = make([]adapter.ToolCall, 0, len(validCalls))
				for k, c := range validCalls {
					if _, short := shortReads[k]; !short {
						runCalls = append(runCalls, c)
					}
				}
			}
		}
		var ranResults []adapter.ToolResult
		if opts.DeadToolGuard {
			ranResults = runGuardedToolCalls(ctx, executor, runCalls, toolResultCache, deadToolErrors)
		} else {
			ranResults = runToolCallBatches(ctx, executor, runCalls, toolResultCache)
		}
		validResults := ranResults
		if len(shortReads) > 0 {
			validResults = make([]adapter.ToolResult, len(validCalls))
			ran := 0
			for k := range validCalls {
				if r, short := shortReads[k]; short {
					validResults[k] = r
				} else {
					validResults[k] = ranResults[ran]
					ran++
				}
			}
		}
		if repeats != nil {
			// turnFPs and readPrior are by resp.ToolCalls index; map them
			// to validCalls.
			var vt []turnCall
			vp := map[int]int{}
			for k, call := range resp.ToolCalls {
				if !allowedTools[call.Name] {
					continue
				}
				if n, ok := readPrior[k]; ok {
					vp[len(vt)] = n
				}
				vt = append(vt, turnFPs[k])
			}
			repeats.annotate(vt, vp, validResults, toolTurn, rereads != nil)
		}
		if rereads != nil {
			rereads.record(executor, validCalls, validResults, toolTurn)
		}
		consecutiveNudges = 0 // tool calls executed — reset nudge counter
		toolsRun += len(validCalls)

		// Reassemble results in original resp.ToolCalls order so the
		// 1:1 tool_call → tool_result mapping is preserved.
		allResults := make([]adapter.ToolResult, len(resp.ToolCalls))
		validIdx := 0
		rejIdx := 0
		for k, call := range resp.ToolCalls {
			if allowedTools[call.Name] {
				allResults[k] = validResults[validIdx]
				validIdx++
			} else {
				allResults[k] = rejectedResults[rejIdx]
				rejIdx++
			}
		}

		if stuck != nil {
			for k := range validCalls {
				if s := stuck.observe(validCalls[k], fingerprints[k], fpPaths[k], &validResults[k]); s != nil {
					log.Printf("tool-loop: %v — ending the loop", s)
					return nil, s
				}
			}
			validIdx = 0
			for k, call := range resp.ToolCalls {
				if allowedTools[call.Name] {
					allResults[k] = validResults[validIdx]
					validIdx++
				}
			}
		}

		awaitingUser := false
		for k, call := range resp.ToolCalls {
			req.Messages = append(req.Messages, adapter.ChatMessage{
				Role:       adapter.RoleTool,
				Content:    allResults[k].Content,
				ToolCallID: call.ID,
				Name:       call.Name,
			})
			if call.Name == "request_user_input" && !allResults[k].IsError {
				awaitingUser = true
			}
		}
		if cycleWarning != "" {
			req.Messages = append(req.Messages, adapter.ChatMessage{Role: adapter.RoleUser, Content: cycleWarning})
		}
		if awaitingUser {
			return &adapter.CompletionResponse{
				Content:      resp.Content,
				FinishReason: "awaiting_user",
			}, nil
		}

		// Dead-tool guard (R1): a tool that answered "not ready" /
		// "unavailable" twice in a row will not recover within this loop;
		// stop offering it so the model stops paying for the call. Any
		// other outcome of the tool resets its count.
		for k, call := range resp.ToolCalls {
			if ledger != nil {
				ledger.note(call, allResults[k])
			}
			if !opts.DeadToolGuard || !allowedTools[call.Name] {
				continue
			}
			if !allResults[k].IsError || !isDeadToolError(allResults[k].Content) {
				delete(deadToolErrors, call.Name)
				continue
			}
			deadToolErrors[call.Name]++
			if deadToolErrors[call.Name] == 2 && !strippedTools[call.Name] {
				strippedTools[call.Name] = true
				strippedWhy[call.Name] = fmt.Sprintf("it failed twice in a row (%s)", errorClass(allResults[k].Content))
				log.Printf("tool-loop: %s returned %q twice in a row — dropped from the tool list", call.Name, errorClass(allResults[k].Content))
				note := fmt.Sprintf("- %s removed from your tools: it failed twice in a row (%s); do not call it again.", call.Name, errorClass(allResults[k].Content))
				if ledger != nil {
					ledger.addNote(note)
				} else {
					req.Messages = append(req.Messages, adapter.ChatMessage{Role: adapter.RoleUser, Content: note[2:]})
				}
			}
		}
		// Evidence ledger (R1): results older than the last keepRecent
		// iterations leave the transcript for one ledger line each.
		if ledger != nil {
			req.Messages = compactTranscript(req.Messages, ledger, keepRecent)
		}

		// Clear any one-shot tool results; they are now embedded in messages
		req.ToolResults = nil
	}

	return nil, fmt.Errorf("%w: %d iterations", ErrSafetyLimit, maxIterations)
}
