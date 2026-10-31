package main

// Acceptance criteria pass (M2-04). After the research tool loop, one more
// LLM call (no tools) turns the user's request, the PRD and what the loop
// found into plan.BriefCriteria. The only acceptable evidence for a path,
// symbol or contract is a result of this task's own tool calls — codex_*,
// the LSP wrappers, list_files/read_file — cited by tool call id. The output
// is then grounded deterministically:
//
//   - a citation that names no such call, an errored call, or a call of the
//     wrong class is not evidence;
//   - existence of a path is decided by os.Stat in the tree research read
//     (the snapshot worktree once M2-03 provides one; the clone until then,
//     and for a legacy-unpinned item). A path that exists without a valid
//     codex/lsp/repo citation becomes repo evidence with ref "stat"; a path
//     that does not exist is "new" only when the model said so, otherwise a
//     hypothesis. Nothing is dropped.
//   - a "user" criterion whose quote is not in the request or PRD becomes
//     "derived" with that reason.
//
// Output that does not parse or validate (unknown kinds included) is
// re-prompted once with the error; a second failure fails the research task
// with the parse error, and the coordinator blocks the item on it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
	"github.com/autonomousengine/autonomous-engine/internal/modeljson"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/plan"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// criteriaRoot is a tree paths are stat'ed in, and what kind of tree it is.
type criteriaRoot struct {
	Path  string
	Basis string // plan.BasisSnapshot or plan.BasisClone
}

// criteriaRootsFor is every repository path this task researched. They are
// the shared clones (basis "clone") until M2-03 hands research a snapshot
// worktree, at which point the caller passes those paths with
// plan.BasisSnapshot instead.
func criteriaRootsFor(repoPaths []string) []criteriaRoot {
	out := make([]criteriaRoot, 0, len(repoPaths))
	for _, p := range repoPaths {
		if strings.TrimSpace(p) == "" {
			continue
		}
		out = append(out, criteriaRoot{Path: p, Basis: plan.BasisClone})
	}
	return out
}

// lspTools are the language-server wrappers (codex_lsp_tools.go).
var lspTools = map[string]bool{
	"codex_find_references":   true,
	"codex_goto_definition":   true,
	"codex_workspace_symbols": true,
	"codex_document_symbols":  true,
	"codex_type_info":         true,
	"codex_diagnostics":       true,
}

// evidenceClassOf is the evidence kind a tool's result can support, or ""
// when the tool is not an acceptable source of evidence.
func evidenceClassOf(tool string) plan.EvidenceKind {
	switch {
	case lspTools[tool]:
		return plan.EvidenceLSP
	case strings.HasPrefix(tool, "codex_"):
		return plan.EvidenceCodex
	case tool == "read_file" || tool == "list_files":
		return plan.EvidenceRepo
	}
	return ""
}

// catalogueEntry is one tool result the model may cite.
type catalogueEntry struct {
	ID      string // unique positional id, "c<n>" (n = 1-based position in the history)
	RawID   string // the provider's tool call id, which need not be unique
	Tool    string
	Kind    plan.EvidenceKind
	Args    string
	Preview string
}

// evidenceCatalogue indexes the citable tool results.
//
// Providers do not guarantee unique tool call ids across a loop: Ollama
// numbers them per turn (call_0, call_1, ...), so the same raw id names
// several calls. Every call therefore gets a unique positional id "c<n>",
// which is what the prompt shows and what EvidenceRef.Ref records. A raw
// provider id is accepted only when it names exactly one call in the whole
// history; an ambiguous raw id is rejected.
type evidenceCatalogue struct {
	list  []catalogueEntry
	byID  map[string]catalogueEntry   // c<n> → entry
	byRaw map[string][]catalogueEntry // raw id → every citable entry with it
	raw   map[string]int              // raw id → number of calls (any tool) with it
}

// positionalID is the unique citation id of the call at index i.
func positionalID(i int) string { return fmt.Sprintf("c%d", i+1) }

