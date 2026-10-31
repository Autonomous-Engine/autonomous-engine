# verifier-eval

Developer tool (not a service) for M3-07: measures the independent verifier
(`internal/verifier`) against seeded defects, next to the author-only LLM
review it now precedes.

## Corpus

`internal/verifier/testdata/seeded/<NN-name>/` — one directory per fixture:

| File | Meaning |
|---|---|
| `fixture.json` | criteria (`plan.Criterion`), task spec (`plan.TaskSpec`), `author_summary` (the persuasive PR description; only the author-only baseline sees it), `expect` (criterion ids the defect breaks, the seeded file and line) |
| `base/` | tree of the base commit |
| `head/` | files written over base for the head commit (`delete` in fixture.json removes files) |

Each fixture is materialized as a real git repository (base commit, head
commit) — the same shape review-agent verifies. The eight defect classes:
off-by-one, missing error path, unhandled nil, wrong status code, swallowed
error, test that asserts nothing, security path touched without validation,
criterion silently dropped. In every fixture the head's own tests pass: the
author's evidence is green and the defect is still there.

## Running

```sh
# Dry run (no model): validate and materialize every fixture, check the
# seeded file is in the diff, the author summary is excluded from the
# verifier prompt, and the author's tests pass.
make verifier-eval            # or: go run ./cmd/verifier-eval -dry-run

# Provider run: both reviewers on every fixture, Markdown detection table.
VERIFIER_EVAL_PROVIDER=anthropic VERIFIER_EVAL_MODEL=<model id> VERIFIER_EVAL_API_KEY=... \
  go run ./cmd/verifier-eval -out verifier-eval.md
```

Provider env: `VERIFIER_EVAL_PROVIDER` (`anthropic`, `openai`, `ollama`,
`vllm`, `lmstudio`, `custom`), `VERIFIER_EVAL_MODEL` (required),
`VERIFIER_EVAL_ENDPOINT`, `VERIFIER_EVAL_API_KEY`. `ANTHROPIC_API_KEY`
selects `anthropic` when no provider is named. Without a provider the tool
runs the dry run and says so; `-dry-run` never reads the provider env.
`go test ./cmd/verifier-eval/` runs the dry run; `TestEvaluateAgainstProvider`
runs the provider evaluation only with `VERIFIER_EVAL=1` set explicitly (a
key in the environment alone never makes a test spend money).

A detection is counted for the verifier when its gate requests changes and an
expected criterion is `not_met` or a critical/major defect names the seeded
file; for the author-only review when review-agent's keyword rule yields
`request_changes` and the review names the seeded file. Both reviewers use
the same model, so the model is held constant — but the comparison measures
the whole verifier stage against today's review, not context isolation
alone: the verifier also receives the acceptance criteria and task spec
(the baseline does not) and may run the tests with `run_command` (the
baseline may not). A higher verifier detection rate says the stage catches
more; it does not attribute the difference to removing the author's
summary. Isolating that would need a third arm (the verifier's input plus
the author summary), which this harness does not run.
