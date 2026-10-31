import { useState, useEffect, FormEvent } from 'react'
import { useParams, useNavigate, Link } from 'react-router-dom'

// ─── Pipeline stage order ──────────────────────────────────────────────────

const PIPELINE_STAGES = [
  { key: 'intake',         label: 'Received' },
  { key: 'prd',           label: 'Analysis' },
  { key: 'research',      label: 'Research' },
  { key: 'planning',      label: 'Planning' },
  { key: 'implementation', label: 'Building' },
  { key: 'verification',  label: 'Verifying' },
  { key: 'review',        label: 'Review' },
  { key: 'qa',            label: 'QA' },
]

const TERMINAL_STAGES: Record<string, { label: string; color: string }> = {
  passed:       { label: 'Complete', color: 'bg-green-100 text-green-800' },
  released:     { label: 'Deployed', color: 'bg-teal-100 text-teal-800' },
  blocked:      { label: 'On Hold',  color: 'bg-yellow-100 text-yellow-800' },
  failed:       { label: 'Investigating', color: 'bg-orange-100 text-orange-800' },
  rejected:     { label: 'Closed',   color: 'bg-red-100 text-red-800' },
  human_review: { label: 'Approval', color: 'bg-purple-100 text-purple-800' },
}

function stageIndex(stage: string): number {
  return PIPELINE_STAGES.findIndex(s => s.key === stage)
}

function statusBadgeColor(stage: string): string {
  if (TERMINAL_STAGES[stage]) return TERMINAL_STAGES[stage].color
  const idx = stageIndex(stage)
  if (idx === -1) return 'bg-gray-100 text-gray-700'
  const colors = [
    'bg-indigo-100 text-indigo-800',
    'bg-violet-100 text-violet-800',
    'bg-blue-100 text-blue-800',
    'bg-cyan-100 text-cyan-800',
    'bg-emerald-100 text-emerald-800',
    'bg-amber-100 text-amber-800',
    'bg-lime-100 text-lime-800',
  ]
  return colors[idx] ?? 'bg-gray-100 text-gray-700'
}

// ─── API types ─────────────────────────────────────────────────────────────

interface TrackData {
  tracking_id: string
  title: string
  status: string
  stage: string
  product_name?: string
  submitted_at: string
  updated_at: string
}

interface UpdateEntry {
  timestamp: string
  status: string
  description: string
}

interface UpdatesData {
  tracking_id: string
  updates: UpdateEntry[]
}

// ─── Component ──────────────────────────────────────────────────────────────

