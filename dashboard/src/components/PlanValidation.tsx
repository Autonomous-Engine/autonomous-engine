// ─── Plan validation ──────────────────────────────────────────────────────────
//
// The planner's deterministic plan validator (M2-05) rejects a decomposition
// that leaves a criterion unowned, cites unknown criteria, has no deliverable
// or verification, names existing paths without evidence, or has a broken
// dependency graph. After one repair round (two when the first made progress,
// R9) a plan that still fails blocks the parent, and the violations are
// recorded in a technical_brief artifact named "plan-validation". This shows
// that list and the record's note.

export interface PlanViolation {
  code: string
  severity: string
  task_index?: number
  criterion_id?: string
  message: string
}

export interface PlanValidationData {
  attempt: number
  // note is why the planner did not resolve more (the R8 orphan threshold,
  // or a second repair round not tried because of the spend, R9).
  note?: string
  violations: PlanViolation[]
}

export const PLAN_VALIDATION_NAME = 'plan-validation'

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

// isPlanValidationArtifact: a technical_brief artifact that is the validator's
// record, not a brief (readers of the brief skip it).
export function isPlanValidationArtifact(content: unknown): boolean {
  return parse(content)?.name === PLAN_VALIDATION_NAME
}

// planValidationFrom reads the violation list; null when content is not a
// plan-validation record.
export function planValidationFrom(content: unknown): PlanValidationData | null {
  const c = parse(content)
  if (!c || c.name !== PLAN_VALIDATION_NAME || !Array.isArray(c.violations)) return null
  return {
    attempt: typeof c.attempt === 'number' ? c.attempt : 0,
    violations: c.violations as PlanViolation[],
    note: typeof c.note === 'string' && c.note.trim() !== '' ? c.note.trim() : undefined,
  }
}

function ts(v: string | undefined): number {
  if (!v) return 0
  const n = new Date(v).getTime()
  return Number.isFinite(n) ? n : 0
}

// isCurrentPlanValidation: the record still describes the item — it is newer
// than every later sign of planning (a newer technical brief, i.e. a retry
// that re-researched, or a subtask, i.e. a later plan that was committed).
// A record older than any of them is stale and not shown.
export function isCurrentPlanValidation(recordedAt: string, laterEvents: (string | undefined)[]): boolean {
  const at = ts(recordedAt)
  return laterEvents.every((e) => ts(e) <= at)
}

// attemptsPhrase says what the attempts were: the decomposition, then one
// repair round, or two when the first made progress (R9).
export function attemptsPhrase(attempt: number): string {
  if (attempt <= 1) return 'the decomposition'
  if (attempt === 2) return 'the decomposition and one repair round'
  return `the decomposition and ${attempt - 1} repair rounds`
}

export default function PlanValidation({ data, recordedAt }: { data: PlanValidationData; recordedAt?: string }) {
  const errors = data.violations.filter((v) => v.severity !== 'warning')
  const warnings = data.violations.filter((v) => v.severity === 'warning')
  return (
    <div className="space-y-2">
      {recordedAt && (
        <p className="text-xs text-dim">
          Recorded {new Date(recordedAt).toLocaleString(undefined, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' })}
        </p>
      )}
      <p className="text-xs text-muted">
        The plan was rejected after {data.attempt} attempt{data.attempt === 1 ? '' : 's'} ({attemptsPhrase(data.attempt)}):{' '}
        {errors.length} violation{errors.length === 1 ? '' : 's'}
        {warnings.length > 0 ? `, ${warnings.length} warning${warnings.length === 1 ? '' : 's'}` : ''}.
      </p>
      {data.note && <p className="text-xs text-muted">{data.note}</p>}
      <ul className="space-y-1">
        {data.violations.map((v, i) => (
          <li key={i} className="flex flex-wrap items-baseline gap-2 text-xs">
            <span
              className={`rounded px-1.5 py-0.5 font-mono ${
                v.severity === 'warning'
                  ? 'bg-amber-500/20 text-amber-300 border border-amber-500/30'
                  : 'bg-red-500/20 text-red-300 border border-red-500/30'
              }`}
            >
              {v.code}
            </span>
            {v.criterion_id && <span className="font-mono text-muted">{v.criterion_id}</span>}
            {v.task_index ? <span className="text-muted">task {v.task_index}</span> : null}
            <span className="text-fg">{v.message}</span>
          </li>
        ))}
      </ul>
    </div>
  )
}
