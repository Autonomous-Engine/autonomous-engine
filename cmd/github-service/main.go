package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/config"
	"github.com/autonomousengine/autonomous-engine/internal/configstore"
	"github.com/autonomousengine/autonomous-engine/internal/crypto"
	"github.com/autonomousengine/autonomous-engine/internal/db"
	"github.com/autonomousengine/autonomous-engine/internal/gitremote"
	"github.com/autonomousengine/autonomous-engine/internal/guards"
	"github.com/autonomousengine/autonomous-engine/internal/health"
	"github.com/autonomousengine/autonomous-engine/internal/products"
	gogithub "github.com/google/go-github/v60/github"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	agentID       = "github-service"
	healthPort    = 8085
	workspaceBase = "/workspace/repos"
)

// cloneRequest is the body for POST /clone.
type cloneRequest struct {
	RepositoryID string `json:"repository_id"`
}

// cloneAllRequest is the body for POST /clone-all.
type cloneAllRequest struct {
	ProductID string `json:"product_id"`
}

// pullRequest is the body for POST /pull.
type pullRequest struct {
	RepositoryID string `json:"repository_id"`
}

// branchRequest is the body for POST /branch.
type branchRequest struct {
	RepositoryID string `json:"repository_id"`
	BranchName   string `json:"branch_name"`
	Base         string `json:"base"`
}

// commitRequest is the body for POST /commit.
type commitRequest struct {
	RepositoryID string            `json:"repository_id"`
	Message      string            `json:"message"`
	Files        map[string]string `json:"files"` // path -> content
}

// createPRRequest is the body for POST /pr.
type createPRRequest struct {
	RepositoryID string `json:"repository_id"`
	Title        string `json:"title"`
	Body         string `json:"body"`
	Head         string `json:"head"`
	Base         string `json:"base"`
}

// reviewCommentRequest is the body for POST /review-comment.
type reviewCommentRequest struct {
	RepositoryID string `json:"repository_id"`
	PRNumber     int    `json:"pr_number"`
	Body         string `json:"body"`
	Path         string `json:"path"`
	Line         int    `json:"line"`
	CommitID     string `json:"commit_id"`
}

// mergePRRequest is used as context for POST /merge/:pr.
type mergePRRequest struct {
	RepositoryID string `json:"repository_id"`
	MergeMethod  string `json:"merge_method"`
}

// genericResponse is returned for most endpoints.
type genericResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message,omitempty"`
}

// repoCloneResult reports clone status for a single repo.
type repoCloneResult struct {
	RepositoryID string `json:"repository_id"`
	Name         string `json:"name"`
	Success      bool   `json:"success"`
	Message      string `json:"message,omitempty"`
}

// svc holds dependencies for the github-service.
type svc struct {
	cfg          *config.Config
	pool         *pgxpool.Pool
	productStore *products.ProductStore
	configStore  *configstore.ConfigStore
	ghFactory    func(token string) *gogithub.Client
	mu           sync.RWMutex
}

func newSvc(cfg *config.Config, pool *pgxpool.Pool, cs *configstore.ConfigStore) *svc {
	return &svc{
		cfg:          cfg,
		pool:         pool,
		productStore: products.NewStore(pool),
		configStore:  cs,
		ghFactory: func(token string) *gogithub.Client {
			if token == "" {
				return gogithub.NewClient(nil)
			}
			return gogithub.NewClient(nil).WithAuthToken(token)
		},
	}
}

