import { useEffect, useState, useCallback } from 'react'
import { Link } from 'react-router-dom'
import { useApi } from '@/hooks/useApi'
import { useWebSocket } from '@/hooks/useWebSocket'
import type { AgentDefinition, AgentStatus } from '@/types'

// Pipeline steps in display order
const PIPELINE_STEPS: { key: keyof AgentDefinition; label: string }[] = [
  { key: 'step_product_analysis', label: 'Product' },
  { key: 'step_research', label: 'Research' },
  { key: 'step_planning', label: 'Planning' },
  { key: 'step_implementation', label: 'Impl' },
  { key: 'step_review', label: 'Review' },
  { key: 'step_human_review', label: 'Human' },
  { key: 'step_qa', label: 'QA' },
]

function formatDuration(startedAt?: string): string {
  if (!startedAt) return '—'
  const diffMs = Date.now() - new Date(startedAt).getTime()
  const seconds = Math.floor(diffMs / 1000)
  if (seconds < 60) return `${seconds}s`
  const minutes = Math.floor(seconds / 60)
  if (minutes < 60) return `${minutes}m ${seconds % 60}s`
  return `${Math.floor(minutes / 60)}h ${minutes % 60}m`
}



interface AgentCardProps {
  definition: AgentDefinition
  containers: AgentStatus[]
}

function AgentCard({ definition, containers }: AgentCardProps) {
  const activeContainers = containers.filter((c) => c.status === 'active')
  const currentTask = activeContainers[0]

  return (
    <div className="rounded-sm border border-rule bg-surface-800 p-4 flex flex-col gap-3">
      {/* Header row */}
      <div className="flex items-start justify-between gap-2">
        <div className="flex-1 min-w-0">
          <div className="flex items-center gap-2 flex-wrap">
            <span className="text-sm font-semibold text-fg truncate">
              {definition.display_name || definition.name}
            </span>
            {definition.is_default && (
              <span className="rounded-full bg-accent/10 px-2 py-0.5 text-xs font-medium text-accent">
                Default
              </span>
            )}
            {definition.enabled ? (
              <span className="rounded-full bg-emerald-500/20 px-2 py-0.5 text-xs font-medium text-emerald-300">
                Enabled
              </span>
            ) : (
              <span className="rounded-full bg-surface-700/40 px-2 py-0.5 text-xs font-medium text-dim">
                Disabled
              </span>
            )}
          </div>
          {definition.description && (
            <p className="mt-0.5 text-xs text-dim line-clamp-2">
              {definition.description}
            </p>
          )}
        </div>
      </div>

      {/* Provider + Model badge */}
      <div className="flex items-center gap-2 flex-wrap">
        <span className="rounded bg-surface-700 px-2 py-0.5 text-xs text-muted font-medium">
          {definition.provider}
        </span>
        <span className="rounded bg-surface-700/40 px-2 py-0.5 font-mono text-xs text-dim">
          {definition.model_id}
        </span>
      </div>

      {/* Pipeline steps */}
      <div className="flex items-center gap-1 flex-wrap">
        {PIPELINE_STEPS.map(({ key, label }) => {
          const active = definition[key] as boolean
          return (
            <span
              key={key}
              className={[
                'rounded px-1.5 py-0.5 text-xs font-medium',
                active
                  ? 'bg-accent/15 text-accent'
                  : 'bg-surface-700/30 text-dim',
              ].join(' ')}
            >
              {label}
            </span>
          )
        })}
      </div>

      {/* Current activity */}
      <div className="rounded bg-surface-700/30 px-3 py-2">
        {currentTask ? (
          <div className="flex items-start gap-2">
            <span className="mt-0.5 h-1.5 w-1.5 shrink-0 animate-pulse rounded-full bg-emerald-400" />
            <div className="min-w-0">
              <p className="text-xs font-medium text-emerald-300">Active</p>
              {(currentTask.work_item_id ?? currentTask.current_work_item_id) ? (
                <Link
                  to={`/work-items/${currentTask.work_item_id ?? currentTask.current_work_item_id}`}
                  className="block truncate text-xs text-accent hover:underline"
                >
                  {currentTask.work_item_title ?? currentTask.current_task ?? currentTask.work_item_id ?? currentTask.current_work_item_id}
                </Link>
              ) : (
                <p className="truncate text-xs text-muted">
                  {currentTask.current_task ?? 'Processing…'}
                </p>
              )}
              <p className="text-xs text-dim">
                Running {formatDuration(currentTask.started_at)} &middot; instance{' '}
                <span className="font-mono">{currentTask.instance_id}</span>
              </p>
            </div>
          </div>
        ) : (
          <div className="flex items-center gap-2">
            <span className="h-1.5 w-1.5 rounded-full bg-dim" />
            <span className="text-xs text-dim">Idle</span>
            {containers.length > 0 && (
              <span className="text-xs text-dim">
                ({containers.length} container{containers.length !== 1 ? 's' : ''})
              </span>
            )}
          </div>
        )}
      </div>
    </div>
  )
}

