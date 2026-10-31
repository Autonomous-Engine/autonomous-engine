import { useCallback, useEffect, useState } from 'react'
import { Link, useSearchParams } from 'react-router-dom'
import { useApi } from '@/hooks/useApi'
import { configDiffLink, formatUsd, shortSnapshotId } from '../ConfigSnapshotBadge'
import type { ConfigSnapshotDiff, ConfigSnapshotRollup, ModelPrice } from '@/types/configVersions'

// M4-05 admin page: every configuration snapshot with the production
// outcomes of the work items that ran under it, a diff view, and the
// model_prices editor that turns token counts into USD.

export function formatRate(v: number | null | undefined): string {
  if (v === null || v === undefined) return '—'
  return `${(v * 100).toFixed(0)}%`
}

export function defectsCell(r: ConfigSnapshotRollup): string {
  if (r.defect_data_items === 0) return 'no data'
  return `${r.defects_critical}/${r.defects_major}/${r.defects_minor}`
}

export function itemCostCell(r: ConfigSnapshotRollup): string {
  if (r.items === 0) return '—'
  if (r.item_cost_unknown) {
    return r.item_cost_unknown_items > 0 ? `unknown (${r.item_cost_unknown_items} items)` : 'unknown'
  }
  return formatUsd(r.item_cost_usd)
}

export function costCell(r: ConfigSnapshotRollup): string {
  if (r.cost_unknown) {
    return r.unpriced_calls > 0 ? `unknown (${r.unpriced_calls} unpriced)` : 'unknown'
  }
  return formatUsd(r.cost_usd)
}

// parsePrice turns an editor field into a price: '' is "no price yet"
// (null), anything else must be a non-negative number.
export function parsePrice(s: string): { value: number | null; error?: string } {
  const t = s.trim()
  if (t === '') return { value: null }
  const n = Number(t)
  if (!Number.isFinite(n) || n < 0) return { value: null, error: 'Prices must be non-negative numbers' }
  return { value: n }
}

function renderValue(v: unknown): string {
  if (v === undefined) return ''
  const s = typeof v === 'string' ? v : JSON.stringify(v)
  return s.length > 240 ? `${s.slice(0, 240)}…` : s
}

