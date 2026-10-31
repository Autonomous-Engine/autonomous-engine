package main

import (
	"archive/tar"
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/contracts"
	"github.com/google/uuid"
)

// newContractRepo is newDriftRepo with the base commit holding files.
func newContractRepo(t *testing.T, files map[string]string) *driftRepo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	r := &driftRepo{t: t, root: t.TempDir()}
	r.work = filepath.Join(r.root, "work")
	r.remote = filepath.Join(r.root, "remote.git")
	r.clone = filepath.Join(r.root, "clone")
	r.git(r.root, "init", "-q", "-b", "main", r.work)
	r.git(r.work, "config", "core.autocrlf", "false")
	r.writeFiles(files)
	r.git(r.work, "add", "-A")
	r.git(r.work, "commit", "-q", "-m", "base")
	r.pin = r.git(r.work, "rev-parse", "HEAD")
	r.git(r.root, "clone", "-q", "--bare", r.work, r.remote)
	r.git(r.root, "clone", "-q", r.remote, r.clone)
	return r
}

func (r *driftRepo) writeFiles(files map[string]string) {
	r.t.Helper()
	for name, content := range files {
		p := filepath.Join(r.work, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			r.t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			r.t.Fatal(err)
		}
	}
}

// pushBranchFiles commits files on a branch from the pin and pushes it to
// origin only (the clone does not have it); returns its head.
func (r *driftRepo) pushBranchFiles(branch string, files map[string]string) string {
	r.t.Helper()
	r.git(r.work, "checkout", "-q", "-b", "src-"+strings.ReplaceAll(branch, "/", "-"), r.pin)
	r.writeFiles(files)
	r.git(r.work, "add", "-A")
	r.git(r.work, "commit", "-q", "-m", "agent change")
	head := r.git(r.work, "rev-parse", "HEAD")
	r.git(r.work, "push", "-q", r.remote, "HEAD:refs/heads/"+branch)
	r.git(r.work, "checkout", "-q", "main")
	return head
}

func usersSpec(fields ...string) string {
	props := make([]string, 0, len(fields))
	for _, f := range fields {
		props = append(props, `"`+f+`": {"type": "string"}`)
	}
	return `{"openapi": "3.0.0", "paths": {"/api/users/{id}": {"get": {"responses": {"200": {"content": {"application/json": {"schema": {"type": "object", "properties": {` + strings.Join(props, ", ") + `}}}}}}}}}}`
}

const usersClient = "export async function loadUser(id: string) {\n  const res = await fetch(`/api/users/${id}`)\n  const body = await res.json()\n  return body.email\n}\n"

// The producer's head and pinned base and the consumer's head are
// materialised from origin without touching either clone, and the REST
// checker finds the removed field the consumer reads.
func TestMaterialize_ProducerAndConsumerTreesFeedTheChecker(t *testing.T) {
	producer := newContractRepo(t, map[string]string{"api/openapi.json": usersSpec("id", "email")})
	consumer := newContractRepo(t, map[string]string{"README.md": "web\n"})
	workID, producerTask, consumerTask := uuid.New(), uuid.New(), uuid.New()
	pHead := producer.pushBranchFiles(agentBranchFor(workID, producerTask), map[string]string{"api/openapi.json": usersSpec("id")})
	cHead := consumer.pushBranchFiles(agentBranchFor(workID, consumerTask), map[string]string{"src/user.ts": usersClient})
	before := producer.cloneState() + consumer.cloneState()

	m := &treeMaterializer{}
	ctx := context.Background()
	p, err := m.materialize(ctx, producer.clone, agentBranchFor(workID, producerTask), "main", pHead, producer.pin)
	if err != nil {
		t.Fatalf("materialize producer: %v", err)
	}
	defer p.cleanup()
	c, err := m.materialize(ctx, consumer.clone, agentBranchFor(workID, consumerTask), "", cHead)
	if err != nil {
		t.Fatalf("materialize consumer: %v", err)
	}
	defer c.cleanup()
	if after := producer.cloneState() + consumer.cloneState(); after != before {
		t.Fatal("materialising changed a clone")
	}
	if _, err := os.Stat(filepath.Join(c.dirs[cHead], "src", "user.ts")); err != nil {
		t.Fatalf("consumer head tree: %v", err)
	}

	res := contracts.Check(ctx, contracts.Ref{Type: "rest", Name: "GET /api/users/{id}"},
		contracts.Side{Base: p.dirs[producer.pin], Head: p.dirs[pHead]}, contracts.Side{Head: c.dirs[cHead]})
	if res.Status != contracts.StatusMismatch || !strings.Contains(res.Summary(), "removed field email") || !strings.Contains(res.Summary(), "src/user.ts:4:") {
		t.Fatalf("result %s: %s", res.Status, res.Summary())
	}

	// No verified head: the branch tip is used.
	tip, err := m.materialize(ctx, consumer.clone, agentBranchFor(workID, consumerTask), "", "")
	if err != nil {
		t.Fatalf("materialize branch tip: %v", err)
	}
	defer tip.cleanup()
	if _, err := os.Stat(filepath.Join(tip.dirs[""], "src", "user.ts")); err != nil {
		t.Fatalf("branch tip tree: %v", err)
	}

	// A branch origin does not have: an error (the caller fails closed).
	if _, err := m.materialize(ctx, consumer.clone, "agent/none/none", "", ""); err == nil {
		t.Fatal("materialising a missing branch succeeded")
	}
}