// buildEvidenceCatalogue lists the successful evidence-class tool calls.
func buildEvidenceCatalogue(calls []toolCallRecord) *evidenceCatalogue {
	cat := &evidenceCatalogue{
		byID:  map[string]catalogueEntry{},
		byRaw: map[string][]catalogueEntry{},
		raw:   map[string]int{},
	}
	for i, c := range calls {
		raw := strings.TrimSpace(c.callID)
		if raw != "" {
			cat.raw[raw]++
		}
		kind := evidenceClassOf(c.tool)
		if kind == "" || c.isError {
			continue
		}
		e := catalogueEntry{
			ID:      positionalID(i),
			RawID:   raw,
			Tool:    c.tool,
			Kind:    kind,
			Args:    truncate(strings.TrimSpace(c.arguments), 200),
			Preview: truncate(strings.Join(strings.Fields(c.result), " "), 240),
		}
		cat.byID[e.ID] = e
		if raw != "" {
			cat.byRaw[raw] = append(cat.byRaw[raw], e)
		}
		cat.list = append(cat.list, e)
	}
	return cat
}

// resolve finds the entry a citation names: a positional id, or a raw
// provider id that names exactly one call. problem is "" on success.
func (cat *evidenceCatalogue) resolve(ref string) (catalogueEntry, string) {
	ref = strings.TrimSpace(ref)
	if e, ok := cat.byID[ref]; ok {
		return e, ""
	}
	if n := cat.raw[ref]; n > 1 {
		return catalogueEntry{}, fmt.Sprintf("cited %q is ambiguous: the provider reused that id for %d tool calls; cite the catalogue id (c<n>)", ref, n)
	}
	if es := cat.byRaw[ref]; len(es) == 1 {
		return es[0], ""
	}
	return catalogueEntry{}, fmt.Sprintf("cited %q is not a successful tool result of this research task", ref)
}

// criteriaInput is what the criteria pass reads.
type criteriaInput struct {
	Request  string // the work item's title and description: the user's words
	PRD      string // the research task's starting context (PRD + guidance)
	Findings string // the research loop's final answer
	Calls    []toolCallRecord
	Roots    []criteriaRoot
	// LogInference records the call (inference_logs); nil in tests.
	LogInference func(resp *adapter.CompletionResponse, latency time.Duration, err error)
}

const maxCatalogueEntries = 80

const criteriaSystemPrompt = `You extract ACCEPTANCE CRITERIA for a software change. A criterion is an observable behaviour that must hold when the work is done. You do not plan or implement.

Return ONE JSON object and nothing else (no markdown fences, no prose):
{"schema_version":1,"criteria":[{"id":"AC-1","text":"...","source":{"kind":"user","quote":"..."},"files":[{"path":"...","evidence":{"kind":"repo","ref":"<tool call id>","confidence":0.9}}],"symbols":[{"name":"...","file":"...","line":12,"evidence":{"kind":"lsp","ref":"<tool call id>","confidence":0.8}}],"contracts":[{"type":"rest_openapi","name":"POST /api/x","evidence":{"kind":"codex","ref":"<tool call id>","confidence":0.7}}],"risk":"medium","required_checks":["test"]}]}

RULES:
1. ids are AC-1, AC-2, ... in order, unique.
2. source.kind is "user" or "derived". A "user" criterion MUST carry "quote": the user's words copied EXACTLY from the REQUEST or the PRD below. Anything you infer (from the codebase or from good practice) is "derived" and MUST carry "reason".
3. Every path you name that already exists MUST cite evidence from the EVIDENCE CATALOGUE below: "ref" is the catalogue id (c<n>, exactly as listed) and "kind" is that entry's kind (codex, lsp or repo). The catalogue is the ONLY acceptable evidence; never invent an id.
4. A path you believe exists but cannot cite: evidence kind "hypothesis" (no ref). A file the change must create: evidence kind "new" (no ref). Do not leave a path out because it is unproven — mark it.
5. Allowed values, exactly: evidence.kind codex|lsp|repo|hypothesis|new; risk low|medium|high; required_checks from %s. confidence is a number from 0 to 1.
6. Paths are relative to the repository root.
7. At least one criterion. Prefer few, precise criteria over many vague ones.`

