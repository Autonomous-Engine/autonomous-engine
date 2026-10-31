-- 048: plan_tasks, plan_task_dependencies
-- Persisted execution state for the implementation-planner (roadmap 1.3,
-- ticket M1-06). Before this, the planner held its held-task queue, its
-- completed set and its title -> task id map in process memory: a restart lost
-- them, two replicas could not share them, and the dependency path was dormant
-- because decomposition always sent Dependencies = nil.
--
-- One plan_tasks row per repo-scoped subtask, written in the SAME transaction
-- as the subtask's work_items row and (for tasks with no unmet dependencies)
-- its implementation.tasks outbox event. status:
--   held      - waiting for every depends_on task to be completed; no event
--               has been enqueued for it.
--   released  - its implementation.tasks event has been enqueued.
--   completed - implementation.complete carried a release decision (M0-07).
--   failed    - implementation.complete carried a block decision. Terminal:
--               a failed task never satisfies a dependency (D1).
--
-- message is the exact implementation.tasks payload to enqueue on release.
-- A held task's message cannot be rebuilt later (it carries the brief prose,
-- codex patterns and file scope as they were at planning time), so it is
-- stored with the row; a restarted planner releases from the table alone.

CREATE TABLE IF NOT EXISTS plan_tasks (
    id              UUID        PRIMARY KEY,
    parent_work_id  UUID        NOT NULL,
    title           TEXT        NOT NULL,
    repository_id   UUID        NULL,
    status          TEXT        NOT NULL,
    message         JSONB       NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    released_at     TIMESTAMPTZ NULL,
    completed_at    TIMESTAMPTZ NULL
);

ALTER TABLE plan_tasks DROP CONSTRAINT IF EXISTS plan_tasks_status_check;
ALTER TABLE plan_tasks ADD CONSTRAINT plan_tasks_status_check
    CHECK (status IN ('held', 'released', 'completed', 'failed'));

-- The row is the planner's view of a subtask work item: deleting the subtask
-- (or its parent) deletes the plan state with it.
ALTER TABLE plan_tasks DROP CONSTRAINT IF EXISTS plan_tasks_id_fkey;
ALTER TABLE plan_tasks ADD CONSTRAINT plan_tasks_id_fkey
    FOREIGN KEY (id) REFERENCES work_items(id) ON DELETE CASCADE;

ALTER TABLE plan_tasks DROP CONSTRAINT IF EXISTS plan_tasks_parent_work_id_fkey;
ALTER TABLE plan_tasks ADD CONSTRAINT plan_tasks_parent_work_id_fkey
    FOREIGN KEY (parent_work_id) REFERENCES work_items(id) ON DELETE CASCADE;

ALTER TABLE plan_tasks DROP CONSTRAINT IF EXISTS plan_tasks_repository_id_fkey;
ALTER TABLE plan_tasks ADD CONSTRAINT plan_tasks_repository_id_fkey
    FOREIGN KEY (repository_id) REFERENCES repositories(id) ON DELETE SET NULL;

-- Completion locks a plan's rows and the release query scans the plan's held
-- tasks; startup reconstruction looks for plans that still have held tasks.
CREATE INDEX IF NOT EXISTS idx_plan_tasks_parent_status
    ON plan_tasks (parent_work_id, status);

CREATE TABLE IF NOT EXISTS plan_task_dependencies (
    task_id             UUID NOT NULL,
    depends_on_task_id  UUID NOT NULL
);

ALTER TABLE plan_task_dependencies DROP CONSTRAINT IF EXISTS plan_task_dependencies_pkey;
ALTER TABLE plan_task_dependencies ADD CONSTRAINT plan_task_dependencies_pkey
    PRIMARY KEY (task_id, depends_on_task_id);

ALTER TABLE plan_task_dependencies DROP CONSTRAINT IF EXISTS plan_task_dependencies_task_id_fkey;
ALTER TABLE plan_task_dependencies ADD CONSTRAINT plan_task_dependencies_task_id_fkey
    FOREIGN KEY (task_id) REFERENCES plan_tasks(id) ON DELETE CASCADE;

ALTER TABLE plan_task_dependencies DROP CONSTRAINT IF EXISTS plan_task_dependencies_depends_on_task_id_fkey;
ALTER TABLE plan_task_dependencies ADD CONSTRAINT plan_task_dependencies_depends_on_task_id_fkey
    FOREIGN KEY (depends_on_task_id) REFERENCES plan_tasks(id) ON DELETE CASCADE;

ALTER TABLE plan_task_dependencies DROP CONSTRAINT IF EXISTS plan_task_dependencies_no_self_check;
ALTER TABLE plan_task_dependencies ADD CONSTRAINT plan_task_dependencies_no_self_check
    CHECK (task_id <> depends_on_task_id);

-- The release query walks edges from a held task to what it depends on (the
-- primary key covers that); this index serves the reverse direction.
CREATE INDEX IF NOT EXISTS idx_plan_task_dependencies_depends_on
    ON plan_task_dependencies (depends_on_task_id);
