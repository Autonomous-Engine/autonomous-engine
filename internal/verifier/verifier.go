// Package verifier is the independent verifier (M3-07): a model call that
// receives the requirements (acceptance criteria and task specifications),
// the diff against the pinned base, the recorded run evidence and a scratch
// worktree at the head commit, and returns a structured per-criterion
// verdict. It never receives the author's persuasive prose: the PR
// description, the implementation summary, the author's conversation and
// the prose of the author's plan revisions have no field in Input, so no
// caller can pass them in.
//
// The verification stage (R14, internal/verifier/stage, hosted by
// review-agent) runs it on implementation.complete, before any PR exists;
// the verdict gates whether a PR is created (Gate). review-agent reuses
// that verdict on pr.ready and runs the verifier there only when none was
// recorded for the head. cmd/verifier-eval measures it against seeded
// defects.
package verifier

import (
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/plan"
)

// Version identifies the verifier's prompt contract and gate rules. It is
// recorded in configuration snapshots (configsnap.VerifierVersion must equal
// it; a test checks) so a change to either gets a new snapshot id.
//
// "2" (R3): the verdict gained Gaps, and non-code criteria are recorded as
// not_verifiable gaps (escalating) instead of being judged by the model.
// "3" (R6): the verifier's tools changed behaviour and descriptions —
// read_file returns 400 lines by default, run_command filters test output
// (raw=true for all of it), search_code searches in Go.
// "4" (R9): a task's verification step the planner attached (attached_by
// "planner", the repository's whole-suite command) is rendered as the
// planner's, not as a check of the task's criteria.
// "5" (R16): the gate's decision contract changed — a recorded gap no
// longer pre-empts request_changes, a security defect escalates, and a fix
// round needs a small (VERIFIER_FIX_MAX_ITEMS) and actionable verdict.
// "6" (R18): the parse contract changed — a bare path of a file at head is
// file-level evidence, and an invalid evidence reference voids only its
// criterion (not_verifiable, "evidence ref invalid: …") after the one
// re-prompt, which names only the offending citations.
// "7" (R18 review M2): the gate contract changed — only the categories
// injection, secrets, authz, ssrf, path_traversal, deserialization and xss
// escalate a critical or major defect as security; the prompt lists them.
const Version = "7"

// SchemaVersion is the verdict document's schema version.
const SchemaVersion = 1

// Status of one criterion.
type Status string

const (
	StatusMet           Status = "met"
	StatusNotMet        Status = "not_met"
	StatusNotVerifiable Status = "not_verifiable"
)

// Severity of a defect.
type Severity string

const (
	SeverityCritical Severity = "critical"
	SeverityMajor    Severity = "major"
	SeverityMinor    Severity = "minor"
)

// Overall verdict.
type Overall string

const (
	OverallApprove        Overall = "approve"
	OverallRequestChanges Overall = "request_changes"
	OverallEscalate       Overall = "escalate"
)

// Ref is one piece of evidence a criterion result cites: a tool call id of
// the verifier's own run, or a "path:line" (or "path:start-end") reference
// into the tree at head.
type Ref string

// CriterionResult is the verdict on one acceptance criterion.
type CriterionResult struct {
	ID       string `json:"id"`
	Status   Status `json:"status"`
	Evidence []Ref  `json:"evidence"`
	Note     string `json:"note"`
}

// Defect is one problem the verifier found.
type Defect struct {
	Severity     Severity `json:"severity"`
	File         string   `json:"file"`
	Line         int      `json:"line"`
	Category     string   `json:"category"`
	Description  string   `json:"description"`
	Reproduction string   `json:"reproduction"`
}

