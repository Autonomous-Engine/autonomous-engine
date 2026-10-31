package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
)

type fakeVersions struct {
	product uuid.UUID
	branch  string
	v       int
	err     error
}

func (f *fakeVersions) GetCurrentVersion(_ context.Context, productID uuid.UUID, branch string) (int, error) {
	f.product, f.branch = productID, branch
	return f.v, f.err
}

// The research result's codex version is the product_version of the branch
// research read the codex on, not the global version_number.
func TestCodexVersionForReadsTheProductAndBranch(t *testing.T) {
	pid := uuid.New()
	f := &fakeVersions{v: 7}
	if got := codexVersionFor(context.Background(), f, pid, "develop"); got != 7 || f.product != pid || f.branch != "develop" {
		t.Fatalf("codexVersionFor = %d (asked %s/%s)", got, f.product, f.branch)
	}
	if got := codexVersionFor(context.Background(), &fakeVersions{err: errors.New("db down")}, pid, "main"); got != 0 {
		t.Fatalf("on error = %d, want 0", got)
	}
	if got := codexVersionFor(context.Background(), nil, pid, "main"); got != 0 {
		t.Fatalf("without a codex = %d, want 0", got)
	}
}

// research.results carries codex_branch with codex_version, so the
// coordinator can tell a product_version from a pre-060 global number.
func TestResearchResultCarriesCodexBranch(t *testing.T) {
	b, err := json.Marshal(researchResultMessage{CodexVersion: 3, CodexBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if m["codex_version"] != float64(3) || m["codex_branch"] != "main" {
		t.Fatalf("result = %s", b)
	}
}