// buildCriteriaPrompt returns the system and user messages for the pass.
func buildCriteriaPrompt(in criteriaInput, catalogue []catalogueEntry) (string, string) {
	checks := []string{
		string(evidence.CheckKindTest), string(evidence.CheckKindBuild), string(evidence.CheckKindLint),
		string(evidence.CheckKindDiagnostic), string(evidence.CheckKindOther),
	}
	system := fmt.Sprintf(criteriaSystemPrompt, strings.Join(checks, "|"))

	var b strings.Builder
	b.WriteString("## REQUEST (the user's words)\n")
	b.WriteString(truncate(strings.TrimSpace(in.Request), 4000))
	b.WriteString("\n\n## PRD\n")
	b.WriteString(truncate(strings.TrimSpace(in.PRD), 8000))
	b.WriteString("\n\n## RESEARCH FINDINGS\n")
	b.WriteString(truncate(strings.TrimSpace(in.Findings), 6000))
	b.WriteString("\n\n## EVIDENCE CATALOGUE (cite by id)\n")
	if len(catalogue) == 0 {
		b.WriteString("(no tool results — every existing path is a hypothesis)\n")
	}
	shown := catalogue
	if len(shown) > maxCatalogueEntries {
		shown = shown[len(shown)-maxCatalogueEntries:]
	}
	for _, e := range shown {
		fmt.Fprintf(&b, "- id=%s kind=%s tool=%s args=%s\n  result: %s\n", e.ID, e.Kind, e.Tool, e.Args, e.Preview)
	}
	b.WriteString("\nReturn the JSON object now.")
	return system, b.String()
}

// parseCriteriaOutput decodes and structurally validates the model's JSON.
// A missing schema_version is taken as 1; every enumerated value must be
// known; at least one criterion is required.
func parseCriteriaOutput(content string) (*plan.BriefCriteria, error) {
	raw := extractJSONObject(content)
	if raw == "" {
		return nil, errors.New("no JSON object in the response")
	}
	var bc plan.BriefCriteria
	if err := json.Unmarshal([]byte(raw), &bc); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	if bc.SchemaVersion == 0 {
		bc.SchemaVersion = plan.SchemaVersion
	}
	// R3: only the planner marks a criterion non-code; research output that
	// claims it would exempt the criterion from plan ownership.
	plan.StripNonCode(&bc)
	if len(bc.Criteria) == 0 {
		return nil, errors.New("no criteria")
	}
	if err := bc.Validate(); err != nil {
		return nil, err
	}
	return &bc, nil
}

// extractJSONObject strips reasoning tags and returns the first balanced
// top-level {...} that is valid JSON, ignoring prose, code fences and
// anything after it (live run attempt 6: a stray trailing "}" cost a
// re-prompt). With no valid object it returns the first candidate so the
// decode error names the real problem.
func extractJSONObject(s string) string {
	return modeljson.FirstObject(modeljson.StripThinking(s))
}

// cleanRepoPath makes p relative to a root and rejects a path that escapes
// it. ok=false: the path is not inside any repository.
func cleanRepoPath(p string, roots []criteriaRoot) (string, bool) {
	p = strings.TrimSpace(strings.ReplaceAll(p, "\\", "/"))
	if p == "" {
		return "", false
	}
	for _, r := range roots {
		root := strings.TrimRight(strings.ReplaceAll(r.Path, "\\", "/"), "/")
		if root != "" && strings.HasPrefix(p, root+"/") {
			p = strings.TrimPrefix(p, root+"/")
			break
		}
	}
	p = strings.TrimPrefix(p, "./")
	c := filepath.ToSlash(filepath.Clean(filepath.FromSlash(p)))
	if c == "." || strings.HasPrefix(c, "../") || c == ".." || strings.HasPrefix(c, "/") || filepath.IsAbs(c) {
		return "", false
	}
	return c, true
}

// statInRoots reports the first root in which rel exists.
func statInRoots(rel string, roots []criteriaRoot) (criteriaRoot, bool) {
	for _, r := range roots {
		if _, err := os.Stat(filepath.Join(r.Path, filepath.FromSlash(rel))); err == nil {
			return r, true
		}
	}
	return criteriaRoot{}, false
}

// checkCitation resolves ev's citation. It returns the canonical
// (positional) id when ev cites a catalogue entry of its own kind, otherwise
// why the citation does not count.
func checkCitation(ev plan.EvidenceRef, cat *evidenceCatalogue) (string, string) {
	e, problem := cat.resolve(ev.Ref)
	if problem != "" {
		return "", problem
	}
	if e.Kind != ev.Kind {
		return "", fmt.Sprintf("cited %s is a %s result (%s), not %s", e.ID, e.Kind, e.Tool, ev.Kind)
	}
	return e.ID, ""
}

