package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/gitremote"
	"github.com/autonomousengine/autonomous-engine/internal/guards"
	"github.com/autonomousengine/autonomous-engine/internal/interactions"
	"github.com/autonomousengine/autonomous-engine/internal/products"
	"github.com/autonomousengine/autonomous-engine/internal/snapshot"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// M2-03 base drift check. The GitHub PR's base stays the branch (GitHub
// needs a ref), but before each PR is opened pr-coordinator compares the
// branch's current tip on origin with the work item's pin:
//
//   - equal: proceed;
//   - moved: record a drift_detected event (old = pin, new = tip) and run
//     `git merge-tree --write-tree <new_tip> <head>`:
//   - no conflicts: proceed; the PR description and pr.ready say the base
//     moved and the change merges cleanly (pr.ready.base_drift);
//   - conflicts: block the parent with the conflicting paths in the reason
//     and raise the escalation prompt in the same transaction. No automatic
//     rebase or re-research: the block is the human decision point.
//
// Nothing is written to the shared clone (pr-coordinator mounts repo-cache
// read-only): the tip is read with ls-remote against the clone's origin (the
// snapshot.Resolver read path), and the fetch and merge run in a scratch bare
// repository under the temp dir whose object store borrows the clone's
// objects through objects/info/alternates.

// baseDrift is pr.ready.base_drift: the base branch moved from Old (the
// pin) to New after the work item was pinned. Only a clean merge reaches
// pr.ready; a conflicting one blocks instead.
type baseDrift struct {
	Old   string `json:"old"`
	New   string `json:"new"`
	Clean bool   `json:"clean"`
}

// driftResult is what the check found for one repository.
type driftResult struct {
	Old, New  string
	Moved     bool
	Clean     bool
	Conflicts []string
}

// driftChecker evaluates base drift for one PR. Run defaults to real git;
// TempDir to os.TempDir().
type driftChecker struct {
	Run     snapshot.Runner
	TempDir string
	Timeout time.Duration
}

func (d *driftChecker) git(ctx context.Context, args ...string) (string, error) {
	timeout := d.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	run := d.Run
	if run == nil {
		run = snapshot.GitRunner
	}
	return run(cctx, args...)
}

// Check compares origin's baseRef tip with pin and, when it moved, whether
// head still merges onto the new tip. branch is the PR head branch, fetched
// into the scratch repository when the clone lacks head.
func (d *driftChecker) Check(ctx context.Context, repoName, clonePath, baseRef, pin, head, branch string) (driftResult, error) {
	res := driftResult{Old: pin}
	if !snapshot.ValidSHA(pin) {
		return res, fmt.Errorf("pin %q is not a full commit id", pin)
	}
	if clonePath == "" {
		return res, errors.New("repository has no clone path")
	}
	resolver := &snapshot.Resolver{Run: d.Run, Timeout: d.Timeout}
	tip, err := resolver.Resolve(ctx, products.Repository{Name: repoName, ClonePath: clonePath, BaseBranch: baseRef})
	if err != nil {
		return res, fmt.Errorf("read origin %s: %w", baseRef, err)
	}
	res.New = tip
	if tip == pin {
		return res, nil
	}
	res.Moved = true

	origin, err := d.git(ctx, "-C", clonePath, "config", "--get", "remote.origin.url")
	origin = strings.TrimSpace(origin)
	if err != nil || origin == "" || strings.HasPrefix(origin, "-") {
		return res, fmt.Errorf("read the clone's origin: %v", err)
	}
	objects, err := d.git(ctx, "-C", clonePath, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return res, fmt.Errorf("locate the clone's object store: %w", err)
	}
	scratch, err := os.MkdirTemp(d.TempDir, "ae-drift-")
	if err != nil {
		return res, fmt.Errorf("scratch repository: %w", err)
	}
	defer os.RemoveAll(scratch)
	if _, err := d.git(ctx, "init", "--bare", "--quiet", scratch); err != nil {
		return res, fmt.Errorf("init scratch repository: %w", err)
	}
	alt := filepath.Join(strings.TrimSpace(objects), "objects")
	if err := os.MkdirAll(filepath.Join(scratch, "objects", "info"), 0o755); err != nil {
		return res, err
	}
	if err := os.WriteFile(filepath.Join(scratch, "objects", "info", "alternates"), []byte(filepath.ToSlash(alt)+"\n"), 0o644); err != nil {
		return res, err
	}
	if _, err := d.git(ctx, "-C", scratch, "fetch", "--quiet", "--no-tags", "--", origin, "+refs/heads/"+baseRef+":refs/ae/base"); err != nil {
		return res, fmt.Errorf("fetch origin %s into the scratch repository: %s", baseRef, redactOrigin(err.Error(), origin))
	}
	headRef := head
	if head == "" || !d.has(ctx, scratch, head) {
		if branch == "" {
			return res, fmt.Errorf("head %s is not available and no branch is known to fetch", head)
		}
		if _, err := d.git(ctx, "-C", scratch, "fetch", "--quiet", "--no-tags", "--", origin, "+refs/heads/"+branch+":refs/ae/head"); err != nil {
			return res, fmt.Errorf("fetch %s into the scratch repository: %s", branch, redactOrigin(err.Error(), origin))
		}
		switch {
		case head == "":
			headRef = "refs/ae/head"
		case !d.has(ctx, scratch, head):
			return res, fmt.Errorf("verified head %s is not on origin %s", head, branch)
		}
	}
	if !d.has(ctx, scratch, tip) {
		return res, fmt.Errorf("origin %s tip %s is not available after fetch", baseRef, tip)
	}
	res.Clean, res.Conflicts, err = d.mergeTree(ctx, scratch, pin, tip, headRef)
	return res, err
}

