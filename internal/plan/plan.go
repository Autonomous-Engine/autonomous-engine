// Package plan is the vocabulary that ties a work item's acceptance criteria
// to the evidence for its scope and to the tasks that implement it (M2-04).
//
// research-agent produces criteria (it has the repository and the user's
// text), research-coordinator merges them into the technical brief, and
// implementation-planner assigns criterion ids to tasks. Everything here is
// JSON inside existing artifacts and messages (the technical_brief artifact,
// research.results, technical-brief.ready, implementation.tasks and its
// stored plan_tasks.message); there is no table. The payloads carry
// schema_version so a reader can tell the shape it is looking at.
//
// Validate checks structure only (ids, known kinds, ranges, required
// fields). Rules that span a whole plan — every criterion covered, every
// cited id known, every existing path evidenced — are M2-05's validator.
package plan

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"github.com/autonomousengine/autonomous-engine/internal/evidence"
)

// SchemaVersion is the version written into BriefCriteria and TaskSpec.
const SchemaVersion = 1

// SourceKind says where a criterion came from.
type SourceKind string

const (
	// SourceUser: stated by the user; Quote holds their words verbatim.
	SourceUser SourceKind = "user"
	// SourceDerived: inferred (from the codebase, or by the planner); Reason
	// says why. Never silent.
	SourceDerived SourceKind = "derived"
)

// EvidenceKind says what supports a claim (a file, a symbol, a contract).
type EvidenceKind string

const (
	// EvidenceCodex: a Codex result (module, pattern, contract edge, Codex
	// file read or grep); Ref is the tool call id that returned it.
	EvidenceCodex EvidenceKind = "codex"
	// EvidenceLSP: a language-server result (symbol, file, line); Ref is the
	// tool call id.
	EvidenceLSP EvidenceKind = "lsp"
	// EvidenceRepo: the path exists in the repository, established by
	// os.Stat in the snapshot worktree (or in the clone for a legacy-unpinned
	// item, and until M2-03 provides the worktree); Basis records which. Ref
	// is the read_file/list_files call id that surfaced it, or "stat".
	EvidenceRepo EvidenceKind = "repo"
	// EvidenceUser: the user's own text; Ref is the quote.
	EvidenceUser EvidenceKind = "user"
	// EvidenceHypothesis: no support. Explicitly marked, never dropped.
	EvidenceHypothesis EvidenceKind = "hypothesis"
	// EvidenceNew: the path is proposed as a new file and does not exist.
	EvidenceNew EvidenceKind = "new"
)

// Risk of a criterion.
type Risk string

const (
	RiskLow    Risk = "low"
	RiskMedium Risk = "medium"
	RiskHigh   Risk = "high"
)

// Basis values for repo evidence.
const (
	BasisSnapshot = "snapshot" // os.Stat in a worktree at the pinned base SHA
	BasisClone    = "clone"    // os.Stat in the shared clone's working tree
)

// RefStat is the Ref of repo evidence established by os.Stat alone.
const RefStat = "stat"

// EvidenceRef is one piece of support for a claim.
type EvidenceRef struct {
	Kind EvidenceKind `json:"kind"`
	// Ref identifies the support: a tool call id (codex, lsp, repo), "stat"
	// (repo established by os.Stat only), or the quote (user). Empty for
	// hypothesis and new.
	Ref        string  `json:"ref,omitempty"`
	Confidence float64 `json:"confidence"`
	// Basis is set on repo evidence: "snapshot" or "clone".
	Basis string `json:"basis,omitempty"`
	// Note explains a downgrade or a grounding result ("cited call_7 is not
	// a repo tool result", "not found in the clone").
	Note string `json:"note,omitempty"`
}

// FileRef is a path a criterion or task touches and why it is believed to
// exist (or to be new).
type FileRef struct {
	Path     string      `json:"path"`
	Evidence EvidenceRef `json:"evidence"`
}

// SymbolRef is a code symbol a criterion touches.
type SymbolRef struct {
	Name     string      `json:"name"`
	File     string      `json:"file,omitempty"`
	Line     int         `json:"line,omitempty"`
	Evidence EvidenceRef `json:"evidence"`
}

// ContractRef is an interface between components (REST route, event topic,
// schema, exported package API) a criterion touches. Type follows the Codex
// contract extractors (rest_openapi, graphql, grpc_protobuf, events,
// sql_schemas, shared_packages, go_interfaces, ts_exports, python_exports).
//
// ProducerRepository and ConsumerRepository (M2-06) name the two sides of a
// cross-repository contract, each by repository id or name. A contract that
// names both, in different repositories, is a contract edge: the planner
// makes the consumer's subtask depend on the producer's, and the validator
// rejects a plan that changes only one side (one_sided_contract).
type ContractRef struct {
	Type               string      `json:"type,omitempty"`
	Name               string      `json:"name"`
	Evidence           EvidenceRef `json:"evidence"`
	ProducerRepository string      `json:"producer_repository,omitempty"`
	ConsumerRepository string      `json:"consumer_repository,omitempty"`
}

