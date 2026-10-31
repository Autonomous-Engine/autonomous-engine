package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/logging"
	"github.com/google/uuid"
)

// initRepoAt initialises a git repository with one commit on `main` at an
// exact path (initTestRepository in main_test.go picks its own temp dir).
func initRepoAt(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "init", "-b", "main")
	gitRun(t, dir, "config", "user.email", "test@example.com")
	gitRun(t, dir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "README.md")
	gitRun(t, dir, "commit", "-m", "initial")
	return dir
}

// newMarkedWorktree creates a real linked worktree for a fresh task ID next to
// clonePath and writes its marker, exactly as the handler does.
func newMarkedWorktree(t *testing.T, clonePath string) (string, uuid.UUID) {
	t.Helper()
	taskID := uuid.New()
	path := worktreePathFor(clonePath, taskID)
	branch := "agent/work/" + taskID.String()[:8]
	if err := createIsolatedWorktree(context.Background(), clonePath, path, branch, "main"); err != nil {
		t.Fatalf("createIsolatedWorktree(%s): %v", path, err)
	}
	if err := writeWorktreeMarker(path, taskID.String(), "work-1", clonePath); err != nil {
		t.Fatalf("writeWorktreeMarker(%s): %v", path, err)
	}
	return path, taskID
}

// editMarker rewrites a worktree's marker in place.
func editMarker(t *testing.T, worktreePath string, edit func(*worktreeMarker)) {
	t.Helper()
	m, err := readWorktreeMarker(worktreePath)
	if err != nil {
		t.Fatal(err)
	}
	edit(&m)
	data, _ := json.Marshal(m)
	if err := os.WriteFile(filepath.Join(worktreePath, worktreeMarkerFile), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// markDead rewrites a marker so it looks like it was written by a previous
// process instance on this host (the crashed-agent case). Hostname and PID
// are left as written: after an in-place container restart both are reused.
func markDead(t *testing.T, worktreePath string) {
	t.Helper()
	editMarker(t, worktreePath, func(m *worktreeMarker) { m.InstanceID = "dead-" + uuid.NewString() })
}

func registeredWorktrees(t *testing.T, clonePath string) []string {
	t.Helper()
	var paths []string
	for _, line := range strings.Split(gitOutput(t, clonePath, "worktree", "list", "--porcelain"), "\n") {
		if strings.HasPrefix(line, "worktree ") {
			paths = append(paths, strings.TrimPrefix(line, "worktree "))
		}
	}
	return paths
}

func assertPrimaryIntact(t *testing.T, clonePath string) {
	t.Helper()
	if branch := gitOutput(t, clonePath, "branch", "--show-current"); branch != "main" {
		t.Fatalf("primary checkout branch = %q, want main", branch)
	}
	if status := gitOutput(t, clonePath, "status", "--porcelain"); status != "" {
		t.Fatalf("primary checkout status not clean:\n%s", status)
	}
	if _, err := os.Stat(filepath.Join(clonePath, "README.md")); err != nil {
		t.Fatalf("primary checkout lost README.md: %v", err)
	}
}

func TestWorktreePathForUsesFullTaskID(t *testing.T) {
	taskID := uuid.New()
	got := worktreePathFor(filepath.Join("/repo-cache", "product", "repo")+string(filepath.Separator), taskID)
	if filepath.Base(got) != "worktree-"+taskID.String() {
		t.Fatalf("worktree dir = %q, want worktree-%s", filepath.Base(got), taskID)
	}
	if filepath.Dir(got) != filepath.Join("/repo-cache", "product") {
		t.Fatalf("worktree parent = %q, want sibling of the clone", filepath.Dir(got))
	}
}

func TestWorktreeMarkerIsWrittenAndExcludedFromStatus(t *testing.T) {
	root := t.TempDir()
	clone := initRepoAt(t, filepath.Join(root, "product", "repo"))
	path, taskID := newMarkedWorktree(t, clone)

	m, err := readWorktreeMarker(path)
	if err != nil {
		t.Fatalf("readWorktreeMarker: %v", err)
	}
	if m.TaskID != taskID.String() || m.WorkID != "work-1" || m.ClonePath != clone {
		t.Fatalf("marker = %+v", m)
	}
	if m.PID != os.Getpid() || m.CreatedAt.IsZero() || m.InstanceID != processInstanceID {
		t.Fatalf("marker pid/created_at/instance_id not populated: %+v", m)
	}
	// The handler runs `git add -A` in the worktree; the marker must never be
	// staged into the task branch.
	if status := gitOutput(t, path, "status", "--porcelain"); status != "" {
		t.Fatalf("marker leaks into git status:\n%s", status)
	}
	assertPrimaryIntact(t, clone)
}

func TestCreateIsolatedWorktreeParallel(t *testing.T) {
	const n = 20
	root := t.TempDir()
	clone := initRepoAt(t, filepath.Join(root, "product", "repo"))

	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		paths = make(map[string]struct{}, n)
		errs  []error
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			taskID := uuid.New()
			path := worktreePathFor(clone, taskID)
			branch := "agent/work/" + taskID.String()[:8]
			err := createIsolatedWorktree(context.Background(), clone, path, branch, "main")
			if err == nil {
				err = writeWorktreeMarker(path, taskID.String(), "work-1", clone)
			}
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			paths[path] = struct{}{}
		}()
	}
	wg.Wait()

	if len(errs) > 0 {
		t.Fatalf("%d of %d parallel creations failed; first: %v", len(errs), n, errs[0])
	}
	if len(paths) != n {
		t.Fatalf("got %d unique worktree paths, want %d", len(paths), n)
	}
	for path := range paths {
		if info, err := os.Lstat(filepath.Join(path, ".git")); err != nil || !info.Mode().IsRegular() {
			t.Fatalf("%s: .git is not a file (err=%v)", path, err)
		}
		if _, err := readWorktreeMarker(path); err != nil {
			t.Fatalf("%s: marker missing: %v", path, err)
		}
	}
	if got := len(registeredWorktrees(t, clone)); got != n+1 {
		t.Fatalf("git worktree list shows %d entries, want %d", got, n+1)
	}
	assertPrimaryIntact(t, clone)
}

