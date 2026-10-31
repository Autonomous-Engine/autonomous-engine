package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/contracts"
	"github.com/autonomousengine/autonomous-engine/internal/plan"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Cross-repository contract edges (M2-06). Derived inside the plan's
// transaction (commitDecomposition), after M2-05 validation and M1-06's
// title dependencies, from two sources:
//
//   - codex_contract_edges rows whose producer file is among the producer
//     subtask's files and whose consumer file is among the consumer
//     subtask's files (both subtasks of this plan, different repositories);
//   - the plan's criteria Contracts that name a producer and a consumer
//     repository (plan.ContractRef.CrossRepository), both of which have a
//     subtask in this plan (the validator has rejected a one-sided one).
//
// Each edge makes the consumer subtask depend on the producer subtask
// (plan_task_dependencies kind = 'contract', the contracts in contract_ref),
// so M1-06 holds the consumer until the producer completes: the producer
// always releases first. Both subtasks' messages carry the contracts
// (implementationTaskMessage.Contracts) and their descriptions say so.

// errContractCycle: the contract edges contradict the plan's own ordering
// (the producer already waits for the consumer), so neither could ever be
// released.
var errContractCycle = errors.New("contract edges form a dependency cycle")

// taskContract is one contract on an implementation.tasks message: the
// contract, which side this subtask is, and the subtask on the other side.
type taskContract struct {
	contracts.Ref
	Role            string `json:"role"` // "producer" | "consumer"
	CounterpartTask string `json:"counterpart_task_id"`
	CounterpartRepo string `json:"counterpart_repository,omitempty"`
}

const (
	roleProducer = "producer"
	roleConsumer = "consumer"
)

// contractEdge is one derived edge between two planned subtasks (indices).
type contractEdge struct {
	consumer, producer int
	refs               []contracts.Ref
}

// codexEdgeRow is one codex_contract_edges row.
type codexEdgeRow struct {
	contractType, contractName string
	producerRepo, consumerRepo uuid.UUID
	producerFile, consumerFile string
	producerLine, consumerLine int
}

// codexEdgeQuerier is the transaction surface loadCodexContractEdgesTx needs.
type codexEdgeQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

const codexContractEdgesSQL = `
	SELECT contract_type, contract_name, producer_repo_id, producer_file, COALESCE(producer_line, 0),
	       consumer_repo_id, consumer_file, COALESCE(consumer_line, 0)
	  FROM codex_contract_edges
	 WHERE producer_repo_id = ANY($1) AND consumer_repo_id = ANY($1)
	   AND producer_repo_id <> consumer_repo_id
	 ORDER BY contract_name, producer_file, consumer_file, hash`