// CrossRepository reports whether the contract names a producer and a
// consumer repository that differ (M2-06).
func (c ContractRef) CrossRepository() bool {
	p, k := strings.TrimSpace(c.ProducerRepository), strings.TrimSpace(c.ConsumerRepository)
	return p != "" && k != "" && !strings.EqualFold(p, k)
}

// Source of a criterion.
type Source struct {
	Kind SourceKind `json:"kind"`
	// Quote is the user's words, verbatim; required for user criteria.
	Quote string `json:"quote,omitempty"`
	// Reason is why a derived criterion exists; required for derived.
	Reason string `json:"reason,omitempty"`
}

// Criterion is one acceptance criterion: a behaviour that must hold when
// the work is done, and how that is demonstrated.
type Criterion struct {
	ID             string               `json:"id"` // "AC-<n>"
	Text           string               `json:"text"`
	Source         Source               `json:"source"`
	Files          []FileRef            `json:"files,omitempty"`
	Symbols        []SymbolRef          `json:"symbols,omitempty"`
	Contracts      []ContractRef        `json:"contracts,omitempty"`
	Risk           Risk                 `json:"risk"`
	RequiredChecks []evidence.CheckKind `json:"required_checks,omitempty"`
	// NonCode marks a criterion no code change can satisfy: every task of
	// the plan that cited it was dropped as process or deployment work (it
	// named no repository file). It is exempt from plan ownership and is
	// carried to review as a recorded gap (NonCodeGap) for a human to
	// verify. NonCodeReason says why. Set by the planner (R3); absent on
	// every criterion research writes.
	NonCode       bool   `json:"non_code,omitempty"`
	NonCodeReason string `json:"non_code_reason,omitempty"`
	// Unowned (R8) marks a criterion no task of the plan owns after the
	// repair round and that no task's files cover, so the planner could not
	// re-home it. It travels R3's non-code plumbing (NonCode is set with it)
	// and its gap line says it is unowned (NonCodeGap). Set by the planner
	// only; stripped from every other source with NonCode.
	Unowned bool `json:"unowned,omitempty"`
}

// BriefCriteria is the technical brief's criteria and evidence map.
type BriefCriteria struct {
	SchemaVersion int         `json:"schema_version"`
	Criteria      []Criterion `json:"criteria"`
	// FileEvidence is the best evidence for every path any criterion names.
	FileEvidence map[string]EvidenceRef `json:"file_evidence,omitempty"`
}

// VerificationStep is how one part of a task is shown to be done: a command
// to run or, when there is none, a description of the check.
type VerificationStep struct {
	CheckKind   evidence.CheckKind `json:"check_kind"`
	Command     string             `json:"command,omitempty"`
	Description string             `json:"description,omitempty"`
	// AttachedBy is "planner" when the planner attached the step to satisfy
	// a required check kind (R9: the repository's one whole-suite command).
	// Such a step proves the kind is run, never a criterion specifically.
	// Only the planner sets it; the model's value is cleared on parse.
	AttachedBy string `json:"attached_by,omitempty"`
}

// AttachedByPlanner marks a verification step the planner attached.
const AttachedByPlanner = "planner"

// TaskSpec is what a decomposed task commits to: the criteria it serves, the
// deliverable, how it is verified, and the files it touches with their
// evidence.
type TaskSpec struct {
	SchemaVersion int                `json:"schema_version"`
	CriterionIDs  []string           `json:"criterion_ids"`
	Deliverable   string             `json:"deliverable"`
	Verification  []VerificationStep `json:"verification"`
	Files         []FileRef          `json:"files,omitempty"`
}

var criterionIDRE = regexp.MustCompile(`^AC-[1-9][0-9]*$`)

// ValidCriterionID reports whether id has the form AC-<n>.
func ValidCriterionID(id string) bool { return criterionIDRE.MatchString(id) }

// KnownCheckKind reports whether k is an evidence.CheckKind the evidence
// contract accepts. It asks the evidence package rather than keeping a copy
// of its list, so a kind added there is accepted here without an edit.
func KnownCheckKind(k evidence.CheckKind) bool {
	c := evidence.CheckResult{Kind: k, Status: evidence.CheckNotRun}
	return c.Validate() == nil
}

// KnownEvidenceKind reports whether k is one of the evidence kinds.
func KnownEvidenceKind(k EvidenceKind) bool {
	switch k {
	case EvidenceCodex, EvidenceLSP, EvidenceRepo, EvidenceUser, EvidenceHypothesis, EvidenceNew:
		return true
	}
	return false
}