// ModelInfo says which model produced the verdict and how it was chosen.
type ModelInfo struct {
	Provider string `json:"provider,omitempty"`
	ModelID  string `json:"model_id,omitempty"`
	// Source is how the model was resolved: SourceVerifierDefinition,
	// SourceVerifierCredential, SourceSharedWithReviewer, or "" when no
	// model was called (the verdict was decided without one).
	Source string `json:"source,omitempty"`
	// SharedWithAuthor is true when the verifier's (provider, model) equals
	// the author's recorded model, or — the author's model unknown — when
	// it fell back to review-agent's model (the author's whenever the work
	// item names an agent definition). Independence of context still holds;
	// independence of model does not.
	SharedWithAuthor bool `json:"verifier_model_shared_with_author"`
	// AuthorModel is the author's "provider/model" it was compared with;
	// "" when the author's model was not recorded.
	AuthorModel string `json:"author_model,omitempty"`
}

// Model sources.
const (
	SourceVerifierDefinition = "verifier_agent_definition"
	SourceVerifierCredential = "verifier_credential"
	SourceSharedWithReviewer = "review_agent_model"
)

// Verdict is the verifier's structured output (schema version 1).
type Verdict struct {
	SchemaVersion int               `json:"schema_version"`
	PerCriterion  []CriterionResult `json:"per_criterion"`
	Defects       []Defect          `json:"defects"`
	Overall       Overall           `json:"overall"`
	ModelInfo     ModelInfo         `json:"model_info"`
	// Summary is the verifier's own one-paragraph account (optional).
	Summary string `json:"summary,omitempty"`
	// Error is set when the verdict was decided without a parsed model
	// answer: no repository, a tool-loop failure, or output that failed
	// strict parsing twice. Such a verdict is always escalate.
	Error string `json:"error,omitempty"`
	// Gaps are the recorded gaps a human must check: one line per non-code
	// criterion ("criterion AC-3 is non-code: <reason>; verify manually",
	// plan.NonCodeGap). They come from the requirements, never from the
	// model (ParseVerdict clears them); each such criterion is also in
	// PerCriterion as not_verifiable with the line as its note, so the gate
	// never approves while one is open (R3). Since R16 a gap does not
	// pre-empt a fix round: a not_met criterion or a major defect next to it
	// requests changes first, and the gap escalates once the rest is met.
	Gaps []string `json:"gaps,omitempty"`
	// InvalidEvidence are the criteria an invalid evidence reference
	// voided after the re-prompt (R18): each is in PerCriterion as
	// not_verifiable with a note opening "evidence ref invalid: ". Set by
	// Run, never by the model (parsing clears it). The gate lets a fix
	// round go ahead next to them and escalates them once the rest is met.
	InvalidEvidence []string `json:"invalid_evidence,omitempty"`
	// planChecks are the commands the plan and the recorded evidence run
	// (task specs' verification steps, evidence checks), set by Run so the
	// gate can tell a backticked check in a note from any other backticked
	// text. Not serialised: a verdict re-read from its artifact keeps the
	// gate decision recorded with it.
	planChecks []string
}

// WithPlanChecks returns v with the commands the plan runs (see
// planChecks); for callers that gate a verdict Run did not produce.
func (v Verdict) WithPlanChecks(cmds []string) Verdict {
	v.planChecks = append([]string(nil), cmds...)
	return v
}

