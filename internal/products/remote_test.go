package products_test

import (
	"context"
	"errors"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/gitremote"
	"github.com/autonomousengine/autonomous-engine/internal/products"
	"github.com/google/uuid"
)

func TestClassifyRemote(t *testing.T) {
	cases := []struct {
		in, url string
		kind    gitremote.Kind
		invalid bool
	}{
		{"owner/repo", "owner/repo", gitremote.KindGitHubShorthand, false},
		{"  https://github.com/o/r.git ", "https://github.com/o/r.git", gitremote.KindGitHubHTTPS, false},
		{"/repo-cache/mirror.git", "/repo-cache/mirror.git", gitremote.KindLocalPath, false},
		{"file:///repo-cache/mirror.git", "file:///repo-cache/mirror.git", gitremote.KindFileURL, false},
		{"https://evil.example/o/r.git", "https://evil.example/o/r.git", gitremote.KindOtherRemote, false},
		{"", "", gitremote.KindInvalid, true},
		{"-oProxyCommand=x", "", gitremote.KindInvalid, true},
		{"o/r x", "", gitremote.KindInvalid, true},
	}
	for _, tc := range cases {
		u, k, err := products.ClassifyRemote(tc.in)
		var inv *products.InvalidRemoteError
		if tc.invalid != errors.As(err, &inv) {
			t.Errorf("ClassifyRemote(%q) err = %v, want invalid=%v", tc.in, err, tc.invalid)
		}
		if u != tc.url || k != tc.kind {
			t.Errorf("ClassifyRemote(%q) = (%q, %q), want (%q, %q)", tc.in, u, k, tc.url, tc.kind)
		}
	}
}

// An invalid remote is rejected before the database is touched (the store
// here has no pool at all).
func TestAddRepository_RejectsInvalidRemoteBeforeDB(t *testing.T) {
	s := products.NewStore(nil)
	_, err := s.AddRepository(context.Background(), uuid.New(), "r", "-oProxyCommand=x", "main", "squash", "ae/")
	var inv *products.InvalidRemoteError
	if !errors.As(err, &inv) {
		t.Fatalf("AddRepository with an invalid remote: err = %v, want *InvalidRemoteError", err)
	}
	bad := "--upload-pack=x"
	if err := s.UpdateRepository(context.Background(), uuid.New(), products.RepositoryUpdate{GitHubURL: &bad}); !errors.As(err, &inv) {
		t.Fatalf("UpdateRepository with an invalid remote: err = %v, want *InvalidRemoteError", err)
	}
}

// TestStoreIntegration_RemoteKind: the kind is written on create and on a
// github_url update (migration 057).
func TestStoreIntegration_RemoteKind(t *testing.T) {
	pool := integrationPool(t)
	defer pool.Close()
	ctx := context.Background()
	store := products.NewStore(pool)

	p, err := store.CreateProduct(ctx, "g1-remote-kind-"+uuid.New().String()[:8], "G1")
	if err != nil {
		t.Fatalf("CreateProduct: %v", err)
	}
	t.Cleanup(func() { _ = store.DeleteProduct(context.Background(), p.ID) })

	r, err := store.AddRepository(ctx, p.ID, "mirror", " /repo-cache/mirror.git ", "main", "squash", "ae/")
	if err != nil {
		t.Fatalf("AddRepository: %v", err)
	}
	if r.GitHubURL != "/repo-cache/mirror.git" || r.RemoteKind != string(gitremote.KindLocalPath) {
		t.Fatalf("AddRepository = (%q, %q), want trimmed path and local_path", r.GitHubURL, r.RemoteKind)
	}
	var stored string
	if err := pool.QueryRow(ctx, `SELECT remote_kind FROM repositories WHERE id = $1`, r.ID).Scan(&stored); err != nil || stored != string(gitremote.KindLocalPath) {
		t.Fatalf("stored remote_kind = %q (%v), want local_path", stored, err)
	}
	u := "https://github.com/o/r.git"
	if err := store.UpdateRepository(ctx, r.ID, products.RepositoryUpdate{GitHubURL: &u}); err != nil {
		t.Fatalf("UpdateRepository: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT remote_kind FROM repositories WHERE id = $1`, r.ID).Scan(&stored); err != nil || stored != string(gitremote.KindGitHubHTTPS) {
		t.Fatalf("stored remote_kind after update = %q (%v), want github_https", stored, err)
	}
}

func TestRepository_KindDerivesFromURL(t *testing.T) {
	// A stale stored label never wins over the URL.
	r := &products.Repository{GitHubURL: "https://evil.example/o/r.git", RemoteKind: string(gitremote.KindGitHubHTTPS)}
	if k := r.Kind(); k != gitremote.KindOtherRemote {
		t.Errorf("Kind() = %q, want %q", k, gitremote.KindOtherRemote)
	}
}
