package main

// Codex zones in onboarding (migration 063; codex coherence design, Part 2).
//
//   - GET  /api/onboarding/zones/proposed?product_id=  the zones each of the
//     product's repositories would get: its existing zones when it has any,
//     otherwise the codex-config.yaml defaults plus the guarded zones derived
//     from the risk_zones answer (the wizard's confirmation step);
//   - POST /api/onboarding/zones {repository_id, zones}  the wizard's
//     confirmation (zones.Store.ConfirmOnboarding). The onboarding service
//     is unauthenticated, so it may never relax state: the first
//     confirmation of a repository sets exactly the chosen zones (none is an
//     explicit, recorded choice); after that it can only add zones with new
//     names, and never changes, unfreezes or deletes an existing zone.
//     Edits, weakening and deletion are admin-only (dashboard API);
//   - approve-codex applies the proposal to every repository whose zones
//     were never set (zones.Store.ApplyIfEmpty), so a confirmed — even
//     empty — or edited set is never overwritten.
//
// Every write records a section:zones codex version (internal/zones).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"

	"github.com/autonomousengine/autonomous-engine/internal/onboarding"
	"github.com/autonomousengine/autonomous-engine/internal/zones"
	"github.com/google/uuid"
)

// repoZoneProposal is one repository's zones in the wizard.
type repoZoneProposal struct {
	RepositoryID   uuid.UUID `json:"repository_id"`
	RepositoryName string    `json:"repository_name"`
	// Existing: the repository's zones are already set (zones present, or
	// confirmed — possibly as an explicit empty choice) and Zones are they;
	// onboarding can then only add zones. Otherwise Zones are the proposal.
	Existing bool             `json:"existing"`
	Zones    []zones.Template `json:"zones"`
	// Frozen names the existing zones that are frozen (shown, kept).
	Frozen []string `json:"frozen,omitempty"`
}

// riskAnswer is the risk_zones answer the proposal is built from, for
// repository repoID: the session's draft answer (the conversation), else
// the prefill candidate of that repository (or of the product), with the
// candidate's module evidence unless the user edited the text.
func (s *server) riskAnswer(ctx context.Context, productID, repoID uuid.UUID, first bool) (string, []string) {
	s.sessionMu.RLock()
	sess := s.session
	s.sessionMu.RUnlock()
	if sess != nil && first {
		sess.mu.RLock()
		draft, ok := sess.CodexDraft[onboarding.QRiskZones]
		sess.mu.RUnlock()
		if ok && draft != "" {
			return draft, nil
		}
	}
	if s.onboardingStore == nil {
		return "", nil
	}
	cands, err := s.onboardingStore.List(ctx, productID)
	if err != nil {
		log.Printf("onboarding: zones: list candidates for %s: %v (no risk_zones proposal)", productID, err)
		return "", nil
	}
	var pick *onboarding.Candidate
	for i := range cands {
		c := &cands[i]
		if c.QuestionKey != onboarding.QRiskZones || c.UserStatus == onboarding.StatusRejected {
			continue
		}
		if c.RepositoryID != nil && *c.RepositoryID == repoID {
			pick = c
			break
		}
		if c.RepositoryID == nil && first && pick == nil {
			pick = c
		}
	}
	if pick == nil {
		return "", nil
	}
	var modules []string
	if pick.UserStatus != onboarding.StatusEdited {
		for _, e := range pick.Evidence {
			if e.Kind == "module" && e.Name != "" {
				modules = append(modules, e.Name)
			}
		}
	}
	return pick.EffectiveValue(), modules
}

// zoneProposals builds the wizard's view for every repository of productID.
func (s *server) zoneProposals(ctx context.Context, productID uuid.UUID) ([]repoZoneProposal, error) {
	if s.productStore == nil || s.pool == nil {
		return nil, errors.New("no database configured")
	}
	repos, err := s.productStore.GetRepositoriesForProduct(ctx, productID)
	if err != nil {
		return nil, fmt.Errorf("list repositories: %w", err)
	}
	defaults, err := zones.DefaultTemplates()
	if err != nil {
		return nil, fmt.Errorf("load default zones: %w", err)
	}
	store := zones.NewStore(s.pool)
	out := make([]repoZoneProposal, 0, len(repos))
	for i, r := range repos {
		p := repoZoneProposal{RepositoryID: r.ID, RepositoryName: r.Name, Zones: []zones.Template{}}
		existing, err := store.ListForRepository(ctx, r.ID)
		if err != nil {
			return nil, err
		}
		confirmed, err := store.Confirmed(ctx, r.ID)
		if err != nil {
			return nil, err
		}
		if len(existing) > 0 || confirmed {
			p.Existing = true
			for _, z := range existing {
				p.Zones = append(p.Zones, zones.Template{Name: z.Name, Description: z.Description, Patterns: z.Patterns, Policy: z.Policy, Source: z.Source})
				if z.Frozen() {
					p.Frozen = append(p.Frozen, z.Name)
				}
			}
		} else {
			answer, modules := s.riskAnswer(ctx, productID, r.ID, i == 0)
			p.Zones = append(append(p.Zones, defaults...), riskTemplatesWithout(zones.RiskZoneTemplates(answer, modules), defaults)...)
		}
		out = append(out, p)
	}
	return out, nil
}