// PlanCommands are the commands in.TaskSpecs' verification steps and
// in.Evidence's recorded checks run.
func (in Input) PlanCommands() []string {
	var out []string
	seen := map[string]bool{}
	add := func(c string) {
		c = strings.TrimSpace(c)
		if c != "" && !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	for _, ts := range in.TaskSpecs {
		for _, st := range ts.Spec.Verification {
			add(st.Command)
		}
	}
	for _, r := range in.Evidence.Records {
		for _, c := range r.Checks {
			add(c.Command)
		}
	}
	return out
}

// TaskSpec is one subtask's specification as the planner wrote it
// (plan_tasks.message.task_spec). Only the structured spec crosses over:
// the subtask's title and description prose do not.
type TaskSpec struct {
	TaskID string        `json:"task_id"`
	Spec   plan.TaskSpec `json:"spec"`
}

// Diff is the change under verification: the pinned base to the head.
type Diff struct {
	// BaseRef is what the diff is taken against: the pinned base SHA, or
	// origin/<base_branch> for a legacy-unpinned item.
	BaseRef string `json:"base_ref"`
	BaseSHA string `json:"base_sha,omitempty"`
	HeadSHA string `json:"head_sha"`
	// Files is the git-derived list of changed files.
	Files []string `json:"files"`
	// Patch is `git diff <base>...<head>`, truncated to MaxPatchBytes.
	Patch     string `json:"patch"`
	Truncated bool   `json:"truncated,omitempty"`
}

// CheckSummary is one check of a verification record.
type CheckSummary struct {
	Kind       evidence.CheckKind   `json:"kind"`
	Command    string               `json:"command,omitempty"`
	ExitCode   *int                 `json:"exit_code,omitempty"`
	Status     evidence.CheckStatus `json:"status"`
	SkipReason string               `json:"skip_reason,omitempty"`
}

// EvidenceRecord is the latest verification record (M0) of one subtask.
// Checks include the impact-selected fast checks the gate appended (M3-04).
type EvidenceRecord struct {
	TaskID       string           `json:"task_id"`
	Outcome      evidence.Outcome `json:"outcome"`
	BaseSHA      string           `json:"base_sha,omitempty"`
	HeadSHA      string           `json:"head_sha,omitempty"`
	ChangedFiles []string         `json:"changed_files,omitempty"`
	Checks       []CheckSummary   `json:"checks,omitempty"`
	Flags        []string         `json:"flags,omitempty"`
}

// RunCommand is one run_command the implementation run recorded.
type RunCommand struct {
	Seq      int64  `json:"seq"`
	CallID   string `json:"call_id,omitempty"`
	Command  string `json:"command"`
	ExitCode *int   `json:"exit_code,omitempty"`
}

// RunTransition is one controller state change of the run (M3-02). Why is
// the controller's deterministic reason, never model text.
type RunTransition struct {
	Seq  int64  `json:"seq"`
	From string `json:"from"`
	To   string `json:"to"`
	Why  string `json:"why,omitempty"`
}

// RunSummary summarises the latest implementation run of one subtask
// (implementation_runs + implementation_run_events). Only structured
// fields cross over; free-text note events are counted, not copied.
type RunSummary struct {
	TaskID       string          `json:"task_id"`
	Attempt      int             `json:"attempt"`
	State        string          `json:"state"`
	ToolCounts   map[string]int  `json:"tool_counts,omitempty"`
	Commands     []RunCommand    `json:"commands,omitempty"`
	Transitions  []RunTransition `json:"transitions,omitempty"`
	WrittenPaths []string        `json:"written_paths,omitempty"`
	NoteEvents   int             `json:"note_events,omitempty"`
}

// Evidence is what the implementation recorded about itself, in structured
// form only.
type Evidence struct {
	Records []EvidenceRecord `json:"records,omitempty"`
	Runs    []RunSummary     `json:"runs,omitempty"`
}

// Input is everything the verifier may see. It deliberately has no field
// for the PR description, the implementation summary, the author's
// conversation or the author's plan-revision prose (the isolation argument
// in the spec); TestInputHasNoAuthorProseField pins that.
type Input struct {
	WorkID    string           `json:"work_id"`
	Criteria  []plan.Criterion `json:"criteria"`
	TaskSpecs []TaskSpec       `json:"task_specs"`
	Diff      Diff             `json:"diff"`
	Evidence  Evidence         `json:"evidence"`
	// RepoTree is the scratch repository at head the tools run in; "" when
	// no clone is available (the verdict is then escalate without a call).
	RepoTree string `json:"-"`
	// Unavailable says why RepoTree is empty.
	Unavailable string `json:"-"`
	// Tools are the tool names the verifier may call (ToolNames()).
	Tools []string `json:"tools"`
}

// MaxPatchBytes caps the diff inlined in the prompt; git_diff returns the
// rest.
const MaxPatchBytes = 60000
