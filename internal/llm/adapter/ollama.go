package adapter

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// ollamaAdapter implements ModelAdapter for the Ollama REST API.
type ollamaAdapter struct {
	endpoint string
	model    string
	client   *http.Client
}

// NewOllama returns a ModelAdapter backed by a local Ollama instance.
// If endpoint is empty it defaults to "http://localhost:11434".
func NewOllama(endpoint, model string) ModelAdapter {
	if endpoint == "" {
		endpoint = "http://localhost:11434"
	}
	return &ollamaAdapter{
		endpoint: strings.TrimRight(endpoint, "/"),
		model:    model,
		// No HTTP client timeout. An inference call that takes 15 minutes
		// is producing tokens; one that takes 2 hours is still producing
		// tokens. Cutting it off on the wall clock loses real work. The
		// correct "we're stuck" signal is emitted by the tool loop's
		// stuck detector (same tool call repeated 4×) — never by the
		// HTTP client. If the Ollama server truly hangs (network issue,
		// process death), operators will see elapsed time climb in the
		// dashboard and can intervene. Setting Timeout=0 means no
		// http.Client-imposed deadline; the request ends when the caller
		// cancels or the response completes.
		client: &http.Client{Timeout: 0},
	}
}

// ---- internal JSON types ----

type ollamaMessage struct {
	Role      string           `json:"role"`
	Content   string           `json:"content"`
	Thinking  string           `json:"thinking,omitempty"`
	ToolCalls []ollamaToolCall `json:"tool_calls,omitempty"`
}

type ollamaToolCall struct {
	Function ollamaFunctionCall `json:"function"`
}

type ollamaFunctionCall struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type ollamaTool struct {
	Type     string         `json:"type"` // "function"
	Function ollamaFunction `json:"function"`
}

type ollamaFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type ollamaChatRequest struct {
	Model    string          `json:"model"`
	Messages []ollamaMessage `json:"messages"`
	Stream   bool            `json:"stream"`
	Tools    []ollamaTool    `json:"tools,omitempty"`
	Options  *ollamaOptions  `json:"options,omitempty"`
	Format   string          `json:"format,omitempty"`
	Think    *bool           `json:"think,omitempty"`
}

type ollamaOptions struct {
	NumPredict  int      `json:"num_predict,omitempty"`
	Temperature float64  `json:"temperature,omitempty"`
	TopP        float64  `json:"top_p,omitempty"`
	Stop        []string `json:"stop,omitempty"`
}

type ollamaChatResponse struct {
	Model           string        `json:"model"`
	Message         ollamaMessage `json:"message"`
	Done            bool          `json:"done"`
	PromptEvalCount int           `json:"prompt_eval_count"`
	EvalCount       int           `json:"eval_count"`
	DoneReason      string        `json:"done_reason"`
}

type ollamaTagsResponse struct {
	Models []struct {
		Name string `json:"name"`
	} `json:"models"`
}

// ---- ModelAdapter implementation ----

func (a *ollamaAdapter) Name() string {
	return "ollama/" + a.model
}

func (a *ollamaAdapter) Capabilities() ModelCapabilities {
	return ModelCapabilities{
		MaxContextTokens:  a.inferContextWindow(),
		SupportsJSON:      true,
		SupportsStreaming: true,
		SupportsFunctions: a.supportsTools(),
	}
}

func (a *ollamaAdapter) inferContextWindow() int {
	model := strings.ToLower(a.model)
	switch {
	case strings.Contains(model, "qwen3"):
		return 32768
	case strings.Contains(model, "qwen2.5"):
		return 32768
	case strings.Contains(model, "llama3"):
		return 32768
	case strings.Contains(model, "llama4"):
		return 131072
	case strings.Contains(model, "gemma3"), strings.Contains(model, "gemma4"):
		return 32768
	case strings.Contains(model, "deepseek"):
		return 32768
	case strings.Contains(model, "mistral"), strings.Contains(model, "mixtral"):
		return 32768
	default:
		return 8192
	}
}