export default function TrackPage() {
  const { trackingId: urlId } = useParams<{ trackingId: string }>()
  const navigate = useNavigate()

  const [inputId, setInputId] = useState(urlId ?? '')
  const [trackData, setTrackData] = useState<TrackData | null>(null)
  const [updates, setUpdates] = useState<UpdateEntry[]>([])
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    if (urlId) {
      fetchStatus(urlId)
      const interval = setInterval(() => fetchStatus(urlId, true), 12_000)
      return () => clearInterval(interval)
    }
  }, [urlId])

  async function fetchStatus(id: string, silent = false) {
    if (!silent) {
      setLoading(true)
      setError(null)
      setTrackData(null)
      setUpdates([])
    }

    try {
      const [statusRes, updatesRes] = await Promise.all([
        fetch(`/api/track/${id}`),
        fetch(`/api/track/${id}/updates`),
      ])

      if (!statusRes.ok) {
        const body = await statusRes.json().catch(() => ({}))
        throw new Error(body.error ?? 'Tracking ID not found.')
      }

      const status: TrackData = await statusRes.json()
      setTrackData(status)

      if (updatesRes.ok) {
        const upd: UpdatesData = await updatesRes.json()
        setUpdates(upd.updates ?? [])
      }
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : 'Failed to fetch status.')
    } finally {
      setLoading(false)
    }
  }

  function handleSearch(e: FormEvent) {
    e.preventDefault()
    const id = inputId.trim().toUpperCase()
    if (!id) return
    navigate(`/track/${id}`)
  }

  const currentStageIdx = trackData ? stageIndex(trackData.stage) : -1
  const isTerminal = trackData ? Boolean(TERMINAL_STAGES[trackData.stage]) : false

  return (
    <div className="min-h-screen bg-gray-50 py-12 px-4 sm:px-6 lg:px-8">
      <div className="max-w-2xl mx-auto">

        {/* Header */}
        <div className="mb-8">
          <Link to="/" className="inline-flex items-center gap-1.5 text-sm text-indigo-600 hover:text-indigo-500 mb-6">
            <svg className="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
              <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M15 19l-7-7 7-7" />
            </svg>
            Submit a request
          </Link>
          <h1 className="text-3xl font-bold text-gray-900">Track Your Request</h1>
          <p className="mt-1 text-gray-500 text-sm">Enter your tracking ID to see the current status.</p>
        </div>

        {/* Search bar */}
        <form onSubmit={handleSearch} className="flex gap-3 mb-8">
          <input
            type="text"
            value={inputId}
            onChange={e => setInputId(e.target.value)}
            placeholder="TRK-a1b2c3d4"
            className="flex-1 px-4 py-2.5 border border-gray-300 rounded-lg text-sm font-mono text-gray-900 placeholder-gray-400
                       focus:outline-none focus:ring-2 focus:ring-indigo-500 focus:border-transparent transition"
          />
          <button
            type="submit"
            className="px-5 py-2.5 bg-indigo-600 text-white text-sm font-semibold rounded-lg hover:bg-indigo-700
                       focus:outline-none focus:ring-2 focus:ring-indigo-500 focus:ring-offset-2 transition"
          >
            Track
          </button>
        </form>

        {/* Loading */}
        {loading && (
          <div className="flex justify-center py-16">
            <svg className="animate-spin w-8 h-8 text-indigo-500" fill="none" viewBox="0 0 24 24">
              <circle className="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" strokeWidth="4" />
              <path className="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4z" />
            </svg>
          </div>
        )}

        {/* Error */}
        {error && !loading && (
          <div className="bg-red-50 border border-red-200 rounded-xl p-6 text-center">
            <svg className="w-10 h-10 text-red-400 mx-auto mb-3" fill="none" stroke="currentColor" viewBox="0 0 24 24">
              <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={1.5}
                d="M9.172 16.172a4 4 0 015.656 0M9 10h.01M15 10h.01M21 12a9 9 0 11-18 0 9 9 0 0118 0z" />
            </svg>
            <p className="text-sm font-medium text-red-800">{error}</p>
            <p className="text-xs text-red-600 mt-1">Double-check the tracking ID and try again.</p>
          </div>
        )}

        {/* Status card */}
        {trackData && !loading && (
          <div className="space-y-6">

            {/* Main status card */}
            <div className="bg-white border border-gray-200 rounded-2xl shadow-sm p-6">
              <div className="flex items-start justify-between gap-4 mb-6">
                <div className="flex-1 min-w-0">
                  <h2 className="text-lg font-semibold text-gray-900 truncate">{trackData.title}</h2>
                  <p className="text-xs text-gray-400 mt-0.5 font-mono">{trackData.tracking_id}</p>
                  {trackData.product_name && (
                    <p className="text-xs text-indigo-600 mt-1 font-medium">{trackData.product_name}</p>
                  )}
                </div>
                <span className={`shrink-0 inline-flex items-center px-3 py-1 rounded-full text-xs font-semibold ${statusBadgeColor(trackData.stage)}`}>
                  {trackData.status}
                </span>
              </div>

              {/* Progress bar — shown for non-terminal stages */}
              {!isTerminal && (
                <div>
                  <div className="flex items-center justify-between mb-2">
                    <span className="text-xs text-gray-500">Pipeline progress</span>
                    <span className="text-xs text-gray-400">
                      {currentStageIdx + 1} / {PIPELINE_STAGES.length}
                    </span>
                  </div>
                  <div className="flex items-center gap-1">
                    {PIPELINE_STAGES.map((stage, idx) => {
                      const isDone = idx < currentStageIdx
                      const isCurrent = idx === currentStageIdx
                      return (
                        <div key={stage.key} className="flex-1 flex flex-col items-center gap-1">
                          <div
                            className={`w-full h-1.5 rounded-full transition-colors ${
                              isDone ? 'bg-indigo-500' : isCurrent ? 'bg-indigo-300' : 'bg-gray-200'
                            }`}
                          />
                          <span className={`text-[10px] font-medium hidden sm:block ${
                            isCurrent ? 'text-indigo-600' : isDone ? 'text-gray-400' : 'text-gray-300'
                          }`}>
                            {stage.label}
                          </span>
                        </div>
                      )
                    })}
                  </div>
                </div>
              )}

              {/* Terminal state indicator */}
              {isTerminal && TERMINAL_STAGES[trackData.stage] && (
                <div className={`inline-flex items-center gap-2 px-3 py-2 rounded-lg text-sm font-medium ${TERMINAL_STAGES[trackData.stage].color}`}>
                  <svg className="w-4 h-4" fill="currentColor" viewBox="0 0 20 20">
                    <path fillRule="evenodd"
                      d="M10 18a8 8 0 100-16 8 8 0 000 16zm3.707-9.293a1 1 0 00-1.414-1.414L9 10.586 7.707 9.293a1 1 0 00-1.414 1.414l2 2a1 1 0 001.414 0l4-4z"
                      clipRule="evenodd" />
                  </svg>
                  {trackData.status}
                </div>
              )}

              {/* Meta */}
              <div className="mt-5 pt-5 border-t border-gray-100 grid grid-cols-2 gap-4 text-sm">
                <div>
                  <p className="text-xs text-gray-400 mb-0.5">Submitted</p>
                  <p className="text-gray-700 font-medium">
                    {new Date(trackData.submitted_at).toLocaleDateString('en-US', {
                      year: 'numeric', month: 'long', day: 'numeric'
                    })}
                  </p>
                </div>
                <div>
                  <p className="text-xs text-gray-400 mb-0.5">Last updated</p>
                  <p className="text-gray-700 font-medium">
                    {new Date(trackData.updated_at).toLocaleDateString('en-US', {
                      year: 'numeric', month: 'long', day: 'numeric'
                    })}
                  </p>
                </div>
              </div>
            </div>

            {/* Timeline */}
            {updates.length > 0 && (
              <div className="bg-white border border-gray-200 rounded-2xl shadow-sm p-6">
                <h3 className="text-base font-semibold text-gray-900 mb-5">Activity Timeline</h3>
                <div className="space-y-4">
                  {updates.map((entry, idx) => (
                    <div key={idx} className="flex gap-4">
                      {/* Timeline dot + line */}
                      <div className="flex flex-col items-center">
                        <div className={`w-2.5 h-2.5 rounded-full shrink-0 mt-1 ${
                          idx === 0 ? 'bg-indigo-500' : 'bg-gray-300'
                        }`} />
                        {idx < updates.length - 1 && (
                          <div className="w-px flex-1 bg-gray-200 mt-1" />
                        )}
                      </div>
                      {/* Content */}
                      <div className="pb-4 flex-1">
                        <div className="flex items-center gap-2 flex-wrap mb-1">
                          <span className={`inline-flex items-center px-2 py-0.5 rounded text-xs font-semibold ${statusBadgeColor(
                            Object.entries(TERMINAL_STAGES).find(([, v]) => v.label === entry.status)?.[0] ??
                            PIPELINE_STAGES.find(s => entry.status.toLowerCase().includes(s.label.toLowerCase()))?.key ??
                            ''
                          )}`}>
                            {entry.status}
                          </span>
                          <span className="text-xs text-gray-400">
                            {new Date(entry.timestamp).toLocaleString('en-US', {
                              month: 'short', day: 'numeric',
                              hour: 'numeric', minute: '2-digit'
                            })}
                          </span>
                        </div>
                        <p className="text-sm text-gray-600">{entry.description}</p>
                      </div>
                    </div>
                  ))}
                </div>
              </div>
            )}

          </div>
        )}
      </div>
    </div>
  )
}
