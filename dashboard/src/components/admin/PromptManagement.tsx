import { useState, useEffect, useCallback } from 'react'
import { useApi } from '@/hooks/useApi'
import { guarded } from '@/hooks/rolloutGuard'

// ─── Types ────────────────────────────────────────────────────────────────────

interface PromptTemplate {
  id: string
  intent_type: string
  agent_type: string
  provider: string
  model_id: string
  operating_mode: string
  prompt_type: string
  content: string
  description: string
  is_default: boolean
  created_by?: string
  created_at: string
  updated_at: string
}

interface ResolutionEntry {
  template: PromptTemplate
  wins: boolean
}

interface ResolutionResult {
  chain: ResolutionEntry[]
  winner: PromptTemplate | null
}

// ─── Constants ────────────────────────────────────────────────────────────────

const AGENT_TYPES = [
  'product_agent',
  'research_coordinator',
  'research_agent',
  'implementation_planner',
  'implementation_agent',
  'pr_coordinator',
  'review_agent',
  'qa_agent',
]

const PROVIDERS = ['anthropic', 'openai', 'ollama', 'custom']

const OPERATING_MODES = ['constrained', 'standard', 'advanced']

const PROMPT_TYPES = ['system', 'instruction']

function badge(label: string, color: string) {
  if (!label) return null
  return (
    <span
      className={`inline-flex items-center rounded px-1.5 py-0.5 text-xs font-medium ${color}`}
    >
      {label}
    </span>
  )
}

function formatDate(iso: string) {
  return new Date(iso).toLocaleString()
}

// ─── Add Override Modal ───────────────────────────────────────────────────────

interface AddOverrideModalProps {
  intentType: string
  onClose: () => void
  onSaved: () => void
}

