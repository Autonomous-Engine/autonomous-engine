package lsp

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Manager lazily spawns and caches one LSP client per (repoRoot, language)
// combination. Clients are kept warm across calls so the server only pays
// the (expensive) TypeScript project-load cost once.
//
// The zero value is not usable — construct via NewManager.
//
// Readiness (R1): a workspace that failed its readiness probe is
// remembered per (repoRoot, language) for a cooldown (LSP_READY_COOLDOWN,
// default 10m). During the cooldown ClientForRepo returns the cached error
// at once: no spawn, no probe wait. Concurrent callers share one in-flight
// probe. Ready reports the cached state without blocking; Warm starts a
// probe for callers that want the server up before they advertise LSP
// tools. The first probe ever made for a key waits LSP_READY_TIMEOUT
// (default 30s); later probes wait 15s.
type Manager struct {
	mu      sync.Mutex
	clients map[string]*Client // key = repoRoot + "|" + language

	// detector overrides language detection for tests.
	detector func(repoRoot string) (language string, command string, args []string, err error)

	// start spawns, initializes and readiness-probes one server. Tests
	// replace it with a fake spawner.
	start func(ctx context.Context, repoRoot, language, command string, args []string, readyTimeout time.Duration) (*Client, error)
	// now is the clock for the cooldown; tests replace it.
	now func() time.Time

	failures map[string]readinessFailure // key → last failed probe
	probed   map[string]bool             // keys probed at least once
	inflight map[string]chan struct{}    // key → closed when its probe ends
}

type readinessFailure struct {
	err error
	at  time.Time
}

const (
	defaultReadyCooldown     = 10 * time.Minute
	defaultFirstReadyTimeout = 30 * time.Second
	defaultReadyTimeout      = 15 * time.Second
)

// NewManager returns a Manager with the default language detector.
func NewManager() *Manager {
	return &Manager{
		clients:  make(map[string]*Client),
		detector: defaultDetectLanguage,
		start:    startClient,
		now:      time.Now,
		failures: make(map[string]readinessFailure),
		probed:   make(map[string]bool),
		inflight: make(map[string]chan struct{}),
	}
}

// envDuration reads a duration env var ("90s", "10m", or plain seconds).
func envDuration(name string, def time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return d
	}
	var secs int
	if _, err := fmt.Sscanf(v, "%d", &secs); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	return def
}

func readyCooldown() time.Duration { return envDuration("LSP_READY_COOLDOWN", defaultReadyCooldown) }

func firstReadyTimeout() time.Duration {
	return envDuration("LSP_READY_TIMEOUT", defaultFirstReadyTimeout)
}

// Ready reports, without blocking or spawning, whether a live client for
// repoRoot's language is cached. False while a probe is running, during a
// failure cooldown, and before any probe.
func (m *Manager) Ready(repoRoot string) bool {
	language, _, _, err := m.detector(repoRoot)
	if err != nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.clients[repoRoot+"|"+language]
	return ok && c.Dead() == nil
}

// Warm probes repoRoot's language server unless it is already ready, being
// probed, or inside a failure cooldown. It blocks for the probe; callers
// run it in a goroutine with a context that outlives one request.
func (m *Manager) Warm(ctx context.Context, repoRoot string) {
	language, _, _, err := m.detector(repoRoot)
	if err != nil {
		return
	}
	key := repoRoot + "|" + language
	m.mu.Lock()
	if c, ok := m.clients[key]; ok && c.Dead() == nil {
		m.mu.Unlock()
		return
	}
	if _, busy := m.inflight[key]; busy {
		m.mu.Unlock()
		return
	}
	if f, ok := m.failures[key]; ok && m.now().Sub(f.at) < readyCooldown() {
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()
	_, _, _ = m.ClientForRepo(ctx, repoRoot)
}

// Close shuts down every cached LSP client. Callers invoke this during
// agent shutdown; Manager is otherwise safe to leak.
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, c := range m.clients {
		_ = c.Close()
		delete(m.clients, k)
	}
}

