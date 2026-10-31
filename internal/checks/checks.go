// Package checks selects the fast checks for a change (roadmap 3.2, ticket
// M3-04): given the repository tree, the files changed against the pinned
// base, the repository verification policy and the codex contract edges,
// Select returns a deterministic Plan — the impact-selected targets the
// implementation agent runs in its fast loop, and the full suite the final
// gate runs regardless.
//
// Selection narrows the FAST loop only. The final gate keeps running the
// policy's full suite (cmd/implementation-agent/finalize.go), so a plan can
// never weaken the M0 gate. When the impact graph cannot be built the plan
// falls back to the full suite with a recorded reason, never to nothing.
//
// Languages (M3-04 Go; M3-05 JavaScript/TypeScript and Python): every
// language the repository shows signals for contributes, and the plan is
// the union. Only the language whose runner the gate's command invokes
// narrows the test suite.
package checks

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/verifypolicy"
)

// Target is one check a plan selects. A target with an empty Command cannot
// be executed here; NotRunReason says why (no migration test detected, no
// api_compat checker yet, a required kind the agent has no runner for).
type Target struct {
	Kind    evidence.CheckKind `json:"kind"`
	Command string             `json:"command,omitempty"`
	// Scope names what the target covers: package directories ("./internal/x"),
	// files, or a consumer repository.
	Scope []string `json:"scope,omitempty"`
	// Reason says why the target was selected ("owner of changed file X",
	// "reverse dependent of ./pkg/y", "policy requires lint").
	Reason       string `json:"reason"`
	NotRunReason string `json:"not_run_reason,omitempty"`
	// Advisory targets run and are reported (plan, model note, run
	// output) but never fail the fast run and are never recorded as failed
	// evidence: a check the gate does not run must not make the fast loop
	// stricter than the gate. go vet and go build are advisory unless the
	// policy requires lint / build.
	Advisory bool `json:"advisory,omitempty"`
	// Skipped is set after a run whose command ran no test (an all-skipped
	// migration run): such a target is shown, never recorded as evidence.
	Skipped string `json:"skipped,omitempty"`
	// batchPrefix/batchSuffix, when set, let Batches merge targets of the
	// same kind into one invocation: prefix + scopes + suffix.
	batchPrefix string
	batchSuffix string
	// batchArg, when set, is the target's argument as written on the
	// command line (shell-quoted / runner-escaped); Scope keeps the plain
	// path for display.
	batchArg string
}

// Executable reports whether the target has a command to run.
func (t Target) Executable() bool { return strings.TrimSpace(t.Command) != "" }

// Batch is one command the fast runner executes: a single target, or
// several mergeable targets (per-package go test targets) in one
// invocation so many dependents do not serialise into the time budget.
type Batch struct {
	Kind     evidence.CheckKind
	Command  string
	Advisory bool
	Targets  []Target
}

// MarkSkipped records on every target of batch b (matched by kind and
// command) that its run executed no test.
func (p *Plan) MarkSkipped(b Batch, why string) {
	for _, bt := range b.Targets {
		for i := range p.Targets {
			if p.Targets[i].Kind == bt.Kind && p.Targets[i].Command == bt.Command {
				p.Targets[i].Skipped = why
			}
		}
	}
}

// Batches groups the executable targets for execution, in plan order.
func (p Plan) Batches() []Batch {
	var out []Batch
	index := map[string]int{}
	for _, t := range p.Executable() {
		if t.batchPrefix == "" {
			out = append(out, Batch{Kind: t.Kind, Command: t.Command, Advisory: t.Advisory, Targets: []Target{t}})
			continue
		}
		key := fmt.Sprintf("%s\x1f%s\x1f%s\x1f%t", t.Kind, t.batchPrefix, t.batchSuffix, t.Advisory)
		if i, ok := index[key]; ok {
			out[i].Targets = append(out[i].Targets, t)
			continue
		}
		index[key] = len(out)
		out = append(out, Batch{Kind: t.Kind, Advisory: t.Advisory, Targets: []Target{t}})
	}
	for i := range out {
		b := &out[i]
		if b.Command != "" {
			continue
		}
		t0 := b.Targets[0]
		parts := []string{t0.batchPrefix}
		for _, t := range b.Targets {
			if t.batchArg != "" {
				parts = append(parts, t.batchArg)
			} else {
				parts = append(parts, t.Scope...)
			}
		}
		if t0.batchSuffix != "" {
			parts = append(parts, t0.batchSuffix)
		}
		b.Command = strings.Join(parts, " ")
	}
	return out
}

