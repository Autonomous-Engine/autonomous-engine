import { useState, useEffect, useCallback, useRef } from 'react'
import { useApi } from '@/hooks/useApi'
import type { AuditEntry, AuditAction, CredentialCategory } from '@/types'

const ACTION_OPTIONS: Array<{ value: AuditAction | ''; label: string }> = [
  { value: '', label: 'All Actions' },
  { value: 'created', label: 'Created' },
  { value: 'updated', label: 'Updated' },
  { value: 'deleted', label: 'Deleted' },
  { value: 'read', label: 'Read' },
  { value: 'rotated', label: 'Rotated' },
]

const CATEGORY_OPTIONS: Array<{ value: CredentialCategory | ''; label: string }> = [
  { value: '', label: 'All Categories' },
  { value: 'llm', label: 'LLM' },
  { value: 'github', label: 'GitHub' },
  { value: 'jira', label: 'Jira' },
  { value: 'system', label: 'System' },
  { value: 'dashboard', label: 'Dashboard' },
]

const ACTION_BADGE: Record<AuditAction, string> = {
  created: 'bg-green-700 text-green-100',
  updated: 'bg-blue-700 text-blue-100',
  deleted: 'bg-red-700 text-red-100',
  read: 'bg-surface-700 text-fg',
  rotated: 'bg-orange-700 text-orange-100',
}

const PAGE_SIZE = 50

function formatDateTime(iso: string): string {
  return new Date(iso).toLocaleString()
}

