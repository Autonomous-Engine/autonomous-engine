import { useCallback, useEffect, useMemo, useState } from 'react'
import { useSnapshotDiff } from '@/hooks/useCodexV2'
import type { CodexBlob, CodexChangedPair, CodexDiffResponse } from '@/types/codex-v2'

interface Props {
  repoId: string
  fromSHA: string
  toSHA: string
}

// ── Section/Category classification ────────────────────────────────────────

type DiffCategory = 'modules' | 'patterns' | 'contracts' | 'architecture' | 'other'

const CATEGORY_META: Record<DiffCategory, { label: string; icon: string; color: string }> = {
  modules:      { label: 'Modules',       icon: '■', color: 'text-blue-400' },
  patterns:     { label: 'Patterns',      icon: '◆', color: 'text-violet-400' },
  contracts:    { label: 'Contracts',      icon: '●', color: 'text-cyan-400' },
  architecture: { label: 'Architecture',   icon: '▲', color: 'text-amber-400' },
  other:        { label: 'Other',          icon: '○', color: 'text-muted' },
}

function classifyBlob(blob: CodexBlob): DiffCategory {
  const sec = blob.section.toLowerCase()
  const ext = blob.extractor.toLowerCase()

  // Module-related sections
  if (sec === 'module' || sec === 'modules' || ext.includes('module')) return 'modules'

  // Pattern-related
  if (sec === 'pattern' || sec === 'patterns' || sec === 'component_patterns'
    || sec === 'coding_style' || sec === 'api_style' || sec === 'css_methodology'
    || sec === 'testing_conventions' || sec === 'design_tokens'
    || sec === 'design_tokens_css_vars' || sec === 'component_library'
    || ext.includes('pattern') || ext.includes('style') || ext.includes('convention')
    || ext.includes('design_token') || ext.includes('component_library')
    || ext.includes('css_methodology'))
    return 'patterns'

  // Contract edges
  if (sec === 'contract_edges' || sec === 'contracts' || ext.includes('contract'))
    return 'contracts'

  // Architecture / topology / git strategy
  if (sec === 'architecture' || sec === 'product_topology' || sec === 'git_strategy'
    || sec === 'topology' || ext.includes('topology') || ext.includes('git_strategy'))
    return 'architecture'

  return 'other'
}

function classifyPair(pair: CodexChangedPair): DiffCategory {
  return classifyBlob(pair.from)
}

// ── Field-level diff helpers ───────────────────────────────────────────────

/** Extract a human-readable name from a blob payload. */
function blobDisplayName(blob: CodexBlob): string {
  const p = blob.payload as Record<string, unknown>
  return (
    (p.name as string) ??
    (p.module_name as string) ??
    (p.pattern_name as string) ??
    (p.contract_name as string) ??
    blob.extractor
  )
}

/** Get the most relevant fields from a blob payload for display. */
function getPayloadFields(payload: Record<string, unknown>): Array<{ key: string; value: unknown }> {
  // Prioritized field ordering
  const priority = [
    'name', 'module_name', 'pattern_name', 'contract_name',
    'purpose', 'description', 'summary',
    'risk_level', 'confidence',
    'dependencies', 'dependents',
    'rules', 'key_rules', 'when_to_use',
    'rationale', 'common_mistakes',
    'reference_files',
    'role', 'evidence',
    'framework', 'conventions', 'style',
  ]

  const seen = new Set<string>()
  const result: Array<{ key: string; value: unknown }> = []

  // First add priority fields that exist
  for (const key of priority) {
    if (key in payload && payload[key] !== undefined && payload[key] !== null) {
      result.push({ key, value: payload[key] })
      seen.add(key)
    }
  }

  // Then add remaining fields
  for (const [key, value] of Object.entries(payload)) {
    if (!seen.has(key) && value !== undefined && value !== null) {
      result.push({ key, value })
    }
  }

  return result
}

