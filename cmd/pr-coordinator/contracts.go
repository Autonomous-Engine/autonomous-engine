package main

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/contracts"
	"github.com/autonomousengine/autonomous-engine/internal/gitremote"
	"github.com/autonomousengine/autonomous-engine/internal/guards"
	"github.com/autonomousengine/autonomous-engine/internal/interactions"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/snapshot"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// M2-06 PR-time contract checks. Before the CONSUMER subtask's pull request
// is opened (or gated), pr-coordinator reads the consumer's contract edges
// (plan_task_dependencies kind 'contract', written by the planner),
// materialises the producer's pinned base, the producer's head and the
// consumer's head, and runs internal/contracts.Check for every contract:
//
//   - mismatch: the parent is blocked with each mismatch (contract, what the
//     producer changed, the consumer's file:line) and the escalation prompt,
//     in one guarded transaction (the base-drift fail-closed pattern); no PR
//     is opened for this or any later repository;
//   - unchecked: recorded (PR body, pr.ready.contracts_checked, the edge's
//     contract_check); it blocks like a mismatch only when the product's
//     contracts_strict is on;
//   - ok: recorded, and the PR proceeds.
//
// A check that cannot run (a tree that cannot be materialised, the edges
// cannot be read) fails closed: blocked with the cause.
//
// Trees are materialised the way the drift check reads origin: a scratch
// bare repository under the temp dir borrowing the clone's objects
// (objects/info/alternates), fetching from the clone's origin what it lacks,
// and `git archive` of the exact commit extracted into a temp directory.
// Nothing is written to the shared clone.
//
// After the consumer's PR exists, the PR descriptions are paired: the
// consumer's body links the producer PRs opened in this run, and each
// producer PR's body is edited to link the consumer's (github-service
// POST /pr/link). No GitHub-side merge gating.

// contractChecked is one element of pr.ready.contracts_checked.
type contractChecked struct {
	contracts.Result
	ProducerTaskID string `json:"producer_task_id"`
	ProducerRepo   string `json:"producer_repository,omitempty"`
	ProducerPRURL  string `json:"producer_pr_url,omitempty"`
}

// consumerEdge is one contract edge of a consumer subtask: the producer
// subtask, its repository, and the contracts.
type consumerEdge struct {
	ProducerTaskID uuid.UUID
	ProducerRepoID uuid.UUID
	Refs           []contracts.Ref
}

// edgeQuerier is the pool surface the contract reads need.
type edgeQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

const consumerEdgesSQL = `
	SELECT d.depends_on_task_id, p.repository_id, d.contract_ref
	  FROM plan_task_dependencies d
	  JOIN plan_tasks p ON p.id = d.depends_on_task_id
	 WHERE d.task_id = $1 AND d.kind = 'contract' AND p.repository_id IS NOT NULL
	 ORDER BY d.depends_on_task_id`

