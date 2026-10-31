import { useState, useEffect, useCallback } from 'react'
import { useApi } from '@/hooks/useApi'
import type { EmbeddingProvider, EmbeddingProviderTestResult } from '@/types'

// ─── Constants ────────────────────────────────────────────────────────────────

// Per-type defaults prefill the modal so the admin doesn't have to remember
// that Ollama's native port is 11434 or that OpenAI's base URL is /v1.
const EMBEDDING_DEFAULTS: Record<string, { endpoint: string; model: string; suggestedName: string; needsApiKey: boolean }> = {
  ollama:             { endpoint: 'http://host.docker.internal:11434',   model: 'nomic-embed-text:latest', suggestedName: 'Ollama Embeddings', needsApiKey: false },
  openai:             { endpoint: 'https://api.openai.com/v1',           model: 'text-embedding-3-small',   suggestedName: 'OpenAI Embeddings', needsApiKey: true  },
  azure_openai:       { endpoint: 'https://<resource>.openai.azure.com', model: 'text-embedding-3-small',   suggestedName: 'Azure OpenAI',      needsApiKey: true  },
  openai_compatible:  { endpoint: 'http://localhost:8000/v1',            model: 'bge-m3',                   suggestedName: 'Custom (OAI-compat)', needsApiKey: false },
}

const EMBEDDING_TYPE_LABELS: Record<string, string> = {
  ollama:            'Ollama (Local)',
  openai:            'OpenAI',
  azure_openai:      'Azure OpenAI',
  openai_compatible: 'OpenAI-Compatible (vLLM, LM Studio, …)',
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
  if (type === 'openai' || type === 'azure_openai') {
    return (
      <span className="rounded px-1.5 py-0.5 text-xs font-medium bg-blue-800/40 text-blue-300 border border-blue-700/40">
        Cloud
      </span>
    )
  }
  return (
    <span className="rounded px-1.5 py-0.5 text-xs font-medium bg-violet-800/40 text-violet-300 border border-violet-700/40">
      Self-hosted
    </span>
  )
}

// ─── Embedding provider card ──────────────────────────────────────────────────

interface EmbeddingCardProps {
  provider: EmbeddingProvider
  onEdit: (p: EmbeddingProvider) => void
  onDelete: (p: EmbeddingProvider) => void
  onSetDefault: (p: EmbeddingProvider) => void
  onToggleEnabled: (p: EmbeddingProvider) => void
  onTested: () => void
}

