import type { BudgetState, ImplementationRun, RepairState } from '@/types/controller'

// ─── Repair loop and D4 budget of one implementation attempt (M3-06) ──────────
//
// Shows implementation_runs.detail.repair and .budget exactly as the agent
// recorded them: failure records grouped by fingerprint with the number of
// check runs each failed in, the repair attempts and what the next check run
// showed, plan revisions linked to the record and check run that contradicted
// the plan, and the remaining token and active-time budget.

export function shortFingerprint(fp: string): string {
  return fp.slice(0, 12)
}

function fpAnchor(runId: string, fp: string): string {
  return `fp-${runId.slice(0, 8)}-${shortFingerprint(fp)}`
}

export function formatMinutes(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds <= 0) return '0m'
  const m = Math.floor(seconds / 60)
  if (m >= 60) return `${Math.floor(m / 60)}h ${m % 60}m`
  return `${m}m`
}

// remainingBudget is what is left of each cap (never negative).
export function remainingBudget(b: BudgetState): { tokens: number; activeSeconds: number } {
  return {
    tokens: Math.max(0, b.token_limit - b.tokens_used),
    activeSeconds: Math.max(0, b.active_limit_seconds - b.active_seconds),
  }
}

const STOP_LABEL: Record<string, string> = {
  passed: 'checks passed',
  repeated_failure: 'stopped: the same failure survived a repair',
  repairs_exhausted: 'stopped: repair attempts exhausted',
  budget_exhausted: 'stopped: budget exhausted',
}

export function BudgetLine({ budget }: { budget: BudgetState }) {
  const left = remainingBudget(budget)
  return (
    <div data-testid="run-budget" className="flex flex-wrap gap-x-4 gap-y-1 text-xs text-muted">
      <span>
        tokens {budget.tokens_used.toLocaleString()} / {budget.token_limit.toLocaleString()} ·{' '}
        <span className={left.tokens === 0 ? 'text-red-300' : ''}>{left.tokens.toLocaleString()} left</span>
      </span>
      <span>
        active {formatMinutes(budget.active_seconds)} / {formatMinutes(budget.active_limit_seconds)} ·{' '}
        <span className={left.activeSeconds === 0 ? 'text-red-300' : ''}>{formatMinutes(left.activeSeconds)} left</span>
        {budget.paused_seconds > 0 && <> · {formatMinutes(budget.paused_seconds)} awaiting input excluded</>}
        {budget.paused && <> · paused (prompt pending)</>}
      </span>
      {budget.exhausted && (
        <span className="text-red-300" title={budget.exhausted_detail}>
          exhausted: {budget.exhausted.replace(/_/g, ' ')}
        </span>
      )}
    </div>
  )
}

export function RepairDetail({ runId, repair }: { runId: string; repair: RepairState }) {
  const failures = repair.failures ?? []
  const attempts = repair.attempts ?? []
  const revisions = repair.revisions ?? []
  return (
    <div data-testid="run-repair" className="space-y-2 text-xs">
      <p className="text-muted">
        {repair.runs} check run{repair.runs === 1 ? '' : 's'} · {repair.repairs} / {repair.max_repairs} repair attempts
        {repair.stop_reason && (
          <>
            {' · '}
            <span className={repair.stop_reason === 'passed' ? 'text-emerald-300' : 'text-red-300'} title={repair.stop_detail}>
              {STOP_LABEL[repair.stop_reason] ?? repair.stop_reason}
            </span>
          </>
        )}
      </p>
      {failures.length > 0 && (
        <table className="w-full">
          <thead>
            <tr className="text-dim text-left">
              <th className="pr-3 font-medium">Fingerprint</th>
              <th className="pr-3 font-medium">Runs</th>
              <th className="pr-3 font-medium">Category</th>
              <th className="pr-3 font-medium">Location</th>
              <th className="pr-3 font-medium">Ownership</th>
              <th className="font-medium">Message</th>
            </tr>
          </thead>
          <tbody>
            {failures.map((f) => (
              <tr key={f.fingerprint} id={fpAnchor(runId, f.fingerprint)} data-testid="failure-record" className="align-top">
                <td className="pr-3 font-mono text-muted" title={f.fingerprint}>{shortFingerprint(f.fingerprint)}</td>
                <td className="pr-3 text-muted">×{f.count}</td>
                <td className="pr-3 text-muted">{f.category}</td>
                <td className="pr-3 font-mono text-muted">
                  {f.file ? `${f.file}${f.line ? `:${f.line}` : ''}` : '—'}
                  {f.test ? <span className="block text-dim">{f.test}</span> : null}
                </td>
                <td className="pr-3 text-muted">{f.ownership}{f.retryable ? '' : ' (not repairable)'}</td>
                <td className="text-muted whitespace-pre-wrap break-words" title={f.raw_ref ? `raw output: ${f.raw_ref}` : undefined}>{f.message}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {attempts.length > 0 && (
        <ol className="list-decimal pl-5 text-muted" data-testid="repair-attempts">
          {attempts.map((a) => (
            <li key={a.attempt}>
              repair {a.attempt} after {a.after_call_id} ({a.fingerprints.length} record{a.fingerprints.length === 1 ? '' : 's'}) →{' '}
              {a.outcome ?? 'pending'}
              {a.error ? ` · ${a.error}` : ''}
            </li>
          ))}
        </ol>
      )}
      {revisions.length > 0 && (
        <ul className="space-y-1" data-testid="plan-revisions">
          {revisions.map((r) => (
            <li key={`${r.evidence_call_id}-${r.fingerprint}`} className="text-muted">
              <span className="text-cyan-300">{r.revision}</span>
              <span className="block text-dim">
                {r.reason} — evidence{' '}
                <a href={`#${fpAnchor(runId, r.fingerprint)}`} className="underline font-mono">
                  {shortFingerprint(r.fingerprint)}
                </a>{' '}
                from check run {r.evidence_call_id}
                {r.evidence_seq ? ` (event ${r.evidence_seq})` : ''}
              </span>
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}

// RunRepair renders a run's repair loop and budget, or nothing when the
// run recorded neither.
export default function RunRepair({ run }: { run: ImplementationRun }) {
  if (!run.repair && !run.budget) return null
  return (
    <div className="space-y-2">
      {run.budget && <BudgetLine budget={run.budget} />}
      {run.repair && <RepairDetail runId={run.id} repair={run.repair} />}
    </div>
  )
}
