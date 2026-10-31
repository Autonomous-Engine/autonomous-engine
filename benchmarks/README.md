# Implementation benchmark corpus

Sealed, executable fixtures for comparing code-implementation agents under
paired, equal-budget conditions. Every fixture pins a public repository to a full commit SHA,
gives the agent a prompt, and is scored by held-out tests that the agent never sees.

The runner is `scripts/code-implementation-benchmark.py`; it consumes `benchmarks/tasks.json`.
Competitor wrappers (`benchmarks/run-*.sh`, `run-*.py`, `benchmarks/wrappers/`) are owned
separately and are not part of this corpus.

## Layout

```
benchmarks/
  tasks.json                  manifest consumed by the runner (paths relative to this file)
  tasks.example.json          minimal example of the runner's original manifest shape
  verify-fixtures.py          proves every fixture discriminates (FAIL at base, PASS with reference)
  corpus-targets.json         corpus-level targets (D5 mix) per language and overall
  tasks/go.mod                nested module so the hidden *_test.go files (which belong to the
                              target repositories) are skipped by go build/vet/test ./... at the root
  tasks/<task-id>/
    prompt.md                 the only text the agent sees; also exported as BENCH_PROMPT_FILE
    fixture.json              evaluator metadata: repo, base_sha, language, task_class,
                              difficulty, size_stratum, test_command, partition,
                              expected_outcome, provenance, reference (upstream fix recipe)
    hidden-tests/             files copied into the clone only after the agent exits
```

Blocked/underspecified fixtures have no `hidden-tests/` and no `reference`. Their
`expected_outcome` is `"blocked"` and their "test" is an expected-outcome rule, not code.

### Manifest fields (`tasks.json`)

Read by the runner:

| field              | meaning                                                                 |
|--------------------|-------------------------------------------------------------------------|
| `id`               | stable task id; equals the fixture directory name                       |
| `repo`             | public `https://` URL in canonical form: no trailing slash, no `.git` suffix (the verifier rejects other spellings; pairing for the leak check is case-insensitive). A local path is still accepted by the runner for ad-hoc runs |
| `base_revision`    | full 40-hex commit SHA fetched into the agent's clone                   |
| `prompt_file`      | path to `prompt.md`, relative to `tasks.json` (`prompt` inline still works) |
| `hidden_tests_dir` | directory copied into the clone after the agent exits                   |
| `test_command`     | run with `bash -lc` inside the clone after the hidden tests are in place |
| `expected_outcome` | `pass` (default) or `blocked`                                            |

Strata metadata (`language`, `task_class`, `difficulty`, `size_stratum`, `partition`,
`fail_mode`) is mirrored from `fixture.json` so result aggregation can group by stratum; the
verifier fails if the two disagree.

`fail_mode` is **required** on every non-blocked fixture and declares how the hidden tests
fail at the base revision: `assert` (they compile and their assertions fail — the behaviour is
wrong) or `build` (they do not compile — the API/module the prompt asks for does not exist
yet). The verifier classifies the real base run and fails when the observed mode differs from
the declared one or the field is missing. `build`-mode fixtures are a weaker gate (an agent
only has to make the code compile to move past the base failure), so `assert` is preferred;
use `build` only when the task is genuinely "add a new API" and no behavioural check can
compile at base, and say why in `fixture.json.provenance`.

Every non-blocked fixture in the corpus is currently `assert`-mode. M4-04 replaced the four
original `build`-mode fixtures with `assert`-mode equivalents (same prompt, same reference;
each explains the technique in `provenance.fail_mode_note`):

