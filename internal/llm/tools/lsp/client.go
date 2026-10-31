// Package lsp provides a minimal Language Server Protocol (LSP) client.
//
// The LSP primitive is the single biggest structural-understanding win for
// small models: parser-accurate references / definitions / diagnostics
// replace several custom tools we would otherwise maintain. We host this
// client in-process and speak JSON-RPC over stdin/stdout to whatever
// language server is installed in the agent container (typescript-language-
// server, gopls, pyright, etc.).
//
// This is NOT a general-purpose LSP library. It implements only the methods
// the autonomous-engine codex LSP tools (codex_find_references,
// codex_goto_definition, codex_workspace_symbols, codex_document_symbols,
// codex_type_info, codex_diagnostics) expose:
//
//	initialize, initialized, textDocument/definition, textDocument/references,
//	textDocument/hover, textDocument/documentSymbol, workspace/symbol,
//	textDocument/publishDiagnostics (inbound notification).
//
// Path concerns and server lifecycle are handled by the Manager in
// manager.go — Client is just the transport.
package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Client is a minimal JSON-RPC 2.0 client for a language server reached
// over stdin/stdout. Each Client instance is associated with one server
// process and one workspace root.
type Client struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser

	// reqID is incremented atomically for each outgoing Request.
	reqMu sync.Mutex
	reqID int64

	// responses maps request-id -> waiter channel. Inbound responses
	// are demultiplexed off the single read goroutine.
	respMu    sync.Mutex
	responses map[int64]chan rawResponse

	// Diagnostics collected per file URI. Populated asynchronously by the
	// server via textDocument/publishDiagnostics notifications.
	diagMu      sync.Mutex
	diagnostics map[string][]Diagnostic

	// openDocs tracks which URIs we've sent didOpen for, to avoid
	// resending on repeat calls against the same file.
	openMu   sync.Mutex
	openDocs map[string]bool

	closed chan struct{}
	dead   error
}

// rawResponse carries the response id + payload from the read loop to the
// waiting caller. Either result or err (as a JSON-RPC error) is populated.
type rawResponse struct {
	Result json.RawMessage `json:"result,omitempty"`
	Error  *RPCError       `json:"error,omitempty"`
}

// RPCError is the JSON-RPC 2.0 error object.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("lsp rpc %d: %s", e.Code, e.Message)
}

// Diagnostic is a single diagnostic as returned by the server. We keep only
// the fields the tool surfaces to the LLM — callers don't need the full LSP
// struct (related info, tags, data, etc.).
type Diagnostic struct {
	Range    Range  `json:"range"`
	Severity int    `json:"severity,omitempty"` // 1=error, 2=warning, 3=info, 4=hint
	Code     any    `json:"code,omitempty"`
	Source   string `json:"source,omitempty"`
	Message  string `json:"message"`
}

// Range is an LSP range (zero-based).
type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

// Position is an LSP position (zero-based line, zero-based UTF-16 character).
type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

// NewClient spawns the given language-server command and attaches stdio.
// The caller must invoke Initialize with the workspace root before issuing
// any text-document requests.
func NewClient(ctx context.Context, command string, args ...string) (*Client, error) {
	cmd := exec.CommandContext(ctx, command, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("lsp: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("lsp: stdout pipe: %w", err)
	}
	// We deliberately discard stderr — language servers emit enormous
	// amounts of it, and we don't want it cluttering agent logs. Hook it
	// up to a file if you need to debug a specific server.
	cmd.Stderr = io.Discard

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("lsp: start %q: %w", command, err)
	}

	c := &Client{
		cmd:         cmd,
		stdin:       stdin,
		stdout:      stdout,
		responses:   make(map[int64]chan rawResponse),
		diagnostics: make(map[string][]Diagnostic),
		openDocs:    make(map[string]bool),
		closed:      make(chan struct{}),
	}
	go c.readLoop()
	return c, nil
}

// Close terminates the server process and releases resources. Safe to call
// more than once.
func (c *Client) Close() error {
	select {
	case <-c.closed:
		return nil
	default:
	}
	close(c.closed)
	_ = c.stdin.Close()
	// Give the server a brief chance to exit cleanly after EOF on stdin.
	done := make(chan error, 1)
	go func() { done <- c.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		_ = c.cmd.Process.Kill()
		<-done
	}
	return nil
}