// loadConsumerEdges returns the contract edges on which taskID is the
// consumer.
func loadConsumerEdges(ctx context.Context, db edgeQuerier, taskID uuid.UUID) ([]consumerEdge, error) {
	rows, err := db.Query(ctx, consumerEdgesSQL, taskID)
	if isMissingContractSchema(err) {
		// A database before migration 056: there can be no contract edge.
		// Every other read error stays fail-closed.
		warnMissingContractSchema(err)
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read contract edges of subtask %s: %w", taskID, err)
	}
	defer rows.Close()
	var out []consumerEdge
	for rows.Next() {
		var e consumerEdge
		var raw []byte
		if err := rows.Scan(&e.ProducerTaskID, &e.ProducerRepoID, &raw); err != nil {
			return nil, fmt.Errorf("scan contract edge: %w", err)
		}
		if err := json.Unmarshal(raw, &e.Refs); err != nil {
			return nil, fmt.Errorf("contract edge %s -> %s: contract_ref is not a JSON array of contracts: %w", taskID, e.ProducerTaskID, err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read contract edges of subtask %s: %w", taskID, err)
	}
	return out, nil
}

// isMissingContractSchema reports whether err is PostgreSQL's undefined
// column (42703) or undefined table (42P01): the contract columns of
// migration 056 are not there yet.
func isMissingContractSchema(err error) bool {
	var pgErr *pgconn.PgError
	return err != nil && errors.As(err, &pgErr) && (pgErr.Code == "42703" || pgErr.Code == "42P01")
}

var missingSchemaOnce sync.Once

// warnMissingContractSchema logs, loudly and once per process, that
// contract checks are off because migration 056 is not applied.
func warnMissingContractSchema(err error) {
	missingSchemaOnce.Do(func() {
		log.Printf("pr-coordinator: WARNING contract checks are OFF: plan_task_dependencies has no contract columns (%v) — apply migration 056 (restart status-orchestrator); PRs open without contract checks until then", err)
	})
}

// contractsStrict reads products.contracts_strict. A read failure is
// treated as strict: it can only make the check stricter.
func contractsStrict(ctx context.Context, db edgeQuerier, productID uuid.UUID) bool {
	if productID == uuid.Nil {
		return false
	}
	var strict bool
	if err := db.QueryRow(ctx, `SELECT contracts_strict FROM products WHERE id = $1`, productID).Scan(&strict); err != nil {
		log.Printf("pr-coordinator: product %s: read contracts_strict: %v — treating unchecked contracts as blocking", productID, err)
		return true
	}
	return strict
}

// orderProducersFirst reorders a parent's children so that every producer
// of a contract edge comes before its consumers (stable otherwise): the
// producer's PR then exists when the consumer's is opened and linked. A read
// failure keeps the original order (the check itself still fails closed).
func orderProducersFirst(ctx context.Context, db edgeQuerier, children []*models.WorkItem) []*models.WorkItem {
	if db == nil || len(children) < 2 {
		return children
	}
	ids := make([]uuid.UUID, 0, len(children))
	for _, c := range children {
		ids = append(ids, c.ID)
	}
	rows, err := db.Query(ctx, `SELECT task_id, depends_on_task_id FROM plan_task_dependencies WHERE kind = 'contract' AND task_id = ANY($1)`, ids)
	if isMissingContractSchema(err) {
		warnMissingContractSchema(err)
		return children
	}
	if err != nil {
		log.Printf("pr-coordinator: read contract edges for PR order: %v", err)
		return children
	}
	producersOf := map[uuid.UUID][]uuid.UUID{}
	for rows.Next() {
		var c, p uuid.UUID
		if rows.Scan(&c, &p) == nil {
			producersOf[c] = append(producersOf[c], p)
		}
	}
	rows.Close()
	return orderByProducers(children, producersOf)
}

// orderByProducers is the ordering itself: a stable depth-first order in
// which every producer precedes its consumers.
func orderByProducers(children []*models.WorkItem, producersOf map[uuid.UUID][]uuid.UUID) []*models.WorkItem {
	if len(producersOf) == 0 {
		return children
	}
	index := map[uuid.UUID]int{}
	for i, c := range children {
		index[c.ID] = i
	}
	out := make([]*models.WorkItem, 0, len(children))
	state := map[uuid.UUID]int{} // 1 visiting, 2 done
	var visit func(i int)
	visit = func(i int) {
		id := children[i].ID
		if state[id] != 0 {
			return
		}
		state[id] = 1
		for _, p := range producersOf[id] {
			if j, ok := index[p]; ok {
				visit(j)
			}
		}
		state[id] = 2
		out = append(out, children[i])
	}
	for i := range children {
		visit(i)
	}
	return out
}

// treeMaterializer extracts commits of a repository into temp directories,
// reusing the drift checker's git runner and scratch-repository approach.
type treeMaterializer struct {
	driftChecker
}

// prContractChecker is the production materialiser (real git, os temp dir).
var prContractChecker = &treeMaterializer{}

// repoTrees is one repository's materialised commits.
type repoTrees struct {
	dirs    map[string]string // rev -> directory
	cleanup func()
}

// materialize extracts each of revs (commit ids; "" skipped) of the
// repository at clonePath into its own directory. A rev missing from the
// clone is fetched from origin: branch (refs/heads/<branch>) for the head,
// baseRef for a pin. head may be "" — the branch's tip is then used, and is
// returned under the key "".
func (m *treeMaterializer) materialize(ctx context.Context, clonePath, branch, baseRef string, revs ...string) (*repoTrees, error) {
	if clonePath == "" {
		return nil, errors.New("repository has no clone path")
	}
	root, err := os.MkdirTemp(m.TempDir, "ae-contracts-")
	if err != nil {
		return nil, fmt.Errorf("scratch directory: %w", err)
	}
	trees := &repoTrees{dirs: map[string]string{}, cleanup: func() { os.RemoveAll(root) }}
	fail := func(err error) (*repoTrees, error) {
		trees.cleanup()
		return nil, err
	}
	origin, err := m.git(ctx, "-C", clonePath, "config", "--get", "remote.origin.url")
	origin = strings.TrimSpace(origin)
	if err != nil || origin == "" || strings.HasPrefix(origin, "-") {
		return fail(fmt.Errorf("read the clone's origin: %v", err))
	}
	objects, err := m.git(ctx, "-C", clonePath, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return fail(fmt.Errorf("locate the clone's object store: %w", err))
	}
	scratch := filepath.Join(root, "scratch.git")
	if _, err := m.git(ctx, "init", "--bare", "--quiet", scratch); err != nil {
		return fail(fmt.Errorf("init scratch repository: %w", err))
	}
	if err := os.MkdirAll(filepath.Join(scratch, "objects", "info"), 0o755); err != nil {
		return fail(err)
	}
	alt := filepath.Join(strings.TrimSpace(objects), "objects")
	if err := os.WriteFile(filepath.Join(scratch, "objects", "info", "alternates"), []byte(filepath.ToSlash(alt)+"\n"), 0o644); err != nil {
		return fail(err)
	}
	fetched := map[string]bool{}
	fetch := func(ref, into string) error {
		if ref == "" || fetched[into] {
			return nil
		}
		fetched[into] = true
		if _, err := m.git(ctx, "-C", scratch, "fetch", "--quiet", "--no-tags", "--", origin, "+refs/heads/"+ref+":"+into); err != nil {
			return fmt.Errorf("fetch %s: %s", ref, redactOrigin(err.Error(), origin))
		}
		return nil
	}
	for i, rev := range revs {
		target := rev
		switch {
		case rev == "" && i == 0:
			// The head with no verified SHA: the branch tip.
			if err := fetch(branch, "refs/ae/head"); err != nil {
				return fail(err)
			}
			target = "refs/ae/head"
		case rev == "":
			continue
		case !m.has(ctx, scratch, rev):
			ref, into := branch, "refs/ae/head"
			if i > 0 {
				ref, into = baseRef, "refs/ae/base"
			}
			if err := fetch(ref, into); err != nil {
				return fail(err)
			}
			if !m.has(ctx, scratch, rev) {
				return fail(fmt.Errorf("commit %s is not on origin %s", snapshot.Short(rev), ref))
			}
		}
		dir := filepath.Join(root, fmt.Sprintf("tree-%d", i))
		if err := m.extract(ctx, scratch, target, dir, root); err != nil {
			return fail(err)
		}
		trees.dirs[rev] = dir
	}
	return trees, nil
}

// extract writes the tree of rev into dir through `git archive`.
func (m *treeMaterializer) extract(ctx context.Context, scratch, rev, dir, root string) error {
	archive := filepath.Join(root, "archive.tar")
	defer os.Remove(archive)
	if _, err := m.git(ctx, "-C", scratch, "archive", "--format=tar", "-o", archive, rev); err != nil {
		return fmt.Errorf("archive %s: %w", rev, err)
	}
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	return untar(f, dir)
}

// untar extracts regular files and directories; anything that would land
// outside dir, and links, are skipped.
func untar(r io.Reader, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read archive: %w", err)
		}
		name := filepath.Clean(filepath.FromSlash(h.Name))
		if name == "." || filepath.IsAbs(name) || strings.HasPrefix(name, ".."+string(filepath.Separator)) || name == ".." {
			continue
		}
		target := filepath.Join(dir, name)
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if err != nil {
				return err
			}
			_, cerr := io.Copy(out, tr)
			out.Close()
			if cerr != nil {
				return cerr
			}
		}
	}
}