// resolveRepo looks up a repository by ID and returns the repo record plus the
// token to use (per-repo override, or org-level default from config).
func (s *svc) resolveRepo(ctx context.Context, repositoryID string) (*products.Repository, string, error) {
	id, err := uuid.Parse(repositoryID)
	if err != nil {
		return nil, "", fmt.Errorf("invalid repository_id: %w", err)
	}
	repo, err := s.productStore.GetRepository(ctx, id)
	if err != nil {
		return nil, "", fmt.Errorf("lookup repository: %w", err)
	}
	// G1: a remote that is not on github.com (local or bare mirror,
	// file://, another host) never gets a GitHub token, so neither the token
	// nor the GitHub credential gate applies to it.
	if kind := repo.Kind(); !kind.IsGitHub() {
		if kind == gitremote.KindInvalid {
			return nil, "", fmt.Errorf("repository %s has an invalid remote; fix its github_url", repo.Name)
		}
		scrubOriginCredentials(repo)
		return repo, "", nil
	}
	// Override → credential registry → GITHUB_TOKEN (backward compat); the
	// order is shared with product-agent's base-SHA resolution (M2-01).
	token := gitremote.ResolveToken(ctx, s.credentials(), repo.TokenOverride, s.cfg.GitHub.Token)

	// Credential health gate — blocks git operations when the effective
	// GitHub credential is missing, expired, or invalid. The guard resolves
	// the token in the same order as above and re-probes GitHub when the
	// recorded outcome is older than its TTL (guards.CheckGitHubCredential).
	if s.configStore != nil {
		if v := guards.CheckGitHubCredential(ctx, s.configStore, repo.TokenOverride); !v.Valid {
			return nil, "", fmt.Errorf("github credential check failed: %s", v.Message)
		}
	}

	return repo, token, nil
}

// scrubOriginCredentials removes credentials that a clone made before G1 left
// in remote.origin.url of a repository whose remote is not on GitHub (the old
// URL builder put the PAT into any https:// URL). Best-effort: a clone that
// is missing, unreadable or already clean is left alone; every later pull,
// push or fetch through that origin then goes without the PAT.
func scrubOriginCredentials(repo *products.Repository) {
	if repo.ClonePath == "" || repo.Kind().IsGitHub() {
		return
	}
	out, err := runGitInDir(repo.ClonePath, "config", "--get", "remote.origin.url")
	origin := strings.TrimSpace(out)
	if err != nil || !strings.Contains(origin, "@") {
		return
	}
	u, perr := url.Parse(origin)
	if perr != nil || u.User == nil || (u.Scheme != "https" && u.Scheme != "http") {
		return
	}
	u.User = nil
	if _, err := runGitInDir(repo.ClonePath, "remote", "set-url", "origin", "--", u.String()); err != nil {
		log.Printf("github-service: repository %s: could not remove credentials from the clone's origin: %v", repo.Name, err)
		return
	}
	log.Printf("github-service: repository %s: removed credentials from the clone's origin (%s remote, made before G1)", repo.Name, repo.Kind())
}

// credentials is the config store as a gitremote.CredentialResolver: nil
// (not a typed nil pointer) when no store is configured.
func (s *svc) credentials() gitremote.CredentialResolver {
	if s.configStore == nil {
		return nil
	}
	return s.configStore
}

// noGitHubRemoteError: the repository's remote is not on github.com, so no
// GitHub API call (PR create/merge/link/comment) applies to it. Handlers
// answer 409; pr-coordinator routes such repositories through the manual PR
// gate before ever calling github-service.
type noGitHubRemoteError struct {
	kind gitremote.Kind
}

func (e *noGitHubRemoteError) Error() string {
	return fmt.Sprintf("repository has no GitHub remote (remote kind %s): GitHub PRs are not available for it; use the manual PR gate and merge the branch by hand", e.kind)
}

