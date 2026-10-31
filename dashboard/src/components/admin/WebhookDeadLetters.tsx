import { useState, useEffect, useCallback } from 'react'
import { useApi } from '@/hooks/useApi'

interface DeadLetterEntry {
  id: string
  source: string
  event_type: string
  payload: unknown
  error_message: string
  retry_count: number
  max_retries: number
  next_retry_at?: string
  resolved: boolean
  created_at: string
}

function formatDateTime(iso: string): string {
  return new Date(iso).toLocaleString()
}

function sourceBadgeClass(source: string): string {
  switch (source) {
    case 'github':
      return 'bg-surface-700/60 text-fg'
    case 'jira':
      return 'bg-blue-700/40 text-blue-200'
    default:
      return 'bg-surface-700 text-muted'
  }
}

export default function WebhookDeadLetters() {
  const { get, post } = useApi()
  const [entries, setEntries] = useState<DeadLetterEntry[]>([])
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState('')
  const [showResolved, setShowResolved] = useState(false)
  const [actionId, setActionId] = useState<string | null>(null)
  const [expandedId, setExpandedId] = useState<string | null>(null)

  const fetchEntries = useCallback(async () => {
    setLoading(true)
    setError('')
    try {
      const params = new URLSearchParams()
      if (showResolved) params.set('resolved', 'true')
      const data = await get<DeadLetterEntry[]>(`/admin/webhook/dead-letters?${params}`)
      setEntries(data ?? [])
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to load dead letters')
    } finally {
      setLoading(false)
    }
  }, [get, showResolved])

  useEffect(() => {
    fetchEntries()
  }, [fetchEntries])

  async function handleRetry(id: string) {
    setActionId(id)
    try {
      await post(`/admin/webhook/dead-letters/retry?id=${id}`)
      await fetchEntries()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Retry failed')
    } finally {
      setActionId(null)
    }
  }

  async function handleResolve(id: string) {
    setActionId(id)
    try {
      await post(`/admin/webhook/dead-letters/resolve?id=${id}`)
      await fetchEntries()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Resolve failed')
    } finally {
      setActionId(null)
    }
  }

  return (
    <div className="flex flex-col gap-4">
      {/* Header */}
      <div className="flex items-center justify-between gap-4">
        <div>
          <h2 className="text-lg font-semibold text-fg">Webhook Dead Letters</h2>
          <p className="text-sm text-dim">
            Webhook events that failed to publish to Kafka and are pending retry
          </p>
        </div>
        <div className="flex items-center gap-3">
          <label className="flex items-center gap-2 text-sm text-muted cursor-pointer select-none">
            <input
              type="checkbox"
              checked={showResolved}
              onChange={e => setShowResolved(e.target.checked)}
              className="rounded-sm border-rule bg-surface-950 text-accent focus:ring-accent"
            />
            Show resolved
          </label>
          <button
            onClick={fetchEntries}
            disabled={loading}
            className="rounded-sm border border-rule bg-surface-850 px-3 py-1.5 text-xs text-muted hover:bg-surface-700 disabled:opacity-50 transition-colors"
          >
            {loading ? 'Refreshing…' : 'Refresh'}
          </button>
        </div>
      </div>

      {/* Error */}
      {error && (
        <div className="rounded-sm border border-red-500/30 bg-red-500/10 px-4 py-2 text-sm text-red-300">
          {error}
        </div>
      )}

      {/* Empty state */}
      {!loading && entries.length === 0 && (
        <div className="flex h-40 items-center justify-center rounded-sm border border-rule bg-surface-850">
          <p className="text-sm text-dim">
            {showResolved ? 'No dead-letter entries found.' : 'No unresolved dead letters.'}
          </p>
        </div>
      )}

      {/* Loading */}
      {loading && entries.length === 0 && (
        <div className="flex h-40 items-center justify-center">
          <div className="h-6 w-6 animate-spin rounded-full border-2 border-accent border-t-transparent" />
        </div>
      )}

      {/* Table */}
      {entries.length > 0 && (
        <div className="overflow-x-auto rounded-sm border border-rule">
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b border-rule bg-surface-850 text-left">
                <th className="px-4 py-2.5 text-xs font-semibold uppercase tracking-wider text-dim">Source</th>
                <th className="px-4 py-2.5 text-xs font-semibold uppercase tracking-wider text-dim">Event Type</th>
                <th className="px-4 py-2.5 text-xs font-semibold uppercase tracking-wider text-dim">Error</th>
                <th className="px-4 py-2.5 text-xs font-semibold uppercase tracking-wider text-dim">Retries</th>
                <th className="px-4 py-2.5 text-xs font-semibold uppercase tracking-wider text-dim">Next Retry</th>
                <th className="px-4 py-2.5 text-xs font-semibold uppercase tracking-wider text-dim">Created</th>
                <th className="px-4 py-2.5 text-xs font-semibold uppercase tracking-wider text-dim">Status</th>
                <th className="px-4 py-2.5 text-xs font-semibold uppercase tracking-wider text-dim">Actions</th>
              </tr>
            </thead>
            <tbody>
              {entries.map(entry => (
                <>
                  <tr
                    key={entry.id}
                    className="border-b border-rule/40 bg-surface-900 hover:bg-surface-850 cursor-pointer"
                    onClick={() => setExpandedId(expandedId === entry.id ? null : entry.id)}
                  >
                    <td className="px-4 py-3">
                      <span className={['rounded px-1.5 py-0.5 text-xs font-medium', sourceBadgeClass(entry.source)].join(' ')}>
                        {entry.source}
                      </span>
                    </td>
                    <td className="px-4 py-3 font-mono text-xs text-muted">
                      {entry.event_type}
                    </td>
                    <td className="max-w-xs px-4 py-3 text-xs text-red-300 truncate">
                      {entry.error_message}
                    </td>
                    <td className="px-4 py-3 text-center text-xs text-muted">
                      <span className={entry.retry_count >= entry.max_retries ? 'text-red-400 font-semibold' : ''}>
                        {entry.retry_count}/{entry.max_retries}
                      </span>
                    </td>
                    <td className="px-4 py-3 text-xs text-muted">
                      {entry.next_retry_at ? formatDateTime(entry.next_retry_at) : '—'}
                    </td>
                    <td className="px-4 py-3 text-xs text-muted whitespace-nowrap">
                      {formatDateTime(entry.created_at)}
                    </td>
                    <td className="px-4 py-3">
                      {entry.resolved ? (
                        <span className="rounded bg-emerald-500/20 px-1.5 py-0.5 text-xs font-medium text-emerald-300">
                          resolved
                        </span>
                      ) : entry.retry_count >= entry.max_retries ? (
                        <span className="rounded bg-red-500/20 px-1.5 py-0.5 text-xs font-medium text-red-300">
                          exhausted
                        </span>
                      ) : (
                        <span className="rounded bg-amber-500/20 px-1.5 py-0.5 text-xs font-medium text-amber-300">
                          pending
                        </span>
                      )}
                    </td>
                    <td className="px-4 py-3">
                      {!entry.resolved && (
                        <div className="flex items-center gap-2" onClick={e => e.stopPropagation()}>
                          <button
                            disabled={actionId === entry.id}
                            onClick={() => handleRetry(entry.id)}
                            className="rounded bg-accent/10 px-2 py-1 text-xs font-medium text-accent border border-indigo-600/30 hover:bg-accent/15 disabled:opacity-50 transition-colors"
                          >
                            {actionId === entry.id ? '…' : 'Retry Now'}
                          </button>
                          <button
                            disabled={actionId === entry.id}
                            onClick={() => handleResolve(entry.id)}
                            className="rounded bg-surface-700/40 px-2 py-1 text-xs font-medium text-muted border border-rule/40 hover:bg-surface-700/60 disabled:opacity-50 transition-colors"
                          >
                            Resolve
                          </button>
                        </div>
                      )}
                    </td>
                  </tr>
                  {/* Expanded payload row */}
                  {expandedId === entry.id && (
                    <tr key={`${entry.id}-expanded`} className="border-b border-rule/40 bg-surface-950">
                      <td colSpan={8} className="px-4 py-3">
                        <p className="mb-1 text-xs font-semibold text-muted">Payload</p>
                        <pre className="overflow-x-auto rounded bg-surface-950 p-3 text-xs text-muted max-h-48">
                          {JSON.stringify(entry.payload, null, 2)}
                        </pre>
                      </td>
                    </tr>
                  )}
                </>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  )
}