// riskTemplatesWithout drops risk proposals whose name collides with a
// default zone (the defaults win; names are unique per repository).
func riskTemplatesWithout(risk, defaults []zones.Template) []zones.Template {
	taken := map[string]bool{}
	for _, d := range defaults {
		taken[d.Name] = true
	}
	var out []zones.Template
	for _, r := range risk {
		if !taken[r.Name] {
			out = append(out, r)
		}
	}
	return out
}

// applyOnboardingZones gives every repository of productID that has no zone
// yet its proposal (defaults and risk_zones). Failures are returned; the
// caller decides whether they fail the approval.
func (s *server) applyOnboardingZones(ctx context.Context, productID uuid.UUID) (int, error) {
	proposals, err := s.zoneProposals(ctx, productID)
	if err != nil {
		return 0, err
	}
	store := zones.NewStore(s.pool)
	created := 0
	for _, p := range proposals {
		if p.Existing {
			continue
		}
		n, err := store.ApplyIfEmpty(ctx, p.RepositoryID, p.Zones, fmt.Sprintf("onboarding approval: %d default and risk_zones zone(s) for %s", len(p.Zones), p.RepositoryName))
		if err != nil {
			return created, fmt.Errorf("repository %s: %w", p.RepositoryName, err)
		}
		created += n
	}
	return created, nil
}

// proposedZonesHandler serves GET /api/onboarding/zones/proposed.
func (s *server) proposedZonesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	productID, err := uuid.Parse(r.URL.Query().Get("product_id"))
	if err != nil {
		http.Error(w, "invalid product_id", http.StatusBadRequest)
		return
	}
	out, err := s.zoneProposals(r.Context(), productID)
	if err != nil {
		log.Printf("onboarding: proposed zones for %s: %v", productID, err)
		http.Error(w, "failed to build zone proposals", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// confirmZonesHandler serves POST /api/onboarding/zones.
func (s *server) confirmZonesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		RepositoryID string           `json:"repository_id"`
		Zones        []zones.Template `json:"zones"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	repoID, err := uuid.Parse(req.RepositoryID)
	if err != nil {
		http.Error(w, "invalid repository_id", http.StatusBadRequest)
		return
	}
	seen := map[string]bool{}
	for i := range req.Zones {
		z := &req.Zones[i]
		if z.Source == "" || z.Source == zones.SourceDashboard {
			z.Source = zones.SourceOnboarding
		}
		z.Patterns = zones.NormalizePatterns(z.Patterns)
		if err := zones.Validate(z.Name, z.Patterns, z.Policy); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if seen[z.Name] {
			http.Error(w, "zone "+z.Name+" listed twice", http.StatusBadRequest)
			return
		}
		seen[z.Name] = true
	}
	if s.pool == nil {
		http.Error(w, "no database configured", http.StatusServiceUnavailable)
		return
	}
	out, err := zones.NewStore(s.pool).ConfirmOnboarding(r.Context(), repoID, req.Zones, "onboarding wizard confirmation")
	switch {
	case errors.Is(err, zones.ErrNotFound):
		http.Error(w, "repository not found", http.StatusNotFound)
		return
	case errors.Is(err, zones.ErrInvalid), errors.Is(err, zones.ErrDuplicate):
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	case errors.Is(err, zones.ErrNoBranch):
		http.Error(w, err.Error(), http.StatusPreconditionFailed)
		return
	case err != nil:
		log.Printf("onboarding: confirm zones for %s: %v", repoID, err)
		http.Error(w, "failed to save zones", http.StatusInternalServerError)
		return
	}
	if len(out.Kept) > 0 {
		log.Printf("onboarding: confirm zones for %s: kept existing zone(s) %v unchanged (onboarding only adds; edit them in the admin dashboard)", repoID, out.Kept)
	}
	writeJSON(w, http.StatusOK, out)
}