// Dead returns nil while the client is healthy, or the terminal error if
// the server process has exited or the transport broke. Managers use this
// to evict dead clients from their cache.
func (c *Client) Dead() error {
	return c.dead
}

// ─── JSON-RPC transport ──────────────────────────────────────────────────────

// send writes a single framed JSON-RPC message to the server.
func (c *Client) send(payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(body))
	if _, err := c.stdin.Write([]byte(header)); err != nil {
		return err
	}
	if _, err := c.stdin.Write(body); err != nil {
		return err
	}
	return nil
}

// Request sends a JSON-RPC request and waits for the matching response or
// the context deadline. Callers must use this for any method that expects
// a result; fire-and-forget notifications go through Notify.
func (c *Client) Request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if c.dead != nil {
		return nil, c.dead
	}

	c.reqMu.Lock()
	c.reqID++
	id := c.reqID
	c.reqMu.Unlock()

	ch := make(chan rawResponse, 1)
	c.respMu.Lock()
	c.responses[id] = ch
	c.respMu.Unlock()
	defer func() {
		c.respMu.Lock()
		delete(c.responses, id)
		c.respMu.Unlock()
	}()

	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
	}
	if params != nil {
		payload["params"] = params
	}
	if err := c.send(payload); err != nil {
		return nil, fmt.Errorf("lsp send %s: %w", method, err)
	}

	select {
	case resp := <-ch:
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.closed:
		return nil, fmt.Errorf("lsp: client closed")
	}
}

// Notify sends a JSON-RPC notification (no id, no response expected).
func (c *Client) Notify(method string, params any) error {
	if c.dead != nil {
		return c.dead
	}
	payload := map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
	}
	if params != nil {
		payload["params"] = params
	}
	return c.send(payload)
}

// readLoop consumes framed messages from the server and dispatches responses
// to waiting callers, or caches diagnostics for later retrieval.
func (c *Client) readLoop() {
	defer func() {
		// Notify all outstanding waiters that the transport is dead.
		c.respMu.Lock()
		for _, ch := range c.responses {
			close(ch)
		}
		c.responses = map[int64]chan rawResponse{}
		c.respMu.Unlock()
	}()

	reader := bufio.NewReader(c.stdout)
	for {
		select {
		case <-c.closed:
			return
		default:
		}

		// Read headers until a blank line.
		var contentLength int
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				c.dead = fmt.Errorf("lsp: read header: %w", err)
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				break
			}
			if strings.HasPrefix(line, "Content-Length:") {
				lenStr := strings.TrimSpace(strings.TrimPrefix(line, "Content-Length:"))
				n, convErr := strconv.Atoi(lenStr)
				if convErr != nil {
					c.dead = fmt.Errorf("lsp: bad content-length %q", lenStr)
					return
				}
				contentLength = n
			}
		}
		if contentLength == 0 {
			continue
		}
		body := make([]byte, contentLength)
		if _, err := io.ReadFull(reader, body); err != nil {
			c.dead = fmt.Errorf("lsp: read body: %w", err)
			return
		}

		// Inspect the envelope. Messages come in three flavours:
		//   - responses (id present, method absent)
		//   - requests (id present, method present) — we ignore them
		//   - notifications (id absent, method present)
		var env struct {
			ID     *int64          `json:"id,omitempty"`
			Method string          `json:"method,omitempty"`
			Result json.RawMessage `json:"result,omitempty"`
			Error  *RPCError       `json:"error,omitempty"`
			Params json.RawMessage `json:"params,omitempty"`
		}
		if err := json.Unmarshal(body, &env); err != nil {
			continue // malformed; ignore and keep reading
		}
		if env.ID != nil && env.Method == "" {
			c.respMu.Lock()
			ch, ok := c.responses[*env.ID]
			c.respMu.Unlock()
			if ok {
				ch <- rawResponse{Result: env.Result, Error: env.Error}
			}
			continue
		}
		// Server -> client request (id + method both present). Some
		// servers expect us to reply to requests like
		// workspace/configuration or client/registerCapability; if we
		// don't, they can stall waiting for the response. We reply with
		// a generic null result for any unhandled request — the server
		// will treat missing config as "defaults", which is what we
		// want. `workspace/configuration` specifically expects an array
		// of configs, so we special-case it.
		if env.ID != nil && env.Method != "" {
			var reply any
			if env.Method == "workspace/configuration" {
				// Spec: respond with one entry per requested item. We
				// don't know the count without parsing; an array with
				// one empty object works for most servers.
				reply = []any{map[string]any{}}
			} else {
				reply = nil
			}
			_ = c.send(map[string]any{
				"jsonrpc": "2.0",
				"id":      *env.ID,
				"result":  reply,
			})
			continue
		}
		// Notification — the only one we care about is diagnostics.
		if env.Method == "textDocument/publishDiagnostics" && len(env.Params) > 0 {
			var diag struct {
				URI         string       `json:"uri"`
				Diagnostics []Diagnostic `json:"diagnostics"`
			}
			if err := json.Unmarshal(env.Params, &diag); err == nil {
				c.diagMu.Lock()
				c.diagnostics[diag.URI] = diag.Diagnostics
				c.diagMu.Unlock()
			}
		}
	}
}