// Plan is the result of Select.
type Plan struct {
	// Language names the impact implementations that applied, joined
	// with "+" in registration order ("go", "javascript+python"), or ""
	// when none applied.
	Language string `json:"language,omitempty"`
	// ChangedFiles is the normalised, sorted input.
	ChangedFiles []string `json:"changed_files"`
	// Targets are the fast checks, in execution order.
	Targets []Target `json:"targets"`
	// Full is what the final gate runs: the policy's full suite.
	Full []Target `json:"full"`
	// Fallback is non-empty when Targets fell back to the full suite; it
	// says why (no go.mod, parse error, go.mod changed, custom command).
	Fallback string `json:"fallback,omitempty"`
}

// Executable returns the targets that have a command, in order.
func (p Plan) Executable() []Target {
	var out []Target
	for _, t := range p.Targets {
		if t.Executable() {
			out = append(out, t)
		}
	}
	return out
}

// Signature identifies the plan's fast targets (kind and command, in
// order) so callers can tell whether a recomputed plan changed.
func (p Plan) Signature() string {
	var b strings.Builder
	for _, t := range p.Targets {
		fmt.Fprintf(&b, "%s\x1f%s\x1f%s\x1e", t.Kind, t.Command, t.NotRunReason)
	}
	return b.String()
}

// Note is the short system note shown to the model: the fast checks for
// its change, and that the full suite runs at the gate.
func (p Plan) Note() string {
	var parts []string
	for _, t := range p.Targets {
		if t.Executable() {
			reason := t.Reason
			if t.Advisory {
				reason += "; advisory: reported, never fails the run"
			}
			parts = append(parts, fmt.Sprintf("`%s` (%s)", t.Command, reason))
		}
	}
	var b strings.Builder
	b.WriteString("[system note] Fast checks for your change: ")
	if len(parts) == 0 {
		b.WriteString("none selected")
	} else {
		b.WriteString(strings.Join(parts, "; "))
	}
	b.WriteString(". Run these with run_command while you iterate")
	full := ""
	for _, t := range p.Full {
		if t.Kind == evidence.CheckKindTest && t.Executable() {
			full = t.Command
		}
	}
	if full != "" {
		fmt.Fprintf(&b, "; the full suite (`%s`) runs at the final gate regardless.", full)
	} else {
		b.WriteString("; the final gate runs the repository's full verification policy regardless.")
	}
	for _, t := range p.Targets {
		if !t.Executable() && t.Kind != evidence.CheckKindTest {
			fmt.Fprintf(&b, " Not run here: %s (%s): %s.", t.Kind, t.Reason, t.NotRunReason)
		}
	}
	return b.String()
}

// Policy is what selection needs from the repository verification policy
// (M3-03) and, later, from the work item's acceptance criteria (M2-04).
type Policy struct {
	// FullTestCommand is the command the final gate runs: the policy's
	// test_command override, else the detected command ("" when none).
	FullTestCommand string
	// RequiredKinds are the check kinds that must appear in the fast plan.
	RequiredKinds []evidence.CheckKind
}

// PolicyFor builds the selection policy from the repository verification
// policy and the detected test command. criteriaKinds are additional
// required kinds (the RequiredChecks of the work item's acceptance criteria
// once M2-04 lands); they are unioned with the policy's.
func PolicyFor(vp verifypolicy.Policy, detectedTestCommand string, criteriaKinds ...evidence.CheckKind) Policy {
	p := Policy{FullTestCommand: vp.EffectiveTestCommand(detectedTestCommand)}
	seen := map[evidence.CheckKind]bool{}
	for _, k := range append(append([]evidence.CheckKind{}, vp.RequiredChecks...), criteriaKinds...) {
		k = evidence.CheckKind(strings.ToLower(strings.TrimSpace(string(k))))
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		p.RequiredKinds = append(p.RequiredKinds, k)
	}
	return p
}

func (p Policy) requires(k evidence.CheckKind) bool {
	for _, r := range p.RequiredKinds {
		if r == k {
			return true
		}
	}
	return false
}

// ContractEdge is one codex_contract_edges row whose producer is this
// repository (migration 026; internal/codex/extractors/contracts).
type ContractEdge struct {
	ContractType string `json:"contract_type"`
	ContractName string `json:"contract_name"`
	ProducerFile string `json:"producer_file"`
	ConsumerRepo string `json:"consumer_repo"` // repository name, else its id
	ConsumerFile string `json:"consumer_file"`
}

