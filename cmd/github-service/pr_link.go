package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"

	gogithub "github.com/google/go-github/v60/github"
)

// POST /pr/link (M2-06): append a line to an existing pull request's
// description, once. pr-coordinator uses it to pair the PRs of a
// cross-repository contract: after the consumer's PR exists, each producer's
// PR is edited to link it. Idempotent: a body that already contains the line
// is left alone, so a redelivered completion does not stack duplicates.

type linkPRRequest struct {
	RepositoryID string `json:"repository_id"`
	PRNumber     int    `json:"pr_number"`
	Append       string `json:"append"`
}

// linkedBody returns body with line appended (separated by a blank line),
// and whether that changed anything.
func linkedBody(body, line string) (string, bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.Contains(body, line) {
		return body, false
	}
	if strings.TrimSpace(body) == "" {
		return line, true
	}
	return strings.TrimRight(body, "\n") + "\n\n" + line, true
}

func (s *svc) handleLinkPR(appCtx context.Context) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req linkPRRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		if req.RepositoryID == "" || req.PRNumber <= 0 || strings.TrimSpace(req.Append) == "" {
			http.Error(w, "repository_id, pr_number and append are required", http.StatusBadRequest)
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
		pr, _, err := gh.PullRequests.Get(appCtx, owner, repoName, req.PRNumber)
		if err != nil {
			log.Printf("github-service: link PR #%d: get: %v", req.PRNumber, err)
			jsonResponse(w, http.StatusInternalServerError, genericResponse{Success: false, Message: err.Error()})
			return
		}
		body, changed := linkedBody(pr.GetBody(), req.Append)
		if changed {
			if _, _, err := gh.PullRequests.Edit(appCtx, owner, repoName, req.PRNumber, &gogithub.PullRequest{Body: gogithub.String(body)}); err != nil {
				log.Printf("github-service: link PR #%d: edit: %v", req.PRNumber, err)
				jsonResponse(w, http.StatusInternalServerError, genericResponse{Success: false, Message: err.Error()})
				return
			}
		}
		jsonResponse(w, http.StatusOK, map[string]interface{}{"success": true, "changed": changed, "pr_number": req.PRNumber})
	}
}
