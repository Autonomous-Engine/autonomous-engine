package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/products"
	"github.com/google/uuid"
)

// fakeTriggerRepos answers the planner's repository lookups.
type fakeTriggerRepos struct {
	repos   map[uuid.UUID]*products.Repository
	byProd  map[uuid.UUID][]*products.Repository
	listErr error
}

func (f *fakeTriggerRepos) GetRepository(_ context.Context, id uuid.UUID) (*products.Repository, error) {
	if r, ok := f.repos[id]; ok {
		return r, nil
	}
	return nil, errors.New("no rows")
}

func (f *fakeTriggerRepos) GetRepositoriesForProduct(_ context.Context, id uuid.UUID) ([]*products.Repository, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.byProd[id], nil
}

func TestPlanIncrementalTrigger(t *testing.T) {
	const legacy = "/repo-cache"
	repoA, repoB, repoOff, repoNoClone := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	product, emptyProduct := uuid.New(), uuid.New()
	src := &fakeTriggerRepos{
		repos: map[uuid.UUID]*products.Repository{
			repoA: {ID: repoA, ProductID: product, ClonePath: "/clones/a", Enabled: true},
		},
		byProd: map[uuid.UUID][]*products.Repository{
			product: {
				{ID: repoA, ProductID: product, ClonePath: "/clones/a", Enabled: true},
				{ID: repoB, ProductID: product, ClonePath: "/clones/b", Enabled: true},
				{ID: repoOff, ClonePath: "/clones/off", Enabled: false},
				{ID: repoNoClone, ClonePath: "/clones/missing", Enabled: true},
			},
			emptyProduct: {{ID: repoNoClone, ClonePath: "/clones/missing", Enabled: true}},
		},
	}
	clones := map[string]bool{"/clones/a": true, "/clones/b": true, "/clones/off": true}
	exists := func(p string) bool { return clones[p] }

	cases := []struct {
		name      string
		trigger   indexTrigger
		repos     triggerRepoSource
		wantSteps []indexStep
		wantSkip  string
	}{
		{
			// Migration 060: a resolved repository names its product, so the
			// incremental index writes a codex version for it.
			name:      "registered push: the repository's clone, refreshed first",
			trigger:   indexTrigger{RepositoryID: repoA.String(), Since: "HEAD~1"},
			repos:     src,
			wantSteps: []indexStep{{path: "/clones/a", refresh: true, productID: product, repoID: repoA}},
		},
		{
			name:      "post_merge: the repository's clone, refreshed (unchanged)",
			trigger:   indexTrigger{Trigger: "post_merge", RepositoryID: repoA.String(), Since: "HEAD~1"},
			repos:     src,
			wantSteps: []indexStep{{path: "/clones/a", refresh: true, productID: product, repoID: repoA}},
		},
		{
			name:      "post_merge with an unknown repository: legacy path, refreshed (unchanged)",
			trigger:   indexTrigger{Trigger: "post_merge", RepositoryID: uuid.NewString(), Since: "HEAD~1"},
			repos:     src,
			wantSteps: []indexStep{{path: legacy, refresh: true}},
		},
		{
			name:      "repository_id that does not resolve: legacy path, not refreshed (unchanged)",
			trigger:   indexTrigger{RepositoryID: uuid.NewString(), Since: "HEAD~1"},
			repos:     src,
			wantSteps: []indexStep{{path: legacy}},
		},
		{
			name:      "product only: every enabled repository with a clone, never the legacy path",
			trigger:   indexTrigger{ProductID: product.String(), Since: "HEAD~1"},
			repos:     src,
			wantSteps: []indexStep{{path: "/clones/a", productID: product, repoID: repoA}, {path: "/clones/b", productID: product, repoID: repoB}},
		},
		{
			name:     "product without clones: skipped",
			trigger:  indexTrigger{ProductID: emptyProduct.String(), Since: "HEAD~1"},
			repos:    src,
			wantSkip: "no enabled repository with a clone",
		},
		{
			name:     "unknown product: skipped",
			trigger:  indexTrigger{ProductID: uuid.NewString(), Since: "HEAD~1"},
			repos:    src,
			wantSkip: "no enabled repository with a clone",
		},
		{
			name:     "product lookup error: skipped",
			trigger:  indexTrigger{ProductID: product.String(), Since: "HEAD~1"},
			repos:    &fakeTriggerRepos{listErr: errors.New("db down")},
			wantSkip: "db down",
		},
		{
			name:     "product without a product store: skipped",
			trigger:  indexTrigger{ProductID: product.String(), Since: "HEAD~1"},
			repos:    nil,
			wantSkip: "no product store",
		},
		{
			name:     "malformed product_id: skipped",
			trigger:  indexTrigger{ProductID: "not-a-uuid", Since: "HEAD~1"},
			repos:    src,
			wantSkip: "not a uuid",
		},
		{
			name:      "explicit legacy trigger without ids keeps the legacy path",
			trigger:   indexTrigger{Trigger: "manual_reindex", Since: "HEAD~1"},
			repos:     src,
			wantSteps: []indexStep{{path: legacy}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := planIncrementalTrigger(context.Background(), tc.trigger, tc.repos, legacy, exists)
			if tc.wantSkip != "" {
				if len(got.steps) != 0 || !strings.Contains(got.skip, tc.wantSkip) {
					t.Fatalf("plan = %+v, want a skip containing %q", got, tc.wantSkip)
				}
				return
			}
			if got.skip != "" || !reflect.DeepEqual(got.steps, tc.wantSteps) {
				t.Fatalf("plan = %+v, want steps %+v", got, tc.wantSteps)
			}
			for _, st := range got.steps {
				if st.path == legacy && tc.trigger.ProductID != "" && tc.trigger.RepositoryID == "" && tc.trigger.Trigger == "" {
					t.Fatalf("product-only trigger planned the legacy path: %+v", got)
				}
			}
		})
	}
}