// resolveOwnerRepo extracts owner/repo from a GitHub remote: owner/repo,
// https://github.com/owner/repo(.git), git@github.com:owner/repo.git or
// ssh://git@github.com/owner/repo. Any other remote is a
// *noGitHubRemoteError.
func resolveOwnerRepo(githubURL string) (owner, repo string, err error) {
	kind, norm := gitremote.Classify(githubURL)
	if !kind.IsGitHub() {
		return "", "", &noGitHubRemoteError{kind: kind}
	}
	var path string
	if kind == gitremote.KindGitHubSSH && !strings.Contains(norm, "://") {
		path = norm[strings.Index(norm, ":")+1:] // scp-like git@github.com:owner/repo
	} else {
		u, perr := url.Parse(norm)
		if perr != nil {
			return "", "", fmt.Errorf("parse GitHub remote: %w", perr)
		}
		path = u.Path
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	parts := strings.SplitN(path, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("GitHub remote %q does not name owner/repo", githubURL)
	}
	return parts[0], parts[1], nil
}

// ownerRepoOrRespond resolves owner/repo for a GitHub API handler, or writes
// the error (409 for a non-GitHub remote) and returns ok=false.
func ownerRepoOrRespond(w http.ResponseWriter, repo *products.Repository, op string) (owner, name string, ok bool) {
	owner, name, err := resolveOwnerRepo(repo.GitHubURL)
	if err == nil {
		return owner, name, true
	}
	var ng *noGitHubRemoteError
	if errors.As(err, &ng) {
		log.Printf("github-service: %s for repository %s skipped: %v", op, repo.Name, err)
		jsonResponse(w, http.StatusConflict, genericResponse{Success: false, Message: err.Error()})
		return "", "", false
	}
	jsonResponse(w, http.StatusBadRequest, genericResponse{Success: false, Message: err.Error()})
	return "", "", false
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("github-service: load config: %v", err)
	}

	pool, err := db.NewPool(ctx, cfg.Database.URL)
	if err != nil {
		log.Fatalf("github-service: connect db: %v", err)
	}
	defer pool.Close()

	// Ensure workspace base directory exists.
	if err := os.MkdirAll(workspaceBase, 0o755); err != nil {
		log.Printf("github-service: create workspace base: %v", err)
	}

	var cs *configstore.ConfigStore
	masterKeyStr := os.Getenv("MASTER_ENCRYPTION_KEY")
	if masterKeyStr != "" {
		mk, err := crypto.LoadMasterKey(masterKeyStr)
		if err == nil {
			cs = configstore.New(pool, mk)
			// Listen on pg_notify('config_changed') so UI updates to credentials/
			// endpoints flush the in-memory cache without a restart.
			cs.StartWatcher(ctx)
		}
	}

	service := newSvc(cfg, pool, cs)

	checker := health.New(agentID)
	checker.AddCheck("database", func() error {
		return pool.Ping(ctx)
	})

	mux := http.NewServeMux()
	mux.HandleFunc("/clone", service.handleClone(ctx))
	mux.HandleFunc("/clone-all", service.handleCloneAll(ctx))
	mux.HandleFunc("/pull", service.handlePull(ctx))
	mux.HandleFunc("/branch", service.handleBranch(ctx))
	mux.HandleFunc("/commit", service.handleCommit(ctx))
	mux.HandleFunc("/pr", service.handleCreatePR(ctx))
	mux.HandleFunc("/pr/link", service.handleLinkPR(ctx)) // M2-06: pair contract PRs
	mux.HandleFunc("/diff/", service.handleGetDiff(ctx))
	mux.HandleFunc("/review-comment", service.handleReviewComment(ctx))
	mux.HandleFunc("/merge/", service.handleMergePR(ctx))
	mux.HandleFunc("/health", checker.Handler())

	addr := fmt.Sprintf(":%d", healthPort)
	srv := &http.Server{Addr: addr, Handler: mux}

	go func() {
		log.Printf("github-service: listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("github-service: server error: %v", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("github-service: shutting down...")
	cancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("github-service: shutdown error: %v", err)
	}
	log.Println("github-service: stopped")
}

func (s *svc) handleClone(appCtx context.Context) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req cloneRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		if req.RepositoryID == "" {
			http.Error(w, "repository_id required", http.StatusBadRequest)
			return
		}
		repo, token, err := s.resolveRepo(r.Context(), req.RepositoryID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		clonePath := repo.ClonePath
		if err := os.MkdirAll(clonePath, 0o755); err != nil {
			jsonResponse(w, http.StatusInternalServerError, genericResponse{Success: false, Message: err.Error()})
			return
		}
		out, err := cloneRepository(repo, token)
		if err != nil {
			log.Printf("github-service: clone %s: %v: %s", repo.Name, err, out)
			jsonResponse(w, http.StatusInternalServerError, genericResponse{Success: false, Message: err.Error()})
			return
		}
		log.Printf("github-service: cloned repo %s to %s", repo.Name, clonePath)
		jsonResponse(w, http.StatusOK, genericResponse{Success: true, Message: fmt.Sprintf("cloned %s", repo.Name)})
	}
}