/** Format a single value for display. */
function formatValue(value: unknown): string {
  if (value === null || value === undefined) return '—'
  if (typeof value === 'string') return value
  if (typeof value === 'number' || typeof value === 'boolean') return String(value)
  if (Array.isArray(value)) {
    if (value.length === 0) return '(empty)'
    if (value.every((v) => typeof v === 'string')) return value.join(', ')
    return JSON.stringify(value, null, 2)
  }
  return JSON.stringify(value, null, 2)
}

/** Check if two values are different (deep comparison via JSON). */
function valuesEqual(a: unknown, b: unknown): boolean {
  return JSON.stringify(a) === JSON.stringify(b)
}

/** Format a field key into a human-readable label. */
function fieldLabel(key: string): string {
  return key.replace(/_/g, ' ').replace(/\b\w/g, (c) => c.toUpperCase())
}

// ── Diff stat counters ─────────────────────────────────────────────────────

interface DiffStats {
  added: number
  removed: number
  changed: number
  fieldChanges: number
}

function computeFieldChanges(changed: CodexChangedPair[]): number {
  let count = 0
  for (const pair of changed) {
    const fromPayload = pair.from.payload as Record<string, unknown>
    const toPayload = pair.to.payload as Record<string, unknown>
    const allKeys = new Set([...Object.keys(fromPayload), ...Object.keys(toPayload)])
    for (const key of allKeys) {
      if (!valuesEqual(fromPayload[key], toPayload[key])) count++
    }
  }
  return count
}

// ── Sub-components ─────────────────────────────────────────────────────────

function StatBadge({ label, count, color }: { label: string; count: number; color: string }) {
  if (count === 0) return null
  return (
    <span className={`inline-flex items-center gap-1.5 rounded-sm border px-2 py-0.5 font-mono text-xs ${color}`}>
      {count} {label}
    </span>
  )
}

/** Renders a single field row in side-by-side mode for a changed pair. */
function ChangedFieldRow({ fieldKey, fromValue, toValue }: {
  fieldKey: string
  fromValue: unknown
  toValue: unknown
}) {
  const changed = !valuesEqual(fromValue, toValue)
  const fromStr = formatValue(fromValue)
  const toStr = formatValue(toValue)
  const fromMissing = fromValue === undefined || fromValue === null
  const toMissing = toValue === undefined || toValue === null

  return (
    <tr className={changed ? 'bg-yellow-500/5' : ''}>
      <td className="w-36 whitespace-nowrap px-3 py-1.5 align-top font-mono text-xs text-dim">
        {fieldLabel(fieldKey)}
      </td>
      <td className={`px-3 py-1.5 align-top font-mono text-xs ${
        changed
          ? fromMissing
            ? 'text-muted italic'
            : 'text-red-400 bg-red-500/5'
          : 'text-muted'
      }`}>
        <pre className="whitespace-pre-wrap">{fromMissing ? '—' : fromStr}</pre>
      </td>
      <td className={`px-3 py-1.5 align-top font-mono text-xs ${
        changed
          ? toMissing
            ? 'text-muted italic'
            : 'text-emerald-400 bg-emerald-500/5'
          : 'text-muted'
      }`}>
        <pre className="whitespace-pre-wrap">{toMissing ? '—' : toStr}</pre>
      </td>
    </tr>
  )
}