export default function AgentActivity() {
  const { get } = useApi()
  const { lastMessage, subscribe, unsubscribe } = useWebSocket()
  const [definitions, setDefinitions] = useState<AgentDefinition[]>([])
  const [containers, setContainers] = useState<AgentStatus[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)

  const fetchData = useCallback(async () => {
    try {
      setError(null)
      const [defs, status] = await Promise.all([
        get<AgentDefinition[]>('/admin/agents'),
        get<AgentStatus[]>('/agents/status'),
      ])
      setDefinitions(defs)
      setContainers(status)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to load agent data')
    } finally {
      setLoading(false)
    }
  }, [get])

  useEffect(() => {
    fetchData()
  }, [fetchData])

  useEffect(() => {
    subscribe('agents')
    return () => unsubscribe('agents')
  }, [subscribe, unsubscribe])

  useEffect(() => {
    if (!lastMessage || lastMessage.channel !== 'agents') return
    fetchData()
  }, [lastMessage, fetchData])

  const activeContainerCount = containers.filter((c) => c.status === 'active').length

  if (loading) {
    return (
      <div className="flex h-64 items-center justify-center">
        <div className="h-8 w-8 animate-spin rounded-full border-2 border-accent border-t-transparent" />
      </div>
    )
  }

  return (
    <div className="flex flex-col gap-6">
      {/* Header */}
      <div className="flex items-center justify-between">
        <div>
          <h2 className="text-lg font-semibold text-fg">Agent Activity</h2>
          <p className="text-sm text-dim">
            {definitions.length} agent definition{definitions.length !== 1 ? 's' : ''} &middot;{' '}
            {activeContainerCount} container{activeContainerCount !== 1 ? 's' : ''} active
          </p>
        </div>
        {error && (
          <span className="rounded bg-red-500/20 px-3 py-1 text-xs text-red-300">
            {error}
          </span>
        )}
      </div>

      {/* Agent definition cards */}
      {definitions.length === 0 ? (
        <div className="flex h-48 items-center justify-center rounded-sm border border-rule bg-surface-800">
          <p className="text-sm text-dim">No agent definitions found.</p>
        </div>
      ) : (
        <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 xl:grid-cols-3">
          {definitions.map((def) => {
            // Match containers to this definition by name/display_name
            const matched = containers.filter(
              (c) =>
                c.agent_type === def.name ||
                (c.agent_type as string) === def.display_name,
            )
            return <AgentCard key={def.id} definition={def} containers={matched} />
          })}
        </div>
      )}

      {/* Running Containers section */}
      <div>
        <h3 className="mb-3 text-sm font-semibold text-muted uppercase tracking-wider">
          Running Containers
        </h3>
        <div className="overflow-hidden rounded-sm border border-rule">
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b border-rule bg-surface-850">
                <th className="px-4 py-3 text-left text-xs font-medium uppercase tracking-wider text-dim">
                  Agent Type
                </th>
                <th className="px-4 py-3 text-left text-xs font-medium uppercase tracking-wider text-dim">
                  Instance
                </th>
                <th className="px-4 py-3 text-left text-xs font-medium uppercase tracking-wider text-dim">
                  Status
                </th>
                <th className="px-4 py-3 text-left text-xs font-medium uppercase tracking-wider text-dim">
                  Current Task
                </th>
                <th className="px-4 py-3 text-left text-xs font-medium uppercase tracking-wider text-dim">
                  Duration
                </th>
              </tr>
            </thead>
            <tbody className="divide-y divide-rule/40">
              {containers.length === 0 ? (
                <tr>
                  <td
                    colSpan={5}
                    className="px-4 py-8 text-center text-sm text-dim"
                  >
                    No running containers
                  </td>
                </tr>
              ) : (
                containers.map((c, idx) => (
                  <tr
                    key={`${c.agent_type}-${c.instance_id}-${idx}`}
                    className="bg-surface-800 transition-colors hover:bg-surface-850"
                  >
                    <td className="px-4 py-3 font-medium text-fg">
                      {c.agent_type}
                    </td>
                    <td className="px-4 py-3 font-mono text-xs text-muted">
                      {c.instance_id}
                    </td>
                    <td className="px-4 py-3">
                      {c.status === 'active' ? (
                        <span className="inline-flex items-center gap-1.5 rounded-full bg-emerald-500/20 px-2.5 py-0.5 text-xs font-medium text-emerald-300">
                          <span className="h-1.5 w-1.5 animate-pulse rounded-full bg-emerald-400" />
                          Active
                        </span>
                      ) : (
                        <span className="inline-flex items-center gap-1.5 rounded-full bg-surface-700/40 px-2.5 py-0.5 text-xs font-medium text-muted">
                          <span className="h-1.5 w-1.5 rounded-full bg-dim" />
                          Idle
                        </span>
                      )}
                    </td>
                    <td className="max-w-xs px-4 py-3 truncate">
                      {(c.work_item_id ?? c.current_work_item_id) ? (
                        <Link
                          to={`/work-items/${c.work_item_id ?? c.current_work_item_id}`}
                          className="font-mono text-sm text-accent hover:underline"
                        >
                          {c.work_item_title ?? c.current_task ?? c.work_item_id ?? c.current_work_item_id}
                        </Link>
                      ) : (
                        <span className="text-muted">{c.current_task ?? '—'}</span>
                      )}
                    </td>
                    <td className="px-4 py-3 font-mono text-sm text-muted">
                      {formatDuration(c.started_at)}
                    </td>
                  </tr>
                ))
              )}
            </tbody>
          </table>
        </div>
      </div>
    </div>
  )
}
