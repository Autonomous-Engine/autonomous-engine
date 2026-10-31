package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/contracts"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// M2-06 PR-time contract checks end to end through the guarded handler,
// against a real PostgreSQL and real git: a producer repository (api) and a
// consumer repository (web), one passed subtask each, a contract edge from
// the consumer to the producer, both pinned at their bases, both agent
// branches pushed to origin. Env-gated (TEST_POSTGRES_DSN) and git-gated.

type contractScenario struct {
	productID, workID          uuid.UUID
	producerTask, consumerTask uuid.UUID
	producerRepo, consumerRepo uuid.UUID
	producerGit, consumerGit   *driftRepo
}

func seedContractScenario(t *testing.T, pool *pgxpool.Pool, producerHead, consumerHead map[string]string) contractScenario {
	t.Helper()
	ctx := context.Background()
	s := contractScenario{producerTask: uuid.New(), consumerTask: uuid.New(), producerRepo: uuid.New(), consumerRepo: uuid.New()}
	s.producerGit = newContractRepo(t, map[string]string{"api/openapi.json": usersSpec("id", "name", "email")})
	s.consumerGit = newContractRepo(t, map[string]string{"README.md": "web\n"})
	s.productID, s.workID = seedWorkItem(t, pool)
	s.producerGit.pushBranchFiles(agentBranchFor(s.workID, s.producerTask), producerHead)
	s.consumerGit.pushBranchFiles(agentBranchFor(s.workID, s.consumerTask), consumerHead)

	ref, _ := json.Marshal([]contracts.Ref{{Type: "rest", Name: "GET /api/users/{id}", ProducerRepoID: s.producerRepo.String(),
		ConsumerRepoID: s.consumerRepo.String(), Source: contracts.SourceCodex, ProducerFile: "api/openapi.json", ConsumerFile: "src/user.ts"}})
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`UPDATE work_items SET pr_gate_mode = 'manual' WHERE id = $1`, []any{s.workID}},
		{`INSERT INTO repositories (id, product_id, name, github_url, clone_path, base_branch) VALUES ($1, $2, $3, 'https://example.invalid/api.git', $4, 'main')`,
			[]any{s.producerRepo, s.productID, "m206-api-" + s.producerRepo.String()[:8], s.producerGit.clone}},
		{`INSERT INTO repositories (id, product_id, name, github_url, clone_path, base_branch) VALUES ($1, $2, $3, 'https://example.invalid/web.git', $4, 'main')`,
			[]any{s.consumerRepo, s.productID, "m206-web-" + s.consumerRepo.String()[:8], s.consumerGit.clone}},
		{`INSERT INTO work_items (id, type, state, title, product_id, parent_id, repository_id) VALUES ($1, 'subtask', 'passed', 'api subtask', $2, $3, $4)`,
			[]any{s.producerTask, s.productID, s.workID, s.producerRepo}},
		{`INSERT INTO work_items (id, type, state, title, product_id, parent_id, repository_id) VALUES ($1, 'subtask', 'passed', 'web subtask', $2, $3, $4)`,
			[]any{s.consumerTask, s.productID, s.workID, s.consumerRepo}},
		{`INSERT INTO plan_tasks (id, parent_work_id, title, repository_id, status, message) VALUES ($1, $2, 'api subtask', $3, 'completed', '{}')`,
			[]any{s.producerTask, s.workID, s.producerRepo}},
		{`INSERT INTO plan_tasks (id, parent_work_id, title, repository_id, status, message) VALUES ($1, $2, 'web subtask', $3, 'released', '{}')`,
			[]any{s.consumerTask, s.workID, s.consumerRepo}},
		{`INSERT INTO plan_task_dependencies (task_id, depends_on_task_id, kind, contract_ref) VALUES ($1, $2, 'contract', $3::jsonb)`,
			[]any{s.consumerTask, s.producerTask, string(ref)}},
		{`INSERT INTO work_item_base_shas (work_item_id, repository_id, base_ref, base_sha) VALUES ($1, $2, 'main', $3)`,
			[]any{s.workID, s.producerRepo, s.producerGit.pin}},
		{`INSERT INTO work_item_base_shas (work_item_id, repository_id, base_ref, base_sha) VALUES ($1, $2, 'main', $3)`,
			[]any{s.workID, s.consumerRepo, s.consumerGit.pin}},
	} {
		if _, err := pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("seed (%s): %v", q.sql, err)
		}
	}
	// Registered after seedWorkItem's cleanup, so it runs first.
	t.Cleanup(func() {
		c := context.Background()
		for _, q := range []string{
			`DELETE FROM base_sha_events WHERE work_item_id = $1`,
			`DELETE FROM work_item_base_shas WHERE work_item_id = $1`,
			`DELETE FROM agent_assignments WHERE work_item_id = $1`,
			`DELETE FROM work_items WHERE parent_id = $1`,
		} {
			_, _ = pool.Exec(c, q, s.workID)
		}
		_, _ = pool.Exec(c, `DELETE FROM repositories WHERE id = ANY($1)`, []uuid.UUID{s.producerRepo, s.consumerRepo})
	})
	return s
}

