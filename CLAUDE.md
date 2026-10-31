# Autonomous Engine — Developer Guidelines

## Migrations (`migrations/`)

Migrations are auto-applied by the `status-orchestrator` on startup via `internal/db/migrate.go`. Each `.up.sql` file runs inside a PostgreSQL transaction (except `ALTER TYPE ADD VALUE` statements, which the runner extracts and executes outside the transaction automatically).

### Rules for writing migrations

1. **Always use `IF NOT EXISTS` / `IF EXISTS` guards:**
   - `CREATE TABLE IF NOT EXISTS`
   - `CREATE INDEX IF NOT EXISTS`
   - `ALTER TABLE ... ADD COLUMN IF NOT EXISTS`
   - `DROP TABLE IF EXISTS`, `DROP INDEX IF EXISTS`
   - `ALTER TABLE ... DROP CONSTRAINT IF EXISTS` before `ADD CONSTRAINT`

   Without these, a partially-applied migration that crashes mid-way cannot be re-run.

2. **`ALTER TYPE ... ADD VALUE` cannot run inside a transaction (PostgreSQL limitation).**
   The migration runner handles this automatically — it extracts these statements and runs them outside the transaction. Always use `IF NOT EXISTS`:
   ```sql
   ALTER TYPE my_enum ADD VALUE IF NOT EXISTS 'new_value';
   ```

3. **Every foreign key MUST specify `ON DELETE` behavior.**
   Never write bare `REFERENCES parent(id)`. Always choose one:
   - `ON DELETE CASCADE` — child rows deleted when parent is deleted (use for owned data: artifacts, transitions, codex entries)
   - `ON DELETE SET NULL` — FK set to NULL when parent deleted (use for loose associations: work_items.product_id, chat_sessions.active_work_item_id)

   A bare `REFERENCES` defaults to `RESTRICT`, which silently blocks parent deletion and surfaces as a 500 error at runtime.

4. **Always provide both `.up.sql` and `.down.sql`.**

5. **Name constraints explicitly** so they can be dropped reliably in down migrations:
   ```sql
   ALTER TABLE foo DROP CONSTRAINT IF EXISTS foo_bar_id_fkey;
   ALTER TABLE foo ADD CONSTRAINT foo_bar_id_fkey
       FOREIGN KEY (bar_id) REFERENCES bar(id) ON DELETE CASCADE;
   ```

6. **Number migrations sequentially.** Check the highest existing number before creating a new one.

### Known historical issues (pre-043)

Migrations 001–035 were written without `IF NOT EXISTS` guards on most CREATE TABLE, CREATE INDEX, CREATE TYPE, and ALTER TABLE ADD COLUMN statements. These work fine on fresh installs and normal upgrades (the runner skips already-applied versions), but will fail if a migration is partially applied and needs to be re-run. If you encounter a dirty migration state, you may need to manually complete the partial migration and insert the version into `schema_migrations`.

Migration 043 fixed missing `ON DELETE CASCADE/SET NULL` on foreign keys in migrations 029, 030, and 032. Migration 042 fixed the same issue on `user_interactions.product_id` from migration 024.

## Build & Deploy

- **Backend:** `docker compose build dashboard-backend && docker compose up -d dashboard-backend`
- **Frontend:** Build first (`cd dashboard && npm run build`), then `docker compose build dashboard-frontend && docker compose up -d dashboard-frontend`
- **Migrations:** Rebuild and restart `status-orchestrator` — it runs all pending migrations on startup.
- **All services share** `internal/db` — changes to `migrate.go` require rebuilding any service that imports it.

## Architecture Notes

- Dashboard backend serves the API on port 8080; the frontend (nginx) is on port 3000/3443.
- WebSocket hub (`websocket.go`) broadcasts to channels; subscribe in frontend via the `useWebSocket` hook.
- Verification runs before the PR (R14): `implementation.complete` → verification stage (review-agent's second consumer group, `internal/verifier/stage`) → `verification.complete` → pr-coordinator opens a PR / pending change only on `approve` or `approve_pending_second_pass`; the parent sits in the `verification` state meanwhile and status-orchestrator settles it from `verification_results`.
- Credentials use scoped storage: `scope` + `scope_target` + `category` + `key_name` as unique key in `credential_registry`.
- LLM provider API keys are scoped to `scope='provider', scope_target=<providerID>` — not the shared `global/default` row.
- GitHub accounts support multiple PATs per username via the `label` column (unique on `username, label`).
