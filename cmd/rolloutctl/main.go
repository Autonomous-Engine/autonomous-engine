// Command rolloutctl is the M4-06 developer CLI for shadow evaluation and
// rollout gates:
//
//	rolloutctl propose   --definition <uuid> | --candidate <snapshot-id>  [--scope global|product --product <uuid>]
//	rolloutctl shadow    --rollout <id> [--manifest benchmarks/tasks.json] [--trials 3] [--out dir] [--dry-run]
//	                     [--replay N --scratch-product <uuid> --gateway URL]
//	rolloutctl evaluate  --rollout <id> --report report.json | --results results.json... [--manifest m.json]
//	rolloutctl evaluate  --offline --report report.json      (no database: no outcomes, no attribution, nothing written)
//	rolloutctl approve   --rollout <id> [--override-reason "..."]
//	rolloutctl block     --rollout <id> [--reason "..."]
//	rolloutctl rollback  --rollout <id> [--reason "..."]
//	rolloutctl list
//	rolloutctl false-success                                  (run the nightly false-success job once)
//
// The database is DATABASE_URL (or --dsn). Spec:
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/autonomousengine/autonomous-engine/internal/configsnap"
	"github.com/autonomousengine/autonomous-engine/internal/outcomes"
	"github.com/autonomousengine/autonomous-engine/internal/rollout"
)

func main() {
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(2)
	}
	ctx := context.Background()
	var err error
	switch os.Args[1] {
	case "propose":
		err = cmdPropose(ctx, os.Args[2:], os.Stdout)
	case "shadow":
		err = cmdShadow(ctx, os.Args[2:], os.Stdout)
	case "evaluate":
		err = cmdEvaluate(ctx, os.Args[2:], os.Stdout)
	case "approve", "block", "rollback":
		err = cmdDecide(ctx, os.Args[1], os.Args[2:], os.Stdout)
	case "list":
		err = cmdList(ctx, os.Args[2:], os.Stdout)
	case "false-success":
		err = cmdFalseSuccess(ctx, os.Args[2:], os.Stdout)
	case "-h", "--help", "help":
		usage(os.Stdout)
		return
	default:
		usage(os.Stderr)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "rolloutctl %s: %v\n", os.Args[1], err)
		os.Exit(1)
	}
}

func usage(w io.Writer) {
	fmt.Fprintln(w, `usage: rolloutctl <propose|shadow|evaluate|approve|block|rollback|list|false-success> [flags]
Run "rolloutctl <command> -h" for a command's flags. Database: DATABASE_URL or --dsn.`)
}

func dsnFlag(fs *flag.FlagSet) *string {
	return fs.String("dsn", os.Getenv("DATABASE_URL"), "PostgreSQL DSN (default $DATABASE_URL)")
}