// The registered push's refresh runs before its index, a failed refresh is
// logged and the clone indexed as it is, and every step of a product runs
// even when one fails.
func TestRunTriggerPlan(t *testing.T) {
	cases := []struct {
		name       string
		plan       triggerPlan
		refreshErr error
		indexErr   map[string]error
		wantCalls  []string
		wantErr    string
	}{
		{
			name:      "refresh then index",
			plan:      triggerPlan{steps: []indexStep{{path: "/clones/a", refresh: true}}},
			wantCalls: []string{"refresh /clones/a", "index /clones/a"},
		},
		{
			name:       "refresh failure still indexes",
			plan:       triggerPlan{steps: []indexStep{{path: "/clones/a", refresh: true}}},
			refreshErr: errors.New("git pull: not possible to fast-forward"),
			wantCalls:  []string{"refresh /clones/a", "index /clones/a"},
		},
		{
			name:      "product: each clone, no refresh, all run despite a failure",
			plan:      triggerPlan{steps: []indexStep{{path: "/clones/a"}, {path: "/clones/b"}}},
			indexErr:  map[string]error{"/clones/a": errors.New("repo not found")},
			wantCalls: []string{"index /clones/a", "index /clones/b"},
			wantErr:   "/clones/a: repo not found",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			err := runTriggerPlan(tc.plan,
				func(p string) error { calls = append(calls, "refresh "+p); return tc.refreshErr },
				func(st indexStep) error { calls = append(calls, "index "+st.path); return tc.indexErr[st.path] },
			)
			if !reflect.DeepEqual(calls, tc.wantCalls) {
				t.Fatalf("calls = %v, want %v", calls, tc.wantCalls)
			}
			if (tc.wantErr == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

// The handler resolves the plan before claiming the indexing slot: a
// product whose repositories cannot be resolved (here: no product store) is skipped without replacing the last
// progress or starting an index; never the legacy REPO_PATH.
func TestHandleKafkaMessageUnresolvableProductIsSkipped(t *testing.T) {
	s := &server{}
	before := newProgress()
	s.progress = before
	msg := `{"source":"github","commit_sha":"abc123","timestamp":"2026-10-03T10:00:00Z","product_id":"` + uuid.NewString() + `"}`
	if err := s.handleKafkaMessage(context.Background(), &bus.Message{Topic: "codex.updated", Value: []byte(msg)}); err != nil {
		t.Fatalf("handleKafkaMessage: %v", err)
	}
	s.progressMu.RLock()
	defer s.progressMu.RUnlock()
	if s.indexing || s.progress != before {
		t.Fatalf("a product-only trigger without a product store started an index (indexing=%v)", s.indexing)
	}
}
