// Repository verification policy (M3-03, migration 051). Mirrors
// internal/verifypolicy: Policy, Input and View.
//
// A repository without a stored policy is governed by the fail-closed
// default: when no executable check exists the run is BLOCKED
// (blocked by default). Accepting such a repository requires the
// operator to choose 'partial_with_human_review' explicitly, and even then
// the work only advances to mandatory human review when the product has
// opted in to partial verification.

export type NoChecksPolicy = 'block' | 'partial_with_human_review'

export type RequiredCheckKind = 'test' | 'build' | 'lint'

export type PolicySource = 'onboarding' | 'admin' | 'default'

export interface RepositoryVerificationPolicy {
  repository_id: string
  // null means "use the detected test command".
  test_command: string | null
  required_checks: RequiredCheckKind[]
  no_checks_policy: NoChecksPolicy
  security_review_paths: string[]
  set_by?: string
  set_at?: string
  source: PolicySource
}

// Body of POST /api/onboarding/verification-policy (plus repository_id) and
// PUT /api/admin/repositories/verification-policy?id=.
export interface RepositoryVerificationPolicyInput {
  test_command: string | null
  required_checks: RequiredCheckKind[]
  no_checks_policy: NoChecksPolicy
  security_review_paths: string[]
}

// Response of both GET endpoints and of a successful save.
export interface RepositoryVerificationPolicyView {
  repository_id: string
  policy: RepositoryVerificationPolicy
  // false: nothing stored yet; the fail-closed default (block) applies.
  found: boolean
  detected_test_command: string
  clone_available: boolean
  language: string
  suggested_security_review_paths: string[]
  selectable_checks: RequiredCheckKind[]
}

// Kinds the implementation agent cannot run yet; requiring them caps the
// outcome at partially_verified (recorded as not_run).
export const UNIMPLEMENTED_CHECKS: RequiredCheckKind[] = ['build', 'lint']
