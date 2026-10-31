import { useState, useEffect, useCallback } from 'react'
import { useApi } from '@/hooks/useApi'
import VerifierVerdict, { verifierVerdictFrom } from '@/components/VerifierVerdict'

// ─── Types ───────────────────────────────────────────────────────────────────

interface PendingChange {
  id: string
  work_item_id: string
  work_item_title: string
  repository_id: string
  repo_name: string
  branch: string
  base_branch: string
  files_changed: string[]
  diff_stats: string
  pr_title: string
  status: string
  created_at: string
}

// R14 (V5): the independent verifier's decision for the gated head,
// recorded before the change reached the gate.
interface PendingVerification {
  decision: string
  reason: string
  recorded_at: string
  verdict?: unknown
}

interface PendingChangeDetail extends PendingChange {
  pr_description: string
  head_sha?: string
  verification?: PendingVerification
}

interface Product {
  id: string
  name: string
}

interface PrGateSetting {
  product_id: string
  mode: 'auto' | 'manual'
}

interface ApproveResponse {
  status: string
  pr_url: string
}

type FilterTab = 'all' | 'pending' | 'approved' | 'rejected'

// ─── Helpers ─────────────────────────────────────────────────────────────────

function formatDate(iso: string): string {
  if (!iso) return '—'
  const d = new Date(iso)
  return d.toLocaleDateString(undefined, {
    month: 'short',
    day: 'numeric',
    year: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
  })
}

// ─── Status Badge ────────────────────────────────────────────────────────────

function StatusBadge({ status }: { status: string }) {
  const styles: Record<string, string> = {
    pending: 'bg-amber-500/20 text-amber-400',
    approved: 'bg-emerald-500/20 text-emerald-400',
    rejected: 'bg-red-500/20 text-red-400',
    expired: 'bg-surface-700 text-dim',
  }
  return (
    <span className={`rounded-sm px-2 py-0.5 text-xs font-medium ${styles[status] || styles.expired}`}>
      {status}
    </span>
  )
}

// ─── Detail View ─────────────────────────────────────────────────────────────

interface DetailViewProps {
  change: PendingChangeDetail
  onClose: () => void
  onApprove: (id: string) => void
  onReject: (id: string) => void
  approving: boolean
  rejecting: boolean
}

function DetailView({ change, onClose, onApprove, onReject, approving, rejecting }: DetailViewProps) {
  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/60">
      <div className="mx-4 max-h-[85vh] w-full max-w-2xl overflow-y-auto rounded-sm border border-rule bg-surface-950 p-6">
        {/* Header */}
        <div className="mb-4 flex items-start justify-between">
          <div className="min-w-0 flex-1">
            <h3 className="text-base font-semibold text-fg">{change.pr_title}</h3>
            <p className="mt-1 font-mono text-xs text-muted">
              {change.branch} → {change.base_branch}
            </p>
          </div>
          <button
            onClick={onClose}
            className="ml-4 rounded-sm px-2 py-1 text-sm text-muted hover:bg-surface-800 hover:text-fg"
          >
            ✕
          </button>
        </div>

        {/* Independent verification — the verdict the operator decides with (R14 V5). A local remote always
            ends at this gate, so the verdict is shown here before the description. */}
        <div className="mb-4 rounded-sm border border-rule/40 bg-surface-900 p-4" data-testid="gate-verification">
          <p className="mb-2 text-xs font-medium uppercase text-dim">Independent verification</p>
          {(() => {
            const v = change.verification
            if (!v) {
              return <p className="text-xs text-dim italic">No verification recorded for this head{change.head_sha ? ` (${change.head_sha.slice(0, 12)})` : ''}.</p>
            }
            const data = v.verdict ? verifierVerdictFrom(v.verdict) : null
            if (data) return <VerifierVerdict data={data} />
            return (
              <p className="text-xs text-fg">
                <span className="font-mono">{v.decision}</span> — {v.reason}
              </p>
            )
          })()}
        </div>

        {/* PR Description */}
        <div className="mb-4 rounded-sm border border-rule/40 bg-surface-900 p-4">
          <p className="mb-1 text-xs font-medium uppercase text-dim">Description</p>
          <div className="whitespace-pre-wrap font-mono text-sm text-fg">
            {change.pr_description || '(no description)'}
          </div>
        </div>

        {/* Meta info */}
        <div className="mb-4 grid grid-cols-2 gap-4">
          <div>
            <p className="text-xs font-medium uppercase text-dim">Repository</p>
            <p className="font-mono text-sm text-fg">{change.repo_name}</p>
          </div>
          <div>
            <p className="text-xs font-medium uppercase text-dim">Work Item</p>
            <p className="text-sm text-accent">{change.work_item_title}</p>
          </div>
          <div>
            <p className="text-xs font-medium uppercase text-dim">Status</p>
            <StatusBadge status={change.status} />
          </div>
          <div>
            <p className="text-xs font-medium uppercase text-dim">Created</p>
            <p className="font-mono text-sm text-muted">{formatDate(change.created_at)}</p>
          </div>
        </div>

        {/* Files changed */}
        <div className="mb-6">
          <p className="mb-2 text-xs font-medium uppercase text-dim">
            Files Changed ({change.files_changed.length})
          </p>
          <div className="max-h-48 overflow-y-auto rounded-sm border border-rule/40 bg-surface-900">
            {change.files_changed.map((file) => (
              <div
                key={file}
                className="border-b border-rule/40 px-3 py-1.5 font-mono text-xs text-muted last:border-b-0"
              >
                {file}
              </div>
            ))}
          </div>
        </div>

        {/* Actions */}
        {change.status === 'pending' && (
          <div className="flex gap-3">
            <button
              onClick={() => onApprove(change.id)}
              disabled={approving}
              className="flex-1 rounded-sm bg-accent px-4 py-2 text-sm font-semibold text-surface-900 hover:bg-accent-hover disabled:opacity-50"
            >
              {approving ? 'Creating PR...' : 'Approve & Create PR'}
            </button>
            <button
              onClick={() => onReject(change.id)}
              disabled={rejecting}
              className="rounded-sm bg-red-600 px-4 py-2 text-sm font-semibold text-white hover:bg-red-700 disabled:opacity-50"
            >
              {rejecting ? 'Rejecting...' : 'Reject'}
            </button>
          </div>
        )}
      </div>
    </div>
  )
}