func TestReconcileAbandonedWorktreesAfterKill(t *testing.T) {
	root := t.TempDir()
	clone := initRepoAt(t, filepath.Join(root, "product", "repo"))
	// A second clone directly under the root, so its worktrees sit at depth 1.
	shallowClone := initRepoAt(t, filepath.Join(root, "default"))

	var abandoned []string
	for i := 0; i < 3; i++ {
		path, _ := newMarkedWorktree(t, clone)
		markDead(t, path)
		abandoned = append(abandoned, path)
	}
	shallowPath, _ := newMarkedWorktree(t, shallowClone)
	markDead(t, shallowPath)
	abandoned = append(abandoned, shallowPath)

	// Decoy: looks like a worktree dir but carries no marker. Never touched.
	decoy := filepath.Join(root, "product", "worktree-xxxx")
	if err := os.MkdirAll(decoy, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(decoy, "keep.txt"), []byte("keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	res := reconcileAbandonedWorktrees(context.Background(), logging.New("test"), root)
	if res.Removed != len(abandoned) || res.Skipped != 0 || res.Failed != 0 || res.Unmarked != 1 {
		t.Fatalf("reconcile result = %+v, want removed=%d skipped=0 failed=0 unmarked=1", res, len(abandoned))
	}
	for _, path := range abandoned {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("abandoned worktree %s still exists (err=%v)", path, err)
		}
	}
	if _, err := os.Stat(filepath.Join(decoy, "keep.txt")); err != nil {
		t.Fatalf("decoy without marker was touched: %v", err)
	}
	assertPrimaryIntact(t, clone)
	assertPrimaryIntact(t, shallowClone)
	for _, c := range []string{clone, shallowClone} {
		if got := registeredWorktrees(t, c); len(got) != 1 {
			t.Fatalf("%s: git worktree list still has stale entries: %v", c, got)
		}
	}

	// Second run is a no-op: nothing left with a marker, the decoy still counted.
	if res := reconcileAbandonedWorktrees(context.Background(), logging.New("test"), root); res != (reconcileResult{Unmarked: 1}) {
		t.Fatalf("second reconcile result = %+v, want only unmarked=1", res)
	}
}

func TestReconcileStopsAtDeadline(t *testing.T) {
	root := t.TempDir()
	clone := initRepoAt(t, filepath.Join(root, "product", "repo"))
	var abandoned []string
	for i := 0; i < 2; i++ {
		path, _ := newMarkedWorktree(t, clone)
		markDead(t, path)
		abandoned = append(abandoned, path)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // deadline already passed before the first candidate
	res := reconcileAbandonedWorktrees(ctx, logging.New("test"), root)
	if res != (reconcileResult{}) {
		t.Fatalf("reconcile result after deadline = %+v, want all zero", res)
	}
	for _, path := range abandoned {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("worktree touched after deadline: %v", err)
		}
	}
	if got := registeredWorktrees(t, clone); len(got) != 3 {
		t.Fatalf("git worktree list = %v, want primary + 2 untouched", got)
	}
}

func TestIsSafeToRemoveRefusesSymlinkOutsideRoot(t *testing.T) {
	root := t.TempDir()
	clone := initRepoAt(t, filepath.Join(root, "product", "repo"))
	outside := initRepoAt(t, filepath.Join(t.TempDir(), "elsewhere"))
	target, _ := newMarkedWorktree(t, outside)
	markDead(t, target)

	// A worktree-* entry under the root that is really a link to a worktree
	// outside it.
	link := filepath.Join(root, "product", "worktree-"+uuid.NewString())
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create a directory symlink on this host: %v", err)
	}

	err := isSafeToRemove(link, clone, root)
	if err == nil || !strings.Contains(err.Error(), "not under repo cache root") {
		t.Fatalf("isSafeToRemove(symlink outside root) = %v, want not-under-root refusal", err)
	}

	// Reconcile lists the link as a candidate and must refuse it too.
	res := reconcileAbandonedWorktrees(context.Background(), logging.New("test"), root)
	if res.Removed != 0 || res.Skipped != 1 || res.Failed != 0 {
		t.Fatalf("reconcile result = %+v, want skipped=1", res)
	}
	if _, err := os.Stat(filepath.Join(target, worktreeMarkerFile)); err != nil {
		t.Fatalf("symlink target was touched: %v", err)
	}
	if got := registeredWorktrees(t, outside); len(got) != 2 {
		t.Fatalf("outside clone worktree list = %v, want primary + intact worktree", got)
	}
	assertPrimaryIntact(t, outside)
}

