import type { ImplementationVerification } from './evidence'
import type { ImplementationRun } from './controller'
import type { WorkItemOutcome } from './configVersions'

// WorkItem types matching Go models

export type WorkItemType =
  | 'feature'
  | 'bug'
  | 'task'
  | 'subtask'
  | 'research'
  | 'qa'

export type WorkItemState =
  | 'intake'
  | 'prd'
  | 'prd_approval'
  | 'research'
  | 'planning'
  | 'implementation'
  | 'verification'
  | 'review'
  | 'human_review'
  | 'qa'
  | 'passed'
  | 'failed'
  | 'blocked'
  | 'rejected'
  | 'released'
  | 'cancelled'
  | 'awaiting_user'

export type Priority = 'critical' | 'high' | 'medium' | 'low'

export type ArtifactType =
  | 'prd'
  | 'technical_brief'
  | 'research_finding'
  | 'qa_report'
  | 'review_comments'

export interface WorkItem {
  id: string
  external_ref: string
  type: WorkItemType
  state: WorkItemState
  title: string
  description: string
  priority: Priority
  codex_version: number
  parent_id?: string
  product_id?: string
  repository_id?: string
  branch?: string
  // Set while state === 'awaiting_user'. When the user answers, the state
  // machine restores state = previous_state and clears both fields.
  previous_state?: WorkItemState
  awaiting_prompt_id?: string
  created_at: string
  updated_at: string
  // Codex coherence Part 1 (GET /api/work-items only): the codex the item
  // was planned against, and the note when the codex changed since while
  // the item stayed on its plan. Absent when unpinned or from an older
  // backend.
  codex_pin?: CodexPin
  codex_changed_since_planning?: CodexChangeNote
}

// The codex version and modules a work item was planned against
// (work_item_codex_pins).
export interface CodexPin {
  product_version: number
  branch: string
  modules: string[]
  pinned_at: string
}

// The latest codex event of an item that stayed on its plan (noted or
// escalated).
export interface CodexChangeNote {
  kind: 'noted' | 'escalated' | string
  to_version: number
  modules: string[]
  sections: string[]
  reason: string
  at: string
}

// One work_item_codex_events row: what a codex change did to the item.
export interface CodexEvent {
  id: string
  kind: 'replanned' | 'escalated' | 'noted' | 'continued' | string
  automatic: boolean
  branch: string
  from_version: number
  to_version: number
  modules: string[]
  sections: string[]
  item_state: string
  reason: string
  prompt_id?: string
  created_at: string
}

export interface WorkItemTransition {
  id: string
  work_item_id: string
  from_state: WorkItemState
  to_state: WorkItemState
  agent_id: string
  reason: string
  timestamp: string
}

export interface WorkItemArtifact {
  id: string
  work_item_id: string
  type: ArtifactType
  content: unknown
  created_at: string
  agent_id: string
}

// Agent types matching Go models

export type AgentType =
  | 'product'
  | 'research'
  | 'research_coordinator'
  | 'implementation'
  | 'implementation_planner'
  | 'pr_coordinator'
  | 'review'
  | 'qa'

export type AssignmentStatus = 'active' | 'completed' | 'failed' | 'cancelled'

export interface AgentAssignment {
  id: string
  work_item_id: string
  agent_type: AgentType
  agent_instance_id: string
  assigned_at: string
  completed_at?: string
  status: AssignmentStatus
}

// Codex types matching Go models

export type CodexSectionType =
  | 'architecture'
  | 'module'
  | 'pattern'
  | 'anti_pattern'
  | 'testing'
  | 'style'
  | 'process'
  | 'domain'

export interface CodexPattern {
  id: string
  name: string
  category: string
  when_to_use: string
  reference_files: string[]
  rationale: string
  key_rules: string[]
  common_mistakes: string[]
}

export interface CodexModule {
  id: string
  name: string
  purpose: string
  public_interface: unknown
  dependencies: string[]
  dependents: string[]
  internal_patterns: string
  owner_domain: string
  risk_level: string
  test_coverage_notes: string
}

