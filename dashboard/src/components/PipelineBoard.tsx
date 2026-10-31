import { useEffect, useState, useCallback } from 'react'
import { Link } from 'react-router-dom'
import { useApi } from '@/hooks/useApi'
import { useWebSocket } from '@/hooks/useWebSocket'
import WorkItemCard from './WorkItemCard'
import type { WorkItem, WorkItemState, QueueCounts, Product } from '@/types'

interface Column {
  state: WorkItemState
  label: string
  colorClass: string
}

const COLUMNS: Column[] = [
  { state: 'intake', label: 'Intake', colorClass: 'text-accent-dim' },
  { state: 'prd', label: 'PRD', colorClass: 'text-violet-400' },
  { state: 'prd_approval', label: 'PRD Approval', colorClass: 'text-yellow-400' },
  { state: 'research', label: 'Research', colorClass: 'text-blue-400' },
  { state: 'planning', label: 'Planning', colorClass: 'text-cyan-400' },
  { state: 'implementation', label: 'Dev', colorClass: 'text-emerald-400' },
  { state: 'verification', label: 'Verifying', colorClass: 'text-indigo-400' },
  { state: 'review', label: 'Review', colorClass: 'text-amber-400' },
  { state: 'human_review', label: 'Human Review', colorClass: 'text-orange-400' },
  { state: 'qa', label: 'QA', colorClass: 'text-lime-400' },
  { state: 'passed', label: 'Release Ready', colorClass: 'text-teal-400' },
  { state: 'awaiting_user', label: 'Needs Input', colorClass: 'text-yellow-300' },
  { state: 'blocked', label: 'Blocked', colorClass: 'text-red-400' },
  { state: 'failed', label: 'Failed', colorClass: 'text-red-500' },
]

