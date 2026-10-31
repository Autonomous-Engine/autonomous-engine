package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/products"
	"github.com/autonomousengine/autonomous-engine/internal/testdetect"
	"github.com/autonomousengine/autonomous-engine/internal/verifypolicy"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ─── Repository verification policy (M3-03) ──────────────────────────────────
//
// Onboarding asks the operator, per repository, as a structured step rather
// than free text: the test command (detected, editable), the required
// checks, and what should happen when no executable check exists (block
// preselected). Block is the default; accepting a
// repository with no executable check requires this explicit answer.

// onboardingPolicySetBy is recorded as set_by for policies written through
// onboarding, which has no authenticated user.
const onboardingPolicySetBy = "onboarding wizard"

// policyBackend is the persistence and detection surface the policy step
// needs; the production implementation is dbPolicyBackend, tests use a fake.
type policyBackend interface {
	Repository(ctx context.Context, repoID uuid.UUID) (*products.Repository, error)
	Load(ctx context.Context, repoID uuid.UUID) (verifypolicy.Policy, bool, error)
	Save(ctx context.Context, p verifypolicy.Policy) (verifypolicy.Policy, error)
	// Detect returns the detected test command, whether a clone could be
	// inspected, and the repository language.
	Detect(clonePath string) (command string, cloneAvailable bool, language string)
}

type dbPolicyBackend struct{ s *server }

func (b dbPolicyBackend) Repository(ctx context.Context, repoID uuid.UUID) (*products.Repository, error) {
	if b.s.productStore == nil {
		return nil, errors.New("product store not available")
	}
	return b.s.productStore.GetRepository(ctx, repoID)
}

func (b dbPolicyBackend) Load(ctx context.Context, repoID uuid.UUID) (verifypolicy.Policy, bool, error) {
	if b.s.pool == nil {
		return verifypolicy.Default(repoID), false, errors.New("database not available")
	}
	return verifypolicy.NewStore(b.s.pool).LoadVerificationPolicy(ctx, repoID)
}

func (b dbPolicyBackend) Save(ctx context.Context, p verifypolicy.Policy) (verifypolicy.Policy, error) {
	if b.s.pool == nil {
		return verifypolicy.Policy{}, errors.New("database not available")
	}
	// Onboarding is reachable without authentication: it may only create a
	// policy or replace its own, never an admin-owned one (M3-03).
	return verifypolicy.UpsertOnboardingTx(ctx, b.s.pool, p)
}

func (b dbPolicyBackend) Detect(clonePath string) (string, bool, string) {
	return detectForPolicy(clonePath)
}

// detectForPolicy inspects a clone: the test command internal/testdetect
// would pick (the same detection the implementation agent runs) and the
// language used to seed security-review paths.
func detectForPolicy(clonePath string) (string, bool, string) {
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

func (s *server) policies() policyBackend {
	if s.policyStore != nil {
		return s.policyStore
	}
	return dbPolicyBackend{s: s}
}

// verificationPolicyRequest is the POST body: the repository plus the
// operator's answers.
type verificationPolicyRequest struct {
	RepositoryID string `json:"repository_id"`
	verifypolicy.Input
}

// verificationPolicyHandler serves the onboarding policy step.
//
//	GET  /api/onboarding/verification-policy?repository_id=<uuid>
//	     -> verifypolicy.View: the stored policy (found=true) or the proposal
//	        (fail-closed default, block preselected, language-seeded
//	        security paths), plus the detected test command.
//	POST /api/onboarding/verification-policy
//	     {repository_id, test_command, required_checks, no_checks_policy,
//	      security_review_paths} -> the stored View (source=onboarding).
//	     no_checks_policy is required.
func (s *server) verificationPolicyHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.getVerificationPolicy(w, r)
	case http.MethodPost, http.MethodPut:
		s.saveVerificationPolicy(w, r)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

func (s *server) policyView(repo *products.Repository, p verifypolicy.Policy, found bool) verifypolicy.View {
	detected, cloneOK, lang := s.policies().Detect(repo.ClonePath)
	return verifypolicy.NewView(repo.ID, p, found, detected, cloneOK, lang)
}

func (s *server) getVerificationPolicy(w http.ResponseWriter, r *http.Request) {
	repoID, err := uuid.Parse(strings.TrimSpace(r.URL.Query().Get("repository_id")))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "valid repository_id is required"})
		return
	}
	be := s.policies()
	repo, err := be.Repository(r.Context(), repoID)
	if err != nil || repo == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "repository not found"})
		return
	}
	p, found, err := be.Load(r.Context(), repoID)
	if err != nil {
		log.Printf("onboarding: load verification policy for %s: %v", repoID, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to load verification policy"})
		return
	}
	writeJSON(w, http.StatusOK, s.policyView(repo, p, found))
}