// Diagnostics returns (a copy of) the last-seen diagnostic set for the URI.
// Language servers publish diagnostics asynchronously after a file is
// opened or changed; callers typically call OpenDoc then wait briefly
// before reading.
func (c *Client) Diagnostics(uri string) []Diagnostic {
	c.diagMu.Lock()
	defer c.diagMu.Unlock()
	src := c.diagnostics[uri]
	out := make([]Diagnostic, len(src))
	copy(out, src)
	return out
}

// ─── Lifecycle helpers ───────────────────────────────────────────────────────

// Initialize performs the LSP handshake: sends `initialize`, waits for the
// response, and sends the `initialized` notification. rootURI must be a
// file:// URI for the workspace root.
//
// Modern LSP clients must advertise `workspaceFolders` in addition to (or
// instead of) the legacy `rootUri`. Tsserver specifically will fall back to
// single-file inferred-project mode (`"No Project"` errors on
// workspace/symbol) if workspaceFolders is missing — even when rootUri is
// present — because it needs the folder to locate tsconfig.json.
//
// After the initialize response we send:
//   - the `initialized` notification (required by spec; tsserver defers
//     project indexing until it arrives)
//   - `workspace/didChangeConfiguration` with empty settings to nudge the
//     server into starting background indexing immediately
//
// CRITICAL: tsserver is *lazy* — even with workspaceFolders present, it
// will not load the TypeScript program (and will reply "No Project" to
// workspace/symbol) until the client opens at least one file from the
// workspace. Callers must invoke SeedOpen or OpenDoc on a real .ts/.tsx
// file from the repo before issuing any workspace-scoped request.
// Initialize does NOT perform the seed-open itself because it doesn't
// know the language — that's the Manager's job.
func (c *Client) Initialize(ctx context.Context, rootURI string) error {
	repoName := filepath.Base(strings.TrimPrefix(rootURI, "file://"))
	params := map[string]any{
		"processId": os.Getpid(),
		"rootUri":   rootURI, // legacy field, some servers still require
		"workspaceFolders": []map[string]any{
			{"uri": rootURI, "name": repoName},
		},
		"capabilities": map[string]any{
			"textDocument": map[string]any{
				"synchronization":    map[string]any{"dynamicRegistration": false},
				"hover":              map[string]any{"dynamicRegistration": false},
				"definition":         map[string]any{"dynamicRegistration": false},
				"references":         map[string]any{"dynamicRegistration": false},
				"documentSymbol":     map[string]any{"dynamicRegistration": false},
				"publishDiagnostics": map[string]any{"relatedInformation": false},
			},
			"workspace": map[string]any{
				"workspaceFolders": true,
				"symbol":           map[string]any{"dynamicRegistration": false},
				// NOTE: we deliberately do NOT advertise
				// workspace.configuration: the server would then send
				// workspace/configuration RPCs back to us, which our
				// read loop would have to reply to. tsserver works fine
				// without it; falling back to the server's defaults.
			},
		},
	}
	if _, err := c.Request(ctx, "initialize", params); err != nil {
		return fmt.Errorf("lsp initialize: %w", err)
	}
	if err := c.Notify("initialized", map[string]any{}); err != nil {
		return fmt.Errorf("lsp initialized notification: %w", err)
	}
	// Best-effort nudge — tsserver uses this to (re)evaluate project config.
	// Errors here are non-fatal; the server may simply not implement it.
	_ = c.Notify("workspace/didChangeConfiguration", map[string]any{"settings": map[string]any{}})
	return nil
}

