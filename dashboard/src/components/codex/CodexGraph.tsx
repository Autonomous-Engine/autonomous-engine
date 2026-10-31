import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import ReactFlow, {
  Background,
  Controls,
  MiniMap,
  ReactFlowProvider,
  useReactFlow,
  type Edge,
  type Node,
  type NodeMouseHandler,
  type NodeTypes,
  type OnNodesChange,
  applyNodeChanges,
} from 'reactflow'
import 'reactflow/dist/style.css'
import type { GraphEdgeData, GraphNodeData, GraphResponse, RiskLevel, CodexSection } from '@/types'
import { ModuleNode } from './ModuleNode'
import { ExternalNode } from './ExternalNode'
import { forceSeed, layeredLayout } from './layout'

export type ViewMode = 'force' | 'layered'

export interface CodexGraphProps {
  /** Raw response from /api/codex/graph — the component does NOT fetch. */
  data: GraphResponse | null
  /** Called when the user clicks/activates a node (real or external). */
  onNodeSelect?: (node: GraphNodeData | null) => void
  /** Highlighted node id (controlled by parent). */
  selectedNodeId?: string | null
  /** If true, dragging and pan are still allowed but node data cannot be edited. */
  readOnly?: boolean
  /** Layout mode: force-directed (default, locked decision 1) or layered. */
  viewMode?: ViewMode
  /** Hide external:* nodes. Default: show (locked decision 3). */
  hideExternal?: boolean
  /** Show only nodes whose risk_level is in this set. Empty = all. */
  risks?: Set<RiskLevel>
  /** Substring match against node name + purpose. Empty = all. */
  search?: string
  /** Optional loading flag — renders dim overlay + spinner. */
  loading?: boolean
  /** Optional error — renders inline red box with Retry hook. */
  error?: string | null
  /** Called when the user clicks Retry in the error box. */
  onRetry?: () => void
  /** Called when user hits "fit to screen" (also wired to `f` key). */
  onFitView?: () => void
  /** Map of node ID → related deep analysis sections (for insight badges). */
  insightCounts?: Map<string, CodexSection[]>
}

const nodeTypes: NodeTypes = {
  module: ModuleNode,
  external: ExternalNode,
}

function toReactFlowNodes(data: GraphResponse, insightCounts?: Map<string, CodexSection[]>): Node<GraphNodeData>[] {
  return data.nodes.map((n) => ({
    id: n.id,
    type: n.external ? 'external' : 'module',
    position: { x: 0, y: 0 },
    data: {
      ...n,
      insight_count: insightCounts?.get(n.id)?.length ?? 0,
    },
  }))
}

function toReactFlowEdges(data: GraphResponse): Edge<GraphEdgeData>[] {
  return data.edges.map((e) => ({
    id: e.id,
    source: e.source,
    target: e.target,
    type: 'default',
    animated: false,
    style: {
      stroke: e.resolved ? '#475569' : '#475569',
      strokeOpacity: e.resolved ? 0.7 : 0.3,
      strokeDasharray: e.resolved ? undefined : '4 3',
    },
    data: e,
  }))
}

function filterNodes(
  nodes: Node<GraphNodeData>[],
  edges: Edge<GraphEdgeData>[],
  risks: Set<RiskLevel> | undefined,
  search: string | undefined,
  hideExternal: boolean | undefined,
): { nodes: Node<GraphNodeData>[]; edges: Edge<GraphEdgeData>[] } {
  const q = (search ?? '').trim().toLowerCase()
  const filtered = nodes.filter((n) => {
    const d = n.data
    if (hideExternal && d.external) return false
    if (!d.external && risks && risks.size > 0 && !risks.has(d.risk_level)) return false
    if (q) {
      const hay = (d.name + ' ' + d.purpose).toLowerCase()
      if (!hay.includes(q)) return false
    }
    return true
  })
  const keep = new Set(filtered.map((n) => n.id))
  const eOut = edges.filter((e) => keep.has(e.source) && keep.has(e.target))
  return { nodes: filtered, edges: eOut }
}