func (s contractScenario) edgeCheck(t *testing.T, pool *pgxpool.Pool) map[string]any {
	t.Helper()
	var raw []byte
	if err := pool.QueryRow(context.Background(), `SELECT contract_check FROM plan_task_dependencies WHERE task_id = $1 AND depends_on_task_id = $2`,
		s.consumerTask, s.producerTask).Scan(&raw); err != nil {
		t.Fatalf("contract_check: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("contract_check %s: %v", raw, err)
	}
	return out
}

// A seeded REST mismatch (the producer drops the email field, the consumer
// still reads it) blocks the parent with the mismatch in the reason; no PR
// is gated for the consumer.
func TestIntegration_Contracts_RESTMismatchBlocksTheParent(t *testing.T) {
	pool := integrationPool(t)
	s := seedContractScenario(t, pool,
		map[string]string{"api/openapi.json": usersSpec("id", "name")},
		map[string]string{"src/user.ts": usersClient})

	s.completeBoth(t, pool)

	var state, reason string
	if err := pool.QueryRow(context.Background(), `SELECT state FROM work_items WHERE id = $1`, s.workID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "blocked" {
		t.Fatalf("parent state = %s, want blocked", state)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT reason FROM work_item_transitions WHERE work_item_id = $1 AND to_state = 'blocked' ORDER BY "timestamp" DESC LIMIT 1`, s.workID).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Contract mismatch: the pull request of m206-web-",
		`contract rest "GET /api/users/{id}": the producer removed field email from the GET /api/users/{id} response (api/openapi.json)`,
		"but the consumer still uses it at src/user.ts:4: return body.email",
	} {
		if !strings.Contains(reason, want) {
			t.Errorf("block reason lacks %q:\n%s", want, reason)
		}
	}
	if got := s.edgeCheck(t, pool); got["status"] != "mismatch" {
		t.Errorf("edge contract_check = %v, want status mismatch", got)
	}
	if n := count(t, pool, `SELECT count(*) FROM pending_pr_changes WHERE work_item_id = $1 AND repository_id = $2`, s.workID, s.consumerRepo); n != 0 {
		t.Errorf("consumer pending PR rows = %d, want none for a blocked parent", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM user_interactions WHERE work_item_id = $1 AND trigger = 'blocked' AND state = 'pending' AND asking_agent = 'pr-coordinator'`, s.workID); n != 1 {
		t.Errorf("escalation prompts = %d, want 1", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND topic = 'work.blocked'`, s.workID); n != 1 {
		t.Errorf("work.blocked events = %d, want 1", n)
	}
}

// A compatible producer change (a field added) passes: the consumer's PR is
// gated with contracts_checked and the section in its description.
func TestIntegration_Contracts_CompatibleChangeRecordsTheCheck(t *testing.T) {
	pool := integrationPool(t)
	s := seedContractScenario(t, pool,
		map[string]string{"api/openapi.json": usersSpec("id", "name", "email", "avatar")},
		map[string]string{"src/user.ts": usersClient})

	s.completeBoth(t, pool)

	var desc string
	if err := pool.QueryRow(context.Background(), `SELECT pr_description FROM pending_pr_changes WHERE work_item_id = $1 AND repository_id = $2`,
		s.workID, s.consumerRepo).Scan(&desc); err != nil {
		t.Fatalf("consumer pending PR row: %v", err)
	}
	if !strings.Contains(desc, "## Cross-repository contracts") || !strings.Contains(desc, "- `rest` GET /api/users/{id} — **ok** (producer: m206-api-") {
		t.Errorf("PR description lacks the contract section:\n%s", desc)
	}
	var payload []byte
	if err := pool.QueryRow(context.Background(), `SELECT payload FROM outbox_events WHERE aggregate_id = $1 AND topic = 'pr.gated' AND payload->>'repository_id' = $2`,
		s.workID, s.consumerRepo.String()).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var msg prReadyMessage
	if err := json.Unmarshal(payload, &msg); err != nil {
		t.Fatal(err)
	}
	if len(msg.ContractsChecked) != 1 || msg.ContractsChecked[0].Status != contracts.StatusOK || msg.ContractsChecked[0].ProducerTaskID != s.producerTask.String() {
		t.Fatalf("pr.gated contracts_checked = %+v", msg.ContractsChecked)
	}
	if got := s.edgeCheck(t, pool); got["status"] != "ok" {
		t.Errorf("edge contract_check = %v, want status ok", got)
	}
	var state string
	_ = pool.QueryRow(context.Background(), `SELECT state FROM work_items WHERE id = $1`, s.workID).Scan(&state)
	if state == "blocked" {
		t.Fatal("a compatible change must not block")
	}
}

// completeBoth delivers the producer's completion while the consumer is
// still implementing (the handler waits), then the consumer's: one handler,
// so the producer's verified outcome is in its session and its PR is gated
// first.
func (s contractScenario) completeBoth(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	h := guardedCoordinator(pool)
	if _, err := pool.Exec(ctx, `UPDATE work_items SET state = 'implementation' WHERE id = $1`, s.consumerTask); err != nil {
		t.Fatal(err)
	}
	if err := h(ctx, completionMessage(t, pool, s.workID, s.producerTask)); err != nil {
		t.Fatal(err)
	}
	if n := count(t, pool, `SELECT count(*) FROM pending_pr_changes WHERE work_item_id = $1`, s.workID); n != 0 {
		t.Fatalf("pending PR rows before the consumer completed = %d, want 0", n)
	}
	if _, err := pool.Exec(ctx, `UPDATE work_items SET state = 'passed' WHERE id = $1`, s.consumerTask); err != nil {
		t.Fatal(err)
	}
	if err := h(ctx, completionMessage(t, pool, s.workID, s.consumerTask)); err != nil {
		t.Fatal(err)
	}
	if n := count(t, pool, `SELECT count(*) FROM pending_pr_changes WHERE work_item_id = $1 AND repository_id = $2`, s.workID, s.producerRepo); n != 1 {
		t.Fatalf("producer pending PR rows = %d, want 1 (gated before the consumer)", n)
	}
}

// A consumer whose producer got no PR (no completion seen, no stored
// evidence: the producer's outcome is a block) is blocked naming the missing
// producer PR, even though its contract is compatible — it does not ship
// alone.
func TestIntegration_Contracts_ConsumerWithoutProducerPRIsBlocked(t *testing.T) {
	pool := integrationPool(t)
	s := seedContractScenario(t, pool,
		map[string]string{"api/openapi.json": usersSpec("id", "name", "email", "avatar")},
		map[string]string{"src/user.ts": usersClient})
	// R14: a passed subtask counts as done for the PR only once its
	// verification is recorded. The producer's is not_run (its evidence did
	// not release — here there is none stored), so it gets no PR without
	// refusing its siblings (all-or-nothing only counts subtasks whose
	// evidence released), and the consumer meets the missing producer.
	if _, err := pool.Exec(context.Background(), `INSERT INTO verification_results (id, work_item_id, task_id, decision, reason) VALUES ($1, $2, $3, 'not_run', 'producer not releasable')`,
		uuid.New(), s.workID, s.producerTask); err != nil {
		t.Fatal(err)
	}

	if err := guardedCoordinator(pool)(context.Background(), completionMessage(t, pool, s.workID, s.consumerTask)); err != nil {
		t.Fatal(err)
	}
	var state, reason string
	if err := pool.QueryRow(context.Background(), `SELECT state FROM work_items WHERE id = $1`, s.workID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT reason FROM work_item_transitions WHERE work_item_id = $1 AND to_state = 'blocked' ORDER BY "timestamp" DESC LIMIT 1`, s.workID).Scan(&reason); err != nil {
		t.Fatalf("state %s, no block reason: %v", state, err)
	}
	want := "producer subtask " + s.producerTask.String() + " (repository m206-api-"
	if state != "blocked" || !strings.HasPrefix(reason, "Contract producer missing: ") || !strings.Contains(reason, want) || !strings.Contains(reason, "got no pull request in this run") {
		t.Fatalf("state=%s reason=%q", state, reason)
	}
	if n := count(t, pool, `SELECT count(*) FROM pending_pr_changes WHERE work_item_id = $1`, s.workID); n != 0 {
		t.Errorf("pending PR rows = %d, want none", n)
	}
}
