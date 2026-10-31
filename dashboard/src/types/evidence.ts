// Verification evidence types matching internal/evidence (schema version 1)
// and the implementation_verifications table (migration 044).
//
// The dashboard renders what the producer recorded and never reinterprets
// the outcome: a skipped check is shown as skipped with its reason, and only
// `verified` means every recorded check passed; partially verified work is never reported as verified.

export type VerificationOutcome =
  | 'verified'
  | 'partially_verified'
  | 'failed'
  | 'unverified'

export type VerificationCheckKind =
  | 'test'
  | 'diagnostic'
  | 'build'
  | 'lint'
  | 'typecheck'
  | 'migration'
  | 'api_compat'
  | 'other'

export type VerificationCheckStatus = 'passed' | 'failed' | 'skipped' | 'not_run'

export interface VerificationCheck {
  kind: VerificationCheckKind
  command: string
  // null when the command did not run (skipped / not_run) or the runner could
  // not observe an exit code.
  exit_code: number | null
  status: VerificationCheckStatus
  // Mandatory when status is 'skipped'.
  skip_reason?: string
  started_at?: string
  finished_at?: string
  // Artifact id, object key, or URL of the stored raw output. Rendered as a
  // link only when it is an http(s) URL.
  output_ref?: string
}

export interface ImplementationEvidence {
  schema_version: number
  work_id: string
  task_id: string
  repository_id: string | null
  base_sha: string
  head_sha: string
  branch: string
  // Derived from git (diff base_sha...head_sha), never the planner's list.
  changed_files: string[]
  checks: VerificationCheck[]
  outcome: VerificationOutcome
  started_at: string
  finished_at: string
  artifact_refs: string[]
  // Operator-facing explanation of a failed or unverified run.
  reason?: string
  // Optional risk markers (additive within schema v1):
  // 'security_review_required' (D3) and 'zone_review_required' (codex
  // zones, migration 063). The release gate sends even verified work
  // carrying either to human review.
  flags?: string[]
  // Codex zones the changed files fall in, with their policy (additive).
  zones?: Array<{ name: string; policy: 'autonomous' | 'human_review' | 'escalate'; files?: string[] }>
}

// One implementation_verifications row as returned by
// GET /api/work-items/evidence and embedded (latest only) in
// GET /api/work-items/detail as `evidence`.
export interface ImplementationVerification {
  id: string
  created_at: string
  evidence: ImplementationEvidence
}