func TestUntar_SkipsEscapingPathsAndLinks(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	add := func(h *tar.Header, body string) {
		h.Size = int64(len(body))
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if body != "" {
			if _, err := tw.Write([]byte(body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	add(&tar.Header{Name: "ok/a.txt", Typeflag: tar.TypeReg, Mode: 0o644}, "a")
	add(&tar.Header{Name: "../evil.txt", Typeflag: tar.TypeReg, Mode: 0o644}, "x")
	add(&tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}, "")
	tw.Close()
	dir := filepath.Join(t.TempDir(), "out")
	if err := untar(&buf, dir); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "ok", "a.txt")); err != nil || string(b) != "a" {
		t.Fatalf("ok/a.txt = %q, %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "evil.txt")); err == nil {
		t.Fatal("an escaping path was written")
	}
	if _, err := os.Lstat(filepath.Join(dir, "link")); err == nil {
		t.Fatal("a link was written")
	}
}

func TestContractBlockReason_NamesEachMismatch(t *testing.T) {
	mismatch := contractChecked{ProducerRepo: "api", Result: contracts.Result{
		Contract: contracts.Ref{Type: "rest", Name: "GET /api/users/{id}"}, Status: contracts.StatusMismatch,
		Mismatches: []contracts.Mismatch{{ProducerChange: "removed field email from the GET /api/users/{id} response (openapi.json)", ConsumerRef: "src/user.ts:4: return body.email"}},
	}}
	unchecked := contractChecked{ProducerRepo: "api", Result: contracts.Result{
		Contract: contracts.Ref{Type: "shared_package", Name: "@acme/ui"}, Status: contracts.StatusUnchecked, Details: []string{"no checker"},
	}}
	reason := contractBlockReason("web", []contractChecked{mismatch}, []contractChecked{unchecked}, nil)
	for _, want := range []string{
		"Contract mismatch: the pull request of web was not opened",
		`contract rest "GET /api/users/{id}": the producer removed field email from the GET /api/users/{id} response (openapi.json), but the consumer still uses it at src/user.ts:4: return body.email`,
		`contract shared_package "@acme/ui": unchecked (no checker) — blocking because the product requires every contract to be checked (contracts_strict)`,
	} {
		if !strings.Contains(reason, want) {
			t.Errorf("reason lacks %q:\n%s", want, reason)
		}
	}
}

func TestContractDescriptionAndPairing(t *testing.T) {
	checked := []contractChecked{{ProducerRepo: "api", ProducerPRURL: "https://github.com/acme/api/pull/7", Result: contracts.Result{
		Contract: contracts.Ref{Type: "event", Name: "user.created"}, Status: contracts.StatusOK,
	}}, {ProducerRepo: "api", Result: contracts.Result{
		Contract: contracts.Ref{Type: "other", Name: "x"}, Status: contracts.StatusUnchecked, Details: []string{"no checker for contract type \"other\""},
	}}}
	d := contractDescription(checked)
	for _, want := range []string{"## Cross-repository contracts", "- `event` user.created — **ok** (producer: api, paired PR: https://github.com/acme/api/pull/7)", "**unchecked** (producer: api): no checker"} {
		if !strings.Contains(d, want) {
			t.Errorf("description lacks %q:\n%s", want, d)
		}
	}
	if contractDescription(nil) != "" || pairedPRLine(nil) != "" {
		t.Error("no contracts must add nothing to the description")
	}
	if l := pairedPRLine([]openedPR{{URL: "https://github.com/acme/api/pull/7", RepoName: "api"}}); !strings.Contains(l, "https://github.com/acme/api/pull/7 (api)") {
		t.Errorf("paired line = %q", l)
	}
}

func TestContractCheckRecord_WorstStatusPerProducer(t *testing.T) {
	p1, p2 := uuid.NewString(), uuid.NewString()
	updates, err := contractCheckUpdates([]contractChecked{
		{ProducerTaskID: p1, Result: contracts.Result{Status: contracts.StatusOK}},
		{ProducerTaskID: p1, Result: contracts.Result{Status: contracts.StatusUnchecked}},
		{ProducerTaskID: p2, Result: contracts.Result{Status: contracts.StatusOK}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(updates[p1], `"status":"unchecked"`) || !strings.Contains(updates[p2], `"status":"ok"`) {
		t.Fatalf("updates = %v", updates)
	}
}

// Source check: the contract check is wired once, after the drift check and
// before the PR is gated or created, and its results reach both payloads.
func TestPRCoordinator_ContractCheckSitsNextToTheDriftCheck(t *testing.T) {
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	src := strings.ReplaceAll(string(b), "\r\n", "\n")
	drift := strings.Index(src, "drift := applyBaseDrift(")
	check := strings.Index(src, "applyContractChecks(msgCtx, prContractChecker,")
	gate := strings.Index(src, `if gateReason != "" {`) // G1: manual pr_gate_mode or a non-GitHub remote
	create := strings.Index(src, "createGitHubPR(msgCtx, githubServiceURL,")
	if strings.Count(src, "applyContractChecks(") != 1 || drift < 0 || check < drift || gate < check || create < check {
		t.Fatalf("drift=%d check=%d gate=%d create=%d", drift, check, gate, create)
	}
	if strings.Count(src, "ContractsChecked:  contractsChecked.Checked,") != 2 {
		t.Error("contracts_checked must be set on both pr.gated and pr.ready")
	}
}