export interface CodexSection {
  id: string
  section_type: CodexSectionType
  name: string
  content: unknown
  version: number
  updated_at: string
}

// GET /api/codex/version?product_id=…[&branch=…]: the latest version of a
// product and branch. version_number carries the per-product-and-branch
// product_version (migration 060), 0 when the branch has none.
export interface CodexVersion {
  product_id: string
  branch: string
  version: number
  version_number: number
  product_version: number
  sections_changed: string[]
  change_summary: string
  created_at?: string
}

// Model types matching Go models

export type OperatingMode = 'constrained' | 'standard' | 'advanced'

export interface ModelProfile {
  id: string
  provider: string
  model_id: string
  effective_context_window: number
  instruction_following: number
  code_generation: number
  code_understanding: number
  reasoning: number
  synthesis: number
  avg_inference_speed: number
  concurrent_capacity: number
  operating_mode: OperatingMode
  evaluated_at: string
}

// Work Item Detail (full lifecycle response)

export interface WorkItemDetail {
  item: WorkItem
  transitions: WorkItemTransition[]
  children: WorkItem[]
  artifacts: Array<{
    id: string
    artifact_type: string
    content: unknown
    agent_id: string
    created_at: string
  }>
  agent_definition?: {
    name: string
    provider: string
    model_id: string
  }
  assignments: Array<{
    id: string
    agent_type: string
    instance_id: string
    status: string
    assigned_at: string
    completed_at?: string
  }>
  inference_logs: Array<{
    id: string
    model: string
    intent: string
    input_tokens: number
    output_tokens: number
    latency_ms: number
    success: boolean
    error?: string
    agent_id: string
    created_at: string
    // Full conversation. Populated by completeInference; empty array while
    // the call is still running.
    prompt_messages?: Array<{ role: 'system' | 'user' | 'assistant' | 'tool'; content: string; name?: string }>
    response_content?: string
  }>
  repository?: { name: string; github_url: string }
  product?: { name: string }
  // Latest implementation verification (M0-08). null when none recorded;
  // the full history is at GET /api/work-items/evidence?id=.
  evidence?: ImplementationVerification | null
  // Latest implementation controller run per subtask (M3-02), as recorded
  // in implementation_runs. Absent from older backends.
  implementation_runs?: ImplementationRun[]
  // Repository snapshot pins (M2-02): the base commit each repository was
  // pinned to at intake, and their audit trail. Empty for a legacy-unpinned
  // item; absent from an older backend.
  base_shas?: BaseShaPin[]
  base_sha_events?: BaseShaEvent[]
  // Cross-repository contract edges between the item's subtasks and their
  // last PR-time check (M2-06). Absent from older backends.
  contract_edges?: ContractEdge[]
  // Codex coherence Part 1: the codex pin (null when unpinned) and each
  // codex re-plan, note, escalation or "continue as is". Absent from older
  // backends.
  codex_pin?: CodexPin | null
  codex_events?: CodexEvent[]
  // Configuration snapshot the item runs under and its production outcome
  // (M4-05). Absent from older backends; null until recorded.
  config_snapshot_id?: string | null
  outcome?: WorkItemOutcome | null
}

// A contract between two repositories (M2-06, internal/contracts.Ref).
export interface ContractRef {
  type: string
  name: string
  producer_repo_id: string
  consumer_repo_id: string
  source: 'codex' | 'criteria' | string
  producer_file?: string
  producer_line?: number
  consumer_file?: string
  consumer_line?: number
  criterion_id?: string
}

export type ContractCheckStatus = 'ok' | 'mismatch' | 'unchecked'

export interface ContractCheckResult {
  contract: ContractRef
  status: ContractCheckStatus
  details?: string[]
  mismatches?: { producer_change: string; consumer_ref: string }[]
}

// One contract edge: the consumer subtask waits for the producer subtask;
// last_check is pr-coordinator's last compatibility check (null until run).
export interface ContractEdge {
  consumer_task_id: string
  consumer_title: string
  consumer_repository: string
  producer_task_id: string
  producer_title: string
  producer_repository: string
  contracts: ContractRef[]
  last_check: { status: ContractCheckStatus; checked_at: string; results: ContractCheckResult[] } | null
}