// supportsTools returns true if the configured model is known to support
// tool/function calling. Models that don't support it must NOT be used for
// agents that require tool calling — the system will refuse rather than
// silently degrade to text-only output.
func (a *ollamaAdapter) supportsTools() bool {
	model := strings.ToLower(a.model)
	// Models with confirmed tool calling support in Ollama
	toolModels := []string{
		"llama3.1", "llama3.2", "llama3.3", "llama4",
		"qwen2.5", "qwen3", "qwen2",
		"gemma3", "gemma4",
		"mistral", "mixtral",
		"command-r",
		"hermes", "firefunction",
		"deepseek-coder-v2", "deepseek-v2", "deepseek-r1",
		"nemotron",
		"granite",
	}
	for _, tm := range toolModels {
		if strings.Contains(model, tm) {
			return true
		}
	}
	return false
}

// isReasoningModel returns true if the model uses a thinking/reasoning mode
// that splits output into thinking + content fields.
func (a *ollamaAdapter) isReasoningModel() bool {
	model := strings.ToLower(a.model)
	reasoningModels := []string{
		"qwen3",
		"deepseek-r1",
		"qwq",
	}
	for _, rm := range reasoningModels {
		if strings.Contains(model, rm) {
			return true
		}
	}
	return false
}

func (a *ollamaAdapter) Complete(ctx context.Context, req CompletionRequest) (*CompletionResponse, error) {
	olReq := a.buildRequest(req, false)
	respBody, statusCode, err := a.doPost(ctx, "/api/chat", olReq)
	if err != nil {
		return nil, err
	}
	if statusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama: status %d: %s", statusCode, string(respBody))
	}

	var olResp ollamaChatResponse
	if err := json.Unmarshal(respBody, &olResp); err != nil {
		return nil, fmt.Errorf("ollama: decode response: %w", err)
	}

	content := olResp.Message.Content
	thinkLen := len(olResp.Message.Thinking)
	if strings.TrimSpace(content) == "" && thinkLen > 0 {
		log.Printf("ollama: content empty, thinking=%d bytes, done_reason=%q — extracting structured content from thinking", thinkLen, olResp.DoneReason)
		content = extractStructuredFromThinking(olResp.Message.Thinking)
		if content == olResp.Message.Thinking {
			log.Printf("ollama: no structured block found in thinking, using raw thinking (first 200 chars): %.200s", olResp.Message.Thinking)
		}
	} else if strings.TrimSpace(content) == "" {
		log.Printf("ollama: both content and thinking empty, done_reason=%q, eval_count=%d", olResp.DoneReason, olResp.EvalCount)
	}

	resp := &CompletionResponse{
		Content:      content,
		InputTokens:  olResp.PromptEvalCount,
		OutputTokens: olResp.EvalCount,
		Model:        olResp.Model,
		FinishReason: olResp.DoneReason,
	}

	// Parse tool calls if present. Ollama's /api/chat returns tool calls in
	// message.tool_calls for models whose template emits the native
	// function-calling format (llama3.1+, some qwen variants with matching
	// Modelfile). For models like qwen2.5-coder whose default template
	// instead wraps calls in <tools>…</tools> or ```json fences inside the
	// message content, tool_calls is null and the call is invisible to the
	// executor. Fall back to extracting those envelopes so the agent can
	// actually run tools rather than failing silently in plan-mode.
	if len(olResp.Message.ToolCalls) > 0 {
		resp.FinishReason = "tool_calls"
		for i, tc := range olResp.Message.ToolCalls {
			resp.ToolCalls = append(resp.ToolCalls, ToolCall{
				ID:        fmt.Sprintf("call_%d", i),
				Name:      tc.Function.Name,
				Arguments: string(tc.Function.Arguments),
			})
		}
	} else if embedded := extractEmbeddedToolCalls(resp.Content); len(embedded) > 0 {
		resp.FinishReason = "tool_calls"
		resp.ToolCalls = embedded
		// Strip the envelope from content so the tool loop doesn't
		// interpret it as a final answer. The model's remaining prose
		// (if any) stays as the visible message.
		resp.Content = stripToolCallEnvelopes(resp.Content)
	}

	return resp, nil
}

