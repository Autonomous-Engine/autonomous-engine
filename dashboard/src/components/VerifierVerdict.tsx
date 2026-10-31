// ─── Independent verification ─────────────────────────────────────────────────
//
// The independent verifier's verdict (M3-07): a review_comments artifact with
// kind "verifier". The verifier saw the acceptance criteria, the task specs,
// the diff against the pinned base and the recorded evidence — never the PR
// description or the implementation summary — and judged every criterion
// met / not met / not verifiable with cited evidence, plus a defect list.
// Distinct from the second-pass LLM review (kind "second_pass"), which is
// rendered with the other artifacts.

export interface VerifierCriterion {
  id: string
  status: string
  evidence?: string[]
  note?: string
}

export interface VerifierDefect {
  severity: string
  file?: string
  line?: number
  category: string
  description: string
  reproduction?: string
}

export interface VerifierModelInfo {
  provider?: string
  model_id?: string
  source?: string
  verifier_model_shared_with_author?: boolean
}

export interface VerifierVerdictData {
  decision: string
  gateReason: string
  perCriterion: VerifierCriterion[]
  defects: VerifierDefect[]
  modelInfo: VerifierModelInfo
  sharedWithAuthor: boolean
  error?: string
  summary?: string
  headSha?: string
  // R14: gaps a human must check (non-code criteria), and the subtask the
  // verification stage verified before the PR ("" for a pr.ready verdict).
  gaps: string[]
  taskId?: string
}

function parse(content: unknown): Record<string, unknown> | null {
  let c: unknown = content
  if (typeof c === 'string') {
    try {
      c = JSON.parse(c)
    } catch {
      return null
    }
  }
  return c && typeof c === 'object' ? (c as Record<string, unknown>) : null
}

// isVerifierArtifact: a review_comments artifact written by the verifier.
export function isVerifierArtifact(content: unknown): boolean {
  return parse(content)?.kind === 'verifier'
}

// verifierVerdictFrom reads the verdict; null when content is not a verifier
// artifact. Unknown or missing fields read as empty (fail-soft display).
export function verifierVerdictFrom(content: unknown): VerifierVerdictData | null {
  const c = parse(content)
  if (!c || c.kind !== 'verifier') return null
  const verdict = (c.verdict && typeof c.verdict === 'object' ? c.verdict : {}) as Record<string, unknown>
  const gate = (c.gate && typeof c.gate === 'object' ? c.gate : {}) as Record<string, unknown>
  const modelInfo = (verdict.model_info && typeof verdict.model_info === 'object' ? verdict.model_info : {}) as VerifierModelInfo
  return {
    decision: typeof c.decision === 'string' ? c.decision : 'unknown',
    gateReason: typeof gate.reason === 'string' ? gate.reason : '',
    perCriterion: Array.isArray(verdict.per_criterion) ? (verdict.per_criterion as VerifierCriterion[]) : [],
    defects: Array.isArray(verdict.defects) ? (verdict.defects as VerifierDefect[]) : [],
    modelInfo,
    sharedWithAuthor: c.verifier_model_shared_with_author === true || modelInfo.verifier_model_shared_with_author === true,
    error: typeof verdict.error === 'string' && verdict.error ? verdict.error : undefined,
    summary: typeof verdict.summary === 'string' && verdict.summary ? verdict.summary : undefined,
    headSha: typeof c.reviewed_sha === 'string' && c.reviewed_sha ? c.reviewed_sha : undefined,
    gaps: Array.isArray(verdict.gaps) ? (verdict.gaps as unknown[]).filter((g): g is string => typeof g === 'string') : [],
    taskId: typeof c.task_id === 'string' && c.task_id ? c.task_id : undefined,
  }
}

const DECISION_CLASS: Record<string, string> = {
  approve:         'bg-emerald-500/20 text-emerald-300 border border-emerald-500/30',
  approve_pending_second_pass: 'bg-sky-500/20 text-sky-300 border border-sky-500/30',
  request_changes: 'bg-red-500/20 text-red-300 border border-red-500/30',
  escalate:        'bg-orange-500/20 text-orange-300 border border-orange-500/30',
}

const STATUS_CLASS: Record<string, string> = {
  met:            'bg-emerald-500/20 text-emerald-300 border border-emerald-500/30',
  not_met:        'bg-red-500/20 text-red-300 border border-red-500/30',
  not_verifiable: 'bg-amber-500/20 text-amber-300 border border-amber-500/30',
}

