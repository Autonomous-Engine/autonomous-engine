package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/gitremote"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// githubAPIBase is the GitHub REST API root; tests point it at a stub.
var githubAPIBase = "https://api.github.com"

// repoAccessResult is the body of POST /api/admin/repositories/test-access.
// It always travels with HTTP 200: the UI reads accessible and error, so a
// refusal is shown as such instead of a failed request.
type repoAccessResult struct {
	Accessible bool   `json:"accessible"`
	StatusCode int    `json:"status_code,omitempty"`
	RemoteKind string `json:"remote_kind,omitempty"`
	Error      string `json:"error,omitempty"`
}

// checkRepoAccess asks the GitHub API whether token can see the repository
// named by githubURL. A non-GitHub remote (local path, mirror, other host)
// is reported as not checkable, not as inaccessible by the API.
func checkRepoAccess(ctx context.Context, apiBase, token, githubURL string) repoAccessResult {
	owner, repo, err := gitremote.OwnerRepo(githubURL)
	if err != nil {
		var ng *gitremote.NotGitHubError
		if errors.As(err, &ng) {
			return repoAccessResult{RemoteKind: string(ng.Kind), Error: err.Error()}
		}
		return repoAccessResult{Error: err.Error()}
	}
	if token == "" {
		return repoAccessResult{RemoteKind: "github", Error: "no GitHub account connected: add one under Admin → Integrations to check access"}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(apiBase, "/")+"/repos/"+owner+"/"+repo, nil)
	if err != nil {
		return repoAccessResult{RemoteKind: "github", Error: err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return repoAccessResult{RemoteKind: "github", Error: err.Error()}
	}
	defer resp.Body.Close()
	res := repoAccessResult{Accessible: resp.StatusCode == http.StatusOK, StatusCode: resp.StatusCode, RemoteKind: "github"}
	if !res.Accessible {
		res.Error = fmt.Sprintf("GitHub answered %d for %s/%s", resp.StatusCode, owner, repo)
	}
	return res
}

// looksLikeGitHubTokenKey reports whether a credential key name under the
// github category means a personal access token. Admin → Credentials let
// the user type any key name; only the exact "token" was ever found (F8).
func looksLikeGitHubTokenKey(key string) bool {
	k := strings.ToLower(strings.TrimSpace(key))
	k = strings.NewReplacer(" ", "_", "-", "_").Replace(k)
	switch k {
	case "token", "github_token", "pat", "github_pat", "personal_access_token", "access_token":
		return true
	}
	return false
}

// legacyQuerier is the one method findLegacyGitHubToken needs.
type legacyQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// findLegacyGitHubToken finds the global GitHub token from the single-token
// era under any token-like key name.
func findLegacyGitHubToken(ctx context.Context, q legacyQuerier) (uuid.UUID, bool, error) {
	var id uuid.UUID
	err := q.QueryRow(ctx, `
		SELECT id FROM credential_registry
		WHERE scope = 'global' AND category = 'github'
		  AND lower(replace(replace(key_name, ' ', '_'), '-', '_')) IN ('token', 'github_token', 'pat', 'github_pat', 'personal_access_token', 'access_token')
		ORDER BY created_at
		LIMIT 1
	`).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, false, err
	}
	return id, true, nil
}

// githubTokenForAccess resolves the token to check repository access with:
// the named GitHub account, else the first valid account, else the legacy
// global token. TestRepoAccess only ever read the legacy token, so a user
// who added an account the supported way saw "No GitHub token configured".
func (ah *AdminHandlers) githubTokenForAccess(ctx context.Context, accountID string) string {
	if ah.db == nil || ah.store == nil {
		return ""
	}
	var credID uuid.UUID
	if accountID != "" {
		if id, err := uuid.Parse(accountID); err == nil {
			if err := ah.db.QueryRow(ctx, `SELECT credential_id FROM github_accounts WHERE id = $1`, id).Scan(&credID); err == nil {
				if tok, err := ah.store.RevealCredential(ctx, credID, nil, ""); err == nil && tok != "" {
					return tok
				}
			}
		}
	}
	if err := ah.db.QueryRow(ctx, `SELECT credential_id FROM github_accounts ORDER BY valid DESC, created_at LIMIT 1`).Scan(&credID); err == nil {
		if tok, err := ah.store.RevealCredential(ctx, credID, nil, ""); err == nil && tok != "" {
			return tok
		}
	}
	if id, found, err := findLegacyGitHubToken(ctx, ah.db); err == nil && found {
		if tok, err := ah.store.RevealCredential(ctx, id, nil, ""); err == nil {
			return tok
		}
	}
	return ""
}