// --- Content-embedded tool call extraction ------------------------------------
//
// Some Ollama model templates (notably qwen2.5-coder) do not emit the native
// tool_calls field; they wrap the call JSON in the message body. We recognise
// three envelopes and extract inner {"name","arguments"} objects from each:
//   <tools>…</tools>
//   <tool_call>…</tool_call>
//   ```json … ```         (only when the content is a tool-call shaped object)
// Each extracted object becomes a structured ToolCall. If the outer layer is
// a JSON array, each element is treated as a separate call. Anything that
// doesn't parse as a valid tool call is silently skipped — fail-honestly
// doesn't mean translating garbage; if the model emitted prose, let the
// existing verify-gate handle it.

var (
	toolsTagRe    = regexp.MustCompile(`(?s)<tools>\s*(.*?)\s*</tools>`)
	toolCallTagRe = regexp.MustCompile(`(?s)<tool_call>\s*(.*?)\s*</tool_call>`)
	jsonFenceRe   = regexp.MustCompile("(?s)```(?:json)?\\s*(\\{[\\s\\S]*?\\}|\\[[\\s\\S]*?\\])\\s*```")
)

type embeddedToolCall struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

func extractEmbeddedToolCalls(content string) []ToolCall {
	var candidates []string
	for _, m := range toolsTagRe.FindAllStringSubmatch(content, -1) {
		if len(m) >= 2 {
			candidates = append(candidates, m[1])
		}
	}
	for _, m := range toolCallTagRe.FindAllStringSubmatch(content, -1) {
		if len(m) >= 2 {
			candidates = append(candidates, m[1])
		}
	}
	// Only accept fenced JSON when no XML envelope was found — the XML
	// wrappers are a stronger signal. Fenced JSON can also contain tool
	// *results* or example payloads that would trigger false positives.
	if len(candidates) == 0 {
		for _, m := range jsonFenceRe.FindAllStringSubmatch(content, -1) {
			if len(m) >= 2 && looksLikeToolCall(m[1]) {
				candidates = append(candidates, m[1])
			}
		}
	}
	// Last resort: scan for JSON objects embedded ANYWHERE in the content —
	// prose prefix, markdown headings, whatever. qwen2.5-coder emits
	// "### Step 1: ...\n\n{"name": "write_file", ...}" and earlier passes
	// missed it because they only accepted JSON that was the *entire*
	// trimmed content. Walk the string, at every '{' scan forward using
	// balanced-brace matching (respecting string literals) to find a
	// complete top-level object; if it parses and looks like a tool call,
	// accept it. Multiple inline calls are supported.
	//
	// R18 review M1: it runs whenever the envelopes gave no call — not
	// only when no envelope was found — so an envelope that held no usable
	// call does not hide calls written elsewhere in the content.
	var out []ToolCall
	for _, raw := range candidates {
		out = appendEmbeddedCalls(out, raw)
	}
	if len(out) == 0 {
		for _, raw := range scanInlineJSONObjects(content) {
			if looksLikeToolCall(raw) {
				out = appendEmbeddedCalls(out, raw)
			}
		}
	}
	return out
}

// appendEmbeddedCalls parses one envelope's body — a single call object or
// an array of them — and appends its calls with sequential call_N ids.
// R18 review M1: a body that does not unmarshal as one value (live run
// attempt 15 wrote nine call objects, one per line, in one ```json fence)
// is split into its top-level objects by balanced scanning, and each one
// that looks like a tool call is parsed on its own; other objects (example
// payloads) are still skipped.
func appendEmbeddedCalls(out []ToolCall, raw string) []ToolCall {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return out
	}
	add := func(e embeddedToolCall) {
		if e.Name != "" {
			out = append(out, ToolCall{ID: fmt.Sprintf("call_%d", len(out)), Name: e.Name, Arguments: string(e.Arguments)})
		}
	}
	// Accept either a single object or an array of objects.
	if strings.HasPrefix(raw, "[") {
		var arr []embeddedToolCall
		if err := json.Unmarshal([]byte(raw), &arr); err == nil {
			for _, e := range arr {
				add(e)
			}
			return out
		}
	}
	var single embeddedToolCall
	if err := json.Unmarshal([]byte(raw), &single); err == nil {
		add(single)
		return out
	}
	for _, obj := range scanInlineJSONObjects(raw) {
		if !looksLikeToolCall(obj) {
			continue
		}
		var e embeddedToolCall
		if err := json.Unmarshal([]byte(obj), &e); err == nil {
			add(e)
		}
	}
	return out
}