| fixture | technique |
|---------|-----------|
| `go-version-sql-scanner-valuer` | the hidden tests assert `sql.Scanner`/`driver.Valuer` at run time (`interface{}(v).(sql.Scanner)`) instead of with compile-time guards |
| `go-humanize-ronna-quetta-prefixes` | two-step test command `go test . && go test ./...`: the root-package tests use only API that exists at base (behaviour); the new exported constants are checked in a hidden external test package `bench_bigsizes/` that the second step compiles |
| `cron-functional-options` | same two-step pattern: root-package tests assert the new default-parser behaviour of `New()`; the option API is tested in a hidden `bench_options/` package (each step keeps the upstream-flake retry) |
| `godotenv-go-module` | step 1 runs the root package in GOPATH mode (`GO111MODULE=off go test .`), where the hidden test compiles without a `go.mod` and its `go list -m` assertion fails; step 2 is `go test ./...` in module mode. If a future Go drops GOPATH mode, revert to `build` (see the fixture's `test_command_note`) |

The two-step pattern is the general recipe for "new API" tasks: put behaviour that the base
API can express in the package under test, put references to new identifiers in a separate
hidden package, and run the package under test first so the base failure is an assertion.

`task_class` is one of `historical_defect`, `small_feature`, `cross_file_refactor`,
`dependency_or_config`, `blocked_or_underspecified`. `size_stratum` is `small` (roughly under
2k non-test source lines), `medium` (2k-10k) or `large`. `difficulty` is `easy`, `medium`
or `hard`.

### Scoring rule per `expected_outcome`

- `pass`: the agent command exits 0 **and** `test_command` exits 0 after the hidden tests are
  copied in.
- `blocked`: all three must hold —
  1. the wrapper's normalized output document (written to `BENCH_OUTPUT_FILE`, read by the
     runner before the file is removed and kept in the result as `agent_output`) has
     `outcome == "blocked"`;
  2. no tracked file differs from the pinned base revision: the runner checks
     `git diff --quiet <base_revision> HEAD`, `git diff --cached --quiet` and
     `git diff --quiet <base_revision>` (working tree). The comparison is against the base,
     not against `HEAD`, because wrappers legitimately commit or check out a produced commit
     and would otherwise always present a clean tree;
  3. `test_command` exits 0. For the shipped blocked fixture it is
     `git diff --quiet "$BENCH_BASE_REVISION" HEAD && git diff --cached --quiet`.

  A non-zero agent exit on its own (token cap, wrapper exception, timeout, missing or
  unparsable output document) is **never** credited as a blocker. Untracked scratch files
  (notes, a report) are allowed so an honest blocker is not penalised for leaving them. An
  agent that guesses the missing specification and "succeeds" is scored as a false success.

  The wrapper document is the wrappers' normalized JSON with top-level fields `competitor`,
  `task_id`, `outcome` (`passed_agent | failed | blocked | timeout`), `elapsed_s`,
  `head_sha`, `model_calls`, `tokens_in`, `tokens_out`, `cost_usd`, `versions`, `raw_ref`.
  The runner only interprets `outcome`; everything else is carried through for aggregation.

  **Disclosure — the blocked stratum is not winnable by every competitor.** The runner
  checks only that the wrapper reported `outcome == "blocked"` and that the tree is
  unchanged; it does not, and cannot, check that the agent named the missing specification.
  A competitor whose wrapper has no escalation channel (today: the Claude CLI wrapper, which
  only ever emits `passed_agent`, `failed` or `timeout`) can never pass a
  `blocked_or_underspecified` task, by construction. **Reporting rule:** the
  `blocked_or_underspecified` stratum is scored and reported separately, alongside the
  competitor's escalation capability, and is never folded into the head-to-head pass rate.
  **Manual spot-check:** for every blocked pass, a reviewer reads `agent_output` (the
  wrapper's `details`/`raw_ref` for the agent's stated reason) to confirm the blocker was the
  missing specification rather than an unrelated refusal or a guess that was later reverted.

  **Runner exit status:** the runner process exits 1 whenever *any* task result is not
  `passed` — including expected losses such as a blocked task for a competitor without an
  escalation channel. It is a data-collection tool; do not wire its exit code to a CI gate
  for paired runs. Gate on `scripts/benchmark-report.py` (paired trials with confidence intervals),
  which reports this stratum in its own table and excludes it from the head-to-head rate.

  **Change evidence:** each result carries HEAD-relative fields (`changed_files`,
  `diff_stat`; kept for backward compatibility, usually empty because wrappers commit or
  check out a produced commit) and base-relative fields: `changed_files_vs_base`
  (`git diff --name-status <base_revision>..HEAD` lines followed by every
  `git status --porcelain` line, i.e. uncommitted tracked modifications and untracked
  files), `changed_file_count_vs_base`, and `diff_stat_vs_base`
  (`git diff --stat <base_revision>..HEAD`). Use the base-relative fields for
  unrelated-file-change and diff-size metrics.

## Sealing rules

1. **Hidden tests never enter the agent's clone or prompt.** The runner copies
   `hidden_tests_dir` in only after the agent command has exited; `prompt.md` must not name
   hidden test files or test functions (the verifier greps for this).
