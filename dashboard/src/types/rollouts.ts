// M4-06: rollouts, D7 decisions, override audit, replay opt-in.

export type RolloutStatus = 'proposed' | 'shadow' | 'evaluating' | 'approved' | 'blocked' | 'rolled_back'
// INCOMPLETE: no gate blocked or was inconclusive, but a gate had no data;
// never an approval (approve only with an audited override).
export type DecisionVerdict = 'APPROVED' | 'BLOCKED' | 'MORE_TRIALS' | 'INCOMPLETE'
export type GateVerdict = 'PASS' | 'BLOCK' | 'MORE_TRIALS' | 'NOT_EVALUATED'

export interface DecisionGate {
  name: string
  verdict: GateVerdict
  reason: string
  data?: Record<string, unknown>
}

export interface RateSummary {
  passes: number
  attempts: number
  tasks: number
  rate: number | null
  wilson95: number[] | null
}

export interface RolloutDecision {
  policy: string
  verdict: DecisionVerdict
  summary: string
  gates: DecisionGate[]
  evaluated_at: string
  not_evaluated?: string[]
  cost_note: string
  report: {
    title?: string
    candidate: string
    baseline: string
    partition_filter: string | null
    // field → value → competitor → rate
    strata?: Record<string, Record<string, Record<string, RateSummary>>>
    paired?: { ci95_pp: number[] | null; mean_difference_pp: number | null; paired_tasks: number; min_trials_per_task: number }
  } | null
}

export interface Rollout {
  id: string
  candidate_snapshot_id: string | null
  baseline_snapshot_id: string | null
  scope: 'global' | 'product'
  product_id: string | null
  status: RolloutStatus
  created_by: string
  created_at: string
  updated_at: string
  decided_at: string | null
  decided_by: string | null
  decision_report: RolloutDecision | null
  notes: string
}

export interface PerformanceRow {
  group: string
  items: number
  released: number
  rate: number | null
}

export interface RolloutDetail {
  rollout: Rollout
  verdict: DecisionVerdict | ''
  candidate_performance: Record<string, PerformanceRow[]>
  baseline_performance: Record<string, PerformanceRow[]>
  not_evaluated?: string[] | null
  scope_note?: string
}

export interface RolloutOverride {
  id: string
  rollout_id: string | null
  action: string
  scope: string
  before_snapshot_id: string | null
  after_snapshot_id: string | null
  override_reason: string
  actor: string
  created_at: string
}

export interface ProductReplayFlag {
  id: string
  name: string
  allow_replay: boolean
  // The product is a scratch target shadow runs and replays may use.
  benchmark_scratch: boolean
}
