import type { GraphMeta, RiskLevel } from '@/types'
import { ALL_RISKS, RISK_COLORS } from './palette'

export interface FilterState {
  risks: Set<RiskLevel>
  search: string
  hideExternal: boolean
}

interface GraphFiltersProps {
  value: FilterState
  onChange: (next: FilterState) => void
  meta?: GraphMeta | null
}

export function GraphFilters({ value, onChange, meta }: GraphFiltersProps) {
  const toggleRisk = (r: RiskLevel) => {
    const next = new Set(value.risks)
    if (next.has(r)) next.delete(r)
    else next.add(r)
    onChange({ ...value, risks: next })
  }

  return (
    <div className="flex flex-wrap items-center gap-3">
      <input
        type="text"
        placeholder="Search modules…"
        value={value.search}
        onChange={(e) => onChange({ ...value, search: e.target.value })}
        className="w-60 rounded-sm border border-rule bg-surface-800 px-3 py-1.5 font-mono text-sm text-fg placeholder-dim focus:border-accent focus:outline-none"
        aria-label="Search modules by name or purpose"
      />

      <div className="flex items-center gap-2" role="group" aria-label="Filter by risk level">
        {ALL_RISKS.map((r) => {
          const palette = RISK_COLORS[r]
          const active = value.risks.has(r)
          return (
            <button
              key={r}
              type="button"
              onClick={() => toggleRisk(r)}
              aria-pressed={active}
              className="rounded-sm border px-2 py-0.5 font-mono text-xs font-medium transition-opacity"
              style={{
                borderColor: palette.border,
                color: palette.text,
                background: active ? palette.fill : 'transparent',
                opacity: active ? 1 : 0.45,
              }}
            >
              {r}
            </button>
          )
        })}
      </div>

      <label className="flex items-center gap-1.5 font-mono text-xs text-muted">
        <input
          type="checkbox"
          checked={value.hideExternal}
          onChange={(e) => onChange({ ...value, hideExternal: e.target.checked })}
          className="rounded-sm border-rule bg-surface-800 accent-accent"
        />
        Hide external
      </label>

      {meta && (
        <span className="ml-auto font-mono text-xs text-dim">
          {meta.node_count} node{meta.node_count !== 1 ? 's' : ''} · {meta.edge_count} edge{meta.edge_count !== 1 ? 's' : ''}
        </span>
      )}
    </div>
  )
}