2. **The agent's clone contains only the base revision, and no remote.** For `https://` repos
   the runner and the verifier do not `git clone`; they `git init`, `git fetch --depth 1
   origin <sha>`, check out `FETCH_HEAD` and then `git remote remove origin`. Both assert
   `HEAD == base_sha`, `git rev-list --all --count == 1` and that no remote remains, and both
   run git with `GIT_TERMINAL_PROMPT=0`. This closes the *local* paths to the upstream fix
   (`git log --all`, tags, `origin/*`, `git fetch`).

   **What this does not close:** every prompt names the upstream repository, and the
   upstream fix exists publicly. An agent with network access to the upstream host (GitHub)
   can find it. Scores are therefore only meaningful when agents run **without network access
   to the upstream host** (the intended setup: disposable containers with an egress
   allowlist for the language package proxy only). If a comparison is run with network
   access, treat the leak as accepted and symmetric — both competitors get the same
   opportunity — and say so in the results; do not present such a run as sealed.
3. **Historical defects use the parent of the upstream fix as `base_sha`.** The fix commit is
   recorded only in `fixture.json` (evaluator-only, never exported to an agent). The corpus
   ships no upstream code: `reference.steps` names the upstream commit and paths, and the
   verifier renders the reference patch with `git diff --binary -M <base_sha> <commit> -- <paths>`
   from a shallow fetch (optionally dropping hunks that contain a `drop_hunks_containing` string).
   **No fixture's base may contain another fixture's fix.** When two fixtures share a
   repository, the later base would otherwise ship the earlier fixture's answer. The
   verifier checks this in its default mode: for every other fixture on the same repository
   it runs `git apply --reverse --check -C0 --include=<file> <other's rendered reference patch>` in
   the sealed clone, once per file the patch touches, and fails if any file reverses cleanly
   (the base already contains that change). `-C0` matches on the changed lines only: a later
   base normally drifts the context around an earlier fix (imports, neighbouring code), so
   the full-context form reports "does not apply" even when the fix is present byte for
   byte — that was exactly the case with the leak found in review. The only
   allowed escape is `reverse_apply_exceptions` in `fixture.json`, mapping the other fixture
   id to a written reason, for content that is not an answer at all (currently one: the
   godotenv parser-fix base already has the three-line `go.mod` the go-module task asks
   for; the harmful direction, the go-module base containing the parser fix, is checked and
   clean). Exceptions are shown in the `xleak` column of the verifier table.
4. **Hidden tests test behaviour, not the patch.** They are written from the prompt's
   acceptance criteria, use only the public API, and do not copy the upstream test. Where the
   upstream fix necessarily changed existing tests (e.g. tests that pinned the old, wrong
   behaviour) the prompt says so explicitly.
5. **No prompt tuning against `partition: sealed`.** Prompts, routing, retrieval and model
   settings may be iterated against `development` fixtures only. Sealed fixtures are for
   final comparisons; a sealed fixture that has been used to tune anything must be moved to
   `development` or replaced.
6. **Reference patches are not shown to agents and are not used for scoring.** They exist so
   the verifier can prove the hidden tests are satisfiable.
7. **Test commands must be deterministic for the hidden tests.** A fixture whose *upstream*
   suite is timing-sensitive may allow a single retry in `test_command`
   (`go test ./... || go test -count=1 ./...`) and must say so in `fixture.json`
   (`test_command_note`); a retry can absorb an upstream flake but never turn a failing
   hidden test into a pass.

## Verifying the corpus

```
python benchmarks/verify-fixtures.py            # all fixtures
python benchmarks/verify-fixtures.py --only gjson-integer-overflow-clamp --verbose
python benchmarks/verify-fixtures.py --keep     # keep the sealed clones for inspection
```

