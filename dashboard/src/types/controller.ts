// Implementation controller types matching internal/controller and the
// implementation_runs / implementation_run_events tables (migration 052).
//
// A run's state advances only from tool evidence recorded by the
// implementation agent (write_file/edit_file paths, run_command exit codes,
// git_diff/read_file after green) and, for final_verified, from the final
// gate's evidence outcome. The dashboard shows these rows as recorded and
// never infers a phase from anything else (M3-02).

export type ImplementationPhase =
  | 'inspect'
  | 'test_authored'
  | 'red_observed'
  | 'implement'
  | 'green_observed'
  | 'diff_reviewed'
  | 'final_verified'
  | 'blocked'
  | 'failed'

// The transition row that recorded red or green.
export interface PhaseEvidence {
  seq: number
  tool_call_id: string
  tool: string
  command?: string
  exit_code: number | null
  at: string
}

// The latest attempt of one task, from GET /api/work-items/detail.
export interface ImplementationRun {
  id: string
  work_item_id: string
  task_id: string
  attempt: number
  // Typed as string as well: an unknown future state is shown verbatim.
  state: ImplementationPhase | string
  started_at: string
  updated_at: string
  last_event_seq: number
  red: PhaseEvidence | null
  green: PhaseEvidence | null
  // M3-06: implementation_runs.detail.repair / .budget as the agent
  // recorded them; absent for runs without a pre-commit check or budget.
  repair?: RepairState
  budget?: BudgetState
}

// One normalised failure (internal/failures.Record) with its history in
// the repair loop.
export interface FailureSummary {
  fingerprint: string
  category: string
  tool?: string
  command?: string
  exit_code?: number | null
  file?: string
  line?: number
  col?: number
  span?: string
  message: string
  test?: string
  ownership: 'changed_file' | 'dependency' | 'environment' | string
  retryable: boolean
  raw_ref?: string
  count: number
  first_run: number
  last_run: number
  last_call_id?: string
}

export interface RepairAttempt {
  attempt: number
  after_call_id: string
  fingerprints: string[]
  outcome?: string
  error?: string
}

export interface CheckRun {
  run: number
  call_id: string
  seq?: number
  passed: boolean
  fingerprints?: string[]
  new?: string[]
  repeated?: string[]
  raw_ref?: string
}

// A plan revision and the evidence that contradicted the plan.
export interface PlanRevision {
  type: 'plan_revision'
  seq?: number
  evidence_call_id: string
  evidence_seq?: number
  fingerprint: string
  reason: string
  revision: string
  category?: string
  location?: string
}

export interface RepairState {
  max_repairs: number
  repairs: number
  runs: number
  stop_reason?: string
  stop_detail?: string
  failures: FailureSummary[]
  attempts: RepairAttempt[]
  check_runs: CheckRun[]
  revisions: PlanRevision[]
}

// D4 budget of the attempt.
export interface BudgetState {
  token_limit: number
  tokens_used: number
  active_limit_seconds: number
  active_seconds: number
  paused_seconds: number
  paused: boolean
  pauses?: { prompt_id: string; trigger?: string; from: string; to?: string; seconds: number }[]
  exhausted?: string
  exhausted_detail?: string
  started_at: string
  updated_at: string
}