export interface BaseShaPin {
  repository_id: string
  repository_name: string
  base_ref: string
  base_sha: string
  resolved_at: string
}

export type BaseShaEventKind = 'resolved' | 'drift_detected' | 'repinned'

export interface BaseShaEvent {
  id: string
  repository_id: string
  repository_name: string
  kind: BaseShaEventKind | string
  old_sha: string
  new_sha: string
  detail: string
  created_at: string
}

// Dashboard-specific types

export type QueueCounts = Partial<Record<WorkItemState, number>>

export interface WebSocketMessage {
  channel: string
  data: unknown
}

export interface AgentStatus {
  agent_type: AgentType
  instance_id: string
  status: 'idle' | 'active'
  current_task?: string
  current_work_item_id?: string
  work_item_id?: string
  work_item_title?: string
  started_at?: string
}

export interface SystemConfig {
  pool_sizes: Record<AgentType, number>
  retry_count: number
  human_review_enabled: boolean
  max_concurrent_work_items: number
}

export type UserRole = 'viewer' | 'operator' | 'admin'

export interface User {
  id: string
  email: string
  name: string
  role: UserRole
  created_at: string
}

export type CredentialScope = 'global' | 'integration' | 'agent_type' | 'agent_instance' | 'github_account';
export type CredentialCategory = 'llm' | 'github' | 'jira' | 'system' | 'dashboard';
export type AuditAction = 'created' | 'updated' | 'deleted' | 'read' | 'rotated';

export interface CredentialEntry {
  id: string;
  scope: CredentialScope;
  scope_target: string;
  key_name: string;
  value: string;
  category: CredentialCategory;
  is_secret: boolean;
  description: string;
  created_by?: string;
  updated_at: string;
}

export interface ConfigEntry {
  id: string;
  scope: CredentialScope;
  scope_target: string;
  key_name: string;
  value: string;
  value_type: 'string' | 'int' | 'bool' | 'json';
  category: string;
  description: string;
  updated_at: string;
}

export interface AuditEntry {
  id: string;
  credential_id?: string;
  action: AuditAction;
  actor_id?: string;
  actor_type: string;
  ip_address: string;
  metadata: Record<string, string>;
  timestamp: string;
}

export interface AuthProvider {
  id: string;
  provider_type: 'local' | 'saml' | 'ldap';
  name: string;
  enabled: boolean;
  is_primary: boolean;
  config: Record<string, unknown>;
}

export interface Product {
  id: string;
  name: string;
  description: string;
  created_at: string;
  updated_at: string;
  // D2 opt-in: partially verified work may advance to mandatory human
  // review instead of being blocked. Absent on older backends (treat as false).
  partial_verification_opt_in?: boolean;
  // M2-06: when true, a cross-repository contract the PR-time checkers
  // cannot evaluate blocks the consumer's PR. Absent on older backends.
  contracts_strict?: boolean;
}

export interface Repository {
  id: string;
  product_id: string;
  name: string;
  github_url: string;
  base_branch: string;
  merge_strategy: 'squash' | 'merge' | 'rebase';
  branch_prefix: string;
  pr_template: string;
  clone_path: string;
  last_indexed_at?: string;
  last_indexed_commit: string;
  enabled: boolean;
  created_at: string;
  github_account_id?: string;
}

export interface GitHubAccount {
  id: string;
  username: string;
  avatar_url: string;
  credential_id: string;
  last_validated_at?: string;
  valid: boolean;
  created_at: string;
  label: string;
}

export interface AgentDefinition {
  id: string;
  name: string;
  display_name: string;
  description: string;
  provider: string;
  model_id: string;
  model_endpoint: string;
  step_product_analysis: boolean;
  step_research: boolean;
  step_planning: boolean;
  step_implementation: boolean;
  step_review: boolean;
  step_human_review: boolean;
  step_qa: boolean;
  routing_tags: string[];
  routing_complexity: string[];
  routing_priority: boolean;
  max_retries: number;
  max_files_per_task: number;
  timeout_minutes: number;
  enabled: boolean;
  is_default: boolean;
  created_at?: string;
  updated_at?: string;
}

