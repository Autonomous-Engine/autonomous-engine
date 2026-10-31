# Benchmark competitor wrappers (M4-02)

Reference wrappers for the paired runner `scripts/code-implementation-benchmark.py`
(competitors run under equivalent conditions and the same fairness controls).
One wrapper per competitor, both honoring the same env-var
contract, the same permission surface, the same time and token caps, and writing
one normalized JSON document.

| Competitor | Shim (runner command) | Python entry | Driver |
|---|---|---|---|
| Autonomous Engine (the product) | `benchmarks/run-autonomous-engine.sh` | `benchmarks/run-autonomous-engine.py` | `wrappers/ae.py` |
| Claude CLI (baseline) | `benchmarks/run-claude.sh` | `benchmarks/run-claude.py` | `wrappers/claude_cli.py` |

Shared code: `wrappers/common.py` (contract, result model, `finalize`, git
helpers), `wrappers/schema.py` + `wrappers/output_schema.json` (normalized
output and a tiny validator). Python 3.13 standard library only. The `.sh` shims
just `exec` the Python entry next to them (`BENCH_PYTHON` overrides the
interpreter).

```bash
python -m unittest discover -s benchmarks/wrappers/tests -v
python benchmarks/run-claude.py --help
python benchmarks/run-autonomous-engine.py --help
python benchmarks/run-claude.py --print-command      # exact claude argv, no API call
```

## Human checkpoint: the live path is unverified here

The unit tests exercise both wrappers end to end against an in-process fake
gateway/dashboard and a fake `claude` executable (no network, no Docker). The
Claude wrapper was additionally run once against the real Claude CLI 2.1.267
with a trivial task and a $0.10 budget cap (it passed). **The Autonomous Engine
wrapper has not been run against a live stack: Docker was not running in the
environment where this was written, so Postgres/Kafka/the gateway/the dashboard
were unavailable.** Before trusting a paired result, a human must run the
"Running against a live stack" recipe below once and confirm the work item
reaches `passed` and the produced branch is fetched.

## Environment variables

### Runner contract (exported by `scripts/code-implementation-benchmark.py`)

| Var | Meaning |
|---|---|
| `BENCH_REPO` | Clean clone at the task's `base_revision`; also the cwd. |
| `BENCH_PROMPT_FILE` | Identical task prompt for both competitors. |
| `BENCH_OUTPUT_FILE` | Where the wrapper writes its normalized JSON (the runner deletes it afterwards, so the same document is always also printed on stdout as `BENCH_RESULT {...}` and copied to the raw dir). |
| `BENCH_TASK_ID` | Stable task id. |

### Shared wrapper knobs (enforced identically for both competitors)

| Var | Default | Meaning |
|---|---|---|
| `BENCH_TIME_LIMIT_S` | `1800` | Wall-clock cap enforced by the wrapper itself (outcome `timeout`, exit 3). Set it below the runner's `--timeout` so the wrapper, not the runner, records the timeout. |
| `BENCH_MAX_TOKENS` | unset | Prompt + completion token cap. AE: polled from `/api/work-items/cost` on every status poll; a breach cancels the work item and scores `failed`. Claude: checked against the `usage` block of the JSON result after the run and scores `failed` on breach. **Both fail closed:** if the cap is set and usage is unknown at run end (cost endpoint never answered, zero tagged inference logs, missing usage block) the run is `failed` with `token_cap.verified = false`. **This is the only budget that is comparable across the two competitors** (see below). |
| `BENCH_RAW_DIR` | `<tmp>/bench-wrappers` | Root for raw logs: `<root>/<competitor>/<task>/<timestamp>-<pid>/`. Must be outside `BENCH_REPO` (the runner counts every untracked file in the clone as an agent change); the wrapper refuses otherwise. |
| `BENCH_QUIET` | unset | `1` stops echoing log lines to stderr. The `BENCH_RESULT` stdout mirror is part of the contract and is printed regardless. |

### Autonomous Engine (`run-autonomous-engine`)

