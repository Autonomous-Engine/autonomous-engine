# Autonomous Engine

Autonomous Engine turns a written request ("fix the basemap error", "add an export button") into a reviewed pull request. It runs as a set of Go services on Kafka and PostgreSQL, uses a local or hosted LLM for each stage, and refuses to open a pull request for work it cannot verify.

## How a request flows

1. **Intake and PRD.** A request from the dashboard, chat, Jira or a webhook becomes a work item with a product requirements document. PRD approval is a human gate.
2. **Research.** Parallel research tasks read the repository through a code knowledge graph ("codex") and a vector index, seeded with the files the request names.
3. **Planning.** The planner decomposes the work into tasks with acceptance criteria and the checks that prove each one, and repairs or normalises the plan before anything runs.
4. **Implementation.** An agent edits a git worktree pinned to a base commit, with a per-edit syntax check, bounded repair rounds, and a pre-commit test gate.
5. **Verification before any PR.** An independent verifier judges the change against every acceptance criterion in an isolated scratch clone. Small, actionable problems get a bounded fix round; anything else goes to human review. No pull request exists until verification approves.
6. **PR gate, review and QA.** The PR is opened (or held at a manual gate), reviewed, and checked by QA before release.

Every stage is budgeted per work item, idempotent under redelivery, and recorded in an evidence trail shown in the dashboard.

## Requirements

- **Docker Desktop** (or Docker Engine with Compose v2). The full stack is about 25 containers; give Docker at least 16 GB of memory, more if the model runs on the same machine.
- **An LLM.** An [Ollama](https://ollama.com) host is the default (a 30B-class coding model such as `qwen2.5-coder:32b` works); OpenAI and Anthropic providers can be added in the dashboard.
- **For development:** Go 1.22 or newer, Node.js 22 for the dashboard, Python 3.10+ for the benchmark scripts.
- **Windows:** clone into a short path or run `git config --global core.longpaths true` first.

## Quick start

```bash
git clone https://github.com/Autonomous-Engine/autonomous-engine.git
cd autonomous-engine
cp .env.example .env
```

Edit `.env` and set three values. The dashboard refuses to start with the placeholder JWT secret, and without a valid master key the engine cannot store credentials:

```bash
DB_PASSWORD=<output of: openssl rand -hex 16>
MASTER_ENCRYPTION_KEY=<output of: openssl rand -base64 32>
JWT_SECRET=<output of: openssl rand -hex 32>
```

Build and start the stack. Building one image at a time keeps memory use down:

```bash
COMPOSE_PARALLEL_LIMIT=1 docker compose build
docker compose up -d
```

On first start `status-orchestrator` applies the database migrations and creates the admin account `admin@localhost` with a random password. Read it from the log:

```bash
docker compose logs status-orchestrator | grep -A4 "FIRST-RUN ADMIN"
```

Open the dashboard at <http://localhost:32900>, sign in, and set a new password. Then:

1. **Add an LLM provider and an embedding provider** under Admin → Providers (your Ollama URL, or an API key for a hosted provider). Until an embedding provider exists, `knowledge-indexer` logs that none is configured and restarts; that is expected.
2. **Create a product and onboard a repository.** The onboarding wizard indexes the code and asks for the repository's verification policy (its test command, or whether work without tests may go to human review).
3. **Submit a request** from the dashboard. Watch it move through the stages; each one shows its evidence.

To receive GitHub webhooks (pushes and merged PRs), point the repository's webhook at `webhook-listener` (`/webhook/github`, port 8081, JSON content type) and save the same secret under Admin → Integrations → GitHub → Webhook Secret (or set `GITHUB_WEBHOOK_SECRET`). Every delivery's signature is checked against it; without a secret, deliveries are refused.

**Network exposure.** The gateway intake API (port 8082), the webhook listener (8081) and the dashboard API (8080) have no login of their own, so compose publishes them on `127.0.0.1` only. To receive webhooks from GitHub or Jira, put a reverse proxy or tunnel in front of port 8081 rather than opening it directly. The dashboard UI (port 32900) and the customer portal are the only ports published on every interface; keep the host on a trusted network.

## Services

| Service | Role |
|---|---|
| `gateway` | HTTP/WebSocket entry point |
| `status-orchestrator` | Work-item state machine, migrations, prompts and gates |
| `product-agent` | Turns a request into a PRD |
| `research-coordinator`, `research-agent` | Plans and runs research tasks |
| `implementation-planner` | Decomposes work into verifiable tasks |
| `implementation-agent` | Edits code in pinned worktrees and runs the checks |
| `review-agent` | Verification stage before the PR, then code review |
| `pr-coordinator` | Opens pull requests or holds them at the gate |
| `qa-agent` | Post-merge QA |
| `chat-agent` | Conversational access to work items and code |
| `github-service`, `webhook-listener`, `change-reconciler`, `jira-sync` | Source-control and tracker integration |
| `knowledge-indexer` | Builds the codex and the vector index |
| `credential-monitor` | Checks stored credentials |
| `onboarding` | Repository onboarding |
| `dashboard-backend`, `dashboard-frontend` | Operator dashboard |
| `portal` | Customer-facing request portal |
| `postgres`, `kafka`, `qdrant` | Storage, event bus, vector store |

Redis is optional and off by default; a single-node install does not use it. Set `REDIS_URL` when you run more than one replica of a service: the dashboard then relays WebSocket events so clients on every `dashboard-backend` replica receive them, and the portal's per-IP limit and the LLM router's rate limit are shared between replicas instead of counted per process. With compose, `docker compose --profile scale up -d` starts a bundled Redis; in the Helm chart under `deploy/helm`, set `redis.enabled=true` (or `redis.url` for an external one). `dashboard-backend` refuses to start when `DASHBOARD_REPLICAS` (set by the chart from `dashboardBackend.replicas`) is above 1 and `REDIS_URL` is empty. If Redis becomes unreachable, the services fall back to per-process behaviour and count it in `ae_redis_fallback_total`.

Shared code lives under `internal/`, migrations under `migrations/`, container builds under `docker/`, a Helm chart under `deploy/helm`, and the benchmark corpus under `benchmarks/`.

## Running the tests

```bash
go build ./... && go vet ./... && go test ./...
cd dashboard && npm ci && npx vitest run
python -m unittest discover -s scripts -p "test_*.py"
```

Database integration tests run when `TEST_POSTGRES_DSN` points at a disposable database (they apply the migrations themselves); run them with `-p 1` when several packages share one database. The end-to-end evidence test in `internal/e2e` runs against a live stack from inside the compose network; see `CONTRIBUTING.md` for the exact command.

## Contributing

See `CONTRIBUTING.md`. Security issues: see `SECURITY.md`.

## License

Licensed under the Apache License, Version 2.0. See `LICENSE`, `NOTICE` and `THIRD_PARTY_NOTICES.md`.