func connect(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	if dsn == "" {
		return nil, errors.New("no database: set DATABASE_URL or --dsn")
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	pool, err := pgxpool.New(cctx, dsn)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(cctx); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// ─── propose ─────────────────────────────────────────────────────────────────

func cmdPropose(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("propose", flag.ContinueOnError)
	dsn := dsnFlag(fs)
	definition := fs.String("definition", "", "candidate agent definition id (captured as its implementation-agent rollout unit)")
	candidate := fs.String("candidate", "", "candidate config snapshot id (an implementation-agent snapshot)")
	scope := fs.String("scope", rollout.ScopeGlobal, "global | product")
	product := fs.String("product", "", "product id (scope product)")
	createdBy := fs.String("created-by", os.Getenv("USER"), "proposer")
	notes := fs.String("notes", "", "free-text notes")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if (*definition == "") == (*candidate == "") {
		return errors.New("give exactly one of --definition or --candidate")
	}
	pool, err := connect(ctx, *dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	snaps := configsnap.NewStore(pool)

	baselineID, baselineDoc, err := rollout.ProductionSnapshot(ctx, pool)
	if err != nil {
		return fmt.Errorf("capture production default: %w", err)
	}
	if _, err := snaps.GetOrCreate(ctx, baselineDoc); err != nil {
		return err
	}

	candidateID := *candidate
	if *definition != "" {
		defID, err := uuid.Parse(*definition)
		if err != nil {
			return fmt.Errorf("invalid --definition: %w", err)
		}
		id, doc, err := rollout.CaptureUnit(ctx, pool, &defID)
		if err != nil {
			return err
		}
		if _, err := snaps.GetOrCreate(ctx, doc); err != nil {
			return err
		}
		candidateID = id
	} else {
		s, err := snaps.Get(ctx, candidateID)
		if err != nil {
			return fmt.Errorf("candidate snapshot: %w", err)
		}
		if s.AgentType != rollout.UnitAgentType {
			return fmt.Errorf("candidate snapshot %s is a %q snapshot; the rollout unit is the %s snapshot", candidateID, s.AgentType, rollout.UnitAgentType)
		}
	}
	surface, err := rollout.ProductionSurface(ctx, pool)
	if err != nil {
		return err
	}
	for key, id := range surface {
		if id == candidateID {
			return fmt.Errorf("candidate %s is already production (%s): a candidate is a snapshot that is not the production default — create the candidate definition disabled", candidateID, key)
		}
	}
	var productID *uuid.UUID
	if *product != "" {
		id, err := uuid.Parse(*product)
		if err != nil {
			return fmt.Errorf("invalid --product: %w", err)
		}
		productID = &id
	}
	if *scope == rollout.ScopeProduct {
		fmt.Fprintln(os.Stderr, "rolloutctl propose: note: a product-scoped rollout cannot unlock the configuration guard (there is no product-level default); an approval only scopes replay to the product")
	}
	r, err := rollout.NewStore(pool).Propose(ctx, rollout.ProposeInput{
		CandidateSnapshotID: candidateID, BaselineSnapshotID: baselineID,
		Scope: *scope, ProductID: productID, CreatedBy: *createdBy, Notes: *notes,
	})
	if err != nil {
		return err
	}
	return printJSON(out, r)
}

// ─── shadow ──────────────────────────────────────────────────────────────────

func cmdShadow(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("shadow", flag.ContinueOnError)
	dsn := dsnFlag(fs)
	rolloutID := fs.String("rollout", "", "rollout id")
	manifest := fs.String("manifest", "benchmarks/tasks.json", "task manifest")
	trials := fs.Int("trials", 3, "trials per task per side (at least 3 for a fair comparison)")
	outDir := fs.String("out", "", "output directory (default benchmark-results/rollout-<id>)")
	wrapper := fs.String("wrapper", "python benchmarks/wrappers/ae.py", "AE wrapper command (both sides)")
	python := fs.String("python", "python", "python interpreter")
	timeout := fs.Int("timeout", 0, "runner --timeout seconds (0 = runner default)")
	dryRun := fs.Bool("dry-run", false, "print the commands, run nothing, change nothing")
	replayN := fs.Int("replay", 0, "also re-submit the last N completed production items (products that opted in) against --scratch-product")
	scratch := fs.String("scratch-product", "", "benchmark scratch product (products.benchmark_scratch) the shadow benchmark and replay intakes target (AE_PRODUCT_ID)")
	gateway := fs.String("gateway", envOr("AE_GATEWAY_URL", "http://localhost:8082"), "gateway URL for replay intakes")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *trials < 3 {
		return fmt.Errorf("--trials %d: the shadow benchmark needs at least 3 trials per task", *trials)
	}
	id, err := uuid.Parse(*rolloutID)
	if err != nil {
		return fmt.Errorf("invalid --rollout: %w", err)
	}
	pool, err := connect(ctx, *dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	store := rollout.NewStore(pool)
	r, err := store.Get(ctx, id)
	if err != nil {
		return err
	}
	if r.CandidateSnapshotID == nil || r.BaselineSnapshotID == nil {
		return errors.New("the rollout has lost its candidate or baseline snapshot (pruned?)")
	}
	dir := *outDir
	if dir == "" {
		dir = filepath.Join("benchmark-results", "rollout-"+id.String())
	}
	plan := shadowPlan{
		Python: *python, Manifest: *manifest, Trials: *trials, OutputDir: dir, Wrapper: *wrapper,
		Candidate: *r.CandidateSnapshotID, Baseline: *r.BaselineSnapshotID, RolloutID: id.String(), Timeout: *timeout, ScratchProduct: *scratch,
		ResultsOut: filepath.ToSlash(filepath.Join(dir, "results.json")),
		ReportOut:  filepath.ToSlash(filepath.Join(dir, "report.json")),
	}
	runner := plan.runnerArgs()
	report := reportArgs(plan.Manifest, plan.ReportOut, []string{plan.ResultsOut})
	fmt.Fprintf(out, "runner: %s %s\n", plan.Python, strings.Join(quoteAll(runner), " "))
	fmt.Fprintf(out, "report: %s %s\n", plan.Python, strings.Join(quoteAll(report), " "))
	if *replayN > 0 && *scratch == "" {
		return errors.New("--replay needs --scratch-product (replays never run against the source product)")
	}
	if *dryRun {
		if *replayN > 0 {
			items, err := replayCandidates(ctx, pool, r, *replayN)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "replay: would re-submit %d item(s) to product %s via %s\n", len(items), *scratch, *gateway)
		}
		return nil
	}
	if _, err := store.MarkShadow(ctx, id, fmt.Sprintf("shadow run %s: %d trials, output %s", time.Now().UTC().Format(time.RFC3339), *trials, dir)); err != nil {
		return err
	}
	if *replayN > 0 {
		if err := submitReplays(ctx, pool, r, *replayN, *scratch, *gateway, out); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// The runner exits 1 whenever any attempt failed; that is data, not an
	// error (the report gates).
	if err := runPython(ctx, *python, runner, out); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
			return fmt.Errorf("benchmark runner: %w", err)
		}
	}
	if err := runPython(ctx, *python, report, out); err != nil {
		return fmt.Errorf("benchmark report: %w", err)
	}
	fmt.Fprintf(out, "next: rolloutctl evaluate --rollout %s --report %s --results %s\n", id, plan.ReportOut, plan.ResultsOut)
	return nil
}

func quoteAll(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		if strings.ContainsAny(a, " '\"$") {
			out[i] = shellQuote(a)
		} else {
			out[i] = a
		}
	}
	return out
}

func runPython(ctx context.Context, python string, args []string, out io.Writer) error {
	cmd := exec.CommandContext(ctx, python, args...)
	cmd.Stdout, cmd.Stderr = out, os.Stderr
	return cmd.Run()
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

type replayItem struct {
	ID          uuid.UUID
	Description string
}

// replayCandidates are the last n completed top-level production items of
// products that opted in to replay (only the rollout's product for a
// product-scoped rollout). Replay and shadow items are never replayed.
func replayCandidates(ctx context.Context, pool *pgxpool.Pool, r *rollout.Rollout, n int) ([]replayItem, error) {
	rows, err := pool.Query(ctx, `SELECT w.id, w.description FROM work_items w
		JOIN products p ON p.id = w.product_id
		WHERE p.allow_replay AND w.parent_id IS NULL AND NOT w.replay AND w.shadow_rollout_id IS NULL
		  AND w.requested_config_snapshot_id IS NULL
		  AND w.state IN ('passed', 'released')
		  AND ($1::uuid IS NULL OR w.product_id = $1)
		ORDER BY w.updated_at DESC LIMIT $2`, r.ProductID, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []replayItem
	for rows.Next() {
		var it replayItem
		if err := rows.Scan(&it.ID, &it.Description); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

func submitReplays(ctx context.Context, pool *pgxpool.Pool, r *rollout.Rollout, n int, scratch, gateway string, out io.Writer) error {
	items, err := replayCandidates(ctx, pool, r, n)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		fmt.Fprintln(out, "replay: no completed items in products that opted in (products.allow_replay)")
		return nil
	}
	client := &http.Client{Timeout: 30 * time.Second}
	for _, it := range items {
		body, _ := json.Marshal(map[string]any{
			"product_id":  scratch,
			"description": it.Description,
			"source":      "rollout-replay",
			"metadata": map[string]any{
				rollout.MetaReplay:           true,
				rollout.MetaReplayOf:         it.ID.String(),
				rollout.MetaConfigSnapshotID: *r.CandidateSnapshotID,
				rollout.MetaRolloutID:        r.ID.String(),
			},
		})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(gateway, "/")+"/api/intake", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("replay %s: %w", it.ID, err)
		}
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			return fmt.Errorf("replay %s: gateway answered %d: %s", it.ID, resp.StatusCode, strings.TrimSpace(string(msg)))
		}
		fmt.Fprintf(out, "replay: %s → %s\n", it.ID, strings.TrimSpace(string(msg)))
	}
	return nil
}

// ─── evaluate ────────────────────────────────────────────────────────────────

func cmdEvaluate(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("evaluate", flag.ContinueOnError)
	dsn := dsnFlag(fs)
	rolloutID := fs.String("rollout", "", "rollout id")
	reportPath := fs.String("report", "", "benchmark-report.py --json summary (the --partition sealed run)")
	manifest := fs.String("manifest", "benchmarks/tasks.json", "task manifest (to build the report from --results)")
	python := fs.String("python", "python", "python interpreter")
	offline := fs.Bool("offline", false, "no database: evaluate the report alone (no production outcomes, no attribution) and write nothing")
	dryRun := fs.Bool("dry-run", false, "evaluate but do not write the decision")
	var results multiFlag
	fs.Var(&results, "results", "runner results (JSON or JSONL; repeatable): attribution, and the report when --report is absent")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *reportPath == "" && len(results) == 0 {
		return errors.New("give --report or --results")
	}
	path := *reportPath
	if path == "" {
		tmp, err := os.CreateTemp("", "rollout-report-*.json")
		if err != nil {
			return err
		}
		tmp.Close()
		defer os.Remove(tmp.Name())
		path = tmp.Name()
		if err := runPython(ctx, *python, reportArgs(*manifest, path, results), io.Discard); err != nil {
			return fmt.Errorf("benchmark report: %w", err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	report, err := rollout.ParseReport(data)
	if err != nil {
		return err
	}

	if *offline {
		d := rollout.Evaluate(report, rollout.ProdOutcomes{Source: "none (offline)"}, rollout.ProdOutcomes{Source: "none (offline)"})
		return printDecision(out, d, nil)
	}

	id, err := uuid.Parse(*rolloutID)
	if err != nil {
		return fmt.Errorf("invalid --rollout: %w", err)
	}
	pool, err := connect(ctx, *dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	store := rollout.NewStore(pool)
	r, err := store.Get(ctx, id)
	if err != nil {
		return err
	}
	cand, base := deref(r.CandidateSnapshotID), deref(r.BaselineSnapshotID)
	baseline, err := rollout.BaselineOutcomes(ctx, pool, base, id)
	if err != nil {
		return err
	}
	candidate, err := rollout.CandidateOutcomes(ctx, pool, cand, id)
	if err != nil {
		return err
	}
	var opts []rollout.Option
	if len(results) > 0 {
		records, err := loadRecords(results)
		if err != nil {
			return err
		}
		a, err := rollout.Attribute(ctx, pool, cand, base, recordRefs(records, report.Candidate, report.Baseline))
		if err != nil {
			return err
		}
		opts = append(opts, rollout.WithAttribution(a))
	}
	d := rollout.Evaluate(report, baseline, candidate, opts...)
	if *dryRun {
		return printDecision(out, d, nil)
	}
	stored, err := store.RecordDecision(ctx, id, d)
	if err != nil {
		return err
	}
	return printDecision(out, d, stored)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// printDecision prints the verdict, each gate and, when stored, the
// rollout's new status. The full decision (raw counts, intervals and the
// report) is in rollouts.decision_report.
func printDecision(out io.Writer, d rollout.Decision, stored *rollout.Rollout) error {
	fmt.Fprintf(out, "Rollout decision: %s — %s\n", d.Verdict, d.Summary)
	for _, g := range d.Gates {
		fmt.Fprintf(out, "  %-24s %-13s %s\n", g.Name, g.Verdict, g.Reason)
	}
	if len(d.NotEvaluated) > 0 {
		fmt.Fprintf(out, "not evaluated (no data): %s\n", strings.Join(d.NotEvaluated, ", "))
	}
	fmt.Fprintf(out, "cost/latency: %s\n", d.CostNote)
	if stored != nil {
		fmt.Fprintf(out, "rollout %s is now %s\n", stored.ID, stored.Status)
	} else {
		fmt.Fprintln(out, "decision not written")
	}
	return nil
}

// ─── approve / block / rollback / list / false-success ───────────────────────

func cmdDecide(ctx context.Context, verb string, args []string, out io.Writer) error {
	fs := flag.NewFlagSet(verb, flag.ContinueOnError)
	dsn := dsnFlag(fs)
	rolloutID := fs.String("rollout", "", "rollout id")
	actor := fs.String("actor", os.Getenv("USER"), "who decides")
	override := fs.String("override-reason", "", "approve: approve without an APPROVED decision (audited)")
	reason := fs.String("reason", "", "block/rollback: why")
	if err := fs.Parse(args); err != nil {
		return err
	}
	id, err := uuid.Parse(*rolloutID)
	if err != nil {
		return fmt.Errorf("invalid --rollout: %w", err)
	}
	pool, err := connect(ctx, *dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	store := rollout.NewStore(pool)
	var r *rollout.Rollout
	switch verb {
	case "approve":
		r, err = store.Approve(ctx, id, *actor, *override)
	case "block":
		r, err = store.Block(ctx, id, *actor, *reason)
	default:
		r, err = store.Rollback(ctx, id, *actor, *reason)
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "rollout %s is now %s\n", r.ID, r.Status)
	return nil
}

func cmdList(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	dsn := dsnFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	pool, err := connect(ctx, *dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	list, err := rollout.NewStore(pool).List(ctx, 50)
	if err != nil {
		return err
	}
	for _, r := range list {
		fmt.Fprintf(out, "%s  %-11s %-7s candidate=%s baseline=%s verdict=%s\n", r.ID, r.Status, r.Scope,
			short(deref(r.CandidateSnapshotID)), short(deref(r.BaselineSnapshotID)), r.Verdict())
	}
	return nil
}

func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func cmdFalseSuccess(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("false-success", flag.ContinueOnError)
	dsn := dsnFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	pool, err := connect(ctx, *dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	n, err := outcomes.MarkFalseSuccess(ctx, pool)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "marked %d outcome(s) false_success (verified, reverted within %d days)\n", n, outcomes.FalseSuccessWindowDays)
	return nil
}