// Supported reports whether k actually supports an existing path or symbol
// (codex, lsp, repo). hypothesis, new and user do not.
func (k EvidenceKind) Supported() bool {
	return k == EvidenceCodex || k == EvidenceLSP || k == EvidenceRepo
}

// KnownRisk reports whether r is low, medium or high.
func KnownRisk(r Risk) bool {
	return r == RiskLow || r == RiskMedium || r == RiskHigh
}

// Validate checks one evidence ref.
func (e EvidenceRef) Validate() error {
	if !KnownEvidenceKind(e.Kind) {
		return fmt.Errorf("unknown evidence kind %q", e.Kind)
	}
	if e.Confidence < 0 || e.Confidence > 1 {
		return fmt.Errorf("confidence %v is outside 0..1", e.Confidence)
	}
	switch e.Kind {
	case EvidenceCodex, EvidenceLSP, EvidenceRepo:
		if strings.TrimSpace(e.Ref) == "" {
			return fmt.Errorf("%s evidence needs a ref (the tool call id)", e.Kind)
		}
	case EvidenceUser:
		if strings.TrimSpace(e.Ref) == "" {
			return errors.New("user evidence needs a ref (the quote)")
		}
	}
	if e.Basis != "" && e.Basis != BasisSnapshot && e.Basis != BasisClone {
		return fmt.Errorf("unknown basis %q", e.Basis)
	}
	return nil
}

// Validate checks a file ref. A file's evidence is codex, lsp, repo,
// hypothesis or new; user text does not establish that a path exists.
func (f FileRef) Validate() error {
	if strings.TrimSpace(f.Path) == "" {
		return errors.New("file path is empty")
	}
	if err := f.Evidence.Validate(); err != nil {
		return fmt.Errorf("file %s: %w", f.Path, err)
	}
	if f.Evidence.Kind == EvidenceUser {
		return fmt.Errorf("file %s: user evidence cannot support a path; use codex, lsp, repo, hypothesis or new", f.Path)
	}
	return nil
}

// Validate checks a symbol ref.
func (s SymbolRef) Validate() error {
	if strings.TrimSpace(s.Name) == "" {
		return errors.New("symbol name is empty")
	}
	if s.Line < 0 {
		return fmt.Errorf("symbol %s: negative line", s.Name)
	}
	if err := s.Evidence.Validate(); err != nil {
		return fmt.Errorf("symbol %s: %w", s.Name, err)
	}
	if s.Evidence.Kind == EvidenceUser || s.Evidence.Kind == EvidenceNew {
		return fmt.Errorf("symbol %s: %s evidence cannot support a symbol", s.Name, s.Evidence.Kind)
	}
	return nil
}

// Validate checks a contract ref.
func (c ContractRef) Validate() error {
	if strings.TrimSpace(c.Name) == "" {
		return errors.New("contract name is empty")
	}
	if err := c.Evidence.Validate(); err != nil {
		return fmt.Errorf("contract %s: %w", c.Name, err)
	}
	return nil
}

// Validate checks a source.
func (s Source) Validate() error {
	switch s.Kind {
	case SourceUser:
		if strings.TrimSpace(s.Quote) == "" {
			return errors.New("a user criterion needs a quote of the user's words")
		}
	case SourceDerived:
		if strings.TrimSpace(s.Reason) == "" {
			return errors.New("a derived criterion needs a reason")
		}
	default:
		return fmt.Errorf("unknown source kind %q", s.Kind)
	}
	return nil
}

// Validate checks one criterion's structure.
func (c Criterion) Validate() error {
	if !ValidCriterionID(c.ID) {
		return fmt.Errorf("criterion id %q is not of the form AC-<n>", c.ID)
	}
	if strings.TrimSpace(c.Text) == "" {
		return fmt.Errorf("%s: text is empty", c.ID)
	}
	if err := c.Source.Validate(); err != nil {
		return fmt.Errorf("%s: %w", c.ID, err)
	}
	if !KnownRisk(c.Risk) {
		return fmt.Errorf("%s: unknown risk %q", c.ID, c.Risk)
	}
	for _, k := range c.RequiredChecks {
		if !KnownCheckKind(k) {
			return fmt.Errorf("%s: unknown required check %q", c.ID, k)
		}
	}
	for _, f := range c.Files {
		if err := f.Validate(); err != nil {
			return fmt.Errorf("%s: %w", c.ID, err)
		}
	}
	for _, s := range c.Symbols {
		if err := s.Validate(); err != nil {
			return fmt.Errorf("%s: %w", c.ID, err)
		}
	}
	for _, k := range c.Contracts {
		if err := k.Validate(); err != nil {
			return fmt.Errorf("%s: %w", c.ID, err)
		}
	}
	return nil
}