// ─── PR Gate Settings ────────────────────────────────────────────────────────

function PrGateSettings() {
  const { get, post } = useApi()
  const [expanded, setExpanded] = useState(false)
  const [products, setProducts] = useState<Product[]>([])
  const [gates, setGates] = useState<Record<string, 'auto' | 'manual'>>({})
  const [loading, setLoading] = useState(false)
  const [toggling, setToggling] = useState<string | null>(null)

  const loadSettings = useCallback(async () => {
    setLoading(true)
    try {
      const prods = await get<Product[]>('/admin/products')
      setProducts(prods)
      const gateMap: Record<string, 'auto' | 'manual'> = {}
      await Promise.all(
        prods.map(async (p) => {
          try {
            const gate = await get<PrGateSetting>(`/admin/products/pr-gate?product_id=${p.id}`)
            gateMap[p.id] = gate.mode
          } catch {
            gateMap[p.id] = 'auto'
          }
        }),
      )
      setGates(gateMap)
    } catch {
      // silently fail — products may not be loaded yet
    } finally {
      setLoading(false)
    }
  }, [get])

  useEffect(() => {
    if (expanded && products.length === 0) {
      loadSettings()
    }
  }, [expanded, products.length, loadSettings])

  async function handleToggle(productId: string) {
    const current = gates[productId] || 'auto'
    const next = current === 'auto' ? 'manual' : 'auto'
    setToggling(productId)
    try {
      await post<{ status: string; mode: string }>('/admin/products/pr-gate', {
        product_id: productId,
        mode: next,
      })
      setGates((prev) => ({ ...prev, [productId]: next }))
    } catch {
      // revert on failure — no-op since state was not changed
    } finally {
      setToggling(null)
    }
  }

  return (
    <div className="rounded-sm border border-rule bg-surface-950">
      <button
        onClick={() => setExpanded((v) => !v)}
        className="flex w-full items-center justify-between px-4 py-3 text-left transition-colors hover:bg-surface-800"
      >
        <span className="text-sm font-semibold text-fg">PR Gate Settings</span>
        <span className="text-muted">{expanded ? '▾' : '▸'}</span>
      </button>
      {expanded && (
        <div className="border-t border-rule px-4 py-3">
          {loading ? (
            <p className="text-sm text-muted">Loading products...</p>
          ) : products.length === 0 ? (
            <p className="text-sm text-muted">No products configured.</p>
          ) : (
            <div className="space-y-3">
              <p className="text-xs text-dim">
                Set each product to <span className="font-semibold text-muted">manual</span> to require approval before PRs are created, or{' '}
                <span className="font-semibold text-muted">auto</span> to create PRs automatically.
              </p>
              {products.map((p) => {
                const mode = gates[p.id] || 'auto'
                const isToggling = toggling === p.id
                return (
                  <div
                    key={p.id}
                    className="flex items-center justify-between rounded-sm border border-rule/40 bg-surface-900 px-3 py-2"
                  >
                    <span className="text-sm text-fg">{p.name}</span>
                    <button
                      onClick={() => handleToggle(p.id)}
                      disabled={isToggling}
                      className={`relative inline-flex h-6 w-11 items-center rounded-full transition-colors ${
                        mode === 'manual' ? 'bg-accent' : 'bg-surface-700'
                      } ${isToggling ? 'opacity-50' : ''}`}
                    >
                      <span
                        className={`inline-block h-4 w-4 rounded-full bg-white transition-transform ${
                          mode === 'manual' ? 'translate-x-6' : 'translate-x-1'
                        }`}
                      />
                    </button>
                    <span className="w-16 text-right font-mono text-xs text-muted">
                      {mode}
                    </span>
                  </div>
                )
              })}
            </div>
          )}
        </div>
      )}
    </div>
  )
}

