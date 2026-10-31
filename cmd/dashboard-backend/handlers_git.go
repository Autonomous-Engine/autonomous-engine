package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// GitHandlers provides HTTP handlers for browsing git repositories.
type GitHandlers struct {
	db *pgxpool.Pool
}

// NewGitHandlers creates a GitHandlers instance.
func NewGitHandlers(db *pgxpool.Pool) *GitHandlers {
	return &GitHandlers{db: db}
}

// repoInfo holds the result of a repository lookup.
type repoInfo struct {
	ClonePath  string
	BaseBranch string
}

// lookupRepo fetches clone_path and base_branch for a repository by ID.
func (h *GitHandlers) lookupRepo(ctx context.Context, repoID uuid.UUID) (*repoInfo, error) {
	var clonePath, baseBranch string
	err := h.db.QueryRow(ctx,
		"SELECT clone_path, base_branch FROM repositories WHERE id = $1", repoID,
	).Scan(&clonePath, &baseBranch)
	if err != nil {
		return nil, fmt.Errorf("repository not found: %w", err)
	}
	if clonePath == "" {
		return nil, fmt.Errorf("repository not cloned")
	}
	if _, statErr := os.Stat(filepath.Join(clonePath, ".git")); statErr != nil {
		if _, dirErr := os.Stat(clonePath); dirErr != nil {
			return nil, fmt.Errorf("repository not cloned")
		}
		return nil, fmt.Errorf("repository clone is incomplete or corrupt")
	}
	if baseBranch == "" {
		baseBranch = "main"
	}
	return &repoInfo{ClonePath: clonePath, BaseBranch: baseBranch}, nil
}

// validateGitRef returns true if the ref string is safe to use in a git command.
// It rejects refs containing characters that could enable command injection.
func validateGitRef(ref string) bool {
	if ref == "" {
		return true
	}
	for _, dangerous := range []string{"..", ";", "|", "&", "$", "`", "\n", "\r", "'", "\""} {
		if strings.Contains(ref, dangerous) {
			return false
		}
	}
	return true
}

// validateGitPath returns true if the path string is safe to use in a git command.
func validateGitPath(path string) bool {
	if path == "" {
		return true
	}
	for _, dangerous := range []string{"..", ";", "|", "&", "$", "`", "\n", "\r"} {
		if strings.Contains(path, dangerous) {
			return false
		}
	}
	return true
}

// ─── RepoTree ────────────────────────────────────────────────────────────────

// treeEntry represents a single entry in a git tree listing.
type treeEntry struct {
	Name string `json:"name"`
	Type string `json:"type"` // "blob" or "tree"
	Path string `json:"path"`
}

// RepoTree handles GET /api/repos/tree?repo_id=UUID&ref=REF&path=PATH.
// Lists directory contents of a git repository at a given ref and path.
func (h *GitHandlers) RepoTree(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	repoIDStr := r.URL.Query().Get("repo_id")
	if repoIDStr == "" {
		writeError(w, http.StatusBadRequest, "repo_id is required")
		return
	}
	repoID, err := uuid.Parse(repoIDStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid repo_id format")
		return
	}

	repo, err := h.lookupRepo(r.Context(), repoID)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	ref := r.URL.Query().Get("ref")
	if !validateGitRef(ref) {
		writeError(w, http.StatusBadRequest, "invalid ref")
		return
	}
	if ref == "" {
		ref = repo.BaseBranch
	}

	path := r.URL.Query().Get("path")
	if !validateGitPath(path) {
		writeError(w, http.StatusBadRequest, "invalid path")
		return
	}

	// Build the git ls-tree command. Without --name-only we get mode/type info.
	args := []string{"ls-tree", ref}
	if path != "" {
		args = append(args, "--", path+"/")
	}

	cmd := exec.CommandContext(r.Context(), "git", args...)
	cmd.Dir = repo.ClonePath
	output, err := cmd.CombinedOutput()
	if err != nil {
		outStr := string(output)
		if strings.Contains(outStr, "Not a valid object name") || strings.Contains(outStr, "fatal:") {
			writeError(w, http.StatusNotFound, "ref or path not found")
			return
		}
		log.Printf("git-handlers: ls-tree error: %v: %s", err, outStr)
		writeError(w, http.StatusInternalServerError, "git ls-tree failed")
		return
	}

	// Parse output lines: each line is "mode type hash\tname"
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	entries := make([]treeEntry, 0, len(lines))
	for _, line := range lines {
		if line == "" {
			continue
		}
		// Split on tab to separate metadata from name
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		name := parts[1]
		// metadata is "mode type hash"
		meta := strings.Fields(parts[0])
		if len(meta) < 2 {
			continue
		}
		entryType := meta[1] // "blob" or "tree"

		// Build full path for the entry
		entryPath := name
		if path != "" && !strings.HasPrefix(name, path+"/") {
			entryPath = path + "/" + name
		}

		entries = append(entries, treeEntry{
			Name: nameFromPath(name),
			Type: entryType,
			Path: entryPath,
		})
	}

	repoPath := path
	if repoPath == "" {
		repoPath = ""
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"entries":   entries,
		"ref":       ref,
		"repo_path": repoPath,
	})
}