// contractInput is one consumer PR's contract check.
type contractInput struct {
	WorkID, TaskID, RepoID uuid.UUID
	ProductID              uuid.UUID
	WorkTitle              string
	RepoName               string
	ClonePath              string
	HeadSHA                string
	Branch                 string
	// ProducerHead returns a producer subtask's verified head SHA ("" when
	// unknown: its branch tip is used).
	ProducerHead func(taskID uuid.UUID) string
	// OpenedPRs are the PRs opened earlier in this run, by subtask.
	OpenedPRs map[uuid.UUID]openedPR
	// Shipped are the subtasks whose PR was opened or gated earlier in this
	// run. A consumer whose producer is not among them is blocked: it must
	// not ship without the producer change it depends on.
	Shipped map[uuid.UUID]bool
}

// openedPR is a PR pr-coordinator opened in this run.
type openedPR struct {
	RepoID   string
	RepoName string
	URL      string
	Number   int
}

// contractDecision is what the PR path does after the check.
type contractDecision struct {
	// Checked is pr.ready.contracts_checked; nil when the subtask consumes
	// no contract.
	Checked []contractChecked
	// DescriptionSection is appended to the PR description ("" when none).
	DescriptionSection string
	// Producers are the producer PRs opened in this run, to link.
	Producers []openedPR
	// Stop is true when the parent was blocked: open no PR for this or any
	// later repository.
	Stop bool
}

