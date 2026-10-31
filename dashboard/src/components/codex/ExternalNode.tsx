import { memo } from 'react'
import { Handle, Position, type NodeProps } from 'reactflow'
import type { GraphNodeData } from '@/types'
import { EXTERNAL_COLORS } from './palette'

/**
 * Custom node for unresolved external dependencies. Dimmed (50% opacity,
 * per locked decision 3). Renders the dependency name as plain text.
 */
function ExternalNodeImpl({ data, selected }: NodeProps<GraphNodeData>) {
  const ring = selected ? 'ring-2 ring-indigo-400' : ''
  return (
    <div
      tabIndex={0}
      role="button"
      title={`${data.name} — external dependency, not indexed`}
      aria-label={`External dependency ${data.name}, not indexed in this product`}
      className={`rounded-sm border border-dashed px-3 py-1.5 text-xs italic outline-none focus:ring-2 focus:ring-accent ${ring}`}
      style={{
        background: EXTERNAL_COLORS.fill,
        borderColor: EXTERNAL_COLORS.border,
        color: EXTERNAL_COLORS.text,
        opacity: EXTERNAL_COLORS.opacity,
        minWidth: 120,
      }}
    >
      <Handle type="target" position={Position.Top} style={{ background: EXTERNAL_COLORS.border }} />
      <span className="truncate">{data.name}</span>
    </div>
  )
}

export const ExternalNode = memo(ExternalNodeImpl)
