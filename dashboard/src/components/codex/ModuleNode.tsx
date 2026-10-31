import { memo } from 'react'
import { Handle, Position, type NodeProps } from 'reactflow'
import type { GraphNodeData } from '@/types'
import { RISK_COLORS } from './palette'

function ModuleNodeImpl({ data, selected }: NodeProps<GraphNodeData>) {
  const palette = RISK_COLORS[data.risk_level] ?? RISK_COLORS.unknown
  const ring = selected ? 'ring-2 ring-indigo-400' : ''
  const insightCount = data.insight_count ?? 0

  return (
    <div
      tabIndex={0}
      role="button"
      title={`${data.name}${data.purpose ? ` — ${data.purpose.split('.')[0]}` : ''}`}
      aria-label={`${data.name}, risk ${data.risk_level}, ${insightCount} insights, ${data.purpose.split('.')[0] ?? ''}`}
      className={`relative rounded-sm border px-3 py-2 text-xs font-medium shadow-md outline-none transition-all hover:brightness-110 focus:ring-2 focus:ring-accent ${ring}`}
      style={{
        background: palette.fill,
        borderColor: palette.border,
        color: palette.text,
        minWidth: 140,
        maxWidth: 220,
      }}
    >
      <Handle type="target" position={Position.Top} style={{ background: palette.border }} />
      <div className="flex items-center gap-2">
        <span
          aria-hidden
          className="inline-block h-2 w-2 rounded-full"
          style={{ background: palette.border }}
        />
        <span className="truncate">{data.name}</span>
      </div>
      {insightCount > 0 && (
        <div
          className="absolute -top-2 -right-2 flex h-4 min-w-[16px] items-center justify-center rounded-full bg-indigo-500 px-1 font-mono text-[9px] font-bold text-white shadow-sm"
          title={`${insightCount} deep analysis insight${insightCount !== 1 ? 's' : ''} reference this module`}
        >
          {insightCount}
        </div>
      )}
      <Handle type="source" position={Position.Bottom} style={{ background: palette.border }} />
    </div>
  )
}

export const ModuleNode = memo(ModuleNodeImpl)
