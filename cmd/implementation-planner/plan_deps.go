package main

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/plan"
	"github.com/google/uuid"
)

// Dependency resolution for a decomposition (M1-06). Pure: no database, no
// bus. It runs after phase 1 has built every repo-scoped subtask and before
// phase 2 commits them, so an invalid graph blocks the parent with nothing
// written.
//
// The LLM states dependencies between its granular steps, by title. The
// planner groups those steps into one subtask per repository, so a step's
// dependency becomes an edge between the two subtasks that subsumed them —
// or nothing, when both steps landed in the same subtask (that ordering is
// spelled out in the subtask's description and is the single agent run's
// business).

var (
	// errUnknownDependency: a step depends on a title the LLM never
	// produced. The plan is not what the model thinks it is.
	errUnknownDependency = errors.New("unknown dependency")
	// errAmbiguousDependency: the title a step depends on names steps in two
	// different subtasks, so there is no single task to wait for.
	errAmbiguousDependency = errors.New("ambiguous dependency")
	// errDependencyCycle: the subtask graph has a cycle, so no task on it
	// could ever be released.
	errDependencyCycle = errors.New("dependency cycle")
)

// planNode is one repo-scoped subtask as dependency resolution sees it.
type planNode struct {
	// label names the subtask in error messages (its title).
	label string
	// titles are the granular step titles this subtask subsumed.
	titles []string
	// dependsOn is the union of those steps' depends_on titles.
	dependsOn []string
}

// planStep is one step the LLM produced, kept or dropped: its title and the
// titles it depends on. Dropped steps are needed for their dependencies.
type planStep struct {
	title     string
	dependsOn []string
}

// planDependencies is the resolved graph.
type planDependencies struct {
	// deps[i] lists, sorted and without duplicates, the indices of the
	// subtasks subtask i depends on; nil when it depends on none.
	deps [][]int
	// dropped lists dependency titles that name a step the LLM produced but
	// the planner dropped (non-implementation filter, test-only filter, no
	// resolvable repository). Such a step will never run, so waiting on it
	// would hold the dependent forever; the edge to it is ignored and
	// reported, and the dropped step's own dependencies are inherited.
	dropped []string
	// inherited describes each edge a subtask gained through a dropped step
	// (A -> D(dropped) -> B gives A -> B), for the log.
	inherited []string
}

