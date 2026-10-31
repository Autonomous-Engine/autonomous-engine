# Contributing to Autonomous Engine

Thanks for helping. This guide covers how to propose a change, the rules the codebase relies on, and how to test.

## Workflow

1. Open an issue first for anything larger than a small fix, so the approach can be agreed before you write it.
2. Fork the repository and create a branch from `main`.
3. Make the change with tests (see below). Keep each pull request to one concern.
4. Sign off every commit (Developer Certificate of Origin, <https://developercertificate.org>): `git commit -s` adds `Signed-off-by: Your Name <you@example.com>`. Pull requests without sign-off cannot be merged.
5. Open a pull request against `main`. CI runs the Go build and tests, the dashboard and portal builds, and a secret scan.

## Rules the codebase relies on

- **Fail closed.** Unknown or partial states are never treated as success. Only `verified` evidence may pass the automated gate; a repository with no executable checks blocks by default unless the product opts in to human review.
- **Every state-changing publish goes through the transactional outbox,** and every consumer is idempotent under redelivery (the delivery ledger in `internal/idempotency`).
- **Budgets are per work item.** Do not raise a token or time budget to make a run pass; fix the cause.
- **Comments and user-facing text stand on their own.** State the rule in the comment; do not point at documents that are not in this repository.

### Migrations (`migrations/`)

Migrations are applied by `status-orchestrator` on startup, each `.up.sql` inside a transaction (the runner moves `ALTER TYPE ... ADD VALUE` statements outside it).

1. Use `IF NOT EXISTS` / `IF EXISTS` guards on every `CREATE`, `DROP` and `ADD COLUMN`, and `DROP CONSTRAINT IF EXISTS` before `ADD CONSTRAINT`.
2. Write `ALTER TYPE my_enum ADD VALUE IF NOT EXISTS 'new_value';` for enum additions.
3. Every foreign key states its `ON DELETE` behaviour: `CASCADE` for owned data, `SET NULL` for loose associations. Never a bare `REFERENCES`.
4. Provide both `.up.sql` and `.down.sql`.
5. Name constraints explicitly so the down migration can drop them.
6. Number migrations sequentially after the highest existing one.

## Tests

```bash
go build ./... && go vet ./... && go test ./...
cd dashboard && npm ci && npx vitest run && npx tsc --noEmit
python -m unittest discover -s scripts -p "test_*.py"
python scripts/third_party_notices.py --check
```

**Database integration tests** run when `TEST_POSTGRES_DSN` names a disposable PostgreSQL database; the tests apply the migrations themselves. Use `-p 1` when several packages share one database:

```bash
TEST_POSTGRES_DSN='postgres://user:pass@localhost:5432/ae_test?sslmode=disable' \
  go test -p 1 ./cmd/... ./internal/... -run Integration -count=1
```

**End-to-end evidence test** (`internal/e2e`) runs against a live stack from inside the compose network:

```bash
docker run --rm --network autonomous-engine_agent-net -v "$PWD:/src" -w /src \
  -e TEST_POSTGRES_DSN='postgres://ae_user:<DB_PASSWORD>@postgres:5432/autonomous_engine?sslmode=disable' \
  -e TEST_KAFKA_BROKERS=kafka:9092 -e E2E_COMPAT_WINDOW=legacy -e E2E_HUMAN_REVIEW_REQUIRED=true \
  golang:1.24 go test ./internal/e2e/ -count=1 -v
```

New behaviour needs a test that fails without it. Tests that scan source files must normalise CRLF line endings.

## Dependencies and licenses

New Go or npm dependencies must be under Apache-2.0, MIT, BSD, ISC, MPL-2.0 or another permissive license. After changing dependencies, regenerate the inventories in `third_party/` (the commands are in the docstring of `scripts/third_party_notices.py`), collect the license texts with `python scripts/collect_license_texts.py`, and run `python scripts/third_party_notices.py --write`; CI fails if `THIRD_PARTY_NOTICES.md` is stale.

## Benchmarks

`benchmarks/` holds a sealed corpus of real upstream bug fixes and features with hidden tests. `benchmarks/README.md` explains how to verify fixtures and run paired comparisons; the corpus ships no upstream code, only each task's upstream repository and commits, which the tools fetch at run time.

## Code of conduct

This project follows the Contributor Covenant; see `CODE_OF_CONDUCT.md`.