// cloneArgs is the git clone invocation for a repository: options first,
// then "--", so a remote value can never be read as a git option. The token
// reaches the URL only through gitremote.AuthenticatedURL, which attaches it
// to an https://github.com URL and to nothing else.
func cloneArgs(repo *products.Repository, token string) []string {
	args := []string{"clone"}
	if repo.BaseBranch != "" {
		args = append(args, "--branch", repo.BaseBranch)
	}
	return append(args, "--", gitremote.AuthenticatedURL(repo.GitHubURL, token), repo.ClonePath)
}

// cloneRepository replaces repo.ClonePath with a fresh clone of the
// repository's remote. It works for every valid remote kind: GitHub over
// https (with the token), and a local or bare mirror, file:// URL or other
// host (never with the token). Output and error are redacted.
func cloneRepository(repo *products.Repository, token string) (string, error) {
	if k := repo.Kind(); k == gitremote.KindInvalid {
		return "", fmt.Errorf("repository %s has an invalid remote; fix its github_url", repo.Name)
	} else if !k.IsGitHub() {
		token = ""
	}
	os.RemoveAll(repo.ClonePath) //nolint:errcheck
	out, err := runGit(cloneArgs(repo, token)...)
	out = gitremote.Redact(out, token)
	if err != nil {
		return out, errors.New(gitremote.Redact(err.Error(), token))
	}
	return out, nil
}

func (s *svc) handleCloneAll(appCtx context.Context) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req cloneAllRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		if req.ProductID == "" {
			http.Error(w, "product_id required", http.StatusBadRequest)
			return
		}
		productID, err := uuid.Parse(req.ProductID)
		if err != nil {
			http.Error(w, "invalid product_id", http.StatusBadRequest)
			return
		}
		repos, err := s.productStore.GetRepositoriesForProduct(r.Context(), productID)
		if err != nil {
			http.Error(w, fmt.Sprintf("lookup repos: %v", err), http.StatusInternalServerError)
			return
		}
		if len(repos) == 0 {
			http.Error(w, "no repositories configured for this product", http.StatusPreconditionFailed)
			return
		}
		var results []repoCloneResult
		for _, repo := range repos {
			if !repo.Enabled {
				continue
			}
			// ResolveCredential returns "" with every error, so the shared
			// helper's err check changes nothing here.
			token := gitremote.ResolveToken(r.Context(), s.credentials(), repo.TokenOverride, s.cfg.GitHub.Token)
			clonePath := repo.ClonePath
			os.MkdirAll(clonePath, 0o755) //nolint:errcheck
			out, cloneErr := cloneRepository(repo, token)
			res := repoCloneResult{
				RepositoryID: repo.ID.String(),
				Name:         repo.Name,
			}
			if cloneErr != nil {
				log.Printf("github-service: clone-all %s: %v: %s", repo.Name, cloneErr, out)
				res.Success = false
				res.Message = cloneErr.Error()
			} else {
				res.Success = true
				res.Message = fmt.Sprintf("cloned to %s", clonePath)
			}
			results = append(results, res)
		}
		jsonResponse(w, http.StatusOK, map[string]interface{}{
			"success": true,
			"results": results,
		})
	}
}

func (s *svc) handlePull(appCtx context.Context) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req pullRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		if req.RepositoryID == "" {
			http.Error(w, "repository_id required", http.StatusBadRequest)
			return
		}
		repo, _, err := s.resolveRepo(r.Context(), req.RepositoryID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		out, err := runGitInDir(repo.ClonePath, "pull")
		if err != nil {
			log.Printf("github-service: pull %s: %v: %s", repo.Name, err, out)
			jsonResponse(w, http.StatusInternalServerError, genericResponse{Success: false, Message: err.Error()})
			return
		}
		jsonResponse(w, http.StatusOK, genericResponse{Success: true, Message: "pulled"})
	}
}