function AddOverrideModal({ intentType, onClose, onSaved }: AddOverrideModalProps) {
  const { post } = useApi()
  const [form, setForm] = useState({
    agent_type: '',
    provider: '',
    model_id: '',
    operating_mode: '',
    prompt_type: 'system',
    content: '',
    description: '',
  })
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)

  function set(key: string, value: string) {
    setForm(f => ({ ...f, [key]: value }))
  }

  async function handleSave() {
    setError('')
    setLoading(true)
    try {
      await guarded('/admin/prompts/create', p => post(p, {
        intent_type: intentType,
        ...form,
      }))
      onSaved()
      onClose()
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Failed to create')
    } finally {
      setLoading(false)
    }
  }

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/60">
      <div className="w-[560px] rounded-sm border border-rule bg-surface-900 p-6 shadow-xl">
        <h2 className="mb-4 text-base font-semibold text-fg">
          Add Override — <span className="text-accent">{intentType}</span>
        </h2>

        <div className="space-y-3">
          <div className="grid grid-cols-2 gap-3">
            <div>
              <label className="mb-1 block text-xs font-medium text-muted">Agent Type</label>
              <select
                value={form.agent_type}
                onChange={e => set('agent_type', e.target.value)}
                className="w-full rounded-sm border border-rule bg-surface-950 px-3 py-2 text-sm text-fg focus:outline-none focus:ring-1 focus:ring-accent"
              >
                <option value="">Any (global)</option>
                {AGENT_TYPES.map(a => (
                  <option key={a} value={a}>{a}</option>
                ))}
              </select>
            </div>
            <div>
              <label className="mb-1 block text-xs font-medium text-muted">Provider</label>
              <select
                value={form.provider}
                onChange={e => set('provider', e.target.value)}
                className="w-full rounded-sm border border-rule bg-surface-950 px-3 py-2 text-sm text-fg focus:outline-none focus:ring-1 focus:ring-accent"
              >
                <option value="">Any</option>
                {PROVIDERS.map(p => (
                  <option key={p} value={p}>{p}</option>
                ))}
              </select>
            </div>
          </div>

          <div className="grid grid-cols-2 gap-3">
            <div>
              <label className="mb-1 block text-xs font-medium text-muted">Model ID</label>
              <input
                type="text"
                placeholder="e.g. claude-opus-4-5 (leave blank for any)"
                value={form.model_id}
                onChange={e => set('model_id', e.target.value)}
                className="w-full rounded-sm border border-rule bg-surface-950 px-3 py-2 text-sm text-fg placeholder-dim focus:outline-none focus:ring-1 focus:ring-accent"
              />
            </div>
            <div>
              <label className="mb-1 block text-xs font-medium text-muted">Operating Mode</label>
              <select
                value={form.operating_mode}
                onChange={e => set('operating_mode', e.target.value)}
                className="w-full rounded-sm border border-rule bg-surface-950 px-3 py-2 text-sm text-fg focus:outline-none focus:ring-1 focus:ring-accent"
              >
                <option value="">Any</option>
                {OPERATING_MODES.map(m => (
                  <option key={m} value={m}>{m}</option>
                ))}
              </select>
            </div>
          </div>

          <div>
            <label className="mb-1 block text-xs font-medium text-muted">Prompt Type</label>
            <select
              value={form.prompt_type}
              onChange={e => set('prompt_type', e.target.value)}
              className="w-full rounded-sm border border-rule bg-surface-950 px-3 py-2 text-sm text-fg focus:outline-none focus:ring-1 focus:ring-accent"
            >
              {PROMPT_TYPES.map(pt => (
                <option key={pt} value={pt}>{pt}</option>
              ))}
            </select>
          </div>

          <div>
            <label className="mb-1 block text-xs font-medium text-muted">Prompt Content</label>
            <textarea
              rows={6}
              value={form.content}
              onChange={e => set('content', e.target.value)}
              className="w-full rounded-sm border border-rule bg-surface-950 px-3 py-2 font-mono text-sm text-fg focus:outline-none focus:ring-1 focus:ring-accent"
              placeholder="Enter prompt content..."
            />
          </div>

          <div>
            <label className="mb-1 block text-xs font-medium text-muted">Description</label>
            <input
              type="text"
              value={form.description}
              onChange={e => set('description', e.target.value)}
              className="w-full rounded-sm border border-rule bg-surface-950 px-3 py-2 text-sm text-fg placeholder-dim focus:outline-none focus:ring-1 focus:ring-accent"
              placeholder="Brief description of this override"
            />
          </div>
        </div>

        {error && <p className="mt-3 text-xs text-red-400">{error}</p>}

        <div className="mt-4 flex justify-end gap-2">
          <button
            onClick={onClose}
            className="rounded px-3 py-1.5 text-sm text-muted hover:text-fg"
          >
            Cancel
          </button>
          <button
            onClick={handleSave}
            disabled={loading || !form.content}
            className="rounded bg-accent px-3 py-1.5 text-sm font-medium text-surface-900 hover:bg-accent-hover disabled:opacity-50"
          >
            {loading ? 'Saving...' : 'Save Override'}
          </button>
        </div>
      </div>
    </div>
  )
}

// ─── Edit Prompt Modal ────────────────────────────────────────────────────────

interface EditPromptModalProps {
  template: PromptTemplate
  onClose: () => void
  onSaved: () => void
}

function EditPromptModal({ template, onClose, onSaved }: EditPromptModalProps) {
  const { put } = useApi()
  const [content, setContent] = useState(template.content)
  const [description, setDescription] = useState(template.description)
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)

  async function handleSave() {
    setError('')
    setLoading(true)
    try {
      await guarded(`/admin/prompts/update?id=${template.id}`, p => put(p, { content, description }))
      onSaved()
      onClose()
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Failed to update')
    } finally {
      setLoading(false)
    }
  }

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/60">
      <div className="w-[560px] rounded-sm border border-rule bg-surface-900 p-6 shadow-xl">
        <h2 className="mb-1 text-base font-semibold text-fg">Edit Prompt</h2>
        <p className="mb-4 text-xs text-dim">
          {template.intent_type}
          {template.agent_type && ` · ${template.agent_type}`}
          {template.provider && ` · ${template.provider}`}
          {template.model_id && ` · ${template.model_id}`}
          {template.operating_mode && ` · ${template.operating_mode}`}
        </p>

        <div className="space-y-3">
          <div>
            <label className="mb-1 block text-xs font-medium text-muted">Content</label>
            <textarea
              rows={8}
              value={content}
              onChange={e => setContent(e.target.value)}
              className="w-full rounded-sm border border-rule bg-surface-950 px-3 py-2 font-mono text-sm text-fg focus:outline-none focus:ring-1 focus:ring-accent"
            />
          </div>
          <div>
            <label className="mb-1 block text-xs font-medium text-muted">Description</label>
            <input
              type="text"
              value={description}
              onChange={e => setDescription(e.target.value)}
              className="w-full rounded-sm border border-rule bg-surface-950 px-3 py-2 text-sm text-fg focus:outline-none focus:ring-1 focus:ring-accent"
            />
          </div>
        </div>

        {error && <p className="mt-3 text-xs text-red-400">{error}</p>}

        <div className="mt-4 flex justify-end gap-2">
          <button onClick={onClose} className="rounded px-3 py-1.5 text-sm text-muted hover:text-fg">
            Cancel
          </button>
          <button
            onClick={handleSave}
            disabled={loading || !content}
            className="rounded bg-accent px-3 py-1.5 text-sm font-medium text-surface-900 hover:bg-accent-hover disabled:opacity-50"
          >
            {loading ? 'Saving...' : 'Save Changes'}
          </button>
        </div>
      </div>
    </div>
  )
}

