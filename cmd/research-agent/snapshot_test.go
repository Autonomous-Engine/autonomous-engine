package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/snapshot"
	"github.com/google/uuid"
)

const (
	pinA = "7af0d7084cd8b7c8a96b50da4311b560bebcb8e8"
	pinB = "0123456789abcdef0123456789abcdef01234567"
)

type fakeEnsurer struct {
	calls [][3]string
	err   error
}

func (f *fakeEnsurer) Ensure(_ context.Context, clonePath, baseRef, sha string) (snapshot.Snapshot, error) {
	f.calls = append(f.calls, [3]string{clonePath, baseRef, sha})
	if f.err != nil {
		return snapshot.Snapshot{}, f.err
	}
	return snapshot.Snapshot{Path: snapshot.WorktreePathIn(filepath.Join(clonePath, ".git"), sha), SHA: sha, Created: true}, nil
}

type fakePins map[uuid.UUID]snapshot.Pin

func (f fakePins) Get(_ context.Context, _ uuid.UUID, repoID uuid.UUID) (snapshot.Pin, bool, error) {
	p, ok := f[repoID]
	return p, ok, nil
}

func fakeClone(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestPinnedReadPaths_PinnedRepoReadsItsSnapshot(t *testing.T) {
	repoID := uuid.New()
	clone := fakeClone(t)
	ens := &fakeEnsurer{}
	deps := snapshotDeps{worktrees: ens, pins: fakePins{repoID: {RepositoryID: repoID, BaseRef: "develop", BaseSHA: pinA}}}
	repos := []repoRef{{ID: repoID.String(), Name: "app", ClonePath: clone, BaseSHA: pinA}}

	got, err := pinnedReadPaths(context.Background(), deps, uuid.New(), repos)
	if err != nil {
		t.Fatal(err)
	}
	want := snapshot.WorktreePathIn(filepath.Join(clone, ".git"), pinA)
	if got.Paths[0] != want {
		t.Fatalf("read path = %s, want the snapshot %s (never the clone)", got.Paths[0], want)
	}
	if got.BaseSHAs[repoID.String()] != pinA {
		t.Fatalf("base_shas = %v", got.BaseSHAs)
	}
	if len(ens.calls) != 1 || ens.calls[0] != [3]string{clone, "develop", pinA} {
		t.Fatalf("Ensure calls = %v", ens.calls)
	}
}

func TestPinnedReadPaths_PinRowWinsOverAStaleMessage(t *testing.T) {
	repoID := uuid.New()
	clone := fakeClone(t)
	ens := &fakeEnsurer{}
	deps := snapshotDeps{worktrees: ens, pins: fakePins{repoID: {RepositoryID: repoID, BaseRef: "main", BaseSHA: pinB}}}
	got, err := pinnedReadPaths(context.Background(), deps, uuid.New(), []repoRef{{ID: repoID.String(), ClonePath: clone, BaseSHA: pinA}})
	if err != nil {
		t.Fatal(err)
	}
	if got.BaseSHAs[repoID.String()] != pinB || ens.calls[0][2] != pinB {
		t.Fatalf("expected the stored pin %s to be read; got %v / %v", pinB, got.BaseSHAs, ens.calls)
	}
}

func TestPinnedReadPaths_MessagePinWithoutRowUsesRepositoryBaseBranch(t *testing.T) {
	repoID := uuid.New()
	clone := fakeClone(t)
	ens := &fakeEnsurer{}
	deps := snapshotDeps{worktrees: ens, pins: fakePins{}, baseBranch: func(context.Context, string) string { return "trunk" }}
	if _, err := pinnedReadPaths(context.Background(), deps, uuid.New(), []repoRef{{ID: repoID.String(), ClonePath: clone, BaseSHA: pinA}}); err != nil {
		t.Fatal(err)
	}
	if ens.calls[0] != [3]string{clone, "trunk", pinA} {
		t.Fatalf("Ensure calls = %v", ens.calls)
	}
}

func TestPinnedReadPaths_LegacyUnpinnedKeepsTheCloneAndIsCounted(t *testing.T) {
	clone := fakeClone(t)
	ens := &fakeEnsurer{}
	before := snapshot.UnpinnedCount(agentID)
	got, err := pinnedReadPaths(context.Background(), snapshotDeps{worktrees: ens, pins: fakePins{}}, uuid.New(),
		[]repoRef{{ID: uuid.NewString(), ClonePath: clone}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Paths[0] != clone || got.BaseSHAs != nil || len(ens.calls) != 0 {
		t.Fatalf("legacy read = %+v, calls %v", got, ens.calls)
	}
	if snapshot.UnpinnedCount(agentID) != before+1 {
		t.Fatal("a legacy-unpinned read must be counted")
	}
}

func TestPinnedReadPaths_SnapshotFailureFailsTheTask(t *testing.T) {
	repoID := uuid.New()
	ens := &fakeEnsurer{err: errors.New("fetching origin main failed")}
	_, err := pinnedReadPaths(context.Background(), snapshotDeps{worktrees: ens, pins: fakePins{}}, uuid.New(),
		[]repoRef{{ID: repoID.String(), Name: "app", ClonePath: fakeClone(t), BaseSHA: pinA}})
	if err == nil || !strings.Contains(err.Error(), "pinned snapshot 7af0d7084cd8 unavailable") {
		t.Fatalf("err = %v", err)
	}
}

func TestPinnedReadPaths_CloneNotOnDiskIsLeftToTheAvailabilityCheck(t *testing.T) {
	ens := &fakeEnsurer{}
	missing := filepath.Join(t.TempDir(), "nope")
	got, err := pinnedReadPaths(context.Background(), snapshotDeps{worktrees: ens}, uuid.New(),
		[]repoRef{{ID: uuid.NewString(), ClonePath: missing, BaseSHA: pinA}})
	if err != nil || got.Paths[0] != missing || len(ens.calls) != 0 {
		t.Fatalf("got %+v err %v calls %v", got, err, ens.calls)
	}
}

func TestSelectResearchRepos(t *testing.T) {
	a := repoRef{ID: "a", ClonePath: "/a"}
	b := repoRef{ID: "b", ClonePath: "/b"}
	if got := selectResearchRepos(researchTaskMessage{RepositoryID: "b", RepoList: []repoRef{a, b}}); len(got) != 1 || got[0].ID != "b" {
		t.Fatalf("named repo: %v", got)
	}
	if got := selectResearchRepos(researchTaskMessage{RepositoryID: "c", RepoList: []repoRef{a}}); len(got) != 0 {
		t.Fatalf("unknown named repo must select nothing: %v", got)
	}
	if got := selectResearchRepos(researchTaskMessage{RepoList: []repoRef{a, b}}); len(got) != 2 {
		t.Fatalf("all repos: %v", got)
	}
	if got := selectResearchRepos(researchTaskMessage{}); len(got) != 1 || got[0].ClonePath != defaultRepoPath {
		t.Fatalf("legacy default: %v", got)
	}
}

func TestSnapshotPromptLine(t *testing.T) {
	repos := []repoRef{{ID: "r1"}}
	line := snapshotPromptLine(repos, []string{"/snap"}, "/snap", map[string]string{"r1": pinA})
	if !strings.Contains(line, "base 7af0d7084cd8 (pinned") {
		t.Fatalf("line = %q", line)
	}
	if snapshotPromptLine(repos, []string{"/clone"}, "/clone", nil) != "" {
		t.Fatal("a legacy read has no snapshot line")
	}
}