// parentDirExists reports whether rel's directory exists in any root.
func parentDirExists(rel string, roots []criteriaRoot) bool {
	dir := filepath.Dir(filepath.FromSlash(rel))
	if dir == "." {
		return true
	}
	for _, r := range roots {
		if st, err := os.Stat(filepath.Join(r.Path, dir)); err == nil && st.IsDir() {
			return true
		}
	}
	return false
}

// newFileNote is the grounding note of a new path whose directory is absent.
func newFileNote(rel string) string {
	return fmt.Sprintf("grounding: directory %s does not exist; a file to be created there must be justified", filepath.ToSlash(filepath.Dir(rel)))
}

func joinNotes(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + "; " + b
}

func basisLabel(b string) string {
	if b == plan.BasisSnapshot {
		return "snapshot worktree"
	}
	return "clone"
}

// groundFile applies the evidence rules to one path.
func groundFile(f plan.FileRef, cat *evidenceCatalogue, roots []criteriaRoot) plan.FileRef {
	ev := f.Evidence
	path, ok := cleanRepoPath(f.Path, roots)
	if !ok {
		return plan.FileRef{Path: f.Path, Evidence: plan.EvidenceRef{Kind: plan.EvidenceHypothesis, Note: "path is not inside a researched repository"}}
	}
	note := ev.Note
	cited := ev.Kind.Supported()
	if cited {
		id, problem := checkCitation(ev, cat)
		if problem != "" {
			note = joinNotes(note, problem)
			cited = false
		} else {
			ev.Ref = id
		}
	}
	if len(roots) == 0 {
		// Nothing to stat in: only a valid citation supports the path.
		if cited {
			return plan.FileRef{Path: path, Evidence: plan.EvidenceRef{Kind: ev.Kind, Ref: ev.Ref, Confidence: ev.Confidence, Note: note}}
		}
		if ev.Kind == plan.EvidenceNew {
			return plan.FileRef{Path: path, Evidence: plan.EvidenceRef{Kind: plan.EvidenceNew, Confidence: ev.Confidence, Note: note}}
		}
		return plan.FileRef{Path: path, Evidence: plan.EvidenceRef{Kind: plan.EvidenceHypothesis, Note: joinNotes(note, "no repository tree to check")}}
	}
	root, exists := statInRoots(path, roots)
	switch {
	case exists && cited:
		out := plan.EvidenceRef{Kind: ev.Kind, Ref: ev.Ref, Confidence: ev.Confidence, Note: note}
		if ev.Kind == plan.EvidenceRepo {
			out.Basis = root.Basis
		}
		return plan.FileRef{Path: path, Evidence: out}
	case exists:
		if ev.Kind == plan.EvidenceNew {
			note = joinNotes(note, "proposed as new but it already exists")
		}
		return plan.FileRef{Path: path, Evidence: plan.EvidenceRef{Kind: plan.EvidenceRepo, Ref: plan.RefStat, Confidence: 1, Basis: root.Basis, Note: note}}
	case ev.Kind == plan.EvidenceNew:
		if !parentDirExists(path, roots) {
			note = joinNotes(note, newFileNote(path))
		}
		return plan.FileRef{Path: path, Evidence: plan.EvidenceRef{Kind: plan.EvidenceNew, Confidence: ev.Confidence, Note: note}}
	default:
		return plan.FileRef{Path: path, Evidence: plan.EvidenceRef{Kind: plan.EvidenceHypothesis, Note: joinNotes(note, "not found in the "+basisLabel(roots[0].Basis))}}
	}
}

// groundCitation keeps a supported citation that checks out and turns any
// other into a hypothesis with the reason.
func groundCitation(ev plan.EvidenceRef, cat *evidenceCatalogue) plan.EvidenceRef {
	if !ev.Kind.Supported() {
		return plan.EvidenceRef{Kind: plan.EvidenceHypothesis, Note: ev.Note}
	}
	id, problem := checkCitation(ev, cat)
	if problem != "" {
		return plan.EvidenceRef{Kind: plan.EvidenceHypothesis, Note: joinNotes(ev.Note, problem)}
	}
	ev.Ref = id
	return ev
}