// Validate checks the brief's criteria: the schema version, every criterion,
// unique ids, and the evidence map.
func (b BriefCriteria) Validate() error {
	if b.SchemaVersion != SchemaVersion {
		return fmt.Errorf("schema_version %d is not %d", b.SchemaVersion, SchemaVersion)
	}
	seen := make(map[string]bool, len(b.Criteria))
	for _, c := range b.Criteria {
		if err := c.Validate(); err != nil {
			return err
		}
		if seen[c.ID] {
			return fmt.Errorf("duplicate criterion id %s", c.ID)
		}
		seen[c.ID] = true
	}
	for path, ev := range b.FileEvidence {
		if err := (FileRef{Path: path, Evidence: ev}).Validate(); err != nil {
			return fmt.Errorf("file_evidence: %w", err)
		}
	}
	return nil
}

// Validate checks a verification step.
func (v VerificationStep) Validate() error {
	if !KnownCheckKind(v.CheckKind) {
		return fmt.Errorf("unknown check kind %q", v.CheckKind)
	}
	if strings.TrimSpace(v.Command) == "" && strings.TrimSpace(v.Description) == "" {
		return fmt.Errorf("%s verification needs a command or a description", v.CheckKind)
	}
	return nil
}

// Validate checks a task spec's structure. An empty CriterionIDs is allowed
// here (the task is then a hypothesis); whether every task cites a known
// criterion is a plan rule (M2-05).
func (t TaskSpec) Validate() error {
	if t.SchemaVersion != SchemaVersion {
		return fmt.Errorf("schema_version %d is not %d", t.SchemaVersion, SchemaVersion)
	}
	seen := make(map[string]bool, len(t.CriterionIDs))
	for _, id := range t.CriterionIDs {
		if !ValidCriterionID(id) {
			return fmt.Errorf("criterion id %q is not of the form AC-<n>", id)
		}
		if seen[id] {
			return fmt.Errorf("criterion id %s cited twice", id)
		}
		seen[id] = true
	}
	for i, v := range t.Verification {
		if err := v.Validate(); err != nil {
			return fmt.Errorf("verification[%d]: %w", i, err)
		}
	}
	for _, f := range t.Files {
		if err := f.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// ParseBriefCriteria decodes and validates a BriefCriteria.
func ParseBriefCriteria(data []byte) (*BriefCriteria, error) {
	var b BriefCriteria
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, fmt.Errorf("decode criteria: %w", err)
	}
	if err := b.Validate(); err != nil {
		return nil, err
	}
	return &b, nil
}

// ParseTaskSpec decodes and validates a TaskSpec.
func ParseTaskSpec(data []byte) (*TaskSpec, error) {
	var t TaskSpec
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("decode task spec: %w", err)
	}
	if err := t.Validate(); err != nil {
		return nil, err
	}
	return &t, nil
}

// NormalizeText is the dedupe key for criterion text: lower case, letters
// and digits only, single spaces.
func NormalizeText(s string) string {
	var b strings.Builder
	space := false
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			space = false
			b.WriteRune(r)
			continue
		}
		space = true
	}
	return b.String()
}

// EvidenceRank orders evidence by strength for merging: supported kinds
// (codex, lsp, repo) over new over hypothesis/user; ties go to confidence.
func EvidenceRank(e EvidenceRef) int {
	switch e.Kind {
	case EvidenceRepo, EvidenceLSP, EvidenceCodex:
		return 3
	case EvidenceNew:
		return 1
	}
	return 0
}

// Better reports whether a is stronger evidence than b.
func Better(a, b EvidenceRef) bool {
	ra, rb := EvidenceRank(a), EvidenceRank(b)
	if ra != rb {
		return ra > rb
	}
	return a.Confidence > b.Confidence
}

// BuildFileEvidence is the best evidence per path across criteria; nil when
// no criterion names a file.
func BuildFileEvidence(criteria []Criterion) map[string]EvidenceRef {
	out := map[string]EvidenceRef{}
	for _, c := range criteria {
		for _, f := range c.Files {
			if cur, ok := out[f.Path]; !ok || Better(f.Evidence, cur) {
				out[f.Path] = f.Evidence
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// EvidenceLabel renders evidence compactly for prose: "repo call_7 (clone)",
// "hypothesis — not found in the clone".
func EvidenceLabel(e EvidenceRef) string {
	s := string(e.Kind)
	if e.Ref != "" && e.Kind != EvidenceUser {
		s += " " + e.Ref
	}
	if e.Basis != "" {
		s += " (" + e.Basis + ")"
	}
	if e.Note != "" {
		s += " — " + e.Note
	}
	return s
}

// CheckList renders check kinds as "test, build".
func CheckList(ks []evidence.CheckKind) string {
	parts := make([]string, 0, len(ks))
	for _, k := range ks {
		parts = append(parts, string(k))
	}
	return strings.Join(parts, ", ")
}