// ClientForRepo returns (creating if necessary) the LSP client for the
// repoRoot's dominant language. The second return value is the detected
// language identifier (e.g. "typescript", "go") so callers can include it
// in didOpen notifications.
func (m *Manager) ClientForRepo(ctx context.Context, repoRoot string) (*Client, string, error) {
	language, command, args, err := m.detector(repoRoot)
	if err != nil {
		return nil, "", err
	}
	key := repoRoot + "|" + language

	for {
		m.mu.Lock()
		existing, ok := m.clients[key]
		if ok && existing.Dead() == nil {
			m.mu.Unlock()
			return existing, language, nil
		}
		if ok {
			// Stale client — drop it and fall through to spawn a fresh one.
			_ = existing.Close()
			delete(m.clients, key)
		}
		if f, failed := m.failures[key]; failed {
			if m.now().Sub(f.at) < readyCooldown() {
				m.mu.Unlock()
				return nil, language, fmt.Errorf("lsp: %s workspace not ready (cached failure, next probe after %s): %w",
					language, f.at.Add(readyCooldown()).Format(time.RFC3339), f.err)
			}
			delete(m.failures, key)
		}
		if wait, busy := m.inflight[key]; busy {
			// Another caller is probing this workspace: share its outcome.
			m.mu.Unlock()
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				return nil, language, fmt.Errorf("lsp: %s workspace not ready: %w", language, ctx.Err())
			}
		}
		done := make(chan struct{})
		m.inflight[key] = done
		timeout := defaultReadyTimeout
		first := !m.probed[key]
		if first {
			timeout = firstReadyTimeout()
			m.probed[key] = true
		}
		m.mu.Unlock()

		client, err := m.start(ctx, repoRoot, language, command, args, timeout)

		m.mu.Lock()
		delete(m.inflight, key)
		close(done)
		switch {
		case err == nil:
			m.clients[key] = client
			delete(m.failures, key)
		case ctx.Err() != nil:
			// The caller's own deadline or cancellation ended the probe
			// (validateAfterWrite allows 10s, a tool call 15s, the first
			// probe 30s). That says nothing about the server: cache nothing,
			// and let the next probe still get the first-probe timeout.
			if first {
				delete(m.probed, key)
			}
		default:
			m.failures[key] = readinessFailure{err: err, at: m.now()}
		}
		m.mu.Unlock()
		if err != nil {
			return nil, language, err
		}
		return client, language, nil
	}
}

// startClient spawns the language server, initializes it, seed-opens one
// source file and waits up to readyTimeout for the workspace to answer.
func startClient(ctx context.Context, repoRoot, language, command string, args []string, readyTimeout time.Duration) (*Client, error) {
	// Per-language runtime arg injection. Some LSP servers won't start
	// without a workspace / data directory, but the directory must be
	// derived from repoRoot (so every repo gets its own index cache) —
	// which is something the detector can't know on its own. Inject here.
	//
	// jdtls (Java) requires `-data <workspace-dir>`: the persistent
	// metadata directory it writes `.plugins`, `.metadata`, index cache,
	// etc. into. Without it, the bin/jdtls wrapper prints usage and
	// exits. One dir per repoRoot so parallel repos don't collide.
	// /tmp is ephemeral, but within a container lifetime the LSP manager
	// reuses the same client, so the first-run index cost is paid once
	// per container. For persistent cache across restarts, the operator
	// would mount a volume at /var/lib/jdtls-workspaces and we'd point
	// there instead — left as a follow-up if first-run latency bites.
	if language == "java" {
		workspace := filepath.Join(os.TempDir(), "jdtls-workspace", hashPath(repoRoot))
		_ = os.MkdirAll(workspace, 0755)
		args = append(args, "-data", workspace)
	}

	client, err := NewClient(ctx, command, args...)
	if err != nil {
		return nil, fmt.Errorf("lsp: spawn %s: %w", command, err)
	}

	rootURI := PathToURI(repoRoot)
	if err := client.Initialize(ctx, rootURI); err != nil {
		_ = client.Close()
		return nil, err
	}

	// Seed-open at least one source file from the workspace. tsserver is
	// lazy: it refuses to build the TypeScript program (and replies
	// "No Project" to workspace/symbol) until a file from the project is
	// opened. gopls is not lazy in the same way but a seed-open is still
	// cheap and guarantees it starts the analysis pipeline immediately.
	seedPath, seedLang, seedErr := findSeedFile(repoRoot, language)
	if seedErr == nil {
		data, readErr := os.ReadFile(seedPath)
		if readErr == nil {
			_ = client.OpenDoc(PathToURI(seedPath), seedLang, string(data))
		}
	}

	// Workspace-scoped smoke probe: tsserver indexes async after the
	// seed-open; any workspace/symbol query during that window returns
	// "No Project" or empty. WaitUntilReady polls workspace/symbol with
	// a generic query until the server returns a non-empty result — at
	// which point the project is confirmed loaded. If the 15s budget
	// expires without a healthy response, we refuse to register the
	// client so the caller gets a clear up-front error rather than a
	// silent degradation to "per-file methods work, workspace scope
	// doesn't".
	readyCtx, cancel := context.WithTimeout(ctx, readyTimeout)
	readyErr := client.WaitUntilReady(readyCtx, readyTimeout)
	cancel()
	if readyErr != nil {
		_ = client.Close()
		return nil, fmt.Errorf("lsp: %s workspace not ready: %w", language, readyErr)
	}
	return client, nil
}