// repoRow is a repositories row's name, clone path and base branch.
func repoRow(ctx context.Context, db edgeQuerier, id uuid.UUID) (name, clonePath, baseBranch string, err error) {
	err = db.QueryRow(ctx, `SELECT name, clone_path, COALESCE(NULLIF(base_branch, ''), 'main') FROM repositories WHERE id = $1`, id).Scan(&name, &clonePath, &baseBranch)
	return name, clonePath, baseBranch, err
}

// applyContractChecks runs the checks for one consumer PR and acts on them.
// db is the pool (reads, and the edge's contract_check on success); guarded
// is the delivery's guarded beginner (the block).
func applyContractChecks(ctx context.Context, m *treeMaterializer, db *pgxpool.Pool, guarded workgraph.TxBeginner, prompts *interactions.Publisher, in contractInput) contractDecision {
	if db == nil {
		return contractDecision{}
	}
	edges, err := loadConsumerEdges(ctx, db, in.TaskID)
	if err != nil {
		return blockContracts(ctx, guarded, prompts, in, nil, fmt.Sprintf("contract check could not run for %s: %v", in.RepoName, err))
	}
	if len(edges) == 0 {
		return contractDecision{}
	}
	strict := contractsStrict(ctx, db, in.ProductID)

	consumer, err := m.materialize(ctx, in.ClonePath, in.Branch, "", in.HeadSHA)
	if err != nil {
		return blockContracts(ctx, guarded, prompts, in, nil, unrunnableContractReason(in.RepoName, in.RepoName, err))
	}
	defer consumer.cleanup()
	consumerSide := contracts.Side{Head: consumer.dirs[in.HeadSHA]}

	var checked []contractChecked
	var producers []openedPR
	var missing []string
	for _, e := range edges {
		pName, pClone, pBase, rerr := repoRow(ctx, db, e.ProducerRepoID)
		if rerr != nil {
			return blockContracts(ctx, guarded, prompts, in, checked, unrunnableContractReason(in.RepoName, e.ProducerRepoID.String(), fmt.Errorf("read producer repository: %w", rerr)))
		}
		head := ""
		if in.ProducerHead != nil {
			head = in.ProducerHead(e.ProducerTaskID)
		}
		pin := pinnedBaseSHA(ctx, db, in.WorkID, e.ProducerRepoID)
		producer, merr := m.materialize(ctx, pClone, agentBranchFor(in.WorkID, e.ProducerTaskID), pBase, head, pin)
		if merr != nil {
			return blockContracts(ctx, guarded, prompts, in, checked, unrunnableContractReason(in.RepoName, pName, merr))
		}
		side := contracts.Side{Head: producer.dirs[head]}
		if pin != "" {
			side.Base = producer.dirs[pin]
		}
		pr, opened := in.OpenedPRs[e.ProducerTaskID]
		if opened {
			producers = append(producers, pr)
		}
		if !in.Shipped[e.ProducerTaskID] {
			missing = append(missing, fmt.Sprintf("producer subtask %s (repository %s) got no pull request in this run", e.ProducerTaskID, pName))
		}
		for _, ref := range e.Refs {
			res := contracts.Check(ctx, ref, side, consumerSide)
			cc := contractChecked{Result: res, ProducerTaskID: e.ProducerTaskID.String(), ProducerRepo: pName}
			if opened {
				cc.ProducerPRURL = pr.URL
			}
			checked = append(checked, cc)
			log.Printf("pr-coordinator: work_id=%s consumer=%s %s", in.WorkID, in.RepoName, res.Summary())
		}
		producer.cleanup()
	}

	var mismatches, uncheckedBlocking []contractChecked
	for _, c := range checked {
		switch {
		case c.Status == contracts.StatusMismatch:
			mismatches = append(mismatches, c)
		case c.Status == contracts.StatusUnchecked && strict:
			uncheckedBlocking = append(uncheckedBlocking, c)
		}
	}
	if len(mismatches) > 0 || len(uncheckedBlocking) > 0 || len(missing) > 0 {
		return blockContracts(ctx, guarded, prompts, in, checked, contractBlockReason(in.RepoName, mismatches, uncheckedBlocking, missing))
	}
	if err := recordContractChecks(ctx, db, in.TaskID, checked); err != nil {
		log.Printf("pr-coordinator: work_id=%s: record contract checks of %s: %v", in.WorkID, in.RepoName, err)
	}
	return contractDecision{Checked: checked, DescriptionSection: contractDescription(checked), Producers: producers}
}