// ─── Inner component (needs to sit inside ReactFlowProvider) ────────────────

function CodexGraphInner({
  data,
  onNodeSelect,
  selectedNodeId,
  viewMode = 'force',
  hideExternal,
  risks,
  search,
  loading,
  error,
  onRetry,
  onFitView,
  insightCounts,
}: CodexGraphProps) {
  const rf = useReactFlow()
  const [nodes, setNodes] = useState<Node<GraphNodeData>[]>([])
  const [edges, setEdges] = useState<Edge<GraphEdgeData>[]>([])

  // Re-seed positions whenever the data or layout mode changes.
  useEffect(() => {
    if (!data) {
      setNodes([])
      setEdges([])
      return
    }
    const baseNodes = toReactFlowNodes(data, insightCounts)
    const baseEdges = toReactFlowEdges(data)
    const laidOut = viewMode === 'layered'
      ? layeredLayout(baseNodes, baseEdges)
      : forceSeed(baseNodes)
    setNodes(laidOut)
    setEdges(baseEdges)
    // Fit view on first render of fresh data.
    requestAnimationFrame(() => rf.fitView({ padding: 0.15, duration: 400 }))
  }, [data, viewMode, rf, insightCounts])

  // Filter pass on top of layout — keeps positions stable when only the
  // filter set changes.
  const visible = useMemo(
    () => filterNodes(nodes, edges, risks, search, hideExternal),
    [nodes, edges, risks, search, hideExternal],
  )

  // Highlight the selected node.
  const displayedNodes = useMemo(
    () =>
      visible.nodes.map((n) =>
        n.id === selectedNodeId ? { ...n, selected: true } : { ...n, selected: false },
      ),
    [visible.nodes, selectedNodeId],
  )

  const onNodesChange: OnNodesChange = useCallback(
    (changes) => setNodes((ns) => applyNodeChanges(changes, ns)),
    [],
  )

  const handleNodeClick: NodeMouseHandler = useCallback(
    (_, node) => {
      onNodeSelect?.(node.data as GraphNodeData)
    },
    [onNodeSelect],
  )

  const handlePaneClick = useCallback(() => onNodeSelect?.(null), [onNodeSelect])

  const [hoverId, setHoverId] = useState<string | null>(null)

  const onNodeMouseEnter: NodeMouseHandler = useCallback((_, node) => setHoverId(node.id), [])
  const onNodeMouseLeave: NodeMouseHandler = useCallback(() => setHoverId(null), [])

  const onNodeDoubleClick: NodeMouseHandler = useCallback(
    (_, node) => {
      rf.setCenter(node.position.x, node.position.y, { zoom: 1.5, duration: 400 })
    },
    [rf],
  )

  // Dim edges not touching the hovered node (only when there IS a hover).
  // Performance guard: skip dimming when there are > 200 nodes (spec §6).
  const styledEdges = useMemo(() => {
    if (!hoverId || visible.nodes.length > 200) return visible.edges
    return visible.edges.map((e) => {
      const connected = e.source === hoverId || e.target === hoverId
      return {
        ...e,
        style: { ...e.style, strokeOpacity: connected ? 1 : 0.15 },
      }
    })
  }, [hoverId, visible.edges, visible.nodes.length])

  // Keyboard: `f` fits view; Tab cycles focus (native); Esc clears selection.
  useEffect(() => {
    const handler = (e: KeyboardEvent) => {
      if (e.key === 'f' && !e.ctrlKey && !e.metaKey) {
        rf.fitView({ padding: 0.15, duration: 400 })
        onFitView?.()
      }
      if (e.key === 'Escape') onNodeSelect?.(null)
    }
    window.addEventListener('keydown', handler)
    return () => window.removeEventListener('keydown', handler)
  }, [rf, onFitView, onNodeSelect])

  if (error) {
    return (
      <div className="flex h-full items-center justify-center p-6">
        <div className="max-w-md rounded-sm border border-red-500/40 bg-red-500/10 p-5 text-sm text-red-200">
          <p className="mb-3 font-semibold">Failed to load graph</p>
          <p className="mb-4 break-words text-red-300/90">{error}</p>
          {onRetry && (
            <button
              type="button"
              onClick={onRetry}
              className="rounded-sm border border-red-400/60 px-3 py-1.5 text-xs font-medium text-red-100 hover:bg-red-500/20"
            >
              Retry
            </button>
          )}
        </div>
      </div>
    )
  }

  if (!data) {
    return (
      <div className="flex h-full items-center justify-center">
        <p className="text-sm text-dim">
          Pick a product to see its codex graph.
        </p>
      </div>
    )
  }

  if (data.meta.empty_reason === 'no_modules_indexed') {
    return (
      <div className="flex h-full items-center justify-center p-6">
        <div className="max-w-md rounded-sm border border-rule bg-surface-800 p-5 text-center text-sm text-muted">
          <p className="mb-2 font-semibold">No indexed modules yet</p>
          <p className="mb-3 text-dim">
            This product has no modules in the codex. Run indexing from the product
            admin page.
          </p>
          <a
            href="/admin/products"
            className="inline-block rounded-sm border border-accent/60 px-3 py-1.5 text-xs font-medium text-accent hover:bg-accent-hover/20"
          >
            Go to Products
          </a>
        </div>
      </div>
    )
  }

  return (
    <div className="relative h-full w-full">
      {loading && (
        <div className="pointer-events-none absolute inset-0 z-10 flex items-center justify-center bg-surface-900/40">
          <div className="h-8 w-8 animate-spin rounded-full border-2 border-accent border-t-transparent" />
        </div>
      )}
      {data.meta.truncated && (
        <div className="absolute left-4 top-4 z-10 rounded-sm border border-amber-500/40 bg-amber-500/10 px-3 py-1.5 text-xs text-amber-200">
          Showing top 500 of {data.meta.node_count}+ modules by connectedness. Use filters to narrow.
        </div>
      )}
      <ReactFlow
        nodes={displayedNodes}
        edges={styledEdges}
        onNodeMouseEnter={onNodeMouseEnter}
        onNodeMouseLeave={onNodeMouseLeave}
        onNodeDoubleClick={onNodeDoubleClick}
        nodeTypes={nodeTypes}
        onNodesChange={onNodesChange}
        onNodeClick={handleNodeClick}
        onPaneClick={handlePaneClick}
        fitView
        fitViewOptions={{ padding: 0.15 }}
        onlyRenderVisibleElements={displayedNodes.length > 200}
        minZoom={0.2}
        maxZoom={2}
        proOptions={{ hideAttribution: true }}
      >
        <Background color="#334155" gap={24} />
        <Controls />
        <MiniMap
          nodeColor={(n) => {
            const d = n.data as GraphNodeData | undefined
            if (!d) return '#475569'
            if (d.external) return '#64748b'
            if (d.risk_level === 'high') return '#ef4444'
            if (d.risk_level === 'medium') return '#f59e0b'
            if (d.risk_level === 'low') return '#10b981'
            return '#64748b'
          }}
          maskColor="rgba(15, 23, 42, 0.6)"
          pannable
          zoomable
        />
      </ReactFlow>
    </div>
  )
}

/**
 * Reusable, drop-in codex graph.
 *
 * Designed to be lifted into `ae-marketing` unchanged — it imports only
 * reactflow, dagre, and the palette/layout helpers in this same folder.
 * It DOES NOT import useApi, localStorage, or react-router. All data
 * fetching is the caller's responsibility.
 */
export function CodexGraph(props: CodexGraphProps) {
  const outerRef = useRef<HTMLDivElement>(null)
  return (
    <div ref={outerRef} className="h-full w-full">
      <ReactFlowProvider>
        <CodexGraphInner {...props} />
      </ReactFlowProvider>
    </div>
  )
}