func (s *svc) handleBranch(appCtx context.Context) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req branchRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		if req.RepositoryID == "" || req.BranchName == "" {
			http.Error(w, "repository_id and branch_name required", http.StatusBadRequest)
			return
		}
		repo, _, err := s.resolveRepo(r.Context(), req.RepositoryID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		base := req.Base
		if base == "" {
			base = repo.BaseBranch
		}
		if base == "" {
			base = s.cfg.GitHub.BaseBranch
		}
		branchName := req.BranchName
		if repo.BranchPrefix != "" && !strings.HasPrefix(branchName, repo.BranchPrefix) {
			branchName = repo.BranchPrefix + branchName
		}
		clonePath := repo.ClonePath
		if out, err := runGitInDir(clonePath, "checkout", base); err != nil {
			log.Printf("github-service: checkout base: %v: %s", err, out)
		}
		runGitInDir(clonePath, "pull") //nolint:errcheck
		out, err := runGitInDir(clonePath, "checkout", "-b", branchName)
		if err != nil {
			log.Printf("github-service: create branch %s: %v: %s", branchName, err, out)
			jsonResponse(w, http.StatusInternalServerError, genericResponse{Success: false, Message: err.Error()})
			return
		}
		jsonResponse(w, http.StatusOK, genericResponse{Success: true, Message: fmt.Sprintf("branch %s created", branchName)})
	}
}

func (s *svc) handleCommit(appCtx context.Context) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req commitRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		if req.RepositoryID == "" {
			http.Error(w, "repository_id required", http.StatusBadRequest)
			return
		}
		repo, _, err := s.resolveRepo(r.Context(), req.RepositoryID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		clonePath := repo.ClonePath
		// Write files.
		for path, content := range req.Files {
			fullPath := clonePath + "/" + path
			if err := os.MkdirAll(dirOf(fullPath), 0o755); err != nil {
				log.Printf("github-service: mkdir for %s: %v", path, err)
				continue
			}
			if err := os.WriteFile(fullPath, []byte(content), 0o644); err != nil {
				log.Printf("github-service: write file %s: %v", path, err)
			}
		}
		// Stage all changes.
		if out, err := runGitInDir(clonePath, "add", "-A"); err != nil {
			log.Printf("github-service: git add: %v: %s", err, out)
		}
		// Commit.
		msg := req.Message
		if msg == "" {
			msg = "automated commit by agent"
		}
		if out, err := runGitInDir(clonePath, "commit", "-m", msg, "--allow-empty"); err != nil {
			log.Printf("github-service: git commit: %v: %s", err, out)
			jsonResponse(w, http.StatusInternalServerError, genericResponse{Success: false, Message: err.Error()})
			return
		}
		// Get current branch.
		branch := ""
		out, _ := runGitInDir(clonePath, "rev-parse", "--abbrev-ref", "HEAD")
		branch = strings.TrimSpace(out)
		if out, err := runGitInDir(clonePath, "push", "--set-upstream", "origin", branch); err != nil {
			log.Printf("github-service: git push: %v: %s", err, out)
			jsonResponse(w, http.StatusInternalServerError, genericResponse{Success: false, Message: err.Error()})
			return
		}
		jsonResponse(w, http.StatusOK, genericResponse{Success: true, Message: "committed and pushed"})
	}
}

func (s *svc) handleCreatePR(appCtx context.Context) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req createPRRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		if req.RepositoryID == "" {
			http.Error(w, "repository_id required", http.StatusBadRequest)
			return
		}
		repo, token, err := s.resolveRepo(r.Context(), req.RepositoryID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		owner, repoName, ok := ownerRepoOrRespond(w, repo, "PR creation via the GitHub API")
		if !ok {
			return
		}
		gh := s.ghFactory(token)
		base := req.Base
		if base == "" {
			base = repo.BaseBranch
		}
		newPR := &gogithub.NewPullRequest{
			Title: gogithub.String(req.Title),
			Body:  gogithub.String(req.Body),
			Head:  gogithub.String(req.Head),
			Base:  gogithub.String(base),
		}
		pr, _, err := gh.PullRequests.Create(appCtx, owner, repoName, newPR)
		if err != nil {
			// Idempotency: if a PR already exists for this head → base pair,
			// return it rather than 500ing. This happens whenever the
			// pipeline re-fires implementation.complete for a work item
			// that already pushed its branch to origin — common during
			// retries after the caller failed to parse the prior response.
			if isAlreadyExistsError(err) {
				existing, lookupErr := findExistingPR(appCtx, gh, owner, repoName, req.Head, base)
				if lookupErr == nil && existing != nil {
					log.Printf("github-service: PR already exists for %s → %s, returning #%d", req.Head, base, existing.GetNumber())
					jsonResponse(w, http.StatusOK, map[string]interface{}{
						"success":   true,
						"pr_number": existing.GetNumber(),
						"pr_url":    existing.GetHTMLURL(),
					})
					return
				}
				log.Printf("github-service: PR already exists but lookup failed for %s → %s: %v", req.Head, base, lookupErr)
			}
			log.Printf("github-service: create PR: %v", err)
			jsonResponse(w, http.StatusInternalServerError, genericResponse{Success: false, Message: err.Error()})
			return
		}
		// Response keys MUST match pr-coordinator's createPRResponse struct
		// tags: pr_number + pr_url. Previously this returned "url" and the
		// caller unmarshaled into PRURL (`json:"pr_url"`), silently leaving
		// PRURL empty — the caller then logged "github-service returned no
		// PR" even though the PR had been created successfully on GitHub.
		jsonResponse(w, http.StatusOK, map[string]interface{}{
			"success":   true,
			"pr_number": pr.GetNumber(),
			"pr_url":    pr.GetHTMLURL(),
		})
	}
}