// looksLikeToolCall does a cheap pre-check so the jsonFenceRe fallback
// doesn't grab arbitrary JSON snippets the model emits as examples.
func looksLikeToolCall(s string) bool {
	s = strings.TrimSpace(s)
	return strings.Contains(s, `"name"`) && strings.Contains(s, `"arguments"`)
}

// scanInlineJSONObjects walks the content and extracts every top-level
// balanced JSON object it finds. It respects string literals (including
// escaped quotes) so a `}` inside a string value doesn't break nesting.
// Objects shorter than 20 bytes are skipped — they're never a valid tool
// call with a name + arguments. The output is the raw substring for each
// candidate; the caller parses and filters.
func scanInlineJSONObjects(content string) []string {
	var out []string
	for i := 0; i < len(content); i++ {
		if content[i] != '{' {
			continue
		}
		depth := 0
		inString := false
		escaped := false
		for j := i; j < len(content); j++ {
			c := content[j]
			if inString {
				if escaped {
					escaped = false
					continue
				}
				if c == '\\' {
					escaped = true
					continue
				}
				if c == '"' {
					inString = false
				}
				continue
			}
			if c == '"' {
				inString = true
				continue
			}
			if c == '{' {
				depth++
			} else if c == '}' {
				depth--
				if depth == 0 {
					raw := content[i : j+1]
					if len(raw) >= 20 {
						out = append(out, raw)
					}
					i = j // advance outer loop past this object
					break
				}
			}
		}
	}
	return out
}

// extractStructuredFromThinking pulls the useful output from a reasoning
// model's thinking field. Reasoning models (qwen3, deepseek-r1) put
// chain-of-thought in "thinking" and the final answer in "content". When
// content is empty, the model may have put the final answer at the end of
// its thinking — often as a fenced code block or a raw JSON array/object.
// We try to extract just that structured part; if nothing structured is
// found, we return the full thinking text (correct for free-form intents).
func extractStructuredFromThinking(thinking string) string {
	// 1. Try fenced code blocks — most reliable signal. Take the last one,
	//    since earlier fences are typically examples/drafts in the reasoning.
	fences := jsonFenceRe.FindAllStringSubmatch(thinking, -1)
	if len(fences) > 0 {
		last := fences[len(fences)-1]
		if len(last) >= 2 {
			return strings.TrimSpace(last[1])
		}
	}

	// 2. Try to find the last top-level JSON array — the final answer for
	//    decomposition-style prompts that ask for "[{...}, ...]".
	lastArr := lastJSONBlock(thinking, '[', ']')
	if lastArr != "" {
		return lastArr
	}

	// 3. Try the last top-level JSON object.
	lastObj := lastJSONBlock(thinking, '{', '}')
	if lastObj != "" {
		return lastObj
	}

	// 4. No structured content found — return the full thinking text.
	//    This is correct for intents like synthesize-brief where the
	//    reasoning IS the useful content.
	return thinking
}

// lastJSONBlock scans backwards for the last balanced JSON block opened by
// `open` and closed by `close` (either {/} or [/]). Returns the raw
// substring if it parses as valid JSON, empty string otherwise.
func lastJSONBlock(s string, open, close byte) string {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] != close {
			continue
		}
		depth := 0
		inStr := false
		esc := false
		for j := i; j >= 0; j-- {
			c := s[j]
			if inStr {
				if esc {
					esc = false
					continue
				}
				if c == '\\' {
					esc = true
					continue
				}
				if c == '"' {
					inStr = false
				}
				continue
			}
			if c == '"' {
				inStr = true
				continue
			}
			if c == close {
				depth++
			} else if c == open {
				depth--
				if depth == 0 {
					candidate := s[j : i+1]
					if json.Valid([]byte(candidate)) {
						return candidate
					}
					break
				}
			}
		}
	}
	return ""
}

func stripToolCallEnvelopes(content string) string {
	out := toolsTagRe.ReplaceAllString(content, "")
	out = toolCallTagRe.ReplaceAllString(out, "")
	out = jsonFenceRe.ReplaceAllStringFunc(out, func(m string) string {
		inner := jsonFenceRe.FindStringSubmatch(m)
		if len(inner) >= 2 && looksLikeToolCall(inner[1]) {
			return ""
		}
		return m
	})
	return strings.TrimSpace(out)
}

