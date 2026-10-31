package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/codex"
	"github.com/autonomousengine/autonomous-engine/internal/products"
	"github.com/autonomousengine/autonomous-engine/internal/zones"
	"github.com/google/uuid"
)

func zonePoliciesOf(t *testing.T, zs []zones.Zone) map[string]zones.Policy {
	t.Helper()
	out := map[string]zones.Policy{}
	for _, z := range zs {
		out[z.Name] = z.Policy
	}
	return out
}

// Onboarding approval (codex zones, migration 063) creates the repository's
// zones from the codex-config defaults and the risk_zones answer, records
// them in a section:zones codex version, and leaves a repository that
// already has zones alone. The wizard's proposal endpoint shows the same
// zones before approval, and its confirmation replaces them.
func TestIntegrationOnboardingApprovalCreatesZones(t *testing.T) {
	pool := onboardingIntegrationPool(t)
	ctx := context.Background()
	productID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO products (id, name) VALUES ($1, $2)`, productID, "p2-onboarding-"+productID.String()[:8]); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM outbox_events WHERE aggregate_id = $1`, productID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM products WHERE id = $1`, productID)
	})
	ps := products.NewStore(pool)
	repo, err := ps.AddRepository(ctx, productID, "api", "https://github.com/example/zones-"+productID.String()[:8], "trunk", "squash", "ae/")
	if err != nil {
		t.Fatal(err)
	}
	sess := newSession()
	sess.CodexDraft["risk_zones"] = "2 of 9 modules are high-risk (>5 dependents): internal/billing, internal/auth. Changes ripple."
	s := &server{pool: pool, productStore: ps, cx: codex.New(pool), session: sess}

	// The wizard's proposal, before approval.
	rec := httptest.NewRecorder()
	s.proposedZonesHandler(rec, httptest.NewRequest(http.MethodGet, "/api/onboarding/zones/proposed?product_id="+productID.String(), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("proposed = %d %s", rec.Code, rec.Body.String())
	}
	var proposals []repoZoneProposal
	if err := json.Unmarshal(rec.Body.Bytes(), &proposals); err != nil {
		t.Fatal(err)
	}
	if len(proposals) != 1 || proposals[0].RepositoryID != repo.ID || proposals[0].Existing {
		t.Fatalf("proposals = %+v", proposals)
	}
	want := map[string]zones.Policy{
		"guarded": zones.PolicyHumanReview, "restricted": zones.PolicyEscalate,
		"internal/auth": zones.PolicyHumanReview, "internal/billing": zones.PolicyHumanReview,
	}
	got := map[string]zones.Policy{}
	for _, z := range proposals[0].Zones {
		got[z.Name] = z.Policy
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("proposed zones = %v, want %v", got, want)
	}

	// Approval creates them.
	rec = httptest.NewRecorder()
	body, _ := json.Marshal(map[string]string{"product_id": productID.String()})
	s.approveCodexHandler(rec, httptest.NewRequest(http.MethodPost, "/api/onboarding/approve-codex", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("approve = %d %s", rec.Code, rec.Body.String())
	}
	store := zones.NewStore(pool)
	zs, err := store.ListForRepository(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := zonePoliciesOf(t, zs); !reflect.DeepEqual(got, want) {
		t.Fatalf("zones after approval = %v, want %v", got, want)
	}
	for _, z := range zs {
		wantSource := zones.SourceConfig
		if z.Name == "internal/auth" || z.Name == "internal/billing" {
			wantSource = zones.SourceOnboarding
		}
		if z.Source != wantSource {
			t.Errorf("%s source = %s, want %s", z.Name, z.Source, wantSource)
		}
	}
	var zoneVersions int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM codex_versions WHERE product_id = $1 AND branch = 'trunk' AND 'section:zones' = ANY(sections_changed)`, productID).Scan(&zoneVersions); err != nil {
		t.Fatal(err)
	}
	if zoneVersions != 1 {
		t.Fatalf("%d section:zones versions after approval, want 1", zoneVersions)
	}

	// The onboarding endpoint is unauthenticated: once the repository has
	// zones it may only add new ones. The review's reproduction: an admin
	// makes payments escalate and frozen; an anonymous POST asking for
	// payments autonomous and restricted autonomous changes neither row, and
	// removes nothing it does not list.
	store2 := zones.NewStore(pool)
	pay, err := store2.Create(ctx, repo.ID, zones.Template{Name: "payments", Patterns: []string{"payments/**"}, Policy: zones.PolicyEscalate, Source: zones.SourceDashboard})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store2.Freeze(ctx, pay.ID, "audit"); err != nil {
		t.Fatal(err)
	}
	before, _ := store.ListForRepository(ctx, repo.ID)
	attack := map[string]any{"repository_id": repo.ID.String(), "zones": []map[string]any{
		{"name": "payments", "patterns": []string{"payments/**"}, "policy": "autonomous", "source": "dashboard"},
		{"name": "restricted", "patterns": []string{"nothing/**"}, "policy": "autonomous", "source": "config"},
	}}
	body, _ = json.Marshal(attack)
	rec = httptest.NewRecorder()
	s.confirmZonesHandler(rec, httptest.NewRequest(http.MethodPost, "/api/onboarding/zones", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("anonymous confirm = %d %s", rec.Code, rec.Body.String())
	}
	var res zones.OnboardingResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil || res.FirstTime || len(res.Added) != 0 {
		t.Fatalf("anonymous confirm result = %+v, %v", res, err)
	}
	after, _ := store.ListForRepository(ctx, repo.ID)
	if got, want := zoneRows(after), zoneRows(before); !reflect.DeepEqual(got, want) {
		t.Fatalf("anonymous confirm changed zones:\n before %v\n after  %v", want, got)
	}

	// A re-confirmation adds a new zone but cannot delete or weaken: it
	// lists only the new zone and a weakened guarded.
	body, _ = json.Marshal(map[string]any{"repository_id": repo.ID.String(), "zones": []zones.Template{
		{Name: "guarded", Patterns: []string{"x/**"}, Policy: zones.PolicyAutonomous, Source: zones.SourceConfig},
		{Name: "ledger", Patterns: []string{"ledger/**"}, Policy: zones.PolicyEscalate, Source: zones.SourceOnboarding},
	}})
	rec = httptest.NewRecorder()
	s.confirmZonesHandler(rec, httptest.NewRequest(http.MethodPost, "/api/onboarding/zones", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("re-confirm = %d %s", rec.Code, rec.Body.String())
	}
	final, _ := store.ListForRepository(ctx, repo.ID)
	if len(final) != len(after)+1 {
		t.Fatalf("re-confirm: %d zones → %d, want one added, none removed", len(after), len(final))
	}
	if got := zonePoliciesOf(t, final); got["guarded"] != zones.PolicyHumanReview || got["ledger"] != zones.PolicyEscalate || got["payments"] != zones.PolicyEscalate {
		t.Fatalf("re-confirm policies = %v", got)
	}

	// Re-approving does not bring defaults back or change anything.
	rec = httptest.NewRecorder()
	body, _ = json.Marshal(map[string]string{"product_id": productID.String()})
	s.approveCodexHandler(rec, httptest.NewRequest(http.MethodPost, "/api/onboarding/approve-codex", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("re-approve = %d %s", rec.Code, rec.Body.String())
	}
	if again, _ := store.ListForRepository(ctx, repo.ID); !reflect.DeepEqual(zoneRows(again), zoneRows(final)) {
		t.Fatalf("re-approval changed the zones")
	}

	// A bad zone is rejected.
	body, _ = json.Marshal(map[string]any{"repository_id": repo.ID.String(), "zones": []zones.Template{{Name: "x", Policy: "maybe", Patterns: []string{"a"}}}})
	rec = httptest.NewRecorder()
	s.confirmZonesHandler(rec, httptest.NewRequest(http.MethodPost, "/api/onboarding/zones", bytes.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad confirm = %d %s", rec.Code, rec.Body.String())
	}
}

// First-time onboarding through the wizard: the confirmation writes exactly
// the chosen zones (one default dropped, a risk zone raised to escalate);
// unticking everything on another repository is an explicit "no zones"
// choice that approval seeding then respects.
func TestIntegrationOnboardingWizardFirstConfirmation(t *testing.T) {
	pool := onboardingIntegrationPool(t)
	ctx := context.Background()
	productID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO products (id, name) VALUES ($1, $2)`, productID, "p2-wizard-"+productID.String()[:8]); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM outbox_events WHERE aggregate_id = $1`, productID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM products WHERE id = $1`, productID)
	})
	ps := products.NewStore(pool)
	chosen, err := ps.AddRepository(ctx, productID, "api", "https://github.com/example/wz-a-"+productID.String()[:8], "main", "squash", "ae/")
	if err != nil {
		t.Fatal(err)
	}
	none, err := ps.AddRepository(ctx, productID, "web", "https://github.com/example/wz-b-"+productID.String()[:8], "main", "squash", "ae/")
	if err != nil {
		t.Fatal(err)
	}
	sess := newSession()
	sess.CodexDraft["risk_zones"] = "high-risk: billing"
	s := &server{pool: pool, productStore: ps, cx: codex.New(pool), session: sess}

	post := func(repo uuid.UUID, zs []zones.Template) zones.OnboardingResult {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"repository_id": repo.String(), "zones": zs})
		rec := httptest.NewRecorder()
		s.confirmZonesHandler(rec, httptest.NewRequest(http.MethodPost, "/api/onboarding/zones", bytes.NewReader(body)))
		if rec.Code != http.StatusOK {
			t.Fatalf("confirm = %d %s", rec.Code, rec.Body.String())
		}
		var res zones.OnboardingResult
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		return res
	}
	res := post(chosen.ID, []zones.Template{
		{Name: "restricted", Patterns: []string{"**/secrets/**"}, Policy: zones.PolicyEscalate, Source: zones.SourceConfig},
		{Name: "billing", Patterns: []string{"billing/**"}, Policy: zones.PolicyEscalate, Source: zones.SourceOnboarding},
	})
	if !res.FirstTime || !reflect.DeepEqual(res.Added, []string{"billing", "restricted"}) {
		t.Fatalf("first confirm = %+v", res)
	}
	if res := post(none.ID, []zones.Template{}); !res.FirstTime || len(res.Zones) != 0 {
		t.Fatalf("no-zones confirm = %+v", res)
	}

	rec := httptest.NewRecorder()
	body, _ := json.Marshal(map[string]string{"product_id": productID.String()})
	s.approveCodexHandler(rec, httptest.NewRequest(http.MethodPost, "/api/onboarding/approve-codex", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("approve = %d %s", rec.Code, rec.Body.String())
	}
	store := zones.NewStore(pool)
	if zs, _ := store.ListForRepository(ctx, chosen.ID); !reflect.DeepEqual(zonePoliciesOf(t, zs), map[string]zones.Policy{"restricted": zones.PolicyEscalate, "billing": zones.PolicyEscalate}) {
		t.Fatalf("chosen repo after approval = %v", zonePoliciesOf(t, zs))
	}
	if zs, _ := store.ListForRepository(ctx, none.ID); len(zs) != 0 {
		t.Fatalf("approval re-applied defaults over an explicit no-zones choice: %v", zonePoliciesOf(t, zs))
	}
	// The wizard then shows both repositories as already set.
	rec = httptest.NewRecorder()
	s.proposedZonesHandler(rec, httptest.NewRequest(http.MethodGet, "/api/onboarding/zones/proposed?product_id="+productID.String(), nil))
	var proposals []repoZoneProposal
	_ = json.Unmarshal(rec.Body.Bytes(), &proposals)
	for _, p := range proposals {
		if !p.Existing {
			t.Fatalf("proposal for %s not marked existing: %+v", p.RepositoryName, p)
		}
	}
}

func zoneRows(zs []zones.Zone) []string {
	var out []string
	for _, z := range zs {
		reason := ""
		if z.FrozenReason != nil {
			reason = *z.FrozenReason
		}
		out = append(out, fmt.Sprintf("%s|%s|%s|%s|%v|%s", z.Name, z.Policy, z.Status, z.Source, z.Patterns, reason))
	}
	sort.Strings(out)
	return out
}