// nameFromPath extracts the last path component from a full path.
func nameFromPath(path string) string {
	path = strings.TrimSuffix(path, "/")
	if idx := strings.LastIndex(path, "/"); idx >= 0 {
		return path[idx+1:]
	}
	return path
}

// ─── RepoBlob ────────────────────────────────────────────────────────────────

// RepoBlob handles GET /api/repos/blob?repo_id=UUID&ref=REF&path=PATH.
// Returns the content of a file at a given ref and path.
func (h *GitHandlers) RepoBlob(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	repoIDStr := r.URL.Query().Get("repo_id")
	if repoIDStr == "" {
		writeError(w, http.StatusBadRequest, "repo_id is required")
		return
	}
	repoID, err := uuid.Parse(repoIDStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid repo_id format")
		return
	}

	path := r.URL.Query().Get("path")
	if path == "" {
		writeError(w, http.StatusBadRequest, "path is required")
		return
	}
	if !validateGitPath(path) {
		writeError(w, http.StatusBadRequest, "invalid path")
		return
	}

	repo, err := h.lookupRepo(r.Context(), repoID)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	ref := r.URL.Query().Get("ref")
	if !validateGitRef(ref) {
		writeError(w, http.StatusBadRequest, "invalid ref")
		return
	}
	if ref == "" {
		ref = repo.BaseBranch
	}

	// Get file size from ls-tree -l
	var fileSize int64
	lsCmd := exec.CommandContext(r.Context(), "git", "ls-tree", "-l", ref, "--", path)
	lsCmd.Dir = repo.ClonePath
	lsOutput, err := lsCmd.CombinedOutput()
	if err != nil {
		outStr := string(lsOutput)
		if strings.Contains(outStr, "Not a valid object name") || strings.Contains(outStr, "fatal:") {
			writeError(w, http.StatusNotFound, "ref or path not found")
			return
		}
		log.Printf("git-handlers: ls-tree -l error: %v: %s", err, outStr)
		writeError(w, http.StatusInternalServerError, "git ls-tree failed")
		return
	}
	lsLine := strings.TrimSpace(string(lsOutput))
	if lsLine == "" {
		writeError(w, http.StatusNotFound, "file not found")
		return
	}
	// Parse: "mode type hash size\tname"
	if tabIdx := strings.Index(lsLine, "\t"); tabIdx > 0 {
		fields := strings.Fields(lsLine[:tabIdx])
		if len(fields) >= 4 {
			fileSize, _ = strconv.ParseInt(fields[3], 10, 64)
		}
	}

	// Get file content
	showCmd := exec.CommandContext(r.Context(), "git", "show", ref+":"+path)
	showCmd.Dir = repo.ClonePath
	content, err := showCmd.Output()
	if err != nil {
		writeError(w, http.StatusNotFound, "file not found at ref")
		return
	}

	// Check for binary content (null bytes in first 512 bytes)
	checkLen := len(content)
	if checkLen > 512 {
		checkLen = 512
	}
	if bytes.ContainsRune(content[:checkLen], 0) {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"binary": true,
			"path":   path,
			"size":   fileSize,
			"ref":    ref,
		})
		return
	}

	// Truncate to 1MB if needed
	const maxSize = 1024 * 1024
	truncated := false
	contentStr := string(content)
	if len(content) > maxSize {
		contentStr = string(content[:maxSize])
		truncated = true
	}

	resp := map[string]interface{}{
		"content": contentStr,
		"path":    path,
		"size":    fileSize,
		"ref":     ref,
	}
	if truncated {
		resp["truncated"] = true
	}
	writeJSON(w, http.StatusOK, resp)
}

