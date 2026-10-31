// benchmark-probe is a one-shot CLI harness for head-to-head benchmarks
// between an Ollama-hosted model (default: gemma4:26b) + the codex+qdrant
// tool surface against Claude with raw grep. It:
//
//   - connects to Postgres (codex) and Qdrant (embeddings)
//   - resolves the DB-configured embedding provider via configstore
//   - constructs an Executor exposing the 15 codex_* tools
//   - drives an Ollama /api/chat tool-calling loop to answer --question
//   - prints a JSON report with answer, tool calls, iterations, tokens,
//     wall time, and tool_protocol (always "native" for now — gemma4:26b
//     advertises the "tools" capability; a manual-protocol fallback path
//     exists in code but is not triggered unless the native call errors).
//
// Deliberately minimal: no Kafka, no HTTP server, no auth, no DB writes.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/codex"
	"github.com/autonomousengine/autonomous-engine/internal/configstore"
	"github.com/autonomousengine/autonomous-engine/internal/crypto"
	"github.com/autonomousengine/autonomous-engine/internal/db"
	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
	"github.com/autonomousengine/autonomous-engine/internal/llm/strategy"
	"github.com/autonomousengine/autonomous-engine/internal/llm/tools"
	"github.com/autonomousengine/autonomous-engine/internal/vector"
	"github.com/google/uuid"
)

const vectorCollection = "codebase"

// CodexFirstStrategy / ImplementStrategy now live in
// internal/llm/strategy/codex_prompt.go so research-agent and benchmark-probe
// share one source of truth. Do not re-add local copies here.

// toolCallRecord captures a single tool invocation for the JSON report.
type toolCallRecord struct {
	Name          string `json:"name"`
	Args          string `json:"args"`
	ResultPreview string `json:"result_preview"`
	LatencyMs     int64  `json:"latency_ms"`
	IsError       bool   `json:"is_error"`
}

// report is the JSON document emitted to stdout.
type report struct {
	Question              string           `json:"question"`
	ProductID             string           `json:"product_id"`
	Model                 string           `json:"model"`
	ToolProtocol          string           `json:"tool_protocol"`
	Answer                string           `json:"answer"`
	ToolCalls             []toolCallRecord `json:"tool_calls"`
	TotalIterations       int              `json:"total_iterations"`
	TotalPromptTokens     int              `json:"total_prompt_tokens"`
	TotalCompletionTokens int              `json:"total_completion_tokens"`
	WallMs                int64            `json:"wall_ms"`
	Error                 string           `json:"error,omitempty"`

	// Verify-gate observability fields — populated only when task_type is
	// "implement". FilesProposed is the list the strategy parser extracted
	// from the final answer; FilesVerified is the count verified via
	// codex_verify_imports; GateRetries counts how many times the probe
	// injected a corrective verify-prompt; ExitPath is
	// "clean_final_answer" or "verify_gate_exhausted".
	FilesProposed  []string `json:"files_proposed,omitempty"`
	FilesVerified  int      `json:"files_verified"`
	GateRetries    int      `json:"gate_retries"`
	ExitPath       string   `json:"exit_path,omitempty"`
}

// Ollama /api/chat wire types — only the fields we need.
type ollamaTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

