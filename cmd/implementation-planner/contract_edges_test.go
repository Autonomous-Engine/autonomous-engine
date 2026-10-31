package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/contracts"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/plan"
	"github.com/google/uuid"
)

// contractSubtask is a phase-1 subtask in repository repo with files.
func contractSubtask(repo uuid.UUID, name string, files ...string) plannedSubtask {
	id := uuid.New()
	r := repo
	return plannedSubtask{
		repoName: name,
		item:     &models.WorkItem{ID: id, RepositoryID: &r, Title: "Implement in " + name, Description: "steps"},
		msg:      implementationTaskMessage{TaskID: id.String(), Title: "Implement in " + name, Description: "steps", FilesToChange: files},
	}
}

func TestDeriveContractEdges_CodexRowsMatchBothSidesFiles(t *testing.T) {
	api, web := uuid.New(), uuid.New()
	planned := []plannedSubtask{
		contractSubtask(web, "web", "src/client.ts"),
		contractSubtask(api, "api", "/repo-cache/api/internal/http/routes.go"),
	}
	rows := []codexEdgeRow{
		// Both sides' files are the plan's: an edge (clone-path prefix tolerated).
		{contractType: "rest_openapi", contractName: "GET /api/users", producerRepo: api, producerFile: "internal/http/routes.go", producerLine: 12, consumerRepo: web, consumerFile: "src/client.ts", consumerLine: 3},
		// The consumer file is not in the consumer subtask: no edge.
		{contractType: "events", contractName: "user.created", producerRepo: api, producerFile: "internal/http/routes.go", consumerRepo: web, consumerFile: "src/other.ts"},
		// A repository with no subtask in this plan: no edge.
		{contractType: "rest", contractName: "GET /x", producerRepo: uuid.New(), producerFile: "a.go", consumerRepo: web, consumerFile: "src/client.ts"},
	}
	edges := deriveContractEdges(planned, rows, nil)
	if len(edges) != 1 || edges[0].consumer != 0 || edges[0].producer != 1 || len(edges[0].refs) != 1 {
		t.Fatalf("edges = %+v", edges)
	}
	ref := edges[0].refs[0]
	if ref.Type != contracts.TypeREST || ref.Name != "GET /api/users" || ref.Source != contracts.SourceCodex ||
		ref.ProducerRepoID != api.String() || ref.ConsumerRepoID != web.String() || ref.ProducerLine != 12 {
		t.Fatalf("ref = %+v", ref)
	}
}

func TestDeriveContractEdges_CriteriaContractsByRepositoryName(t *testing.T) {
	api, web := uuid.New(), uuid.New()
	planned := []plannedSubtask{contractSubtask(api, "api"), contractSubtask(web, "web")}
	criteria := &plan.BriefCriteria{Criteria: []plan.Criterion{{ID: "AC-2", Contracts: []plan.ContractRef{
		{Type: "event", Name: "user.created", ProducerRepository: "API", ConsumerRepository: web.String()},
		{Type: "rest", Name: "GET /one-sided", ProducerRepository: "api", ConsumerRepository: "billing"},
		{Type: "rest", Name: "GET /same", ProducerRepository: "api", ConsumerRepository: "api"},
	}}}}
	edges := deriveContractEdges(planned, nil, criteria)
	if len(edges) != 1 || edges[0].consumer != 1 || edges[0].producer != 0 {
		t.Fatalf("edges = %+v", edges)
	}
	if r := edges[0].refs[0]; r.Source != contracts.SourceCriteria || r.CriterionID != "AC-2" || r.Type != contracts.TypeEvent {
		t.Fatalf("ref = %+v", r)
	}
}

// The consumer depends on the producer (held), both messages carry the
// contract, both descriptions say so, and the edge keeps its contracts for
// the plan_task_dependencies row.
func TestApplyContractEdges_ConsumerWaitsForProducer(t *testing.T) {
	api, web := uuid.New(), uuid.New()
	planned := []plannedSubtask{contractSubtask(web, "web", "src/client.ts"), contractSubtask(api, "api", "routes.go")}
	edges := deriveContractEdges(planned, []codexEdgeRow{{contractType: "rest", contractName: "GET /api/users", producerRepo: api, producerFile: "routes.go", consumerRepo: web, consumerFile: "src/client.ts"}}, nil)
	if err := applyContractEdges(planned, edges); err != nil {
		t.Fatal(err)
	}
	consumer, producer := planned[0], planned[1]
	if consumer.initialStatus() != planTaskHeld || producer.initialStatus() != planTaskReleased {
		t.Fatalf("consumer %s, producer %s; want held, released", consumer.initialStatus(), producer.initialStatus())
	}
	if len(consumer.msg.Dependencies) != 1 || consumer.msg.Dependencies[0] != producer.msg.TaskID {
		t.Fatalf("consumer dependencies = %v", consumer.msg.Dependencies)
	}
	if len(consumer.contractRefs[producer.item.ID]) != 1 {
		t.Fatalf("contract refs = %+v", consumer.contractRefs)
	}
	if len(consumer.msg.Contracts) != 1 || consumer.msg.Contracts[0].Role != roleConsumer || consumer.msg.Contracts[0].CounterpartTask != producer.msg.TaskID {
		t.Fatalf("consumer contracts = %+v", consumer.msg.Contracts)
	}
	if len(producer.msg.Contracts) != 1 || producer.msg.Contracts[0].Role != roleProducer {
		t.Fatalf("producer contracts = %+v", producer.msg.Contracts)
	}
	if !strings.Contains(consumer.item.Description, "**Consumer of rest \"GET /api/users\"**") || consumer.item.Description != consumer.msg.Description {
		t.Fatalf("consumer description:\n%s", consumer.item.Description)
	}
	if !strings.Contains(producer.msg.Description, "**Producer of rest \"GET /api/users\"**") {
		t.Fatalf("producer description:\n%s", producer.msg.Description)
	}
	b, _ := json.Marshal(consumer.msg)
	if !strings.Contains(string(b), `"contracts":[{"type":"rest","name":"GET /api/users"`) || !strings.Contains(string(b), `"role":"consumer"`) {
		t.Fatalf("message JSON: %s", b)
	}
	raw, err := contractRefJSON(consumer.contractRefs[producer.item.ID])
	if err != nil || !strings.HasPrefix(raw, `[{"type":"rest"`) {
		t.Fatalf("contract_ref = %s (%v)", raw, err)
	}
}