func TestReconcileSkipsWorktreeOwnedByThisInstance(t *testing.T) {
	root := t.TempDir()
	clone := initRepoAt(t, filepath.Join(root, "product", "repo"))
	path, _ := newMarkedWorktree(t, clone) // marker carries this process's instance_id

	res := reconcileAbandonedWorktrees(context.Background(), logging.New("test"), root)
	if res.Removed != 0 || res.Skipped != 1 || res.Failed != 0 {
		t.Fatalf("reconcile result = %+v, want skipped=1", res)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("live worktree was removed: %v", err)
	}
}

func TestReconcileRestartInPlaceRemovesPreviousInstanceWorktree(t *testing.T) {
	root := t.TempDir()
	clone := initRepoAt(t, filepath.Join(root, "product", "repo"))
	path, _ := newMarkedWorktree(t, clone)
	// Container restarted in place: same hostname, same PID (the agent is
	// PID 1), only the instance ID differs.
	hostname, _ := os.Hostname()
	editMarker(t, path, func(m *worktreeMarker) {
		m.Hostname = hostname
		m.PID = os.Getpid()
		m.InstanceID = "previous-" + uuid.NewString()
	})

	res := reconcileAbandonedWorktrees(context.Background(), logging.New("test"), root)
	if res.Removed != 1 || res.Skipped != 0 || res.Failed != 0 {
		t.Fatalf("reconcile result = %+v, want removed=1", res)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("previous instance's worktree still exists (err=%v)", err)
	}
	if got := registeredWorktrees(t, clone); len(got) != 1 {
		t.Fatalf("stale worktree entries remain: %v", got)
	}
	assertPrimaryIntact(t, clone)
}

