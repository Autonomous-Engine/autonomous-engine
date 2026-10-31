import dagre from '@dagrejs/dagre'
import type { Edge, Node } from 'reactflow'

const NODE_WIDTH = 180
const NODE_HEIGHT = 56

/**
 * Layered (hierarchical) layout via dagre. Top-down; dependencies above
 * dependents. Used when viewMode === "layered".
 */
export function layeredLayout(nodes: Node[], edges: Edge[]): Node[] {
  const g = new dagre.graphlib.Graph()
  g.setDefaultEdgeLabel(() => ({}))
  g.setGraph({ rankdir: 'TB', nodesep: 40, ranksep: 80 })

  for (const n of nodes) {
    g.setNode(n.id, { width: NODE_WIDTH, height: NODE_HEIGHT })
  }
  for (const e of edges) {
    g.setEdge(e.source, e.target)
  }
  dagre.layout(g)

  return nodes.map((n) => {
    const pos = g.node(n.id)
    return {
      ...n,
      position: { x: pos.x - NODE_WIDTH / 2, y: pos.y - NODE_HEIGHT / 2 },
      width: NODE_WIDTH,
      height: NODE_HEIGHT,
    }
  })
}

/**
 * Deterministic seeded radial placement for the force-directed layout.
 * ReactFlow's built-in physics runs on top of these initial positions via
 * the GraphCanvas simulation loop (see CodexGraph.tsx). Giving the sim
 * a consistent seed means two consecutive renders of the same product
 * produce near-identical layouts.
 */
export function forceSeed(nodes: Node[]): Node[] {
  const R = Math.max(280, Math.sqrt(nodes.length) * 60)
  return nodes.map((n, i) => {
    const angle = (i / Math.max(1, nodes.length)) * Math.PI * 2
    return {
      ...n,
      position: { x: Math.cos(angle) * R, y: Math.sin(angle) * R },
      width: NODE_WIDTH,
      height: NODE_HEIGHT,
    }
  })
}
