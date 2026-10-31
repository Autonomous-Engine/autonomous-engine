import { useState, useEffect, useCallback } from 'react'
import { useApi } from '@/hooks/useApi'
import { guarded } from '@/hooks/rolloutGuard'
import type { LLMProvider, OllamaModel } from '@/types'
import OllamaModelBrowser from './OllamaModelBrowser'

// ─── Constants ────────────────────────────────────────────────────────────────

const PROVIDER_DEFAULTS: Record<string, { endpoint: string; suggestedName: string }> = {
  ollama: { endpoint: 'http://host.docker.internal:11434', suggestedName: 'My Ollama' },
  openai: { endpoint: 'https://api.openai.com/v1', suggestedName: 'OpenAI Account' },
  anthropic: { endpoint: 'https://api.anthropic.com', suggestedName: 'Anthropic' },
  vllm: { endpoint: 'http://localhost:8000/v1', suggestedName: 'My vLLM' },
  custom: { endpoint: '', suggestedName: 'Custom Provider' },
}

const PROVIDER_TYPE_LABELS: Record<string, string> = {
  ollama: 'Ollama (Local)',
  openai: 'OpenAI (Cloud)',
  anthropic: 'Anthropic (Cloud)',
  vllm: 'vLLM (Self-hosted)',
  custom: 'Custom',
}

// ─── Type badge ───────────────────────────────────────────────────────────────

function TypeBadge({ type }: { type: string }) {
  if (type === 'ollama') {
    return (
      <span className="rounded px-1.5 py-0.5 text-xs font-medium bg-emerald-800/40 text-emerald-300 border border-emerald-700/40">
        Local
      </span>
    )
  }
  if (type === 'openai' || type === 'anthropic') {
    return (
      <span className="rounded px-1.5 py-0.5 text-xs font-medium bg-blue-800/40 text-blue-300 border border-blue-700/40">
        Cloud
      </span>
    )
  }
  if (type === 'vllm') {
    return (
      <span className="rounded px-1.5 py-0.5 text-xs font-medium bg-violet-800/40 text-violet-300 border border-violet-700/40">
        Self-hosted
      </span>
    )
  }
  return (
    <span className="rounded px-1.5 py-0.5 text-xs font-medium bg-surface-700 text-muted border border-rule/40">
      Custom
    </span>
  )
}

// ─── OpenAI / Anthropic model list (fetched or hardcoded) ─────────────────────

interface ModelListProps {
  providerId: string
  providerType: string
  currentModel?: string
  onSelectModel?: (model: string) => void
}

function ModelList({ providerId, providerType, currentModel, onSelectModel }: ModelListProps) {
  const { get } = useApi()
  const [models, setModels] = useState<string[]>([])
  const [loading, setLoading] = useState(false)

  useEffect(() => {
    if (providerType !== 'openai' && providerType !== 'anthropic') return
    setLoading(true)
    get<{ models: string[] | Array<{ id: string }> }>(`/admin/providers/models?id=${providerId}`)
      .then(data => {
        if (!data) return
        if (Array.isArray(data.models) && data.models.length > 0) {
          if (typeof data.models[0] === 'string') {
            setModels(data.models as string[])
          } else {
            setModels((data.models as Array<{ id: string }>).map(m => m.id))
          }
        }
      })
      .catch(() => {})
      .finally(() => setLoading(false))
  }, [providerId, providerType, get])

  if (loading) {
    return <p className="text-xs text-dim">Loading models...</p>
  }

  if (models.length === 0) {
    return <p className="text-xs text-dim">No models found. Check API key and connection.</p>
  }

  return (
    <div className="flex flex-wrap gap-1.5">
      {models.map(m => {
        const isActive = currentModel === m
        return (
          <button
            key={m}
            type="button"
            onClick={() => onSelectModel?.(m)}
            className={[
              'rounded border px-2 py-0.5 text-xs font-mono transition-colors',
              isActive
                ? 'border-accent bg-accent/15 text-accent'
                : 'border-rule bg-surface-800 text-muted hover:border-accent/50 hover:text-fg',
            ].join(' ')}
            title={isActive ? 'Active model' : `Switch to ${m}`}
          >
            {isActive && <span className="mr-1">●</span>}
            {m}
          </button>
        )
      })}
    </div>
  )
}