// Impact is one language's impact analysis.
type Impact interface {
	// Language names the implementation ("go", "javascript", "python").
	Language() string
	// Applies reports whether the repository shows this language's
	// signals: the same predicates internal/testdetect.Languages uses
	// (HasGoModule, HasNodePackage, HasPythonProject).
	Applies(repoTree string) bool
	// Targets returns the impact-selected targets for changed (normalised,
	// sorted, repository-relative). A *runnerMismatchError means the
	// gate's test command is not this language's runner: the targets
	// returned with it (non-test checks only) are kept, and no test target
	// is narrowed from this language. Any other error means the impact
	// graph cannot be built; Select then falls back to the full suite with
	// the error as the reason.
	Targets(ctx context.Context, repoTree string, changed []string, p Policy) ([]Target, error)
}

// Impacts are the registered implementations, in a fixed order; every one
// that applies contributes to the plan (Select).
var Impacts = []Impact{GoImpact{}, JSImpact{}, PythonImpact{}}

// Select computes the plan. It is deterministic: the same tree, changed
// files, policy and edges always yield the same plan. No model is involved.
func Select(ctx context.Context, repoTree string, changedFiles []string, policy Policy, contractEdges []ContractEdge) Plan {
	changed := NormalizeFiles(changedFiles)
	plan := Plan{ChangedFiles: changed, Full: fullSuite(policy)}

	// Every applying language contributes (a repository may be
	// multi-language: the plan is the union). Only the language whose
	// runner the gate's command invokes can narrow the test suite; the
	// others report a runnerMismatchError and contribute their non-test
	// targets (typecheck, lint, compile) only.
	var applying []Impact
	var langs []string
	for _, im := range Impacts {
		if im.Applies(repoTree) {
			applying = append(applying, im)
			langs = append(langs, im.Language())
		}
	}
	plan.Language = strings.Join(langs, "+")
	var targets, extra []Target
	var mismatches []string
	narrowed := false
	for _, im := range applying {
		ts, err := im.Targets(ctx, repoTree, changed, policy)
		var rm *runnerMismatchError
		switch {
		case err == nil:
			narrowed = true
			targets = append(targets, ts...)
		case errors.As(err, &rm):
			mismatches = append(mismatches, prefixLanguage(len(applying), im.Language(), err.Error()))
			extra = append(extra, ts...)
		default:
			plan.Fallback = prefixLanguage(len(applying), im.Language(), err.Error())
		}
		if plan.Fallback != "" {
			break
		}
	}
	switch {
	case plan.Fallback != "":
		// The impact graph of a language could not be built: the full
		// suite, nothing narrowed.
		targets = fallbackTargets(policy, plan.Fallback)
	case len(applying) == 0:
		plan.Fallback = "no impact implementation applies to this repository"
		targets = fallbackTargets(policy, plan.Fallback)
	case !narrowed:
		// No language's runner is the gate's command (a custom command
		// such as `make test`): the full suite, plus the languages'
		// non-test checks.
		plan.Fallback = strings.Join(mismatches, "; ")
		targets = append(fallbackTargets(policy, plan.Fallback), extra...)
	default:
		targets = append(targets, extra...)
	}

	targets = append(targets, contractTargets(changed, contractEdges)...)
	targets = enforceRequired(targets, policy, plan.Language, plan.Fallback != "")

	// Never nothing: with a test suite configured, a plan always runs
	// something.
	if policy.FullTestCommand != "" && !anyExecutable(targets) {
		targets = append(targets, Target{
			Kind:    evidence.CheckKindTest,
			Command: policy.FullTestCommand,
			Scope:   []string{"./..."},
			Reason:  "no fast check selected for the changed files; running the full suite",
		})
	}
	plan.Targets = targets
	return plan
}

// FallbackPlan is the plan when the change itself cannot be determined
// (e.g. the diff could not be listed): the full suite, with why.
func FallbackPlan(policy Policy, why string) Plan {
	targets := enforceRequired(fallbackTargets(policy, why), policy, "", true)
	return Plan{ChangedFiles: []string{}, Targets: targets, Full: fullSuite(policy), Fallback: why}
}

// fullSuite is what the final gate runs under policy: the test command,
// and a not_run record for each required kind the agent has no runner for
// at the gate (finalize.go addUnimplementedRequiredChecks).
func fullSuite(p Policy) []Target {
	var out []Target
	if p.FullTestCommand != "" {
		out = append(out, Target{
			Kind: evidence.CheckKindTest, Command: p.FullTestCommand, Scope: []string{"./..."},
			Reason: "final gate: the repository's full test suite",
		})
	} else {
		out = append(out, Target{
			Kind:         evidence.CheckKindTest,
			Reason:       "final gate: the repository's full test suite",
			NotRunReason: "no test runner detected and no policy test_command",
		})
	}
	for _, k := range []evidence.CheckKind{evidence.CheckKindBuild, evidence.CheckKindLint} {
		if p.requires(k) {
			out = append(out, Target{
				Kind:         k,
				Reason:       "policy requires " + string(k),
				NotRunReason: "the final gate records required " + string(k) + " as not_run (no gate runner yet)",
			})
		}
	}
	return out
}

