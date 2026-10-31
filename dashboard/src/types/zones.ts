// Codex zones (migration 063): per-repository path globs with a policy.
// Mirrors internal/zones (Zone, Template) and the onboarding service's
// repoZoneProposal.

export type ZonePolicy = 'autonomous' | 'human_review' | 'escalate'
export type ZoneStatus = 'active' | 'frozen' | 'thawing'
export type ZoneSource = 'config' | 'onboarding' | 'dashboard'

export const ZONE_POLICIES: ZonePolicy[] = ['autonomous', 'human_review', 'escalate']

export const ZONE_POLICY_HELP: Record<ZonePolicy, string> = {
  autonomous: 'agents may change matching files without an extra gate',
  human_review: 'changes are allowed, but a human must approve the PR (even with HUMAN_REVIEW_REQUIRED=false)',
  escalate: 'a change to matching files stops the subtask with an escalation before verification',
}

export interface CodeZone {
  id: string
  repository_id: string
  name: string
  description: string
  patterns: string[]
  module_paths: string[]
  policy: ZonePolicy
  status: ZoneStatus
  frozen_reason?: string | null
  frozen_at?: string | null
  source: ZoneSource
  created_at: string
  updated_at: string
}

export interface ZoneTemplate {
  name: string
  description: string
  patterns: string[]
  policy: ZonePolicy
  source: ZoneSource
}

export interface RepoZoneProposal {
  repository_id: string
  repository_name: string
  existing: boolean
  zones: ZoneTemplate[]
  frozen?: string[]
}