// ─── Resolution Preview Modal ─────────────────────────────────────────────────

interface ResolutionPreviewModalProps {
  intentType: string
  onClose: () => void
}

function ResolutionPreviewModal({ intentType, onClose }: ResolutionPreviewModalProps) {
  const { get } = useApi()
  const [agentType, setAgentType] = useState('')
  const [provider, setProvider] = useState('')
  const [modelId, setModelId] = useState('')
  const [operatingMode, setOperatingMode] = useState('')
  const [result, setResult] = useState<ResolutionResult | null>(null)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState('')

  async function handlePreview() {
    setError('')
    setLoading(true)
    try {
      const params = new URLSearchParams({
        intent_type: intentType,
        agent_type: agentType,
        provider,
        model_id: modelId,
        operating_mode: operatingMode,
      })
      const res = await get<ResolutionResult>(`/admin/prompts/resolve?${params}`)
      setResult(res)
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Failed to resolve')
    } finally {
      setLoading(false)
    }
  }

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/60">
      <div className="w-[640px] max-h-[90vh] overflow-y-auto rounded-sm border border-rule bg-surface-900 p-6 shadow-xl">
        <h2 className="mb-1 text-base font-semibold text-fg">Preview Resolution</h2>
        <p className="mb-4 text-xs text-dim">
          See which template wins for a given combination of agent, provider, model, and mode.
        </p>

        <div className="mb-4 grid grid-cols-2 gap-3">
          <div>
            <label className="mb-1 block text-xs font-medium text-muted">Agent Type</label>
            <select
              value={agentType}
              onChange={e => setAgentType(e.target.value)}
              className="w-full rounded-sm border border-rule bg-surface-950 px-3 py-2 text-sm text-fg focus:outline-none focus:ring-1 focus:ring-accent"
            >
              <option value="">Any</option>
              {AGENT_TYPES.map(a => <option key={a} value={a}>{a}</option>)}
            </select>
          </div>
          <div>
            <label className="mb-1 block text-xs font-medium text-muted">Provider</label>
            <select
              value={provider}
              onChange={e => setProvider(e.target.value)}
              className="w-full rounded-sm border border-rule bg-surface-950 px-3 py-2 text-sm text-fg focus:outline-none focus:ring-1 focus:ring-accent"
            >
              <option value="">Any</option>
              {PROVIDERS.map(p => <option key={p} value={p}>{p}</option>)}
            </select>
          </div>
          <div>
            <label className="mb-1 block text-xs font-medium text-muted">Model ID</label>
            <input
              type="text"
              value={modelId}
              onChange={e => setModelId(e.target.value)}
              placeholder="e.g. qwen2.5:16b"
              className="w-full rounded-sm border border-rule bg-surface-950 px-3 py-2 text-sm text-fg placeholder-dim focus:outline-none focus:ring-1 focus:ring-accent"
            />
          </div>
          <div>
            <label className="mb-1 block text-xs font-medium text-muted">Operating Mode</label>
            <select
              value={operatingMode}
              onChange={e => setOperatingMode(e.target.value)}
              className="w-full rounded-sm border border-rule bg-surface-950 px-3 py-2 text-sm text-fg focus:outline-none focus:ring-1 focus:ring-accent"
            >
              <option value="">Any</option>
              {OPERATING_MODES.map(m => <option key={m} value={m}>{m}</option>)}
            </select>
          </div>
        </div>

        <button
          onClick={handlePreview}
          disabled={loading}
          className="mb-4 rounded bg-accent px-4 py-2 text-sm font-medium text-surface-900 hover:bg-accent-hover disabled:opacity-50"
        >
          {loading ? 'Resolving...' : 'Resolve'}
        </button>

        {error && <p className="mb-3 text-xs text-red-400">{error}</p>}

        {result && (
          <div className="space-y-2">
            <p className="text-xs font-semibold uppercase tracking-widest text-dim">
              Resolution Chain ({result.chain.length} candidate{result.chain.length !== 1 ? 's' : ''})
            </p>
            {result.chain.length === 0 && (
              <p className="text-sm text-muted">No templates found — hardcoded fallback will be used.</p>
            )}
            {result.chain.map((entry, i) => (
              <div
                key={entry.template.id}
                className={[
                  'rounded-sm border p-3',
                  entry.wins
                    ? 'border-emerald-600 bg-emerald-900/20'
                    : 'border-rule bg-surface-950',
                ].join(' ')}
              >
                <div className="mb-1 flex items-center gap-2">
                  {entry.wins && (
                    <span className="text-xs font-bold text-emerald-400">WINS</span>
                  )}
                  <span className="text-xs text-dim">#{i + 1}</span>
                  <div className="flex flex-wrap gap-1">
                    {badge(entry.template.agent_type || 'any agent', 'bg-green-800 text-green-200')}
                    {badge(entry.template.provider || 'any provider', 'bg-blue-800 text-blue-200')}
                    {badge(entry.template.model_id || 'any model', 'bg-purple-800 text-purple-200')}
                    {badge(entry.template.operating_mode || 'any mode', 'bg-amber-800 text-amber-200')}
                  </div>
                </div>
                <p className="truncate font-mono text-xs text-muted">
                  {entry.template.content.slice(0, 120)}
                  {entry.template.content.length > 120 ? '…' : ''}
                </p>
              </div>
            ))}
          </div>
        )}

        <div className="mt-4 flex justify-end">
          <button onClick={onClose} className="rounded px-3 py-1.5 text-sm text-muted hover:text-fg">
            Close
          </button>
        </div>
      </div>
    </div>
  )
}