// quoteFound reports whether quote appears, modulo case/punctuation/space,
// in any of texts.
func quoteFound(quote string, texts ...string) bool {
	q := plan.NormalizeText(quote)
	if q == "" {
		return false
	}
	for _, t := range texts {
		if strings.Contains(plan.NormalizeText(t), q) {
			return true
		}
	}
	return false
}

// groundCriteria applies every rule in place and rebuilds FileEvidence. It
// returns one note per change, for the log.
func groundCriteria(bc *plan.BriefCriteria, in criteriaInput, cat *evidenceCatalogue) []string {
	var notes []string
	for i := range bc.Criteria {
		c := &bc.Criteria[i]
		if c.Source.Kind == plan.SourceUser && !quoteFound(c.Source.Quote, in.Request, in.PRD) {
			c.Source = plan.Source{
				Kind:   plan.SourceDerived,
				Quote:  c.Source.Quote,
				Reason: "quoted text was not found verbatim in the request or the PRD",
			}
			notes = append(notes, fmt.Sprintf("%s: quote not found — source set to derived", c.ID))
		}
		seen := map[string]bool{}
		files := make([]plan.FileRef, 0, len(c.Files))
		for _, f := range c.Files {
			g := groundFile(f, cat, in.Roots)
			if g.Evidence.Kind != f.Evidence.Kind || g.Evidence.Ref != f.Evidence.Ref {
				notes = append(notes, fmt.Sprintf("%s: %s %s → %s", c.ID, f.Path, f.Evidence.Kind, plan.EvidenceLabel(g.Evidence)))
			}
			if seen[g.Path] {
				continue
			}
			seen[g.Path] = true
			files = append(files, g)
		}
		c.Files = files
		for j := range c.Symbols {
			before := c.Symbols[j].Evidence
			c.Symbols[j].Evidence = groundCitation(before, cat)
			if c.Symbols[j].Evidence.Kind != before.Kind {
				notes = append(notes, fmt.Sprintf("%s: symbol %s → %s", c.ID, c.Symbols[j].Name, plan.EvidenceLabel(c.Symbols[j].Evidence)))
			}
		}
		for j := range c.Contracts {
			before := c.Contracts[j].Evidence
			c.Contracts[j].Evidence = groundCitation(before, cat)
			if c.Contracts[j].Evidence.Kind != before.Kind {
				notes = append(notes, fmt.Sprintf("%s: contract %s → %s", c.ID, c.Contracts[j].Name, plan.EvidenceLabel(c.Contracts[j].Evidence)))
			}
		}
	}
	bc.FileEvidence = plan.BuildFileEvidence(bc.Criteria)
	sort.Strings(notes)
	return notes
}

// extractBriefCriteria runs the criteria pass: one call, one re-prompt with
// the rejection reason, then an error carrying the parse/validation failure.
func extractBriefCriteria(ctx context.Context, llmAdapter adapter.ModelAdapter, in criteriaInput) (*plan.BriefCriteria, []string, error) {
	cat := buildEvidenceCatalogue(in.Calls)
	system, user := buildCriteriaPrompt(in, cat.list)
	msgs := []adapter.ChatMessage{
		{Role: adapter.RoleSystem, Content: system},
		{Role: adapter.RoleUser, Content: user},
	}
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		start := time.Now()
		resp, err := llmAdapter.Complete(ctx, adapter.CompletionRequest{
			Messages:    msgs,
			MaxTokens:   4096,
			Temperature: 0,
		})
		if in.LogInference != nil {
			in.LogInference(resp, time.Since(start), err)
		}
		if err != nil {
			return nil, nil, fmt.Errorf("acceptance criteria LLM call failed: %w", err)
		}
		content := ""
		if resp != nil {
			content = resp.Content
		}
		bc, perr := parseCriteriaOutput(content)
		if perr == nil {
			notes := groundCriteria(bc, in, cat)
			if verr := bc.Validate(); verr != nil {
				perr = verr
			} else {
				return bc, notes, nil
			}
		}
		lastErr = perr
		msgs = append(msgs,
			adapter.ChatMessage{Role: adapter.RoleAssistant, Content: content},
			adapter.ChatMessage{Role: adapter.RoleUser, Content: fmt.Sprintf(
				"Your output was rejected: %v. Return the corrected JSON object only, following every rule.", perr)},
		)
	}
	return nil, nil, fmt.Errorf("acceptance criteria output rejected after one re-prompt: %w", lastErr)
}