func (s *server) saveVerificationPolicy(w http.ResponseWriter, r *http.Request) {
	var req verificationPolicyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	repoID, err := uuid.Parse(strings.TrimSpace(req.RepositoryID))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "valid repository_id is required"})
		return
	}
	p, err := req.Input.ToPolicy(repoID, verifypolicy.SourceOnboarding, onboardingPolicySetBy)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	be := s.policies()
	repo, err := be.Repository(r.Context(), repoID)
	if err != nil || repo == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "repository not found"})
		return
	}
	stored, err := be.Save(r.Context(), p)
	if errors.Is(err, verifypolicy.ErrNotOnboardingOwned) {
		// An administrator owns this policy; onboarding (unauthenticated)
		// must not relax it. The row is unchanged.
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "this repository's verification policy was set by an administrator and cannot be changed from onboarding; edit it on the admin repository page",
		})
		return
	}
	if err != nil {
		log.Printf("onboarding: save verification policy for %s: %v", repoID, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save verification policy"})
		return
	}
	log.Printf("onboarding: verification policy for repository %s set: no_checks_policy=%s required_checks=%v test_command_override=%t security_paths=%d",
		repoID, stored.NoChecksPolicy, stored.RequiredChecks, stored.TestCommand != nil, len(stored.SecurityReviewPaths))
	writeJSON(w, http.StatusOK, s.policyView(repo, stored, true))
}

// addRepositoryWithPolicy inserts the repository and, when the operator
// answered the verification-policy questions with the add-repo request, the
// policy row in the same transaction: either both exist or neither does.
// Without answers no row is written and the fail-closed default (block)
// governs the repository until the policy step stores one.
func (s *server) addRepositoryWithPolicy(ctx context.Context, in *verifypolicy.Input, productID uuid.UUID, name, githubURL, baseBranch, mergeStrategy, branchPrefix string) (*products.Repository, error) {
	if in == nil {
		return s.productStore.AddRepository(ctx, productID, name, githubURL, baseBranch, mergeStrategy, branchPrefix)
	}
	// Validate before touching the database so a bad answer is a 400, not a
	// rolled-back insert.
	if _, err := in.ToPolicy(uuid.New(), verifypolicy.SourceOnboarding, onboardingPolicySetBy); err != nil {
		return nil, &policyInputError{err: err}
	}
	var repo *products.Repository
	err := workgraph.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		r, err := s.productStore.AddRepositoryTx(ctx, tx, productID, name, githubURL, baseBranch, mergeStrategy, branchPrefix)
		if err != nil {
			return err
		}
		p, err := in.ToPolicy(r.ID, verifypolicy.SourceOnboarding, onboardingPolicySetBy)
		if err != nil {
			return &policyInputError{err: err}
		}
		if _, err := verifypolicy.UpsertOnboardingTx(ctx, tx, p); err != nil {
			return fmt.Errorf("store verification policy: %w", err)
		}
		repo = r
		return nil
	})
	if err != nil {
		return nil, err
	}
	return repo, nil
}

// policyInputError marks an invalid verification-policy answer (a 400).
type policyInputError struct{ err error }

func (e *policyInputError) Error() string { return "invalid verification_policy: " + e.err.Error() }
func (e *policyInputError) Unwrap() error { return e.err }