function EmbeddingCard({ provider, onEdit, onDelete, onSetDefault, onToggleEnabled, onTested }: EmbeddingCardProps) {
  const { post } = useApi()
  const [testing, setTesting] = useState(false)
  const [testResult, setTestResult] = useState<EmbeddingProviderTestResult | null>(null)
  const [confirmDelete, setConfirmDelete] = useState(false)

  async function handleTest() {
    setTesting(true)
    setTestResult(null)
    try {
      const result = await post<EmbeddingProviderTestResult>(
        `/admin/embedding-providers/test?id=${provider.id}`,
        {},
      )
      setTestResult(result ?? { reachable: false, latency_ms: 0, error: 'No response' })
      onTested()
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
          {provider.dimension > 0 && (
            <span className="rounded px-1.5 py-0.5 text-xs font-medium bg-surface-800 text-muted border border-rule">
              {provider.dimension}-D
            </span>
          )}
        </div>

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
          <span className="text-dim">Model:</span>
          <span className="font-mono text-muted">{provider.model || '—'}</span>
        </div>
        <div className="flex items-center gap-2 text-xs">
          <span className="text-dim">API Key:</span>
          {provider.has_api_key ? (
            <span className="text-emerald-400">Configured</span>
          ) : (
            <span className="text-dim">{provider.provider_type === 'ollama' ? 'Not needed' : 'Not set'}</span>
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
          {testing ? 'Testing...' : 'Test'}
        </button>
        {testResult && testResult.reachable && (
          <span className="text-xs text-emerald-400">
            OK · {testResult.dimension}-D · {testResult.latency_ms}ms
            {typeof testResult.vector_norm === 'number' && ` · |v|=${testResult.vector_norm.toFixed(3)}`}
          </span>
        )}
        {testResult && !testResult.reachable && (
          <span className="text-xs text-red-400 break-all">
            {testResult.error ?? 'Unreachable'}
          </span>
        )}
      </div>

      {/* Actions */}
      <div className="flex items-center gap-2 border-t border-rule pt-3">
        <button
          onClick={() => onEdit(provider)}
          className="rounded-sm border border-rule px-3 py-1.5 text-xs text-muted hover:bg-surface-700"
        >
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

// ─── Modal ────────────────────────────────────────────────────────────────────

interface EmbeddingModalProps {
  initial?: EmbeddingProvider
  onClose: () => void
  onSaved: () => void
}

function EmbeddingModal({ initial, onClose, onSaved }: EmbeddingModalProps) {
  const { post, put } = useApi()

  const [provType, setProvType] = useState<EmbeddingProvider['provider_type']>(initial?.provider_type ?? 'ollama')
  const [name, setName] = useState(initial?.name ?? EMBEDDING_DEFAULTS.ollama.suggestedName)
  const [endpoint, setEndpoint] = useState(initial?.endpoint ?? EMBEDDING_DEFAULTS.ollama.endpoint)
  const [model, setModel] = useState(initial?.model ?? EMBEDDING_DEFAULTS.ollama.model)
  const [apiKey, setApiKey] = useState('')
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState('')

  function handleTypeChange(newType: string) {
    const t = newType as EmbeddingProvider['provider_type']
    setProvType(t)
    if (!initial) {
      const def = EMBEDDING_DEFAULTS[t]
      if (def) {
        setName(def.suggestedName)
        setEndpoint(def.endpoint)
        setModel(def.model)
      }
    }
  }

  async function handleSave() {
    setError('')
    if (!name.trim() || !endpoint.trim() || !model.trim()) {
      setError('Name, endpoint, and model are required')
      return
    }

    setSaving(true)
    try {
      if (initial) {
        await put(`/admin/embedding-providers/update?id=${initial.id}`, {
          name,
          endpoint,
          model,
          api_key: apiKey,
          settings: {},
          enabled: initial.enabled,
        })
      } else {
        await post('/admin/embedding-providers/create', {
          name,
          provider_type: provType,
          endpoint,
          model,
          api_key: apiKey,
          settings: {},
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
        <div className="flex items-center justify-between border-b border-rule px-6 py-4">
          <h2 className="text-base font-semibold text-fg">
            {initial ? 'Edit Embedding Provider' : 'Add Embedding Provider'}
          </h2>
          <button onClick={onClose} className="text-muted hover:text-fg">×</button>
        </div>

        <div className="flex-1 overflow-y-auto px-6 py-5 space-y-4">
          {!initial && (
            <div>
              <label className="mb-1 block text-xs font-medium text-muted">Provider Type</label>
              <select
                value={provType}
                onChange={e => handleTypeChange(e.target.value)}
                className="w-full rounded-sm border border-rule bg-surface-950 px-3 py-2 text-sm text-fg focus:outline-none focus:ring-1 focus:ring-accent"
              >
                {Object.entries(EMBEDDING_TYPE_LABELS).map(([val, label]) => (
                  <option key={val} value={val}>{label}</option>
                ))}
              </select>
            </div>
          )}

          <div>
            <label className="mb-1 block text-xs font-medium text-muted">Name</label>
            <input
              value={name}
              onChange={e => setName(e.target.value)}
              placeholder="e.g. Ollama nomic-embed-text"
              className="w-full rounded-sm border border-rule bg-surface-950 px-3 py-2 text-sm text-fg focus:outline-none focus:ring-1 focus:ring-accent"
            />
          </div>

          <div>
            <label className="mb-1 block text-xs font-medium text-muted">Endpoint (base URL, no /embeddings)</label>
            <input
              value={endpoint}
              onChange={e => setEndpoint(e.target.value)}
              placeholder="http://host.docker.internal:11434"
              className="w-full rounded-sm border border-rule bg-surface-950 px-3 py-2 text-sm text-fg font-mono focus:outline-none focus:ring-1 focus:ring-accent"
            />
          </div>

          <div>
            <label className="mb-1 block text-xs font-medium text-muted">Model</label>
            <input
              value={model}
              onChange={e => setModel(e.target.value)}
              placeholder="nomic-embed-text:latest"
              className="w-full rounded-sm border border-rule bg-surface-950 px-3 py-2 text-sm text-fg font-mono focus:outline-none focus:ring-1 focus:ring-accent"
            />
          </div>

          {needsApiKey && (
            <div>
              <label className="mb-1 block text-xs font-medium text-muted">
                API Key{initial?.has_api_key && <span className="ml-1 text-dim">(leave blank to keep existing)</span>}
              </label>
              <input
                type="password"
                value={apiKey}
                onChange={e => setApiKey(e.target.value)}
                placeholder={initial?.has_api_key ? '••••••••' : 'sk-...'}
                className="w-full rounded-sm border border-rule bg-surface-950 px-3 py-2 text-sm text-fg focus:outline-none focus:ring-1 focus:ring-accent"
              />
            </div>
          )}

          <p className="text-xs text-dim">
            After saving, click <span className="font-medium text-muted">Test</span> on the provider card to
            verify connectivity and detect the vector dimension.
          </p>

          {error && <p className="text-xs text-red-400">{error}</p>}
        </div>

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

// ─── Page ─────────────────────────────────────────────────────────────────────

export default function EmbeddingProviders() {
  const { get, post, put } = useApi()
  const [providers, setProviders] = useState<EmbeddingProvider[]>([])
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState('')
  const [showModal, setShowModal] = useState(false)
  const [editTarget, setEditTarget] = useState<EmbeddingProvider | null>(null)

  const fetchProviders = useCallback(async () => {
    setLoading(true)
    setError('')
    try {
      const data = await get<EmbeddingProvider[]>('/admin/embedding-providers')
      setProviders(data ?? [])
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Failed to load embedding providers')
    } finally {
      setLoading(false)
    }
  }, [get])

  useEffect(() => { fetchProviders() }, [fetchProviders])

  async function handleDelete(p: EmbeddingProvider) {
    try {
      await fetch(`/api/admin/embedding-providers/delete?id=${p.id}`, {
        method: 'DELETE',
        headers: { Authorization: `Bearer ${localStorage.getItem('auth_token')}` },
      })
      fetchProviders()
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Delete failed')
    }
  }

  async function handleSetDefault(p: EmbeddingProvider) {
    try {
      await post(`/admin/embedding-providers/default?id=${p.id}`, {})
      fetchProviders()
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Failed to set default')
    }
  }

  async function handleToggleEnabled(p: EmbeddingProvider) {
    try {
      await put(`/admin/embedding-providers/update?id=${p.id}`, {
        name: p.name,
        endpoint: p.endpoint,
        model: p.model,
        api_key: '',
        settings: p.settings,
        enabled: !p.enabled,
      })
      fetchProviders()
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Failed to update provider')
    }
  }

  function openAdd() {
    setEditTarget(null)
    setShowModal(true)
  }

  function openEdit(p: EmbeddingProvider) {
    setEditTarget(p)
    setShowModal(true)
  }

  return (
    <div className="space-y-5">
      <div className="flex items-start justify-between">
        <div>
          <h1 className="text-lg font-semibold text-fg">Embedding Providers</h1>
          <p className="mt-0.5 text-sm text-muted">
            The knowledge-indexer and codex_similar_code tool use the default embedding provider.
            Supported: Ollama, OpenAI, Azure OpenAI, and any OpenAI-compatible endpoint.
          </p>
        </div>
        <button
          onClick={openAdd}
          className="rounded bg-accent px-3 py-1.5 text-sm font-medium text-surface-900 hover:bg-accent-hover"
        >
          + Add Embedding Provider
        </button>
      </div>

      {error && <p className="text-sm text-red-400">{error}</p>}

      {loading ? (
        <p className="text-sm text-muted">Loading...</p>
      ) : providers.length === 0 ? (
        <div className="rounded-sm border border-dashed border-rule p-10 text-center">
          <p className="text-sm font-medium text-muted">No embedding providers configured</p>
          <p className="mt-1 text-xs text-dim">
            Without one, knowledge-indexer refuses to start and codex_similar_code is disabled.
          </p>
          <button
            onClick={openAdd}
            className="mt-4 rounded bg-accent px-4 py-2 text-sm font-medium text-surface-900 hover:bg-accent-hover"
          >
            + Add Embedding Provider
          </button>
        </div>
      ) : (
        <div className="space-y-4">
          {providers.map(p => (
            <EmbeddingCard
              key={p.id}
              provider={p}
              onEdit={openEdit}
              onDelete={handleDelete}
              onSetDefault={handleSetDefault}
              onToggleEnabled={handleToggleEnabled}
              onTested={fetchProviders}
            />
          ))}
        </div>
      )}

      {showModal && (
        <EmbeddingModal
          initial={editTarget ?? undefined}
          onClose={() => setShowModal(false)}
          onSaved={fetchProviders}
        />
      )}
    </div>
  )
}