func agentBranchFor(workID, taskID uuid.UUID) string {
	s := taskID.String()
	return "agent/" + workID.String() + "/" + s[:8]
}

// unrunnableContractReason is the block reason when a check cannot run.
func unrunnableContractReason(consumerRepo, producerRepo string, err error) string {
	return fmt.Sprintf("Contract check could not run for the pull request of %s (producer repository %s): %s. Nothing was opened; retry once the repositories are reachable.",
		consumerRepo, producerRepo, gitremote.Redact(err.Error(), ""))
}

// contractBlockReason names every mismatch (contract, the producer's
// change, the consumer's file:line) and every unchecked contract that
// blocks under contracts_strict.
func contractBlockReason(consumerRepo string, mismatches, unchecked []contractChecked, missingProducers []string) string {
	var parts []string
	for _, m := range missingProducers {
		parts = append(parts, m+" — the consumer depends on its contract change and does not ship without it")
	}
	for _, m := range mismatches {
		parts = append(parts, fmt.Sprintf("[%s] %s", m.ProducerRepo, m.Summary()))
	}
	for _, u := range unchecked {
		parts = append(parts, fmt.Sprintf("[%s] %s — blocking because the product requires every contract to be checked (contracts_strict)", u.ProducerRepo, u.Summary()))
	}
	headline := "Contract mismatch"
	if len(mismatches) == 0 && len(unchecked) == 0 {
		headline = "Contract producer missing"
	}
	return fmt.Sprintf("%s: the pull request of %s was not opened. %s. Make the producer change compatible (and give it a pull request) or update the consumer, then retry.",
		headline, consumerRepo, strings.Join(parts, "; "))
}