func (d *driftChecker) has(ctx context.Context, dir, rev string) bool {
	_, err := d.git(ctx, "-C", dir, "cat-file", "-e", rev+"^{commit}")
	return err == nil
}

var treeLine = regexp.MustCompile(`^[0-9a-f]{40}$`)

// mergeTree merges head onto tip without a working tree. Exit 0 is clean;
// exit 1 lists the conflicted paths after the tree id. Git older than 2.38
// lacks --write-tree; the trivial three-way form (merge-tree base a b) is
// parsed instead.
func (d *driftChecker) mergeTree(ctx context.Context, dir, base, tip, head string) (bool, []string, error) {
	out, err := d.git(ctx, "-C", dir, "merge-tree", "--write-tree", "--name-only", "--no-messages", tip, head)
	lines := splitNonEmpty(out)
	if len(lines) > 0 && treeLine.MatchString(lines[0]) {
		if err == nil {
			return true, nil, nil
		}
		return false, dedupe(lines[1:]), nil
	}
	if err != nil && !strings.Contains(err.Error(), "write-tree") && !strings.Contains(err.Error(), "unknown option") && !strings.Contains(err.Error(), "usage:") {
		return false, nil, fmt.Errorf("git merge-tree: %w", err)
	}
	legacy, lerr := d.git(ctx, "-C", dir, "merge-tree", base, tip, head)
	if lerr != nil {
		return false, nil, fmt.Errorf("git merge-tree (legacy form): %w", lerr)
	}
	conflicts := legacyMergeTreeConflicts(legacy)
	return len(conflicts) == 0, conflicts, nil
}

// legacyMergeTreeConflicts parses `git merge-tree <base> <a> <b>` output: a
// section header ("changed in both", "added in both", ...), indented
// "<role> <mode> <sha> <path>" lines, then a diff; a conflict marker in the
// diff makes the section's path a conflict.
func legacyMergeTreeConflicts(out string) []string {
	var conflicts []string
	var paths []string
	conflicted := false
	flush := func() {
		if conflicted {
			conflicts = append(conflicts, paths...)
		}
		paths, conflicted = nil, false
	}
	for _, line := range strings.Split(out, "\n") {
		switch {
		case line != "" && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "@") &&
			!strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "-") && strings.Contains(line, " in "):
			flush()
		case strings.HasPrefix(line, "  "):
			f := strings.Fields(line)
			if len(f) >= 4 {
				paths = append(paths, f[len(f)-1])
			}
		case strings.Contains(line, "<<<<<<<"):
			conflicted = true
		}
	}
	flush()
	return dedupe(conflicts)
}

