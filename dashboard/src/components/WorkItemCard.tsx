import { useState } from 'react'
import { Link } from 'react-router-dom'
import { useApi } from '@/hooks/useApi'
import type { WorkItem, WorkItemState, Priority } from '@/types'
import { CodexPinBadge } from './CodexPin'

interface WorkItemCardProps {
  item: WorkItem
  productName?: string
  onActionComplete?: () => void
}

// States that can be cancelled (all non-terminal active states)
const CANCELLABLE_STATES: WorkItemState[] = [
  'intake', 'prd', 'research', 'planning', 'implementation', 'verification',
  'review', 'human_review', 'qa', 'failed', 'blocked',
]

// States eligible for manual retry
const RETRYABLE_STATES: WorkItemState[] = ['blocked', 'failed']

function priorityBadgeClass(priority: Priority): string {
  switch (priority) {
    case 'critical':
      return 'bg-red-500/20 text-red-300 border border-red-500/30'
    case 'high':
      return 'bg-orange-500/20 text-orange-300 border border-orange-500/30'
    case 'medium':
      return 'bg-yellow-500/20 text-yellow-300 border border-yellow-500/30'
    case 'low':
      return 'bg-surface-700/40 text-muted border border-sharp/30'
  }
}

function typeBadgeClass(type: WorkItem['type']): string {
  switch (type) {
    case 'feature':
      return 'bg-accent/10 text-accent'
    case 'bug':
      return 'bg-red-500/20 text-red-300'
    case 'task':
      return 'bg-sky-500/20 text-sky-300'
    case 'subtask':
      return 'bg-cyan-500/20 text-cyan-300'
    case 'research':
      return 'bg-violet-500/20 text-violet-300'
    case 'qa':
      return 'bg-lime-500/20 text-lime-300'
  }
}

function borderColorByState(state: WorkItemState): string {
  switch (state) {
    case 'intake':
    case 'prd':
    case 'research':
    case 'planning':
    case 'implementation':
      return 'border-l-emerald-500'
    case 'prd_approval':
    case 'awaiting_user':
      return 'border-l-yellow-400'
    case 'verification':
      return 'border-l-indigo-400'
    case 'human_review':
      return 'border-l-blue-400'
    case 'blocked':
    case 'failed':
    case 'rejected':
      return 'border-l-red-500'
    case 'cancelled':
      return 'border-l-slate-500'
    case 'passed':
    case 'released':
      return 'border-l-yellow-400'
    default:
      return 'border-l-slate-500'
  }
}

function timeAgo(dateStr: string): string {
  const diff = Date.now() - new Date(dateStr).getTime()
  const minutes = Math.floor(diff / 60_000)
  if (minutes < 60) return `${minutes}m`
  const hours = Math.floor(minutes / 60)
  if (hours < 24) return `${hours}h`
  return `${Math.floor(hours / 24)}d`
}