// normalizeTitle is the matching key for titles: case and runs of
// whitespace are not meaningful differences in what an LLM wrote.
func normalizeTitle(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// dependencyProblem is a dependency title resolution could not map to one
// subtask: it names no produced step (owners empty), or steps in several
// subtasks (owners lists them).
type dependencyProblem struct {
	node    int    // the subtask whose dependency it is
	dep     string // the title as written
	through string // the dropped step it was inherited through ("" for the subtask's own)
	owners  []int  // the subtasks an ambiguous title names; empty for an unknown one
}

func (p dependencyProblem) err(nodes []planNode) error {
	through := ""
	if p.through != "" {
		through = fmt.Sprintf(" (through dropped step %q)", p.through)
	}
	if len(p.owners) > 1 {
		labels := make([]string, 0, len(p.owners))
		for _, o := range p.owners {
			labels = append(labels, fmt.Sprintf("%q", nodes[o].label))
		}
		return fmt.Errorf("%w: task %q depends on %q%s, which names steps in %d different subtasks (%s)",
			errAmbiguousDependency, nodes[p.node].label, p.dep, through, len(p.owners), strings.Join(labels, ", "))
	}
	return fmt.Errorf("%w: task %q depends on %q%s, which is not a task in this plan",
		errUnknownDependency, nodes[p.node].label, p.dep, through)
}

// resolvePlanDependencies maps every dependency title to the subtask that
// subsumed it, then rejects the plan if the subtask graph has a cycle.
// steps are all steps the LLM produced, including the ones the planner later
// dropped: a dependency on a dropped step is replaced by that step's own
// dependencies, transitively, so ordering that ran through it is kept
// (A -> D -> B with D dropped still makes A wait for B).
func resolvePlanDependencies(nodes []planNode, steps []planStep) (planDependencies, error) {
	out, problems := resolveDependencyEdges(nodes, steps)
	if len(problems) > 0 {
		return planDependencies{}, problems[0].err(nodes)
	}
	if cycle := findDependencyCycle(out.deps); cycle != nil {
		labels := make([]string, 0, len(cycle))
		for _, idx := range cycle {
			labels = append(labels, fmt.Sprintf("%q", nodes[idx].label))
		}
		return planDependencies{}, fmt.Errorf("%w: %s", errDependencyCycle, strings.Join(labels, " -> "))
	}
	return out, nil
}

// resolveDependencyEdges is resolvePlanDependencies' title resolution
// without the verdict: every edge it can resolve, and every title it cannot
// (in subtask order, then worklist order — so the first problem is the one
// resolvePlanDependencies reports). The plan validator (M2-05) reports all of
// them; the cycle check is the caller's.
func resolveDependencyEdges(nodes []planNode, steps []planStep) (planDependencies, []dependencyProblem) {
	owners := make(map[string][]int) // normalized title -> subtask indices
	for i, n := range nodes {
		for _, title := range n.titles {
			key := normalizeTitle(title)
			if key == "" || containsInt(owners[key], i) {
				continue
			}
			owners[key] = append(owners[key], i)
		}
	}
	// known maps every produced step title to the union of the depends_on
	// of the steps with that title.
	known := make(map[string][]string, len(steps))
	for _, st := range steps {
		key := normalizeTitle(st.title)
		if key == "" {
			continue
		}
		known[key] = append(known[key], st.dependsOn...)
	}

	out := planDependencies{deps: make([][]int, len(nodes))}
	var problems []dependencyProblem
	droppedSeen := map[string]bool{}
	for i, n := range nodes {
		// A worklist of dependency titles; via is the dropped step a title
		// was inherited through ("" for the subtask's own).
		type pending struct{ dep, via string }
		work := make([]pending, 0, len(n.dependsOn))
		for _, dep := range n.dependsOn {
			work = append(work, pending{dep: dep})
		}
		expanded := map[string]bool{} // dropped steps already expanded for this subtask
		reported := map[string]bool{} // problem titles already reported for this subtask
		for len(work) > 0 {
			p := work[0]
			work = work[1:]
			key := normalizeTitle(p.dep)
			if key == "" {
				continue
			}
			owner := owners[key]
			_, isKnown := known[key]
			switch {
			case len(owner) > 1:
				if !reported[key] {
					reported[key] = true
					problems = append(problems, dependencyProblem{node: i, dep: p.dep, through: p.via, owners: append([]int(nil), owner...)})
				}
			case len(owner) == 1:
				if owner[0] != i && !containsInt(out.deps[i], owner[0]) {
					out.deps[i] = append(out.deps[i], owner[0])
					if p.via != "" {
						out.inherited = append(out.inherited, fmt.Sprintf("%q -> %q via dropped step %q", n.label, nodes[owner[0]].label, p.via))
					}
				}
			case isKnown:
				if !droppedSeen[key] {
					droppedSeen[key] = true
					out.dropped = append(out.dropped, p.dep)
				}
				if !expanded[key] {
					expanded[key] = true
					for _, d := range known[key] {
						work = append(work, pending{dep: d, via: p.dep})
					}
				}
			default:
				if !reported[key] {
					reported[key] = true
					problems = append(problems, dependencyProblem{node: i, dep: p.dep, through: p.via})
				}
			}
		}
		sort.Ints(out.deps[i])
	}
	return out, problems
}

// findDependencyCycle is the topological check, shared with the plan
// validator: plan.FindDependencyCycle.
var findDependencyCycle = plan.FindDependencyCycle

// planDependencyGraph is the plan's dependency graph for the validator
// (M2-05): M1-06's resolution of the phase-1 subtasks, with every title it
// could not resolve attributed to the step that declared it.
func planDependencyGraph(planned []plannedSubtask, steps []planStep) plan.DependencyGraph {
	nodes := make([]planNode, len(planned))
	for i, ps := range planned {
		nodes[i] = planNode{label: ps.msg.Title, titles: ps.titles, dependsOn: ps.dependsOnTitles}
	}
	resolved, problems := resolveDependencyEdges(nodes, steps)
	g := plan.DependencyGraph{Edges: resolved.deps}
	for _, n := range nodes {
		g.Labels = append(g.Labels, n.label)
	}
	seen := map[string]bool{}
	for _, st := range steps {
		if key := normalizeTitle(st.title); key != "" && !seen[key] {
			seen[key] = true
			g.KnownTitles = append(g.KnownTitles, st.title)
		}
	}
	for _, p := range problems {
		u := plan.UnresolvedDependency{Subtask: nodes[p.node].label, Dependency: p.dep, Through: p.through}
		for _, o := range p.owners {
			u.Ambiguous = append(u.Ambiguous, nodes[o].label)
		}
		// The declaring step: the kept step of this subtask that names the
		// title (or, for an inherited one, the dropped step it came through).
		want := normalizeTitle(p.dep)
		if p.through != "" {
			want = normalizeTitle(p.through)
		}
		for _, st := range planned[p.node].stepTasks {
			for _, d := range st.DependsOn {
				if normalizeTitle(d) == want && u.Task == "" {
					u.TaskIndex, u.Task = st.index, st.Title
				}
			}
		}
		g.Unresolved = append(g.Unresolved, u)
	}
	return g
}

// applyPlanDependencies resolves the dependency graph of a phase-1 plan and,
// only if it is valid, records each subtask's dependencies on it: as task
// ids for the plan_task_dependencies rows, and in the implementation.tasks
// message's Dependencies field. On error the plan is left untouched. It
// returns the resolution notes (dropped titles, inherited edges) for the log;
// its deps field is nil.
func applyPlanDependencies(planned []plannedSubtask, steps []planStep) (planDependencies, error) {
	nodes := make([]planNode, len(planned))
	for i, ps := range planned {
		nodes[i] = planNode{label: ps.msg.Title, titles: ps.titles, dependsOn: ps.dependsOnTitles}
	}
	resolved, err := resolvePlanDependencies(nodes, steps)
	if err != nil {
		return planDependencies{}, err
	}

	ids := make([]uuid.UUID, len(planned))
	for i, ps := range planned {
		id, err := uuid.Parse(ps.msg.TaskID)
		if err != nil {
			return planDependencies{}, fmt.Errorf("subtask %q has an unparseable task id %q: %w", ps.msg.Title, ps.msg.TaskID, err)
		}
		ids[i] = id
	}
	for i := range planned {
		if len(resolved.deps[i]) == 0 {
			continue
		}
		dependsOn := make([]uuid.UUID, 0, len(resolved.deps[i]))
		names := make([]string, 0, len(resolved.deps[i]))
		for _, d := range resolved.deps[i] {
			dependsOn = append(dependsOn, ids[d])
			names = append(names, ids[d].String())
		}
		planned[i].dependsOn = dependsOn
		planned[i].msg.Dependencies = names
	}
	return planDependencies{dropped: resolved.dropped, inherited: resolved.inherited}, nil
}

// planSteps lists every step the LLM produced, with its dependencies.
func planSteps(tasks []decomposedTask) []planStep {
	out := make([]planStep, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, planStep{title: t.Title, dependsOn: t.DependsOn})
	}
	return out
}

// titleSteps is planSteps for steps without dependencies.
func titleSteps(titles ...string) []planStep {
	out := make([]planStep, 0, len(titles))
	for _, t := range titles {
		out = append(out, planStep{title: t})
	}
	return out
}

func containsInt(xs []int, x int) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