// ─── Template Card ────────────────────────────────────────────────────────────

interface TemplateCardProps {
  template: PromptTemplate
  onEdit: (t: PromptTemplate) => void
  onDelete: (id: string) => void
}

function TemplateCard({ template, onEdit, onDelete }: TemplateCardProps) {
  const [expanded, setExpanded] = useState(false)

  const isGlobal =
    !template.agent_type && !template.provider && !template.model_id && !template.operating_mode

  return (
    <div
      className={[
        'rounded-sm border p-4',
        template.is_default
          ? 'border-rule bg-surface-950'
          : 'border-indigo-800/40 bg-indigo-950/20',
      ].join(' ')}
    >
      <div className="mb-2 flex items-start justify-between gap-2">
        <div className="flex flex-wrap items-center gap-1.5">
          {template.is_default && (
            <span className="rounded bg-surface-700 px-1.5 py-0.5 text-xs font-medium text-muted">
              default — hardcoded fallback
            </span>
          )}
          {isGlobal && !template.is_default && (
            <span className="rounded bg-surface-700 px-1.5 py-0.5 text-xs font-medium text-muted">
              global override
            </span>
          )}
          {badge(template.agent_type, 'bg-green-800 text-green-200')}
          {badge(template.provider, 'bg-blue-800 text-blue-200')}
          {badge(template.model_id, 'bg-purple-800 text-purple-200')}
          {badge(template.operating_mode, 'bg-amber-800 text-amber-200')}
          {badge(template.prompt_type, 'bg-surface-700 text-fg')}
        </div>
        {!template.is_default && (
          <div className="flex shrink-0 gap-1">
            <button
              onClick={() => onEdit(template)}
              className="rounded px-2 py-1 text-xs text-muted hover:bg-surface-700 hover:text-fg"
            >
              Edit
            </button>
            <button
              onClick={() => onDelete(template.id)}
              className="rounded px-2 py-1 text-xs text-muted hover:bg-red-900/40 hover:text-red-300"
            >
              Delete
            </button>
          </div>
        )}
      </div>

      {template.description && (
        <p className="mb-2 text-xs text-dim">{template.description}</p>
      )}

      <div
        className={[
          'relative overflow-hidden rounded-sm border border-rule bg-surface-900 p-3',
          !expanded ? 'max-h-24' : '',
        ].join(' ')}
      >
        <pre className="whitespace-pre-wrap font-mono text-xs text-muted leading-relaxed">
          {template.content}
        </pre>
        {!expanded && template.content.length > 200 && (
          <div className="pointer-events-none absolute inset-x-0 bottom-0 h-8 bg-gradient-to-t from-surface-900" />
        )}
      </div>

      <div className="mt-2 flex items-center justify-between">
        {template.content.length > 200 && (
          <button
            onClick={() => setExpanded(e => !e)}
            className="text-xs text-accent-dim hover:text-accent"
          >
            {expanded ? 'Show less' : 'Show full prompt'}
          </button>
        )}
        <span className="ml-auto text-xs text-dim">
          Updated {formatDate(template.updated_at)}
        </span>
      </div>
    </div>
  )
}

