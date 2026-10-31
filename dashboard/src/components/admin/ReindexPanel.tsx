import { useState, useEffect, useCallback, useRef } from 'react'
import { useApi } from '@/hooks/useApi'
import type { Product } from '@/types'

// ─── Types ────────────────────────────────────────────────────────────────────

interface ProductBranches {
  default_branch: string
  indexed_branches: string[]
  repo_branches_seen: string[]
}

interface ReindexStatusResponse {
  product_id: string
  branch: string
  status: 'idle' | 'running' | 'failed' | 'completed'
  module_count: number
  last_indexed_at?: string
  last_indexed_commit?: string
  indexer_phase?: string
  indexer_error?: string
  job_id?: string
  started_at?: string
}

interface ReindexStartResponse {
  job_id: string
  status: string
  product_id: string
  branch: string
  started_at: string
}

// ─── Constants ────────────────────────────────────────────────────────────────

const CUSTOM_BRANCH_SENTINEL = '__custom__'
const POLL_INTERVAL_MS = 3000
const POLL_TIMEOUT_MS = 5 * 60 * 1000 // 5 minutes

// ─── Helpers ──────────────────────────────────────────────────────────────────

function formatTime(iso?: string): string {
  if (!iso) return '—'
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? '—' : d.toLocaleString()
}

function statusColor(status: string): string {
  switch (status) {
    case 'running':
      return 'bg-blue-800/40 text-blue-300 border-blue-700/40'
    case 'completed':
      return 'bg-emerald-800/40 text-emerald-300 border-emerald-700/40'
    case 'failed':
      return 'bg-red-800/40 text-red-300 border-red-700/40'
    default:
      return 'bg-surface-800/40 text-muted border-rule/40'
  }
}

// ─── ReindexPanel ─────────────────────────────────────────────────────────────

