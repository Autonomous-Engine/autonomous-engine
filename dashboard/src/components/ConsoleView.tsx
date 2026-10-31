import { useEffect, useRef, useState, useCallback } from 'react'
import { useWebSocket } from '@/hooks/useWebSocket'
import { useApi } from '@/hooks/useApi'
import { PhaseBadge } from './ImplementationPhase'

interface ConsoleViewProps {
  workItemId: string
}

interface ConsoleEvent {
  work_item_id: string
  task_id: string
  agent_type: string
  tool: string
  arguments: string
  result: string
  is_error: boolean
  timestamp: string
  // Controller state the agent recorded after this call (M3-02) and
  // run_command's exit code. Absent for agents without a controller.
  phase?: string
  exit_code?: number | null
}

function formatTime(timestamp: string): string {
  try {
    const d = new Date(timestamp)
    const hms = d.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit', second: '2-digit' })
    const ms = String(d.getMilliseconds()).padStart(3, '0')
    return `${hms}.${ms}`
  } catch {
    return timestamp
  }
}

function argsPreview(args: string): string {
  try {
    const parsed = JSON.parse(args)
    if (parsed && typeof parsed === 'object') {
      const val =
        parsed.path ??
        parsed.pattern ??
        parsed.command ??
        parsed.query
      if (val !== undefined && val !== null) {
        return String(val).slice(0, 80)
      }
    }
  } catch {
    // not JSON — fall through
  }
  return args.slice(0, 80)
}

