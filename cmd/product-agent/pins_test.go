package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/products"
	"github.com/autonomousengine/autonomous-engine/internal/snapshot"
	"github.com/google/uuid"
)

const (
	pinSHA1 = "7af0d7084cd8b7c8a96b50da4311b560bebcb8e8"
	pinSHA2 = "0123456789abcdef0123456789abcdef01234567"
)

// fakeResolver resolves by repository id; repositories without an entry in
// shas fail with errs[id] (or a generic error). It records what it resolved.
type fakeResolver struct {
	mu   sync.Mutex
	shas map[uuid.UUID]string
	errs map[uuid.UUID]error
	seen []uuid.UUID
}

func (f *fakeResolver) ResolveDetailed(_ context.Context, repo products.Repository) (snapshot.Resolution, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, repo.ID)
	if sha, ok := f.shas[repo.ID]; ok {
		return snapshot.Resolution{SHA: sha, Ref: repo.BaseBranch, Method: "clone-origin", Detail: "fake clone origin"}, nil
	}
	if err, ok := f.errs[repo.ID]; ok {
		return snapshot.Resolution{}, err
	}
	return snapshot.Resolution{}, errors.New("fake: no answer for " + repo.Name)
}

type fakeLister struct {
	byID      map[uuid.UUID]*products.Repository
	byProduct map[uuid.UUID][]*products.Repository
}

func (f *fakeLister) GetRepository(_ context.Context, id uuid.UUID) (*products.Repository, error) {
	if r, ok := f.byID[id]; ok {
		return r, nil
	}
	return nil, errors.New("not found")
}

func (f *fakeLister) GetRepositoriesForProduct(_ context.Context, pid uuid.UUID) ([]*products.Repository, error) {
	return f.byProduct[pid], nil
}

func repoFixture() (product uuid.UUID, a, b, disabled *products.Repository, lister *fakeLister) {
	product = uuid.New()
	a = &products.Repository{ID: uuid.New(), ProductID: product, Name: "frontend", BaseBranch: "main", Enabled: true}
	b = &products.Repository{ID: uuid.New(), ProductID: product, Name: "backend", BaseBranch: "develop", Enabled: true}
	disabled = &products.Repository{ID: uuid.New(), ProductID: product, Name: "old", BaseBranch: "main", Enabled: false}
	lister = &fakeLister{
		byID:      map[uuid.UUID]*products.Repository{a.ID: a, b.ID: b, disabled.ID: disabled},
		byProduct: map[uuid.UUID][]*products.Repository{product: {a, b, disabled}},
	}
	return
}

func TestIntakePinner_PinsEveryEnabledRepository(t *testing.T) {
	product, a, b, disabled, lister := repoFixture()
	res := &fakeResolver{shas: map[uuid.UUID]string{a.ID: pinSHA1, b.ID: pinSHA2, disabled.ID: pinSHA1}}
	work := uuid.New()
	pins, err := (&intakePinner{repos: lister, resolver: res}).resolve(context.Background(), work, product, &models.WorkItem{ID: work})
	if err != nil {
		t.Fatal(err)
	}
	if len(pins) != 2 {
		t.Fatalf("got %d pins, want 2 (disabled repository skipped): %+v", len(pins), pins)
	}
	got := map[uuid.UUID]snapshot.Pin{}
	for _, p := range pins {
		got[p.pin.RepositoryID] = p.pin
		if p.pin.WorkItemID != work || p.pin.ResolvedAt.IsZero() || !strings.HasPrefix(p.detail, "intake: ") {
			t.Errorf("pin %+v detail %q", p.pin, p.detail)
		}
	}
	if got[a.ID].BaseSHA != pinSHA1 || got[a.ID].BaseRef != "main" || got[b.ID].BaseSHA != pinSHA2 || got[b.ID].BaseRef != "develop" {
		t.Fatalf("pins = %+v", got)
	}
}

