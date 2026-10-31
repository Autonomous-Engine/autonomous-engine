import { useCallback, useEffect, useState } from 'react'
import { Link } from 'react-router-dom'
import { useApi } from '@/hooks/useApi'
import { shortSnapshotId } from '../ConfigSnapshotBadge'
import { formatRate } from './ConfigVersions'
import type {
  DecisionGate, DecisionVerdict, PerformanceRow, ProductReplayFlag, Rollout, RolloutDetail, RolloutOverride,
} from '@/types/rollouts'

// M4-06 admin page: rollouts of candidate configuration snapshots, their
// D7 decision reports (raw counts and intervals), approve/block, the
// override audit, and the per-product replay opt-in. Routing by
// demonstrated performance is surfaced here (per task class / repository
// production rates and the benchmark's per-language strata); the admin
// picks — nothing routes automatically.

export function verdictClass(v: DecisionVerdict | DecisionGate['verdict'] | ''): string {
  switch (v) {
    case 'APPROVED':
    case 'PASS':
      return 'text-green-400'
    case 'BLOCKED':
    case 'BLOCK':
      return 'text-red-400'
    case 'MORE_TRIALS':
    case 'INCOMPLETE':
    case 'NOT_EVALUATED':
      return 'text-yellow-300'
    default:
      return 'text-dim'
  }
}

// canApproveWithoutOverride mirrors the backend rule: only an evaluating
// rollout whose latest decision is APPROVED is approved without a reason.
export function canApproveWithoutOverride(r: Rollout): boolean {
  return r.status === 'evaluating' && r.decision_report?.verdict === 'APPROVED'
}

export function isClosed(r: Rollout): boolean {
  return r.status === 'blocked' || r.status === 'rolled_back'
}

export function formatInterval(ci: number[] | null | undefined): string {
  if (!ci || ci.length !== 2) return '—'
  const f = (x: number) => `${x >= 0 ? '+' : ''}${x.toFixed(1)}`
  return `[${f(ci[0])}, ${f(ci[1])}] pp`
}

export function GateTable({ gates }: { gates: DecisionGate[] }) {
  return (
    <table className="w-full text-xs" data-testid="gate-table">
      <thead className="text-left text-dim">
        <tr><th className="px-2 py-1">Gate</th><th className="px-2 py-1">Verdict</th><th className="px-2 py-1">Reason</th></tr>
      </thead>
      <tbody>
        {gates.map(g => (
          <tr key={g.name} className="border-t border-rule/40 align-top">
            <td className="px-2 py-1 font-mono">{g.name}</td>
            <td className={`px-2 py-1 font-semibold ${verdictClass(g.verdict)}`}>{g.verdict}</td>
            <td className="px-2 py-1">{g.reason}</td>
          </tr>
        ))}
      </tbody>
    </table>
  )
}