export default function ConsoleView({ workItemId }: ConsoleViewProps) {
  const { lastMessage, subscribe, unsubscribe } = useWebSocket()
  const { get } = useApi()

  // Store events without causing re-renders on every arrival
  const eventsRef = useRef<ConsoleEvent[]>([])
  const historyLoaded = useRef(false)
  const [renderTick, setRenderTick] = useState(0)

  // Expanded rows
  const [expandedIds, setExpandedIds] = useState<Set<number>>(new Set())

  // Filter state
  const [agentFilter, setAgentFilter] = useState<string>('all')
  const [toolFilter, setToolFilter] = useState<string>('')
  const [errorsOnly, setErrorsOnly] = useState(false)

  // Dynamically observed agent types
  const agentTypesRef = useRef<Set<string>>(new Set())
  const [agentTypes, setAgentTypes] = useState<string[]>([])

  // Scroll tracking
  const scrollRef = useRef<HTMLDivElement>(null)
  const pinnedRef = useRef(true)
  const [showJumpToBottom, setShowJumpToBottom] = useState(false)

  // Subscribe / unsubscribe on mount/unmount
  useEffect(() => {
    subscribe('console')
    return () => unsubscribe('console')
  }, [subscribe, unsubscribe])

  // Load historical events from DB on mount
  useEffect(() => {
    if (historyLoaded.current) return
    historyLoaded.current = true
    get<ConsoleEvent[]>(`/work-items/tool-events?work_item_id=${workItemId}`)
      .then(events => {
        if (events?.length) {
          eventsRef.current = events
          const types = new Set(events.map(e => e.agent_type).filter(Boolean))
          agentTypesRef.current = types
          setAgentTypes(Array.from(types).sort())
          setRenderTick(t => t + 1)
        }
      })
      .catch(() => {})
  }, [workItemId, get])

  // Handle incoming WebSocket messages
  useEffect(() => {
    if (!lastMessage || lastMessage.channel !== 'console') return
    const event = lastMessage.data as ConsoleEvent
    if (event.work_item_id !== workItemId) return

    eventsRef.current = [...eventsRef.current, event]

    // Track unique agent types
    if (!agentTypesRef.current.has(event.agent_type)) {
      agentTypesRef.current.add(event.agent_type)
      setAgentTypes(Array.from(agentTypesRef.current).sort())
    }

    setRenderTick((t) => t + 1)
  }, [lastMessage, workItemId])

  // Auto-scroll when pinned and new events arrive
  useEffect(() => {
    if (pinnedRef.current && scrollRef.current) {
      scrollRef.current.scrollTop = scrollRef.current.scrollHeight
    }
  }, [renderTick])

  const handleScroll = useCallback(() => {
    const el = scrollRef.current
    if (!el) return
    const distFromBottom = el.scrollHeight - el.scrollTop - el.clientHeight
    const pinned = distFromBottom < 40
    pinnedRef.current = pinned
    setShowJumpToBottom(!pinned)
  }, [])

  const jumpToBottom = useCallback(() => {
    if (scrollRef.current) {
      scrollRef.current.scrollTop = scrollRef.current.scrollHeight
    }
    pinnedRef.current = true
    setShowJumpToBottom(false)
  }, [])

  const toggleExpand = useCallback((idx: number) => {
    setExpandedIds((prev) => {
      const next = new Set(prev)
      if (next.has(idx)) {
        next.delete(idx)
      } else {
        next.add(idx)
      }
      return next
    })
  }, [])

  // Compute filtered events
  const filtered = eventsRef.current.filter((ev) => {
    if (agentFilter !== 'all' && ev.agent_type !== agentFilter) return false
    if (toolFilter && !ev.tool.toLowerCase().includes(toolFilter.toLowerCase())) return false
    if (errorsOnly && !ev.is_error) return false
    return true
  })

  return (
    <div className="flex flex-col h-[600px] rounded-sm border border-rule bg-surface-950 overflow-hidden">
      {/* Filter bar */}
      <div className="flex items-center gap-3 px-3 py-2 border-b border-rule bg-surface-900 text-xs shrink-0">
        {/* Agent type dropdown */}
        <select
          value={agentFilter}
          onChange={(e) => setAgentFilter(e.target.value)}
          className="rounded bg-surface-800 border border-rule px-2 py-1 text-xs text-fg focus:outline-none"
        >
          <option value="all">All agents</option>
          {agentTypes.map((a) => (
            <option key={a} value={a}>
              {a}
            </option>
          ))}
        </select>

        {/* Tool name filter */}
        <input
          type="text"
          value={toolFilter}
          onChange={(e) => setToolFilter(e.target.value)}
          placeholder="Filter tool..."
          className="rounded bg-surface-800 border border-rule px-2 py-1 text-xs text-fg placeholder:text-dim focus:outline-none w-36"
        />

        {/* Errors only */}
        <label className="flex items-center gap-1.5 cursor-pointer select-none text-muted">
          <input
            type="checkbox"
            checked={errorsOnly}
            onChange={(e) => setErrorsOnly(e.target.checked)}
            className="accent-accent"
          />
          Errors only
        </label>

        {/* Event count */}
        <span className="ml-auto text-dim">
          {filtered.length} / {eventsRef.current.length} events
        </span>
      </div>

      {/* Scrolling log area */}
      <div
        ref={scrollRef}
        onScroll={handleScroll}
        className="flex-1 overflow-y-auto font-mono text-xs"
      >
        {filtered.length === 0 ? (
          <div className="flex h-full items-center justify-center text-dim">
            Waiting for agent activity...
          </div>
        ) : (
          filtered.map((ev) => {
            // Use original index in eventsRef for stable key
            const globalIdx = eventsRef.current.indexOf(ev)
            const expanded = expandedIds.has(globalIdx)
            const preview = argsPreview(ev.arguments)

            return (
              <div
                key={globalIdx}
                className={[
                  'border-b border-rule/30 hover:bg-surface-900/50 cursor-pointer',
                  ev.is_error ? 'bg-red-950/20' : '',
                ].join(' ')}
                onClick={() => toggleExpand(globalIdx)}
              >
                {/* Collapsed row */}
                <div className="px-3 py-1.5 flex items-baseline gap-1 leading-snug">
                  <span className="text-dim shrink-0">[{formatTime(ev.timestamp)}]</span>
                  {ev.phase && <PhaseBadge phase={ev.phase} />}
                  <span className="text-cyan-400 shrink-0">{ev.agent_type}</span>
                  <span className="text-dim shrink-0">&gt;</span>
                  <span
                    className={ev.is_error ? 'text-red-400' : 'text-amber-300'}
                  >
                    {ev.tool}
                  </span>
                  <span className="text-dim truncate">({preview})</span>
                  {ev.exit_code !== undefined && ev.exit_code !== null && (
                    <span
                      data-testid="exit-code"
                      className={`shrink-0 ${ev.exit_code === 0 ? 'text-emerald-400' : 'text-red-400'}`}
                    >
                      exit {ev.exit_code}
                    </span>
                  )}
                </div>

                {/* Expanded result */}
                {expanded && (
                  <pre className="px-3 pb-2 text-[11px] text-muted whitespace-pre-wrap break-words max-h-[400px] overflow-y-auto">
                    {ev.result}
                  </pre>
                )}
              </div>
            )
          })
        )}
      </div>

      {/* Jump to bottom button — overlaid at bottom-right of the scroll area */}
      {showJumpToBottom && (
        <div className="shrink-0 flex justify-end px-3 py-1.5 border-t border-rule/30 bg-surface-950">
          <button
            onClick={jumpToBottom}
            className="rounded bg-surface-900 border border-rule px-3 py-1 text-xs text-muted hover:text-fg hover:bg-surface-800 transition-colors"
          >
            Jump to bottom
          </button>
        </div>
      )}
    </div>
  )
}
