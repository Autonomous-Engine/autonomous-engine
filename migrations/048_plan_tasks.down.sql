-- Rollback 048. Dropping the tables loses the planner's held-task state: any
-- task still 'held' will never be released. Drain or re-plan held work first.

DROP INDEX IF EXISTS idx_plan_task_dependencies_depends_on;
DROP TABLE IF EXISTS plan_task_dependencies;
DROP INDEX IF EXISTS idx_plan_tasks_parent_status;
DROP TABLE IF EXISTS plan_tasks;