export default function PipelineBoard() {
  const { get } = useApi()
  const { lastMessage, subscribe, unsubscribe } = useWebSocket()
  const [items, setItems] = useState<WorkItem[]>([])
  const [counts, setCounts] = useState<QueueCounts>({})
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [products, setProducts] = useState<Product[]>([])
  const [selectedProductId, setSelectedProductId] = useState<string>('')
  // null = unknown (check failed); true = configured; false = not configured.
  const [llmConfigured, setLlmConfigured] = useState<boolean | null>(true)

  // Fetch products for filter dropdown
  useEffect(() => {
    get<Product[]>('/admin/products')
      .then(data => setProducts(data))
      .catch((err) => {
        console.error('Failed to fetch:', err)
        setError(err instanceof Error ? err.message : 'Failed to load products')
      })
  }, [get])

  // Check whether an LLM provider has been configured via the credential registry.
  // If no credentials exist for the "llm" category, show the setup banner.
  // On fetch error (401/403/500/network), surface the unknown state rather
  // than silently pretending success — a brand-new operator hitting a
  // backend quirk must be told, not hidden from.
  useEffect(() => {
    get<unknown[]>('/admin/credentials?category=llm')
      .then(data => setLlmConfigured(Array.isArray(data) && data.length > 0))
      .catch((e) => {
        console.warn('PipelineBoard: llm-configured check failed:', e)
        setLlmConfigured(null)
      })
  }, [get])

  const fetchItems = useCallback(async () => {
    try {
      setError(null)
      const data = await get<WorkItem[]>('/work-items')
      setItems(data)
      // derive counts
      const c: QueueCounts = {}
      data.forEach((item) => {
        c[item.state] = (c[item.state] ?? 0) + 1
      })
      setCounts(c)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to load work items')
    } finally {
      setLoading(false)
    }
  }, [get])

  useEffect(() => {
    fetchItems()
  }, [fetchItems])

  useEffect(() => {
    subscribe('pipeline')
    return () => unsubscribe('pipeline')
  }, [subscribe, unsubscribe])

  useEffect(() => {
    if (!lastMessage) return
    if (lastMessage.channel !== 'pipeline') return
    // Re-fetch on pipeline events
    fetchItems()
  }, [lastMessage, fetchItems])

  // Only show top-level items (feature, bug) on the pipeline board.
  // Subtasks are internal decomposition — they're visible in the parent's detail page.
  const topLevelItems = items.filter(i => i.type !== 'subtask' && i.type !== 'research')
  const filteredItems = selectedProductId
    ? topLevelItems.filter(i => i.product_id === selectedProductId)
    : topLevelItems

  const grouped = COLUMNS.reduce<Record<WorkItemState, WorkItem[]>>(
    (acc, col) => {
      acc[col.state] = filteredItems.filter((i) => i.state === col.state)
      return acc
    },
    {} as Record<WorkItemState, WorkItem[]>,
  )

  const totalActive = filteredItems.filter(
    (i) => !['released', 'rejected', 'cancelled'].includes(i.state),
  ).length

  if (loading) {
    return (
      <div className="flex h-64 items-center justify-center">
        <div className="h-8 w-8 animate-spin rounded-full border-2 border-accent border-t-transparent" />
      </div>
    )
  }

  return (
    <div className="flex flex-col gap-4">
      {/* Setup Required banner — shown when no LLM provider has been configured */}
      {llmConfigured === false && (
        <div className="flex flex-col gap-3 rounded-sm border border-yellow-600/50 bg-yellow-900/20 px-4 py-3 sm:flex-row sm:items-center sm:justify-between">
          <div className="flex items-start gap-2">
            <span className="mt-0.5 text-yellow-400" aria-hidden="true">&#9888;</span>
            <div>
              <p className="text-sm font-semibold text-yellow-300">No LLM provider configured</p>
              <p className="text-xs text-yellow-400/80 mt-0.5">
                Work items will be blocked until a model provider is added. Go to Admin &rarr; Models to configure one, or use the Onboarding Wizard for guided setup.
              </p>
            </div>
          </div>
          <div className="flex shrink-0 gap-2 pl-6 sm:pl-0">
            <Link
              to="/admin/providers"
              className="rounded bg-yellow-600 px-3 py-1.5 text-xs font-medium text-white hover:bg-yellow-500 transition-colors"
            >
              Configure Providers
            </Link>
            <Link
              to="/onboarding"
              className="rounded-sm border border-yellow-600/60 px-3 py-1.5 text-xs font-medium text-yellow-300 hover:bg-yellow-900/40 transition-colors"
            >
              Start Onboarding
            </Link>
          </div>
        </div>
      )}

      {/* Unknown-state banner — shown when the LLM-configured check itself failed
          (e.g. 401/403/500 from /admin/credentials). This used to be silently
          collapsed into the all-good state, hiding real backend problems. */}
      {llmConfigured === null && (
        <div className="flex items-start gap-2 rounded-sm border border-orange-600/50 bg-orange-900/20 px-4 py-3">
          <span className="mt-0.5 text-orange-400" aria-hidden="true">&#9888;</span>
          <div>
            <p className="text-sm font-semibold text-orange-300">LLM status unknown</p>
            <p className="text-xs text-orange-400/80 mt-0.5">
              Could not check whether an LLM provider is configured. This usually means the dashboard backend is unreachable or you are not signed in. Check backend connectivity and sign-in state.
            </p>
          </div>
        </div>
      )}

      {/* Header */}
      <div className="flex items-center justify-between gap-4">
        <div>
          <h2 className="text-lg font-semibold text-fg">Pipeline Board</h2>
          <p className="text-sm text-dim">
            {totalActive} active work item{totalActive !== 1 ? 's' : ''}
            {selectedProductId && products.length > 0 && (
              <span className="ml-1 text-accent-dim">
                — {products.find(p => p.id === selectedProductId)?.name}
              </span>
            )}
          </p>
        </div>
        <div className="flex items-center gap-3">
          {products.length > 0 && (
            <select
              value={selectedProductId}
              onChange={e => setSelectedProductId(e.target.value)}
              className="rounded-sm border border-rule bg-surface-950 px-3 py-1.5 text-sm text-fg focus:outline-none focus:ring-1 focus:ring-accent"
            >
              <option value="">All Products</option>
              {products.map(p => (
                <option key={p.id} value={p.id}>{p.name}</option>
              ))}
            </select>
          )}
          {error && (
            <span className="rounded bg-red-500/20 px-3 py-1 text-xs text-red-300">
              {error}
            </span>
          )}
        </div>
      </div>

      {/* Kanban columns */}
      <div className="flex gap-3 overflow-x-auto pb-4">
        {COLUMNS.map((col) => {
          const colItems = grouped[col.state] ?? []
          const count = counts[col.state] ?? 0
          return (
            <div
              key={col.state}
              className="flex w-64 shrink-0 flex-col rounded-sm border border-rule bg-surface-850 p-3"
            >
              {/* Column header */}
              <div className="mb-3 flex items-center justify-between">
                <span className={['text-xs font-semibold uppercase tracking-wider', col.colorClass].join(' ')}>
                  {col.label}
                </span>
                <span className="flex h-5 min-w-5 items-center justify-center rounded-full bg-surface-700 px-1.5 text-xs font-medium text-muted">
                  {count}
                </span>
              </div>

              {/* Cards */}
              <div className="flex flex-col gap-2">
                {colItems.length === 0 ? (
                  <p className="py-4 text-center text-xs text-dim">
                    Empty
                  </p>
                ) : (
                  colItems.map((item) => (
                    <WorkItemCard
                      key={item.id}
                      item={item}
                      productName={
                        !selectedProductId && item.product_id
                          ? products.find(p => p.id === item.product_id)?.name
                          : undefined
                      }
                      onActionComplete={fetchItems}
                    />
                  ))
                )}
              </div>
            </div>
          )
        })}
      </div>
    </div>
  )
}