// ─── Provider card ────────────────────────────────────────────────────────────

interface ProviderCardProps {
  provider: LLMProvider
  onEdit: (p: LLMProvider) => void
  onDelete: (p: LLMProvider) => void
  onSetDefault: (p: LLMProvider) => void
  onToggleEnabled: (p: LLMProvider) => void
  onSetDefaultModel: (p: LLMProvider, modelName: string) => Promise<void>
}

function ProviderCard({ provider, onEdit, onDelete, onSetDefault, onToggleEnabled, onSetDefaultModel }: ProviderCardProps) {
  const { post } = useApi()
  const [testing, setTesting] = useState(false)
  const [testResult, setTestResult] = useState<{ reachable: boolean; latency_ms: number; error: string | null } | null>(null)
  const [confirmDelete, setConfirmDelete] = useState(false)

  // For Ollama: installed models bubble up so header can show count
  const [ollamaModels, setOllamaModels] = useState<OllamaModel[]>([])

  async function handleTest() {
    setTesting(true)
    setTestResult(null)
    try {
      const result = await post<{ reachable: boolean; latency_ms: number; error: string | null }>(
        `/admin/providers/test?id=${provider.id}`,
        {},
      )
      setTestResult(result ?? { reachable: false, latency_ms: 0, error: 'No response' })
    } catch (e: unknown) {
      setTestResult({ reachable: false, latency_ms: 0, error: e instanceof Error ? e.message : 'Test failed' })
    } finally {
      setTesting(false)
    }
  }

  return (
    <div className={`rounded-sm border ${provider.enabled ? 'border-rule' : 'border-rule/40 opacity-60'} bg-surface-900 p-5 shadow-sm`}>
      {/* Header */}
      <div className="mb-4 flex items-start justify-between gap-3">
        <div className="flex flex-wrap items-center gap-2">
          <span className="text-sm font-semibold text-fg">{provider.name}</span>
          <TypeBadge type={provider.provider_type} />
          {provider.is_default && (
            <span className="rounded px-1.5 py-0.5 text-xs font-medium bg-indigo-800/40 text-accent border border-indigo-700/40">
              Default
            </span>
          )}
        </div>

        {/* Enabled toggle */}
        <button
          type="button"
          onClick={() => onToggleEnabled(provider)}
          className={`relative inline-flex h-5 w-9 flex-shrink-0 cursor-pointer rounded-full transition-colors focus:outline-none ${provider.enabled ? 'bg-accent' : 'bg-surface-700'}`}
          title={provider.enabled ? 'Disable' : 'Enable'}
        >
          <span
            className={`inline-block h-4 w-4 transform rounded-full bg-white shadow transition-transform mt-0.5 ${provider.enabled ? 'translate-x-4' : 'translate-x-0.5'}`}
          />
        </button>
      </div>

      {/* Connection info */}
      <div className="mb-4 space-y-2">
        <div className="flex items-center gap-2 text-xs">
          <span className="text-dim">Endpoint:</span>
          <span className="font-mono text-muted break-all">{provider.endpoint || '—'}</span>
        </div>
        <div className="flex items-center gap-2 text-xs">
          <span className="text-dim">API Key:</span>
          {provider.has_api_key ? (
            <span className="text-emerald-400">Configured</span>
          ) : (
            <span className="text-dim">Not set</span>
          )}
        </div>
      </div>

      {/* Test connection row */}
      <div className="mb-4 flex items-center gap-3">
        <button
          onClick={handleTest}
          disabled={testing}
          className="rounded-sm border border-rule px-3 py-1.5 text-xs text-muted hover:bg-surface-700 disabled:opacity-50"
        >
          {testing ? 'Testing...' : 'Test Connection'}
        </button>
        {testResult && (
          <span className={`text-xs ${testResult.reachable ? 'text-emerald-400' : 'text-red-400'}`}>
            {testResult.reachable
              ? `Connected (${testResult.latency_ms}ms)`
              : testResult.error ?? 'Unreachable'}
          </span>
        )}
      </div>

      {/* Available models section */}
      <div className="mb-4">
        <p className="mb-2 text-xs font-medium text-muted">Available Models</p>
        {provider.provider_type === 'ollama' ? (
          <OllamaModelBrowser
            endpoint={provider.endpoint}
            onModelsChanged={setOllamaModels}
            providerId={provider.id}
            currentModel={typeof provider.settings?.model === 'string' ? provider.settings.model : undefined}
            onSetDefaultModel={(modelName) => onSetDefaultModel(provider, modelName)}
          />
        ) : provider.provider_type === 'openai' || provider.provider_type === 'anthropic' ? (
          <ModelList
            providerId={provider.id}
            providerType={provider.provider_type}
            currentModel={typeof provider.settings?.model === 'string' ? provider.settings.model : undefined}
            onSelectModel={(modelName) => onSetDefaultModel(provider, modelName)}
          />
        ) : (
          <p className="text-xs text-dim italic">
            Enter model name when assigning to agents.
          </p>
        )}
        {provider.provider_type === 'ollama' && ollamaModels.length > 0 && (
          <p className="mt-1 text-xs text-dim">{ollamaModels.length} model(s) installed</p>
        )}
      </div>

      {/* Actions */}
      <div className="flex items-center gap-2 border-t border-rule pt-3">
        <button
          onClick={() => onEdit(provider)}
          className="flex items-center gap-1.5 rounded-sm border border-rule px-3 py-1.5 text-xs text-muted hover:bg-surface-700"
          title="Edit"
        >
          <svg className="h-3.5 w-3.5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
            <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M15.232 5.232l3.536 3.536m-2.036-5.036a2.5 2.5 0 113.536 3.536L6.5 21.036H3v-3.572L16.732 3.732z" />
          </svg>
          Edit
        </button>

        {!provider.is_default && (
          <button
            onClick={() => onSetDefault(provider)}
            className="rounded-sm border border-rule px-3 py-1.5 text-xs text-muted hover:bg-surface-700"
          >
            Set as Default
          </button>
        )}

        {confirmDelete ? (
          <div className="ml-auto flex items-center gap-2">
            <span className="text-xs text-red-400">Delete this provider?</span>
            <button
              onClick={() => { onDelete(provider); setConfirmDelete(false) }}
              className="rounded bg-red-700 px-2.5 py-1 text-xs font-medium text-white hover:bg-red-600"
            >
              Yes, delete
            </button>
            <button
              onClick={() => setConfirmDelete(false)}
              className="rounded-sm border border-rule px-2.5 py-1 text-xs text-muted hover:bg-surface-700"
            >
              Cancel
            </button>
          </div>
        ) : (
          <button
            onClick={() => setConfirmDelete(true)}
            className="ml-auto rounded-sm border border-red-700/40 px-3 py-1.5 text-xs text-red-400 hover:bg-red-900/30"
          >
            Delete
          </button>
        )}
      </div>
    </div>
  )
}

