package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/contracts"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
)

// M2-06 integration: a codex contract edge between the plan's two
// repositories makes the consumer subtask a held dependent of the producer
// (kind 'contract', contract_ref recorded), the producer alone is enqueued at
// commit, and the producer's completion releases the consumer — M1-06's
// release path, reused. Env-gated (TEST_POSTGRES_DSN / TEST_DATABASE_URL).
func TestIntegration_ContractEdge_ConsumerHeldUntilProducerCompletes(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	f := newFixture(t, pool)
	wg := workgraph.New(pool)

	// A second repository of the product: the producer.
	producerRepo := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO repositories (id, product_id, name, github_url, clone_path, base_branch)
		VALUES ($1, $2, $3, 'https://example.invalid/m206-api', '/tmp/m206-api', 'main')`,
		producerRepo, f.productID, "m206-api-"+producerRepo.String()[:8]); err != nil {
		t.Fatal(err)
	}
	hash := "m206-" + uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO codex_contract_edges
		(hash, contract_type, contract_name, producer_repo_id, producer_file, producer_line, consumer_repo_id, consumer_file, consumer_line, confidence)
		VALUES ($1, 'rest_openapi', 'GET /api/users/{id}', $2, 'api/openapi.json', 4, $3, 'src/user.ts', 6, 0.90)`,
		hash, producerRepo, f.repositoryID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM codex_contract_edges WHERE hash = $1`, hash)
		_, _ = pool.Exec(c, `DELETE FROM work_items WHERE parent_id = $1`, f.parentWorkID)
		_, _ = pool.Exec(c, `DELETE FROM repositories WHERE id = $1`, producerRepo)
	})

	planned, ids := f.multiGroupPlan(2)
	consumerID, producerID := ids[0], ids[1]
	planned[0].msg.FilesToChange = []string{"src/user.ts"}
	pr := producerRepo
	planned[1].item.RepositoryID = &pr
	planned[1].msg.RepositoryID = producerRepo.String()
	planned[1].msg.FilesToChange = []string{"api/openapi.json"}

	if err := commitDecomposition(ctx, pool, wg, f.parentWorkID, nil, planned); err != nil {
		t.Fatalf("commitDecomposition: %v", err)
	}

	assertStatuses(t, pool, []uuid.UUID{consumerID, producerID}, planTaskHeld, planTaskReleased)
	if got := eventKeys(taskEvents(t, pool, f.parentWorkID)); len(got) != 1 || got[0] != producerID.String() {
		t.Fatalf("events after decomposition = %v, want only the producer %s", got, producerID)
	}

	var kind string
	var refRaw []byte
	if err := pool.QueryRow(ctx, `SELECT kind, contract_ref FROM plan_task_dependencies WHERE task_id = $1 AND depends_on_task_id = $2`,
		consumerID, producerID).Scan(&kind, &refRaw); err != nil {
		t.Fatalf("contract edge row: %v", err)
	}
	var refs []contracts.Ref
	if err := json.Unmarshal(refRaw, &refs); err != nil {
		t.Fatal(err)
	}
	if kind != "contract" || len(refs) != 1 || refs[0].Type != contracts.TypeREST || refs[0].Name != "GET /api/users/{id}" ||
		refs[0].Source != contracts.SourceCodex || refs[0].ProducerRepoID != producerRepo.String() || refs[0].ConsumerRepoID != f.repositoryID.String() {
		t.Fatalf("edge kind=%s refs=%+v", kind, refs)
	}

	// The held consumer's stored message (what its release enqueues)
	// carries the contract and the producer dependency.
	var stored []byte
	if err := pool.QueryRow(ctx, `SELECT message FROM plan_tasks WHERE id = $1`, consumerID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	var msg implementationTaskMessage
	if err := json.Unmarshal(stored, &msg); err != nil {
		t.Fatal(err)
	}
	if len(msg.Contracts) != 1 || msg.Contracts[0].Role != roleConsumer || msg.Contracts[0].CounterpartTask != producerID.String() ||
		len(msg.Dependencies) != 1 || msg.Dependencies[0] != producerID.String() {
		t.Fatalf("stored consumer message contracts=%+v dependencies=%v", msg.Contracts, msg.Dependencies)
	}

	// The producer completes: the consumer is released in that transaction.
	mustComplete(t, pool, f, producerID, true)
	assertStatuses(t, pool, []uuid.UUID{consumerID, producerID}, planTaskReleased, planTaskCompleted)
	if got := eventKeys(taskEvents(t, pool, f.parentWorkID)); len(got) != 2 || got[1] != consumerID.String() {
		t.Fatalf("events after the producer completed = %v, want producer then consumer", got)
	}
}
