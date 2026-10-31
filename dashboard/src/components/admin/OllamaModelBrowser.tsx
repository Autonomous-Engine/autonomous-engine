import { useState, useEffect, useCallback, useRef } from 'react'
import { useApi } from '@/hooks/useApi'
import type { OllamaModel, OllamaStatus, OllamaPullProgress, OllamaRunningModel } from '@/types'

// ─── Suggested models ─────────────────────────────────────────────────────────

const SUGGESTED_MODELS = [
  'qwen2.5:7b',
  'qwen2.5:32b',
  'llama3.1:8b',
  'llama3.1:70b',
  'deepseek-coder-v2:16b',
  'codellama:13b',
  'mistral:7b',
  'phi4:14b',
]

// ─── Helpers ──────────────────────────────────────────────────────────────────

function formatBytes(bytes: number): string {
  if (bytes === 0) return '0 B'
  if (bytes < 1024 * 1024 * 1024) {
    return (bytes / (1024 * 1024)).toFixed(0) + ' MB'
  }
  return (bytes / (1024 * 1024 * 1024)).toFixed(2) + ' GB'
}

function formatDate(iso: string): string {
  try {
    return new Date(iso).toLocaleDateString(undefined, {
      year: 'numeric',
      month: 'short',
      day: 'numeric',
    })
  } catch {
    return iso
  }
}

// ─── Pull progress entry ──────────────────────────────────────────────────────

interface PullEntry {
  model: string
  status: string
  completed: number
  total: number
  done: boolean
  error: string
}

// ─── Props ────────────────────────────────────────────────────────────────────

interface OllamaModelBrowserProps {
  /** The Ollama endpoint URL, e.g. http://host.docker.internal:11434 */
  endpoint: string
  /** Called after a pull completes so the parent can refresh model lists */
  onModelsChanged?: (models: OllamaModel[]) => void
  /**
   * When set, routes model listing / pull / delete through the provider-scoped
   * endpoints (/api/admin/providers/…?id=providerId) instead of the legacy
   * /api/admin/ollama/* endpoints.
   */
  providerId?: string
  /** The provider's currently-selected default model (settings.model). */
  currentModel?: string
  /**
   * Invoked when the operator clicks "Set as default" on a row. The parent is
   * responsible for persisting the change (PUT /admin/providers/update) and
   * refetching the provider list so `currentModel` updates.
   */
  onSetDefaultModel?: (modelName: string) => Promise<void>
}

// ─── OllamaModelBrowser ───────────────────────────────────────────────────────