// A contract edge that reverses the plan's own ordering is a cycle: an
// error, and the plan is left unchanged.
func TestApplyContractEdges_RejectsACycle(t *testing.T) {
	api, web, mobile := uuid.New(), uuid.New(), uuid.New()
	planned := []plannedSubtask{contractSubtask(web, "web", "c.ts"), contractSubtask(api, "api", "p.go"), contractSubtask(mobile, "mobile", "m.ts")}
	planned[1].dependsOn = []uuid.UUID{planned[0].item.ID} // the producer waits for the consumer
	edges := deriveContractEdges(planned, []codexEdgeRow{
		{contractType: "event", contractName: "user.created", producerRepo: api, producerFile: "p.go", consumerRepo: web, consumerFile: "c.ts"},
		// Not on the cycle: must not be named in the error.
		{contractType: "rest", contractName: "GET /api/feed", producerRepo: api, producerFile: "p.go", consumerRepo: mobile, consumerFile: "m.ts"},
	}, nil)
	err := applyContractEdges(planned, edges)
	if !errors.Is(err, errContractCycle) || !strings.Contains(err.Error(), `event "user.created": web consumes from api`) {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "GET /api/feed") || strings.Contains(err.Error(), "mobile") {
		t.Fatalf("the error names an edge that is not on the cycle: %v", err)
	}
	if len(planned[0].dependsOn) != 0 || len(planned[0].msg.Contracts) != 0 || planned[0].item.Description != "steps" {
		t.Fatal("a rejected edge changed the plan")
	}
}

// A pair with an existing title dependency gets no second dependency, but
// is still a contract edge.
func TestApplyContractEdges_ExistingPlanEdgeBecomesContract(t *testing.T) {
	api, web := uuid.New(), uuid.New()
	planned := []plannedSubtask{contractSubtask(web, "web", "c.ts"), contractSubtask(api, "api", "p.go")}
	planned[0].dependsOn = []uuid.UUID{planned[1].item.ID}
	planned[0].msg.Dependencies = []string{planned[1].msg.TaskID}
	edges := deriveContractEdges(planned, []codexEdgeRow{{contractType: "sql_schemas", contractName: "users", producerRepo: api, producerFile: "p.go", consumerRepo: web, consumerFile: "c.ts"}}, nil)
	if err := applyContractEdges(planned, edges); err != nil {
		t.Fatal(err)
	}
	if len(planned[0].dependsOn) != 1 || len(planned[0].msg.Dependencies) != 1 || len(planned[0].contractRefs[planned[1].item.ID]) != 1 {
		t.Fatalf("consumer = %+v", planned[0])
	}
}

// Source check: derivation runs inside commitDecomposition's transaction,
// before any subtask row is written and before the messages are encoded.
func TestPlanner_DerivesContractEdgesInThePlanTransaction(t *testing.T) {
	src := strings.ReplaceAll(stripComments(plannerSources(t)["main.go"]), "\r\n", "\n")
	commit := src[strings.Index(src, "func commitDecomposition("):]
	commit = commit[:strings.Index(commit, "\n}\n")]
	begin := strings.Index(commit, "pool.Begin(ctx)")
	derive := strings.Index(commit, "deriveContractEdgesTx(ctx, tx, planned)")
	encode := strings.Index(commit, "encodeStoredMessage(ps.msg)")
	create := strings.Index(commit, "wg.CreateWorkItemTx(ctx, tx, ps.item)")
	if begin < 0 || derive < 0 || encode < 0 || create < 0 {
		t.Fatalf("begin=%d derive=%d encode=%d create=%d", begin, derive, encode, create)
	}
	if !(begin < derive && derive < encode && derive < create) {
		t.Errorf("derivation must follow Begin and precede encoding and the subtask rows: begin=%d derive=%d encode=%d create=%d", begin, derive, encode, create)
	}
}
