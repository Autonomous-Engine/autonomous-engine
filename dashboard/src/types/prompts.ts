// dashboard/src/types/prompts.ts
//
// Mirrors Go types in internal/interactions. Field names match the wire
// payload (snake_case) so JSON from /api/prompts round-trips directly.

export type PromptType =
  | 'approval'
  | 'clarification'
  | 'escalation'
  | 'block_question'

export type Trigger =
  | 'agent_request'
  | 'heuristic:stuck'
  | 'heuristic:diminishing'
  | 'heuristic:retrieval_contradiction'
  | 'approval:prd'

export type Semantic =
  | 'retry'
  | 'restart'
  | 'continue'
  | 'cancel'
  | 'stop'
  | 'ok'
  | 'custom'

export type State = 'pending' | 'answered' | 'revoked' | 'cancelled'

export type SaveAsOverride = 'codex_fact' | 'ephemeral'

export type ClassifiedAs =
  | 'codex_module_correction'
  | 'codex_pattern'
  | 'codex_antipattern'
  | 'codex_domain_fact'
  | 'codex_style_rule'
  | 'ad_hoc'
  | 'ephemeral'

export interface Action {
  id: string
  label: string
  semantic: Semantic
}

export interface Prompt {
  id: string
  work_item_id: string
  product_id: string
  asking_agent: string
  trigger: Trigger
  prompt_type: PromptType
  headline: string
  question: string
  context_link?: string
  context_payload?: Record<string, unknown>
  actions: Action[]
  state: State
  requested_at: string
  answered_at?: string
  prompted_via?: string[]

  answered_by?: string
  answered_via?: string
  action_chosen?: string
  answer_text?: string
  save_as_override?: SaveAsOverride

  classified_as?: ClassifiedAs
  classification_confidence?: number
  codex_entry_id?: string
  codex_entry_table?: string
  qdrant_point_id?: string

  revoked_at?: string
  revoked_by?: string
  revocation_reason?: string
}

export interface AnswerInput {
  action_chosen?: string
  answer_text?: string
  save_as_override?: SaveAsOverride
}

// WebSocket event types broadcast on the "prompts" channel.
export type PromptWSEvent =
  | { type: 'prompt_requested'; prompt_id: string; prompt_type: PromptType; headline: string; work_item_id: string; product_id: string }
  | { type: 'prompt_answered'; prompt_id: string; classified_as?: ClassifiedAs; codex_entry_id?: string }
  | { type: 'prompt_revoked'; prompt_id: string; codex_entry_id?: string }