func loadCodexContractEdgesTx(ctx context.Context, q codexEdgeQuerier, repos []uuid.UUID) ([]codexEdgeRow, error) {
	if len(repos) < 2 {
		return nil, nil
	}
	rows, err := q.Query(ctx, codexContractEdgesSQL, repos)
	if err != nil {
		return nil, fmt.Errorf("read codex contract edges: %w", err)
	}
	defer rows.Close()
	var out []codexEdgeRow
	for rows.Next() {
		var r codexEdgeRow
		if err := rows.Scan(&r.contractType, &r.contractName, &r.producerRepo, &r.producerFile, &r.producerLine,
			&r.consumerRepo, &r.consumerFile, &r.consumerLine); err != nil {
			return nil, fmt.Errorf("scan codex contract edge: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read codex contract edges: %w", err)
	}
	return out, nil
}

// subtaskFiles is every path the subtask names: its files_to_change and its
// spec's files (hypotheses and new files included — a contract file the plan
// intends to touch is still the plan's).
func subtaskFiles(ps plannedSubtask) []string {
	out := append([]string(nil), ps.msg.FilesToChange...)
	if ps.msg.TaskSpec != nil {
		for _, f := range ps.msg.TaskSpec.Files {
			out = append(out, f.Path)
		}
	}
	return out
}

func normalizePath(p string) string {
	p = strings.TrimSpace(strings.ReplaceAll(p, "\\", "/"))
	p = strings.TrimPrefix(p, "./")
	if p == "" {
		return ""
	}
	return path.Clean(p)
}

// sameFile matches a codex path (repository-relative) with a task path that
// may be repository-relative or carry a clone-path prefix.
func sameFile(a, b string) bool {
	a, b = normalizePath(a), normalizePath(b)
	if a == "" || b == "" {
		return false
	}
	return a == b || strings.HasSuffix(a, "/"+b) || strings.HasSuffix(b, "/"+a)
}

func namesFile(files []string, f string) bool {
	for _, x := range files {
		if sameFile(x, f) {
			return true
		}
	}
	return false
}

// repoIndex maps each planned subtask's repository (id, and name lowered)
// to its index in planned.
func plannedRepoIndex(planned []plannedSubtask) (byID map[uuid.UUID]int, byKey map[string]int) {
	byID, byKey = map[uuid.UUID]int{}, map[string]int{}
	for i, ps := range planned {
		if ps.item == nil || ps.item.RepositoryID == nil {
			continue
		}
		byID[*ps.item.RepositoryID] = i
		byKey[strings.ToLower(ps.item.RepositoryID.String())] = i
		if ps.repoName != "" {
			byKey[strings.ToLower(ps.repoName)] = i
		}
	}
	return byID, byKey
}

// deriveContractEdges is the pure half of derivation: the edges the codex
// rows and the criteria imply between planned subtasks, deduplicated per
// (consumer, producer) pair and per contract, in a stable order.
func deriveContractEdges(planned []plannedSubtask, codexRows []codexEdgeRow, criteria *plan.BriefCriteria) []contractEdge {
	byID, byKey := plannedRepoIndex(planned)
	type pair struct{ consumer, producer int }
	edges := map[pair]*contractEdge{}
	var order []pair
	seen := map[string]bool{}
	add := func(consumer, producer int, ref contracts.Ref) {
		if consumer == producer || seen[ref.Key()+fmt.Sprintf("|%d|%d", consumer, producer)] {
			return
		}
		seen[ref.Key()+fmt.Sprintf("|%d|%d", consumer, producer)] = true
		p := pair{consumer, producer}
		e, ok := edges[p]
		if !ok {
			e = &contractEdge{consumer: consumer, producer: producer}
			edges[p] = e
			order = append(order, p)
		}
		e.refs = append(e.refs, ref)
	}

	for _, r := range codexRows {
		pi, okP := byID[r.producerRepo]
		ci, okC := byID[r.consumerRepo]
		if !okP || !okC || pi == ci {
			continue
		}
		if !namesFile(subtaskFiles(planned[pi]), r.producerFile) || !namesFile(subtaskFiles(planned[ci]), r.consumerFile) {
			continue
		}
		add(ci, pi, contracts.Ref{
			Type: contracts.NormalizeType(r.contractType), Name: r.contractName,
			ProducerRepoID: r.producerRepo.String(), ConsumerRepoID: r.consumerRepo.String(),
			Source:       contracts.SourceCodex,
			ProducerFile: r.producerFile, ProducerLine: r.producerLine,
			ConsumerFile: r.consumerFile, ConsumerLine: r.consumerLine,
		})
	}

	if criteria != nil {
		for _, c := range criteria.Criteria {
			for _, k := range c.Contracts {
				if !k.CrossRepository() {
					continue
				}
				pi, okP := byKey[strings.ToLower(strings.TrimSpace(k.ProducerRepository))]
				ci, okC := byKey[strings.ToLower(strings.TrimSpace(k.ConsumerRepository))]
				if !okP || !okC || pi == ci {
					continue // one-sided: the validator's business
				}
				add(ci, pi, contracts.Ref{
					Type: contracts.NormalizeType(k.Type), Name: k.Name,
					ProducerRepoID: planned[pi].item.RepositoryID.String(), ConsumerRepoID: planned[ci].item.RepositoryID.String(),
					Source: contracts.SourceCriteria, CriterionID: c.ID,
				})
			}
		}
	}

	sort.SliceStable(order, func(i, j int) bool {
		if order[i].consumer != order[j].consumer {
			return order[i].consumer < order[j].consumer
		}
		return order[i].producer < order[j].producer
	})
	out := make([]contractEdge, 0, len(order))
	for _, p := range order {
		out = append(out, *edges[p])
	}
	return out
}

// applyContractEdges records the edges on the plan: the consumer depends on
// the producer (added to its dependsOn and message Dependencies when the
// title dependencies did not already have it), both sides' messages carry
// the contracts, both descriptions say so, and the pair's contracts are kept
// for the plan_task_dependencies row. It rejects edges that close a cycle
// with the plan's own ordering, leaving planned unchanged.
func applyContractEdges(planned []plannedSubtask, edges []contractEdge) error {
	if len(edges) == 0 {
		return nil
	}
	ids := make(map[uuid.UUID]int, len(planned))
	for i, ps := range planned {
		ids[ps.item.ID] = i
	}
	graph := make([][]int, len(planned))
	for i, ps := range planned {
		for _, d := range ps.dependsOn {
			if j, ok := ids[d]; ok {
				graph[i] = append(graph[i], j)
			}
		}
	}
	for _, e := range edges {
		if !containsInt(graph[e.consumer], e.producer) {
			graph[e.consumer] = append(graph[e.consumer], e.producer)
		}
	}
	if cycle := plan.FindDependencyCycle(graph); cycle != nil {
		labels := make([]string, 0, len(cycle))
		for _, n := range cycle {
			labels = append(labels, fmt.Sprintf("%q (%s)", planned[n].msg.Title, planned[n].repoName))
		}
		onCycle := map[[2]int]bool{}
		for i := 0; i+1 < len(cycle); i++ {
			onCycle[[2]int{cycle[i], cycle[i+1]}] = true
		}
		var contractsText []string
		for _, e := range edges {
			if !onCycle[[2]int{e.consumer, e.producer}] {
				continue
			}
			for _, r := range e.refs {
				contractsText = append(contractsText, fmt.Sprintf("%s: %s consumes from %s", r.Label(), planned[e.consumer].repoName, planned[e.producer].repoName))
			}
		}
		return fmt.Errorf("%w: %s; the contracts require each consumer to wait for its producer (%s), but the plan's depends_on makes a producer wait for its consumer",
			errContractCycle, strings.Join(labels, " -> "), strings.Join(contractsText, "; "))
	}

	for _, e := range edges {
		c, p := &planned[e.consumer], &planned[e.producer]
		producerID := p.item.ID
		if !containsUUID(c.dependsOn, producerID) {
			c.dependsOn = append(c.dependsOn, producerID)
			c.msg.Dependencies = append(c.msg.Dependencies, producerID.String())
		}
		if c.contractRefs == nil {
			c.contractRefs = map[uuid.UUID][]contracts.Ref{}
		}
		c.contractRefs[producerID] = append(c.contractRefs[producerID], e.refs...)
		for _, r := range e.refs {
			c.msg.Contracts = append(c.msg.Contracts, taskContract{Ref: r, Role: roleConsumer, CounterpartTask: p.msg.TaskID, CounterpartRepo: p.repoName})
			p.msg.Contracts = append(p.msg.Contracts, taskContract{Ref: r, Role: roleProducer, CounterpartTask: c.msg.TaskID, CounterpartRepo: c.repoName})
		}
	}
	for i := range planned {
		if len(planned[i].msg.Contracts) == 0 {
			continue
		}
		section := renderContracts(planned[i].msg.Contracts)
		planned[i].msg.Description += section
		planned[i].item.Description += section
	}
	return nil
}

// renderContracts is the subtask description's contract section.
func renderContracts(cs []taskContract) string {
	var b strings.Builder
	b.WriteString("\n## Cross-repository contracts\n\n")
	for _, c := range cs {
		src := "declared by " + c.CriterionID
		if c.Source == contracts.SourceCodex {
			src = fmt.Sprintf("codex: %s:%d -> %s:%d", c.ProducerFile, c.ProducerLine, c.ConsumerFile, c.ConsumerLine)
		}
		switch c.Role {
		case roleConsumer:
			fmt.Fprintf(&b, "- **Consumer of %s** (%s), produced by repository %s (subtask %s). This subtask starts only after that one completes; build against the producer's new contract, and do not rely on anything it removes or renames.\n",
				c.Label(), src, c.CounterpartRepo, c.CounterpartTask)
		default:
			fmt.Fprintf(&b, "- **Producer of %s** (%s), consumed by repository %s (subtask %s, which waits for this one). Before its pull request opens, removed or retyped paths, fields and columns the consumer still uses block the work.\n",
				c.Label(), src, c.CounterpartRepo, c.CounterpartTask)
		}
	}
	return b.String()
}

// deriveContractEdgesTx is derivation inside the plan's transaction: read
// the codex edges between the plan's repositories, derive, apply.
func deriveContractEdgesTx(ctx context.Context, tx codexEdgeQuerier, planned []plannedSubtask) ([]contractEdge, error) {
	var repos []uuid.UUID
	var criteria *plan.BriefCriteria
	seen := map[uuid.UUID]bool{}
	for _, ps := range planned {
		if ps.criteria != nil && criteria == nil {
			criteria = ps.criteria
		}
		if ps.item == nil || ps.item.RepositoryID == nil || seen[*ps.item.RepositoryID] {
			continue
		}
		seen[*ps.item.RepositoryID] = true
		repos = append(repos, *ps.item.RepositoryID)
	}
	if len(repos) < 2 {
		return nil, nil // one repository: no cross-repository contract
	}
	rows, err := loadCodexContractEdgesTx(ctx, tx, repos)
	if err != nil {
		return nil, err
	}
	edges := deriveContractEdges(planned, rows, criteria)
	if err := applyContractEdges(planned, edges); err != nil {
		return nil, err
	}
	return edges, nil
}

// contractRefJSON is the plan_task_dependencies.contract_ref value.
func contractRefJSON(refs []contracts.Ref) (string, error) {
	b, err := json.Marshal(refs)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func containsUUID(xs []uuid.UUID, x uuid.UUID) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