Requirements: `git`, `bash` (the runner and verifier execute `test_command` through
`bash -lc`), network access to GitHub without credentials, and the toolchain for each
fixture's language on `PATH`: Go for the Go fixtures (verified with Go 1.27); Node.js ≥ 22
and npm for the TypeScript fixtures (warm npm cache; see [TypeScript corpus](#typescript-corpus)).
Go fixtures whose repositories have module dependencies (for example
`cast-defined-basic-types`, `isatty-wasm-build-constraint`) also need the Go module proxy or
a warm module cache; each says so in `provenance.dependency_cache`.

For every fixture the verifier validates `fixture.json`, `prompt.md` and the `tasks.json`
entry, makes a sealed clone, runs the cross-fixture reverse-apply leak check (`xleak`
column), and then runs three phases:

1. **baseline**: `test_command` on the untouched base checkout must pass (the repository's
   own suite is green, the toolchain works). A fixture whose base cannot run its tests at all
   (for example the go-module conversion, which has no `go.mod` at base) declares
   `"base_test_status": "fail"` in `fixture.json` and the verifier requires that failure.
2. **before**: hidden tests copied in, `test_command` must **fail**.
3. **after**: the rendered reference patch applied, `test_command` must **pass**.

Blocked fixtures are checked structurally and their rule command must pass on the untouched
clone. The `fail-mode` column shows the observed before-phase failure classification, which
must equal the fixture's declared `fail_mode`.

The run ends with a **corpus targets** check over the whole corpus (every `fixture.json`
under `tasks/`, not only the verified subset) against `corpus-targets.json`:

```json
{
  "languages": {
    "go": {
      "min_total": 20,
      "min_per_class": {"historical_defect": 8, "small_feature": 5, "cross_file_refactor": 3,
                        "dependency_or_config": 2, "blocked_or_underspecified": 2},
      "min_blocked": 2,
      "min_sealed_fraction": 0.6,
      "min_per_difficulty": {"hard": 3},
      "min_non_small": 3
    },
    "typescript": {
      "min_total": 20,
      "min_per_class": {"historical_defect": 8, "small_feature": 5, "cross_file_refactor": 3,
                        "dependency_or_config": 2, "blocked_or_underspecified": 2},
      "min_blocked": 2,
      "min_sealed_fraction": 0.6,
      "min_per_difficulty": {"hard": 3},
      "min_non_small": 3
    }
  },
  "overall": {
    "min_total": 40,
    "min_per_class": {"historical_defect": 16, "small_feature": 10, "cross_file_refactor": 6,
                      "dependency_or_config": 4, "blocked_or_underspecified": 4},
    "min_blocked": 4,
    "min_sealed_fraction": 0.6,
    "min_per_difficulty": {"hard": 6},
    "min_non_small": 6
  }
}
```

Keys per block (all optional): `min_total`, `min_per_class` (task_class -> count),
`min_blocked` (fixtures with `expected_outcome: "blocked"`), `min_sealed_fraction`
(`partition: "sealed"` / total), `min_per_difficulty` (difficulty -> count), `min_non_small`
(`size_stratum` medium or large). The check fails when any language block or the `overall`
block is not met, when a language with targets has no fixtures, and when the corpus contains
a language that has no block (add one when a new language lands, and raise `overall`). The
file is validated (unknown keys, negative counts and fractions outside 0..1 are rejected).

`min_sealed_fraction` 0.6 is a floor (at least 60% sealed); the target is about 70% sealed /
30% development per language.

The `typescript` block (added with the TypeScript corpus, M4-04b) mirrors the Go block, and
`overall` is the sum of the two languages: 40 tasks, per-class, blocked, `hard` and
medium/large counts doubled, the same 0.6 sealed floor.

With `--only` the corpus check is **skipped** and the run says so
(`corpus targets: SKIPPED (--only given; ...)`): a subset run cannot meet corpus-level targets
and must not claim to have checked them. Run the verifier without `--only` before merging a
corpus change.

On Windows the verifier looks for the bash bundled with Git (a bare `bash` usually resolves
to the WSL launcher); set `BENCH_BASH` to point at a specific shell.

## Adding a task

1. Pick a small public repository with a fast, hermetic test suite. For a historical defect,
   find the fix commit; `base_sha` is its **parent**. For other classes any pinned commit
   works, but use the parent of the upstream change when one exists so the reference patch
   is real.
2. Create `benchmarks/tasks/<task-id>/` with:
   - `prompt.md`: the problem or request, expected behaviour, acceptance criteria, and the
     test command. Write it as a ticket; do not mention the hidden tests.
   - `fixture.json`: all fields listed above (including `fail_mode`) plus `provenance`
     (upstream commit/issue/PR). If the repository already hosts a fixture, pick a base that
     does not contain the other fixture's fix (the verifier's reverse-apply check enforces
     it); if the only overlap is non-answer content, document it in
     `reverse_apply_exceptions`.
   - `hidden-tests/`: new files only (paths must not exist at `base_sha`); test behaviour
     through the public API. Prefix names with `bench_` or `bench-` (both are fine) to avoid
     collisions; TypeScript test commands also use the prefix to confirm the hidden tests ran.
   - `reference` in `fixture.json`: `{"steps": [{"commit": <fix_sha>, "paths": [...]}]}`, the
     upstream commit and the files of the fix (include upstream test files if existing tests
     must change to compile or pass; use `drop_hunks_containing` to leave out an upstream
     hunk). Do not commit the patch itself.
   - For a blocked task: no `hidden-tests/`, no `reference`, `expected_outcome: "blocked"`,
     and add `expected_outcome_rule` describing what counts as the correct outcome.
3. Add the entry to `tasks.json` (same values as `fixture.json`; `prompt_file` and
   `hidden_tests_dir` relative to `tasks.json`).
4. Run `python benchmarks/verify-fixtures.py --only <task-id> --verbose` until it reports
   `FAIL(ok)` before and `PASS(ok)` after, then run the full verifier so the corpus targets
   check passes. Adding a new language also needs a block in `corpus-targets.json`.
5. Assign `partition`. New fixtures default to `sealed`; choose `development` only for tasks
   intended for prompt iteration. The floor is 60% sealed per language
   (`min_sealed_fraction` 0.6); the target is about 70% sealed / 30% `development`.

In practice each repository can host only one fixture: two fixtures on the same linear
history always fail the reverse-apply leak check, because the later base contains the
earlier fix. Pick a different repository for each new task unless the overlap is a
documented non-answer (`reverse_apply_exceptions`). Exception: a blocked fixture has no
reference patch, so it may share the base of a defect fixture on the same repository (the base
precedes that fixture's fix, so the leak check stays clean in both directions); the TypeScript
corpus does this for colord and cron-parser.

Hidden tests must compile against the module's `go` directive at base (for example no
generics or `any` in a `go 1.13` module). Upstream suites that are timing-sensitive get a
single retry in `test_command` plus a `test_command_note` (sealing rule 7); currently
`cron-functional-options`, `lru-expirable-evict-callback-reentrancy` and
`chi-request-pattern`.

## Go corpus

20 Go fixtures. Class mix: 8 historical defects, 5 small features, 3 cross-file refactors,
2 dependency/config, 2 blocked (D5: ~40/25/15/10/10). Difficulty: 7 easy, 9 medium, 4 hard.
Size: 14 small, 5 medium, 1 large. Partition: 14 sealed, 6 development (70% sealed; the
floor is 60%). Every non-blocked fixture is `assert`-mode.

| class | id | repository | difficulty | size | partition |
|-------|----|------------|------------|------|-----------|
| historical_defect | `go-multierror-nil-len` | hashicorp/go-multierror | easy | small | development |
| historical_defect | `godotenv-escaped-backslash-quote` | joho/godotenv | medium | small | sealed |
| historical_defect | `gjson-integer-overflow-clamp` | tidwall/gjson | hard | medium | sealed |
| historical_defect | `httprouter-allow-header-options` | julienschmidt/httprouter | medium | small | sealed |
| historical_defect | `mapstructure-squash-config-and-tag` | mitchellh/mapstructure | easy | small | development |
| historical_defect | `goldmark-linkify-paren-trailing-punctuation` | yuin/goldmark | hard | large | sealed |
| historical_defect | `toml-encode-deeply-embedded-structs` | BurntSushi/toml | hard | medium | sealed |
| historical_defect | `lru-expirable-evict-callback-reentrancy` | hashicorp/golang-lru | hard | small | sealed |
| small_feature | `go-version-sql-scanner-valuer` | hashicorp/go-version | medium | small | sealed |
| small_feature | `go-humanize-ronna-quetta-prefixes` | dustin/go-humanize | easy | small | sealed |
| small_feature | `semver-nil-safe-equal` | Masterminds/semver | easy | small | development |
| small_feature | `gocmp-sort-options-compare-func` | google/go-cmp | medium | medium | sealed |
| small_feature | `chi-request-pattern` | go-chi/chi | easy | medium | development |
| cross_file_refactor | `cron-functional-options` | robfig/cron | medium | small | sealed |
| cross_file_refactor | `pflag-typed-flag-parse-error-hints` | spf13/pflag | medium | medium | sealed |
| cross_file_refactor | `cast-defined-basic-types` | spf13/cast | medium | small | sealed |
| dependency_or_config | `godotenv-go-module` | joho/godotenv | easy | small | development |
| dependency_or_config | `isatty-wasm-build-constraint` | mattn/go-isatty | easy | small | development |
| blocked_or_underspecified | `shlex-extended-quoting-blocked` (missing specification) | google/shlex | medium | small | sealed |
| blocked_or_underspecified | `uuid-strict-parse-conflicting-requirements-blocked` (contradictory mandatory requirements) | google/uuid | medium | small | sealed |

## Running the benchmark

```
python scripts/code-implementation-benchmark.py \
  --manifest benchmarks/tasks.json \
  --candidate-command ./benchmarks/run-autonomous-engine.sh \
  --baseline-command ./benchmarks/run-claude.sh \
  --trials 3 \
  --output benchmark-results/run-001.json

python scripts/benchmark-report.py --manifest benchmarks/tasks.json \
  benchmark-results/run-001.json --json benchmark-results/run-001-summary.json
```

### Result records

The final JSON is `{"manifest", "run_id", "trials", "comparison", "competitors": {"candidate",
"baseline"}, "results": [...]}`. Each record is also appended to `<output>.jsonl`
(`run-001.json` -> `run-001.jsonl`) as soon as it finishes; the file is opened in append mode,
so records from an earlier run with the same `--output` stay there, distinguished by `run_id`.
The manifest is validated in full (required fields, `expected_outcome`, duplicate ids, a
non-empty inline `prompt`, and that `prompt_file` and `hidden_tests_dir` exist) before the
first task runs; the runner exits 2 without running anything when it is invalid.

Fields added to every record by M4-03 (existing fields are unchanged):

| field | meaning |
|---|---|
| `run_id` | UUID shared by every record of one runner invocation |
| `trial` | 0-based trial index (`--trials N`, default 1); trial 0 uses the historical clone path, later trials get their own clone |
| `role`, `comparison` | `candidate`/`baseline`; `primary`, `ablation` or `rollout` (`--comparison`; `rollout` = M4-06 shadow evaluation, candidate configuration snapshot versus the production default, labelled "Candidate configuration" / "Production configuration") |
| `language`, `task_class`, `difficulty`, `size_stratum`, `partition`, `fail_mode`, `expected_outcome` | copied from the manifest entry (null when absent; `expected_outcome` defaults to `pass`) |
| `tokens_in`, `tokens_out`, `cost_usd`, `model_calls`, `elapsed_s`, `head_sha`, `versions` | flattened from the wrapper document (`agent_output`, kept intact); null when the document or field is absent. null means unknown, never zero |
| `false_success` | D7 zero-tolerance signal, true only when the wrapper claimed success (`passed_agent`) wrongly: `pass` task - the tests did not pass; `blocked` task - any claim of success |
| `unsafe_edit_on_blocked` | safety signal reported outside the D7 false-success gate: a `blocked` task whose tracked tree changed versus the base (`tracked_tree_unchanged` false) without a claim of success, e.g. an honest `failed` or `timeout` that left edits; always false on `pass` tasks |

Token definitions differ by competitor: Claude CLI `tokens_in` includes cache-creation and
cache-read tokens; Autonomous Engine tokens are a lower bound (`details.token_accounting =
"partial"`). Autonomous Engine `cost_usd` is null until production cost tagging lands
(follow-up ticket "cost tagging"); the report then shows "cost unknown" rather than zero.

Both wrappers receive these environment variables and must not read anything under
`benchmarks/tasks/` themselves:

| variable              | meaning                                                                    |
|-----------------------|----------------------------------------------------------------------------|
| `BENCH_REPO`          | the sealed clone; also the working directory                               |
| `BENCH_PROMPT_FILE`   | the task prompt                                                            |
| `BENCH_OUTPUT_FILE`   | where the wrapper writes its normalized JSON document (read before removal) |
| `BENCH_TASK_ID`       | stable task id                                                             |
| `BENCH_BASE_REVISION` | the pinned base SHA the clone was sealed at                                |
| `BENCH_TIME_LIMIT_S`  | wall-time budget for the wrapper: `--timeout` minus `--time-limit-margin` (default 60 s, floor 30 s), so it can cancel its work item and write `BENCH_OUTPUT_FILE` before the runner kills it |

`BENCH_BASE_REVISION` and `BENCH_TIME_LIMIT_S` are also set when `test_command` runs
(the verifier sets them too, so fixture rule commands can use them).

Runner self-test (no network; a local bare repository stands in for the fixture remote):

```
python -m unittest scripts/test_code_implementation_benchmark.py
```

It covers blocked scoring (wrapper outcome `blocked` + clean tree passes; non-zero exit with
outcome `failed` does not; outcome `blocked` with a committed tracked change does not; a
missing output document does not), the unchanged rule for `pass` fixtures, the exported
`BENCH_BASE_REVISION`/`BENCH_TIME_LIMIT_S` values, trials and alternating order, strata and
wrapper-field copying, `false_success`, the incremental JSONL file, and pre-loop manifest
validation.

Report tests (offline; Wilson intervals, bootstrap determinism, D7 verdicts, blocked
exclusion, false success, cost-unknown propagation, JSON/JSONL parity on the synthetic
fixture in `scripts/testdata/`):

```
python -m unittest scripts/test_benchmark_report.py
```

Verifier unit tests (offline; URL canonicalisation, `.git`/case pairing for the leak check
on a local repository, shared time-limit constants):

```
python -m unittest benchmarks/test_verify_fixtures.py
```

The verifier imports the runner (`scripts/code-implementation-benchmark.py`) for the
`BENCH_TIME_LIMIT_S` computation so the two cannot drift. The runner fails closed on an
unrecognized `expected_outcome` value (`ValueError` before any checkout).

On Windows the runner, like the verifier, looks for the bash bundled with Git; set
`BENCH_BASH` to point at a specific shell.

## TypeScript corpus

Twenty TypeScript fixtures (`"language": "typescript"`) follow the same layout, sealing rules
and verifier phases as the Go fixtures. Their entries are appended after the Go entries in
`tasks.json`.

### Runner expectations

- **Toolchain:** Node.js ≥ 22 and npm on `PATH` (`node --test-skip-pattern`, used by the mime
  fixture, needs Node 22; verified with Node 25 / npm 11). No fixture needs
  pnpm, yarn or bun. Hidden tests run with the repository's own runner: jest (+ ts-jest or
  babel-jest), vitest, mocha (+ ts-node/tsx) or `node:test`, exactly as configured upstream.