// findSeedFile walks the repo (skipping node_modules/.git/vendor) for the
// first file in the dominant source language. Returns (absolutePath,
// languageId, nil) on success. The languageId follows the LSP spec
// (textDocument/didOpen `languageId` field).
func findSeedFile(repoRoot, language string) (string, string, error) {
	exts := map[string][]string{
		"typescript": {".ts", ".tsx"},
		"go":         {".go"},
		"python":     {".py"},
		"rust":       {".rs"},
		"java":       {".java"},
		"csharp":     {".cs"},
		"cpp":        {".cpp", ".cc", ".cxx", ".c", ".hpp", ".h"},
		"php":        {".php"},
		"html":       {".html", ".htm"},
		"css":        {".css", ".scss", ".sass", ".less"},
		"assembly":   {".s", ".asm", ".S", ".inc"},
	}[language]
	if len(exts) == 0 {
		return "", "", fmt.Errorf("lsp: no seed extensions for language %q", language)
	}

	var foundPath, foundLang string
	walkErr := filepath.WalkDir(repoRoot, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // keep walking; perms errors on transient files are fine
		}
		base := d.Name()
		if d.IsDir() {
			switch base {
			case "node_modules", ".git", ".next", "dist", "build", "vendor", "__pycache__":
				return filepath.SkipDir
			}
			return nil
		}
		ext := filepath.Ext(base)
		for _, e := range exts {
			if ext == e {
				foundPath = p
				switch ext {
				case ".ts":
					foundLang = "typescript"
				case ".tsx":
					foundLang = "typescriptreact"
				case ".go":
					foundLang = "go"
				case ".py":
					foundLang = "python"
				case ".rs":
					foundLang = "rust"
				case ".java":
					foundLang = "java"
				case ".cs":
					foundLang = "csharp"
				case ".cpp", ".cc", ".cxx", ".hpp":
					foundLang = "cpp"
				case ".c", ".h":
					foundLang = "c"
				case ".php":
					foundLang = "php"
				case ".html", ".htm":
					foundLang = "html"
				case ".css":
					foundLang = "css"
				case ".scss":
					foundLang = "scss"
				case ".sass":
					foundLang = "sass"
				case ".less":
					foundLang = "less"
				case ".s", ".asm", ".S", ".inc":
					foundLang = "assembly"
				default:
					foundLang = language
				}
				return filepath.SkipAll
			}
		}
		return nil
	})
	if walkErr != nil && foundPath == "" {
		return "", "", walkErr
	}
	if foundPath == "" {
		return "", "", fmt.Errorf("lsp: no %s source file found under %s", language, repoRoot)
	}
	return foundPath, foundLang, nil
}

// hashPath produces a stable, filesystem-safe short identifier for a path.
// Used to derive per-repo jdtls workspace directories without path-length
// or special-char headaches. Not cryptographic — we just need collision
// resistance for typical repo-root counts (a few dozen), which a
// truncated FNV-1a gives us in 16 hex chars.
func hashPath(p string) string {
	var h uint64 = 1469598103934665603
	const prime uint64 = 1099511628211
	for i := 0; i < len(p); i++ {
		h ^= uint64(p[i])
		h *= prime
	}
	return fmt.Sprintf("%016x", h)
}

// PathToURI turns an absolute filesystem path into a `file://` URI. It does
// NOT verify the path exists — callers stat as needed.
func PathToURI(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = p
	}
	abs = filepath.ToSlash(abs)
	// On Windows we need an extra slash — `file:///C:/foo` — but the agents
	// run on Linux containers so we keep it straightforward.
	if !strings.HasPrefix(abs, "/") {
		abs = "/" + abs
	}
	u := &url.URL{Scheme: "file", Path: abs}
	return u.String()
}