// isAlreadyExistsError reports whether the GitHub API error is the
// "A pull request already exists for <head>" 422 Validation Failed shape.
// The go-github library doesn't surface this as a distinct error type, so
// we fall back to substring detection. Narrow match so this never swallows
// unrelated 422s.
func isAlreadyExistsError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "422") && strings.Contains(msg, "pull request already exists")
}

// findExistingPR returns the open PR whose head matches headBranch and base
// matches baseBranch, if one exists. GitHub's List API needs the head as
// "<owner>:<branch>"; we try both "<owner>:<head>" and bare "<head>" so
// this also works for PRs opened from forks.
func findExistingPR(ctx context.Context, gh *gogithub.Client, owner, repoName, headBranch, baseBranch string) (*gogithub.PullRequest, error) {
	opts := &gogithub.PullRequestListOptions{
		State: "all",
		Head:  owner + ":" + headBranch,
		Base:  baseBranch,
	}
	prs, _, err := gh.PullRequests.List(ctx, owner, repoName, opts)
	if err != nil {
		return nil, err
	}
	if len(prs) > 0 {
		return prs[0], nil
	}
	// Fallback without owner prefix in case the PR was opened from a fork.
	opts.Head = headBranch
	prs, _, err = gh.PullRequests.List(ctx, owner, repoName, opts)
	if err != nil {
		return nil, err
	}
	if len(prs) > 0 {
		return prs[0], nil
	}
	return nil, fmt.Errorf("no existing PR found for head=%s base=%s", headBranch, baseBranch)
}

func (s *svc) handleGetDiff(appCtx context.Context) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		repositoryID := r.URL.Query().Get("repository_id")
		if repositoryID == "" {
			http.Error(w, "repository_id required", http.StatusBadRequest)
			return
		}
		repo, token, err := s.resolveRepo(r.Context(), repositoryID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		owner, repoName, ok := ownerRepoOrRespond(w, repo, "GitHub API call")
		if !ok {
			return
		}
		gh := s.ghFactory(token)
		parts := strings.Split(r.URL.Path, "/")
		prNum := 0
		fmt.Sscanf(parts[len(parts)-1], "%d", &prNum)
		if prNum == 0 {
			http.Error(w, "invalid pr number", http.StatusBadRequest)
			return
		}
		files, _, err := gh.PullRequests.ListFiles(appCtx, owner, repoName, prNum, nil)
		if err != nil {
			log.Printf("github-service: list PR files: %v", err)
			jsonResponse(w, http.StatusInternalServerError, genericResponse{Success: false, Message: err.Error()})
			return
		}
		type fileEntry struct {
			Filename string `json:"filename"`
			Status   string `json:"status"`
			Patch    string `json:"patch"`
		}
		var entries []fileEntry
		for _, f := range files {
			entries = append(entries, fileEntry{
				Filename: f.GetFilename(),
				Status:   f.GetStatus(),
				Patch:    f.GetPatch(),
			})
		}
		jsonResponse(w, http.StatusOK, map[string]interface{}{"success": true, "files": entries})
	}
}