func (a *ollamaAdapter) Stream(ctx context.Context, req CompletionRequest) (<-chan StreamChunk, error) {
	olReq := a.buildRequest(req, true)
	httpReq, err := a.newRequest(ctx, "/api/chat", olReq)
	if err != nil {
		return nil, err
	}

	resp, err := a.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("ollama: stream request: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("ollama: status %d: %s", resp.StatusCode, string(b))
	}

	ch := make(chan StreamChunk, 32)
	go func() {
		defer close(ch)
		defer resp.Body.Close()

		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.TrimSpace(line) == "" {
				continue
			}
			var event ollamaChatResponse
			if err := json.Unmarshal([]byte(line), &event); err != nil {
				continue
			}
			sc := StreamChunk{Content: event.Message.Content}
			if sc.Content == "" && event.Message.Thinking != "" {
				sc.Content = event.Message.Thinking
			}
			if event.Done {
				sc.Done = true
				sc.InputTokens = event.PromptEvalCount
				sc.OutputTokens = event.EvalCount
				select {
				case ch <- sc:
				case <-ctx.Done():
				}
				return
			}
			select {
			case ch <- sc:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, nil
}

func (a *ollamaAdapter) HealthCheck(ctx context.Context) error {
	healthCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(healthCtx, http.MethodGet, a.endpoint+"/api/tags", nil)
	if err != nil {
		return fmt.Errorf("ollama: health check request: %w", err)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("ollama: health check: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ollama: health check status %d", resp.StatusCode)
	}
	return nil
}

// ---- helpers ----

func (a *ollamaAdapter) buildRequest(req CompletionRequest, stream bool) ollamaChatRequest {
	var msgs []ollamaMessage
	for _, m := range req.Messages {
		msg := ollamaMessage{Role: string(m.Role), Content: m.Content}
		if m.Role == RoleTool {
			msg.Role = "tool"
		}
		if m.Role == RoleAssistant && len(m.ToolCalls) > 0 {
			for _, tc := range m.ToolCalls {
				msg.ToolCalls = append(msg.ToolCalls, ollamaToolCall{
					Function: ollamaFunctionCall{
						Name:      tc.Name,
						Arguments: json.RawMessage(tc.Arguments),
					},
				})
			}
		}
		msgs = append(msgs, msg)
	}

	// Append any ToolResults as tool messages (for backwards compat)
	for _, tr := range req.ToolResults {
		msgs = append(msgs, ollamaMessage{
			Role:    "tool",
			Content: tr.Content,
		})
	}

	r := ollamaChatRequest{
		Model:    a.model,
		Messages: msgs,
		Stream:   stream,
		Options: &ollamaOptions{
			NumPredict:  req.MaxTokens,
			Temperature: req.Temperature,
			TopP:        req.TopP,
			Stop:        req.Stop,
		},
	}

	// Convert tools to Ollama format (same as OpenAI format)
	if len(req.Tools) > 0 {
		for _, t := range req.Tools {
			r.Tools = append(r.Tools, ollamaTool{
				Type: "function",
				Function: ollamaFunction{
					Name:        t.Name,
					Description: t.Description,
					Parameters:  t.Parameters,
				},
			})
		}
	}

	if req.JSONMode {
		r.Format = "json"
	}

	// Reasoning models (qwen3, deepseek-r1) split output into thinking +
	// content. For structured output calls (no tools), thinking burns output
	// tokens on chain-of-thought and can leave content empty. Disable it.
	// For tool-calling, keep thinking enabled — the model needs to reason
	// about which tool to use next. Streaming also keeps thinking on.
	if !stream && len(req.Tools) == 0 && a.isReasoningModel() {
		f := false
		r.Think = &f
	}

	return r
}

func (a *ollamaAdapter) newRequest(ctx context.Context, path string, body interface{}) (*http.Request, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("ollama: marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint+path, bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("ollama: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	return req, nil
}

func (a *ollamaAdapter) doPost(ctx context.Context, path string, body interface{}) ([]byte, int, error) {
	req, err := a.newRequest(ctx, path, body)
	if err != nil {
		return nil, 0, err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("ollama: http: %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("ollama: read body: %w", err)
	}
	return b, resp.StatusCode, nil
}