// contractDescription is the PR description's contract section.
func contractDescription(checked []contractChecked) string {
	if len(checked) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Cross-repository contracts\n\n")
	for _, c := range checked {
		fmt.Fprintf(&b, "- `%s` %s — **%s**", c.Contract.Type, c.Contract.Name, c.Status)
		if c.ProducerRepo != "" {
			fmt.Fprintf(&b, " (producer: %s", c.ProducerRepo)
			if c.ProducerPRURL != "" {
				fmt.Fprintf(&b, ", paired PR: %s", c.ProducerPRURL)
			}
			b.WriteString(")")
		}
		if len(c.Details) > 0 {
			fmt.Fprintf(&b, ": %s", strings.Join(c.Details, "; "))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// contractCheckRecord is plan_task_dependencies.contract_check.
type contractCheckRecord struct {
	Status    contracts.Status   `json:"status"`
	CheckedAt time.Time          `json:"checked_at"`
	Results   []contracts.Result `json:"results"`
}

// overallStatus is the worst status of results: mismatch, unchecked, ok.
func overallStatus(results []contracts.Result) contracts.Status {
	s := contracts.StatusOK
	for _, r := range results {
		switch r.Status {
		case contracts.StatusMismatch:
			return contracts.StatusMismatch
		case contracts.StatusUnchecked:
			s = contracts.StatusUnchecked
		}
	}
	return s
}

// contractCheckUpdates groups results by producer task: one record per
// edge row.
func contractCheckUpdates(checked []contractChecked) (map[string]string, error) {
	byProducer := map[string][]contracts.Result{}
	for _, c := range checked {
		byProducer[c.ProducerTaskID] = append(byProducer[c.ProducerTaskID], c.Result)
	}
	out := map[string]string{}
	for p, rs := range byProducer {
		b, err := json.Marshal(contractCheckRecord{Status: overallStatus(rs), CheckedAt: time.Now().UTC(), Results: rs})
		if err != nil {
			return nil, err
		}
		out[p] = string(b)
	}
	return out, nil
}

const updateContractCheckSQL = `
	UPDATE plan_task_dependencies SET contract_check = $3::jsonb
	 WHERE task_id = $1 AND depends_on_task_id = $2 AND kind = 'contract'`

func recordContractChecksTx(ctx context.Context, tx pgx.Tx, taskID uuid.UUID, checked []contractChecked) error {
	updates, err := contractCheckUpdates(checked)
	if err != nil {
		return err
	}
	keys := make([]string, 0, len(updates))
	for k := range updates {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, p := range keys {
		if _, err := tx.Exec(ctx, updateContractCheckSQL, taskID, p, updates[p]); err != nil {
			return fmt.Errorf("record contract check %s -> %s: %w", taskID, p, err)
		}
	}
	return nil
}

// recordContractChecks records a passing check in its own transaction on
// the pool (like a drift event with no PR transaction to ride in): it never
// takes the delivery's guard row ahead of the PR's own write.
func recordContractChecks(ctx context.Context, db workgraph.TxBeginner, taskID uuid.UUID, checked []contractChecked) error {
	return workgraph.WithTx(ctx, db, func(tx pgx.Tx) error {
		return recordContractChecksTx(ctx, tx, taskID, checked)
	})
}

// blockContracts blocks the parent with reason, records the results checked
// so far on the edges, and raises the escalation prompt, in one guarded
// transaction.
func blockContracts(ctx context.Context, guarded workgraph.TxBeginner, prompts *interactions.Publisher, in contractInput, checked []contractChecked, reason string) contractDecision {
	log.Printf("pr-coordinator: work_id=%s BLOCKED: %s", in.WorkID, reason)
	prompt := contractPrompt(in, reason, checked)
	err := workgraph.WithTx(ctx, guarded, func(tx pgx.Tx) error {
		if err := guards.BlockWorkItemTx(ctx, tx, in.WorkID, in.WorkID, agentID, reason); err != nil {
			return err
		}
		if len(checked) > 0 {
			if err := recordContractChecksTx(ctx, tx, in.TaskID, checked); err != nil {
				return err
			}
		}
		if prompt == nil {
			return nil
		}
		p := prompts
		if p == nil {
			p = interactions.NewPublisher(nil)
		}
		return p.PublishTx(ctx, tx, in.WorkID, prompt)
	})
	if err != nil {
		log.Printf("pr-coordinator: ERROR could not record the contract block of work_id=%s: %v", in.WorkID, err)
	}
	return contractDecision{Stop: true}
}

// contractPrompt is the escalation prompt raised with a contract block; nil
// when the work item has no product.
func contractPrompt(in contractInput, reason string, checked []contractChecked) *interactions.Prompt {
	if in.ProductID == uuid.Nil {
		return nil
	}
	return &interactions.Prompt{
		ID:             uuid.New(),
		WorkItemID:     in.WorkID,
		ProductID:      in.ProductID,
		AskingAgent:    agentID,
		Trigger:        interactions.TriggerBlocked,
		PromptType:     interactions.PromptTypeEscalation,
		Headline:       fmt.Sprintf("Blocked: contract check for %s", in.WorkTitle),
		Question:       reason,
		ContextLink:    fmt.Sprintf("/work-items/%s", in.WorkID),
		ContextPayload: map[string]any{"contracts_checked": checked, "consumer_repository": in.RepoName},
		Actions: []interactions.Action{
			{ID: "retry", Label: "Retry", Semantic: interactions.SemanticRetry},
			{ID: "cancel", Label: "Cancel", Semantic: interactions.SemanticCancel},
		},
		State:       interactions.StatePending,
		RequestedAt: time.Now().UTC(),
		PromptedVia: []string{"dashboard"},
	}
}

// pairedPRLine is what the consumer PR body says about its producers.
func pairedPRLine(producers []openedPR) string {
	if len(producers) == 0 {
		return ""
	}
	parts := make([]string, 0, len(producers))
	for _, p := range producers {
		parts = append(parts, fmt.Sprintf("%s (%s)", p.URL, p.RepoName))
	}
	return "**Paired producer PR(s):** " + strings.Join(parts, ", ") + " — merge the producer first."
}

// linkPRRequest matches github-service POST /pr/link.
type linkPRRequest struct {
	RepositoryID string `json:"repository_id"`
	PRNumber     int    `json:"pr_number"`
	Append       string `json:"append"`
}

// linkProducerPRs edits each producer PR's description to link the
// consumer's PR (github-service appends the line once). Failures are
// logged: the consumer's body already links the producer, and nothing in
// the database depends on the edit.
func linkProducerPRs(ctx context.Context, githubServiceURL string, producers []openedPR, consumerURL, consumerRepo string) {
	for _, p := range producers {
		line := fmt.Sprintf("**Paired consumer PR:** %s (%s) — it consumes a contract this PR changes; merge this one first.", consumerURL, consumerRepo)
		body, _ := json.Marshal(linkPRRequest{RepositoryID: p.RepoID, PRNumber: p.Number, Append: line})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, githubServiceURL+"/pr/link", bytes.NewReader(body))
		if err != nil {
			log.Printf("pr-coordinator: link producer PR %s: %v", p.URL, err)
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
		if err != nil {
			log.Printf("pr-coordinator: link producer PR %s to %s: %v", p.URL, consumerURL, err)
			continue
		}
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if resp.StatusCode >= 400 {
			log.Printf("pr-coordinator: link producer PR %s to %s: github-service returned %d: %s", p.URL, consumerURL, resp.StatusCode, msg)
			continue
		}
		log.Printf("pr-coordinator: linked producer PR %s to consumer PR %s", p.URL, consumerURL)
	}
}
