import { useEffect, useState, useRef } from 'react'
import { useWebSocket } from '@/hooks/useWebSocket'

interface IndexingEvent {
  product_id: string
  product_name: string
  repo_name: string
  phase: string
  detail: string
  files_processed: number
  total_files: number
  modules_found: number
  patterns_found: number
  started_at: string
  timestamp: string
}

const PHASE_LABELS: Record<string, string> = {
  cloning: 'Cloning repository',
  detecting_languages: 'Detecting languages',
  scanning_files: 'Scanning files',
  analyzing_ast: 'Analyzing code structure',
  building_dependencies: 'Building dependency graph',
  detecting_patterns: 'Detecting patterns',
  analyzing_git_history: 'Analyzing git history',
  vector_indexing: 'Building vector index',
  updating_codex: 'Updating codex',
  deep_analysis: 'Deep analysis (LLM)',
  prefilling_candidates: 'Pre-filling onboarding',
  complete: 'Complete',
  failed: 'Failed',
}

const PHASE_ORDER = [
  'cloning',
  'detecting_languages',
  'scanning_files',
  'analyzing_ast',
  'building_dependencies',
  'detecting_patterns',
  'analyzing_git_history',
  'vector_indexing',
  'updating_codex',
  'deep_analysis',
  'prefilling_candidates',
  'complete',
]

export default function IndexingActivity() {
  const { lastMessage, subscribe, unsubscribe } = useWebSocket()
  const [event, setEvent] = useState<IndexingEvent | null>(null)
  const [recentEvents, setRecentEvents] = useState<IndexingEvent[]>([])
  const [expanded, setExpanded] = useState(false)
  const dismissTimer = useRef<ReturnType<typeof setTimeout> | null>(null)

  useEffect(() => {
    subscribe('indexing')
    return () => unsubscribe('indexing')
  }, [subscribe, unsubscribe])

  useEffect(() => {
    if (!lastMessage || lastMessage.channel !== 'indexing') return
    const data = lastMessage.data as IndexingEvent
    if (!data?.phase) return

    setEvent(data)
    setRecentEvents((prev) => {
      const next = [data, ...prev].slice(0, 50)
      return next
    })

    if (dismissTimer.current) clearTimeout(dismissTimer.current)
    if (data.phase === 'complete' || data.phase === 'failed') {
      dismissTimer.current = setTimeout(() => setEvent(null), 15_000)
    }
  }, [lastMessage])

  if (!event) return null

  const isActive = event.phase !== 'complete' && event.phase !== 'failed'
  const phaseLabel = PHASE_LABELS[event.phase] ?? event.phase
  const phaseIndex = PHASE_ORDER.indexOf(event.phase)
  const progressPercent = phaseIndex >= 0
    ? Math.round(((phaseIndex + 1) / PHASE_ORDER.length) * 100)
    : 0

  const elapsed = event.started_at
    ? formatDuration(new Date(event.started_at))
    : ''

  return (
    <div className="border-b border-indigo-500/20 bg-indigo-500/5">
      <button
        type="button"
        onClick={() => setExpanded((e) => !e)}
        className="flex w-full items-center gap-3 px-6 py-2 text-left"
      >
        {/* Spinner or check */}
        {isActive ? (
          <div className="h-4 w-4 animate-spin rounded-full border-2 border-indigo-400 border-t-transparent flex-shrink-0" />
        ) : event.phase === 'complete' ? (
          <span className="text-emerald-400 flex-shrink-0">&#10003;</span>
        ) : (
          <span className="text-red-400 flex-shrink-0">&#10007;</span>
        )}

        {/* Main status */}
        <div className="flex-1 min-w-0">
          <div className="flex items-center gap-2">
            <span className="font-mono text-xs text-indigo-300 font-medium">
              {isActive ? 'Indexing' : event.phase === 'complete' ? 'Indexed' : 'Index failed'}
            </span>
            <span className="font-mono text-xs text-fg truncate">
              {event.product_name}
            </span>
            {event.repo_name && (
              <span className="font-mono text-[10px] text-muted truncate">
                / {event.repo_name}
              </span>
            )}
          </div>
          <div className="flex items-center gap-2 mt-0.5">
            <span className="font-mono text-[10px] text-muted">
              {phaseLabel}
            </span>
            {event.detail && isActive && (
              <span className="font-mono text-[10px] text-dim truncate">
                — {event.detail}
              </span>
            )}
          </div>
        </div>

        {/* Progress bar */}
        {isActive && (
          <div className="flex items-center gap-2 flex-shrink-0">
            <div className="w-24 h-1.5 rounded-full bg-surface-700 overflow-hidden">
              <div
                className="h-full rounded-full bg-indigo-400 transition-all duration-500"
                style={{ width: `${progressPercent}%` }}
              />
            </div>
            <span className="font-mono text-[10px] text-muted w-8 text-right">
              {progressPercent}%
            </span>
          </div>
        )}

        {/* Stats */}
        <div className="flex items-center gap-3 flex-shrink-0">
          {event.modules_found > 0 && (
            <span className="font-mono text-[10px] text-muted">
              {event.modules_found} modules
            </span>
          )}
          {event.patterns_found > 0 && (
            <span className="font-mono text-[10px] text-muted">
              {event.patterns_found} patterns
            </span>
          )}
          {elapsed && (
            <span className="font-mono text-[10px] text-dim">
              {elapsed}
            </span>
          )}
        </div>

        {/* Expand arrow */}
        <span className={`text-muted text-xs transition-transform ${expanded ? 'rotate-180' : ''}`}>
          ▾
        </span>
      </button>

      {/* Expanded event log */}
      {expanded && (
        <div className="border-t border-indigo-500/10 px-6 py-2 max-h-48 overflow-y-auto">
          <div className="space-y-1">
            {recentEvents.map((e, i) => (
              <div key={i} className="flex items-center gap-2 font-mono text-[10px]">
                <span className="text-dim w-16 flex-shrink-0">
                  {new Date(e.timestamp).toLocaleTimeString()}
                </span>
                <span className="text-indigo-400 w-28 flex-shrink-0 truncate">
                  {PHASE_LABELS[e.phase] ?? e.phase}
                </span>
                <span className="text-muted truncate">
                  {e.detail || '—'}
                </span>
              </div>
            ))}
            {recentEvents.length === 0 && (
              <p className="text-dim text-xs">No events yet</p>
            )}
          </div>
        </div>
      )}
    </div>
  )
}

function formatDuration(startedAt: Date): string {
  const seconds = Math.floor((Date.now() - startedAt.getTime()) / 1000)
  if (seconds < 60) return `${seconds}s`
  const minutes = Math.floor(seconds / 60)
  const remainingSeconds = seconds % 60
  if (minutes < 60) return `${minutes}m ${remainingSeconds}s`
  const hours = Math.floor(minutes / 60)
  return `${hours}h ${minutes % 60}m`
}