export interface AgentDefinitionPrompt {
  id: string;
  agent_definition_id: string;
  intent_type: string;
  prompt_type: string;
  content: string;
}

export interface WebhookDeadLetter {
  id: string;
  source: string;
  event_type: string;
  payload: unknown;
  error_message: string;
  retry_count: number;
  max_retries: number;
  next_retry_at?: string;
  resolved: boolean;
  created_at: string;
}

export interface OllamaModel {
  name: string;
  size: number;
  digest: string;
  modified_at: string;
  details?: {
    format: string;
    family: string;
    parameter_size: string;
    quantization_level: string;
  };
}

export interface OllamaStatus {
  reachable: boolean;
  endpoint: string;
  model_count: number;
}

export interface OllamaPullProgress {
  status: string;
  completed?: number;
  total?: number;
}

export interface OllamaRunningModel {
  name: string;
  size: number;
  size_vram: number;
  expires_at: string;
}

export interface LLMProvider {
  id: string;
  name: string;
  provider_type: 'ollama' | 'openai' | 'anthropic' | 'vllm' | 'custom';
  endpoint: string;
  has_api_key: boolean;
  is_default: boolean;
  enabled: boolean;
  settings: Record<string, unknown>;
  created_at: string;
}

export interface EmbeddingProvider {
  id: string;
  name: string;
  provider_type: 'ollama' | 'openai' | 'azure_openai' | 'openai_compatible';
  endpoint: string;
  model: string;
  dimension: number; // 0 until Test is run
  has_api_key: boolean;
  is_default: boolean;
  enabled: boolean;
  settings: Record<string, unknown>;
  created_at: string;
}

export interface EmbeddingProviderTestResult {
  reachable: boolean;
  latency_ms: number;
  dimension?: number;
  vector_norm?: number;
  error: string | null;
}

export interface ProviderModel {
  name: string;
  size?: number;
  details?: {
    parameter_size?: string;
    quantization_level?: string;
  };
}

export interface RepoDependency {
  id: string;
  source_repo_id: string;
  target_repo_id: string;
  dependency_type: string;
  description: string;
  discovered_by: string;
}

export interface OnboardingSession {
  id: string;
  product_id: string;
  phase: string;
  prerequisites: Record<string, boolean>;
  started_at: string;
  completed_at?: string;
}

export interface LoginAuditEntry {
  id: string;
  user_id?: string;
  email: string;
  provider: string;
  success: boolean;
  failure_reason: string;
  ip_address: string;
  user_agent: string;
  timestamp: string;
}

export interface UserEntry {
  id: string;
  email: string;
  name: string;
  role: 'viewer' | 'operator' | 'admin';
  auth_provider: string;
  last_login?: string;
  locked_at?: string;
  password_change_required: boolean;
  created_at: string;
}

// ─── Codex Graph ─────────────────────────────────────────────────────────────

export type RiskLevel = 'low' | 'medium' | 'high' | 'unknown'

export interface GraphNodeData {
  id: string
  name: string
  purpose: string
  risk_level: RiskLevel
  owner_domain: string
  test_coverage_notes: string
  public_interface_summary: string
  dependencies: string[]
  dependents: string[]
  dependencies_count: number
  dependents_count: number
  repository_id?: string
  external: boolean
  insight_count?: number
}

export interface GraphEdgeData {
  id: string
  source: string
  target: string
  kind: 'depends_on'
  resolved: boolean
}

export interface GraphMeta {
  node_count: number
  edge_count: number
  truncated: boolean
  empty_reason?: string
}

export interface GraphResponse {
  product_id: string
  product_name: string
  generated_at: string
  meta: GraphMeta
  nodes: GraphNodeData[]
  edges: GraphEdgeData[]
}