// WaitUntilReady polls `workspace/symbol` with a generic probe query until
// the server returns a stable non-trivial result (project fully indexed)
// or the deadline expires. Tsserver loads a TypeScript project in two
// stages after the first didOpen:
//  1. it builds the minimal "program" for the opened file (fast — the
//     single-file results become queryable in ~100ms), then
//  2. it continues scanning the broader tsconfig "include"/"files" and
//     enriching the symbol table (slow — can take 1-5s for big repos).
//
// If we accept a non-zero result at stage 1, workspace/symbol will return
// only the seed file's symbols and silently mis-report "Location" as
// missing. We therefore require the count to both exceed a small minimum
// AND be stable across two consecutive polls before declaring ready.
//
// maxWait caps the total poll window (pass 0 for a 15s default). The
// function returns:
//   - nil when the project is confirmed loaded
//   - an error if every probe failed within the deadline
//
// Callers that want a fail-fast bring-up (recommended) should treat a
// non-nil return as "LSP client unhealthy — do not register".
func (c *Client) WaitUntilReady(ctx context.Context, maxWait time.Duration) error {
	if maxWait <= 0 {
		maxWait = 15 * time.Second
	}
	// Probe with a single-letter query that should match many symbols in
	// any non-trivial TS/JS project. We reuse the same query so
	// consecutive hit-counts are directly comparable.
	const probeQuery = "a"
	// minimumHits is a conservative floor — real projects return 50-500+
	// matches for "a". A post-stage-1 result (seed file only) usually has
	// <10. We pick 10 as the cutoff.
	const minimumHits = 10

	deadline := time.Now().Add(maxWait)
	var lastErr error
	var prevHits = -1
	backoff := 150 * time.Millisecond
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.closed:
			return fmt.Errorf("lsp: client closed while waiting for ready")
		default:
		}

		reqCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		raw, err := c.Request(reqCtx, "workspace/symbol", map[string]any{"query": probeQuery})
		cancel()
		if err != nil {
			// "No Project" during warm-up is expected; retry. Same for
			// generic transport errors in case the server is still booting.
			lastErr = err
			prevHits = -1
		} else {
			var symbols []json.RawMessage
			if jsonErr := json.Unmarshal(raw, &symbols); jsonErr == nil {
				hits := len(symbols)
				// Stability check: two consecutive polls with hits above
				// the floor confirm the background scan is done. This
				// rules out the stage-1 "just the seed file" state.
				if hits >= minimumHits && hits == prevHits {
					return nil
				}
				prevHits = hits
				lastErr = fmt.Errorf("lsp: workspace/symbol probe hits=%d (awaiting stable>=%d)", hits, minimumHits)
			} else {
				lastErr = fmt.Errorf("lsp: workspace/symbol probe bad json: %w", jsonErr)
				prevHits = -1
			}
		}

		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return ctx.Err()
		}
		if backoff < time.Second {
			backoff = backoff * 3 / 2
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("deadline exceeded")
	}
	return fmt.Errorf("lsp: project not ready after %s: %w", maxWait, lastErr)
}

// OpenDoc sends textDocument/didOpen for the given file, unless this
// client already knows the file is open. Diagnostics typically start
// arriving shortly after.
func (c *Client) OpenDoc(uri, languageID, text string) error {
	c.openMu.Lock()
	if c.openDocs[uri] {
		c.openMu.Unlock()
		return nil
	}
	c.openDocs[uri] = true
	c.openMu.Unlock()

	return c.Notify("textDocument/didOpen", map[string]any{
		"textDocument": map[string]any{
			"uri":        uri,
			"languageId": languageID,
			"version":    1,
			"text":       text,
		},
	})
}