// ─── RepoCommits ─────────────────────────────────────────────────────────────

// commitEntry represents a single commit in a log listing.
type commitEntry struct {
	SHA         string `json:"sha"`
	AuthorName  string `json:"author_name"`
	AuthorEmail string `json:"author_email"`
	Date        string `json:"date"`
	Message     string `json:"message"`
}

// RepoCommits handles GET /api/repos/commits?repo_id=UUID&ref=REF&page=1&per_page=30.
// Returns a paginated list of commits for a repository.
func (h *GitHandlers) RepoCommits(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	repoIDStr := r.URL.Query().Get("repo_id")
	if repoIDStr == "" {
		writeError(w, http.StatusBadRequest, "repo_id is required")
		return
	}
	repoID, err := uuid.Parse(repoIDStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid repo_id format")
		return
	}

	repo, err := h.lookupRepo(r.Context(), repoID)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	ref := r.URL.Query().Get("ref")
	if !validateGitRef(ref) {
		writeError(w, http.StatusBadRequest, "invalid ref")
		return
	}
	if ref == "" {
		ref = repo.BaseBranch
	}

	page := 1
	if p := r.URL.Query().Get("page"); p != "" {
		if v, err := strconv.Atoi(p); err == nil && v > 0 {
			page = v
		}
	}

	perPage := 30
	if pp := r.URL.Query().Get("per_page"); pp != "" {
		if v, err := strconv.Atoi(pp); err == nil && v > 0 {
			perPage = v
		}
	}
	if perPage > 100 {
		perPage = 100
	}

	skip := (page - 1) * perPage

	// Get total count
	countCmd := exec.CommandContext(r.Context(), "git", "rev-list", "--count", ref)
	countCmd.Dir = repo.ClonePath
	countOutput, err := countCmd.CombinedOutput()
	if err != nil {
		outStr := string(countOutput)
		if strings.Contains(outStr, "unknown revision") {
			writeError(w, http.StatusNotFound, "ref not found")
			return
		}
		log.Printf("git-handlers: rev-list --count error: %v: %s", err, outStr)
		writeError(w, http.StatusInternalServerError, "git rev-list failed")
		return
	}
	total, _ := strconv.Atoi(strings.TrimSpace(string(countOutput)))

	// Get commits
	format := "%H|||%an|||%ae|||%at|||%s"
	logCmd := exec.CommandContext(r.Context(), "git", "log",
		"--format="+format,
		fmt.Sprintf("--skip=%d", skip),
		fmt.Sprintf("-n%d", perPage),
		ref,
	)
	logCmd.Dir = repo.ClonePath
	logOutput, err := logCmd.CombinedOutput()
	if err != nil {
		log.Printf("git-handlers: git log error: %v: %s", err, string(logOutput))
		writeError(w, http.StatusInternalServerError, "git log failed")
		return
	}

	lines := strings.Split(strings.TrimSpace(string(logOutput)), "\n")
	commits := make([]commitEntry, 0, len(lines))
	for _, line := range lines {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "|||", 5)
		if len(parts) != 5 {
			continue
		}

		// Convert Unix timestamp to ISO 8601
		ts, _ := strconv.ParseInt(parts[3], 10, 64)
		date := time.Unix(ts, 0).UTC().Format(time.RFC3339)

		commits = append(commits, commitEntry{
			SHA:         parts[0],
			AuthorName:  parts[1],
			AuthorEmail: parts[2],
			Date:        date,
			Message:     parts[4],
		})
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"commits":  commits,
		"total":    total,
		"page":     page,
		"per_page": perPage,
	})
}

// ─── RepoCommit ──────────────────────────────────────────────────────────────