// URIToPath is the inverse of PathToURI. Used when diagnostics come back
// keyed by URI and we want to present repo-relative paths to the LLM.
func URIToPath(u string) string {
	if strings.HasPrefix(u, "file://") {
		return strings.TrimPrefix(u, "file://")
	}
	return u
}

// ─── Language detection ──────────────────────────────────────────────────────

// defaultDetectLanguage inspects the repo root to pick a language server.
// Resolution is by characteristic project markers — the dominant file
// the project declares itself with. A polyglot repo gets whichever
// marker is checked first; operators can override by placing the
// dominant marker at the root. Commands must be on PATH inside the
// container; agent Dockerfiles install them.
//
// Returned tuple: (languageID, command, args, err).
func defaultDetectLanguage(repoRoot string) (string, string, []string, error) {
	has := func(name string) bool {
		_, err := os.Stat(filepath.Join(repoRoot, name))
		return err == nil
	}
	hasGlob := func(pattern string) bool {
		matches, _ := filepath.Glob(filepath.Join(repoRoot, pattern))
		return len(matches) > 0
	}

	switch {
	// TypeScript / JavaScript (same server).
	case has("tsconfig.json"), has("package.json"):
		return "typescript", "typescript-language-server", []string{"--stdio"}, nil
	case has("go.mod"):
		return "go", "gopls", nil, nil
	// Python: pyright-langserver ships with the `pyright` npm package.
	case has("pyproject.toml"), has("requirements.txt"), has("setup.py"), has("Pipfile"):
		return "python", "pyright-langserver", []string{"--stdio"}, nil
	// Rust.
	case has("Cargo.toml"):
		return "rust", "rust-analyzer", nil, nil
	// Java: jdtls is launched with a launcher script; the install provides
	// `jdtls` on PATH. Uses stdio by default.
	case has("pom.xml"), has("build.gradle"), has("build.gradle.kts"):
		return "java", "jdtls", nil, nil
	// C#: csharp-ls (Roslyn-based, pure .NET tool). Launched without args
	// because it speaks LSP over stdio by default. We picked csharp-ls over
	// OmniSharp because the latter's tarball release plus .NET runtime does
	// not have a clean Alpine install path, whereas csharp-ls layers on top
	// of the `dotnet8-sdk` Alpine package that the agent Dockerfiles already
	// install for running customer `dotnet test` commands.
	case hasGlob("*.sln"), hasGlob("*.csproj"):
		return "csharp", "csharp-ls", nil, nil
	// C / C++: clangd handles both. Prefer repos with a compile database
	// or CMakeLists; fall through here based on top-level markers.
	case has("CMakeLists.txt"), has("compile_commands.json"), has("Makefile"), hasGlob("*.c"), hasGlob("*.cpp"), hasGlob("*.cc"), hasGlob("*.h"), hasGlob("*.hpp"):
		return "cpp", "clangd", nil, nil
	// PHP: phpactor (open-source; intelephense requires a license).
	case has("composer.json"):
		return "php", "phpactor", []string{"language-server"}, nil
	// HTML / CSS: the vscode-langservers-extracted bundle provides both.
	// Detection by file presence because there's rarely a project marker
	// at the root of a static site.
	case hasGlob("*.html"), has("index.html"):
		return "html", "vscode-html-language-server", []string{"--stdio"}, nil
	case hasGlob("*.css"), hasGlob("*.scss"):
		return "css", "vscode-css-language-server", []string{"--stdio"}, nil
	// Assembly: no canonical project marker — detect by file presence.
	// Placed LATE so a mixed project isn't misdetected as assembly just
	// because it contains a single `.s` / `.asm` file alongside code for
	// a real project (Cargo.toml, go.mod, etc., all checked above).
	case hasGlob("*.s"), hasGlob("*.asm"), hasGlob("*.S"), hasGlob("*.inc"):
		return "assembly", "asm-lsp", nil, nil
	default:
		return "", "", nil, fmt.Errorf("lsp: no language server configured for repo at %q (no recognised project marker — tsconfig.json, package.json, go.mod, Cargo.toml, pom.xml, *.sln, CMakeLists.txt, composer.json, pyproject.toml, or source-file fallback)", repoRoot)
	}
}