func (s *svc) handleReviewComment(appCtx context.Context) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req reviewCommentRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		if req.RepositoryID == "" {
			http.Error(w, "repository_id required", http.StatusBadRequest)
			return
		}
		repo, token, err := s.resolveRepo(r.Context(), req.RepositoryID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		owner, repoName, ok := ownerRepoOrRespond(w, repo, "GitHub API call")
		if !ok {
			return
		}
		gh := s.ghFactory(token)

		// Two kinds of "review comment" on a GitHub PR:
		//   1. Inline review comment anchored to a file+commit+line — uses
		//      PullRequests.CreateComment and REQUIRES path/commit_id/line.
		//   2. Top-level PR discussion comment (the kind that appears on
		//      the conversation tab) — uses Issues.CreateComment since
		//      GitHub models PRs as issues under the hood.
		// review-agent uses the top-level form for its summary feedback
		// (line=0, path="", commit_id=""). Previously we always called
		// the inline API, which 422'd on the empty anchor — review
		// feedback never landed on the PR. Route based on whether an
		// anchor is present.
		if req.Path == "" && req.CommitID == "" {
			issueComment := &gogithub.IssueComment{Body: gogithub.String(req.Body)}
			_, _, err = gh.Issues.CreateComment(appCtx, owner, repoName, req.PRNumber, issueComment)
			if err != nil {
				log.Printf("github-service: post issue comment on PR #%d: %v", req.PRNumber, err)
				jsonResponse(w, http.StatusInternalServerError, genericResponse{Success: false, Message: err.Error()})
				return
			}
		} else {
			comment := &gogithub.PullRequestComment{
				Body:     gogithub.String(req.Body),
				Path:     gogithub.String(req.Path),
				CommitID: gogithub.String(req.CommitID),
				Position: gogithub.Int(req.Line),
			}
			_, _, err = gh.PullRequests.CreateComment(appCtx, owner, repoName, req.PRNumber, comment)
			if err != nil {
				log.Printf("github-service: post review comment: %v", err)
				jsonResponse(w, http.StatusInternalServerError, genericResponse{Success: false, Message: err.Error()})
				return
			}
		}
		jsonResponse(w, http.StatusOK, genericResponse{Success: true, Message: "comment posted"})
	}
}

func (s *svc) handleMergePR(appCtx context.Context) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		repositoryID := r.URL.Query().Get("repository_id")
		var req mergePRRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err == nil && req.RepositoryID != "" {
			repositoryID = req.RepositoryID
		}
		if repositoryID == "" {
			http.Error(w, "repository_id required", http.StatusBadRequest)
			return
		}
		repo, token, err := s.resolveRepo(r.Context(), repositoryID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		owner, repoName, ok := ownerRepoOrRespond(w, repo, "GitHub API call")
		if !ok {
			return
		}
		gh := s.ghFactory(token)
		parts := strings.Split(r.URL.Path, "/")
		prNum := 0
		fmt.Sscanf(parts[len(parts)-1], "%d", &prNum)
		if prNum == 0 {
			http.Error(w, "invalid pr number", http.StatusBadRequest)
			return
		}
		mergeMethod := req.MergeMethod
		if mergeMethod == "" {
			mergeMethod = repo.MergeStrategy
		}
		if mergeMethod == "" {
			mergeMethod = s.cfg.GitHub.MergeStrategy
		}
		if mergeMethod == "" {
			mergeMethod = "squash"
		}
		opts := &gogithub.PullRequestOptions{MergeMethod: mergeMethod}
		result, _, err := gh.PullRequests.Merge(appCtx, owner, repoName, prNum, "Merged by autonomous engine", opts)
		if err != nil {
			log.Printf("github-service: merge PR %d: %v", prNum, err)
			jsonResponse(w, http.StatusInternalServerError, genericResponse{Success: false, Message: err.Error()})
			return
		}
		jsonResponse(w, http.StatusOK, map[string]interface{}{
			"success": result.GetMerged(),
			"sha":     result.GetSHA(),
			"message": result.GetMessage(),
		})
	}
}

// runGit runs a git command without a working directory.
func runGit(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// runGitInDir runs a git command in the specified directory.
func runGitInDir(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func dirOf(path string) string {
	idx := strings.LastIndexByte(path, '/')
	if idx < 0 {
		return "."
	}
	return path[:idx]
}

func jsonResponse(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}