| Var | Default | Meaning |
|---|---|---|
| `AE_GATEWAY_URL` | `http://localhost:8082` | Gateway; `POST /api/intake` (unauthenticated in `cmd/gateway/main.go`). |
| `AE_DASHBOARD_URL` | `http://localhost:8080` | dashboard-backend; polling, approvals, cancel, cost, pending changes, version. |
| `AE_API_TOKEN` | required | dashboard JWT (`POST /api/auth/login` -> `token`). Sent as `Authorization: Bearer` to every call (the gateway ignores it). |
| `AE_PRODUCT_ID` | unset | Product UUID; optional when exactly one product exists (gateway auto-selects). |
| `AE_REPOSITORY_ID` | unset | Repository UUID. Intake cannot set it (the gateway auto-assigns it for single-repo products), so it is sent as `metadata.repository_id` and verified against the polled work item; a mismatch is a `failed` outcome. |
| `AE_FETCH_REMOTE` | **required** | URL or path of the fork the product pushes `agent/<work_id>/*` branches to. Used directly as the fetch URL for `git ls-remote`/`git fetch`; the clone's `origin` is never used (sealed fixture clones have their origin removed or pointed at an upstream the product cannot push to). The wrapper refuses to start without it. |
| `AE_POLL_INTERVAL_S` | `10` | Poll interval. |
| `AE_AUTO_APPROVE` | `1` | Auto-approve the product's human gates. The product parks them in `awaiting_user` with a pending prompt (`GET /api/prompts`, trigger `approval:prd`, `approval:human_review`, `approval:pr_gate`); the wrapper answers the prompt's `continue` action through `POST /api/prompts/{id}/answer`. The legacy `prd_approval`/`human_review` states are still approved through `/api/work-items/approve-prd` and `/api/work-items/approve`. A pending prompt that is not an approval (a clarification, escalation, or block question) is a genuine blocker and is reported as `blocked` with `details.blocking_prompt`. With `0` the wrapper waits at the gate and times out honestly. |
| `AE_TAGS`, `AE_COMPLEXITY`, `AE_SOURCE` | empty / empty / `benchmark` | Optional intake routing fields (tags/complexity select the agent definition). |
| `AE_CONFIG_SNAPSHOT_ID` | unset | M4-06 shadow evaluation: the configuration snapshot to run, sent as `metadata.config_snapshot_id`. The gateway honours it only when `AE_ROLLOUT_ID` names a rollout in proposed/shadow/evaluating, the snapshot is that rollout's candidate or baseline, and `AE_PRODUCT_ID` is a benchmark scratch product (`products.benchmark_scratch`, Admin → Rollouts → Shadow settings); anything else carrying a snapshot is refused (400). `AE_SOURCE` grants nothing. The item is routed to the snapshot's agent definition instead of tags/complexity and the snapshot is recorded as `work_items.requested_config_snapshot_id`. Recorded in `versions.requested_config_snapshot_id`; `versions.config_snapshot_id` is what the item actually ran under, which `rolloutctl evaluate` checks against it. `rolloutctl shadow` sets it per side. |
| `AE_ROLLOUT_ID` | unset | M4-06: the rollout the run belongs to (`metadata.rollout_id`). |

### Claude CLI (`run-claude`)

| Var | Default | Meaning |
|---|---|---|
| `BENCH_CLAUDE_BIN` | `claude` | Executable name/path, resolved on `PATH`. |
| `BENCH_MODEL` | unset | `--model`. Fairness rule 3: fix model ids; the models actually used are recorded in `versions.models` either way. |
| `BENCH_MAX_BUDGET_USD` | unset | `--max-budget-usd`. **Claude-only.** Refused unless `BENCH_MAX_TOKENS` is also set, so no run is ever capped only on the Claude side. |
| `BENCH_EFFORT` | unset | `--effort`. |

### Why there is no comparable monetary cap