export default function OllamaModelBrowser({ endpoint, onModelsChanged, providerId, currentModel, onSetDefaultModel }: OllamaModelBrowserProps) {
  const { get, del } = useApi()

  const [ollamaStatus, setOllamaStatus] = useState<OllamaStatus | null>(null)
  const [models, setModels] = useState<OllamaModel[]>([])
  const [runningModels, setRunningModels] = useState<OllamaRunningModel[]>([])
  const [loadingModels, setLoadingModels] = useState(true)
  const [error, setError] = useState('')

  // Pull state
  const [pullInput, setPullInput] = useState('')
  const [pulls, setPulls] = useState<PullEntry[]>([])
  const [showSuggestions, setShowSuggestions] = useState(false)
  const suggestionsRef = useRef<HTMLDivElement>(null)

  // Delete confirmation
  const [deleteTarget, setDeleteTarget] = useState<OllamaModel | null>(null)
  const [deleting, setDeleting] = useState(false)

  // Set-as-default state (name of the row currently being saved)
  const [settingDefault, setSettingDefault] = useState<string | null>(null)

  async function handleSetDefault(name: string) {
    if (!onSetDefaultModel) return
    setSettingDefault(name)
    try {
      await onSetDefaultModel(name)
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Failed to set default model')
    } finally {
      setSettingDefault(null)
    }
  }

  // ── Data fetching ─────────────────────────────────────────────────────────

  const fetchStatus = useCallback(async () => {
    try {
      // When scoped to a provider, use the provider models endpoint to derive status.
      // Fall back to legacy endpoint when no providerId.
      if (providerId) {
        const data = await get<{ models: OllamaModel[] }>(`/admin/providers/models?id=${providerId}`)
        const count = data?.models?.length ?? 0
        setOllamaStatus({ reachable: true, endpoint: endpoint || 'unknown', model_count: count })
      } else {
        const data = await get<OllamaStatus>('/admin/ollama/status')
        setOllamaStatus(data)
      }
    } catch {
      setOllamaStatus({ reachable: false, endpoint: endpoint || 'unknown', model_count: 0 })
    }
  }, [get, endpoint, providerId])

  const fetchModels = useCallback(async () => {
    setLoadingModels(true)
    setError('')
    try {
      const url = providerId
        ? `/admin/providers/models?id=${providerId}`
        : '/admin/ollama/models'
      const data = await get<{ models: OllamaModel[] }>(url)
      const list = data?.models ?? []
      setModels(list)
      onModelsChanged?.(list)
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Failed to load models')
    } finally {
      setLoadingModels(false)
    }
  }, [get, onModelsChanged, providerId])

  const fetchRunning = useCallback(async () => {
    // Running models are only available via the legacy endpoint (no provider-scoped equivalent).
    if (providerId) return
    try {
      const data = await get<{ models: OllamaRunningModel[] }>('/admin/ollama/running')
      setRunningModels(data?.models ?? [])
    } catch {
      setRunningModels([])
    }
  }, [get, providerId])

  useEffect(() => {
    fetchStatus()
    fetchModels()
    fetchRunning()

    const interval = setInterval(() => {
      fetchStatus()
      fetchModels()
      fetchRunning()
    }, 30_000)

    return () => clearInterval(interval)
  }, [fetchStatus, fetchModels, fetchRunning])

  // Close suggestions when clicking outside
  useEffect(() => {
    function handleClick(e: MouseEvent) {
      if (suggestionsRef.current && !suggestionsRef.current.contains(e.target as Node)) {
        setShowSuggestions(false)
      }
    }
    document.addEventListener('mousedown', handleClick)
    return () => document.removeEventListener('mousedown', handleClick)
  }, [])

  // ── Pull model ────────────────────────────────────────────────────────────

  function startPull(modelName: string) {
    const name = modelName.trim()
    if (!name) return

    const entry: PullEntry = {
      model: name,
      status: 'starting...',
      completed: 0,
      total: 0,
      done: false,
      error: '',
    }
    setPulls(prev => [entry, ...prev])
    setPullInput('')
    setShowSuggestions(false)

    const token = localStorage.getItem('auth_token')

    const pullUrl = providerId
      ? `/api/admin/providers/pull?id=${providerId}`
      : '/api/admin/ollama/pull'

    fetch(pullUrl, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        ...(token ? { Authorization: `Bearer ${token}` } : {}),
      },
      body: JSON.stringify({ model: name }),
    })
      .then(async res => {
        if (!res.ok || !res.body) {
          const text = await res.text().catch(() => 'Pull failed')
          setPulls(prev =>
            prev.map(p => (p.model === name && !p.done ? { ...p, error: text, done: true } : p)),
          )
          return
        }

        const reader = res.body.getReader()
        const decoder = new TextDecoder()
        let buf = ''

        while (true) {
          const { value, done } = await reader.read()
          if (done) break
          buf += decoder.decode(value, { stream: true })

          const lines = buf.split('\n')
          buf = lines.pop() ?? ''

          for (const line of lines) {
            if (!line.startsWith('data: ')) continue
            const raw = line.slice(6).trim()
            if (!raw) continue
            try {
              const progress: OllamaPullProgress = JSON.parse(raw)
              setPulls(prev =>
                prev.map(p => {
                  if (p.model !== name || p.done) return p
                  return {
                    ...p,
                    status: progress.status ?? p.status,
                    completed: progress.completed ?? p.completed,
                    total: progress.total ?? p.total,
                    done: progress.status === 'success',
                  }
                }),
              )
            } catch {
              // skip malformed lines
            }
          }
        }

        setPulls(prev =>
          prev.map(p => (p.model === name && !p.done ? { ...p, done: true, status: 'complete' } : p)),
        )
        fetchModels()
        fetchStatus()
      })
      .catch(err => {
        setPulls(prev =>
          prev.map(p =>
            p.model === name && !p.done
              ? { ...p, error: err instanceof Error ? err.message : 'Pull failed', done: true }
              : p,
          ),
        )
      })
  }

  // ── Delete model ──────────────────────────────────────────────────────────

  async function confirmDelete() {
    if (!deleteTarget) return
    setDeleting(true)
    try {
      const deleteUrl = providerId
        ? `/admin/providers/models/delete?id=${providerId}&model=${encodeURIComponent(deleteTarget.name)}`
        : `/admin/ollama/delete?name=${encodeURIComponent(deleteTarget.name)}`
      await del(deleteUrl)
      setDeleteTarget(null)
      fetchModels()
      fetchStatus()
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Failed to delete model')
    } finally {
      setDeleting(false)
    }
  }

  // ─── Render ───────────────────────────────────────────────────────────────

  return (
    <div className="mt-4 space-y-3 rounded-sm border border-indigo-700/40 bg-indigo-950/20 p-4">
      {/* Status bar */}
      <div className="flex flex-wrap items-center gap-3">
        <div className="flex items-center gap-1.5">
          <span
            className={[
              'h-2 w-2 rounded-full',
              ollamaStatus?.reachable ? 'bg-emerald-400' : 'bg-red-500',
            ].join(' ')}
          />
          <span
            className={[
              'text-xs font-medium',
              ollamaStatus?.reachable ? 'text-emerald-300' : 'text-red-400',
            ].join(' ')}
          >
            {ollamaStatus === null ? 'Checking...' : ollamaStatus.reachable ? 'Connected' : 'Unreachable'}
          </span>
        </div>
        {ollamaStatus && (
          <>
            <span className="text-xs text-dim">
              Endpoint:{' '}
              <span className="font-mono text-muted">
                {endpoint || ollamaStatus.endpoint}
              </span>
            </span>
            <span className="text-xs text-dim">
              Installed:{' '}
              <span className="font-semibold text-muted">{ollamaStatus.model_count}</span>
            </span>
          </>
        )}
        <button
          onClick={() => { fetchStatus(); fetchModels(); fetchRunning() }}
          className="ml-auto rounded-sm border border-rule px-2 py-0.5 text-xs text-muted hover:bg-surface-800 hover:text-fg"
        >
          Refresh
        </button>
      </div>

      {error && (
        <div className="rounded-sm border border-red-700/50 bg-red-900/20 px-3 py-2 text-xs text-red-400">
          {error}
        </div>
      )}

      {/* Pull new model — inline */}
      <div className="space-y-2">
        <p className="text-xs font-medium text-muted">Pull Model</p>
        <div className="flex gap-2" ref={suggestionsRef}>
          <div className="relative flex-1">
            <input
              value={pullInput}
              onChange={e => setPullInput(e.target.value)}
              onFocus={() => setShowSuggestions(true)}
              onKeyDown={e => { if (e.key === 'Enter') startPull(pullInput) }}
              placeholder="e.g. qwen2.5:7b"
              className="w-full rounded-sm border border-rule bg-surface-950 px-3 py-1.5 text-xs text-fg focus:outline-none focus:ring-1 focus:ring-accent"
            />
            {showSuggestions && (
              <div className="absolute left-0 top-full z-20 mt-1 w-full rounded-sm border border-rule bg-surface-900 py-1 shadow-xl">
                {SUGGESTED_MODELS.filter(
                  m => !pullInput || m.includes(pullInput.toLowerCase()),
                ).map(m => (
                  <button
                    key={m}
                    onMouseDown={() => { setPullInput(m); setShowSuggestions(false) }}
                    className="block w-full px-3 py-1.5 text-left text-xs font-mono text-muted hover:bg-surface-800"
                  >
                    {m}
                  </button>
                ))}
              </div>
            )}
          </div>
          <button
            onClick={() => startPull(pullInput)}
            disabled={!pullInput.trim()}
            className="rounded bg-accent px-3 py-1.5 text-xs font-medium text-surface-900 hover:bg-accent-hover disabled:opacity-40"
          >
            Pull
          </button>
        </div>

        {/* Quick-pull chips */}
        <div className="flex flex-wrap gap-1.5">
          {SUGGESTED_MODELS.map(m => (
            <button
              key={m}
              onClick={() => startPull(m)}
              className="rounded-full border border-rule bg-surface-800/60 px-2.5 py-0.5 text-xs font-mono text-muted hover:border-accent hover:text-accent"
            >
              {m}
            </button>
          ))}
        </div>

        {/* Active pull progress */}
        {pulls.length > 0 && (
          <div className="space-y-2">
            {pulls.map((p, i) => (
              <div key={`${p.model}-${i}`} className="rounded-sm border border-rule bg-surface-900 p-2.5 space-y-1.5">
                <div className="flex items-center justify-between gap-2">
                  <span className="font-mono text-xs text-fg">{p.model}</span>
                  {p.done && !p.error && (
                    <span className="rounded bg-emerald-800/40 px-1.5 py-0.5 text-xs font-medium text-emerald-300">
                      Done
                    </span>
                  )}
                  {p.error && (
                    <span className="rounded bg-red-800/40 px-1.5 py-0.5 text-xs font-medium text-red-300">
                      Error
                    </span>
                  )}
                  {!p.done && !p.error && (
                    <span className="rounded bg-indigo-800/40 px-1.5 py-0.5 text-xs font-medium text-accent animate-pulse">
                      Downloading
                    </span>
                  )}
                </div>
                {p.error ? (
                  <p className="text-xs text-red-400">{p.error}</p>
                ) : (
                  <>
                    <p className="text-xs text-muted capitalize">{p.status}</p>
                    {p.total > 0 && (
                      <div className="space-y-0.5">
                        <div className="h-1 w-full rounded-full bg-surface-700">
                          <div
                            className="h-1 rounded-full bg-indigo-500 transition-all duration-300"
                            style={{ width: `${Math.min(100, (p.completed / p.total) * 100).toFixed(1)}%` }}
                          />
                        </div>
                        <div className="flex justify-between text-xs text-dim">
                          <span>{formatBytes(p.completed)} / {formatBytes(p.total)}</span>
                          <span>{((p.completed / p.total) * 100).toFixed(1)}%</span>
                        </div>
                      </div>
                    )}
                  </>
                )}
              </div>
            ))}
          </div>
        )}
      </div>

      {/* Installed Models — compact table */}
      <div className="rounded-sm border border-rule bg-surface-950 overflow-hidden">
        <div className="flex items-center justify-between px-3 py-2 border-b border-rule">
          <span className="text-xs font-medium text-muted">
            Installed Models
            {models.length > 0 && (
              <span className="ml-1.5 rounded-full bg-surface-700 px-1.5 py-0.5 text-xs font-normal text-muted">
                {models.length}
              </span>
            )}
          </span>
        </div>

        {loadingModels ? (
          <p className="px-3 py-4 text-center text-xs text-dim">Loading models...</p>
        ) : models.length === 0 ? (
          <p className="px-3 py-4 text-center text-xs text-dim">
            No models installed. Pull one above.
          </p>
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full text-xs">
              <thead>
                <tr className="border-b border-rule bg-surface-900/60">
                  <th className="px-3 py-2 text-left font-medium uppercase tracking-wider text-dim">Model</th>
                  <th className="px-3 py-2 text-left font-medium uppercase tracking-wider text-dim">Size</th>
                  <th className="px-3 py-2 text-left font-medium uppercase tracking-wider text-dim">Params</th>
                  <th className="px-3 py-2 text-left font-medium uppercase tracking-wider text-dim">Quant</th>
                  <th className="px-3 py-2 text-left font-medium uppercase tracking-wider text-dim">Modified</th>
                  <th className="px-3 py-2 text-right font-medium uppercase tracking-wider text-dim"></th>
                </tr>
              </thead>
              <tbody className="divide-y divide-rule/40">
                {models.map(m => {
                  const isDefault = currentModel === m.name
                  const isSaving = settingDefault === m.name
                  return (
                    <tr key={m.name} className="hover:bg-surface-800/20">
                      <td className="px-3 py-2 font-mono text-fg">
                        <span className="inline-flex items-center gap-2">
                          {isDefault && (
                            <span className="rounded bg-indigo-800/40 px-1.5 py-0.5 text-xs font-medium text-accent border border-indigo-700/40">
                              Default
                            </span>
                          )}
                          {m.name}
                        </span>
                      </td>
                      <td className="px-3 py-2 text-muted">{formatBytes(m.size)}</td>
                      <td className="px-3 py-2 text-muted">{m.details?.parameter_size ?? '—'}</td>
                      <td className="px-3 py-2 text-muted">{m.details?.quantization_level ?? '—'}</td>
                      <td className="px-3 py-2 text-dim">{formatDate(m.modified_at)}</td>
                      <td className="px-3 py-2 text-right">
                        <div className="flex justify-end gap-1.5">
                          {onSetDefaultModel && (
                            <button
                              onClick={() => handleSetDefault(m.name)}
                              disabled={isDefault || isSaving}
                              className="rounded-sm border border-rule px-2 py-0.5 text-xs font-medium text-muted hover:bg-surface-700 disabled:opacity-50 disabled:hover:bg-transparent"
                            >
                              {isDefault ? 'Default' : isSaving ? 'Saving...' : 'Set as default'}
                            </button>
                          )}
                          <button
                            onClick={() => setDeleteTarget(m)}
                            className="rounded-sm border border-red-700/40 px-2 py-0.5 text-xs font-medium text-red-400 hover:bg-red-900/30"
                          >
                            Delete
                          </button>
                        </div>
                      </td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
          </div>
        )}
      </div>

      {/* Running Models */}
      {runningModels.length > 0 && (
        <div className="rounded-sm border border-rule bg-surface-950 overflow-hidden">
          <div className="px-3 py-2 border-b border-rule">
            <span className="text-xs font-medium text-muted">
              Running
              <span className="ml-1.5 rounded-full bg-emerald-800/40 px-1.5 py-0.5 text-xs font-normal text-emerald-300">
                {runningModels.length} loaded
              </span>
            </span>
          </div>
          <div className="overflow-x-auto">
            <table className="w-full text-xs">
              <thead>
                <tr className="border-b border-rule bg-surface-900/60">
                  <th className="px-3 py-2 text-left font-medium uppercase tracking-wider text-dim">Model</th>
                  <th className="px-3 py-2 text-left font-medium uppercase tracking-wider text-dim">VRAM</th>
                  <th className="px-3 py-2 text-left font-medium uppercase tracking-wider text-dim">Expires</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-rule/40">
                {runningModels.map(m => (
                  <tr key={m.name} className="hover:bg-surface-800/20">
                    <td className="px-3 py-2 font-mono text-fg">{m.name}</td>
                    <td className="px-3 py-2 text-muted">{formatBytes(m.size_vram)}</td>
                    <td className="px-3 py-2 text-dim">{formatDate(m.expires_at)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      )}

      {/* Delete confirmation modal */}
      {deleteTarget && (
        <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/60">
          <div className="w-[400px] rounded-sm border border-rule bg-surface-900 p-6 shadow-xl">
            <h2 className="mb-2 text-base font-semibold text-fg">Delete Model</h2>
            <p className="mb-1 text-sm text-muted">
              Delete <span className="font-mono text-fg">{deleteTarget.name}</span>?
            </p>
            <p className="mb-6 text-xs text-dim">
              This will free {formatBytes(deleteTarget.size)} of disk space. This action cannot be undone.
            </p>
            <div className="flex justify-end gap-3">
              <button
                onClick={() => setDeleteTarget(null)}
                className="rounded-sm border border-rule px-4 py-2 text-sm font-medium text-muted hover:bg-surface-800"
              >
                Cancel
              </button>
              <button
                onClick={confirmDelete}
                disabled={deleting}
                className="rounded bg-red-600 px-4 py-2 text-sm font-medium text-white hover:bg-red-500 disabled:opacity-50"
              >
                {deleting ? 'Deleting...' : 'Delete'}
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  )
}