/** Side-by-side diff table for a changed blob pair. */
function ChangedBlobDiff({ pair }: { pair: CodexChangedPair }) {
  const [expanded, setExpanded] = useState(true)
  const fromPayload = pair.from.payload as Record<string, unknown>
  const toPayload = pair.to.payload as Record<string, unknown>

  const allKeys = useMemo(() => {
    const keys = new Set([...Object.keys(fromPayload), ...Object.keys(toPayload)])
    // Sort: changed fields first, then alphabetical
    return Array.from(keys).sort((a, b) => {
      const aChanged = !valuesEqual(fromPayload[a], toPayload[a])
      const bChanged = !valuesEqual(fromPayload[b], toPayload[b])
      if (aChanged && !bChanged) return -1
      if (!aChanged && bChanged) return 1
      return a.localeCompare(b)
    })
  }, [fromPayload, toPayload])

  const changedCount = allKeys.filter(
    (k) => !valuesEqual(fromPayload[k], toPayload[k]),
  ).length

  return (
    <div className="rounded-sm border border-yellow-500/20 bg-yellow-500/5">
      <button
        type="button"
        onClick={() => setExpanded(!expanded)}
        className="flex w-full items-center gap-2 px-3 py-2 text-left transition-colors hover:bg-surface-800"
      >
        <span className="font-mono text-xs text-yellow-400">
          {expanded ? '▾' : '▸'}
        </span>
        <span className="rounded-sm border border-yellow-500/20 bg-yellow-500/10 px-1.5 py-0.5 font-mono text-[10px] font-medium uppercase text-yellow-400">
          Changed
        </span>
        <span className="font-mono text-sm text-fg">{blobDisplayName(pair.from)}</span>
        <span className="font-mono text-xs text-dim">
          {pair.from.extractor}
        </span>
        <span className="ml-auto font-mono text-xs text-yellow-400/70">
          {changedCount} field{changedCount !== 1 ? 's' : ''} changed
        </span>
      </button>
      {expanded && (
        <div className="overflow-x-auto border-t border-rule">
          <table className="w-full border-collapse text-sm">
            <thead>
              <tr className="bg-surface-800 text-xs uppercase tracking-wider text-dim">
                <th className="w-36 px-3 py-1.5 text-left font-mono font-normal">Field</th>
                <th className="px-3 py-1.5 text-left font-mono font-normal">
                  Before <span className="text-dim">({pair.from.hash.slice(0, 8)})</span>
                </th>
                <th className="px-3 py-1.5 text-left font-mono font-normal">
                  After <span className="text-dim">({pair.to.hash.slice(0, 8)})</span>
                </th>
              </tr>
            </thead>
            <tbody className="divide-y divide-rule/50">
              {allKeys.map((key) => (
                <ChangedFieldRow
                  key={key}
                  fieldKey={key}
                  fromValue={fromPayload[key]}
                  toValue={toPayload[key]}
                />
              ))}
            </tbody>
          </table>
          <div className="flex gap-4 border-t border-rule/50 px-3 py-1.5 text-[10px] text-dim">
            <span>confidence: {Math.round(pair.from.confidence * 100)}% → {Math.round(pair.to.confidence * 100)}%</span>
            <span>section: {pair.from.section}</span>
          </div>
        </div>
      )}
    </div>
  )
}