// ─── Provider modal (add / edit) ──────────────────────────────────────────────

interface ProviderModalProps {
  initial?: LLMProvider
  onClose: () => void
  onSaved: () => void
}

function ProviderModal({ initial, onClose, onSaved }: ProviderModalProps) {
  const { post, put } = useApi()

  const [provType, setProvType] = useState(initial?.provider_type ?? 'ollama')
  const [name, setName] = useState(initial?.name ?? PROVIDER_DEFAULTS.ollama.suggestedName)
  const [endpoint, setEndpoint] = useState(initial?.endpoint ?? PROVIDER_DEFAULTS.ollama.endpoint)
  const [apiKey, setApiKey] = useState('')
  const [showKey, setShowKey] = useState(false)
  const [settings, setSettings] = useState<string>(
    initial?.settings ? JSON.stringify(initial.settings, null, 2) : '{}'
  )
  const [testing, setTesting] = useState(false)
  const [testResult, setTestResult] = useState<string | null>(null)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState('')

  // When type changes, pre-fill name + endpoint if not editing
  function handleTypeChange(newType: string) {
    setProvType(newType as LLMProvider['provider_type'])
    if (!initial) {
      const def = PROVIDER_DEFAULTS[newType]
      setName(def?.suggestedName ?? '')
      setEndpoint(def?.endpoint ?? '')
    }
  }

  async function handleTest() {
    // Can only test if we have an existing provider to test against, or after saving.
    // For new providers we'd need to save first; show a note.
    if (!initial) {
      setTestResult('Save the provider first to test the connection.')
      return
    }
    setTesting(true)
    setTestResult(null)
    try {
      const result = await post<{ reachable: boolean; latency_ms: number; error: string | null }>(
        `/admin/providers/test?id=${initial.id}`,
        {},
      )
      if (result?.reachable) {
        setTestResult(`Connected (${result.latency_ms}ms)`)
      } else {
        setTestResult(`Failed: ${result?.error ?? 'unreachable'}`)
      }
    } catch (e: unknown) {
      setTestResult(e instanceof Error ? e.message : 'Test failed')
    } finally {
      setTesting(false)
    }
  }

  async function handleSave() {
    setError('')
    if (!name.trim()) {
      setError('Name is required')
      return
    }

    let parsedSettings: Record<string, unknown> = {}
    try {
      parsedSettings = JSON.parse(settings || '{}')
    } catch {
      setError('Settings must be valid JSON')
      return
    }

    setSaving(true)
    try {
      if (initial) {
        await guarded(`/admin/providers/update?id=${initial.id}`, p => put(p, {
          name,
          endpoint,
          api_key: apiKey,
          settings: parsedSettings,
          enabled: initial.enabled,
        }))
      } else {
        await post('/admin/providers/create', {
          name,
          provider_type: provType,
          endpoint,
          api_key: apiKey,
          settings: parsedSettings,
        })
      }
      onSaved()
      onClose()
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Save failed')
    } finally {
      setSaving(false)
    }
  }

  const needsApiKey = provType !== 'ollama'

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/60">
      <div className="flex w-[540px] max-h-[90vh] flex-col rounded-sm border border-rule bg-surface-900 shadow-xl">
        {/* Header */}
        <div className="flex items-center justify-between border-b border-rule px-6 py-4">
          <h2 className="text-base font-semibold text-fg">
            {initial ? 'Edit Provider' : 'Add Provider'}
          </h2>
          <button onClick={onClose} className="text-muted hover:text-fg">✕</button>
        </div>

        {/* Body */}
        <div className="flex-1 overflow-y-auto px-6 py-5 space-y-4">
          {/* Provider type — only when adding */}
          {!initial && (
            <div>
              <label className="mb-1 block text-xs font-medium text-muted">Provider Type</label>
              <select
                value={provType}
                onChange={e => handleTypeChange(e.target.value)}
                className="w-full rounded-sm border border-rule bg-surface-950 px-3 py-2 text-sm text-fg focus:outline-none focus:ring-1 focus:ring-accent"
              >
                {Object.entries(PROVIDER_TYPE_LABELS).map(([val, label]) => (
                  <option key={val} value={val}>{label}</option>
                ))}
              </select>
            </div>
          )}

          {/* Name */}
          <div>
            <label className="mb-1 block text-xs font-medium text-muted">Name</label>
            <input
              value={name}
              onChange={e => setName(e.target.value)}
              placeholder="e.g. My Local Ollama"
              className="w-full rounded-sm border border-rule bg-surface-950 px-3 py-2 text-sm text-fg focus:outline-none focus:ring-1 focus:ring-accent"
            />
          </div>

          {/* Endpoint */}
          <div>
            <label className="mb-1 block text-xs font-medium text-muted">Endpoint</label>
            <input
              value={endpoint}
              onChange={e => setEndpoint(e.target.value)}
              placeholder="http://host.docker.internal:11434"
              className="w-full rounded-sm border border-rule bg-surface-950 px-3 py-2 text-sm text-fg font-mono focus:outline-none focus:ring-1 focus:ring-accent"
            />
          </div>

          {/* API Key — hidden for Ollama */}
          {needsApiKey && (
            <div>
              <label className="mb-1 block text-xs font-medium text-muted">
                API Key{initial?.has_api_key && <span className="ml-1 text-dim">(leave blank to keep existing)</span>}
              </label>
              <div className="flex gap-2">
                <input
                  type={showKey ? 'text' : 'password'}
                  value={apiKey}
                  onChange={e => setApiKey(e.target.value)}
                  placeholder={initial?.has_api_key ? '••••••••' : 'sk-...'}
                  className="flex-1 rounded-sm border border-rule bg-surface-950 px-3 py-2 text-sm text-fg focus:outline-none focus:ring-1 focus:ring-accent"
                />
                <button
                  type="button"
                  onClick={() => setShowKey(v => !v)}
                  className="rounded-sm border border-rule px-2 py-2 text-muted hover:text-fg"
                  title={showKey ? 'Hide' : 'Show'}
                >
                  {showKey ? (
                    <svg className="h-4 w-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                      <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M13.875 18.825A10.05 10.05 0 0112 19c-4.478 0-8.268-2.943-9.543-7a9.97 9.97 0 011.563-3.029m5.858.908a3 3 0 114.243 4.243M9.878 9.878l4.242 4.242M9.88 9.88l-3.29-3.29m7.532 7.532l3.29 3.29M3 3l3.59 3.59m0 0A9.953 9.953 0 0112 5c4.478 0 8.268 2.943 9.543 7a10.025 10.025 0 01-4.132 5.411m0 0L21 21" />
                    </svg>
                  ) : (
                    <svg className="h-4 w-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                      <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M15 12a3 3 0 11-6 0 3 3 0 016 0z" />
                      <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M2.458 12C3.732 7.943 7.523 5 12 5c4.478 0 8.268 2.943 9.542 7-1.274 4.057-5.064 7-9.542 7-4.477 0-8.268-2.943-9.542-7z" />
                    </svg>
                  )}
                </button>
              </div>
            </div>
          )}

          {/* Settings JSON */}
          <div>
            <label className="mb-1 block text-xs font-medium text-muted">
              Settings <span className="text-dim">(JSON — provider-specific config)</span>
            </label>
            <textarea
              value={settings}
              onChange={e => setSettings(e.target.value)}
              rows={3}
              className="w-full rounded-sm border border-rule bg-surface-950 px-3 py-2 text-xs text-muted font-mono focus:outline-none focus:ring-1 focus:ring-accent"
            />
          </div>

          {/* Test connection */}
          <div className="flex items-center gap-3">
            <button
              onClick={handleTest}
              disabled={testing}
              className="rounded-sm border border-rule px-3 py-1.5 text-xs text-muted hover:bg-surface-700 disabled:opacity-50"
            >
              {testing ? 'Testing...' : 'Test Connection'}
            </button>
            {testResult && (
              <span className={`text-xs ${testResult.startsWith('Connected') ? 'text-emerald-400' : 'text-amber-400'}`}>
                {testResult}
              </span>
            )}
          </div>

          {error && <p className="text-xs text-red-400">{error}</p>}
        </div>

        {/* Footer */}
        <div className="flex justify-end gap-3 border-t border-rule px-6 py-4">
          <button
            onClick={onClose}
            className="rounded-sm border border-rule px-4 py-2 text-sm font-medium text-muted hover:bg-surface-700"
          >
            Cancel
          </button>
          <button
            onClick={handleSave}
            disabled={saving}
            className="rounded bg-accent px-4 py-2 text-sm font-medium text-surface-900 hover:bg-accent-hover disabled:opacity-50"
          >
            {saving ? 'Saving...' : initial ? 'Save Changes' : 'Add Provider'}
          </button>
        </div>
      </div>
    </div>
  )
}

