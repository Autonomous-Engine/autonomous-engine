import { useEffect, useState, useCallback, useRef, type ReactNode } from 'react'
import { useParams, useNavigate, Link } from 'react-router-dom'
import { useApi } from '@/hooks/useApi'
import { useWebSocket } from '@/hooks/useWebSocket'
import type { WorkItemDetail, WorkItemState, Priority } from '@/types'
import { usePrompt } from '@/hooks/usePrompts'
import { PromptDetailPane } from './PromptSheet'
import Markdown from './Markdown'
import ConsoleView from './ConsoleView'
import VerificationEvidence from './VerificationEvidence'
import ConfigSnapshotBadge from './ConfigSnapshotBadge'
import ImplementationRuns, { SubtaskPhase, runsByTask } from './ImplementationPhase'
import BaseSnapshot from './BaseSnapshot'
import { CodexPinBadge, CodexEventLine } from './CodexPin'
import ContractEdges from './ContractEdges'
import CheckPlans, { checkPlansByTask, isCheckPlanArtifact } from './CheckPlan'
import BriefCriteria, { briefCriteriaFrom } from './BriefCriteria'
import PlanValidation, { isCurrentPlanValidation, isPlanValidationArtifact, planValidationFrom } from './PlanValidation'
import VerifierVerdict, { isVerifierArtifact, verifierVerdictFrom } from './VerifierVerdict'
import { useChat } from '@/features/chat/ChatProvider'

// ─── Tool call event (streamed from Kafka via WebSocket) ──────────────────────

interface ToolCallEvent {
  work_item_id: string
  task_id: string
  tool: string
  arguments: string
  result_preview: string
  is_error: boolean
  timestamp: string
}

// ─── Terminal states — no auto-refresh needed ─────────────────────────────────

const TERMINAL_STATES: WorkItemState[] = ['passed', 'released', 'cancelled', 'rejected']

// ─── Helpers ──────────────────────────────────────────────────────────────────

function stateBadgeClass(state: WorkItemState): string {
  switch (state) {
    case 'intake':      return 'bg-surface-700/40 text-muted border border-sharp/40'
    case 'prd':         return 'bg-violet-500/20 text-violet-300 border border-violet-500/30'
    case 'prd_approval': return 'bg-amber-500/30 text-amber-200 border border-amber-400/50'
    case 'research':    return 'bg-blue-500/20 text-blue-300 border border-blue-500/30'
    case 'planning':    return 'bg-cyan-500/20 text-cyan-300 border border-cyan-500/30'
    case 'implementation': return 'bg-emerald-500/20 text-emerald-300 border border-emerald-500/30'
    case 'verification': return 'bg-indigo-500/20 text-indigo-300 border border-indigo-500/30'
    case 'review':      return 'bg-amber-500/20 text-amber-300 border border-amber-500/30'
    case 'human_review': return 'bg-orange-500/20 text-orange-300 border border-orange-500/30'
    case 'qa':          return 'bg-lime-500/20 text-lime-300 border border-lime-500/30'
    case 'passed':      return 'bg-teal-500/20 text-teal-300 border border-teal-500/30'
    case 'released':    return 'bg-yellow-500/20 text-yellow-300 border border-yellow-500/30'
    case 'failed':      return 'bg-red-500/20 text-red-300 border border-red-500/30'
    case 'blocked':     return 'bg-red-600/20 text-red-400 border border-red-600/30'
    case 'rejected':    return 'bg-rose-600/20 text-rose-300 border border-rose-600/30'
    case 'cancelled':   return 'bg-surface-700/30 text-muted border border-rule/30'
    default:            return 'bg-surface-700/40 text-muted border border-sharp/40'
  }
}

function stateDotColor(state: WorkItemState): string {
  switch (state) {
    case 'intake':      return 'bg-muted'
    case 'prd':         return 'bg-violet-400'
    case 'prd_approval': return 'bg-amber-400'
    case 'research':    return 'bg-blue-400'
    case 'planning':    return 'bg-cyan-400'
    case 'implementation': return 'bg-emerald-400'
    case 'verification': return 'bg-indigo-400'
    case 'review':      return 'bg-amber-400'
    case 'human_review': return 'bg-orange-400'
    case 'qa':          return 'bg-lime-400'
    case 'passed':      return 'bg-teal-400'
    case 'released':    return 'bg-yellow-400'
    case 'failed':      return 'bg-red-500'
    case 'blocked':     return 'bg-red-600'
    case 'rejected':    return 'bg-rose-500'
    case 'cancelled':   return 'bg-dim'
    default:            return 'bg-muted'
  }
}

function priorityBadgeClass(priority: Priority): string {
  switch (priority) {
    case 'critical': return 'bg-red-500/20 text-red-300 border border-red-500/30'
    case 'high':     return 'bg-orange-500/20 text-orange-300 border border-orange-500/30'
    case 'medium':   return 'bg-yellow-500/20 text-yellow-300 border border-yellow-500/30'
    case 'low':      return 'bg-surface-700/40 text-muted border border-sharp/30'
  }
}

function typeBadgeClass(type: string): string {
  switch (type) {
    case 'feature':  return 'bg-accent/10 text-accent'
    case 'bug':      return 'bg-red-500/20 text-red-300'
    case 'task':     return 'bg-sky-500/20 text-sky-300'
    case 'subtask':  return 'bg-cyan-500/20 text-cyan-300'
    case 'research': return 'bg-violet-500/20 text-violet-300'
    case 'qa':       return 'bg-lime-500/20 text-lime-300'
    default:         return 'bg-surface-700/40 text-muted'
  }
}

function artifactTypeBadgeClass(type: string): string {
  switch (type) {
    case 'prd':              return 'bg-violet-500/20 text-violet-300 border border-violet-500/30'
    case 'technical_brief':  return 'bg-blue-500/20 text-blue-300 border border-blue-500/30'
    case 'research_finding': return 'bg-cyan-500/20 text-cyan-300 border border-cyan-500/30'
    case 'review_comments':  return 'bg-amber-500/20 text-amber-300 border border-amber-500/30'
    case 'qa_report':        return 'bg-lime-500/20 text-lime-300 border border-lime-500/30'
    default:                 return 'bg-surface-700/40 text-muted border border-sharp/30'
  }
}

function artifactTypeLabel(type: string): string {
  switch (type) {
    case 'prd':              return 'PRD'
    case 'technical_brief':  return 'Technical Brief'
    case 'research_finding': return 'Research Finding'
    case 'review_comments':  return 'Review Comments'
    case 'qa_report':        return 'QA Report'
    default:                 return type
  }
}

// Extract the human-readable PRD markdown from an artifact's content blob.
// Backend stores the PRD as JSON { "prd": "# markdown..." }, but we also handle
// string payloads (pre-structured) and raw objects defensively.
function extractPrdContent(content: unknown): string {
  if (!content) return ''
  if (typeof content === 'string') {
    try {
      const parsed = JSON.parse(content)
      if (parsed && typeof parsed === 'object') {
        return (parsed.prd ?? parsed.content ?? content) as string
      }
      return content
    } catch {
      return content
    }
  }
  if (typeof content === 'object') {
    const c = content as Record<string, unknown>
    if (typeof c.prd === 'string') return c.prd
    if (typeof c.content === 'string') return c.content
    try {
      return JSON.stringify(content, null, 2)
    } catch {
      return String(content)
    }
  }
  return String(content)
}

function formatTs(ts: string): string {
  return new Date(ts).toLocaleString(undefined, {
    month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit',
  })
}

function formatDuration(ms: number): string {
  if (ms < 1000) return `${ms}ms`
  return `${(ms / 1000).toFixed(1)}s`
}

// Very rough cost estimate: $2 per 1M input tokens, $8 per 1M output tokens (gpt-4o-ish)
function estimateCost(inputTokens: number, outputTokens: number): string {
  const cost = (inputTokens / 1_000_000) * 2 + (outputTokens / 1_000_000) * 8
  return `~$${cost.toFixed(4)}`
}

// ─── Section wrapper ──────────────────────────────────────────────────────────

function Section({ title, children }: { title: string; children: ReactNode }) {
  return (
    <div className="rounded-sm border border-rule bg-surface-850 overflow-hidden">
      <div className="border-b border-rule px-4 py-3">
        <h3 className="text-sm font-semibold text-fg">{title}</h3>
      </div>
      <div className="p-4">{children}</div>
    </div>
  )
}

// ─── Main component ───────────────────────────────────────────────────────────