/** Displays an added or removed blob as a field list. */
function BlobCard({ blob, kind }: { blob: CodexBlob; kind: 'added' | 'removed' }) {
  const [expanded, setExpanded] = useState(false)
  const payload = blob.payload as Record<string, unknown>
  const fields = getPayloadFields(payload)

  const borderColor = kind === 'added' ? 'border-emerald-500/20' : 'border-red-500/20'
  const bgColor = kind === 'added' ? 'bg-emerald-500/5' : 'bg-red-500/5'
  const badgeColor = kind === 'added'
    ? 'border-emerald-500/20 bg-emerald-500/10 text-emerald-400'
    : 'border-red-500/20 bg-red-500/10 text-red-400'
  const arrowColor = kind === 'added' ? 'text-emerald-400' : 'text-red-400'

  return (
    <div className={`rounded-sm border ${borderColor} ${bgColor}`}>
      <button
        type="button"
        onClick={() => setExpanded(!expanded)}
        className="flex w-full items-center gap-2 px-3 py-2 text-left transition-colors hover:bg-surface-800"
      >
        <span className={`font-mono text-xs ${arrowColor}`}>
          {expanded ? '▾' : '▸'}
        </span>
        <span className={`rounded-sm border px-1.5 py-0.5 font-mono text-[10px] font-medium uppercase ${badgeColor}`}>
          {kind === 'added' ? 'Added' : 'Removed'}
        </span>
        <span className="font-mono text-sm text-fg">{blobDisplayName(blob)}</span>
        <span className="font-mono text-xs text-dim">{blob.extractor}</span>
        <span className="ml-auto font-mono text-xs text-dim">
          {fields.length} field{fields.length !== 1 ? 's' : ''}
        </span>
      </button>
      {expanded && (
        <div className="overflow-x-auto border-t border-rule">
          <table className="w-full border-collapse text-sm">
            <thead>
              <tr className="bg-surface-800 text-xs uppercase tracking-wider text-dim">
                <th className="w-36 px-3 py-1.5 text-left font-mono font-normal">Field</th>
                <th className="px-3 py-1.5 text-left font-mono font-normal">Value</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-rule/50">
              {fields.map(({ key, value }) => (
                <tr key={key}>
                  <td className="w-36 whitespace-nowrap px-3 py-1.5 align-top font-mono text-xs text-dim">
                    {fieldLabel(key)}
                  </td>
                  <td className={`px-3 py-1.5 align-top font-mono text-xs ${
                    kind === 'added' ? 'text-emerald-400' : 'text-red-400'
                  }`}>
                    <pre className="whitespace-pre-wrap">{formatValue(value)}</pre>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
          <div className="flex gap-4 border-t border-rule/50 px-3 py-1.5 text-[10px] text-dim">
            <span>hash: {blob.hash.slice(0, 12)}</span>
            <span>confidence: {Math.round(blob.confidence * 100)}%</span>
            <span>section: {blob.section}</span>
          </div>
        </div>
      )}
    </div>
  )
}

/** A collapsible category section in the diff view. */
function CategorySection({
  category,
  added,
  removed,
  changed,
}: {
  category: DiffCategory
  added: CodexBlob[]
  removed: CodexBlob[]
  changed: CodexChangedPair[]
}) {
  const [expanded, setExpanded] = useState(true)
  const meta = CATEGORY_META[category]
  const total = added.length + removed.length + changed.length

  if (total === 0) return null

  return (
    <div className="rounded-sm border border-rule bg-surface-800">
      <button
        type="button"
        onClick={() => setExpanded(!expanded)}
        className="flex w-full items-center gap-2 px-4 py-3 text-left transition-colors hover:bg-surface-800/80"
      >
        <span className={`font-mono text-xs ${expanded ? 'rotate-0' : '-rotate-90'} transition-transform`}>
          {expanded ? '▾' : '▸'}
        </span>
        <span className={`font-mono text-sm ${meta.color}`}>{meta.icon}</span>
        <span className="font-display text-sm text-fg">{meta.label}</span>
        <div className="ml-auto flex gap-2">
          <StatBadge label="added" count={added.length} color="border-emerald-500/20 bg-emerald-500/10 text-emerald-400" />
          <StatBadge label="removed" count={removed.length} color="border-red-500/20 bg-red-500/10 text-red-400" />
          <StatBadge label="changed" count={changed.length} color="border-yellow-500/20 bg-yellow-500/10 text-yellow-400" />
        </div>
      </button>

      {expanded && (
        <div className="space-y-2 border-t border-rule p-3">
          {changed.map((pair, i) => (
            <ChangedBlobDiff key={`c-${i}`} pair={pair} />
          ))}
          {added.map((blob) => (
            <BlobCard key={`a-${blob.hash}`} blob={blob} kind="added" />
          ))}
          {removed.map((blob) => (
            <BlobCard key={`r-${blob.hash}`} blob={blob} kind="removed" />
          ))}
        </div>
      )}
    </div>
  )
}

// ── Main Component ─────────────────────────────────────────────────────────

export default function SnapshotDiff({ repoId, fromSHA, toSHA }: Props) {
  const { fetchDiff } = useSnapshotDiff()
  const [diff, setDiff] = useState<CodexDiffResponse | null>(null)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [filterCategory, setFilterCategory] = useState<DiffCategory | 'all'>('all')
  const [filterKind, setFilterKind] = useState<'all' | 'added' | 'removed' | 'changed'>('all')

  const load = useCallback(async () => {
    setLoading(true)
    setError(null)
    try {
      const data = await fetchDiff(repoId, fromSHA, toSHA)
      setDiff(data)
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : 'Failed to load diff')
      setDiff(null)
    } finally {
      setLoading(false)
    }
  }, [repoId, fromSHA, toSHA, fetchDiff])

  useEffect(() => {
    load()
  }, [load])

  // Categorize all blobs
  const categorized = useMemo(() => {
    if (!diff) return null

    const categories: DiffCategory[] = ['modules', 'patterns', 'contracts', 'architecture', 'other']
    const result = new Map<DiffCategory, { added: CodexBlob[]; removed: CodexBlob[]; changed: CodexChangedPair[] }>()

    for (const cat of categories) {
      result.set(cat, { added: [], removed: [], changed: [] })
    }

    for (const blob of diff.added) {
      const cat = classifyBlob(blob)
      result.get(cat)!.added.push(blob)
    }
    for (const blob of diff.removed) {
      const cat = classifyBlob(blob)
      result.get(cat)!.removed.push(blob)
    }
    for (const pair of diff.changed) {
      const cat = classifyPair(pair)
      result.get(cat)!.changed.push(pair)
    }

    return result
  }, [diff])

  const stats: DiffStats | null = useMemo(() => {
    if (!diff) return null
    return {
      added: diff.added.length,
      removed: diff.removed.length,
      changed: diff.changed.length,
      fieldChanges: computeFieldChanges(diff.changed),
    }
  }, [diff])

  // Active categories (that have content after filtering)
  const activeCategories = useMemo(() => {
    if (!categorized) return []
    const categories: DiffCategory[] = ['modules', 'patterns', 'contracts', 'architecture', 'other']
    return categories.filter((cat) => {
      const data = categorized.get(cat)!
      if (filterCategory !== 'all' && filterCategory !== cat) return false
      if (filterKind === 'all') return data.added.length + data.removed.length + data.changed.length > 0
      if (filterKind === 'added') return data.added.length > 0
      if (filterKind === 'removed') return data.removed.length > 0
      if (filterKind === 'changed') return data.changed.length > 0
      return false
    })
  }, [categorized, filterCategory, filterKind])

  if (loading) {
    return (
      <div className="flex h-32 items-center justify-center">
        <div className="flex items-center gap-3">
          <div className="h-5 w-5 animate-spin rounded-full border-2 border-accent border-t-transparent" />
          <span className="font-mono text-sm text-muted">Computing diff...</span>
        </div>
      </div>
    )
  }

  if (error) {
    return (
      <div className="rounded-sm border border-red-500/30 bg-red-500/10 p-3 text-sm text-red-400">
        {error}
      </div>
    )
  }

  if (!diff || !categorized || !stats) return null

  const isEmpty = stats.added === 0 && stats.removed === 0 && stats.changed === 0

  if (isEmpty) {
    return (
      <div className="flex h-32 items-center justify-center rounded-sm border border-rule bg-surface-800">
        <p className="font-mono text-sm text-muted">
          No differences between <span className="text-fg">{fromSHA.slice(0, 8)}</span> and{' '}
          <span className="text-fg">{toSHA.slice(0, 8)}</span>.
        </p>
      </div>
    )
  }

  // Count items per category for the filter badges
  const categoryCounts = new Map<DiffCategory, number>()
  for (const [cat, data] of categorized) {
    categoryCounts.set(cat, data.added.length + data.removed.length + data.changed.length)
  }

  return (
    <div className="space-y-4">
      {/* Header */}
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h3 className="font-display text-sm text-fg">
            Codex Diff
          </h3>
          <p className="font-mono text-xs text-muted">
            {fromSHA.slice(0, 8)} &rarr; {toSHA.slice(0, 8)}
          </p>
        </div>

        {/* Summary stats */}
        <div className="flex flex-wrap gap-2">
          <StatBadge
            label="added"
            count={stats.added}
            color="border-emerald-500/20 bg-emerald-500/10 text-emerald-400"
          />
          <StatBadge
            label="removed"
            count={stats.removed}
            color="border-red-500/20 bg-red-500/10 text-red-400"
          />
          <StatBadge
            label="changed"
            count={stats.changed}
            color="border-yellow-500/20 bg-yellow-500/10 text-yellow-400"
          />
          {stats.fieldChanges > 0 && (
            <span className="font-mono text-xs text-dim">
              ({stats.fieldChanges} field-level change{stats.fieldChanges !== 1 ? 's' : ''})
            </span>
          )}
        </div>
      </div>

      {/* Filters */}
      <div className="flex flex-wrap items-center gap-2">
        <span className="font-mono text-xs text-dim">Filter:</span>

        {/* Category filter */}
        <div className="flex gap-1">
          <FilterButton
            active={filterCategory === 'all'}
            onClick={() => setFilterCategory('all')}
            label="All categories"
          />
          {(['modules', 'patterns', 'contracts', 'architecture', 'other'] as DiffCategory[]).map((cat) => {
            const count = categoryCounts.get(cat) ?? 0
            if (count === 0) return null
            return (
              <FilterButton
                key={cat}
                active={filterCategory === cat}
                onClick={() => setFilterCategory(filterCategory === cat ? 'all' : cat)}
                label={`${CATEGORY_META[cat].label} (${count})`}
              />
            )
          })}
        </div>

        <span className="text-dim">|</span>

        {/* Kind filter */}
        <div className="flex gap-1">
          <FilterButton active={filterKind === 'all'} onClick={() => setFilterKind('all')} label="All kinds" />
          {stats.added > 0 && (
            <FilterButton
              active={filterKind === 'added'}
              onClick={() => setFilterKind(filterKind === 'added' ? 'all' : 'added')}
              label={`Added (${stats.added})`}
              variant="green"
            />
          )}
          {stats.removed > 0 && (
            <FilterButton
              active={filterKind === 'removed'}
              onClick={() => setFilterKind(filterKind === 'removed' ? 'all' : 'removed')}
              label={`Removed (${stats.removed})`}
              variant="red"
            />
          )}
          {stats.changed > 0 && (
            <FilterButton
              active={filterKind === 'changed'}
              onClick={() => setFilterKind(filterKind === 'changed' ? 'all' : 'changed')}
              label={`Changed (${stats.changed})`}
              variant="yellow"
            />
          )}
        </div>
      </div>

      {/* Diff content by category */}
      <div className="space-y-3">
        {activeCategories.map((cat) => {
          const data = categorized.get(cat)!
          return (
            <CategorySection
              key={cat}
              category={cat}
              added={filterKind === 'removed' ? [] : data.added}
              removed={filterKind === 'added' ? [] : data.removed}
              changed={filterKind === 'added' || filterKind === 'removed' ? [] : data.changed}
            />
          )
        })}

        {activeCategories.length === 0 && (
          <div className="flex h-24 items-center justify-center rounded-sm border border-rule bg-surface-800">
            <p className="font-mono text-sm text-muted">
              No changes match the selected filters.
            </p>
          </div>
        )}
      </div>
    </div>
  )
}

// ── Filter button ──────────────────────────────────────────────────────────

function FilterButton({
  active,
  onClick,
  label,
  variant,
}: {
  active: boolean
  onClick: () => void
  label: string
  variant?: 'green' | 'red' | 'yellow'
}) {
  let cls = 'rounded-sm border px-2 py-0.5 font-mono text-[11px] transition-colors '
  if (active) {
    if (variant === 'green') cls += 'border-emerald-500/40 bg-emerald-500/15 text-emerald-400'
    else if (variant === 'red') cls += 'border-red-500/40 bg-red-500/15 text-red-400'
    else if (variant === 'yellow') cls += 'border-yellow-500/40 bg-yellow-500/15 text-yellow-400'
    else cls += 'border-accent bg-accent/10 text-accent'
  } else {
    cls += 'border-rule bg-transparent text-muted hover:text-fg hover:border-fg/30'
  }

  return (
    <button type="button" onClick={onClick} className={cls}>
      {label}
    </button>
  )
}