export default function ReindexPanel() {
  const { get, post } = useApi()

  const [products, setProducts] = useState<Product[]>([])
  const [selectedProductID, setSelectedProductID] = useState('')
  const [branches, setBranches] = useState<ProductBranches | null>(null)
  const [branchSelection, setBranchSelection] = useState('')
  const [customBranch, setCustomBranch] = useState('')
  const [status, setStatus] = useState<ReindexStatusResponse | null>(null)
  const [loadError, setLoadError] = useState('')
  const [actionError, setActionError] = useState('')
  const [starting, setStarting] = useState(false)

  // Polling control. Clearing the interval whenever the ref changes keeps us
  // from leaking timers across product/branch switches.
  const pollRef = useRef<ReturnType<typeof setInterval> | null>(null)
  const pollStartedAtRef = useRef<number>(0)

  // Resolve the branch the user has actually chosen — "custom" reveals the
  // text input, otherwise we use the dropdown value.
  const activeBranch = branchSelection === CUSTOM_BRANCH_SENTINEL
    ? customBranch.trim()
    : branchSelection.trim()

  // ── Load products on mount ──────────────────────────────────────────────────
  useEffect(() => {
    let cancelled = false
    ;(async () => {
      try {
        const data = await get<Product[]>('/admin/products')
        if (!cancelled) {
          setProducts(data ?? [])
        }
      } catch (e: unknown) {
        if (!cancelled) {
          setLoadError(e instanceof Error ? e.message : 'Failed to load products')
        }
      }
    })()
    return () => {
      cancelled = true
    }
  }, [get])

  // ── Load branches when product changes ──────────────────────────────────────
  useEffect(() => {
    if (!selectedProductID) {
      setBranches(null)
      setBranchSelection('')
      setStatus(null)
      return
    }
    let cancelled = false
    ;(async () => {
      try {
        const data = await get<ProductBranches>(
          `/admin/products/branches?product_id=${encodeURIComponent(selectedProductID)}`,
        )
        if (cancelled) return
        setBranches(data)
        // Preselect the default branch — the 80% case.
        setBranchSelection(data.default_branch || '')
      } catch (e: unknown) {
        if (!cancelled) {
          setLoadError(e instanceof Error ? e.message : 'Failed to load branches')
        }
      }
    })()
    return () => {
      cancelled = true
    }
  }, [selectedProductID, get])

  // ── Fetch current status whenever product+branch pair changes ──────────────
  const fetchStatus = useCallback(async (): Promise<ReindexStatusResponse | null> => {
    if (!selectedProductID || !activeBranch) return null
    try {
      const res = await get<ReindexStatusResponse>(
        `/admin/reindex/status?product_id=${encodeURIComponent(selectedProductID)}&branch=${encodeURIComponent(activeBranch)}`,
      )
      setStatus(res)
      return res
    } catch (e: unknown) {
      setActionError(e instanceof Error ? e.message : 'Status fetch failed')
      return null
    }
  }, [get, selectedProductID, activeBranch])

  useEffect(() => {
    fetchStatus()
  }, [fetchStatus])

  // ── Polling ─────────────────────────────────────────────────────────────────
  const stopPolling = useCallback(() => {
    if (pollRef.current) {
      clearInterval(pollRef.current)
      pollRef.current = null
    }
  }, [])

  const startPolling = useCallback(() => {
    stopPolling()
    pollStartedAtRef.current = Date.now()
    pollRef.current = setInterval(async () => {
      // Bail after the timeout regardless of server state.
      if (Date.now() - pollStartedAtRef.current > POLL_TIMEOUT_MS) {
        stopPolling()
        setActionError('Polling timed out after 5 minutes — check knowledge-indexer logs.')
        return
      }
      const s = await fetchStatus()
      if (s && s.status !== 'running') {
        stopPolling()
      }
    }, POLL_INTERVAL_MS)
  }, [fetchStatus, stopPolling])

  // Clean up timer on unmount.
  useEffect(() => () => stopPolling(), [stopPolling])

  // If the initial status snapshot already says running (e.g. the user
  // refreshed mid-run) start polling so the panel stays live.
  useEffect(() => {
    if (status?.status === 'running' && !pollRef.current) {
      startPolling()
    }
  }, [status?.status, startPolling])

  // ── Trigger a reindex ───────────────────────────────────────────────────────
  async function handleReindex() {
    if (!selectedProductID || !activeBranch) return
    setActionError('')
    setStarting(true)
    try {
      const res = await post<ReindexStartResponse>('/admin/reindex', {
        product_id: selectedProductID,
        branch: activeBranch,
      })
      // Optimistic update: show "running" immediately rather than waiting
      // for the first poll tick.
      setStatus(prev => ({
        product_id: selectedProductID,
        branch: activeBranch,
        status: 'running',
        module_count: prev?.module_count ?? 0,
        last_indexed_at: prev?.last_indexed_at,
        last_indexed_commit: prev?.last_indexed_commit,
        job_id: res?.job_id,
        started_at: res?.started_at,
      }))
      startPolling()
    } catch (e: unknown) {
      setActionError(e instanceof Error ? e.message : 'Failed to start reindex')
    } finally {
      setStarting(false)
    }
  }

  const isRunning = status?.status === 'running'
  const canTrigger = Boolean(selectedProductID) && Boolean(activeBranch) && !isRunning && !starting

  // ── Render ─────────────────────────────────────────────────────────────────
  return (
    <div className="space-y-6 p-6 text-fg">
      <div>
        <h2 className="text-lg font-semibold text-fg">Reindex</h2>
        <p className="mt-1 text-sm text-muted">
          Manually trigger a codex + Qdrant re-index for a specific product and branch.
          The indexer checks out the requested branch in each cloned repository before walking.
        </p>
      </div>

      {loadError && (
        <div className="rounded-sm border border-red-700/40 bg-red-900/30 px-3 py-2 text-sm text-red-300">
          {loadError}
        </div>
      )}

      {/* ── Selectors ─────────────────────────────────────────────────────── */}
      <div className="grid gap-4 md:grid-cols-2">
        <div>
          <label className="mb-1 block text-xs uppercase tracking-wide text-muted">
            Product
          </label>
          <select
            value={selectedProductID}
            onChange={e => setSelectedProductID(e.target.value)}
            className="w-full rounded-sm border border-rule bg-surface-950 px-3 py-2 text-sm text-fg focus:outline-none focus:ring-1 focus:ring-accent"
          >
            <option value="">— select a product —</option>
            {products.map(p => (
              <option key={p.id} value={p.id}>
                {p.name}
              </option>
            ))}
          </select>
        </div>

        <div>
          <label className="mb-1 block text-xs uppercase tracking-wide text-muted">
            Branch
          </label>
          <select
            value={branchSelection}
            onChange={e => setBranchSelection(e.target.value)}
            disabled={!selectedProductID || !branches}
            className="w-full rounded-sm border border-rule bg-surface-950 px-3 py-2 text-sm text-fg focus:outline-none focus:ring-1 focus:ring-accent disabled:opacity-50"
          >
            {!branches && <option value="">—</option>}
            {branches && (
              <>
                <option value={branches.default_branch}>
                  {branches.default_branch} (default)
                </option>
                {branches.indexed_branches
                  .filter(b => b !== branches.default_branch)
                  .map(b => (
                    <option key={b} value={b}>
                      {b}
                    </option>
                  ))}
                <option value={CUSTOM_BRANCH_SENTINEL}>custom…</option>
              </>
            )}
          </select>
          {branchSelection === CUSTOM_BRANCH_SENTINEL && (
            <input
              type="text"
              value={customBranch}
              onChange={e => setCustomBranch(e.target.value)}
              placeholder="feature/my-branch"
              className="mt-2 w-full rounded-sm border border-rule bg-surface-950 px-3 py-2 text-sm text-fg focus:outline-none focus:ring-1 focus:ring-accent"
            />
          )}
        </div>
      </div>

      {/* ── Action ────────────────────────────────────────────────────────── */}
      <div className="flex items-center gap-3">
        <button
          onClick={handleReindex}
          disabled={!canTrigger}
          className="rounded bg-accent px-4 py-2 text-sm font-medium text-surface-900 transition-colors hover:bg-accent-hover disabled:cursor-not-allowed disabled:opacity-50"
        >
          {starting ? 'Starting…' : isRunning ? 'Running…' : 'Reindex now'}
        </button>
        {isRunning && (
          <button
            onClick={() => fetchStatus()}
            className="rounded-sm border border-rule px-3 py-2 text-sm text-muted hover:bg-surface-800"
          >
            Refresh
          </button>
        )}
      </div>

      {actionError && (
        <div className="rounded-sm border border-red-700/40 bg-red-900/30 px-3 py-2 text-sm text-red-300">
          {actionError}
        </div>
      )}

      {/* ── Status panel ──────────────────────────────────────────────────── */}
      {status && (
        <div className="rounded-sm border border-rule bg-surface-950 p-4">
          <div className="mb-3 flex items-center justify-between">
            <h3 className="text-sm font-semibold text-fg">
              Status for {status.branch}
            </h3>
            <span
              className={`rounded-sm border px-2 py-0.5 text-xs font-medium ${statusColor(status.status)}`}
            >
              {status.status}
            </span>
          </div>

          <dl className="grid grid-cols-2 gap-x-6 gap-y-2 text-sm">
            <div>
              <dt className="text-xs uppercase tracking-wide text-dim">Last indexed</dt>
              <dd className="text-fg">{formatTime(status.last_indexed_at)}</dd>
            </div>
            <div>
              <dt className="text-xs uppercase tracking-wide text-dim">Last commit</dt>
              <dd className="font-mono text-xs text-muted">
                {status.last_indexed_commit
                  ? status.last_indexed_commit.substring(0, 12)
                  : '—'}
              </dd>
            </div>
            <div>
              <dt className="text-xs uppercase tracking-wide text-dim">Modules</dt>
              <dd className="text-fg">{status.module_count}</dd>
            </div>
            <div>
              <dt className="text-xs uppercase tracking-wide text-dim">Job ID</dt>
              <dd className="font-mono text-xs text-muted">
                {status.job_id ? status.job_id.substring(0, 8) : '—'}
              </dd>
            </div>
            {status.indexer_phase && (
              <div className="col-span-2">
                <dt className="text-xs uppercase tracking-wide text-dim">Indexer phase</dt>
                <dd className="text-fg">{status.indexer_phase}</dd>
              </div>
            )}
            {status.indexer_error && (
              <div className="col-span-2">
                <dt className="text-xs uppercase tracking-wide text-red-400">Error</dt>
                <dd className="text-red-300">{status.indexer_error}</dd>
              </div>
            )}
          </dl>
        </div>
      )}
    </div>
  )
}