export default function WorkItemDetailPage() {
  const { id } = useParams<{ id: string }>()
  const navigate = useNavigate()
  const { get, post } = useApi()
  const [detail, setDetail] = useState<WorkItemDetail | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [expandedArtifacts, setExpandedArtifacts] = useState<Set<string>>(new Set())
  const [expandedTimeline, setExpandedTimeline] = useState<Set<string>>(new Set())
  const [toolEvents, setToolEvents] = useState<ToolCallEvent[]>([])
  // PRD approval state — inline reason fields and submission status.
  const [prdAction, setPrdAction] = useState<{
    pending: 'approve' | 'reject' | 'revise' | null
    submitting: boolean
    rejectReason: string
    reviseReason: string
    showReject: boolean
    showRevise: boolean
    result?: { success: boolean; message: string }
  }>({
    pending: null,
    submitting: false,
    rejectReason: '',
    reviseReason: '',
    showReject: false,
    showRevise: false,
  })
  const [activeTab, setActiveTab] = useState<'overview' | 'console' | 'code'>('overview')
  const { openChat } = useChat()
  const liveScrollRef = useRef<HTMLDivElement>(null)
  const { lastMessage, subscribe, unsubscribe } = useWebSocket()

  const fetchDetail = useCallback(async () => {
    if (!id) return
    try {
      const data = await get<WorkItemDetail>(`/work-items/detail?id=${id}`)
      setDetail(data)
      setError(null)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to load work item')
    } finally {
      setLoading(false)
    }
  }, [id, get])

  useEffect(() => {
    fetchDetail()
  }, [fetchDetail])

  // PRD approval handlers — shared wrapper so approve/reject/revise all route
  // through the same submission + re-fetch flow and share the same inline
  // result banner.
  const handlePrdAction = useCallback(
    async (action: 'approve' | 'reject' | 'revise', reason?: string) => {
      if (!id) return
      setPrdAction((s) => ({ ...s, pending: action, submitting: true, result: undefined }))
      const endpoint =
        action === 'approve' ? 'approve-prd' :
        action === 'reject'  ? 'reject-prd'  : 'revise-prd'
      const body =
        action === 'approve' ? undefined : { reason: reason ?? '' }
      try {
        await post(`/admin/work-items/${endpoint}?id=${id}`, body)
        const successMsg =
          action === 'approve' ? 'Approved — research dispatching' :
          action === 'reject'  ? 'PRD rejected' : 'Changes requested — product agent will revise'
        setPrdAction((s) => ({
          ...s,
          submitting: false,
          pending: null,
          showReject: false,
          showRevise: false,
          rejectReason: '',
          reviseReason: '',
          result: { success: true, message: successMsg },
        }))
        await fetchDetail()
      } catch (err) {
        setPrdAction((s) => ({
          ...s,
          submitting: false,
          pending: null,
          result: {
            success: false,
            message: err instanceof Error ? err.message : 'Action failed',
          },
        }))
      }
    },
    [id, post, fetchDetail],
  )

  // Auto-refresh at 3s cadence when the item is active OR has an in-flight
  // LLM call (latency_ms=0 running sentinel). 10s was too slow to feel live
  // — the user expects to see calls appear as they happen, not on the next
  // minute-mark. 3s keeps dashboard load trivial and in-flight calls show
  // up within a blink.
  useEffect(() => {
    if (!detail) return
    const terminal = TERMINAL_STATES.includes(detail.item.state)
    const hasRunningLLM = (detail.inference_logs ?? []).some(
      l => l.latency_ms === 0 && !l.success && !l.error
    )
    if (terminal && !hasRunningLLM) return

    const interval = setInterval(fetchDetail, 3_000)
    return () => clearInterval(interval)
  }, [detail, fetchDetail])

  // Load historical tool events from DB on mount.
  useEffect(() => {
    if (!id) return
    interface DBToolEvent {
      work_item_id: string
      task_id: string
      agent_type: string
      tool: string
      arguments: string
      result: string
      is_error: boolean
      timestamp: string
    }
    get<DBToolEvent[]>(`/work-items/tool-events?work_item_id=${id}`)
      .then(events => {
        if (events?.length) {
          setToolEvents(events.map(e => ({
            work_item_id: e.work_item_id,
            task_id: e.task_id,
            tool: e.tool,
            arguments: e.arguments,
            result_preview: e.result?.slice(0, 200) ?? '',
            is_error: e.is_error,
            timestamp: e.timestamp,
          })))
        }
      })
      .catch(() => {})
  }, [id, get])

  // Subscribe to the tool_calls WebSocket channel for real-time tool event streaming.
  useEffect(() => {
    subscribe('tool_calls')
    return () => unsubscribe('tool_calls')
  }, [subscribe, unsubscribe])

  // Filter incoming tool events to those belonging to the current work item.
  useEffect(() => {
    if (!lastMessage || lastMessage.channel !== 'tool_calls' || !id) return
    const event = lastMessage.data as ToolCallEvent
    if (event?.work_item_id !== id) return
    setToolEvents(prev => [...prev.slice(-99), event]) // keep last 100 events
  }, [lastMessage, id])

  // Auto-scroll the live activity feed to the bottom when new events arrive.
  useEffect(() => {
    if (liveScrollRef.current) {
      liveScrollRef.current.scrollTop = liveScrollRef.current.scrollHeight
    }
  }, [toolEvents])

  if (loading) {
    return (
      <div className="flex h-64 items-center justify-center">
        <div className="h-8 w-8 animate-spin rounded-full border-2 border-accent border-t-transparent" />
      </div>
    )
  }

  if (error || !detail) {
    return (
      <div className="flex flex-col items-center gap-4 py-16">
        <p className="text-sm text-red-400">{error ?? 'Work item not found'}</p>
        <button
          type="button"
          onClick={() => navigate(-1)}
          className="rounded bg-surface-700 px-3 py-1.5 text-sm text-muted hover:bg-surface-700 transition-colors"
        >
          Go back
        </button>
      </div>
    )
  }

  const { item, transitions, children, artifacts: allArtifacts, agent_definition, assignments, inference_logs, repository, product, evidence, base_shas, base_sha_events, contract_edges } = detail
  // Codex coherence Part 1: the pin and each re-plan / note / escalation.
  const codexEvents = detail.codex_events ?? []
  const latestCodexEvent = codexEvents.length > 0 ? codexEvents[codexEvents.length - 1] : undefined
  const codexNote = latestCodexEvent && (latestCodexEvent.kind === 'noted' || latestCodexEvent.kind === 'escalated')
    ? { kind: latestCodexEvent.kind, to_version: latestCodexEvent.to_version, modules: latestCodexEvent.modules, sections: latestCodexEvent.sections, reason: latestCodexEvent.reason, at: latestCodexEvent.created_at }
    : undefined
  // check_plan artifacts are shown under "Fast checks" (M3-04), not in the
  // generic artifact list and timeline.
  const artifacts = allArtifacts.filter((a) => !isCheckPlanArtifact(a.content))
  // Implementation controller runs (M3-02): phases exactly as recorded.
  const implementationRuns = detail.implementation_runs ?? []
  const runByTask = runsByTask(implementationRuns)
  const checkPlans = checkPlansByTask(allArtifacts)
  const childTitles = Object.fromEntries(children.map((c) => [c.id, c.title]))

  // A row is "running" if it was inserted by beginInference() but not yet
  // updated by completeInference() — the latency_ms=0 + success=false +
  // empty error sentinel. Running rows don't count as errors, don't count
  // tokens, and get rendered with a live elapsed clock.
  const isRunning = (l: typeof inference_logs[number]) =>
    l.latency_ms === 0 && !l.success && !l.error
  const totalInputTokens = inference_logs.reduce((s, l) => s + l.input_tokens, 0)
  const totalOutputTokens = inference_logs.reduce((s, l) => s + l.output_tokens, 0)
  const runningCount = inference_logs.filter(isRunning).length
  const errorCount = inference_logs.filter(l => !l.success && !isRunning(l)).length
  const totalTokens = totalInputTokens + totalOutputTokens

  const isActive = !TERMINAL_STATES.includes(item.state)

  function toggleArtifact(aid: string) {
    setExpandedArtifacts((prev) => {
      const next = new Set(prev)
      if (next.has(aid)) next.delete(aid)
      else next.add(aid)
      return next
    })
  }

  function toggleTimeline(tid: string) {
    setExpandedTimeline((prev) => {
      const next = new Set(prev)
      if (next.has(tid)) next.delete(tid)
      else next.add(tid)
      return next
    })
  }

  function renderContent(content: unknown): string {
    if (typeof content === 'string') return content
    try {
      return JSON.stringify(content, null, 2)
    } catch {
      return String(content)
    }
  }

  // ── Merge all five event sources into a single reverse-chronological stream.
  // We key each entry by "<kind>:<id>" so duplicate IDs across kinds are safe.
  // Missing/invalid timestamps are treated as epoch 0 so they sort to the bottom
  // instead of crashing Date.parse downstream.
  type TimelineEntry =
    | { kind: 'transition'; key: string; ts: number; data: typeof transitions[number] }
    | { kind: 'llm'; key: string; ts: number; data: typeof inference_logs[number] }
    | { kind: 'tool'; key: string; ts: number; data: ToolCallEvent }
    | { kind: 'artifact'; key: string; ts: number; data: typeof artifacts[number] }
    | { kind: 'assignment'; key: string; ts: number; data: typeof assignments[number] }
    | { kind: 'codex'; key: string; ts: number; data: typeof codexEvents[number] }

  function safeTs(v: string | undefined): number {
    if (!v) return 0
    const n = new Date(v).getTime()
    return Number.isFinite(n) ? n : 0
  }

  const timelineEntries: TimelineEntry[] = [
    ...transitions.map<TimelineEntry>((t) => ({
      kind: 'transition', key: `transition:${t.id}`, ts: safeTs(t.timestamp), data: t,
    })),
    ...inference_logs.map<TimelineEntry>((l) => ({
      kind: 'llm', key: `llm:${l.id}`, ts: safeTs(l.created_at), data: l,
    })),
    ...toolEvents.map<TimelineEntry>((e, i) => ({
      kind: 'tool', key: `tool:${e.timestamp}:${e.tool}:${i}`, ts: safeTs(e.timestamp), data: e,
    })),
    ...artifacts.map<TimelineEntry>((a) => ({
      kind: 'artifact', key: `artifact:${a.id}`, ts: safeTs(a.created_at), data: a,
    })),
    ...assignments.map<TimelineEntry>((a) => ({
      kind: 'assignment', key: `assignment:${a.id}`, ts: safeTs(a.assigned_at), data: a,
    })),
    ...codexEvents.map<TimelineEntry>((e) => ({
      kind: 'codex', key: `codex:${e.id}`, ts: safeTs(e.created_at), data: e,
    })),
  ].sort((a, b) => b.ts - a.ts)

  function timeLabel(ts: number): string {
    if (!ts) return '—'
    return new Date(ts).toLocaleTimeString(undefined, {
      hour: '2-digit', minute: '2-digit', second: '2-digit',
    })
  }

  function kindBadgeClass(kind: TimelineEntry['kind']): string {
    switch (kind) {
      case 'transition': return 'bg-surface-700/40 text-muted border border-sharp/40'
      case 'llm':        return 'bg-accent/10 text-accent border border-accent/30'
      case 'tool':       return 'bg-amber-500/20 text-amber-300 border border-amber-500/30'
      case 'artifact':   return 'bg-emerald-500/20 text-emerald-300 border border-emerald-500/30'
      case 'assignment': return 'bg-cyan-500/20 text-cyan-300 border border-cyan-500/30'
      case 'codex':      return 'bg-sky-500/20 text-sky-300 border border-sky-500/30'
    }
  }

  return (
    <div className="flex flex-col gap-6 pb-10">
      {/* Back nav */}
      <div className="flex items-center gap-2">
        <button
          type="button"
          onClick={() => navigate(-1)}
          className="flex items-center gap-1.5 text-xs text-dim hover:text-muted transition-colors"
        >
          <svg className="h-3.5 w-3.5" fill="none" stroke="currentColor" strokeWidth={2} viewBox="0 0 24 24">
            <path strokeLinecap="round" strokeLinejoin="round" d="M15 19l-7-7 7-7" />
          </svg>
          Pipeline Board
        </button>
      </div>

      {/* ── Header ── */}
      <div className="rounded-sm border border-rule bg-surface-850 p-5">
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div className="flex-1 min-w-0">
            <h1 className="text-xl font-semibold text-fg leading-snug">{item.title}</h1>
            {item.description && (
              <p className="mt-1.5 text-sm text-muted leading-relaxed">{item.description}</p>
            )}
          </div>
          {/* Auto-refresh pulse */}
          <div className="flex items-center gap-3 shrink-0">
            <button
              type="button"
              onClick={() => openChat({ workItemId: item.id, workItemTitle: item.title })}
              className="flex items-center gap-1.5 rounded-sm bg-accent/10 border border-accent/30 px-3 py-1.5 text-xs font-medium text-accent hover:bg-accent/20 transition-colors"
            >
              <svg className="h-3.5 w-3.5" fill="none" stroke="currentColor" strokeWidth={2} viewBox="0 0 24 24">
                <path strokeLinecap="round" strokeLinejoin="round" d="M8 12h.01M12 12h.01M16 12h.01M21 12c0 4.418-4.03 8-9 8a9.863 9.863 0 01-4.255-.949L3 20l1.395-3.72C3.512 15.042 3 13.574 3 12c0-4.418 4.03-8 9-8s9 3.582 9 8z" />
              </svg>
              Chat
            </button>
            {isActive && (
              <>
                <span className="relative flex h-2 w-2">
                  <span className="animate-ping absolute inline-flex h-full w-full rounded-full bg-emerald-400 opacity-75" />
                  <span className="relative inline-flex rounded-full h-2 w-2 bg-emerald-500" />
                </span>
                <span className="text-xs text-dim">Live</span>
              </>
            )}
          </div>
        </div>

        {/* Badge row */}
        <div className="mt-3 flex flex-wrap gap-2">
          <span className={`rounded px-2 py-0.5 text-xs font-semibold ${stateBadgeClass(item.state)}`}>
            {item.state.replace('_', ' ')}
          </span>
          <span className={`rounded px-1.5 py-0.5 text-xs font-medium ${typeBadgeClass(item.type)}`}>
            {item.type}
          </span>
          <span className={`rounded px-1.5 py-0.5 text-xs font-medium ${priorityBadgeClass(item.priority)}`}>
            {item.priority}
          </span>
          {item.external_ref && (
            <span className="rounded bg-surface-700 px-1.5 py-0.5 text-xs text-muted">
              {item.external_ref}
            </span>
          )}
          {product && (
            <span className="rounded bg-accent/10 px-1.5 py-0.5 text-xs text-accent border border-indigo-700/40">
              {product.name}
            </span>
          )}
          {repository && (
            <a
              href={repository.github_url}
              target="_blank"
              rel="noopener noreferrer"
              className="rounded bg-surface-700/40 px-1.5 py-0.5 text-xs text-muted border border-rule/40 hover:bg-surface-700 transition-colors"
            >
              {repository.name}
            </a>
          )}
          {agent_definition && (
            <span className="rounded bg-purple-700/30 px-1.5 py-0.5 text-xs text-purple-300 border border-purple-700/40">
              {agent_definition.name} · {agent_definition.provider}/{agent_definition.model_id}
            </span>
          )}
          <ConfigSnapshotBadge snapshotId={detail.config_snapshot_id} outcome={detail.outcome} />
        </div>

        {/* Meta row */}
        <div className="mt-3 flex flex-wrap gap-x-5 gap-y-1 text-xs text-dim">
          <span>Created: <span className="text-muted">{formatTs(item.created_at)}</span></span>
          <span>Updated: <span className="text-muted">{formatTs(item.updated_at)}</span></span>
          {detail.codex_pin || codexNote ? (
            <CodexPinBadge pin={detail.codex_pin} note={codexNote} />
          ) : (
            <span>Codex v<span className="text-muted">{item.codex_version}</span></span>
          )}
          {item.parent_id && (
            <span>
              Parent:{' '}
              <Link to={`/work-items/${item.parent_id}`} className="text-accent-dim hover:text-accent transition-colors">
                {item.parent_id.slice(0, 8)}…
              </Link>
            </span>
          )}
        </div>
      </div>

      {/* ── Tab bar — shown for all active work items ── */}
      {isActive && (
        <div className="flex gap-0 border-b border-rule">
          {(['overview', 'console', ...(item.state === 'human_review' ? ['code' as const] : [])] as const).map((tab) => (
            <button
              key={tab}
              type="button"
              onClick={() => setActiveTab(tab)}
              className={[
                'px-5 py-2.5 text-sm font-medium transition-colors capitalize',
                activeTab === tab
                  ? 'text-accent border-b-2 border-accent -mb-px'
                  : 'text-muted hover:text-fg',
              ].join(' ')}
            >
              {tab === 'code' ? 'Code Changes' : tab === 'console' ? 'Console' : 'Overview'}
            </button>
          ))}
        </div>
      )}

      {/* ── Code tab — diff view for human_review ── */}
      {item.state === 'human_review' && activeTab === 'code' && (
        <HumanReviewCodeTab workItemId={item.id} onApproved={fetchDetail} />
      )}

      {/* ── Console tab — full-fidelity agent activity stream ── */}
      {activeTab === 'console' && (
        <ConsoleView workItemId={item.id} />
      )}

      {/* ── Overview tab ── */}
      {activeTab === 'overview' && <>

      {/* ── Action Required ── shown when the clarity check or some other guard blocked the item */}
      {item.state === 'blocked' && (
        <BlockedActionRequired
          workItemId={item.id}
          description={item.description}
          blockReason={transitions[transitions.length - 1]?.reason ?? ''}
          onRefetch={fetchDetail}
        />
      )}

      {/* ── Independent verification ── the verifier's per-criterion verdict, gaps and defects (M3-07). R14: it runs
          before any PR exists, so it is shown ahead of the PR gate card below — the operator approves (or not) with
          the verdict in front of them. The second-pass review stays under Artifacts. */}
      {(() => {
        const verdicts = artifacts.filter((a) => a.artifact_type === 'review_comments' && isVerifierArtifact(a.content))
        if (verdicts.length === 0) return null
        const latest = verdicts.reduce((a, b) => (safeTs(b.created_at) > safeTs(a.created_at) ? b : a))
        const data = verifierVerdictFrom(latest.content)
        if (!data) return null
        return (
          <Section title={data.taskId ? 'Independent verification (before the PR)' : 'Independent verification'}>
            <VerifierVerdict data={data} />
          </Section>
        )
      })()}

      {/* ── Awaiting User ── an agent called request_user_input or a heuristic escalated.
          The prompt appears inline here AND in the header bell/sheet. Answering
          either surface advances the work item; the state machine restores
          previous_state once the answer is persisted. */}
      {item.state === 'awaiting_user' && item.awaiting_prompt_id && (
        <AwaitingUserPrompt
          promptId={item.awaiting_prompt_id}
          onAnswered={fetchDetail}
        />
      )}

      {/* ── PRD Awaiting Approval ── human-in-the-loop gate between PRD generation and research dispatch */}
      {item.state === 'prd_approval' && (() => {
        const prdArtifact = artifacts.find((a) => a.artifact_type === 'prd')
        const prdText = prdArtifact ? extractPrdContent(prdArtifact.content) : ''
        return (
          <Section title="PRD Awaiting Approval">
            <div className="flex flex-col gap-4">
              {/* Amber alert explaining the gate */}
              <div className="rounded-sm border-2 border-amber-500/60 bg-amber-500/10 px-4 py-3">
                <div className="flex items-start gap-2">
                  <svg className="h-5 w-5 shrink-0 text-amber-400 mt-0.5" fill="none" stroke="currentColor" strokeWidth={2} viewBox="0 0 24 24">
                    <path strokeLinecap="round" strokeLinejoin="round" d="M12 9v2m0 4h.01m-6.938 4h13.856c1.54 0 2.502-1.667 1.732-3L13.732 4c-.77-1.333-2.694-1.333-3.464 0L3.34 16c-.77 1.333.192 3 1.732 3z" />
                  </svg>
                  <div className="flex-1 min-w-0">
                    <p className="text-sm font-semibold text-amber-200">Human review required before research dispatch.</p>
                    <p className="mt-1 text-xs text-amber-200/80 leading-relaxed">
                      This gate prevents hallucinated or off-target PRDs from consuming research cycles. Approve to publish <code className="font-mono text-amber-100">prd.ready</code> and begin research, reject to terminate, or request changes to send the PRD back to the product agent for revision.
                    </p>
                  </div>
                </div>
              </div>

              {/* PRD content */}
              <div>
                <div className="mb-2 flex items-center gap-2">
                  <span className="rounded bg-violet-500/20 px-1.5 py-0.5 text-xs font-medium text-violet-300 border border-violet-500/30">PRD</span>
                  {prdArtifact?.agent_id && (
                    <span className="text-xs text-dim">by {prdArtifact.agent_id} · {formatTs(prdArtifact.created_at)}</span>
                  )}
                </div>
                {prdText ? (
                  <div className="rounded-sm border border-rule bg-surface-900 px-4 py-3 max-h-[32rem] overflow-y-auto">
                    <Markdown>{prdText}</Markdown>
                  </div>
                ) : (
                  <p className="text-sm italic text-dim">No PRD artifact found. The product agent may still be generating it.</p>
                )}
              </div>

              {/* Result banner */}
              {prdAction.result && (
                <div
                  className={[
                    'rounded px-3 py-2 text-xs',
                    prdAction.result.success
                      ? 'bg-emerald-500/20 text-emerald-300 border border-emerald-500/30'
                      : 'bg-red-500/20 text-red-300 border border-red-500/30',
                  ].join(' ')}
                >
                  {prdAction.result.message}
                </div>
              )}

              {/* Action buttons */}
              <div className="flex flex-wrap gap-2">
                <button
                  type="button"
                  disabled={prdAction.submitting}
                  onClick={() => handlePrdAction('approve')}
                  className="flex-1 min-w-[10rem] rounded-sm bg-emerald-600 px-4 py-2 text-sm font-semibold text-white transition-colors hover:bg-emerald-500 disabled:cursor-not-allowed disabled:opacity-50"
                >
                  {prdAction.pending === 'approve' ? 'Approving…' : 'Approve & Dispatch Research'}
                </button>
                <button
                  type="button"
                  disabled={prdAction.submitting}
                  onClick={() => setPrdAction((s) => ({ ...s, showRevise: !s.showRevise, showReject: false }))}
                  className="flex-1 min-w-[10rem] rounded-sm bg-surface-700 px-4 py-2 text-sm font-semibold text-fg transition-colors hover:bg-surface-700 disabled:cursor-not-allowed disabled:opacity-50"
                >
                  Request Changes
                </button>
                <button
                  type="button"
                  disabled={prdAction.submitting}
                  onClick={() => setPrdAction((s) => ({ ...s, showReject: !s.showReject, showRevise: false }))}
                  className="flex-1 min-w-[10rem] rounded-sm bg-red-700 px-4 py-2 text-sm font-semibold text-white transition-colors hover:bg-red-600 disabled:cursor-not-allowed disabled:opacity-50"
                >
                  Reject
                </button>
              </div>

              {/* Inline revise reason field */}
              {prdAction.showRevise && (
                <div className="rounded-sm border border-rule bg-surface-900 p-3">
                  <label className="mb-2 block text-xs font-medium text-muted">
                    What should the product agent change? (optional)
                  </label>
                  <textarea
                    className="w-full rounded-sm border border-rule bg-surface-950 px-3 py-2 text-sm text-fg placeholder-dim focus:border-accent focus:outline-none focus:ring-1 focus:ring-accent/30 resize-none"
                    rows={3}
                    placeholder="e.g. Scope is too broad — split into two tickets. Focus only on the auth flow."
                    value={prdAction.reviseReason}
                    onChange={(e) => setPrdAction((s) => ({ ...s, reviseReason: e.target.value }))}
                    disabled={prdAction.submitting}
                  />
                  <button
                    type="button"
                    disabled={prdAction.submitting}
                    onClick={() => handlePrdAction('revise', prdAction.reviseReason)}
                    className="mt-2 rounded-sm bg-surface-700 px-3 py-1.5 text-xs font-semibold text-fg transition-colors hover:bg-dim disabled:cursor-not-allowed disabled:opacity-50"
                  >
                    {prdAction.pending === 'revise' ? 'Submitting…' : 'Submit Revision Request'}
                  </button>
                </div>
              )}

              {/* Inline reject reason field */}
              {prdAction.showReject && (
                <div className="rounded-sm border border-red-700/50 bg-red-950/10 p-3">
                  <label className="mb-2 block text-xs font-medium text-red-300">
                    Reason for rejection (optional)
                  </label>
                  <textarea
                    className="w-full rounded-sm border border-rule bg-surface-950 px-3 py-2 text-sm text-fg placeholder-dim focus:border-red-500 focus:outline-none focus:ring-1 focus:ring-red-500/50 resize-none"
                    rows={3}
                    placeholder="e.g. Hallucinated requirements, not aligned with product direction."
                    value={prdAction.rejectReason}
                    onChange={(e) => setPrdAction((s) => ({ ...s, rejectReason: e.target.value }))}
                    disabled={prdAction.submitting}
                  />
                  <button
                    type="button"
                    disabled={prdAction.submitting}
                    onClick={() => handlePrdAction('reject', prdAction.rejectReason)}
                    className="mt-2 rounded-sm bg-red-600 px-3 py-1.5 text-xs font-semibold text-white transition-colors hover:bg-red-500 disabled:cursor-not-allowed disabled:opacity-50"
                  >
                    {prdAction.pending === 'reject' ? 'Rejecting…' : 'Confirm Reject'}
                  </button>
                </div>
              )}
            </div>
          </Section>
        )
      })()}

      {/* ── Activity Timeline ── unified reverse-chron stream of every event on the ticket */}
      <Section title={`Activity Timeline (${timelineEntries.length})`}>
        {timelineEntries.length === 0 ? (
          <p className="text-xs text-dim italic">No activity yet.</p>
        ) : (
          <div className="flex flex-col gap-1.5 max-h-[36rem] overflow-y-auto pr-1">
            {timelineEntries.map((entry) => {
              const isOpen = expandedTimeline.has(entry.key)

              // ── Transition: one-liner, no expansion ──
              if (entry.kind === 'transition') {
                const t = entry.data
                return (
                  <div
                    key={entry.key}
                    className="rounded-sm border border-rule/50 bg-surface-900 px-3 py-2 text-xs"
                  >
                    <div className="flex items-center gap-2 flex-wrap">
                      <span className="font-mono text-dim whitespace-nowrap">{timeLabel(entry.ts)}</span>
                      <span className={`rounded px-1.5 py-0.5 font-medium uppercase tracking-wide text-[10px] ${kindBadgeClass(entry.kind)}`}>
                        transition
                      </span>
                      <span className={`rounded px-1.5 py-0.5 text-xs font-medium ${stateBadgeClass(t.from_state)}`}>
                        {t.from_state.replace('_', ' ')}
                      </span>
                      <svg className="h-3 w-3 text-dim" fill="none" stroke="currentColor" strokeWidth={2} viewBox="0 0 24 24">
                        <path strokeLinecap="round" strokeLinejoin="round" d="M9 5l7 7-7 7" />
                      </svg>
                      <span className={`rounded px-1.5 py-0.5 text-xs font-semibold ${stateBadgeClass(t.to_state)}`}>
                        {t.to_state.replace('_', ' ')}
                      </span>
                      {t.agent_id && <span className="text-dim">({t.agent_id})</span>}
                      {t.reason && <span className="text-dim italic truncate">· {t.reason}</span>}
                    </div>
                  </div>
                )
              }

              // ── Codex change: re-plan, note, escalation or continue ──
              if (entry.kind === 'codex') {
                return (
                  <div
                    key={entry.key}
                    className="rounded-sm border border-rule/50 bg-surface-900 px-3 py-2 text-xs"
                  >
                    <div className="flex items-center gap-2 flex-wrap">
                      <span className="font-mono text-dim whitespace-nowrap">{timeLabel(entry.ts)}</span>
                      <span className={`rounded px-1.5 py-0.5 font-medium uppercase tracking-wide text-[10px] ${kindBadgeClass(entry.kind)}`}>
                        codex
                      </span>
                      <CodexEventLine event={entry.data} />
                    </div>
                  </div>
                )
              }

              // ── Assignment: one-liner, no expansion ──
              if (entry.kind === 'assignment') {
                const a = entry.data
                return (
                  <div
                    key={entry.key}
                    className="rounded-sm border border-rule/50 bg-surface-900 px-3 py-2 text-xs"
                  >
                    <div className="flex items-center gap-2 flex-wrap">
                      <span className="font-mono text-dim whitespace-nowrap">{timeLabel(entry.ts)}</span>
                      <span className={`rounded px-1.5 py-0.5 font-medium uppercase tracking-wide text-[10px] ${kindBadgeClass(entry.kind)}`}>
                        assignment
                      </span>
                      <span className="text-muted">{a.agent_type}</span>
                      <span className="font-mono text-dim">{a.instance_id}</span>
                      <span className={[
                        'rounded px-1.5 py-0.5 text-xs font-medium',
                        a.status === 'active'    ? 'bg-emerald-500/20 text-emerald-300 border border-emerald-500/30' :
                        a.status === 'completed' ? 'bg-teal-500/20 text-teal-300 border border-teal-500/30' :
                        a.status === 'failed'    ? 'bg-red-500/20 text-red-300 border border-red-500/30' :
                                                   'bg-surface-700/40 text-muted border border-sharp/30',
                      ].join(' ')}>
                        {a.status}
                      </span>
                      {a.completed_at && (
                        <span className="text-dim">· done {timeLabel(safeTs(a.completed_at))}</span>
                      )}
                    </div>
                  </div>
                )
              }

              // ── LLM: expandable conversation ──
              if (entry.kind === 'llm') {
                const log = entry.data
                const running = isRunning(log)
                const messages = log.prompt_messages ?? []
                const response = log.response_content ?? ''
                const hasConversation = messages.length > 0 || response.length > 0
                const totalTok = log.input_tokens + log.output_tokens
                return (
                  <div
                    key={entry.key}
                    className={[
                      'rounded-sm border',
                      running
                        ? 'border-indigo-700/60 bg-indigo-950/10'
                        : log.success
                          ? 'border-rule/50 bg-surface-900'
                          : 'border-red-700/50 bg-red-950/10',
                    ].join(' ')}
                  >
                    <button
                      type="button"
                      onClick={() => !running && toggleTimeline(entry.key)}
                      disabled={running}
                      className="w-full flex items-center gap-2 flex-wrap px-3 py-2 text-left text-xs hover:bg-surface-800/30 disabled:cursor-default disabled:hover:bg-transparent"
                      title={running ? 'Conversation available once the call completes' : isOpen ? 'Hide conversation' : 'Show conversation'}
                    >
                      <span className="inline-block w-3 text-dim">{running ? ' ' : isOpen ? '▾' : '▸'}</span>
                      <span className="font-mono text-dim whitespace-nowrap">{timeLabel(entry.ts)}</span>
                      <span className={`rounded px-1.5 py-0.5 font-medium uppercase tracking-wide text-[10px] ${kindBadgeClass(entry.kind)}`}>
                        LLM
                      </span>
                      <span className="text-muted">{log.intent}</span>
                      <span className="font-mono text-dim">{log.model}</span>
                      {running ? (
                        <span className="inline-flex items-center gap-1.5 text-accent">
                          <span className="inline-block h-2 w-2 rounded-full bg-indigo-400 animate-pulse" />
                          <LiveElapsed since={log.created_at} />
                        </span>
                      ) : (
                        <>
                          <span className="text-dim">{formatDuration(log.latency_ms)}</span>
                          <span className="text-dim">· {totalTok.toLocaleString()} tokens</span>
                          {log.success ? (
                            <span className="text-emerald-400">ok</span>
                          ) : (
                            <span className="text-red-400" title={log.error}>error</span>
                          )}
                        </>
                      )}
                    </button>
                    {isOpen && !running && (
                      <div className="px-4 py-3 bg-surface-950/80 border-t border-rule/30">
                        {log.error && (
                          <p className="mb-2 text-red-400/90 text-xs leading-tight">{log.error}</p>
                        )}
                        {!hasConversation ? (
                          <p className="italic text-dim text-xs">No conversation captured for this row (older call, or pre-transparency).</p>
                        ) : (
                          <div className="space-y-3">
                            {messages.map((m, i) => (
                              <ChatBubble key={i} role={m.role} content={m.content} name={m.name} />
                            ))}
                            {response && <ChatBubble role="assistant" content={response} label="response" />}
                          </div>
                        )}
                      </div>
                    )}
                  </div>
                )
              }

              // ── Artifact: expandable JSON content ──
              if (entry.kind === 'artifact') {
                const a = entry.data
                return (
                  <div
                    key={entry.key}
                    className="rounded-sm border border-rule/50 bg-surface-900"
                  >
                    <button
                      type="button"
                      onClick={() => toggleTimeline(entry.key)}
                      className="w-full flex items-center gap-2 flex-wrap px-3 py-2 text-left text-xs hover:bg-surface-800/30"
                    >
                      <span className="inline-block w-3 text-dim">{isOpen ? '▾' : '▸'}</span>
                      <span className="font-mono text-dim whitespace-nowrap">{timeLabel(entry.ts)}</span>
                      <span className={`rounded px-1.5 py-0.5 font-medium uppercase tracking-wide text-[10px] ${kindBadgeClass(entry.kind)}`}>
                        artifact
                      </span>
                      <span className={`rounded px-1.5 py-0.5 text-xs font-medium ${artifactTypeBadgeClass(a.artifact_type)}`}>
                        {artifactTypeLabel(a.artifact_type)}
                      </span>
                      {a.agent_id && <span className="text-dim">by {a.agent_id}</span>}
                    </button>
                    {isOpen && (
                      <div className="px-4 py-3 bg-surface-950/80 border-t border-rule/30">
                        <pre className="text-xs text-muted whitespace-pre-wrap break-words leading-relaxed max-h-96 overflow-y-auto">
                          {renderContent(a.content)}
                        </pre>
                      </div>
                    )}
                  </div>
                )
              }

              // ── Tool call: expandable arguments + result ──
              if (entry.kind === 'tool') {
                const ev = entry.data
                return (
                  <div
                    key={entry.key}
                    className={[
                      'rounded-sm border',
                      ev.is_error
                        ? 'border-red-700/50 bg-red-950/10'
                        : 'border-rule/50 bg-surface-900',
                    ].join(' ')}
                  >
                    <button
                      type="button"
                      onClick={() => toggleTimeline(entry.key)}
                      className="w-full flex items-center gap-2 flex-wrap px-3 py-2 text-left text-xs hover:bg-surface-800/30"
                    >
                      <span className="inline-block w-3 text-dim">{isOpen ? '▾' : '▸'}</span>
                      <span className="font-mono text-dim whitespace-nowrap">{timeLabel(entry.ts)}</span>
                      <span className={`rounded px-1.5 py-0.5 font-medium uppercase tracking-wide text-[10px] ${kindBadgeClass(entry.kind)}`}>
                        tool
                      </span>
                      <span className={[
                        'rounded px-1.5 py-0.5 font-mono font-medium',
                        ev.is_error ? 'bg-red-500/20 text-red-300' : 'bg-amber-500/20 text-amber-300',
                      ].join(' ')}>
                        {ev.tool}
                      </span>
                      {ev.is_error && <span className="text-red-400">error</span>}
                      {ev.result_preview && (
                        <span className={[
                          'truncate max-w-lg',
                          ev.is_error ? 'text-red-400' : 'text-dim',
                        ].join(' ')} title={ev.result_preview}>
                          {ev.result_preview.length > 80 ? ev.result_preview.slice(0, 80) + '…' : ev.result_preview}
                        </span>
                      )}
                    </button>
                    {isOpen && (
                      <div className="px-4 py-3 bg-surface-950/80 border-t border-rule/30 space-y-3">
                        {ev.arguments && ev.arguments !== '{}' && (
                          <div>
                            <div className="mb-1 text-[10px] font-mono uppercase tracking-wider text-dim">arguments</div>
                            <pre className="text-xs text-muted whitespace-pre-wrap break-words leading-relaxed max-h-64 overflow-y-auto">
                              {ev.arguments}
                            </pre>
                          </div>
                        )}
                        {ev.result_preview && (
                          <div>
                            <div className="mb-1 text-[10px] font-mono uppercase tracking-wider text-dim">result</div>
                            <pre className={[
                              'text-xs whitespace-pre-wrap break-words leading-relaxed max-h-64 overflow-y-auto',
                              ev.is_error ? 'text-red-300' : 'text-muted',
                            ].join(' ')}>
                              {ev.result_preview}
                            </pre>
                          </div>
                        )}
                      </div>
                    )}
                  </div>
                )
              }

              return null
            })}
          </div>
        )}
      </Section>

      {/* ── Implementation runs ── controller phase per subtask from recorded tool evidence (M3-02) */}
      {implementationRuns.length > 0 && (
        <Section title="Implementation phase">
          <ImplementationRuns runs={implementationRuns} />
        </Section>
      )}
      {/* ── Acceptance criteria ── the technical brief's criteria and file evidence map (M2-04) */}
      {(() => {
        // The plan-validation record (M2-05) reuses the technical_brief type; it is not a brief.
        const briefs = artifacts.filter((a) => a.artifact_type === 'technical_brief' && !isPlanValidationArtifact(a.content))
        if (briefs.length === 0) return null
        const latest = briefs.reduce((a, b) => (safeTs(b.created_at) > safeTs(a.created_at) ? b : a))
        return (
          <Section title="Acceptance criteria">
            <BriefCriteria data={briefCriteriaFrom(latest.content)} />
          </Section>
        )
      })()}
      {/* ── Plan validation ── violations of a plan rejected after the repair round (M2-05) */}
      {(() => {
        const records = artifacts.filter((a) => a.artifact_type === 'technical_brief' && isPlanValidationArtifact(a.content))
        if (records.length === 0) return null
        const latest = records.reduce((a, b) => (safeTs(b.created_at) > safeTs(a.created_at) ? b : a))
        const data = planValidationFrom(latest.content)
        if (!data) return null
        // Stale once a newer brief or a subtask exists (a retry planned again).
        const later = [
          ...artifacts.filter((a) => a.artifact_type === 'technical_brief' && !isPlanValidationArtifact(a.content)).map((a) => a.created_at),
          ...children.map((c) => c.created_at),
        ]
        if (!isCurrentPlanValidation(latest.created_at, later)) return null
        return (
          <Section title="Plan validation">
            <PlanValidation data={data} recordedAt={latest.created_at} />
          </Section>
        )
      })()}
      {/* ── Base snapshot ── pinned base commit per repository and its audit trail (M2-02) */}
      <Section title="Base snapshot">
        <BaseSnapshot pins={base_shas} events={base_sha_events} />
      </Section>

      {/* ── Cross-repository contracts ── producer → consumer edges and their last PR-time check (M2-06) */}
      {(contract_edges?.length ?? 0) > 0 && (
        <Section title="Cross-repository contracts">
          <ContractEdges edges={contract_edges} />
        </Section>
      )}

      {/* ── Fast checks ── impact-selected pre-commit checks per subtask; the gate runs the full suite (M3-04) */}
      {Object.keys(checkPlans).length > 0 && (
        <Section title="Fast checks">
          <CheckPlans plans={checkPlans} titles={childTitles} />
        </Section>
      )}

      {/* ── Verification evidence ── exact SHAs, checks that ran, skipped/partial states (M0-08) */}
      <Section title="Verification evidence">
        <VerificationEvidence verification={evidence} />
      </Section>

      {/* ── Timeline ── */}
      <Section title={`State Timeline (${transitions.length})`}>
        {transitions.length === 0 ? (
          <p className="text-xs text-dim italic">No transitions recorded.</p>
        ) : (
          <ol className="relative ml-2 border-l border-rule max-h-[24rem] overflow-y-auto pr-1">
            {[...transitions].reverse().map((t) => (
              <li key={t.id} className="mb-5 ml-5 last:mb-0">
                <span className={`absolute -left-1.5 mt-1 flex h-3 w-3 items-center justify-center rounded-full ring-2 ring-surface-850 ${stateDotColor(t.to_state)}`} />
                <div className="flex flex-wrap items-baseline gap-x-2 gap-y-0.5">
                  <span className="text-xs text-dim">{formatTs(t.timestamp)}</span>
                  <span className={`rounded px-1.5 py-0.5 text-xs font-medium ${stateBadgeClass(t.from_state)}`}>
                    {t.from_state.replace('_', ' ')}
                  </span>
                  <svg className="h-3 w-3 text-dim inline" fill="none" stroke="currentColor" strokeWidth={2} viewBox="0 0 24 24">
                    <path strokeLinecap="round" strokeLinejoin="round" d="M9 5l7 7-7 7" />
                  </svg>
                  <span className={`rounded px-1.5 py-0.5 text-xs font-semibold ${stateBadgeClass(t.to_state)}`}>
                    {t.to_state.replace('_', ' ')}
                  </span>
                </div>
                <div className="mt-1 flex flex-wrap gap-x-3 text-xs text-dim">
                  {t.agent_id && <span>by <span className="text-muted">{t.agent_id}</span></span>}
                  {t.reason && <span className="text-muted italic">{t.reason}</span>}
                </div>
              </li>
            ))}
          </ol>
        )}
      </Section>

      {/* ── Artifacts ── */}
      <Section title={`Artifacts (${artifacts.length})`}>
        {artifacts.length === 0 ? (
          <p className="text-xs text-dim italic">No artifacts yet.</p>
        ) : (
          <div className="flex flex-col gap-3 max-h-[36rem] overflow-y-auto pr-1">
            {artifacts.map((a) => {
              const isOpen = expandedArtifacts.has(a.id)
              return (
                <div key={a.id} className="rounded-sm border border-rule overflow-hidden">
                  <button
                    type="button"
                    onClick={() => toggleArtifact(a.id)}
                    className="w-full flex items-center justify-between gap-3 px-3 py-2.5 bg-surface-800 text-left hover:bg-surface-750 transition-colors"
                  >
                    <div className="flex items-center gap-2 min-w-0">
                      <span className={`shrink-0 rounded px-1.5 py-0.5 text-xs font-medium ${artifactTypeBadgeClass(a.artifact_type)}`}>
                        {artifactTypeLabel(a.artifact_type)}
                      </span>
                      <span className="text-xs text-dim truncate">
                        {a.agent_id && <span className="text-muted">by {a.agent_id} · </span>}
                        {formatTs(a.created_at)}
                      </span>
                    </div>
                    <svg
                      className={`h-4 w-4 text-dim shrink-0 transition-transform ${isOpen ? 'rotate-180' : ''}`}
                      fill="none" stroke="currentColor" strokeWidth={2} viewBox="0 0 24 24"
                    >
                      <path strokeLinecap="round" strokeLinejoin="round" d="M19 9l-7 7-7-7" />
                    </svg>
                  </button>
                  {isOpen && (
                    <div className="px-3 py-3 bg-surface-900 border-t border-rule">
                      <pre className="text-xs text-muted whitespace-pre-wrap break-words leading-relaxed max-h-96 overflow-y-auto">
                        {renderContent(a.content)}
                      </pre>
                    </div>
                  )}
                </div>
              )
            })}
          </div>
        )}
      </Section>

      {/* ── Sub-tasks ── */}
      {children.length > 0 && (
        <Section title={`Sub-tasks (${children.length})`}>
          <div className="overflow-x-auto">
            <table className="w-full text-xs">
              <thead>
                <tr className="border-b border-rule text-dim text-left">
                  <th className="pb-2 pr-4 font-medium">Title</th>
                  <th className="pb-2 pr-4 font-medium">Type</th>
                  <th className="pb-2 pr-4 font-medium">State</th>
                  <th className="pb-2 pr-4 font-medium">Phase</th>
                  <th className="pb-2 font-medium">Priority</th>
                </tr>
              </thead>
              <tbody>
                {children.map((child) => (
                  <tr key={child.id} className="border-b border-rule/30 last:border-0">
                    <td className="py-2 pr-4">
                      <Link
                        to={`/work-items/${child.id}`}
                        className="text-fg hover:text-accent transition-colors"
                      >
                        {child.title}
                      </Link>
                    </td>
                    <td className="py-2 pr-4">
                      <span className={`rounded px-1.5 py-0.5 text-xs font-medium ${typeBadgeClass(child.type)}`}>
                        {child.type}
                      </span>
                    </td>
                    <td className="py-2 pr-4">
                      <span className={`rounded px-1.5 py-0.5 text-xs font-medium ${stateBadgeClass(child.state)}`}>
                        {child.state.replace('_', ' ')}
                      </span>
                    </td>
                    <td className="py-2 pr-4">
                      <SubtaskPhase run={runByTask[child.id]} />
                    </td>
                    <td className="py-2">
                      <span className={`rounded px-1.5 py-0.5 text-xs font-medium ${priorityBadgeClass(child.priority)}`}>
                        {child.priority}
                      </span>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </Section>
      )}

      {/* ── LLM Activity ── */}
      <Section title={`LLM Activity (${inference_logs.length})`}>
        {inference_logs.length === 0 ? (
          <p className="text-xs text-dim italic">No inference logs yet.</p>
        ) : (
          <>
            {/* Summary row */}
            <div className="mb-4 flex flex-wrap gap-4 rounded bg-surface-900 px-4 py-3 text-xs border border-rule/40">
              <div>
                <span className="text-dim">Total tokens</span>
                <p className="mt-0.5 text-sm font-semibold text-fg">
                  {totalTokens.toLocaleString()}
                  <span className="ml-1.5 font-normal text-dim text-xs">
                    ({totalInputTokens.toLocaleString()} in / {totalOutputTokens.toLocaleString()} out)
                  </span>
                </p>
              </div>
              <div>
                <span className="text-dim">Est. cost</span>
                <p className="mt-0.5 text-sm font-semibold text-fg">{estimateCost(totalInputTokens, totalOutputTokens)}</p>
              </div>
              <div>
                <span className="text-dim">LLM calls</span>
                <p className="mt-0.5 text-sm font-semibold text-fg">
                  {inference_logs.length}
                  {runningCount > 0 && (
                    <span className="ml-1.5 text-xs font-normal text-accent">
                      ({runningCount} running)
                    </span>
                  )}
                </p>
              </div>
              <div>
                <span className="text-dim">Errors</span>
                <p className={`mt-0.5 text-sm font-semibold ${errorCount > 0 ? 'text-red-400' : 'text-fg'}`}>
                  {errorCount}
                </p>
              </div>
            </div>

            <div className="overflow-x-auto">
              <table className="w-full text-xs">
                <thead>
                  <tr className="border-b border-rule text-dim text-left">
                    <th className="pb-2 pr-3 font-medium whitespace-nowrap">Time</th>
                    <th className="pb-2 pr-3 font-medium">Model</th>
                    <th className="pb-2 pr-3 font-medium">Intent</th>
                    <th className="pb-2 pr-3 font-medium text-right">In</th>
                    <th className="pb-2 pr-3 font-medium text-right">Out</th>
                    <th className="pb-2 pr-3 font-medium text-right">Latency</th>
                    <th className="pb-2 font-medium">Status</th>
                  </tr>
                </thead>
                <tbody>
                  {inference_logs.map((log) => (
                    <InferenceRow key={log.id} log={log} />
                  ))}
                </tbody>
              </table>
            </div>
          </>
        )}
      </Section>

      {/* ── Live Activity ── */}
      {(isActive || toolEvents.length > 0) && (
        <Section title={isActive ? 'Live Activity' : `Tool Activity (${toolEvents.length} calls)`}>
          {toolEvents.length === 0 ? (
            <p className="text-xs text-dim italic">
              Waiting for tool calls… activity will appear here as the agent works.
            </p>
          ) : (
            <div
              ref={liveScrollRef}
              className="max-h-[600px] overflow-y-auto flex flex-col gap-1.5 pr-1"
            >
              {toolEvents.map((ev, i) => (
                <div
                  key={i}
                  className={[
                    'rounded-sm border px-3 py-2.5 text-sm',
                    ev.is_error
                      ? 'border-red-700/50 bg-red-900/10'
                      : 'border-rule/50 bg-surface-900',
                  ].join(' ')}
                >
                  <div className="flex items-center gap-2 flex-wrap">
                    <span className="text-dim whitespace-nowrap text-xs font-mono">
                      {new Date(ev.timestamp).toLocaleTimeString(undefined, {
                        hour: '2-digit',
                        minute: '2-digit',
                        second: '2-digit',
                      })}
                    </span>
                    <span
                      className={[
                        'rounded px-2 py-0.5 font-medium font-mono text-xs',
                        ev.is_error
                          ? 'bg-red-500/20 text-red-300'
                          : 'bg-accent/10 text-accent',
                      ].join(' ')}
                    >
                      {ev.tool}
                    </span>
                    {ev.arguments && ev.arguments !== '{}' && (
                      <span className="text-dim truncate max-w-md text-xs" title={ev.arguments}>
                        {ev.arguments.length > 80 ? ev.arguments.slice(0, 80) + '…' : ev.arguments}
                      </span>
                    )}
                  </div>
                  {ev.result_preview && (
                    <p
                      className={[
                        'mt-1.5 leading-snug text-xs',
                        ev.is_error ? 'text-red-400' : 'text-muted',
                      ].join(' ')}
                      title={ev.result_preview}
                    >
                      {ev.result_preview}
                    </p>
                  )}
                </div>
              ))}
            </div>
          )}
        </Section>
      )}

      {/* ── Agent Assignments ── */}
      <Section title={`Agent Assignments (${assignments.length})`}>
        {assignments.length === 0 ? (
          <p className="text-xs text-dim italic">No agent assignments.</p>
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full text-xs">
              <thead>
                <tr className="border-b border-rule text-dim text-left">
                  <th className="pb-2 pr-4 font-medium">Agent Type</th>
                  <th className="pb-2 pr-4 font-medium">Instance</th>
                  <th className="pb-2 pr-4 font-medium">Status</th>
                  <th className="pb-2 pr-4 font-medium">Assigned</th>
                  <th className="pb-2 font-medium">Completed</th>
                </tr>
              </thead>
              <tbody>
                {assignments.map((a) => (
                  <tr key={a.id} className="border-b border-rule/30 last:border-0">
                    <td className="py-2 pr-4 text-fg">{a.agent_type}</td>
                    <td className="py-2 pr-4 font-mono text-muted">{a.instance_id}</td>
                    <td className="py-2 pr-4">
                      <span className={[
                        'rounded px-1.5 py-0.5 text-xs font-medium',
                        a.status === 'active'    ? 'bg-emerald-500/20 text-emerald-300 border border-emerald-500/30' :
                        a.status === 'completed' ? 'bg-teal-500/20 text-teal-300 border border-teal-500/30' :
                        a.status === 'failed'    ? 'bg-red-500/20 text-red-300 border border-red-500/30' :
                                                   'bg-surface-700/40 text-muted border border-sharp/30',
                      ].join(' ')}>
                        {a.status}
                      </span>
                    </td>
                    <td className="py-2 pr-4 text-muted whitespace-nowrap">{formatTs(a.assigned_at)}</td>
                    <td className="py-2 text-muted whitespace-nowrap">
                      {a.completed_at ? formatTs(a.completed_at) : <span className="text-dim">—</span>}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Section>

      </>}{/* end overview tab / non-human_review wrapper */}

    </div>
  )
}

// ─── Human Review Code Tab ──────────────────────────────────────────────────

interface DiffResponse {
  diff: string
  stats: string
  branch: string
  base: string
  message?: string
}

function HumanReviewCodeTab({ workItemId, onApproved }: { workItemId: string; onApproved: () => void }) {
  const { get, post } = useApi()
  const [data, setData] = useState<DiffResponse | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [approving, setApproving] = useState(false)
  const [approveResult, setApproveResult] = useState<{ success: boolean; message: string } | null>(null)

  useEffect(() => {
    let cancelled = false
    async function fetchDiff() {
      try {
        const resp = await get<DiffResponse>(`/work-items/diff?id=${workItemId}`)
        if (!cancelled) {
          setData(resp)
          setError(null)
        }
      } catch (err) {
        if (!cancelled) setError(err instanceof Error ? err.message : 'Failed to load diff')
      } finally {
        if (!cancelled) setLoading(false)
      }
    }
    fetchDiff()
    return () => { cancelled = true }
  }, [workItemId, get])

  async function handleApprove() {
    setApproving(true)
    setApproveResult(null)
    try {
      await post(`/work-items/approve?id=${workItemId}`)
      setApproveResult({ success: true, message: 'Approved — QA dispatching' })
      onApproved()
    } catch (err) {
      setApproveResult({ success: false, message: err instanceof Error ? err.message : 'Approval failed' })
    } finally {
      setApproving(false)
    }
  }

  if (loading) {
    return (
      <div className="flex h-48 items-center justify-center">
        <div className="h-6 w-6 animate-spin rounded-full border-2 border-accent border-t-transparent" />
      </div>
    )
  }

  if (error) {
    return (
      <Section title="Code Changes">
        <p className="text-sm text-red-400">{error}</p>
      </Section>
    )
  }

  if (!data || !data.diff) {
    return (
      <Section title="Code Changes">
        <p className="text-sm text-dim italic">{data?.message || 'No diff available — the agent branch may not exist yet.'}</p>
      </Section>
    )
  }

  const diffLines = data.diff.split('\n')

  return (
    <div className="flex flex-col gap-4">
      {/* Branch info */}
      <Section title="Code Changes">
        <div className="flex flex-wrap items-center gap-3 mb-4 text-xs">
          <span className="text-dim">Branch:</span>
          <span className="font-mono text-accent bg-accent/10 rounded px-2 py-0.5">{data.branch.replace('origin/', '')}</span>
          <svg className="h-3 w-3 text-dim" fill="none" stroke="currentColor" strokeWidth={2} viewBox="0 0 24 24">
            <path strokeLinecap="round" strokeLinejoin="round" d="M9 5l7 7-7 7" />
          </svg>
          <span className="font-mono text-muted bg-surface-700 rounded px-2 py-0.5">{data.base}</span>
        </div>

        {/* Stats summary */}
        {data.stats && (
          <pre className="mb-4 rounded-sm border border-rule bg-surface-900 px-4 py-3 text-xs text-muted font-mono whitespace-pre-wrap">{data.stats}</pre>
        )}

        {/* Diff */}
        <div className="rounded-sm border border-rule bg-surface-950 overflow-hidden">
          <pre className="overflow-x-auto text-xs font-mono leading-relaxed">
            {diffLines.map((line, i) => {
              let className = 'px-4 '
              if (line.startsWith('diff --git'))
                className += 'text-muted font-semibold mt-2 border-t border-rule/40 pt-1'
              else if (line.startsWith('@@'))
                className += 'text-blue-400 bg-blue-950/20'
              else if (line.startsWith('+'))
                className += 'text-emerald-400 bg-emerald-950/30'
              else if (line.startsWith('-'))
                className += 'text-red-400 bg-red-950/30'
              else className += 'text-dim'
              return (
                <div key={i} className={className}>
                  {line || ' '}
                </div>
              )
            })}
          </pre>
        </div>
      </Section>

      {/* Approve/reject actions */}
      <div className="rounded-sm border border-rule bg-surface-850 p-4">
        {approveResult && (
          <div
            className={[
              'mb-3 rounded px-3 py-2 text-xs',
              approveResult.success
                ? 'bg-emerald-500/20 text-emerald-300 border border-emerald-500/30'
                : 'bg-red-500/20 text-red-300 border border-red-500/30',
            ].join(' ')}
          >
            {approveResult.message}
          </div>
        )}
        <div className="flex items-center gap-3">
          <button
            type="button"
            disabled={approving}
            onClick={handleApprove}
            className="rounded-sm bg-emerald-600 px-5 py-2 text-sm font-semibold text-white transition-colors hover:bg-emerald-500 disabled:cursor-not-allowed disabled:opacity-50"
          >
            {approving ? 'Approving…' : 'Approve & Dispatch QA'}
          </button>
          <span className="text-xs text-dim">
            Approving transitions this work item to QA and dispatches automated testing.
          </span>
        </div>
      </div>
    </div>
  )
}

// InferenceRow renders one row in the LLM Activity table. Click the row to
// expand and see the full conversation (system/user/assistant messages sent
// to the model, plus the model's response). For in-flight rows, renders a
// pulsing indicator + live elapsed clock; when the row lands, the same
// component re-renders with real values.
type InferenceRowLog = {
  id: string
  model: string
  intent: string
  input_tokens: number
  output_tokens: number
  latency_ms: number
  success: boolean
  error?: string
  created_at: string
  prompt_messages?: Array<{ role: 'system' | 'user' | 'assistant' | 'tool'; content: string; name?: string }>
  response_content?: string
}

function InferenceRow({ log }: { log: InferenceRowLog }) {
  const [open, setOpen] = useState(false)
  const running = log.latency_ms === 0 && !log.success && !log.error
  const messages = log.prompt_messages ?? []
  const response = log.response_content ?? ''
  const hasConversation = messages.length > 0 || response.length > 0

  return (
    <>
      <tr
        className="border-b border-rule/30 last:border-0 cursor-pointer hover:bg-surface-800/30"
        onClick={() => !running && setOpen(o => !o)}
        title={running ? 'Conversation available once the call completes' : open ? 'Hide conversation' : 'Show conversation'}
      >
        <td className="py-2 pr-3 text-dim whitespace-nowrap">
          <span className="inline-block w-3 text-dim">{running ? ' ' : open ? '▾' : '▸'}</span>
          {formatTs(log.created_at)}
        </td>
        <td className="py-2 pr-3 text-muted font-mono whitespace-nowrap">{log.model}</td>
        <td className="py-2 pr-3 text-muted">{log.intent}</td>
        <td className="py-2 pr-3 text-right text-muted">
          {running ? <span className="text-dim">—</span> : log.input_tokens.toLocaleString()}
        </td>
        <td className="py-2 pr-3 text-right text-muted">
          {running ? <span className="text-dim">—</span> : log.output_tokens.toLocaleString()}
        </td>
        <td className="py-2 pr-3 text-right text-muted">
          {running ? <LiveElapsed since={log.created_at} /> : formatDuration(log.latency_ms)}
        </td>
        <td className="py-2">
          {running ? (
            <span className="inline-flex items-center gap-1.5 text-accent">
              <span className="inline-block h-2 w-2 rounded-full bg-indigo-400 animate-pulse" />
              running
            </span>
          ) : log.success ? (
            <span className="text-emerald-400">ok</span>
          ) : (
            <span className="text-red-400" title={log.error}>error</span>
          )}
          {log.error && !log.success && !running && (
            <p className="mt-0.5 text-red-400/80 text-xs leading-tight max-w-xs truncate">{log.error}</p>
          )}
        </td>
      </tr>
      {open && (
        <tr>
          <td colSpan={7} className="bg-surface-950/80 px-4 py-4 border-b border-rule/30">
            {!hasConversation ? (
              <p className="italic text-dim">No conversation captured for this row (older call, or pre-transparency).</p>
            ) : (
              <div className="space-y-3">
                {messages.map((m, i) => (
                  <ChatBubble key={i} role={m.role} content={m.content} name={m.name} />
                ))}
                {response && <ChatBubble role="assistant" content={response} label="response" />}
              </div>
            )}
          </td>
        </tr>
      )}
    </>
  )
}

function ChatBubble({ role, content, name, label }: { role: string; content: string; name?: string; label?: string }) {
  const palette: Record<string, { bg: string; border: string; text: string }> = {
    system:    { bg: 'bg-surface-800/60',   border: 'border-rule/50', text: 'text-muted' },
    user:      { bg: 'bg-indigo-900/20',  border: 'border-indigo-700/50', text: 'text-accent' },
    assistant: { bg: 'bg-emerald-900/15', border: 'border-emerald-700/40', text: 'text-emerald-100' },
    tool:      { bg: 'bg-amber-900/15',   border: 'border-amber-700/40', text: 'text-amber-200' },
  }
  const p = palette[role] ?? palette.system
  const heading = label ? `${role} · ${label}` : name ? `${role} · ${name}` : role
  return (
    <div className={`rounded-sm border ${p.border} ${p.bg} px-3 py-2`}>
      <div className="mb-1 flex items-center gap-2">
        <span className={`text-[10px] font-mono uppercase tracking-wider ${p.text}`}>{heading}</span>
        <span className="text-[10px] text-dim">{content.length.toLocaleString()} chars</span>
      </div>
      <pre className={`whitespace-pre-wrap break-words text-xs leading-relaxed ${p.text}`}>{content}</pre>
    </div>
  )
}

// LiveElapsed renders a monospace elapsed-time readout that ticks every
// second while an inference call is in flight (identified by latency_ms=0
// in the row). Mounts cheap: single setInterval, no global state. The
// parent row unmounts this component naturally the moment the row update
// lands via the next poll — the component's own timer never has to fire
// a final "done" state.
function LiveElapsed({ since }: { since: string }) {
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), 1000)
    return () => clearInterval(t)
  }, [])
  const startedAt = new Date(since).getTime()
  const elapsedMs = Math.max(0, now - startedAt)
  const s = Math.floor(elapsedMs / 1000)
  return (
    <span className="text-accent tabular-nums">
      {s < 60 ? `${s}s` : `${Math.floor(s / 60)}m ${s % 60}s`}
    </span>
  )
}

// BlockedActionRequired is the prominent banner shown at the top of a work
// item page when it has been blocked — typically by product-agent's clarity
// check. It pulls the questions out of the block reason, provides an
// editable description textarea, and a Save & Retry button that reruns the
// entire PRD flow against the new description.
function BlockedActionRequired({
  workItemId,
  description,
  blockReason,
  onRefetch,
}: {
  workItemId: string
  description: string
  blockReason: string
  onRefetch: () => void | Promise<void>
}) {
  const { post } = useApi()
  const [draft, setDraft] = useState(description)
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const questions = blockReason
    .split('\n')
    .map(l => l.trim())
    .filter(l => l.startsWith('•') || l.startsWith('-'))
    .map(l => l.replace(/^[•\-]\s*/, ''))

  const isClarityBlock = questions.length > 0 || /ambiguous|too vague|unclear/i.test(blockReason)

  async function handleRetry() {
    if (!draft.trim()) {
      setError('Description cannot be empty.')
      return
    }
    setSubmitting(true)
    setError(null)
    try {
      await post(`/work-items/update-and-retry?id=${workItemId}`, { description: draft })
      await onRefetch()
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Failed to retry')
    } finally {
      setSubmitting(false)
    }
  }

  async function handlePlainRetry() {
    setSubmitting(true)
    setError(null)
    try {
      await post(`/work-items/retry?id=${workItemId}`)
      await onRefetch()
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Retry failed')
    } finally {
      setSubmitting(false)
    }
  }

  async function handleCancel() {
    setSubmitting(true)
    setError(null)
    try {
      await post(`/work-items/cancel?id=${workItemId}`)
      await onRefetch()
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Cancel failed')
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <div className="rounded-sm border-2 border-red-600/60 bg-red-900/15 overflow-hidden">
      <div className="bg-red-900/30 border-b border-red-700/60 px-5 py-3">
        <h3 className="text-sm font-semibold text-red-200 flex items-center gap-2">
          <span className="inline-block h-2 w-2 rounded-full bg-red-400 animate-pulse" />
          {isClarityBlock
            ? 'Action Required — clarity check blocked this work item'
            : 'Blocked — pipeline cannot proceed'}
        </h3>
      </div>
      <div className="p-5 space-y-5">
        {isClarityBlock ? (
          <>
            <p className="text-sm text-red-100/90 leading-relaxed">
              Product-agent refused to generate a PRD because the intake was too ambiguous.
              The LLM produced a list of questions it needs answered. Update the description
              to address them, then click <span className="font-semibold">Save &amp; Retry</span>.
            </p>

            {questions.length > 0 && (
              <div className="rounded-sm border border-red-700/40 bg-surface-950/60 p-4">
                <p className="text-xs font-mono uppercase tracking-wider text-red-300 mb-2">
                  Questions to answer ({questions.length})
                </p>
                <ol className="list-decimal list-inside space-y-1.5 text-sm text-fg">
                  {questions.map((q, i) => <li key={i}>{q}</li>)}
                </ol>
              </div>
            )}

            <div>
              <label className="block text-xs font-mono uppercase tracking-wider text-muted mb-2">
                Updated description
              </label>
              <textarea
                value={draft}
                onChange={e => setDraft(e.target.value)}
                rows={10}
                className="w-full rounded-sm border border-rule bg-surface-950 px-3 py-2 text-sm text-fg font-mono focus:outline-none focus:ring-1 focus:ring-red-500"
                placeholder="Rewrite the description to answer each of the questions above. Be specific about the repo, file, and acceptance criteria."
              />
              <p className="mt-1 text-xs text-dim">
                {draft.length.toLocaleString()} characters
              </p>
            </div>

            {error && (
              <p className="text-sm text-red-400 bg-red-900/20 border border-red-700/40 rounded px-3 py-2">
                {error}
              </p>
            )}

            <div className="flex items-center gap-3">
              <button
                type="button"
                onClick={handleRetry}
                disabled={submitting || !draft.trim() || draft.trim() === description.trim()}
                className="rounded bg-emerald-600 px-4 py-2 text-sm font-medium text-white hover:bg-emerald-500 disabled:opacity-50 disabled:cursor-not-allowed"
              >
                {submitting ? 'Saving…' : 'Save & Retry'}
              </button>
              <button
                type="button"
                onClick={() => setDraft(description)}
                disabled={submitting || draft === description}
                className="rounded-sm border border-rule px-4 py-2 text-sm text-fg hover:bg-surface-900 disabled:opacity-50"
              >
                Reset
              </button>
              <span className="text-xs text-dim">
                {draft.trim() === description.trim()
                  ? 'Edit the description to enable retry'
                  : 'Clicking retry reruns the PRD flow from scratch with the new description'}
              </span>
            </div>
          </>
        ) : (
          <>
            <div className="rounded-sm border border-rule bg-surface-950/60 p-4">
              <p className="text-xs font-mono uppercase tracking-wider text-muted mb-2">Block reason</p>
              <pre className="whitespace-pre-wrap text-sm text-fg">{blockReason || '(no reason recorded)'}</pre>
            </div>

            {/repository|repo|onboarding/i.test(blockReason) && (
              <p className="text-sm text-red-100/90 leading-relaxed">
                This work item requires a connected git repository. Go to{' '}
                <Link to="/admin/products" className="text-accent hover:underline font-medium">Admin &rarr; Products</Link>{' '}
                to connect a repository, then retry.
              </p>
            )}

            {error && (
              <p className="text-sm text-red-400 bg-red-900/20 border border-red-700/40 rounded px-3 py-2">
                {error}
              </p>
            )}

            <div className="flex items-center gap-3">
              <button
                type="button"
                onClick={handlePlainRetry}
                disabled={submitting}
                className="rounded bg-amber-600 px-4 py-2 text-sm font-medium text-white hover:bg-amber-500 disabled:opacity-50 disabled:cursor-not-allowed"
              >
                {submitting ? 'Retrying…' : 'Retry'}
              </button>
              <button
                type="button"
                onClick={handleCancel}
                disabled={submitting}
                className="rounded-sm border border-red-600/60 px-4 py-2 text-sm text-red-300 hover:bg-red-900/30 disabled:opacity-50"
              >
                Cancel Work Item
              </button>
            </div>
          </>
        )}
      </div>
    </div>
  )
}

// AwaitingUserPrompt renders the currently-pending user-interaction prompt
// inline on the work-item detail page. Wraps PromptDetailPane from
// PromptSheet so the same answer form is used in both surfaces.
function AwaitingUserPrompt({
  promptId,
  onAnswered,
}: {
  promptId: string
  onAnswered: () => void
}) {
  const { prompt, loading } = usePrompt(promptId)

  if (loading) {
    return (
      <div className="rounded-sm border-2 border-lime-500/40 bg-lime-500/10 px-4 py-3 text-sm text-lime-200">
        Loading prompt…
      </div>
    )
  }
  if (!prompt) {
    return null
  }
  if (prompt.state !== 'pending') {
    // Prompt was answered or revoked between fetch and render — the parent
    // refetch will move the work item out of awaiting_user shortly.
    return null
  }

  return (
    <div className="rounded-sm border-l-4 border-lime-400 bg-surface-950">
      <div className="border-b border-rule px-4 py-2">
        <div className="font-mono text-[0.65rem] uppercase tracking-widest text-lime-400">
          Awaiting your answer
        </div>
        <div className="mt-0.5 text-xs text-muted">
          The agent has paused. Your answer will be persisted as codex knowledge and the work item will resume.
        </div>
      </div>
      <PromptDetailPane prompt={prompt} onSubmitted={onAnswered} />
    </div>
  )
}