func TestReconcileSkipsInvalidMarkers(t *testing.T) {
	cases := []struct {
		name  string
		write func(t *testing.T, path string)
	}{
		{"empty clone_path", func(t *testing.T, path string) {
			editMarker(t, path, func(m *worktreeMarker) { m.ClonePath = "" })
		}},
		{"relative clone_path", func(t *testing.T, path string) {
			editMarker(t, path, func(m *worktreeMarker) { m.ClonePath = filepath.Join("product", "repo") })
		}},
		{"empty task_id", func(t *testing.T, path string) {
			editMarker(t, path, func(m *worktreeMarker) { m.TaskID = "" })
		}},
		{"malformed JSON", func(t *testing.T, path string) {
			if err := os.WriteFile(filepath.Join(path, worktreeMarkerFile), []byte("{not json"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			clone := initRepoAt(t, filepath.Join(root, "product", "repo"))
			path, _ := newMarkedWorktree(t, clone)
			markDead(t, path) // would be removed if the marker were valid
			tc.write(t, path)

			res := reconcileAbandonedWorktrees(context.Background(), logging.New("test"), root)
			if res.Removed != 0 || res.Skipped != 1 || res.Failed != 0 {
				t.Fatalf("reconcile result = %+v, want skipped=1", res)
			}
			if info, err := os.Lstat(filepath.Join(path, ".git")); err != nil || !info.Mode().IsRegular() {
				t.Fatalf("worktree was touched: .git err=%v", err)
			}
			if _, err := os.Stat(filepath.Join(path, worktreeMarkerFile)); err != nil {
				t.Fatalf("marker was touched: %v", err)
			}
			if got := registeredWorktrees(t, clone); len(got) != 2 {
				t.Fatalf("git worktree list = %v, want primary + untouched worktree", got)
			}
			assertPrimaryIntact(t, clone)
		})
	}
}

func TestReconcileForeignHostUnknownAgeIsSkipped(t *testing.T) {
	root := t.TempDir()
	clone := initRepoAt(t, filepath.Join(root, "product", "repo"))
	path, _ := newMarkedWorktree(t, clone)
	editMarker(t, path, func(m *worktreeMarker) {
		m.Hostname = "other-replica"
		m.InstanceID = "other-" + uuid.NewString()
		m.CreatedAt = time.Time{} // unknown age must never count as stale
	})

	res := reconcileAbandonedWorktrees(context.Background(), logging.New("test"), root)
	if res.Removed != 0 || res.Skipped != 1 || res.Failed != 0 {
		t.Fatalf("reconcile result = %+v, want skipped=1", res)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("unknown-age foreign worktree was removed: %v", err)
	}
}

func TestReconcileForeignHostFreshMarkerIsSkipped(t *testing.T) {
	root := t.TempDir()
	clone := initRepoAt(t, filepath.Join(root, "product", "repo"))
	path, _ := newMarkedWorktree(t, clone)
	// A sibling replica sharing the volume: different host, task in progress.
	editMarker(t, path, func(m *worktreeMarker) {
		m.Hostname = "other-replica"
		m.PID = 1
		m.InstanceID = "other-" + uuid.NewString()
	})

	res := reconcileAbandonedWorktrees(context.Background(), logging.New("test"), root)
	if res.Removed != 0 || res.Skipped != 1 || res.Failed != 0 {
		t.Fatalf("reconcile result = %+v, want skipped=1", res)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("foreign fresh worktree was removed: %v", err)
	}
	assertPrimaryIntact(t, clone)
}

func TestReconcileForeignHostStaleMarkerIsRemoved(t *testing.T) {
	root := t.TempDir()
	clone := initRepoAt(t, filepath.Join(root, "product", "repo"))
	path, _ := newMarkedWorktree(t, clone)
	editMarker(t, path, func(m *worktreeMarker) {
		m.Hostname = "other-replica"
		m.PID = 1
		m.InstanceID = "other-" + uuid.NewString()
		m.CreatedAt = time.Now().Add(-2 * defaultWorktreeStaleAfter)
	})

	res := reconcileAbandonedWorktrees(context.Background(), logging.New("test"), root)
	if res.Removed != 1 || res.Skipped != 0 || res.Failed != 0 {
		t.Fatalf("reconcile result = %+v, want removed=1", res)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("stale foreign worktree still exists (err=%v)", err)
	}
	if got := registeredWorktrees(t, clone); len(got) != 1 {
		t.Fatalf("stale worktree entries remain: %v", got)
	}
	assertPrimaryIntact(t, clone)
}

func TestReconcileForeignHostStaleThresholdFromEnv(t *testing.T) {
	root := t.TempDir()
	clone := initRepoAt(t, filepath.Join(root, "product", "repo"))
	path, _ := newMarkedWorktree(t, clone)
	editMarker(t, path, func(m *worktreeMarker) {
		m.Hostname = "other-replica"
		m.PID = 1
		m.InstanceID = "other-" + uuid.NewString()
		m.CreatedAt = time.Now().Add(-2 * time.Hour)
	})

	// Two hours old is fresh under the 24h default but stale under a 1h threshold.
	t.Setenv(envWorktreeStaleAfter, "1h")
	res := reconcileAbandonedWorktrees(context.Background(), logging.New("test"), root)
	if res.Removed != 1 || res.Skipped != 0 {
		t.Fatalf("reconcile result with 1h threshold = %+v, want removed=1", res)
	}
}

func TestReconcileSameHostFreshMarkerIsRemoved(t *testing.T) {
	root := t.TempDir()
	clone := initRepoAt(t, filepath.Join(root, "product", "repo"))
	path, _ := newMarkedWorktree(t, clone) // hostname is ours, created_at is now
	markDead(t, path)                      // the previous process on this host is gone

	res := reconcileAbandonedWorktrees(context.Background(), logging.New("test"), root)
	if res.Removed != 1 || res.Skipped != 0 || res.Failed != 0 {
		t.Fatalf("reconcile result = %+v, want removed=1", res)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("own fresh abandoned worktree still exists (err=%v)", err)
	}
	assertPrimaryIntact(t, clone)
}

func TestWorktreeStaleAfterFromEnv(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  time.Duration
	}{
		{"", defaultWorktreeStaleAfter},
		{"1h", time.Hour},
		{"90m", 90 * time.Minute},
		{"bogus", defaultWorktreeStaleAfter},
		{"-5m", defaultWorktreeStaleAfter},
		{"0", defaultWorktreeStaleAfter},
	} {
		t.Setenv(envWorktreeStaleAfter, tc.value)
		if got := worktreeStaleAfterFromEnv(); got != tc.want {
			t.Fatalf("%s=%q: got %v, want %v", envWorktreeStaleAfter, tc.value, got, tc.want)
		}
	}
}

func TestWorktreeReconcileTimeoutFromEnv(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  time.Duration
	}{
		{"", defaultWorktreeReconcileTimeout},
		{"30s", 30 * time.Second},
		{"bogus", defaultWorktreeReconcileTimeout},
		{"0", defaultWorktreeReconcileTimeout},
	} {
		t.Setenv(envWorktreeReconcileTimeout, tc.value)
		if got := worktreeReconcileTimeoutFromEnv(); got != tc.want {
			t.Fatalf("%s=%q: got %v, want %v", envWorktreeReconcileTimeout, tc.value, got, tc.want)
		}
	}
}