type ollamaToolCall struct {
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

type ollamaMessage struct {
	Role      string           `json:"role"`
	Content   string           `json:"content"`
	ToolCalls []ollamaToolCall `json:"tool_calls,omitempty"`
}

type ollamaChatRequest struct {
	Model    string          `json:"model"`
	Messages []ollamaMessage `json:"messages"`
	Tools    []ollamaTool    `json:"tools,omitempty"`
	Stream   bool            `json:"stream"`
	Options  map[string]any  `json:"options,omitempty"`
}

type ollamaChatResponse struct {
	Message         ollamaMessage `json:"message"`
	Done            bool          `json:"done"`
	PromptEvalCount int           `json:"prompt_eval_count"`
	EvalCount       int           `json:"eval_count"`
}

func main() {
	question := flag.String("question", "", "engineering question to ask the model")
	productIDStr := flag.String("product-id", "", "product UUID (scopes codex/vector filters)")
	model := flag.String("model", "gemma4:26b", "Ollama model name")
	ollamaURL := flag.String("ollama-url", "http://localhost:11434", "Ollama base URL")
	taskType := flag.String("task-type", "qa", "task type: qa (default, codex-first Q&A) or implement (feature implementation — emits code)")
	flag.Parse()

	if *question == "" || *productIDStr == "" {
		fmt.Fprintln(os.Stderr, "usage: benchmark-probe --question '...' --product-id <uuid> [--model gemma4:26b] [--ollama-url ...]")
		os.Exit(2)
	}
	productID, err := uuid.Parse(*productIDStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid --product-id: %v\n", err)
		os.Exit(2)
	}

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		fmt.Fprintln(os.Stderr, "DATABASE_URL required")
		os.Exit(2)
	}
	qdrantURL := os.Getenv("QDRANT_URL")
	if qdrantURL == "" {
		qdrantURL = "http://qdrant:6333"
	}
	masterKeyStr := os.Getenv("MASTER_ENCRYPTION_KEY")
	if masterKeyStr == "" {
		fmt.Fprintln(os.Stderr, "MASTER_ENCRYPTION_KEY required (for configstore)")
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	pool, err := db.NewPool(ctx, dbURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "db pool: %v\n", err)
		os.Exit(1)
	}
	defer pool.Close()

	mk, err := crypto.LoadMasterKey(masterKeyStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "master key: %v\n", err)
		os.Exit(1)
	}
	cs := configstore.New(pool, mk)

	embSvc, _, err := vector.BuildEmbeddingService(ctx, pool, cs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "embedding service: %v\n", err)
		os.Exit(1)
	}
	vs := vector.NewWithEmbeddings(qdrantURL, vectorCollection, embSvc)
	cx := codex.New(pool)

	// Executor: repoPath irrelevant (we only wire codex tools). Kafka nil.
	executor := tools.NewExecutorWithCodex("/tmp/unused", cx, vs, &productID, nil, nil)

	out := runBenchmark(ctx, executor, *ollamaURL, *model, *question, productID, *taskType)
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(out)
}