// ─── Main Page ────────────────────────────────────────────────────────────────

export default function PromptManagement() {
  const { get, del } = useApi()

  const [intentTypes, setIntentTypes] = useState<string[]>([])
  const [selectedIntent, setSelectedIntent] = useState<string | null>(null)
  const [templates, setTemplates] = useState<PromptTemplate[]>([])
  const [loadingIntents, setLoadingIntents] = useState(false)
  const [loadingTemplates, setLoadingTemplates] = useState(false)
  const [error, setError] = useState('')

  const [showAddModal, setShowAddModal] = useState(false)
  const [editTarget, setEditTarget] = useState<PromptTemplate | null>(null)
  const [showResolutionModal, setShowResolutionModal] = useState(false)

  // Fetch intent types on mount.
  const loadIntentTypes = useCallback(async () => {
    setLoadingIntents(true)
    try {
      const types = await get<string[]>('/admin/prompts/intents')
      setIntentTypes(types ?? [])
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Failed to load intent types')
    } finally {
      setLoadingIntents(false)
    }
  }, [get])

  useEffect(() => {
    loadIntentTypes()
  }, [loadIntentTypes])

  // Fetch templates whenever the selected intent changes.
  const loadTemplates = useCallback(async (intent: string) => {
    setLoadingTemplates(true)
    setError('')
    try {
      const params = new URLSearchParams({ intent_type: intent })
      const result = await get<PromptTemplate[]>(`/admin/prompts?${params}`)
      setTemplates(result ?? [])
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Failed to load templates')
    } finally {
      setLoadingTemplates(false)
    }
  }, [get])

  useEffect(() => {
    if (selectedIntent) {
      loadTemplates(selectedIntent)
    }
  }, [selectedIntent, loadTemplates])

  async function handleDelete(id: string) {
    if (!confirm('Delete this prompt template?')) return
    try {
      await guarded(`/admin/prompts/delete?id=${id}`, p => del(p))
      if (selectedIntent) loadTemplates(selectedIntent)
      loadIntentTypes()
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Failed to delete')
    }
  }

  function handleSaved() {
    if (selectedIntent) loadTemplates(selectedIntent)
    loadIntentTypes()
  }

  // Split templates into defaults and overrides.
  const defaultTemplates = templates.filter(t => t.is_default)
  const overrideTemplates = templates.filter(t => !t.is_default)

  // Persona intents get a friendly display label.
  function intentLabel(intent: string) {
    if (intent === '__persona__') return 'Agent Personas'
    return intent.replace(/_/g, ' ')
  }

  return (
    <div className="flex h-full gap-0">
      {/* ── Left sidebar: intent list ── */}
      <aside className="w-52 shrink-0 border-r border-rule overflow-y-auto">
        <div className="px-3 py-3 border-b border-rule">
          <p className="text-xs font-semibold uppercase tracking-widest text-dim">
            Intent Types
          </p>
        </div>

        {loadingIntents ? (
          <p className="px-3 py-4 text-xs text-dim">Loading...</p>
        ) : (
          <nav className="py-2 space-y-0.5 px-2">
            {/* Personas always first */}
            {['__persona__', ...intentTypes.filter(t => t !== '__persona__')].map(intent => (
              <button
                key={intent}
                onClick={() => setSelectedIntent(intent)}
                className={[
                  'w-full text-left rounded px-3 py-2 text-sm transition-colors',
                  selectedIntent === intent
                    ? 'bg-accent/10 text-accent'
                    : 'text-muted hover:bg-surface-800 hover:text-fg',
                ].join(' ')}
              >
                {intentLabel(intent)}
              </button>
            ))}
          </nav>
        )}
      </aside>

      {/* ── Main content ── */}
      <div className="flex-1 overflow-y-auto p-6">
        {!selectedIntent ? (
          <div className="flex h-full items-center justify-center">
            <p className="text-sm text-dim">Select an intent type to manage its prompts.</p>
          </div>
        ) : (
          <>
            {/* Page header */}
            <div className="mb-6 flex items-center justify-between">
              <div>
                <h2 className="text-lg font-semibold text-fg">
                  {intentLabel(selectedIntent)}
                </h2>
                <p className="mt-0.5 text-xs text-dim">
                  Manage system prompt overrides for this intent.
                </p>
              </div>
              <div className="flex gap-2">
                <button
                  onClick={() => setShowResolutionModal(true)}
                  className="rounded-sm border border-rule px-3 py-1.5 text-sm text-muted hover:bg-surface-800"
                >
                  Preview Resolution
                </button>
                <button
                  onClick={() => setShowAddModal(true)}
                  className="rounded bg-accent px-3 py-1.5 text-sm font-medium text-surface-900 hover:bg-accent-hover"
                >
                  Add Override
                </button>
              </div>
            </div>

            {error && (
              <div className="mb-4 rounded-sm border border-red-700 bg-red-900/20 px-4 py-2 text-sm text-red-300">
                {error}
              </div>
            )}

            {loadingTemplates ? (
              <p className="text-sm text-dim">Loading templates...</p>
            ) : (
              <div className="space-y-6">
                {/* Default / hardcoded fallback */}
                {defaultTemplates.length > 0 && (
                  <section>
                    <p className="mb-3 text-xs font-semibold uppercase tracking-widest text-dim">
                      Default — hardcoded fallback
                    </p>
                    <div className="space-y-3">
                      {defaultTemplates.map(t => (
                        <TemplateCard
                          key={t.id}
                          template={t}
                          onEdit={setEditTarget}
                          onDelete={handleDelete}
                        />
                      ))}
                    </div>
                  </section>
                )}

                {/* Overrides */}
                <section>
                  <p className="mb-3 text-xs font-semibold uppercase tracking-widest text-dim">
                    Overrides
                    {overrideTemplates.length > 0 && (
                      <span className="ml-2 rounded bg-indigo-800/40 px-1.5 py-0.5 font-normal text-accent">
                        {overrideTemplates.length}
                      </span>
                    )}
                  </p>
                  {overrideTemplates.length === 0 ? (
                    <div className="rounded-sm border border-dashed border-rule py-8 text-center">
                      <p className="text-sm text-dim">No overrides yet.</p>
                      <button
                        onClick={() => setShowAddModal(true)}
                        className="mt-2 text-sm text-accent-dim hover:text-accent"
                      >
                        Add the first override
                      </button>
                    </div>
                  ) : (
                    <div className="space-y-3">
                      {overrideTemplates.map(t => (
                        <TemplateCard
                          key={t.id}
                          template={t}
                          onEdit={setEditTarget}
                          onDelete={handleDelete}
                        />
                      ))}
                    </div>
                  )}
                </section>
              </div>
            )}
          </>
        )}
      </div>

      {/* ── Modals ── */}
      {showAddModal && selectedIntent && (
        <AddOverrideModal
          intentType={selectedIntent}
          onClose={() => setShowAddModal(false)}
          onSaved={handleSaved}
        />
      )}

      {editTarget && (
        <EditPromptModal
          template={editTarget}
          onClose={() => setEditTarget(null)}
          onSaved={handleSaved}
        />
      )}

      {showResolutionModal && selectedIntent && (
        <ResolutionPreviewModal
          intentType={selectedIntent}
          onClose={() => setShowResolutionModal(false)}
        />
      )}
    </div>
  )
}