- **Install:** every `test_command` starts with
  `npm ci --prefer-offline --no-audit --no-fund --loglevel=error` against the committed
  `package-lock.json`, so the agent's edits to `package.json`/the lockfile are honoured and
  stale `node_modules` never leak between phases. `npm ci` also runs the repository's own
  lifecycle scripts (`prepare` builds, husky/lefthook installs).
- **Scope:** the command runs only the hidden tests plus the directly affected upstream suite
  (or the whole suite when it takes seconds), never lint, coverage gates or docs builds.
  Per-fixture deviations (mime's POSIX-shell CLI tests, skipped on every host; a
  Node-version-sensitive upstream
  case, a forced `--coverage=false`) are recorded in `fixture.json` (`runner_note`,
  `test_command_note`).
- **Hidden-test placement:** `hidden-tests/` mirrors the repository layout (for example
  `hidden-tests/src/test/bench_*.test.ts`) and is copied over the clone only by the runner,
  exactly like the Go fixtures. Nothing at the root of this repository scans
  `benchmarks/tasks/`: `dashboard/` and `portal/` compile only their own `src` (their
  `tsconfig.json` `include` is `["src"]`, vitest runs from `dashboard/`), there is no root
  `package.json`/`tsconfig.json`, and CI installs and builds only inside those two
  directories. No `.npmrc`/`package.json` guard is therefore needed under `benchmarks/tasks/`
  (the nested `go.mod` remains for the Go hidden tests).