// ─── Main Component ──────────────────────────────────────────────────────────

export default function PendingChanges() {
  const { get, post } = useApi()
  const [changes, setChanges] = useState<PendingChange[]>([])
  const [filter, setFilter] = useState<FilterTab>('pending')
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')

  // Detail view state
  const [detailChange, setDetailChange] = useState<PendingChangeDetail | null>(null)
  const [detailLoading, setDetailLoading] = useState(false)

  // Action states
  const [approving, setApproving] = useState(false)
  const [rejecting, setRejecting] = useState(false)
  const [successMessage, setSuccessMessage] = useState('')

  const fetchChanges = useCallback(async () => {
    setLoading(true)
    setError('')
    try {
      const statusParam = filter === 'all' ? '' : `?status=${filter}`
      const data = await get<PendingChange[]>(`/work-items/pending-changes${statusParam}`)
      setChanges(data)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to load pending changes')
    } finally {
      setLoading(false)
    }
  }, [get, filter])

  useEffect(() => {
    fetchChanges()
  }, [fetchChanges])

  const pendingCount = changes.filter((c) => c.status === 'pending').length

  async function openDetail(id: string) {
    setDetailLoading(true)
    try {
      const detail = await get<PendingChangeDetail>(`/work-items/pending-change?id=${id}`)
      setDetailChange(detail)
    } catch {
      setError('Failed to load change details')
    } finally {
      setDetailLoading(false)
    }
  }

  async function handleApprove(id: string) {
    const change = changes.find((c) => c.id === id) || detailChange
    if (!change) return

    const confirmed = window.confirm(
      `This will create a PR from ${change.branch} into ${change.base_branch} on ${change.repo_name}. Continue?`,
    )
    if (!confirmed) return

    setApproving(true)
    setSuccessMessage('')
    try {
      const result = await post<ApproveResponse>('/work-items/approve-pr', { id })
      setSuccessMessage(`PR created: ${result.pr_url}`)
      setDetailChange(null)
      await fetchChanges()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to approve change')
    } finally {
      setApproving(false)
    }
  }

  async function handleReject(id: string) {
    const reason = window.prompt('Rejection reason (optional):')
    if (reason === null) return // user cancelled

    setRejecting(true)
    try {
      await post<{ status: string }>('/work-items/reject-pr', { id, reason })
      setDetailChange(null)
      await fetchChanges()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to reject change')
    } finally {
      setRejecting(false)
    }
  }

  // Filter changes for display (when filter is 'all', show everything)
  const displayedChanges = filter === 'all'
    ? changes
    : changes.filter((c) => c.status === filter)

  return (
    <div className="space-y-6">
      {/* Header */}
      <div className="flex items-center justify-between">
        <div>
          <h2 className="text-lg font-semibold text-fg">Pending Changes</h2>
          <p className="text-sm text-muted">
            Review and approve implementation changes before PR creation
          </p>
        </div>
      </div>

      {/* Success message */}
      {successMessage && (
        <div className="rounded-sm border border-emerald-500/30 bg-emerald-500/10 px-4 py-3">
          <div className="flex items-center justify-between">
            <p className="font-mono text-sm text-emerald-400">{successMessage}</p>
            <button
              onClick={() => setSuccessMessage('')}
              className="text-sm text-emerald-400 hover:text-emerald-300"
            >
              ✕
            </button>
          </div>
        </div>
      )}

      {/* Error message */}
      {error && (
        <div className="rounded-sm border border-red-500/30 bg-red-500/10 px-4 py-3">
          <div className="flex items-center justify-between">
            <p className="font-mono text-sm text-red-400">{error}</p>
            <button
              onClick={() => setError('')}
              className="text-sm text-red-400 hover:text-red-300"
            >
              ✕
            </button>
          </div>
        </div>
      )}

      {/* Status filter tabs */}
      <div className="flex gap-1 border-b border-rule">
        {(['all', 'pending', 'approved', 'rejected'] as const).map((tab) => (
          <button
            key={tab}
            onClick={() => setFilter(tab)}
            className={`px-4 py-2 text-sm font-medium transition-colors ${
              filter === tab
                ? 'border-b-2 border-accent text-accent'
                : 'text-muted hover:text-fg'
            }`}
          >
            {tab.charAt(0).toUpperCase() + tab.slice(1)}
            {tab === 'pending' && pendingCount > 0 && (
              <span className="ml-1.5 rounded-full bg-amber-500/20 px-1.5 py-0.5 text-xs text-amber-400">
                {pendingCount}
              </span>
            )}
          </button>
        ))}
      </div>

      {/* PR Gate Settings (collapsible) */}
      <PrGateSettings />

      {/* Change cards */}
      {loading ? (
        <div className="rounded-sm border border-rule bg-surface-950 p-8 text-center">
          <p className="text-sm text-muted">Loading changes...</p>
        </div>
      ) : displayedChanges.length === 0 ? (
        <div className="rounded-sm border border-rule bg-surface-950 p-8 text-center">
          <p className="text-sm text-muted">
            {filter === 'all'
              ? 'No changes found.'
              : `No ${filter} changes.`}
          </p>
        </div>
      ) : (
        <div className="space-y-3">
          {displayedChanges.map((change) => (
            <div
              key={change.id}
              className="rounded-sm border border-rule bg-surface-950 p-4"
            >
              <div className="flex items-start justify-between gap-4">
                {/* Left: info */}
                <div className="min-w-0 flex-1">
                  <div className="mb-1 flex items-center gap-2">
                    <button
                      onClick={() => openDetail(change.id)}
                      className="truncate text-sm font-medium text-accent hover:underline"
                    >
                      {change.work_item_title}
                    </button>
                    <StatusBadge status={change.status} />
                  </div>
                  <p className="mb-1 font-mono text-xs text-muted">
                    {change.repo_name}:{' '}
                    <span className="text-fg">{change.branch}</span> →{' '}
                    <span className="text-fg">{change.base_branch}</span>
                  </p>
                  <div className="flex items-center gap-4 text-xs text-dim">
                    <span>{change.files_changed.length} files changed</span>
                    {change.diff_stats && <span className="font-mono">{change.diff_stats}</span>}
                    <span>{formatDate(change.created_at)}</span>
                  </div>
                </div>

                {/* Right: actions */}
                {change.status === 'pending' && (
                  <div className="flex shrink-0 gap-2">
                    <button
                      onClick={() => openDetail(change.id)}
                      className="rounded-sm border border-rule px-3 py-1.5 text-xs font-medium text-muted transition-colors hover:bg-surface-800 hover:text-fg"
                    >
                      Review Changes
                    </button>
                    <button
                      onClick={() => handleApprove(change.id)}
                      disabled={approving}
                      className="rounded-sm bg-accent px-3 py-1.5 text-xs font-semibold text-surface-900 hover:bg-accent-hover disabled:opacity-50"
                    >
                      Approve & Create PR
                    </button>
                    <button
                      onClick={() => handleReject(change.id)}
                      disabled={rejecting}
                      className="rounded-sm bg-red-600 px-3 py-1.5 text-xs font-semibold text-white hover:bg-red-700 disabled:opacity-50"
                    >
                      Reject
                    </button>
                  </div>
                )}
              </div>
            </div>
          ))}
        </div>
      )}

      {/* Detail loading indicator */}
      {detailLoading && (
        <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/60">
          <div className="rounded-sm border border-rule bg-surface-950 px-6 py-4">
            <p className="text-sm text-muted">Loading details...</p>
          </div>
        </div>
      )}

      {/* Detail overlay */}
      {detailChange && (
        <DetailView
          change={detailChange}
          onClose={() => setDetailChange(null)}
          onApprove={handleApprove}
          onReject={handleReject}
          approving={approving}
          rejecting={rejecting}
        />
      )}
    </div>
  )
}