func TestReconcileNeverRemovesPrimaryCheckoutEvenWithMarker(t *testing.T) {
	root := t.TempDir()
	// A primary checkout whose directory happens to match worktree-* and which
	// carries a (bogus) marker pointing at itself.
	primary := initRepoAt(t, filepath.Join(root, "product", "worktree-"+uuid.NewString()))
	if err := writeWorktreeMarker(primary, uuid.NewString(), "work-1", primary); err != nil {
		t.Fatal(err)
	}
	markDead(t, primary)

	res := reconcileAbandonedWorktrees(context.Background(), logging.New("test"), root)
	if res.Removed != 0 || res.Skipped != 1 {
		t.Fatalf("reconcile result = %+v, want skipped=1 removed=0", res)
	}
	assertPrimaryIntact(t, primary)
}

func TestIsSafeToRemove(t *testing.T) {
	root := t.TempDir()
	clone := initRepoAt(t, filepath.Join(root, "product", "repo"))
	linked, _ := newMarkedWorktree(t, clone)
	outside := initRepoAt(t, filepath.Join(t.TempDir(), "elsewhere"))
	outsideLinked, _ := newMarkedWorktree(t, outside)
	plainDir := filepath.Join(root, "product", "worktree-plain")
	if err := os.MkdirAll(plainDir, 0o755); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		path    string
		wantErr string
	}{
		{"linked worktree under root", linked, ""},
		{"primary checkout", clone, "primary checkout"},
		{"primary checkout with trailing separator", clone + string(filepath.Separator), "primary checkout"},
		{"repo cache root itself", root, "not under repo cache root"},
		{"outside repo cache root", outsideLinked, "not under repo cache root"},
		{"directory without .git", plainDir, "not a linked worktree"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := isSafeToRemove(tc.path, clone, root)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("isSafeToRemove(%s) = %v, want nil", tc.path, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("isSafeToRemove(%s) = %v, want error containing %q", tc.path, err, tc.wantErr)
			}
		})
	}

	// A second primary checkout under the root (different clone_path) must
	// still be refused because its .git is a directory.
	other := initRepoAt(t, filepath.Join(root, "product", "other"))
	if err := isSafeToRemove(other, clone, root); err == nil || !strings.Contains(err.Error(), ".git is a directory") {
		t.Fatalf("isSafeToRemove(other primary) = %v, want .git-is-a-directory refusal", err)
	}
}