// runBenchmark drives the Ollama /api/chat tool-calling loop.
func runBenchmark(ctx context.Context, executor *tools.Executor, ollamaURL, model, question string, productID uuid.UUID, taskType string) report {
	start := time.Now()
	r := report{
		Question:     question,
		ProductID:    productID.String(),
		Model:        model,
		ToolProtocol: "native",
		ToolCalls:    []toolCallRecord{},
	}

	var systemPrompt string
	switch taskType {
	case "implement":
		systemPrompt = "You are a senior engineer with read-only access to a codebase via 15 codex_* tools. " +
			"The tools query a structured knowledge graph of the product's modules, dependencies, patterns, " +
			"LSP signals, and semantically-indexed code chunks.\n" + strategy.ImplementStrategy
	default:
		systemPrompt = "You are a research agent answering an engineering question about a codebase you can access " +
			"via 15 read-only codex_* tools. The tools query a structured knowledge graph of the product's " +
			"modules, dependencies, patterns, LSP signals, and semantically-indexed code chunks.\n" +
			strategy.CodexFirstStrategy
	}

	msgs := []ollamaMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: question},
	}

	ollamaTools := buildOllamaTools()
	const maxIter = 20
	const maxVerifyGateRetries = 2

	// Collected across all ollama iterations. The verify-gate checks
	// this slice when the model returns a final answer.
	var turnCalls []strategy.ToolCall
	verifyGateRetries := 0

	for iter := 1; iter <= maxIter; iter++ {
		resp, err := callOllama(ctx, ollamaURL, ollamaChatRequest{
			Model:    model,
			Messages: msgs,
			Tools:    ollamaTools,
			Stream:   false,
			Options:  map[string]any{"temperature": 0.2, "num_ctx": 16384},
		})
		if err != nil {
			r.Error = fmt.Sprintf("ollama call iteration %d: %v", iter, err)
			r.TotalIterations = iter
			r.WallMs = time.Since(start).Milliseconds()
			return r
		}
		r.TotalPromptTokens += resp.PromptEvalCount
		r.TotalCompletionTokens += resp.EvalCount

		// No tool calls → candidate final answer. Gate it before accepting.
		if len(resp.Message.ToolCalls) == 0 {
			// Only enforce the verify-gate for implementation tasks — Q&A
			// doesn't emit code, so ParseProposedFiles would either return
			// nothing (no-op) or match a path the researcher quoted from a
			// read-only tool (spurious).
			if taskType == "implement" {
				proposed := strategy.ParseProposedFiles(resp.Message.Content)
				missing := strategy.MissingVerifications(proposed, turnCalls)
				if len(missing) > 0 && verifyGateRetries < maxVerifyGateRetries {
					verifyGateRetries++
					r.GateRetries = verifyGateRetries
					// Keep the conversation continuing: record the model's
					// answer as an assistant turn, then inject corrective
					// user message.
					msgs = append(msgs, resp.Message)
					msgs = append(msgs, ollamaMessage{
						Role:    "user",
						Content: strategy.ForcedVerifyPrompt(missing),
					})
					continue
				}
				// Either no missing verifications, or retries exhausted.
				r.FilesProposed = proposed
				r.FilesVerified = len(proposed) - len(missing)
				if len(missing) > 0 {
					r.ExitPath = "verify_gate_exhausted"
				} else {
					r.ExitPath = "clean_final_answer"
				}
			}
			r.Answer = resp.Message.Content
			r.TotalIterations = iter
			r.WallMs = time.Since(start).Milliseconds()
			return r
		}

		// Append assistant message (must include tool_calls to satisfy gemma's
		// grammar on the next turn).
		msgs = append(msgs, resp.Message)

		// Execute each tool call.
		for _, tc := range resp.Message.ToolCalls {
			rec := toolCallRecord{Name: tc.Function.Name, Args: string(tc.Function.Arguments)}
			callStart := time.Now()
			// adapter.ToolCall wants Arguments as string JSON.
			result := executor.Execute(ctx, adapter.ToolCall{
				ID:        fmt.Sprintf("call-%d", iter),
				Name:      tc.Function.Name,
				Arguments: string(tc.Function.Arguments),
			})
			rec.LatencyMs = time.Since(callStart).Milliseconds()
			rec.IsError = result.IsError
			rec.ResultPreview = truncate(result.Content, 200)
			r.ToolCalls = append(r.ToolCalls, rec)
			// Record the call for the verify-gate. Gate only uses Name +
			// Args; result content isn't needed.
			turnCalls = append(turnCalls, strategy.ToolCall{
				Name: tc.Function.Name,
				Args: string(tc.Function.Arguments),
			})

			msgs = append(msgs, ollamaMessage{
				Role:    "tool",
				Content: result.Content,
			})
		}
	}

	r.Error = fmt.Sprintf("hit maxIter=%d without final answer", maxIter)
	r.TotalIterations = maxIter
	r.WallMs = time.Since(start).Milliseconds()
	return r
}

// buildOllamaTools translates the codex tool schemas into Ollama's
// OpenAI-compatible function-calling format. Pulls from tools.CodexTools()
// so new tools registered there appear automatically.
func buildOllamaTools() []ollamaTool {
	adapterTools := tools.CodexTools()
	out := make([]ollamaTool, 0, len(adapterTools))
	for _, t := range adapterTools {
		var ot ollamaTool
		ot.Type = "function"
		ot.Function.Name = t.Name
		ot.Function.Description = t.Description
		ot.Function.Parameters = t.Parameters
		out = append(out, ot)
	}
	return out
}

// callOllama posts a single /api/chat request. One retry on transient net error.
func callOllama(ctx context.Context, baseURL string, req ollamaChatRequest) (*ollamaChatResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/chat", bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("build request: %w", err)
		}
		httpReq.Header.Set("Content-Type", "application/json")
		client := &http.Client{Timeout: 20 * time.Minute}
		resp, err := client.Do(httpReq)
		if err != nil {
			lastErr = err
			continue
		}
		defer resp.Body.Close() //nolint:errcheck
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("ollama status %d: %s", resp.StatusCode, truncate(string(data), 500))
		}
		var parsed ollamaChatResponse
		if err := json.Unmarshal(data, &parsed); err != nil {
			return nil, fmt.Errorf("decode: %w (body=%s)", err, truncate(string(data), 500))
		}
		return &parsed, nil
	}
	return nil, lastErr
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