Claude CLI exposes `--max-budget-usd`, a hard kill switch on billed dollars, and
reports `total_cost_usd`. Autonomous Engine's dashboard exposes only partial
token counts (`/api/work-items/cost` aggregates the `inference_logs` rows that
happen to be tagged with the work item) but no billed USD and no budget
control, and its cost depends on which provider/model the agent definition
routes to. A dollar cap on one side and nothing on the other would
bias the comparison, so the wrappers make **tokens** the shared budget:
`BENCH_MAX_TOKENS` is enforced for both (AE mid-run with cancel, Claude post-run
because the CLI has no token-ceiling flag - only `--max-budget-usd` and
`--autocompact` exist in 2.1.267), and `BENCH_MAX_BUDGET_USD` may only be used
as an additional safety net on top of it. AE's `cost_usd` is `null` unless
prices are configured (see "AE cost in USD"); never compare them as if both
were capped.

## Equal-permission policy

The comparison is "full product versus Claude CLI". Autonomous Engine's
implementation agent (`cmd/implementation-agent`, `internal/llm/tools/executor.go`)
gets: `read_file`/`list_files`/`search_code`, `write_file`/`edit_file`,
`run_command` (arbitrary `sh -c` inside the clone with a small regex denylist of
destructive patterns), read-only `git_log`/`git_diff`, plus the product's own
codex/LSP/verify tools (the feature under test). Its "you do NOT have access to
git commands" is a **prompt instruction**, not an executor rule: `run_command`
does not block local git, and the executor only blocks force-push to
`main`/`master`. The system creates the branch, commits and pushes.

The Claude wrapper mirrors that surface (`claude --help`, 2.1.267):

| Purpose | Flag |
|---|---|
| Non-interactive, one JSON document | `-p --output-format json` (prompt on stdin, never argv) |
| Nothing may prompt, nobody answers | `--permission-mode dontAsk --permission-prompts none` |
| Same tool surface as AE | `--tools Read,Edit,Write,Glob,Grep,Bash` and `--allowedTools Read Edit Write Glob Grep Bash` |
| No git publishing (nothing leaves the clone) | `--disallowedTools Bash(git push*) Bash(git remote*) Bash(git send-email*) Bash(git request-pull*) Bash(git svn*)` - force-push to main is covered by the blanket push deny. Local git (commit, checkout, reset, stash, branch, merge, rebase...) is **allowed**, because AE's executor allows it too. |
| AE's destructive-command denylist | `Bash(sudo *) Bash(su *) Bash(doas *) Bash(mkfs*) Bash(dd *) Bash(chmod -R 777*) Bash(rm -rf /*) Bash(rm -rf ~*) Bash(rm -fr /*) Bash(rm -fr ~*)` |
| No tools AE lacks | `WebFetch WebSearch Agent Task` denied; `--strict-mcp-config` (no MCP servers); `--disable-slash-commands` (no skills) |
| No per-machine contamination | `--setting-sources project` (user/local settings and hooks ignored; the repo's own `.claude/` still applies, like the repo's `CLAUDE.md`) |
| Hygiene | `--no-session-persistence` |
| Budgets | `--model`, `--max-budget-usd`, `--effort` from the env vars above; `BENCH_MAX_TOKENS` checked post-run |

Never used: `--dangerously-skip-permissions`, `--allow-dangerously-skip-permissions`,
`--permission-mode bypassPermissions`.