const SEVERITY_CLASS: Record<string, string> = {
  critical: 'bg-red-500/20 text-red-300 border border-red-500/30',
  major:    'bg-orange-500/20 text-orange-300 border border-orange-500/30',
  minor:    'bg-surface-700/40 text-muted border border-sharp/40',
}

const DECISION_LABEL: Record<string, string> = {
  approve: 'Approved',
  approve_pending_second_pass: 'Approved — second-pass review decides',
  request_changes: 'Changes requested',
  escalate: 'Escalated to human review',
}

function badge(cls: string | undefined) {
  return `rounded px-1.5 py-0.5 text-xs font-medium ${cls ?? 'bg-surface-700/40 text-muted border border-sharp/40'}`
}

function location(d: VerifierDefect): string {
  if (!d.file) return ''
  return d.line && d.line > 0 ? `${d.file}:${d.line}` : d.file
}

export default function VerifierVerdict({ data }: { data: VerifierVerdictData }) {
  const model = [data.modelInfo.provider, data.modelInfo.model_id].filter(Boolean).join('/')
  return (
    <div className="space-y-3 text-sm">
      <div className="flex flex-wrap items-center gap-2">
        <span className={badge(DECISION_CLASS[data.decision])} data-testid="verifier-decision">
          {DECISION_LABEL[data.decision] ?? data.decision}
        </span>
        {data.gateReason && <span className="text-xs text-muted">{data.gateReason}</span>}
      </div>
      {data.sharedWithAuthor && (
        <p className="text-xs text-amber-300" data-testid="verifier-shared-model">
          No verifier-specific model is configured: the verifier ran on the review agent&apos;s model, so it is independent in
          context but not in model.
        </p>
      )}
      {data.error && <p className="text-xs text-orange-300">Verifier: {data.error}</p>}
      {data.summary && <p className="text-xs text-dim">{data.summary}</p>}

      <div>
        <h4 className="text-xs font-semibold uppercase tracking-wide text-muted mb-1">Criteria</h4>
        {data.perCriterion.length === 0 ? (
          <p className="text-xs text-dim italic">No acceptance criteria were recorded for this work item.</p>
        ) : (
          <table className="w-full text-xs">
            <thead>
              <tr className="text-left text-muted">
                <th className="py-1 pr-2">Criterion</th>
                <th className="py-1 pr-2">Status</th>
                <th className="py-1 pr-2">Evidence</th>
                <th className="py-1">Note</th>
              </tr>
            </thead>
            <tbody>
              {data.perCriterion.map((r) => (
                <tr key={r.id} className="border-t border-sharp/30 align-top">
                  <td className="py-1 pr-2 font-mono">{r.id}</td>
                  <td className="py-1 pr-2">
                    <span className={badge(STATUS_CLASS[r.status])}>{r.status.replace('_', ' ')}</span>
                  </td>
                  <td className="py-1 pr-2 font-mono break-all">{(r.evidence ?? []).join(' ')}</td>
                  <td className="py-1">{r.note}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>

      {data.gaps.length > 0 && (
        <div data-testid="verifier-gaps">
          <h4 className="text-xs font-semibold uppercase tracking-wide text-muted mb-1">Gaps a human must check</h4>
          <ul className="list-disc pl-4 space-y-0.5">
            {data.gaps.map((g, i) => (
              <li key={i} className="text-xs text-amber-200">{g}</li>
            ))}
          </ul>
        </div>
      )}

      <div>
        <h4 className="text-xs font-semibold uppercase tracking-wide text-muted mb-1">Defects</h4>
        {data.defects.length === 0 ? (
          <p className="text-xs text-dim italic">No defects reported.</p>
        ) : (
          <ul className="space-y-1">
            {data.defects.map((d, i) => (
              <li key={i} className="text-xs">
                <span className={badge(SEVERITY_CLASS[d.severity])}>{d.severity}</span>{' '}
                {location(d) && <span className="font-mono">{location(d)}</span>} <span className="text-muted">{d.category}</span>
                {' — '}
                {d.description}
                {d.reproduction && <span className="block pl-4 text-dim">Reproduce: {d.reproduction}</span>}
              </li>
            ))}
          </ul>
        )}
      </div>
      {(model || data.headSha) && (
        <p className="text-xs text-dim">
          {model && <>Model {model}</>}
          {model && data.headSha && ' · '}
          {data.headSha && <>head <span className="font-mono">{data.headSha.slice(0, 12)}</span></>}
        </p>
      )}
    </div>
  )
}
