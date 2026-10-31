import { useCallback, useEffect, useMemo, useState } from 'react'
import { useApi } from '@/hooks/useApi'
import type { GraphNodeData, GraphResponse, RiskLevel, Repository, CodexSection, CodexPattern } from '@/types'
import { CodexGraph, type ViewMode } from './CodexGraph'
import { NodeDetailPanel } from './NodeDetailPanel'
import { GraphFilters, type FilterState } from './GraphFilters'
import { GraphToolbar } from './GraphToolbar'
import { ProductSelector, useProductSelection } from './ProductSelector'
import { ALL_RISKS } from './palette'
import CodexBrowser from '@/components/CodexBrowser'
import ContractEdgesTab from '@/components/codex/ContractEdgesTab'
import SnapshotHistory from '@/components/codex/SnapshotHistory'
import GitStrategyCard from '@/components/codex/GitStrategyCard'

type Tab = 'graph' | 'list' | 'contracts' | 'history'

const DEFAULT_FILTERS: FilterState = {
  risks: new Set<RiskLevel>(ALL_RISKS),
  search: '',
  hideExternal: false,
}

export default function CodexGraphPage() {
  const { get } = useApi()
  const [productId, setProductId] = useProductSelection()
  const [tab, setTab] = useState<Tab>('graph')
  const [viewMode, setViewMode] = useState<ViewMode>('force')
  const [data, setData] = useState<GraphResponse | null>(null)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [selectedNodeId, setSelectedNodeId] = useState<string | null>(null)
  const [filters, setFilters] = useState<FilterState>(DEFAULT_FILTERS)
  const [refetchToken, setRefetchToken] = useState(0)
  const [sections, setSections] = useState<CodexSection[]>([])
  const [patterns, setPatterns] = useState<CodexPattern[]>([])

  const fetchGraph = useCallback(async () => {
    if (!productId) {
      setData(null)
      setSections([])
      setPatterns([])
      return
    }
    setLoading(true)
    setError(null)
    try {
      const [g, sec, pat] = await Promise.all([
        get<GraphResponse>(`/codex/graph?product_id=${encodeURIComponent(productId)}`),
        get<CodexSection[]>(`/codex/sections?product_id=${encodeURIComponent(productId)}`).catch(() => [] as CodexSection[]),
        get<CodexPattern[]>(`/codex/patterns?product_id=${encodeURIComponent(productId)}`).catch(() => [] as CodexPattern[]),
      ])
      setData(g)
      setSections(sec)
      setPatterns(pat)
      setSelectedNodeId(null)
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : 'Failed to load graph')
      setData(null)
    } finally {
      setLoading(false)
    }
  }, [productId, get])

  useEffect(() => {
    fetchGraph()
  }, [fetchGraph, refetchToken])

  const selectedNode: GraphNodeData | null = useMemo(() => {
    if (!data || !selectedNodeId) return null
    return data.nodes.find((n) => n.id === selectedNodeId) ?? null
  }, [data, selectedNodeId])

  // Cross-reference: which deep-analysis sections mention each module name?
  const deepSections = useMemo(
    () => sections.filter((s) => {
      const c = s.content as Record<string, unknown> | null
      return c && typeof c === 'object' && c.source === 'deep_analysis'
    }),
    [sections],
  )

  const moduleInsightMap = useMemo(() => {
    const map = new Map<string, CodexSection[]>()
    if (!data) return map
    for (const node of data.nodes) {
      if (node.external) continue
      const moduleName = node.name
      const related = deepSections.filter((s) => {
        const c = s.content as Record<string, string>
        const text = c?.analysis ?? ''
        return text.toLowerCase().includes(moduleName.toLowerCase())
      })
      if (related.length > 0) map.set(node.id, related)
    }
    return map
  }, [data, deepSections])

  // Cross-reference: which patterns reference files in each module's directory?
  const modulePatternMap = useMemo(() => {
    const map = new Map<string, CodexPattern[]>()
    if (!data) return map
    for (const node of data.nodes) {
      if (node.external) continue
      const prefix = node.name + '/'
      const related = patterns.filter((p) =>
        p.reference_files?.some((f) => f.startsWith(prefix) || f.startsWith(node.name)),
      )
      if (related.length > 0) map.set(node.id, related)
    }
    return map
  }, [data, patterns])

  const handleFitView = useCallback(() => {
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 'f' }))
  }, [])

  return (
    <div className="flex h-[calc(100vh-var(--topbar,56px))] flex-col gap-3 p-4">
      {/* Header row */}
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h2 className="font-display text-lg text-fg">Codex Browser</h2>
          {data && (
            <p className="font-mono text-xs text-muted">
              {data.product_name} · generated {new Date(data.generated_at).toLocaleString()}
            </p>
          )}
        </div>
        <ProductSelector value={productId} onChange={setProductId} />
      </div>

      {/* Tabs */}
      <div className="flex gap-1 border-b border-rule">
        {([
          { id: 'graph' as Tab, label: 'Graph' },
          { id: 'list' as Tab, label: 'List' },
          { id: 'contracts' as Tab, label: 'Contracts' },
          { id: 'history' as Tab, label: 'History' },
        ]).map((t) => (
          <button
            key={t.id}
            type="button"
            onClick={() => setTab(t.id)}
            className={[
              'px-4 py-2 font-mono text-sm transition-colors -mb-px border-b-2',
              tab === t.id
                ? 'border-accent text-accent'
                : 'border-transparent text-muted hover:text-fg',
            ].join(' ')}
          >
            {t.label}
          </button>
        ))}
      </div>

      {tab === 'graph' && (
        <>
          <div className="flex flex-wrap items-center justify-between gap-3">
            <GraphFilters value={filters} onChange={setFilters} meta={data?.meta} />
            <GraphToolbar
              viewMode={viewMode}
              onViewModeChange={setViewMode}
              onFitView={handleFitView}
              onRefresh={() => setRefetchToken((t) => t + 1)}
            />
          </div>

          <div className="relative flex flex-1 overflow-hidden rounded-sm border border-rule bg-surface-900">
            {!productId ? (
              <div className="flex h-full w-full items-center justify-center">
                <p className="font-mono text-sm text-muted">
                  Pick a product above to see its codex graph.
                </p>
              </div>
            ) : (
              <CodexGraph
                data={data}
                loading={loading}
                error={error}
                onRetry={() => setRefetchToken((t) => t + 1)}
                onNodeSelect={(n) => setSelectedNodeId(n?.id ?? null)}
                selectedNodeId={selectedNodeId}
                viewMode={viewMode}
                risks={filters.risks}
                search={filters.search}
                hideExternal={filters.hideExternal}
                insightCounts={moduleInsightMap}
                readOnly
              />
            )}
            {selectedNode && (
              <NodeDetailPanel
                node={selectedNode}
                onClose={() => setSelectedNodeId(null)}
                relatedInsights={moduleInsightMap.get(selectedNodeId!) ?? []}
                relatedPatterns={modulePatternMap.get(selectedNodeId!) ?? []}
                allModuleNames={data?.nodes.filter((n) => !n.external).map((n) => n.name) ?? []}
                onNavigateModule={(name) => {
                  const target = data?.nodes.find((n) => n.name === name)
                  if (target) setSelectedNodeId(target.id)
                }}
              />
            )}
          </div>
        </>
      )}

      {tab === 'list' && (
        <div className="flex-1 overflow-auto">
          <CodexBrowser productId={productId} />
        </div>
      )}

      {tab === 'contracts' && (
        <div className="flex-1 overflow-auto">
          {productId ? (
            <ContractEdgesTab productId={productId} />
          ) : (
            <div className="flex h-48 items-center justify-center rounded-sm border border-rule bg-surface-800">
              <p className="font-mono text-sm text-muted">Pick a product above to see contract edges.</p>
            </div>
          )}
        </div>
      )}

      {tab === 'history' && (
        <div className="flex-1 overflow-auto">
          <HistoryTabContent productId={productId} />
        </div>
      )}
    </div>
  )
}