Because AE's *system* commits on the agent's behalf, the Claude wrapper does the
same: after `claude` exits it stages and commits everything still uncommitted
(excluding the runner's `.benchmark-*` files) with a `benchmark-wrapper`
identity, so both competitors end with a comparable `head_sha`. If Claude
committed itself, that commit is the head. Partial work after a failure/timeout
is committed too, so `head_sha` is honest about what exists.

### Residual permission differences (disclosed, not hidden)

1. **Prefix rules vs regexes.** Claude's deny rules are command-prefix matches.
   They miss chained or wrapped forms - `cd x && sudo ...`, `sh -c 'git push'`,
   `env FOO=1 git push`, `bash <<EOF ... EOF` - that AE's whole-string regexes
   in `executor.go` `dangerousCommandReason` do catch. Claude can therefore
   reach a denied command through a wrapper; AE cannot.
2. **AE blocks more shapes.** AE additionally rejects `curl|wget|fetch ... | sh`,
   fork bombs (`:(){:|:&};:`), raw-device writes (`> /dev/sdX`) and force-push
   to main by regex. Prefix rules cannot express the first three, so they are
   not denied for Claude.
3. **AE can get real git tools on one path.** The test-fix retry loop in
   `cmd/implementation-agent/main.go` (`Tools: tools.ImplementationTools()`
   around line 1541) hands the model `CreateBranch`, `CommitAndPush` and
   `CreatePullRequest` tools, unlike the main loop's `SafeImplementationTools`.
   On that path AE's agent can publish; Claude never can.
4. Neither competitor is network-sandboxed by the wrapper (AE's executor is not
   either); both must run inside the same disposable container/VM with the same
   network policy.
5. AE's `run_command` defaults to a 60 s per-command timeout; Claude's Bash tool
   has its own default. Both share `BENCH_TIME_LIMIT_S` overall.
6. Claude auto-loads the repository's `CLAUDE.md`; AE's agent can read it but
   does not auto-load it, and gets the codex instead. This is the "full product
   versus CLI" framing, not model versus model.
7. `BENCH_MAX_TOKENS` cuts AE short mid-run but can only fail Claude after the
   fact (no CLI token flag). A Claude run that overruns still spends the
   tokens; it is scored `failed` either way.

## Normalized output

`benchmarks/wrappers/output_schema.json` (validated on every write; tests also
validate the samples in `testdata/`):

| Field | Type | Notes |
|---|---|---|
| `competitor` | string | `autonomous-engine` / `claude-cli` |
| `task_id` | string | `BENCH_TASK_ID` |
| `outcome` | enum | `passed_agent` \| `failed` \| `blocked` \| `timeout` |
| `elapsed_s` | number | wrapper wall clock |
| `head_sha` | string\|null | commit checked out in `BENCH_REPO` on return |
| `model_calls` | int\|null | AE: `total_llm_calls` from `/api/work-items/cost` (**lower bound**, see below); Claude: `num_turns` |
| `tokens_in` | int\|null | AE: `total_input_tokens` (**lower bound**); Claude: `input + cache_creation + cache_read` |
| `tokens_out` | int\|null | AE: `total_output_tokens` (**lower bound**); Claude: `output_tokens` |
| `cost_usd` | number\|null | Claude: `total_cost_usd`; AE: `cost_usd` from `/api/work-items/cost` when every call is priced in `model_prices`, else **null** (see "AE cost in USD") |
| `versions` | object | CLI/build versions, model ids, wrapper version, platform |
| `raw_ref` | string | absolute path to `wrapper.log` in the raw dir |
| `details` | object | free-form diagnostics, see below |

`details` always carries, for **both** competitors:

* `exit_code` - the wrapper's exit code (from the table below).
* `changed_files` / `diff_stat` / `diff_base` / `diff_head` - `git diff
  --name-only|--stat <base_sha>..HEAD` where `base_sha` is the runner's pinned
  revision. Both wrappers leave a clean working tree (AE checks out the produced
  branch, the Claude wrapper commits), so the runner's own `changed_files` and
  `diff_stat` come back empty; use these instead. All four keys are present on
  every path (null when unknown).
* `base_is_ancestor` - whether the final HEAD contains the pinned base. A
  `passed_agent` outcome requires it to be true and `head_sha` to be non-null
  for **both** competitors; otherwise the run is `failed`.
* `token_cap` - `{limit, observed, verified}` on **every** path when
  `BENCH_MAX_TOKENS` is set (`observed` null and `verified` false when usage
  is unknown). Absent when the cap is not set.
* `reason` - free text for every non-pass outcome.

AE-specific: `work_item_id`, `state_history`, `approvals`, `final_state`,
`remote_branches`, `merged_branches` (`[{branch, head}]` in merge order),
`wrapper_side_merge`, `token_accounting`, `cancelled_after_*` flags.
Claude-specific: `claude_exit_code` (raw status), `claude` (`is_error`,
`subtype`, `num_turns`, `permission_denials`, ...), `status_porcelain`, `command`.

`null` means unknown, never zero. `passed_agent` means the agent/product
*claims* success and a checkout exists; the runner's hidden tests decide the
real pass.

### AE token accounting is partial

`/api/work-items/cost` sums `inference_logs` rows tagged with the work item.
Only some agents tag their rows (research-coordinator and the tool-executor
loop); PRD generation, planning and reconciliation spend is not attributed, and
the handler `COALESCE`s "nothing tagged" to `0`. So AE's `tokens_in`,
`tokens_out` and `model_calls` are a **lower bound**, marked
`details.token_accounting = "partial"` on every AE run, and a terminal-state
run whose endpoint reports `total_llm_calls == 0` is reported as unknown
(`null`), never as zero. Do not compare AE token totals with Claude's as equals;
treat them as "at least this much".

Since M4-05 every AE agent tags its calls with the work item, its role
(author / reviewer / planner / research / qa / other) and the configuration
snapshot, and the endpoint sums the item **and its subtasks**. Runs against
services built before M4-05 are still partial, so the `partial` label stays
until a run is known to use only M4-05 images.

### AE cost in USD

`/api/work-items/cost` prices each call through the `model_prices` table
(USD per 1M input / output tokens, effective from a date; Admin →
Configuration versions → Model prices). The table is seeded with placeholder
rows whose prices are empty. The endpoint returns `cost_usd` as a number and
`cost_unknown: false` only when **every** call with tokens has an effective,
non-empty price; otherwise `cost_usd` is `null` and `cost_unknown: true`. The
AE wrapper copies that: `cost_usd` becomes non-null once prices exist for every
model the run used, and stays `null` (never `0`) otherwise. AE's figure is a
price-list computation, not a provider invoice; Claude's `total_cost_usd` is
what the CLI reports. Compare them only with that caveat.

### Exit codes (identical table for both wrappers)

| Code | Outcome |
|---|---|
| 0 | `passed_agent` |
| 1 | `failed` (incl. token-cap breach, non-zero Claude exit, `is_error` with exit 0, missing branch, wrong base) |
| 2 | `blocked` |
| 3 | `timeout` |
| 4 | usage / configuration error (nothing was submitted or run) |

The Claude wrapper never returns Claude's raw status: a raw 2 or 3 would
collide with `blocked`/`timeout`. The raw value is in `details.claude_exit_code`.
The Claude wrapper can never return 2/`blocked`: print-mode Claude has no
escalation channel, so a "needs a human" outcome cannot be observed on that
side (compare AE's `awaiting_user`/`blocked`).

## How the AE wrapper finds the produced commit

Work-item state comes from `GET /api/work-items/detail?id=` (`item.state`,
`transitions`, `agent_definition`, `inference_logs`). State classes
(`internal/models/workitem.go`, `internal/workgraph/statemachine.go`):

* success: `passed`, `released`
* failed: `failed`, `rejected`, `cancelled`
* blocked: `blocked`, `awaiting_user` (the product escalated; nobody can answer)
* human gates (auto-approved by default): `prd_approval`, `human_review`
* everything else is in progress

Robustness: transport/HTTP errors and HTTP-200 bodies without `item.state` are
counted separately; five in a row of either cancels the work item and scores
`failed` (a dashboard that answers but cannot be tracked is not a `timeout`).

The dashboard does not expose a head SHA. The durable convention is the
branch the implementation-agent pushes, `agent/<work_id>/<task_id[:8]>`
(`cmd/implementation-agent/main.go`; `cmd/dashboard-backend/handlers.go`
`WorkItemDiff` and `cmd/pr-coordinator` search by the same prefix, and
`pending_pr_changes.branch` stores it). The wrapper therefore runs
`git ls-remote --heads <AE_FETCH_REMOTE> refs/heads/agent/<work_id>/*` in
`BENCH_REPO` (`AE_FETCH_REMOTE` is a URL/path used directly; `origin` is never
consulted), fetches each branch into `refs/bench-ae/...`, checks the oldest
out as the local branch `bench-autonomous-engine` (deliberately not nested
under `bench/`, because the runner's own checkout creates a local branch named
`bench`) and merges the rest in commit order. All git work after the polling
phase is bounded by the remaining time budget (floor 30 s, cap 300 s), so a
run cannot overshoot `BENCH_TIME_LIMIT_S` by minutes.
`/api/work-items/pending-changes` rows for the work item are recorded in
`details.pending_changes` as a cross-check. Finally the wrapper verifies the
benchmark base revision is an ancestor of the new HEAD; if the product built on
a different base the run is `failed`, not silently compared.

**Multi-subtask results are a wrapper-side merge.** A work item the planner
splits into several subtasks produces one `agent/<work_id>/<task8>` branch per
subtask, each reviewed/QA'd and turned into its own PR by the product. The
wrapper merges them so the hidden tests can run against one tree, but the
product never assembled or QA'd that combined tree. Every such run has
`details.wrapper_side_merge = true` and lists each branch and head in
`details.merged_branches`; a merge conflict is a `failed` outcome. Treat these
runs as a distinct stratum when scoring.

On timeout or token-cap breach the wrapper `POST /api/work-items/cancel`s the
item so it stops spending, then exits 3 or 1 respectively. When
`BENCH_MAX_TOKENS` is set, a terminal run whose usage is still unknown (cost
endpoint erroring throughout, or zero tagged inference logs) is `failed` with
`token_cap.verified = false` - the cap fails closed on both sides.

## Running against a live stack

1. Bring the stack up (`docker compose up -d`), complete onboarding, and
   register the product and a repository that points at a **fork of the
   fixture repository at the same base revision** - a remote the product can
   push `agent/<work_id>/*` branches to. Sealed fixture clones have their
   `origin` removed (or pointed at a public upstream the product cannot push
   to), so the wrapper never uses `origin`: set `AE_FETCH_REMOTE` to that
   fork's URL/path and the wrapper fetches the agent branches from it
   directly. Make sure the fork's `base_branch` equals the task's
   `base_revision`, otherwise the ancestor check fails the run.
2. Get a dashboard token: `curl -s -X POST $AE_DASHBOARD_URL/api/auth/login -d '{"email":"...","password":"..."}' | jq -r .token`.
3. Export the env vars: `AE_API_TOKEN`, `AE_PRODUCT_ID`, `AE_FETCH_REMOTE`
   (required), optionally `AE_REPOSITORY_ID`; the shared caps
   `BENCH_TIME_LIMIT_S` and `BENCH_MAX_TOKENS`; `BENCH_MODEL` (fix it for
   Claude and make the AE agent definition use the same model);
   `BENCH_RAW_DIR`. `BENCH_MAX_BUDGET_USD` is optional and Claude-only.
4. Run the paired runner (manifest shape: `benchmarks/tasks.example.json`,
   sample: `benchmarks/wrappers/testdata/tasks.sample.json`):

```bash
python scripts/code-implementation-benchmark.py \
  --manifest benchmarks/tasks.json \
  --candidate-command './benchmarks/run-autonomous-engine.sh' \
  --baseline-command './benchmarks/run-claude.sh' \
  --timeout 2100 \
  --output benchmark-results/run-001.json
```

5. The normalized documents are in each result's `agent.stdout`
   (`BENCH_RESULT {...}`) and in `BENCH_RAW_DIR`. Aggregate with the fairness
   rules (3+ trials, paired CIs, cost per
   accepted task alongside raw pass rate), stratifying wrapper-side merges.