func TestRemoveWorktreeRefusesPrimaryCheckout(t *testing.T) {
	root := t.TempDir()
	clone := initRepoAt(t, filepath.Join(root, "product", "repo"))
	if err := removeWorktree(context.Background(), clone, clone, root); err == nil {
		t.Fatal("removeWorktree(primary) = nil, want refusal")
	}
	assertPrimaryIntact(t, clone)

	linked, _ := newMarkedWorktree(t, clone)
	if err := removeWorktree(context.Background(), clone, linked, root); err != nil {
		t.Fatalf("removeWorktree(linked) = %v", err)
	}
	if _, err := os.Stat(linked); !os.IsNotExist(err) {
		t.Fatalf("linked worktree still exists (err=%v)", err)
	}
	if got := registeredWorktrees(t, clone); len(got) != 1 {
		t.Fatalf("stale worktree entries remain: %v", got)
	}
}

func TestReconcileEnabledFlag(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"", true}, {"on", true}, {"1", true}, {"off", false}, {"OFF", false}, {"0", false}, {"false", false},
	} {
		t.Setenv(envWorktreeReconcile, tc.value)
		if got := reconcileEnabled(); got != tc.want {
			t.Fatalf("%s=%q: reconcileEnabled() = %v, want %v", envWorktreeReconcile, tc.value, got, tc.want)
		}
	}
}

func TestRepoCacheRootFromEnv(t *testing.T) {
	t.Setenv(envRepoCacheRoot, "")
	if got := repoCacheRootFromEnv(); got != defaultRepoCacheRoot {
		t.Fatalf("default root = %q, want %q", got, defaultRepoCacheRoot)
	}
	t.Setenv(envRepoCacheRoot, "/custom/cache")
	if got := repoCacheRootFromEnv(); got != "/custom/cache" {
		t.Fatalf("root = %q, want /custom/cache", got)
	}
}
