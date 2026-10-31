// Impact-selected check plan types matching internal/checks (M3-04). The
// implementation agent stores each task's plan as a work-item artifact whose
// content is { type: 'check_plan', task_id, plan, computed_at }. The fast
// targets narrow only the agent's pre-commit loop; the final gate always
// runs the full suite (plan.full).

export interface CheckTarget {
  kind: string
  // Absent when the target cannot be executed; not_run_reason says why.
  command?: string
  scope?: string[]
  reason: string
  not_run_reason?: string
  // Reported to the agent but never fails the fast loop or the evidence
  // (go vet / go build the policy does not require).
  advisory?: boolean
  // Set after a run that executed no test (an all-skipped migration run):
  // shown here, never recorded as evidence.
  skipped?: string
}

export interface CheckPlan {
  language?: string
  changed_files: string[]
  targets: CheckTarget[]
  full: CheckTarget[]
  // Set when the fast targets fell back to the full suite.
  fallback?: string
}

export interface TaskCheckPlan {
  task_id: string
  computed_at: string
  plan: CheckPlan
}