function PerformanceTable({ title, rows }: { title: string; rows: PerformanceRow[] | undefined }) {
  if (!rows || rows.length === 0) return <p className="text-xs text-dim">{title}: no production outcomes</p>
  return (
    <div>
      <p className="text-xs text-dim">{title}</p>
      <table className="w-full text-xs">
        <tbody>
          {rows.map(r => (
            <tr key={r.group} className="border-t border-rule/40">
              <td className="px-2 py-0.5">{r.group}</td>
              <td className="px-2 py-0.5 text-right">{r.released}/{r.items}</td>
              <td className="px-2 py-0.5 text-right">{formatRate(r.rate)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

function StrataTable({ detail }: { detail: RolloutDetail }) {
  const report = detail.rollout.decision_report?.report
  const strata = report?.strata
  if (!report || !strata) return null
  return (
    <div className="space-y-2">
      {(['language', 'task_class'] as const).map(field => strata[field] && (
        <div key={field}>
          <p className="text-xs text-dim">Sealed benchmark pass rate by {field.replace('_', ' ')}</p>
          <table className="w-full text-xs">
            <thead className="text-left text-dim"><tr><th className="px-2">{field}</th><th className="px-2 text-right">candidate</th><th className="px-2 text-right">baseline</th></tr></thead>
            <tbody>
              {Object.entries(strata[field]).map(([value, byComp]) => (
                <tr key={value} className="border-t border-rule/40">
                  <td className="px-2 py-0.5">{value}</td>
                  <td className="px-2 py-0.5 text-right">{byComp[report.candidate] ? `${byComp[report.candidate].passes}/${byComp[report.candidate].attempts}` : '—'}</td>
                  <td className="px-2 py-0.5 text-right">{byComp[report.baseline] ? `${byComp[report.baseline].passes}/${byComp[report.baseline].attempts}` : '—'}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ))}
    </div>
  )
}

function RolloutDetailView({ id, onChanged }: { id: string; onChanged: () => void }) {
  const { get, post } = useApi()
  const [detail, setDetail] = useState<RolloutDetail | null>(null)
  const [error, setError] = useState('')
  const [reason, setReason] = useState('')

  const load = useCallback(async () => {
    try {
      setDetail(await get<RolloutDetail>(`/admin/rollouts/detail?id=${id}`))
      setError('')
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Failed to load rollout')
    }
  }, [get, id])

  useEffect(() => { load() }, [load])

  async function decide(kind: 'approve' | 'block') {
    try {
      const body = kind === 'approve' ? { override_reason: reason } : { reason }
      await post(`/admin/rollouts/${kind}?id=${id}`, body)
      setReason('')
      await load()
      onChanged()
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : `Failed to ${kind}`)
    }
  }

  if (error) return <p className="text-sm text-red-400">{error}</p>
  if (!detail) return <p className="text-sm text-dim">Loading…</p>
  const r = detail.rollout
  const d = r.decision_report
  const needsReason = !canApproveWithoutOverride(r)
  const missing = detail.not_evaluated ?? d?.not_evaluated ?? []
  return (
    <div className="space-y-3 rounded border border-rule p-3" data-testid="rollout-detail">
      <div className="text-xs">
        <span className="font-mono">{r.id}</span> · {r.scope} · status <b>{r.status}</b>
        {r.decided_by && <> · decided by {r.decided_by}</>}
      </div>
      <div className="text-xs">
        candidate <Link className="font-mono text-accent hover:underline" to={`/admin/config-versions?a=${r.candidate_snapshot_id}&b=${r.baseline_snapshot_id}`}>{shortSnapshotId(r.candidate_snapshot_id ?? '')}</Link>
        {' '}vs baseline <span className="font-mono">{shortSnapshotId(r.baseline_snapshot_id ?? '')}</span>
      </div>
      {detail.scope_note && <p className="text-xs text-yellow-300" data-testid="scope-note">{detail.scope_note}</p>}
      {d ? (
        <>
          <p className={`text-sm font-semibold ${verdictClass(d.verdict)}`}>Rollout decision: {d.verdict} — {d.summary}</p>
          <GateTable gates={d.gates} />
          <p className="text-xs text-dim">
            Paired CI {formatInterval(d.report?.paired?.ci95_pp)} over {d.report?.paired?.paired_tasks ?? 0} tasks
            {d.not_evaluated && d.not_evaluated.length > 0 && <> · not evaluated (no data): {d.not_evaluated.join(', ')}</>}
          </p>
          <p className="text-xs text-dim">{d.cost_note}</p>
        </>
      ) : (
        <p className="text-sm text-dim">No evaluation yet. Run <code>rolloutctl shadow</code> then <code>rolloutctl evaluate</code>.</p>
      )}
      <div className="grid gap-3 md:grid-cols-2">
        <div className="space-y-2">
          <p className="text-xs font-semibold">Candidate — production / replay rates</p>
          <PerformanceTable title="by task class" rows={detail.candidate_performance?.task_class} />
          <PerformanceTable title="by repository" rows={detail.candidate_performance?.repository} />
        </div>
        <div className="space-y-2">
          <p className="text-xs font-semibold">Baseline — production rates</p>
          <PerformanceTable title="by task class" rows={detail.baseline_performance?.task_class} />
          <PerformanceTable title="by repository" rows={detail.baseline_performance?.repository} />
        </div>
      </div>
      <StrataTable detail={detail} />
      {!isClosed(r) && (
        <div className="flex flex-wrap items-center gap-2">
          <input
            className="min-w-[16rem] flex-1 rounded border border-rule bg-surface-800 px-2 py-1 text-xs"
            placeholder={needsReason
              ? (missing.length > 0 ? `Override reason naming the gates without data (${missing.join(', ')}); audited` : 'Override reason (required to approve; audited)')
              : 'Reason (optional)'}
            value={reason}
            onChange={e => setReason(e.target.value)}
          />
          {r.status !== 'approved' && (
            <button
              className="rounded bg-green-700 px-3 py-1 text-xs disabled:opacity-40"
              disabled={needsReason && reason.trim() === ''}
              onClick={() => decide('approve')}
            >{needsReason ? 'Approve with override' : 'Approve'}</button>
          )}
          <button className="rounded bg-red-700 px-3 py-1 text-xs" onClick={() => decide('block')}>Block</button>
        </div>
      )}
    </div>
  )
}

function ReplayOptIn() {
  const { get, post } = useApi()
  const [products, setProducts] = useState<ProductReplayFlag[]>([])
  const [error, setError] = useState('')
  const load = useCallback(async () => {
    try {
      setProducts((await get<{ products: ProductReplayFlag[] }>('/admin/products/replay')).products ?? [])
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Failed to load products')
    }
  }, [get])
  useEffect(() => { load() }, [load])
  async function toggle(p: ProductReplayFlag, field: 'allow_replay' | 'benchmark_scratch') {
    try {
      await post('/admin/products/replay', { product_id: p.id, [field]: !p[field] })
      load()
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Failed to update')
    }
  }
  return (
    <section className="space-y-2">
      <h2 className="text-sm font-semibold">Shadow settings</h2>
      <p className="text-xs text-dim">
        <b>Allow replay</b>: shadow evaluation may re-submit this product's recent completed work items with the
        candidate. <b>Benchmark scratch</b>: this product is a scratch target (its repositories are forks) — only a
        scratch product may run a candidate snapshot or receive a replay. Replay items are reviewed on their
        branch, never get a PR, and are then cancelled. Both off by default.
      </p>
      {error && <p className="text-xs text-red-400">{error}</p>}
      <table className="text-xs">
        <thead className="text-left text-dim"><tr><th className="px-2">Product</th><th className="px-2">Allow replay</th><th className="px-2">Benchmark scratch</th></tr></thead>
        <tbody>
          {products.map(p => (
            <tr key={p.id}>
              <td className="px-2">{p.name}</td>
              <td className="px-2"><input type="checkbox" checked={p.allow_replay} onChange={() => toggle(p, 'allow_replay')} aria-label={`allow replay for ${p.name}`} /></td>
              <td className="px-2"><input type="checkbox" checked={p.benchmark_scratch} onChange={() => toggle(p, 'benchmark_scratch')} aria-label={`benchmark scratch for ${p.name}`} /></td>
            </tr>
          ))}
        </tbody>
      </table>
    </section>
  )
}

export function OverrideTable({ rows }: { rows: RolloutOverride[] }) {
  if (rows.length === 0) return <p className="text-xs text-dim">No overrides recorded.</p>
  return (
    <table className="w-full text-xs" data-testid="override-table">
      <thead className="text-left text-dim">
        <tr><th className="px-2">When</th><th className="px-2">Action</th><th className="px-2">Change</th><th className="px-2">Reason</th><th className="px-2">Actor</th></tr>
      </thead>
      <tbody>
        {rows.map(o => (
          <tr key={o.id} className="border-t border-rule/40">
            <td className="px-2 py-0.5">{new Date(o.created_at).toLocaleString()}</td>
            <td className="px-2 py-0.5 font-mono">{o.action}</td>
            <td className="px-2 py-0.5 font-mono">{shortSnapshotId(o.before_snapshot_id ?? '') || '(new)'} → {shortSnapshotId(o.after_snapshot_id ?? '')}</td>
            <td className="px-2 py-0.5">{o.override_reason}</td>
            <td className="px-2 py-0.5">{o.actor || '—'}</td>
          </tr>
        ))}
      </tbody>
    </table>
  )
}

export default function Rollouts() {
  const { get } = useApi()
  const [rollouts, setRollouts] = useState<Rollout[]>([])
  const [overrides, setOverrides] = useState<RolloutOverride[]>([])
  const [selected, setSelected] = useState<string | null>(null)
  const [error, setError] = useState('')

  const load = useCallback(async () => {
    try {
      const [r, o] = await Promise.all([
        get<{ rollouts: Rollout[] }>('/admin/rollouts'),
        get<{ overrides: RolloutOverride[] }>('/admin/rollouts/overrides'),
      ])
      setRollouts(r.rollouts ?? [])
      setOverrides(o.overrides ?? [])
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Failed to load rollouts')
    }
  }, [get])

  useEffect(() => { load() }, [load])

  return (
    <div className="space-y-5">
      <div>
        <h1 className="text-lg font-semibold text-fg">Rollouts</h1>
        <p className="mt-0.5 text-sm text-muted">
          A configuration change becomes the production default only through an approved rollout
          or an audited override. Shadow runs and evaluations are driven by <code>rolloutctl</code>.
        </p>
      </div>
      {error && <p className="text-sm text-red-400">{error}</p>}
      {rollouts.length === 0 ? (
        <p className="text-sm text-dim">No rollouts yet. Propose one with <code>rolloutctl propose --definition &lt;id&gt;</code>.</p>
      ) : (
        <table className="w-full text-xs">
          <thead className="text-left text-dim">
            <tr><th className="px-2 py-1">Created</th><th className="px-2 py-1">Candidate</th><th className="px-2 py-1">Baseline</th><th className="px-2 py-1">Scope</th><th className="px-2 py-1">Status</th><th className="px-2 py-1">Verdict</th></tr>
          </thead>
          <tbody>
            {rollouts.map(r => (
              <tr key={r.id} className="cursor-pointer border-t border-rule/40 hover:bg-surface-800" onClick={() => setSelected(r.id)} data-testid="rollout-row">
                <td className="px-2 py-1">{new Date(r.created_at).toLocaleString()}</td>
                <td className="px-2 py-1 font-mono">{shortSnapshotId(r.candidate_snapshot_id ?? '')}</td>
                <td className="px-2 py-1 font-mono">{shortSnapshotId(r.baseline_snapshot_id ?? '')}</td>
                <td className="px-2 py-1">{r.scope}</td>
                <td className="px-2 py-1">{r.status}</td>
                <td className={`px-2 py-1 font-semibold ${verdictClass(r.decision_report?.verdict ?? '')}`}>{r.decision_report?.verdict ?? '—'}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {selected && <RolloutDetailView id={selected} onChanged={load} />}
      <section className="space-y-2">
        <h2 className="text-sm font-semibold">Override audit</h2>
        <OverrideTable rows={overrides} />
      </section>
      <ReplayOptIn />
    </div>
  )
}