func fallbackTargets(p Policy, why string) []Target {
	if p.FullTestCommand == "" {
		return nil
	}
	return []Target{{
		Kind: evidence.CheckKindTest, Command: p.FullTestCommand, Scope: []string{"./..."},
		Reason: "impact graph unavailable (" + why + "); running the full suite",
	}}
}

// contractTargets adds one api_compat target per consumer whose contract's
// producer file changed. No checker exists yet (M2-06), so each is recorded
// not_run with the consumer named.
func contractTargets(changed []string, edges []ContractEdge) []Target {
	changedSet := map[string]bool{}
	for _, f := range changed {
		changedSet[f] = true
	}
	type key struct{ repo, file string }
	byConsumer := map[key][]ContractEdge{}
	var keys []key
	for _, e := range edges {
		pf := normalizeFile(e.ProducerFile)
		if pf == "" || !changedSet[pf] {
			continue
		}
		k := key{e.ConsumerRepo, e.ConsumerFile}
		if _, ok := byConsumer[k]; !ok {
			keys = append(keys, k)
		}
		byConsumer[k] = append(byConsumer[k], e)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].repo != keys[j].repo {
			return keys[i].repo < keys[j].repo
		}
		return keys[i].file < keys[j].file
	})
	var out []Target
	for _, k := range keys {
		es := byConsumer[k]
		sort.Slice(es, func(i, j int) bool {
			if es[i].ProducerFile != es[j].ProducerFile {
				return es[i].ProducerFile < es[j].ProducerFile
			}
			return es[i].ContractName < es[j].ContractName
		})
		var names []string
		seen := map[string]bool{}
		for _, e := range es {
			n := fmt.Sprintf("%s %s (producer %s)", e.ContractType, e.ContractName, normalizeFile(e.ProducerFile))
			if !seen[n] {
				seen[n] = true
				names = append(names, n)
			}
		}
		out = append(out, Target{
			Kind:         evidence.CheckKindAPICompat,
			Scope:        []string{k.repo + ":" + k.file},
			Reason:       fmt.Sprintf("contract edge: consumer %s %s uses %s", k.repo, k.file, strings.Join(names, ", ")),
			NotRunReason: "no api_compat checker available yet (M2-06)",
		})
	}
	return out
}

// enforceRequired makes every required kind visible in the plan: an
// existing target of that kind gets "policy requires <kind>" appended to its
// reason; a missing one is added when the language can supply it. Test is
// supplied by the full suite.
func enforceRequired(targets []Target, p Policy, language string, fellBack bool) []Target {
	for _, k := range p.RequiredKinds {
		found := false
		for i := range targets {
			if targets[i].Kind == k {
				found = true
				if !strings.Contains(targets[i].Reason, "policy requires "+string(k)) {
					targets[i].Reason += "; policy requires " + string(k)
				}
				break // the first target of the kind carries the mark
			}
		}
		if found {
			continue
		}
		switch {
		case k == evidence.CheckKindTest && p.FullTestCommand != "":
			targets = append(targets, Target{
				Kind: k, Command: p.FullTestCommand, Scope: []string{"./..."},
				Reason: "policy requires test and no package owns the changed files; running the full suite",
			})
		case k == evidence.CheckKindTest:
			targets = append(targets, Target{Kind: k, Reason: "policy requires test", NotRunReason: "no test runner detected and no policy test_command"})
		default:
			targets = append(targets, Target{Kind: k, Reason: "policy requires " + string(k), NotRunReason: "no " + string(k) + " command known for this repository"})
		}
	}
	return targets
}

func anyExecutable(ts []Target) bool {
	for _, t := range ts {
		if t.Executable() {
			return true
		}
	}
	return false
}

// NormalizeFiles converts to forward slashes, trims "./", drops empties and
// duplicates, and sorts.
func NormalizeFiles(files []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, f := range files {
		f = normalizeFile(f)
		if f == "" || seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

func normalizeFile(f string) string {
	f = strings.TrimSpace(strings.ReplaceAll(f, "\\", "/"))
	if f == "" {
		return ""
	}
	f = path.Clean(f)
	f = strings.TrimPrefix(f, "./")
	if f == "." || f == "/" || strings.HasPrefix(f, "../") {
		return ""
	}
	return strings.TrimPrefix(f, "/")
}
