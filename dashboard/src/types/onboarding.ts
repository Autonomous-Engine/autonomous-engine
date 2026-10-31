// Types for the proactive-onboarding candidate review flow.
// Mirrors the server-side contract exposed by /api/onboarding/candidates*.

export type Confidence = 'high' | 'medium' | 'low' | 'not_detected'

export type UserStatus = 'pending' | 'confirmed' | 'edited' | 'rejected'

export interface Evidence {
  kind: string
  path?: string
  lines?: string
  name?: string
  detail?: string
}

export interface Candidate {
  id: string
  product_id: string
  repository_id?: string
  question_key: string
  value: string
  confidence: Confidence
  evidence: Evidence[]
  extractor: string
  user_status: UserStatus
  user_value?: string
  generated_at: string
  reviewed_at?: string
}

export interface Drift {
  question_key: string
  old_value: string
  new_value: string
  extractor: string
}

// Request body for POST /api/onboarding/candidates/review
export interface ReviewRequest {
  product_id: string
  question_key: string
  status: Exclude<UserStatus, 'pending'>
  user_value?: string
}