export default function AuditLog() {
  const { get } = useApi()
  const [entries, setEntries] = useState<AuditEntry[]>([])
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState('')
  const [page, setPage] = useState(0)

  const [filterAction, setFilterAction] = useState<AuditAction | ''>('')
  const [filterCategory, setFilterCategory] = useState<CredentialCategory | ''>('')
  const [filterActor, setFilterActor] = useState('')
  const [autoRefresh, setAutoRefresh] = useState(false)

  const timerRef = useRef<ReturnType<typeof setInterval> | null>(null)

  const fetchEntries = useCallback(async () => {
    setLoading(true)
    setError('')
    try {
      const params = new URLSearchParams()
      if (filterAction) params.set('action', filterAction)
      if (filterCategory) params.set('category', filterCategory)
      params.set('limit', String(PAGE_SIZE))
      params.set('offset', String(page * PAGE_SIZE))
      const data = await get<AuditEntry[]>(`/admin/audit?${params.toString()}`)
      setEntries(data ?? [])
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Failed to load audit log')
    } finally {
      setLoading(false)
    }
  }, [get, filterAction, filterCategory, page])

  useEffect(() => { fetchEntries() }, [fetchEntries])

  useEffect(() => {
    if (timerRef.current) clearInterval(timerRef.current)
    if (autoRefresh) {
      timerRef.current = setInterval(fetchEntries, 30_000)
    }
    return () => {
      if (timerRef.current) clearInterval(timerRef.current)
    }
  }, [autoRefresh, fetchEntries])

  function resetPage() { setPage(0) }

  const filteredEntries = filterActor
    ? entries.filter(e =>
        e.actor_type.toLowerCase().includes(filterActor.toLowerCase()) ||
        (e.actor_id ?? '').toLowerCase().includes(filterActor.toLowerCase())
      )
    : entries

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <h1 className="text-lg font-semibold text-fg">Audit Log</h1>
        <label className="flex items-center gap-2 text-sm text-muted">
          <input
            type="checkbox"
            checked={autoRefresh}
            onChange={e => setAutoRefresh(e.target.checked)}
            className="rounded-sm border-rule"
          />
          Auto-refresh (30s)
        </label>
      </div>

      {/* Filter Bar */}
      <div className="flex flex-wrap gap-3">
        <select
          value={filterAction}
          onChange={e => { setFilterAction(e.target.value as AuditAction | ''); resetPage() }}
          className="rounded-sm border border-rule bg-surface-950 px-3 py-2 text-sm text-fg focus:outline-none focus:ring-1 focus:ring-accent"
        >
          {ACTION_OPTIONS.map(o => <option key={o.value} value={o.value}>{o.label}</option>)}
        </select>

        <select
          value={filterCategory}
          onChange={e => { setFilterCategory(e.target.value as CredentialCategory | ''); resetPage() }}
          className="rounded-sm border border-rule bg-surface-950 px-3 py-2 text-sm text-fg focus:outline-none focus:ring-1 focus:ring-accent"
        >
          {CATEGORY_OPTIONS.map(o => <option key={o.value} value={o.value}>{o.label}</option>)}
        </select>

        <input
          type="text"
          value={filterActor}
          onChange={e => setFilterActor(e.target.value)}
          placeholder="Search actor..."
          className="rounded-sm border border-rule bg-surface-950 px-3 py-2 text-sm text-fg focus:outline-none focus:ring-1 focus:ring-accent"
        />

        <button
          onClick={fetchEntries}
          className="rounded-sm border border-rule px-3 py-2 text-sm text-muted hover:bg-surface-700"
        >
          Refresh
        </button>
      </div>

      {error && <p className="text-sm text-red-400">{error}</p>}

      {loading ? (
        <p className="text-sm text-muted">Loading...</p>
      ) : (
        <div className="overflow-x-auto rounded-sm border border-rule">
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b border-rule bg-surface-950 text-left">
                <th className="px-4 py-3 text-xs font-medium uppercase tracking-wider text-muted">Timestamp</th>
                <th className="px-4 py-3 text-xs font-medium uppercase tracking-wider text-muted">Action</th>
                <th className="px-4 py-3 text-xs font-medium uppercase tracking-wider text-muted">Category</th>
                <th className="px-4 py-3 text-xs font-medium uppercase tracking-wider text-muted">Key Name</th>
                <th className="px-4 py-3 text-xs font-medium uppercase tracking-wider text-muted">Actor Type</th>
                <th className="px-4 py-3 text-xs font-medium uppercase tracking-wider text-muted">IP Address</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-rule/50">
              {filteredEntries.length === 0 && (
                <tr>
                  <td colSpan={6} className="px-4 py-6 text-center text-dim">
                    No audit entries found.
                  </td>
                </tr>
              )}
              {filteredEntries.map(entry => (
                <tr key={entry.id} className="bg-surface-900 hover:bg-surface-800/50">
                  <td className="whitespace-nowrap px-4 py-3 text-xs text-muted">
                    {formatDateTime(entry.timestamp)}
                  </td>
                  <td className="px-4 py-3">
                    <span className={`rounded px-2 py-0.5 text-xs font-medium ${ACTION_BADGE[entry.action]}`}>
                      {entry.action}
                    </span>
                  </td>
                  <td className="px-4 py-3 text-xs text-muted">
                    {entry.metadata?.category ?? '—'}
                  </td>
                  <td className="px-4 py-3 font-mono text-xs text-muted">
                    {entry.metadata?.key_name ?? '—'}
                  </td>
                  <td className="px-4 py-3 text-xs text-muted">{entry.actor_type}</td>
                  <td className="px-4 py-3 font-mono text-xs text-muted">{entry.ip_address}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {/* Pagination */}
      <div className="flex items-center gap-3">
        <button
          onClick={() => setPage(p => Math.max(0, p - 1))}
          disabled={page === 0}
          className="rounded-sm border border-rule px-3 py-1.5 text-sm text-muted hover:bg-surface-700 disabled:opacity-40"
        >
          Previous
        </button>
        <span className="text-sm text-muted">Page {page + 1}</span>
        <button
          onClick={() => setPage(p => p + 1)}
          disabled={filteredEntries.length < PAGE_SIZE}
          className="rounded-sm border border-rule px-3 py-1.5 text-sm text-muted hover:bg-surface-700 disabled:opacity-40"
        >
          Next
        </button>
      </div>
    </div>
  )
}
