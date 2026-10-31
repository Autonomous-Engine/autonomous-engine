package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/testdetect"
	"github.com/autonomousengine/autonomous-engine/internal/verifypolicy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ─── Repository verification policy (M3-03) ──────────────────────────────────
//
// The admin editor for repository_verification_policies (migration 051): the
// same fields onboarding asks for, stored with source='admin'. A repository
// without a stored policy is governed by the fail-closed default — no
// executable checks blocks the run by default — which the
// editor shows as such.

var errRepositoryNotFound = errors.New("repository not found")

// repositoryPolicyBackend is the persistence and detection surface of the
// editor; tests use a fake.
type repositoryPolicyBackend interface {
	// ClonePath returns the repository's clone path, or
	// errRepositoryNotFound.
	ClonePath(ctx context.Context, repoID uuid.UUID) (string, error)
	Load(ctx context.Context, repoID uuid.UUID) (verifypolicy.Policy, bool, error)
	Save(ctx context.Context, p verifypolicy.Policy) (verifypolicy.Policy, error)
	Detect(clonePath string) (command string, cloneAvailable bool, language string)
}

type pgRepositoryPolicyBackend struct{ h *AdminHandlers }

func (b pgRepositoryPolicyBackend) ClonePath(ctx context.Context, repoID uuid.UUID) (string, error) {
	var clonePath string
	err := b.h.db.QueryRow(ctx, `SELECT COALESCE(clone_path, '') FROM repositories WHERE id = $1`, repoID).Scan(&clonePath)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errRepositoryNotFound
	}
	return clonePath, err
}

func (b pgRepositoryPolicyBackend) Load(ctx context.Context, repoID uuid.UUID) (verifypolicy.Policy, bool, error) {
	return verifypolicy.NewStore(b.h.db).LoadVerificationPolicy(ctx, repoID)
}

func (b pgRepositoryPolicyBackend) Save(ctx context.Context, p verifypolicy.Policy) (verifypolicy.Policy, error) {
	return verifypolicy.UpsertTx(ctx, b.h.db, p)
}

func (b pgRepositoryPolicyBackend) Detect(clonePath string) (string, bool, string) {
	clonePath = strings.TrimSpace(clonePath)
	if clonePath == "" {
		return "", false, ""
	}
	entries, err := os.ReadDir(clonePath)
	if err != nil || len(entries) == 0 {
		return "", false, ""
	}
	return testdetect.DetectTestCommand(clonePath), true, verifypolicy.DetectLanguage(clonePath)
}

func (h *AdminHandlers) repositoryPolicies() repositoryPolicyBackend {
	if h.repoPolicy != nil {
		return h.repoPolicy
	}
	return pgRepositoryPolicyBackend{h: h}
}

// RepositoryVerificationPolicy handles
//
//	GET /api/admin/repositories/verification-policy?id=<uuid>
//	    -> verifypolicy.View (stored policy, or the fail-closed default with
//	       found=false, plus the detected test command)
//	PUT /api/admin/repositories/verification-policy?id=<uuid>
//	    {test_command, required_checks, no_checks_policy, security_review_paths}
//	    -> the stored View; source='admin', set_by = the acting user.
func (h *AdminHandlers) RepositoryVerificationPolicy(w http.ResponseWriter, r *http.Request) {
	repoID, err := uuidFromQuery(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	be := h.repositoryPolicies()
	clonePath, err := be.ClonePath(r.Context(), repoID)
	if errors.Is(err, errRepositoryNotFound) {
		writeError(w, http.StatusNotFound, "repository not found")
		return
	}
	if err != nil {
		log.Printf("admin: verification policy: look up repository %s: %v", repoID, err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	switch r.Method {
	case http.MethodGet:
		p, found, err := be.Load(r.Context(), repoID)
		if err != nil {
			log.Printf("admin: load verification policy for %s: %v", repoID, err)
			writeError(w, http.StatusInternalServerError, "failed to load verification policy")
			return
		}
		detected, cloneOK, lang := be.Detect(clonePath)
		writeJSON(w, http.StatusOK, verifypolicy.NewView(repoID, p, found, detected, cloneOK, lang))
	case http.MethodPut, http.MethodPost:
		var in verifypolicy.Input
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		setBy := "admin"
		if actor := actorIDFromCtx(r); actor != nil {
			setBy = "user:" + actor.String()
		}
		p, err := in.ToPolicy(repoID, verifypolicy.SourceAdmin, setBy)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		stored, err := be.Save(r.Context(), p)
		if err != nil {
			log.Printf("admin: save verification policy for %s: %v", repoID, err)
			writeError(w, http.StatusInternalServerError, "failed to save verification policy")
			return
		}
		log.Printf("admin: verification policy for repository %s set by %s: no_checks_policy=%s required_checks=%v test_command_override=%t security_paths=%d",
			repoID, setBy, stored.NoChecksPolicy, stored.RequiredChecks, stored.TestCommand != nil, len(stored.SecurityReviewPaths))
		detected, cloneOK, lang := be.Detect(clonePath)
		writeJSON(w, http.StatusOK, verifypolicy.NewView(repoID, stored, true, detected, cloneOK, lang))
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}