// ─── ProvidersPage ────────────────────────────────────────────────────────────

export default function ProvidersPage() {
  const { get, post, put, del } = useApi()
  const [providers, setProviders] = useState<LLMProvider[]>([])
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState('')
  const [showModal, setShowModal] = useState(false)
  const [editTarget, setEditTarget] = useState<LLMProvider | null>(null)

  const fetchProviders = useCallback(async () => {
    setLoading(true)
    setError('')
    try {
      const data = await get<LLMProvider[]>('/admin/providers')
      setProviders(data ?? [])
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Failed to load providers')
    } finally {
      setLoading(false)
    }
  }, [get])

  useEffect(() => { fetchProviders() }, [fetchProviders])

  async function handleDelete(p: LLMProvider) {
    try {
      await guarded(`/admin/providers/delete?id=${p.id}`, path => del(path))
      fetchProviders()
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Delete failed')
    }
  }

  async function handleSetDefault(p: LLMProvider) {
    try {
      await guarded(`/admin/providers/set-default?id=${p.id}`, path => post(path, {}))
      fetchProviders()
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Failed to set default')
    }
  }

  async function handleToggleEnabled(p: LLMProvider) {
    try {
      await guarded(`/admin/providers/update?id=${p.id}`, path => put(path, {
        name: p.name,
        endpoint: p.endpoint,
        api_key: '',
        settings: p.settings,
        enabled: !p.enabled,
      }))
      fetchProviders()
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Failed to update provider')
    }
  }

  async function handleSetDefaultModel(p: LLMProvider, modelName: string) {
    await guarded(`/admin/providers/update?id=${p.id}`, path => put(path, {
      name: p.name,
      endpoint: p.endpoint,
      api_key: '',
      settings: { ...p.settings, model: modelName },
      enabled: p.enabled,
    }))
    fetchProviders()
  }

  function openAdd() {
    setEditTarget(null)
    setShowModal(true)
  }

  function openEdit(p: LLMProvider) {
    setEditTarget(p)
    setShowModal(true)
  }

  return (
    <div className="space-y-5">
      {/* Page header */}
      <div className="flex items-start justify-between">
        <div>
          <h1 className="text-lg font-semibold text-fg">LLM Providers</h1>
          <p className="mt-0.5 text-sm text-muted">
            Connect to LLM services. Add providers here, then assign models to agents.
          </p>
        </div>
        <button
          onClick={openAdd}
          className="rounded bg-accent px-3 py-1.5 text-sm font-medium text-surface-900 hover:bg-accent-hover"
        >
          + Add Provider
        </button>
      </div>

      {error && <p className="text-sm text-red-400">{error}</p>}

      {loading ? (
        <p className="text-sm text-muted">Loading...</p>
      ) : providers.length === 0 ? (
        <div className="rounded-sm border border-dashed border-rule p-10 text-center">
          <p className="text-sm font-medium text-muted">No providers configured</p>
          <p className="mt-1 text-xs text-dim">
            Add your first LLM provider to get started.
          </p>
          <button
            onClick={openAdd}
            className="mt-4 rounded bg-accent px-4 py-2 text-sm font-medium text-surface-900 hover:bg-accent-hover"
          >
            + Add Provider
          </button>
        </div>
      ) : (
        <div className="space-y-4">
          {providers.map(p => (
            <ProviderCard
              key={p.id}
              provider={p}
              onEdit={openEdit}
              onDelete={handleDelete}
              onSetDefault={handleSetDefault}
              onToggleEnabled={handleToggleEnabled}
              onSetDefaultModel={handleSetDefaultModel}
            />
          ))}
        </div>
      )}

      {/* Add / Edit modal */}
      {showModal && (
        <ProviderModal
          initial={editTarget ?? undefined}
          onClose={() => setShowModal(false)}
          onSaved={fetchProviders}
        />
      )}
    </div>
  )
}