func splitNonEmpty(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// redactOrigin strips the origin URL (it may embed a credential) from a git
// error.
func redactOrigin(msg, origin string) string {
	if origin == "" {
		return msg
	}
	return strings.ReplaceAll(msg, origin, "<origin>")
}

// driftEvent is the drift_detected row for a moved base.
func driftEvent(workID, repoID uuid.UUID, d driftResult, head, branch string) snapshot.Event {
	detail, _ := json.Marshal(map[string]any{
		"source":    "pr-coordinator",
		"branch":    branch,
		"head_sha":  head,
		"clean":     d.Clean,
		"conflicts": d.Conflicts,
	})
	return snapshot.Event{
		WorkItemID: workID, RepositoryID: repoID, Kind: snapshot.EventDriftDetected,
		OldSHA: d.Old, NewSHA: d.New, Detail: string(detail),
	}
}

// unevaluatedDriftEvent is the drift_detected row for a base that moved but
// whose merge check could not run (scratch fetch or merge-tree failed).
func unevaluatedDriftEvent(in driftInput, d driftResult, reason string) snapshot.Event {
	detail, _ := json.Marshal(map[string]any{
		"source":    "pr-coordinator",
		"branch":    in.Branch,
		"head_sha":  in.HeadSHA,
		"evaluated": false,
		"error":     reason,
	})
	return snapshot.Event{
		WorkItemID: in.WorkID, RepositoryID: in.RepoID, Kind: snapshot.EventDriftDetected,
		OldSHA: d.Old, NewSHA: d.New, Detail: string(detail),
	}
}

// driftDescriptionLine is the PR description's note for a clean drift.
func driftDescriptionLine(d driftResult) string {
	return fmt.Sprintf("**Base drift:** base moved from %s to %s; merges cleanly.", snapshot.Short(d.Old), snapshot.Short(d.New))
}

// driftBlockReason names the repository, both commits and every conflicting
// path.
func driftBlockReason(repoName, baseRef, branch string, d driftResult) string {
	return fmt.Sprintf("Base drift: %s moved from %s (the pinned base) to %s after the work item was pinned, and branch %s of repository %s no longer merges cleanly onto it — conflicts in: %s. Decide whether to re-run the work on the new base or cancel it; nothing was rebased automatically.",
		baseRef, snapshot.Short(d.Old), snapshot.Short(d.New), branch, repoName, strings.Join(d.Conflicts, ", "))
}

// driftPrompt is the escalation prompt raised with a conflicting-drift
// block. nil when the work item has no product (user_interactions.product_id
// is required).
func driftPrompt(workID, productID uuid.UUID, title, reason string, d driftResult) *interactions.Prompt {
	return blockedPrompt(workID, productID, title, reason, map[string]any{
		"base_drift": map[string]any{"old": d.Old, "new": d.New, "clean": false, "conflicts": d.Conflicts},
	})
}

// blockedPrompt is the `blocked` escalation prompt for a base-drift block.
func blockedPrompt(workID, productID uuid.UUID, title, reason string, payload map[string]any) *interactions.Prompt {
	if productID == uuid.Nil {
		return nil
	}
	return &interactions.Prompt{
		ID:             uuid.New(),
		WorkItemID:     workID,
		ProductID:      productID,
		AskingAgent:    agentID,
		Trigger:        interactions.TriggerBlocked,
		PromptType:     interactions.PromptTypeEscalation,
		Headline:       fmt.Sprintf("Blocked: base branch check for %s", title),
		Question:       reason,
		ContextLink:    fmt.Sprintf("/work-items/%s", workID),
		ContextPayload: payload,
		Actions: []interactions.Action{
			{ID: "retry", Label: "Retry", Semantic: interactions.SemanticRetry},
			{ID: "cancel", Label: "Cancel", Semantic: interactions.SemanticCancel},
		},
		State:       interactions.StatePending,
		RequestedAt: time.Now().UTC(),
		PromptedVia: []string{"dashboard"},
	}
}

// unevaluableDriftReason is the block reason when the drift check could not
// run. The error is already redacted (resolver and redactOrigin strip the
// origin's credential); gitremote.Redact is applied again for safety.
func unevaluableDriftReason(repoName string, err error) string {
	return fmt.Sprintf("base drift could not be evaluated for %s: %s", repoName, gitremote.Redact(err.Error(), ""))
}

// blockForDriftTx blocks the parent, records the drift event and raises the
// escalation prompt in one transaction (the caller's guarded beginner, so
// the M1-05 guard row is its first statement).
// ev is nil when no drift was observed (the check could not run).
func blockForDriftTx(ctx context.Context, db workgraph.TxBeginner, prompts *interactions.Publisher, workID uuid.UUID, ev *snapshot.Event, reason string, prompt *interactions.Prompt) error {
	return workgraph.WithTx(ctx, db, func(tx pgx.Tx) error {
		if err := guards.BlockWorkItemTx(ctx, tx, workID, workID, agentID, reason); err != nil {
			return err
		}
		if ev != nil {
			if err := snapshots.RecordEventTx(ctx, tx, *ev); err != nil {
				return err
			}
		}
		if prompt == nil {
			return nil
		}
		if prompts == nil {
			prompts = interactions.NewPublisher(nil)
		}
		return prompts.PublishTx(ctx, tx, workID, prompt)
	})
}

// recordDriftEventOwnTx records a drift event that has no PR transaction to
// ride in (github-service opened no PR). It uses the pool, not the guarded
// beginner, so it never takes the delivery's guard row ahead of a later
// write.
func recordDriftEventOwnTx(ctx context.Context, db workgraph.TxBeginner, ev snapshot.Event) error {
	return workgraph.WithTx(ctx, db, func(tx pgx.Tx) error {
		return snapshots.RecordEventTx(ctx, tx, ev)
	})
}

// snapshots writes base_sha_events inside callers' transactions.
var snapshots = snapshot.NewStore(nil)

// prDriftChecker is the production checker (real git, os temp dir).
var prDriftChecker = &driftChecker{}

// driftInput is one PR's drift check.
type driftInput struct {
	WorkID, RepoID uuid.UUID
	ProductID      uuid.UUID
	WorkTitle      string
	RepoName       string
	ClonePath      string
	BaseRef        string
	Pin            string
	HeadSHA        string
	Branch         string
}

// driftDecision is what the PR path does after the check.
type driftDecision struct {
	// Events commit with the PR's pr.ready (or gated) transaction.
	Events []snapshot.Event
	// Drift is pr.ready.base_drift; nil when the base did not move (or the
	// check could not run).
	Drift *baseDrift
	// DescriptionLine is appended to the PR description ("" when none).
	DescriptionLine string
	// Stop is true when the parent was blocked (conflicting drift, or the
	// block itself failed): open no PR for this or any later repository.
	Stop bool
}

// applyBaseDrift runs the drift check for one PR and acts on it. A
// legacy-unpinned PR (no pin) is not checked. A check that cannot run (the
// remote is unreachable, e.g. a rejected credential) fails closed: the parent
// is blocked with the cause and the escalation prompt, and no PR is opened.
func applyBaseDrift(ctx context.Context, checker *driftChecker, db workgraph.TxBeginner, prompts *interactions.Publisher, in driftInput) driftDecision {
	if in.Pin == "" {
		return driftDecision{}
	}
	d, err := checker.Check(ctx, in.RepoName, in.ClonePath, in.BaseRef, in.Pin, in.HeadSHA, in.Branch)
	switch {
	case err != nil:
		// Fail closed: a base that cannot be compared with the pin could
		// have moved; opening the PR would hide that. A rejected GitHub
		// credential (D8) therefore surfaces here as a block.
		// When the tip was read and differs from the pin, the move itself
		// was observed even though the merge check failed: audit it in the
		// same transaction as the block.
		reason := unevaluableDriftReason(in.RepoName, err)
		log.Printf("pr-coordinator: work_id=%s BLOCKED: %s", in.WorkID, reason)
		var ev *snapshot.Event
		payload := map[string]any{"old": in.Pin, "evaluated": false}
		if d.Moved && d.New != "" {
			e := unevaluatedDriftEvent(in, d, reason)
			ev = &e
			payload["new"] = d.New
		}
		if berr := blockForDriftTx(ctx, db, prompts, in.WorkID, ev, reason, blockedPrompt(in.WorkID, in.ProductID, in.WorkTitle, reason, map[string]any{
			"base_drift": payload,
		})); berr != nil {
			log.Printf("pr-coordinator: ERROR could not record the base-drift block of work_id=%s: %v", in.WorkID, berr)
		}
		return driftDecision{Stop: true}
	case !d.Moved:
		log.Printf("pr-coordinator: work_id=%s repo=%s: origin %s is still at the pinned base %s", in.WorkID, in.RepoName, in.BaseRef, snapshot.Short(in.Pin))
		return driftDecision{}
	case d.Clean:
		log.Printf("pr-coordinator: work_id=%s repo=%s: base moved from %s to %s; %s merges cleanly — drift recorded, PR proceeds", in.WorkID, in.RepoName, snapshot.Short(d.Old), snapshot.Short(d.New), in.Branch)
		return driftDecision{
			Events:          []snapshot.Event{driftEvent(in.WorkID, in.RepoID, d, in.HeadSHA, in.Branch)},
			Drift:           &baseDrift{Old: d.Old, New: d.New, Clean: true},
			DescriptionLine: driftDescriptionLine(d),
		}
	}
	reason := driftBlockReason(in.RepoName, in.BaseRef, in.Branch, d)
	log.Printf("pr-coordinator: work_id=%s BLOCKED: %s", in.WorkID, reason)
	ev := driftEvent(in.WorkID, in.RepoID, d, in.HeadSHA, in.Branch)
	if err := blockForDriftTx(ctx, db, prompts, in.WorkID, &ev, reason, driftPrompt(in.WorkID, in.ProductID, in.WorkTitle, reason, d)); err != nil {
		log.Printf("pr-coordinator: ERROR could not record the base-drift block of work_id=%s: %v", in.WorkID, err)
	}
	return driftDecision{Stop: true}
}