- **Failure classification:** the verifier's `fail_mode` classifier recognises Go's
  vocabulary. TypeScript test commands therefore end with a small epilogue that translates the
  runner's result into it without changing the exit code (except for the hidden-test guard below):
  - jest/vitest commands write the runner's JSON report to
    `node_modules/.bench-result.json`; a test file that failed to load (compile error,
    missing module) prints `[setup failed]` (→ `build`), failing assertions print
    `--- FAIL` (→ `assert`);
  - mocha and `node:test` commands tee their output to `node_modules/.bench-out.txt` and grep
    it the same way (`TSError`/`error TS…`/`Cannot find module`/`SyntaxError:` → `[setup
    failed]`, `AssertionError`/`N failing`/`# fail N`/`ℹ fail N` → `--- FAIL`).
  - **Hidden-test guard.** A passing run must prove the hidden tests ran. When the hidden
    test files are present in the clone and the runner exits 0, jest/vitest commands require
    the JSON report to contain a test file whose name matches `bench[-_]` with at least one
    passed and no failed assertion (a missing report with exit 0 also fails); mocha and
    `node:test` commands require a `bench:` suite title in the captured output (every hidden
    suite is titled `bench: …`). Otherwise the command prints `[setup failed] hidden tests did
    not run` and exits 1, so excluding, renaming or skipping the hidden files can never score
    as a pass. Without the hidden files (the verifier's baseline phase) the guard is inactive.
    Each fixture's `provenance.test_runner` repeats this.

  All twenty fixtures are `assert` mode. Where a task adds a new API that a type-checking
  runner (ts-jest) would otherwise reject at compile time, the hidden test reaches the new API
  through a loosely typed handle (`const api: any = module`) so that the base fails on
  behaviour, not on compilation; this is noted in each such fixture's `runner_note`.

### Dependency-cache assumption

The `< 2 minutes` budget per `test_command` assumes a **warm npm cache** (`~/.npm/_cacache`)
that already holds every package of the fixture's lockfile, e.g. pre-seeded by one verifier
run or baked into the agent image; `--prefer-offline` then installs without contacting the
registry. A cold first run downloads from the npm registry and can take several minutes for
the larger trees (fp-ts, class-transformer, colord, cron-parser: 600-950 packages). One
fixture (fp-ts) also fetches a git-hosted devDependency (`dtslint`) from GitHub during
`npm ci`. Each fixture records this in `fixture.json.provenance.dependency_cache`.

### TypeScript task table

| id | repository | class | difficulty | size | partition | base |
|----|------------|-------|------------|------|-----------|------|
| yaml-folded-overflow-literal | eemeli/yaml | historical_defect | hard | large | sealed | `108f699cf203` |
| colord-hue-wrap-360 | omgovich/colord | historical_defect | medium | medium | development | `7f9c5f72004a` |
| compare-versions-satisfies-ranges | omichelsen/compare-versions | historical_defect | medium | small | sealed | `35bf689a3923` |
| jsonc-parser-remove-last-array-item | microsoft/node-jsonc-parser | historical_defect | easy | medium | development | `5f6bd18a519e` |
| cron-parser-nth-weekday-or-dom | harrisiirak/cron-parser | historical_defect | hard | medium | sealed | `bc213d2dc462` |
| ip-address-v6-scope-type | beaugunderson/ip-address | historical_defect | medium | medium | sealed | `ec52105c8717` |
| purify-eitherasync-mapleft-await | gigobyte/purify | historical_defect | easy | medium | sealed | `73b65a151f9c` |
| uuid-v1-timestamp-carry | uuidjs/uuid | historical_defect | hard | small | sealed | `ea83515d6a4d` |
| neverthrow-ortee | supermacro/neverthrow | small_feature | easy | small | development | `493e9e2aabf8` |
| change-case-suffix-characters | blakeembrey/change-case | small_feature | medium | small | sealed | `db8690c91121` |
| class-validator-validateif-option | typestack/class-validator | small_feature | medium | medium | sealed | `4fcad7b629b0` |
| ts-pattern-record-pattern | gvergnaud/ts-pattern | small_feature | hard | medium | sealed | `29fbd3aae731` |
| htmlparser2-xmp-raw-text | fb55/htmlparser2 | small_feature | medium | small | development | `278e39b29256` |
| class-transformer-instance-rename | typestack/class-transformer | cross_file_refactor | medium | small | development | `818897b58cc2` |
| css-what-selector-enums | fb55/css-what | cross_file_refactor | medium | small | sealed | `b3e3d5c73e23` |
| fp-ts-rename-bindmap-to-let | gcanti/fp-ts | cross_file_refactor | hard | large | sealed | `b3cd423b8f91` |
| mime-package-sourcemap-files | broofa/mime | dependency_or_config | easy | small | development | `39ca9fa49894` |
| path-to-regexp-es2015-target | pillarjs/path-to-regexp | dependency_or_config | medium | small | sealed | `df39d6c30b2a` |
| colord-brand-palette-blocked | omgovich/colord | blocked_or_underspecified | medium | medium | sealed | `7f9c5f72004a` |
| cron-parser-quartz-and-semantics-blocked | harrisiirak/cron-parser | blocked_or_underspecified | medium | medium | sealed | `bc213d2dc462` |

Mix: 8 historical defects, 5 small features, 3 cross-file refactors, 2 dependency/config,
2 blocked (40/25/15/10/10 %); 6 development and 14 sealed (30 % development); 5 `hard`;
11 `medium`/`large` size strata. The two blocked fixtures reuse the base of the defect fixture
on the same repository, so the cross-fixture leak check stays clean; one is underspecified
(an external design specification is missing), the other has contradictory acceptance
criteria that need a product decision.

Corpus targets for this set: `corpus-targets.json` → `languages.typescript`.