// ── History tab with repo selector ──────────────────────────────────────────

function HistoryTabContent({ productId }: { productId: string | null }) {
  const { get } = useApi()
  const [repos, setRepos] = useState<Repository[]>([])
  const [selectedRepoId, setSelectedRepoId] = useState<string | null>(null)
  const [loadingRepos, setLoadingRepos] = useState(false)

  useEffect(() => {
    if (!productId) {
      setRepos([])
      setSelectedRepoId(null)
      return
    }
    setLoadingRepos(true)
    get<Repository[]>(`/admin/repositories?product_id=${productId}`)
      .then((data) => {
        setRepos(data)
        setSelectedRepoId(data.length > 0 ? data[0].id : null)
      })
      .catch(() => setRepos([]))
      .finally(() => setLoadingRepos(false))
  }, [productId, get])

  if (!productId) {
    return (
      <div className="flex h-48 items-center justify-center rounded-sm border border-rule bg-surface-800">
        <p className="font-mono text-sm text-muted">Pick a product above to see snapshot history.</p>
      </div>
    )
  }

  if (loadingRepos) {
    return (
      <div className="flex h-32 items-center justify-center">
        <div className="h-6 w-6 animate-spin rounded-full border-2 border-accent border-t-transparent" />
      </div>
    )
  }

  return (
    <div className="flex flex-col gap-4">
      <div className="flex items-center gap-3">
        <label className="font-mono text-xs text-muted">Repository</label>
        <select
          value={selectedRepoId ?? ''}
          onChange={(e) => setSelectedRepoId(e.target.value || null)}
          className="rounded-sm border border-rule bg-surface-950 px-3 py-1.5 font-mono text-sm text-fg focus:outline-none focus:border-accent"
        >
          <option value="">Select a repository…</option>
          {repos.map((r) => (
            <option key={r.id} value={r.id}>{r.name}</option>
          ))}
        </select>
      </div>

      {selectedRepoId ? (
        <div className="flex flex-col gap-4">
          <GitStrategyCard repoId={selectedRepoId} />
          <SnapshotHistory repoId={selectedRepoId} />
        </div>
      ) : (
        <div className="flex h-48 items-center justify-center rounded-sm border border-rule bg-surface-800">
          <p className="font-mono text-sm text-muted">Select a repository to view its snapshot history.</p>
        </div>
      )}
    </div>
  )
}
