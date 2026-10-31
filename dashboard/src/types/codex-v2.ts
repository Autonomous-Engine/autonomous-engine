// dashboard/src/types/codex-v2.ts
//
// TypeScript interfaces mirroring the codex v2 REST response shapes.
// Keep in sync with the Go backend structs in internal/codex/*.go.

export interface CodexBlob {
  hash: string
  section: string
  extractor: string
  payload: Record<string, unknown>
  confidence: number
  created_at: string
}

export interface CodexChangedPair {
  from: CodexBlob
  to: CodexBlob
}

export interface CodexDiffResponse {
  added: CodexBlob[]
  removed: CodexBlob[]
  changed: CodexChangedPair[]
}

export interface ContractEdge {
  hash: string
  contract_type: string
  contract_name: string
  producer_repo_id: string
  producer_file: string
  producer_line: number
  consumer_repo_id: string
  consumer_file: string
  consumer_line: number
  confidence: number
  extracted_at: string
}

export interface TopologyEntry {
  repo_id: string
  role: string
  confidence: number
  evidence: Record<string, unknown>
}

export interface WatchedBranch {
  repo_id: string
  branch: string
  source: string
  added_by: string
  added_at: string
  enabled: boolean
  last_snapshot_commit: string
  last_snapshot_time: string | null
}

export interface GitStrategy {
  repo_id: string
  pattern: string
  main_branch: string
  long_lived_branches: string[]
  merge_strategy: string
  pr_required: boolean
  evidence: Record<string, unknown>
  extracted_at: string
}

export interface CodexSnapshot {
  repo_id: string
  commit_sha: string
  branch: string
  parent_sha: string
  blob_hashes: string[]
  extracted_at: string
}

export interface SnapshotWithBlobs {
  repo_id: string
  commit_sha: string
  branch: string
  parent_sha: string
  extracted_at: string
  blobs: CodexBlob[]
}

export interface ContractEdgeFilters {
  product_id: string
  contract_type?: string
  producer_repo?: string
  consumer_repo?: string
  limit?: number
  offset?: number
}