export function SnapshotRollupTable({ rows }: { rows: ConfigSnapshotRollup[] }) {
  if (rows.length === 0) {
    return <p className="text-sm text-dim">No configuration snapshots recorded yet. They are created when an agent resolves its model for a work item.</p>
  }
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-xs">
        <thead className="text-left text-dim">
          <tr>
            <th className="px-2 py-1">Snapshot</th>
            <th className="px-2 py-1">Agent</th>
            <th className="px-2 py-1">Model</th>
            <th className="px-2 py-1 text-right">Items</th>
            <th className="px-2 py-1 text-right" title="Released on verified evidence (release_decision = release)">Verified</th>
            <th className="px-2 py-1 text-right" title="critical/major/minor from structured review verdicts">Review defects</th>
            <th className="px-2 py-1 text-right">Human-edit rate</th>
            <th className="px-2 py-1 text-right">False success</th>
            <th className="px-2 py-1 text-right" title="Spend of the calls tagged with this snapshot: this agent type's calls only">Agent cost</th>
            <th className="px-2 py-1 text-right" title="Sum of the items' full cost (every agent's calls)">Item cost</th>
            <th className="px-2 py-1" />
          </tr>
        </thead>
        <tbody>
          {rows.map(r => (
            <tr key={r.id} className="border-t border-rule/40" data-testid="snapshot-row">
              <td className="px-2 py-1 font-mono" title={r.id}>
                {shortSnapshotId(r.id)}
                {r.is_current_default && <span className="ml-1 rounded bg-green-700/30 px-1 text-green-300">current</span>}
              </td>
              <td className="px-2 py-1">{r.agent_type === '*' ? 'pipeline (intake)' : r.agent_type}{r.agent_definition_name ? ` · ${r.agent_definition_name}` : ''}</td>
              <td className="px-2 py-1">{r.provider || r.model_id ? `${r.provider}/${r.model_id}` : 'resolved per agent'}</td>
              <td className="px-2 py-1 text-right">{r.items}</td>
              <td className="px-2 py-1 text-right">{formatRate(r.verified_rate)}</td>
              <td className="px-2 py-1 text-right">{defectsCell(r)}</td>
              <td className="px-2 py-1 text-right">{formatRate(r.human_edit_rate)}</td>
              <td className="px-2 py-1 text-right">{r.false_success}</td>
              <td className="px-2 py-1 text-right">{costCell(r)}</td>
              <td className="px-2 py-1 text-right">{itemCostCell(r)}</td>
              <td className="px-2 py-1">
                <Link className="text-accent hover:underline" to={configDiffLink(r.id)}>diff vs current</Link>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

export function DiffView({ diff }: { diff: ConfigSnapshotDiff }) {
  return (
    <div className="space-y-2" data-testid="config-diff">
      <p className="text-xs text-dim">
        <span className="font-mono">{shortSnapshotId(diff.a)}</span> → <span className="font-mono">{shortSnapshotId(diff.b)}</span>
        {diff.b_is_current && ' (what the current default would capture now)'}
      </p>
      {diff.identical ? (
        <p className="text-sm text-green-400">Identical configuration.</p>
      ) : (
        <table className="w-full text-xs">
          <thead className="text-left text-dim">
            <tr><th className="px-2 py-1">Path</th><th className="px-2 py-1">Change</th><th className="px-2 py-1">Before</th><th className="px-2 py-1">After</th></tr>
          </thead>
          <tbody>
            {diff.changes.map(c => (
              <tr key={c.path + c.kind} className="border-t border-rule/40 align-top">
                <td className="px-2 py-1 font-mono">{c.path}</td>
                <td className="px-2 py-1">{c.kind}</td>
                <td className="px-2 py-1 font-mono text-red-300 break-all">{renderValue(c.before)}</td>
                <td className="px-2 py-1 font-mono text-green-300 break-all">{renderValue(c.after)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  )
}

function PriceEditor() {
  const { get, post, del } = useApi()
  const [prices, setPrices] = useState<ModelPrice[]>([])
  const [error, setError] = useState('')
  const [form, setForm] = useState({ provider: '', model_id: '', input: '', output: '', effective_from: '' })

  const load = useCallback(async () => {
    try {
      const data = await get<{ prices: ModelPrice[] }>('/admin/model-prices')
      setPrices(data.prices ?? [])
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Failed to load prices')
    }
  }, [get])

  useEffect(() => { load() }, [load])

  const save = async (provider: string, modelID: string, input: string, output: string, effectiveFrom: string) => {
    const i = parsePrice(input)
    const o = parsePrice(output)
    if (i.error || o.error) {
      setError(i.error ?? o.error ?? '')
      return
    }
    setError('')
    try {
      await post('/admin/model-prices', {
        provider, model_id: modelID, input_per_1m: i.value, output_per_1m: o.value,
        effective_from: effectiveFrom || undefined,
      })
      await load()
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Failed to save price')
    }
  }

  return (
    <section className="space-y-3">
      <h2 className="text-sm font-semibold text-fg">Model prices (USD per 1M tokens)</h2>
      <p className="text-xs text-dim">
        Cost stays “unknown” (never $0) until every model a call used has a price effective at the call time.
        Placeholder rows were created for every model seen so far.
      </p>
      {error && <p className="text-xs text-red-400">{error}</p>}
      <table className="w-full text-xs">
        <thead className="text-left text-dim">
          <tr><th className="px-2 py-1">Provider</th><th className="px-2 py-1">Model</th><th className="px-2 py-1">Input / 1M</th><th className="px-2 py-1">Output / 1M</th><th className="px-2 py-1">Effective from</th><th className="px-2 py-1" /></tr>
        </thead>
        <tbody>
          {prices.map(p => (
            <PriceRow key={p.id} price={p} onSave={(i, o) => save(p.provider, p.model_id, i, o, p.effective_from)} onDelete={async () => { await del(`/admin/model-prices?id=${p.id}`); await load() }} />
          ))}
          <tr className="border-t border-rule/40">
            <td className="px-2 py-1"><input aria-label="provider" className="w-full rounded bg-surface-800 px-1" value={form.provider} onChange={e => setForm({ ...form, provider: e.target.value })} /></td>
            <td className="px-2 py-1"><input aria-label="model" className="w-full rounded bg-surface-800 px-1" value={form.model_id} onChange={e => setForm({ ...form, model_id: e.target.value })} /></td>
            <td className="px-2 py-1"><input aria-label="input price" className="w-20 rounded bg-surface-800 px-1" value={form.input} onChange={e => setForm({ ...form, input: e.target.value })} /></td>
            <td className="px-2 py-1"><input aria-label="output price" className="w-20 rounded bg-surface-800 px-1" value={form.output} onChange={e => setForm({ ...form, output: e.target.value })} /></td>
            <td className="px-2 py-1"><input aria-label="effective from" placeholder="YYYY-MM-DD" className="w-28 rounded bg-surface-800 px-1" value={form.effective_from} onChange={e => setForm({ ...form, effective_from: e.target.value })} /></td>
            <td className="px-2 py-1">
              <button className="rounded bg-accent px-2 py-0.5 text-white" onClick={() => save(form.provider, form.model_id, form.input, form.output, form.effective_from)}>Add</button>
            </td>
          </tr>
        </tbody>
      </table>
    </section>
  )
}

function PriceRow({ price, onSave, onDelete }: { price: ModelPrice; onSave: (input: string, output: string) => void; onDelete: () => void }) {
  const [input, setInput] = useState(price.input_per_1m === null ? '' : String(price.input_per_1m))
  const [output, setOutput] = useState(price.output_per_1m === null ? '' : String(price.output_per_1m))
  const epoch = new Date(price.effective_from).getTime() === 0
  return (
    <tr className="border-t border-rule/40">
      <td className="px-2 py-1">{price.provider}</td>
      <td className="px-2 py-1 font-mono">{price.model_id}</td>
      <td className="px-2 py-1"><input aria-label={`input price ${price.model_id}`} className="w-20 rounded bg-surface-800 px-1" placeholder="not set" value={input} onChange={e => setInput(e.target.value)} /></td>
      <td className="px-2 py-1"><input aria-label={`output price ${price.model_id}`} className="w-20 rounded bg-surface-800 px-1" placeholder="not set" value={output} onChange={e => setOutput(e.target.value)} /></td>
      <td className="px-2 py-1 text-dim">{epoch ? 'always' : new Date(price.effective_from).toLocaleDateString()}</td>
      <td className="px-2 py-1 space-x-2">
        <button className="text-accent hover:underline" onClick={() => onSave(input, output)}>Save</button>
        <button className="text-red-400 hover:underline" onClick={onDelete}>Delete</button>
      </td>
    </tr>
  )
}

export default function ConfigVersions() {
  const { get } = useApi()
  const [params] = useSearchParams()
  const [rows, setRows] = useState<ConfigSnapshotRollup[]>([])
  const [diff, setDiff] = useState<ConfigSnapshotDiff | null>(null)
  const [error, setError] = useState('')
  const a = params.get('a')
  const b = params.get('b') ?? 'current'

  useEffect(() => {
    get<{ snapshots: ConfigSnapshotRollup[] }>('/admin/config-snapshots')
      .then(d => setRows(d.snapshots ?? []))
      .catch((e: unknown) => setError(e instanceof Error ? e.message : 'Failed to load snapshots'))
  }, [get])

  useEffect(() => {
    if (!a) { setDiff(null); return }
    get<ConfigSnapshotDiff>(`/config-snapshots/diff?${new URLSearchParams({ a, b }).toString()}`)
      .then(setDiff)
      .catch((e: unknown) => setError(e instanceof Error ? e.message : 'Failed to diff snapshots'))
  }, [get, a, b])

  return (
    <div className="space-y-6 p-4">
      <header>
        <h1 className="text-lg font-semibold text-fg">Configuration versions</h1>
        <p className="text-xs text-dim">
          Content-addressed snapshots of agent definition, prompts, model, providers and controller version, with the
          production outcomes of the work items that ran under each. Review defects come from structured review verdicts
          and read “no data” until reviews record them.
        </p>
      </header>
      {error && <p className="text-xs text-red-400">{error}</p>}
      {diff && (
        <section className="space-y-2 rounded border border-rule/40 p-3">
          <h2 className="text-sm font-semibold text-fg">Diff</h2>
          <DiffView diff={diff} />
        </section>
      )}
      <section className="space-y-2">
        <h2 className="text-sm font-semibold text-fg">Snapshots</h2>
        <SnapshotRollupTable rows={rows} />
      </section>
      <PriceEditor />
    </div>
  )
}