// RepoCommit handles GET /api/repos/commit?repo_id=UUID&sha=SHA.
// Returns detailed information about a single commit including the diff.
func (h *GitHandlers) RepoCommit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	repoIDStr := r.URL.Query().Get("repo_id")
	if repoIDStr == "" {
		writeError(w, http.StatusBadRequest, "repo_id is required")
		return
	}
	repoID, err := uuid.Parse(repoIDStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid repo_id format")
		return
	}

	sha := r.URL.Query().Get("sha")
	if sha == "" {
		writeError(w, http.StatusBadRequest, "sha is required")
		return
	}
	if !validateGitRef(sha) {
		writeError(w, http.StatusBadRequest, "invalid sha")
		return
	}

	repo, err := h.lookupRepo(r.Context(), repoID)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	// Get commit info with stats
	format := "%H|||%an|||%ae|||%at|||%s|||%b"
	showCmd := exec.CommandContext(r.Context(), "git", "show", "--stat", "--format="+format, sha)
	showCmd.Dir = repo.ClonePath
	showOutput, err := showCmd.CombinedOutput()
	if err != nil {
		outStr := string(showOutput)
		if strings.Contains(outStr, "unknown revision") || strings.Contains(outStr, "bad object") {
			writeError(w, http.StatusNotFound, "commit not found")
			return
		}
		log.Printf("git-handlers: git show error: %v: %s", err, outStr)
		writeError(w, http.StatusInternalServerError, "git show failed")
		return
	}

	showStr := string(showOutput)

	// The first line of output contains the formatted commit info.
	// Everything after the first blank line pair is the stat output.
	// The format output ends where the stat section begins (after a blank line).
	var commitSHA, authorName, authorEmail, date, message, body, stats string

	// Split at the first newline to get the formatted line vs the rest
	lines := strings.SplitN(showStr, "\n", 2)
	if len(lines) >= 1 {
		parts := strings.SplitN(lines[0], "|||", 6)
		if len(parts) >= 5 {
			commitSHA = parts[0]
			authorName = parts[1]
			authorEmail = parts[2]
			ts, _ := strconv.ParseInt(parts[3], 10, 64)
			date = time.Unix(ts, 0).UTC().Format(time.RFC3339)
			message = parts[4]
			if len(parts) == 6 {
				body = strings.TrimSpace(parts[5])
			}
		}
	}
	if len(lines) == 2 {
		stats = strings.TrimSpace(lines[1])
		// The body may have spilled into the stats section if it contained
		// newlines. The body from the format ends where a blank line
		// separates it from the stat summary. We keep the stats as-is here
		// since git show --stat appends after the format output.
	}

	// Get the full diff (cap at 500KB)
	diffCmd := exec.CommandContext(r.Context(), "git", "diff", sha+"^.."+sha)
	diffCmd.Dir = repo.ClonePath
	diffOutput, diffErr := diffCmd.CombinedOutput()
	diffStr := ""
	if diffErr == nil {
		const maxDiff = 500 * 1024
		if len(diffOutput) > maxDiff {
			diffStr = string(diffOutput[:maxDiff]) + "\n... (truncated)"
		} else {
			diffStr = string(diffOutput)
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"sha":          commitSHA,
		"author_name":  authorName,
		"author_email": authorEmail,
		"date":         date,
		"message":      message,
		"body":         body,
		"stats":        stats,
		"diff":         diffStr,
	})
}

// ─── RepoBranches ────────────────────────────────────────────────────────────

// RepoBranches handles GET /api/repos/branches?repo_id=UUID.
// Returns a list of branches for a repository.
func (h *GitHandlers) RepoBranches(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	repoIDStr := r.URL.Query().Get("repo_id")
	if repoIDStr == "" {
		writeError(w, http.StatusBadRequest, "repo_id is required")
		return
	}
	repoID, err := uuid.Parse(repoIDStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid repo_id format")
		return
	}

	repo, err := h.lookupRepo(r.Context(), repoID)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	// List remote branches
	branchCmd := exec.CommandContext(r.Context(), "git", "branch", "-r", "--format=%(refname:short)")
	branchCmd.Dir = repo.ClonePath
	branchOutput, err := branchCmd.CombinedOutput()
	if err != nil {
		log.Printf("git-handlers: git branch error: %v: %s", err, string(branchOutput))
		writeError(w, http.StatusInternalServerError, "git branch failed")
		return
	}

	lines := strings.Split(strings.TrimSpace(string(branchOutput)), "\n")
	branches := make([]string, 0, len(lines))
	for _, line := range lines {
		branch := strings.TrimSpace(line)
		if branch == "" {
			continue
		}
		// Strip origin/ prefix
		branch = strings.TrimPrefix(branch, "origin/")
		// Filter out HEAD
		if branch == "HEAD" || strings.HasPrefix(branch, "HEAD ") {
			continue
		}
		branches = append(branches, branch)
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"branches":       branches,
		"default_branch": repo.BaseBranch,
	})
}