// inferenceLogger is the pool's Exec and QueryRow: the inference_logs row,
// and the work item's cumulative token use.
type inferenceLogger interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// workItemTokenBudget is the tool loop's per-work-item cap
// (WORK_ITEM_TOKEN_BUDGET, default 500K; internal/llm/tools RunToolLoop).
func workItemTokenBudget() int64 {
	if s := os.Getenv("WORK_ITEM_TOKEN_BUDGET"); s != "" {
		if v, err := strconv.ParseInt(s, 10, 64); err == nil && v > 0 {
			return v
		}
	}
	return 500_000
}

// criteriaBudgetExhausted reports whether the work item has used its token
// budget: the same sum the tool loop checks (inference_logs input+output
// tokens). A read error is treated as not exhausted, as in the tool loop.
func criteriaBudgetExhausted(ctx context.Context, db inferenceLogger, workID uuid.UUID) (bool, int64, int64) {
	budget := workItemTokenBudget()
	var used int64
	_ = db.QueryRow(ctx,
		`SELECT COALESCE(SUM(input_tokens + output_tokens), 0) FROM inference_logs WHERE work_item_id = $1`,
		workID,
	).Scan(&used)
	return used >= budget, used, budget
}

// runCriteriaPass is the research handler's single call site: it assembles
// the input from the task, the work item and the tool history, records the
// inference, and logs every grounding change.
func runCriteriaPass(
	ctx context.Context,
	db inferenceLogger,
	llmAdapter adapter.ModelAdapter,
	workID uuid.UUID,
	wi *models.WorkItem,
	task researchTaskMessage,
	repoPaths []string,
	findings string,
	calls []toolCallRecord,
) (*plan.BriefCriteria, error) {
	request := ""
	if wi != nil {
		request = strings.TrimSpace(wi.Title + "\n\n" + wi.Description)
	}
	in := criteriaInput{
		Request:  request,
		PRD:      task.StartingContext,
		Findings: findings,
		Calls:    calls,
		Roots:    criteriaRootsFor(repoPaths),
	}
	if db != nil {
		if exhausted, used, budget := criteriaBudgetExhausted(ctx, db, workID); exhausted {
			log.Printf("research-agent: task_id=%s work_id=%s: skipping the acceptance criteria pass — work item token budget exhausted (%d of %d tokens used, WORK_ITEM_TOKEN_BUDGET); the result carries no criteria",
				task.TaskID, workID, used, budget)
			return nil, nil
		}
		in.LogInference = func(resp *adapter.CompletionResponse, latency time.Duration, err error) {
			var inTok, outTok int
			model := ""
			if resp != nil {
				inTok, outTok, model = resp.InputTokens, resp.OutputTokens, resp.Model
			}
			errMsg := ""
			if err != nil {
				errMsg = err.Error()
			}
			if _, lerr := db.Exec(ctx,
				`INSERT INTO inference_logs (model_provider, model_id, intent_type, input_tokens, output_tokens, latency_ms, success, error_message, work_item_id, created_at)
				 VALUES ($1, $2, 'extract_criteria', $3, $4, $5, $6, $7, $8, NOW())`,
				llmAdapter.Name(), model, inTok, outTok, latency.Milliseconds(), err == nil, errMsg, workID,
			); lerr != nil {
				log.Printf("research-agent: record criteria inference: %v", lerr)
			}
		}
	}
	bc, notes, err := extractBriefCriteria(ctx, llmAdapter, in)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(bc.Criteria))
	for _, c := range bc.Criteria {
		ids = append(ids, c.ID)
	}
	log.Printf("research-agent: task_id=%s work_id=%s: %d acceptance criteria %v, %d evidenced paths", task.TaskID, workID, len(bc.Criteria), ids, len(bc.FileEvidence))
	for _, n := range notes {
		log.Printf("research-agent: task_id=%s criteria grounding: %s", task.TaskID, n)
	}
	return bc, nil
}
