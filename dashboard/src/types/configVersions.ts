// M4-05: configuration snapshots, outcome rollups, model prices.

export interface ConfigSnapshotRollup {
  id: string
  agent_type: string
  provider: string
  model_id: string
  agent_definition_id: string | null
  agent_definition_name: string | null
  created_at: string
  items: number
  verified: number
  verified_rate: number | null
  defect_data_items: number
  defects_critical: number
  defects_major: number
  defects_minor: number
  human_edit_known: number
  human_edited: number
  human_edit_rate: number | null
  false_success: number
  reverted: number
  inference_calls: number
  tokens_in: number
  tokens_out: number
  cost_usd: number | null
  cost_unknown: boolean
  unpriced_calls: number
  is_current_default: boolean
  referenced_by_items: number
  // cost_usd above covers calls tagged with this snapshot (one agent type).
  // item_cost_usd sums work_item_outcomes.cost_usd (every agent's calls on
  // the items rolled up here); null while any item's cost is unknown.
  item_cost_usd: number | null
  item_cost_unknown: boolean
  item_cost_unknown_items: number
}

export interface ConfigSnapshotChange {
  path: string
  kind: 'added' | 'removed' | 'changed'
  before?: unknown
  after?: unknown
}

export interface ConfigSnapshotDiff {
  a: string
  b: string
  b_is_current: boolean
  identical: boolean
  changes: ConfigSnapshotChange[]
}

export interface ModelPrice {
  id: string
  provider: string
  model_id: string
  input_per_1m: number | null
  output_per_1m: number | null
  effective_from: string
  updated_at: string
}

// work_item_outcomes row as served on the work-item detail (no user text).
export interface WorkItemOutcome {
  work_item_id: string
  config_snapshot_id: string | null
  verified_head_sha: string | null
  reviewed_head_sha: string | null
  release_decision: 'release' | 'legacy_release' | 'human_review' | 'block' | null
  review_defects: { critical?: number; major?: number; minor?: number } | null
  human_review_required: boolean
  human_edits_after_review: boolean | null
  qa_result: 'pass' | 'fail' | null
  pr_number: number | null
  merge_sha: string | null
  merged_at: string | null
  reverted_at: string | null
  revert_detection: 'heuristic' | null
  false_success: boolean
  tokens_in: number
  tokens_out: number
  cost_usd: number | null
}