func TestIntakePinner_ItemRepositoryOnly(t *testing.T) {
	product, a, b, _, lister := repoFixture()
	res := &fakeResolver{shas: map[uuid.UUID]string{a.ID: pinSHA1, b.ID: pinSHA2}}
	work := uuid.New()
	pins, err := (&intakePinner{repos: lister, resolver: res}).resolve(context.Background(), work, product, &models.WorkItem{ID: work, RepositoryID: &b.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(pins) != 1 || pins[0].pin.RepositoryID != b.ID || len(res.seen) != 1 {
		t.Fatalf("pins = %+v, resolved %v; want only the item's repository %s", pins, res.seen, b.ID)
	}
}

func TestIntakePinner_FailureNamesEveryFailedRepository(t *testing.T) {
	product, a, b, _, lister := repoFixture()
	res := &fakeResolver{
		shas: map[uuid.UUID]string{},
		errs: map[uuid.UUID]error{
			a.ID: errors.New("could not resolve main of repository frontend: ls-remote: 401; GitHub credential: token rejected"),
		},
	}
	work := uuid.New()
	pins, err := (&intakePinner{repos: lister, resolver: res}).resolve(context.Background(), work, product, nil)
	if err == nil || pins != nil {
		t.Fatalf("pins=%v err=%v, want an error and no pins", pins, err)
	}
	for _, want := range []string{"frontend", a.ID.String(), "token rejected", "backend", b.ID.String()} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	if len(res.seen) != 2 {
		t.Errorf("resolved %d repositories, want every one attempted", len(res.seen))
	}
	reason := pinFailureReason(err)
	if !strings.Contains(reason, "token rejected") || !strings.Contains(reason, "retry") {
		t.Errorf("block reason %q", reason)
	}
}

func TestIntakePinner_NoRepositoryNoPins(t *testing.T) {
	lister := &fakeLister{byProduct: map[uuid.UUID][]*products.Repository{}}
	pins, err := (&intakePinner{repos: lister, resolver: &fakeResolver{}}).resolve(context.Background(), uuid.New(), uuid.New(), nil)
	if err != nil || len(pins) != 0 {
		t.Fatalf("pins=%v err=%v", pins, err)
	}
}

func TestNewResolver_NamesTokenSource(t *testing.T) {
	r := newResolver(nil, "env-secret")
	if r.TokenSource == nil || r.Token != nil {
		t.Fatal("the production resolver must use TokenSource so failures name the source")
	}
	tok, src := r.TokenSource(context.Background(), products.Repository{TokenOverride: "ovr"})
	if tok != "ovr" || src != "override" {
		t.Fatalf("override: (%q, %q)", tok, src)
	}
	if _, src := r.TokenSource(context.Background(), products.Repository{}); src != "env" {
		t.Fatalf("env: %q", src)
	}
}

func TestPinBlockedPrompt(t *testing.T) {
	w, p := uuid.New(), uuid.New()
	pr := pinBlockedPrompt(w, p, "Add a phone field", "reason")
	if pr.WorkItemID != w || pr.ProductID != p || pr.Question != "reason" || pr.AskingAgent != agentID ||
		string(pr.Trigger) != "blocked" || string(pr.PromptType) != "escalation" || !strings.Contains(pr.Headline, "Add a phone field") {
		t.Fatalf("prompt = %+v", pr)
	}
}

// Source scan: resolution (network) runs before any LLM work and before, not
// inside, a transaction; the pins commit in the PRD transaction; a failure
// blocks with the prompt in one guarded transaction.
func TestProductAgent_PinsResolvedBeforeTxWrittenInPRDTx(t *testing.T) {
	handler := funcSource(t, "main.go", "buildHandler")
	resolveAt := strings.Index(handler, "pinner.resolve(")
	if resolveAt < 0 {
		t.Fatal("buildHandler never resolves pins")
	}
	if strings.Index(handler, "productID == nil") > resolveAt {
		t.Error("pins are resolved before the missing-product check")
	}
	for _, later := range []string{"llm.NewServiceForWorkItem(", "codexGate(", "commitPRD("} {
		if i := strings.Index(handler, later); i < resolveAt {
			t.Errorf("%s runs before pin resolution", later)
		}
	}
	if !strings.Contains(handler, "blockWithPromptInOwnTx(msgCtx, idempotency.Wrap(msgCtx, pool), aggregateID, workID, reason, prompt)") {
		t.Error("a resolution failure is not blocked through blockWithPromptInOwnTx in a guarded transaction")
	}
	if !strings.Contains(handler, "assignment.ID, pins)") {
		t.Error("commitPRD is not given the resolved pins")
	}

	commit := funcSource(t, "main.go", "commitPRD")
	pinAt := strings.Index(commit, "writePinsTx(ctx, tx, workID, pins)")
	if pinAt < 0 {
		t.Fatal("commitPRD does not write the pins in its transaction")
	}
	if pinAt > strings.Index(commit, "wg.TransitionStateTx(") {
		t.Error("pins are written after the transition; write them first")
	}

	block := funcSource(t, "pins.go", "blockWithPromptInOwnTx")
	for _, want := range []string{"workgraph.WithTx(", "guards.BlockWorkItemTx(ctx, tx, aggregateID, workID, agentID, reason)", "PublishTx(ctx, tx, aggregateID, prompt)"} {
		if !strings.Contains(block, want) {
			t.Errorf("blockWithPromptInOwnTx does not contain %q", want)
		}
	}
	if strings.Index(block, "PublishTx(") < strings.Index(block, "BlockWorkItemTx(") {
		t.Error("the prompt is raised before the block it announces")
	}

	// No git inside a transaction: the tx helpers never resolve.
	for _, fn := range []string{"writePinsTx", "blockWithPromptInOwnTx"} {
		if src := funcSource(t, "pins.go", fn); strings.Contains(src, "Resolve") || strings.Contains(src, "GitRunner") {
			t.Errorf("%s resolves inside the transaction", fn)
		}
	}
}