export default function WorkItemCard({ item, productName, onActionComplete }: WorkItemCardProps) {
  const [expanded, setExpanded] = useState(false)
  const [confirmCancel, setConfirmCancel] = useState(false)
  const [actionLoading, setActionLoading] = useState(false)
  const [actionError, setActionError] = useState<string | null>(null)
  const { post } = useApi()

  const canCancel = CANCELLABLE_STATES.includes(item.state)
  const canRetry = RETRYABLE_STATES.includes(item.state)

  async function handleRetry(e: React.MouseEvent) {
    e.stopPropagation()
    setActionLoading(true)
    setActionError(null)
    try {
      await post(`/work-items/retry?id=${item.id}`)
      onActionComplete?.()
    } catch (err) {
      setActionError(err instanceof Error ? err.message : 'Retry failed')
    } finally {
      setActionLoading(false)
    }
  }

  async function handleCancel(e: React.MouseEvent) {
    e.stopPropagation()
    if (!confirmCancel) {
      setConfirmCancel(true)
      return
    }
    setActionLoading(true)
    setActionError(null)
    try {
      await post(`/work-items/cancel?id=${item.id}`)
      setConfirmCancel(false)
      onActionComplete?.()
    } catch (err) {
      setActionError(err instanceof Error ? err.message : 'Cancel failed')
    } finally {
      setActionLoading(false)
    }
  }

  function handleCancelAbort(e: React.MouseEvent) {
    e.stopPropagation()
    setConfirmCancel(false)
  }

  return (
    <div
      className={[
        'w-full rounded-sm border border-rule border-l-2',
        borderColorByState(item.state),
        'bg-surface-800 p-3 text-left transition-all hover:border-rule',
        expanded ? 'ring-1 ring-indigo-500/40' : '',
      ].join(' ')}
    >
      {/* Action-required pulse for states that need user input */}
      {(item.state === 'prd_approval' || item.state === 'awaiting_user') && (
        <div className="mb-2 flex items-center gap-1.5 text-xs font-medium text-yellow-300">
          <span className="relative flex h-2 w-2">
            <span className="absolute inline-flex h-full w-full animate-ping rounded-full bg-yellow-400 opacity-75" />
            <span className="relative inline-flex h-2 w-2 rounded-full bg-yellow-400" />
          </span>
          {item.state === 'prd_approval' ? 'Awaiting your approval' : 'Needs your input'}
        </div>
      )}

      {/* Clickable header area — toggle expand on click, navigate on title click */}
      <button
        type="button"
        className="w-full text-left"
        onClick={() => setExpanded((v) => !v)}
      >
        {/* Header row */}
        <div className="flex items-start justify-between gap-2">
          <Link
            to={`/work-items/${item.id}`}
            onClick={(e) => e.stopPropagation()}
            className="truncate-2 text-sm font-medium text-fg leading-snug hover:text-accent transition-colors"
          >
            {item.title}
          </Link>
          <span className="mt-0.5 shrink-0 text-xs text-dim">
            {timeAgo(item.updated_at)}
          </span>
        </div>

        {/* Agent active indicator for in-flight states */}
        {['intake', 'prd', 'research', 'planning', 'implementation', 'verification', 'review', 'qa'].includes(item.state) && (
          <div className="mt-1.5 flex items-center gap-1.5 text-[10px] text-accent-dim">
            <span className="inline-block h-1.5 w-1.5 rounded-full bg-accent animate-pulse" />
            Agent working
          </div>
        )}

        {/* Badges */}
        <div className="mt-2 flex flex-wrap gap-1.5">
          <span
            className={[
              'rounded px-1.5 py-0.5 text-xs font-medium',
              typeBadgeClass(item.type),
            ].join(' ')}
          >
            {item.type}
          </span>
          <span
            className={[
              'rounded px-1.5 py-0.5 text-xs font-medium',
              priorityBadgeClass(item.priority),
            ].join(' ')}
          >
            {item.priority}
          </span>
          {item.external_ref && (
            <span className="rounded bg-surface-700 px-1.5 py-0.5 text-xs text-muted">
              {item.external_ref}
            </span>
          )}
          {productName && (
            <span className="rounded bg-accent/10 px-1.5 py-0.5 text-xs text-accent border border-indigo-700/40">
              {productName}
            </span>
          )}
        </div>
        {/* Codex the item was planned against (codex coherence Part 1) */}
        {(item.codex_pin || item.codex_changed_since_planning) && (
          <div className="mt-1.5">
            <CodexPinBadge pin={item.codex_pin} note={item.codex_changed_since_planning} />
          </div>
        )}
      </button>

      {/* Expanded details */}
      {expanded && (
        <div className="mt-3 border-t border-rule pt-3">
          {item.description ? (
            <p className="text-xs text-muted leading-relaxed line-clamp-4">
              {item.description}
            </p>
          ) : (
            <p className="text-xs italic text-dim">No description.</p>
          )}

          <div className="mt-2 grid grid-cols-2 gap-x-4 gap-y-1 text-xs text-dim">
            <span>
              Created:{' '}
              <span className="text-muted">
                {new Date(item.created_at).toLocaleDateString()}
              </span>
            </span>
            <span>
              Codex v
              <span className="text-muted">{item.codex_version}</span>
            </span>
          </div>
        </div>
      )}

      {/* Action buttons */}
      {(canCancel || canRetry) && (
        <div className="mt-2.5 flex items-center gap-2 border-t border-rule/40 pt-2.5">
          {canRetry && (
            <button
              type="button"
              disabled={actionLoading}
              onClick={handleRetry}
              className="rounded bg-amber-600/20 px-2 py-1 text-xs font-medium text-amber-300 border border-amber-600/30 hover:bg-amber-600/30 disabled:opacity-50 transition-colors"
            >
              {actionLoading ? 'Retrying…' : 'Retry'}
            </button>
          )}
          {canCancel && !confirmCancel && (
            <button
              type="button"
              disabled={actionLoading}
              onClick={handleCancel}
              className="rounded bg-red-600/20 px-2 py-1 text-xs font-medium text-red-300 border border-red-600/30 hover:bg-red-600/30 disabled:opacity-50 transition-colors"
            >
              Cancel
            </button>
          )}
          {canCancel && confirmCancel && (
            <div className="flex items-center gap-1.5">
              <span className="text-xs text-muted">Confirm?</span>
              <button
                type="button"
                disabled={actionLoading}
                onClick={handleCancel}
                className="rounded bg-red-600 px-2 py-1 text-xs font-medium text-white hover:bg-red-700 disabled:opacity-50 transition-colors"
              >
                {actionLoading ? 'Cancelling…' : 'Yes, cancel'}
              </button>
              <button
                type="button"
                onClick={handleCancelAbort}
                className="rounded bg-surface-700 px-2 py-1 text-xs font-medium text-muted hover:bg-surface-700 transition-colors"
              >
                No
              </button>
            </div>
          )}
        </div>
      )}

      {/* Inline action error */}
      {actionError && (
        <p className="mt-1.5 text-xs text-red-400">{actionError}</p>
      )}
    </div>
  )
}
