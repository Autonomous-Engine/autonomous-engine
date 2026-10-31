import { useState, useEffect, useCallback } from 'react'
import { useApi } from '@/hooks/useApi'
import { guarded } from '@/hooks/rolloutGuard'
import type { AgentDefinition, AgentDefinitionPrompt, LLMProvider } from '@/types'

// ─── Constants ────────────────────────────────────────────────────────────────

const ANTHROPIC_MODELS = [
  'claude-opus-4-0-20250115',
  'claude-sonnet-4-20250514',
  'claude-haiku-4-5-20251001',
  'claude-3-5-sonnet-20241022',
  'claude-3-5-haiku-20241022',
  'claude-3-opus-20240229',
]

const COMPLEXITY_OPTIONS = ['trivial', 'simple', 'moderate', 'complex']

const PIPELINE_STEPS: { key: keyof AgentDefinition; label: string; alwaysOn?: boolean }[] = [
  { key: 'step_product_analysis', label: 'Product Analysis', alwaysOn: true },
  { key: 'step_research', label: 'Research' },
  { key: 'step_planning', label: 'Planning' },
  { key: 'step_implementation', label: 'Implementation', alwaysOn: true },
  { key: 'step_review', label: 'Code Review' },
  { key: 'step_human_review', label: 'Human Review' },
  { key: 'step_qa', label: 'QA Validation' },
]

function emptyDef(): Partial<AgentDefinition> {
  return {
    name: '',
    display_name: '',
    description: '',
    provider: '',
    model_id: '',
    model_endpoint: '',
    step_product_analysis: true,
    step_research: true,
    step_planning: true,
    step_implementation: true,
    step_review: true,
    step_human_review: true,
    step_qa: true,
    routing_tags: [],
    routing_complexity: [],
    routing_priority: false,
    max_retries: 3,
    max_files_per_task: 10,
    timeout_minutes: 120,
    enabled: true,
    is_default: false,
  }
}

function slugify(s: string) {
  return s
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-+|-+$/g, '')
}

// ─── Pipeline Preview ──────────────────────────────────────────────────────────

function PipelinePreview({ def }: { def: Partial<AgentDefinition> }) {
  return (
    <div className="flex flex-wrap items-center gap-1.5 py-1">
      {PIPELINE_STEPS.map((step, i) => {
        const active = def[step.key] as boolean
        return (
          <div key={step.key} className="flex items-center gap-1.5">
            {i > 0 && (
              <div
                className={`h-px w-5 ${active ? 'bg-indigo-500' : 'border-t border-dashed border-rule'}`}
              />
            )}
            <div
              title={step.label}
              className={[
                'flex h-6 w-6 items-center justify-center rounded-full border text-[9px] font-bold',
                active
                  ? 'border-accent bg-accent/15 text-accent'
                  : 'border-dashed border-rule bg-transparent text-dim',
              ].join(' ')}
            >
              {i + 1}
            </div>
          </div>
        )
      })}
    </div>
  )
}

// ─── Tag input ────────────────────────────────────────────────────────────────

function TagInput({
  tags,
  onChange,
  placeholder,
}: {
  tags: string[]
  onChange: (tags: string[]) => void
  placeholder?: string
}) {
  const [input, setInput] = useState('')

  function addTag() {
    const t = input.trim()
    if (t && !tags.includes(t)) {
      onChange([...tags, t])
    }
    setInput('')
  }

  return (
    <div className="flex flex-wrap gap-1.5 rounded-sm border border-rule bg-surface-950 px-2 py-1.5">
      {tags.map(t => (
        <span
          key={t}
          className="inline-flex items-center gap-1 rounded bg-indigo-800/40 px-2 py-0.5 text-xs text-accent"
        >
          {t}
          <button
            type="button"
            onClick={() => onChange(tags.filter(x => x !== t))}
            className="text-muted hover:text-fg"
          >
            ×
          </button>
        </span>
      ))}
      <input
        className="min-w-[120px] flex-1 bg-transparent text-sm text-fg outline-none placeholder:text-dim"
        value={input}
        onChange={e => setInput(e.target.value)}
        onKeyDown={e => {
          if (e.key === 'Enter' || e.key === ',') {
            e.preventDefault()
            addTag()
          }
        }}
        onBlur={addTag}
        placeholder={placeholder ?? 'Type and press Enter'}
      />
    </div>
  )
}

// ─── Prompt editor ────────────────────────────────────────────────────────────

interface PromptEditorProps {
  agentId: string
  onClose: () => void
}

function PromptEditor({ agentId, onClose }: PromptEditorProps) {
  const { get, post, del } = useApi()
  const [prompts, setPrompts] = useState<AgentDefinitionPrompt[]>([])
  const [loading, setLoading] = useState(true)
  const [addMode, setAddMode] = useState(false)
  const [newIntent, setNewIntent] = useState('')
  const [newType, setNewType] = useState('system')
  const [newContent, setNewContent] = useState('')
  const [error, setError] = useState('')

  const load = useCallback(async () => {
    setLoading(true)
    try {
      const data = await get<AgentDefinitionPrompt[]>(
        `/admin/agents/prompts?agent_id=${agentId}`,
      )
      setPrompts(data ?? [])
    } catch (err) {
      console.error('Failed to fetch:', err)
      setError(err instanceof Error ? err.message : 'Failed to load prompts')
    } finally {
      setLoading(false)
    }
  }, [agentId, get])

  useEffect(() => {
    load()
  }, [load])

  async function handleAdd() {
    setError('')
    if (!newIntent.trim()) {
      setError('Intent type is required')
      return
    }
    try {
      await guarded('/admin/agents/prompts/set', p => post(p, {
        agent_definition_id: agentId,
        intent_type: newIntent.trim(),
        prompt_type: newType,
        content: newContent,
      }))
      setAddMode(false)
      setNewIntent('')
      setNewContent('')
      load()
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Failed to save')
    }
  }

  async function handleDelete(p: AgentDefinitionPrompt) {
    try {
      await guarded(
        `/admin/agents/prompts/delete?agent_id=${agentId}&intent_type=${encodeURIComponent(p.intent_type)}`,
        path => del(path),
      )
      load()
    } catch (err) {
      console.error('Failed to fetch:', err)
      setError(err instanceof Error ? err.message : 'Failed to delete prompt')
    }
  }

  async function handleSave(p: AgentDefinitionPrompt, content: string) {
    try {
      await guarded('/admin/agents/prompts/set', path => post(path, {
        agent_definition_id: agentId,
        intent_type: p.intent_type,
        prompt_type: p.prompt_type,
        content,
      }))
      load()
    } catch (err) {
      console.error('Failed to fetch:', err)
      setError(err instanceof Error ? err.message : 'Failed to save prompt')
    }
  }

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/60">
      <div className="flex w-[700px] max-h-[90vh] flex-col rounded-sm border border-rule bg-surface-900 shadow-xl">
        <div className="flex items-center justify-between border-b border-rule px-6 py-4">
          <h2 className="text-base font-semibold text-fg">Agent Prompts</h2>
          <button
            onClick={onClose}
            className="text-muted hover:text-fg"
          >
            ✕
          </button>
        </div>

        <div className="flex-1 overflow-y-auto px-6 py-4 space-y-4">
          <p className="text-xs text-dim">
            These prompts override the global defaults for this agent only.
          </p>

          {loading && (
            <p className="text-sm text-muted">Loading…</p>
          )}

          {!loading && prompts.map(p => (
            <PromptRow key={`${p.intent_type}-${p.prompt_type}`} prompt={p} onSave={handleSave} onDelete={handleDelete} />
          ))}

          {!loading && prompts.length === 0 && !addMode && (
            <p className="text-sm text-dim">No prompts configured for this agent.</p>
          )}

          {addMode && (
            <div className="rounded-sm border border-rule bg-surface-950 p-4 space-y-3">
              <div className="grid grid-cols-2 gap-3">
                <div>
                  <label className="mb-1 block text-xs font-medium text-muted">Intent Type</label>
                  <input
                    value={newIntent}
                    onChange={e => setNewIntent(e.target.value)}
                    placeholder="e.g. feature_development"
                    className="w-full rounded-sm border border-rule bg-surface-900 px-3 py-2 text-sm text-fg focus:outline-none focus:ring-1 focus:ring-accent"
                  />
                </div>
                <div>
                  <label className="mb-1 block text-xs font-medium text-muted">Prompt Type</label>
                  <select
                    value={newType}
                    onChange={e => setNewType(e.target.value)}
                    className="w-full rounded-sm border border-rule bg-surface-900 px-3 py-2 text-sm text-fg focus:outline-none focus:ring-1 focus:ring-accent"
                  >
                    <option value="system">system</option>
                    <option value="instruction">instruction</option>
                  </select>
                </div>
              </div>
              <div>
                <label className="mb-1 block text-xs font-medium text-muted">Content</label>
                <textarea
                  value={newContent}
                  onChange={e => setNewContent(e.target.value)}
                  rows={4}
                  className="w-full rounded-sm border border-rule bg-surface-900 px-3 py-2 text-sm text-fg focus:outline-none focus:ring-1 focus:ring-accent font-mono"
                />
              </div>
              {error && <p className="text-xs text-red-400">{error}</p>}
              <div className="flex gap-2">
                <button
                  onClick={handleAdd}
                  className="rounded bg-accent px-3 py-1.5 text-xs font-medium text-surface-900 hover:bg-accent-hover"
                >
                  Save Prompt
                </button>
                <button
                  onClick={() => { setAddMode(false); setError('') }}
                  className="rounded-sm border border-rule px-3 py-1.5 text-xs font-medium text-muted hover:bg-surface-800"
                >
                  Cancel
                </button>
              </div>
            </div>
          )}
        </div>

        <div className="border-t border-rule px-6 py-3 flex justify-between">
          <button
            onClick={() => setAddMode(true)}
            className="rounded-sm border border-indigo-600 px-3 py-1.5 text-xs font-medium text-accent hover:bg-accent/10"
          >
            + Add Prompt
          </button>
          <button
            onClick={onClose}
            className="rounded-sm border border-rule px-3 py-1.5 text-xs font-medium text-muted hover:bg-surface-800"
          >
            Close
          </button>
        </div>
      </div>
    </div>
  )
}

function PromptRow({
  prompt,
  onSave,
  onDelete,
}: {
  prompt: AgentDefinitionPrompt
  onSave: (p: AgentDefinitionPrompt, content: string) => void
  onDelete: (p: AgentDefinitionPrompt) => void
}) {
  const [content, setContent] = useState(prompt.content)
  const [dirty, setDirty] = useState(false)

  return (
    <div className="rounded-sm border border-rule bg-surface-950 p-3 space-y-2">
      <div className="flex items-center gap-2">
        <span className="rounded bg-indigo-800/40 px-2 py-0.5 text-xs font-medium text-accent">
          {prompt.intent_type}
        </span>
        <span className="rounded bg-surface-700 px-2 py-0.5 text-xs text-muted">
          {prompt.prompt_type}
        </span>
      </div>
      <textarea
        value={content}
        onChange={e => { setContent(e.target.value); setDirty(true) }}
        rows={3}
        className="w-full rounded-sm border border-rule bg-surface-900 px-3 py-2 text-sm text-fg focus:outline-none focus:ring-1 focus:ring-accent font-mono"
      />
      <div className="flex gap-2">
        {dirty && (
          <button
            onClick={() => { onSave(prompt, content); setDirty(false) }}
            className="rounded bg-accent px-2.5 py-1 text-xs font-medium text-surface-900 hover:bg-accent-hover"
          >
            Save
          </button>
        )}
        <button
          onClick={() => onDelete(prompt)}
          className="rounded-sm border border-red-700/50 px-2.5 py-1 text-xs font-medium text-red-400 hover:bg-red-900/30"
        >
          Delete
        </button>
      </div>
    </div>
  )
}

// ─── Agent Form (Create / Edit) ───────────────────────────────────────────────

interface AgentFormProps {
  initial?: AgentDefinition
  onClose: () => void
  onSaved: () => void
}

function AgentForm({ initial, onClose, onSaved }: AgentFormProps) {
  const { get, post, put } = useApi()
  const [form, setForm] = useState<Partial<AgentDefinition>>(
    initial ? { ...initial } : emptyDef(),
  )
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)

  // ── Provider + model state ───────────────────────────────���─────────────────

  const [connectedProviders, setConnectedProviders] = useState<LLMProvider[]>([])
  // The llm_providers.id of the currently selected provider (UI state only)
  const [selectedProviderId, setSelectedProviderId] = useState<string>('')
  // Available models for the chosen provider
  const [availableModels, setAvailableModels] = useState<string[]>([])
  const [loadingModels, setLoadingModels] = useState(false)

  // Load connected providers once on mount
  useEffect(() => {
    get<LLMProvider[]>('/admin/providers')
      .then(data => {
        const list = (data ?? []).filter(p => p.enabled)
        setConnectedProviders(list)

        // If editing, try to match the saved provider type + endpoint to a
        // known provider record so we can pre-select the dropdown.
        if (initial?.provider && initial?.model_endpoint) {
          const match = list.find(
            p => p.provider_type === initial.provider && p.endpoint === initial.model_endpoint,
          )
          if (match) setSelectedProviderId(match.id)
        }
      })
      .catch(() => {})
  }, [get, initial])

  // When the selected provider changes, fetch its model list
  useEffect(() => {
    if (!selectedProviderId) {
      setAvailableModels([])
      return
    }
    const prov = connectedProviders.find(p => p.id === selectedProviderId)
    if (!prov) return

    if (prov.provider_type === 'anthropic') {
      setAvailableModels(ANTHROPIC_MODELS)
      return
    }
    if (prov.provider_type === 'vllm' || prov.provider_type === 'custom') {
      setAvailableModels([])
      return
    }

    // ollama or openai — fetch from API
    setLoadingModels(true)
    get<{ models: Array<string | { id: string; name?: string }> }>(`/admin/providers/models?id=${prov.id}`)
      .then(data => {
        const raw = data?.models ?? []
        const names: string[] = raw.map(m =>
          typeof m === 'string' ? m : (m.id ?? ''),
        ).filter(Boolean)
        setAvailableModels(names)
      })
      .catch(() => setAvailableModels([]))
      .finally(() => setLoadingModels(false))
  }, [selectedProviderId, connectedProviders, get])

  // When user picks a provider from the dropdown, auto-fill form fields
  function handleProviderSelect(providerId: string) {
    setSelectedProviderId(providerId)
    const prov = connectedProviders.find(p => p.id === providerId)
    if (prov) {
      setForm(f => ({
        ...f,
        provider: prov.provider_type,
        model_endpoint: prov.endpoint,
        // Reset model_id so user picks from the new list
        model_id: '',
      }))
    } else {
      // "Use system default" selected
      setForm(f => ({ ...f, provider: '', model_endpoint: '', model_id: '' }))
    }
  }

  function set<K extends keyof AgentDefinition>(key: K, value: AgentDefinition[K]) {
    setForm(f => {
      const next = { ...f, [key]: value }
      // Auto-generate slug from display_name when the name field is still
      // empty or was previously auto-generated from the display name.
      if (key === 'display_name' && (!f.name || f.name === slugify(f.display_name ?? ''))) {
        next.name = slugify(value as string)
      }
      return next
    })
  }

  function toggleStep(key: keyof AgentDefinition) {
    const step = PIPELINE_STEPS.find(s => s.key === key)
    if (step?.alwaysOn) return
    setForm(f => ({ ...f, [key]: !f[key] }))
  }

  function toggleComplexity(c: string) {
    const current = form.routing_complexity ?? []
    if (current.includes(c)) {
      setForm(f => ({ ...f, routing_complexity: current.filter(x => x !== c) }))
    } else {
      setForm(f => ({ ...f, routing_complexity: [...current, c] }))
    }
  }

  async function handleSave() {
    setError('')
    if (!form.name?.trim()) {
      setError('Name (slug) is required')
      return
    }
    setLoading(true)
    try {
      // Ensure array fields are never null/undefined before sending
      const payload = {
        ...form,
        routing_tags: form.routing_tags ?? [],
        routing_complexity: form.routing_complexity ?? [],
      }
      if (initial?.id) {
        await guarded(`/admin/agents/update?id=${initial.id}`, p => put(p, payload))
      } else {
        await guarded('/admin/agents/create', p => post(p, payload))
      }
      onSaved()
      onClose()
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Failed to save')
    } finally {
      setLoading(false)
    }
  }

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/60">
      <div className="flex w-[680px] max-h-[92vh] flex-col rounded-sm border border-rule bg-surface-900 shadow-xl">
        {/* Header */}
        <div className="flex items-center justify-between border-b border-rule px-6 py-4">
          <h2 className="text-base font-semibold text-fg">
            {initial ? 'Edit Agent' : 'Create Agent'}
          </h2>
          <button onClick={onClose} className="text-muted hover:text-fg">✕</button>
        </div>

        {/* Body */}
        <div className="flex-1 overflow-y-auto px-6 py-5 space-y-6">

          {/* Basics */}
          <section>
            <h3 className="mb-3 text-xs font-semibold uppercase tracking-widest text-dim">Basics</h3>
            <div className="space-y-3">
              <div>
                <label className="mb-1 block text-xs font-medium text-muted">Display Name</label>
                <input
                  value={form.display_name ?? ''}
                  onChange={e => set('display_name', e.target.value)}
                  className="w-full rounded-sm border border-rule bg-surface-950 px-3 py-2 text-sm text-fg focus:outline-none focus:ring-1 focus:ring-accent"
                />
              </div>
              <div>
                <label className="mb-1 block text-xs font-medium text-muted">
                  Name <span className="text-dim">(slug, used internally)</span>
                </label>
                <input
                  value={form.name ?? ''}
                  onChange={e => set('name', e.target.value)}
                  className="w-full rounded-sm border border-rule bg-surface-950 px-3 py-2 text-sm text-fg font-mono focus:outline-none focus:ring-1 focus:ring-accent"
                />
              </div>
              <div>
                <label className="mb-1 block text-xs font-medium text-muted">Description</label>
                <textarea
                  value={form.description ?? ''}
                  onChange={e => set('description', e.target.value)}
                  rows={2}
                  className="w-full rounded-sm border border-rule bg-surface-950 px-3 py-2 text-sm text-fg focus:outline-none focus:ring-1 focus:ring-accent"
                />
              </div>
            </div>
          </section>

          {/* Model */}
          <section>
            <h3 className="mb-3 text-xs font-semibold uppercase tracking-widest text-dim">Model</h3>
            <div className="space-y-3">
              {/* Provider — dropdown of connected providers */}
              <div>
                <label className="mb-1 block text-xs font-medium text-muted">Provider</label>
                {connectedProviders.length > 0 ? (
                  <select
                    value={selectedProviderId}
                    onChange={e => handleProviderSelect(e.target.value)}
                    className="w-full rounded-sm border border-rule bg-surface-950 px-3 py-2 text-sm text-fg focus:outline-none focus:ring-1 focus:ring-accent"
                  >
                    <option value="">— Use system default —</option>
                    {connectedProviders.map(p => (
                      <option key={p.id} value={p.id}>
                        {p.name}
                        {p.is_default ? ' (default)' : ''}
                      </option>
                    ))}
                  </select>
                ) : (
                  <p className="rounded-sm border border-amber-700/40 bg-amber-900/20 px-3 py-2 text-xs text-amber-300">
                    No providers configured.{' '}
                    <a href="/admin/providers" className="underline hover:text-amber-100">
                      Add one in Admin → Providers
                    </a>
                    {' '}first.
                  </p>
                )}
                {/* Read-only resolved type + endpoint */}
                {selectedProviderId && form.provider && (
                  <p className="mt-1 text-xs text-dim">
                    Type: <span className="text-dim">{form.provider}</span>
                    {form.model_endpoint && (
                      <> &middot; Endpoint: <span className="font-mono text-dim">{form.model_endpoint}</span></>
                    )}
                  </p>
                )}
              </div>

              {/* Model ID — dropdown or free text depending on provider type */}
              <div>
                <label className="mb-1 block text-xs font-medium text-muted">Model</label>
                {selectedProviderId && availableModels.length > 0 ? (
                  <select
                    value={form.model_id ?? ''}
                    onChange={e => set('model_id', e.target.value)}
                    className="w-full rounded-sm border border-rule bg-surface-950 px-3 py-2 text-sm text-fg focus:outline-none focus:ring-1 focus:ring-accent"
                  >
                    <option value="">— select model —</option>
                    {availableModels.map(m => (
                      <option key={m} value={m}>{m}</option>
                    ))}
                  </select>
                ) : (
                  <input
                    value={form.model_id ?? ''}
                    onChange={e => set('model_id', e.target.value)}
                    placeholder={
                      loadingModels
                        ? 'Loading models...'
                        : selectedProviderId
                        ? 'Enter model name (e.g. my-custom-model)'
                        : 'e.g. gpt-4o'
                    }
                    disabled={loadingModels}
                    className="w-full rounded-sm border border-rule bg-surface-950 px-3 py-2 text-sm text-fg focus:outline-none focus:ring-1 focus:ring-accent disabled:opacity-60"
                  />
                )}
                {selectedProviderId && availableModels.length > 0 && (
                  <p className="mt-0.5 text-xs text-dim">
                    {availableModels.length} model(s) available from this provider.
                  </p>
                )}
              </div>

              {/* Endpoint — shown read-only when a provider is selected, editable otherwise */}
              <div>
                <label className="mb-1 block text-xs font-medium text-muted">
                  Endpoint{' '}
                  <span className="text-dim">
                    {selectedProviderId ? '(set by provider — edit in Admin → Providers)' : '(optional — leave empty for system default)'}
                  </span>
                </label>
                <input
                  value={form.model_endpoint ?? ''}
                  onChange={e => set('model_endpoint', e.target.value)}
                  readOnly={!!selectedProviderId}
                  placeholder="https://api.openai.com/v1"
                  className={`w-full rounded-sm border border-rule bg-surface-950 px-3 py-2 text-sm text-fg focus:outline-none focus:ring-1 focus:ring-accent ${selectedProviderId ? 'cursor-not-allowed opacity-60' : ''}`}
                />
              </div>
            </div>
          </section>

          {/* Pipeline Steps */}
          <section>
            <h3 className="mb-3 text-xs font-semibold uppercase tracking-widest text-dim">Pipeline Steps</h3>
            <div className="mb-3">
              <PipelinePreview def={form} />
            </div>
            <div className="space-y-2">
              {PIPELINE_STEPS.map(step => {
                const active = form[step.key] as boolean
                return (
                  <div key={step.key} className="flex items-center justify-between rounded-sm border border-rule px-3 py-2">
                    <span className="text-sm text-fg">{step.label}</span>
                    <div className="flex items-center gap-2">
                      {step.alwaysOn && (
                        <span className="text-xs text-dim">always on</span>
                      )}
                      <button
                        type="button"
                        onClick={() => toggleStep(step.key)}
                        disabled={step.alwaysOn}
                        className={[
                          'relative inline-flex h-5 w-9 cursor-pointer rounded-full transition-colors focus:outline-none',
                          active ? 'bg-accent' : 'bg-surface-700',
                          step.alwaysOn ? 'opacity-50 cursor-not-allowed' : '',
                        ].join(' ')}
                      >
                        <span
                          className={[
                            'inline-block h-4 w-4 transform rounded-full bg-white shadow transition-transform mt-0.5',
                            active ? 'translate-x-4' : 'translate-x-0.5',
                          ].join(' ')}
                        />
                      </button>
                    </div>
                  </div>
                )
              })}
            </div>
          </section>

          {/* Routing */}
          <section>
            <h3 className="mb-3 text-xs font-semibold uppercase tracking-widest text-dim">Routing</h3>
            <div className="space-y-3">
              <div>
                <label className="mb-1 block text-xs font-medium text-muted">
                  Tags <span className="text-dim">(work items with these tags route here)</span>
                </label>
                <TagInput
                  tags={form.routing_tags ?? []}
                  onChange={tags => set('routing_tags', tags)}
                  placeholder='e.g. text-change, trivial'
                />
              </div>
              <div>
                <label className="mb-2 block text-xs font-medium text-muted">Complexity</label>
                <div className="flex flex-wrap gap-2">
                  {COMPLEXITY_OPTIONS.map(c => (
                    <label key={c} className="flex items-center gap-1.5 cursor-pointer">
                      <input
                        type="checkbox"
                        checked={(form.routing_complexity ?? []).includes(c)}
                        onChange={() => toggleComplexity(c)}
                        className="rounded-sm border-rule bg-surface-950 text-indigo-600"
                      />
                      <span className="text-sm text-muted">{c}</span>
                    </label>
                  ))}
                </div>
              </div>
              <label className="flex items-center gap-2 cursor-pointer">
                <input
                  type="checkbox"
                  checked={form.routing_priority ?? false}
                  onChange={e => set('routing_priority', e.target.checked)}
                  className="rounded-sm border-rule bg-surface-950 text-indigo-600"
                />
                <span className="text-sm text-muted">
                  Priority routing — this agent takes priority when multiple agents match
                </span>
              </label>
            </div>
          </section>

          {/* Behavior */}
          <section>
            <h3 className="mb-3 text-xs font-semibold uppercase tracking-widest text-dim">Behavior</h3>
            <div className="grid grid-cols-3 gap-3">
              <div>
                <label className="mb-1 block text-xs font-medium text-muted">Max Retries</label>
                <input
                  type="number"
                  min={0}
                  value={form.max_retries ?? 3}
                  onChange={e => set('max_retries', Number(e.target.value))}
                  className="w-full rounded-sm border border-rule bg-surface-950 px-3 py-2 text-sm text-fg focus:outline-none focus:ring-1 focus:ring-accent"
                />
              </div>
              <div>
                <label className="mb-1 block text-xs font-medium text-muted">Max Files / Task</label>
                <input
                  type="number"
                  min={1}
                  value={form.max_files_per_task ?? 10}
                  onChange={e => set('max_files_per_task', Number(e.target.value))}
                  className="w-full rounded-sm border border-rule bg-surface-950 px-3 py-2 text-sm text-fg focus:outline-none focus:ring-1 focus:ring-accent"
                />
              </div>
              <div>
                <label className="mb-1 block text-xs font-medium text-muted">Timeout (minutes)</label>
                <input
                  type="number"
                  min={1}
                  value={form.timeout_minutes ?? 120}
                  onChange={e => set('timeout_minutes', Number(e.target.value))}
                  className="w-full rounded-sm border border-rule bg-surface-950 px-3 py-2 text-sm text-fg focus:outline-none focus:ring-1 focus:ring-accent"
                />
              </div>
            </div>
          </section>

          {/* Status */}
          <section>
            <h3 className="mb-3 text-xs font-semibold uppercase tracking-widest text-dim">Status</h3>
            <label className="flex items-center gap-2 cursor-pointer">
              <input
                type="checkbox"
                checked={form.enabled ?? true}
                onChange={e => set('enabled', e.target.checked)}
                className="rounded-sm border-rule bg-surface-950 text-indigo-600"
              />
              <span className="text-sm text-muted">Enabled</span>
            </label>
          </section>
        </div>

        {/* Footer */}
        <div className="border-t border-rule px-6 py-4 flex items-center justify-between">
          {error && <p className="text-xs text-red-400">{error}</p>}
          {!error && <span />}
          <div className="flex gap-2">
            <button
              onClick={onClose}
              className="rounded-sm border border-rule px-4 py-2 text-sm font-medium text-muted hover:bg-surface-800"
            >
              Cancel
            </button>
            <button
              onClick={handleSave}
              disabled={loading}
              className="rounded bg-accent px-4 py-2 text-sm font-medium text-surface-900 hover:bg-accent-hover disabled:opacity-50"
            >
              {loading ? 'Saving…' : 'Save'}
            </button>
          </div>
        </div>
      </div>
    </div>
  )
}

// ─── Agent Card ───────────────────────────────────────────────────────────────

interface AgentCardProps {
  def: AgentDefinition
  onEdit: (def: AgentDefinition) => void
  onDelete: (id: string) => void
  onSetDefault: (id: string) => void
  onPrompts: (id: string) => void
  onToggleEnabled: (def: AgentDefinition) => void
}

function AgentCard({
  def,
  onEdit,
  onDelete,
  onSetDefault,
  onPrompts,
  onToggleEnabled,
}: AgentCardProps) {
  return (
    <div className="rounded-sm border border-rule bg-surface-950 p-5 space-y-3">
      {/* Header row */}
      <div className="flex items-start justify-between gap-3">
        <div className="flex-1 min-w-0">
          <div className="flex items-center gap-2 flex-wrap">
            <h3 className="text-sm font-semibold text-fg truncate">
              {def.display_name || def.name}
            </h3>
            {def.is_default && (
              <span className="inline-flex items-center rounded bg-emerald-800/40 px-2 py-0.5 text-xs font-medium text-emerald-300">
                Default
              </span>
            )}
            {!def.enabled && (
              <span className="inline-flex items-center rounded bg-surface-700 px-2 py-0.5 text-xs font-medium text-muted">
                Disabled
              </span>
            )}
          </div>
          {def.description && (
            <p className="mt-0.5 text-xs text-dim line-clamp-2">{def.description}</p>
          )}
        </div>
        {/* Enable toggle */}
        <button
          type="button"
          onClick={() => onToggleEnabled(def)}
          className={[
            'relative inline-flex h-5 w-9 flex-shrink-0 cursor-pointer rounded-full transition-colors focus:outline-none',
            def.enabled ? 'bg-accent' : 'bg-surface-700',
          ].join(' ')}
          title={def.enabled ? 'Disable' : 'Enable'}
        >
          <span
            className={[
              'inline-block h-4 w-4 transform rounded-full bg-white shadow transition-transform mt-0.5',
              def.enabled ? 'translate-x-4' : 'translate-x-0.5',
            ].join(' ')}
          />
        </button>
      </div>

      {/* Model badges */}
      {(def.provider || def.model_id) && (
        <div className="flex gap-1.5 flex-wrap">
          {def.provider && (
            <span className="rounded bg-indigo-900/40 px-2 py-0.5 text-xs text-accent">
              {def.provider}
            </span>
          )}
          {def.model_id && (
            <span className="rounded bg-surface-700 px-2 py-0.5 text-xs text-muted font-mono">
              {def.model_id}
            </span>
          )}
        </div>
      )}

      {/* Pipeline preview */}
      <PipelinePreview def={def} />

      {/* Routing tags */}
      {def.routing_tags.length > 0 && (
        <div className="flex flex-wrap gap-1">
          {def.routing_tags.map(t => (
            <span key={t} className="rounded bg-surface-800 px-2 py-0.5 text-xs text-muted">
              {t}
            </span>
          ))}
        </div>
      )}

      {/* Action buttons */}
      <div className="flex flex-wrap gap-2 pt-1">
        <button
          onClick={() => onEdit(def)}
          className="rounded-sm border border-rule px-3 py-1.5 text-xs font-medium text-muted hover:bg-surface-800"
        >
          Edit
        </button>
        <button
          onClick={() => onPrompts(def.id)}
          className="rounded-sm border border-rule px-3 py-1.5 text-xs font-medium text-muted hover:bg-surface-800"
        >
          Prompts
        </button>
        {!def.is_default && (
          <button
            onClick={() => onSetDefault(def.id)}
            className="rounded-sm border border-emerald-700/50 px-3 py-1.5 text-xs font-medium text-emerald-400 hover:bg-emerald-900/30"
          >
            Set Default
          </button>
        )}
        <button
          onClick={() => onDelete(def.id)}
          className="rounded-sm border border-red-700/50 px-3 py-1.5 text-xs font-medium text-red-400 hover:bg-red-900/30 ml-auto"
        >
          Delete
        </button>
      </div>
    </div>
  )
}

// ─── Main Page ────────────────────────────────────────────────────────────────

export default function AgentDefinitions() {
  const { get, put, del, post } = useApi()
  const [defs, setDefs] = useState<AgentDefinition[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [showCreate, setShowCreate] = useState(false)
  const [editTarget, setEditTarget] = useState<AgentDefinition | null>(null)
  const [promptsTarget, setPromptsTarget] = useState<string | null>(null)

  const load = useCallback(async () => {
    setLoading(true)
    setError('')
    try {
      const data = await get<AgentDefinition[]>('/admin/agents')
      setDefs(data ?? [])
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Failed to load agent definitions')
    } finally {
      setLoading(false)
    }
  }, [get])

  useEffect(() => {
    load()
  }, [load])

  async function handleDelete(id: string) {
    if (!confirm('Delete this agent definition? This cannot be undone.')) return
    try {
      await guarded(`/admin/agents/delete?id=${id}`, p => del(p))
      load()
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Failed to delete')
    }
  }

  async function handleSetDefault(id: string) {
    try {
      await guarded(`/admin/agents/set-default?id=${id}`, p => post(p))
      load()
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Failed to set default')
    }
  }

  async function handleToggleEnabled(def: AgentDefinition) {
    try {
      await guarded(`/admin/agents/update?id=${def.id}`, p => put(p, { ...def, enabled: !def.enabled }))
      load()
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Failed to update')
    }
  }

  return (
    <div className="space-y-6">
      {/* Page header */}
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-lg font-semibold text-fg">Agent Definitions</h1>
          <p className="mt-0.5 text-sm text-dim">
            Configure custom agents with their own models, pipelines, and routing rules.
          </p>
        </div>
        <button
          onClick={() => setShowCreate(true)}
          className="rounded bg-accent px-4 py-2 text-sm font-medium text-surface-900 hover:bg-accent-hover"
        >
          + Create Agent
        </button>
      </div>

      {/* Model Routing Guide */}
      <div className="rounded-sm border border-indigo-700/40 bg-indigo-900/10 px-5 py-4 space-y-3">
        <h2 className="text-xs font-semibold uppercase tracking-widest text-accent-dim">
          Model Routing Guide
        </h2>
        <ul className="space-y-1.5 text-xs text-muted">
          <li>
            <span className="font-medium text-fg">Research agents</span>
            {' — '}Use a capable model (Claude Opus, GPT-4o) — research requires deep analysis.
          </li>
          <li>
            <span className="font-medium text-fg">Implementation agents</span>
            {' — '}Use a strong code model (Claude Sonnet, GPT-4o, Qwen 2.5 Coder 32B).
          </li>
          <li>
            <span className="font-medium text-fg">Review agents</span>
            {' — '}Use any capable model — reviewing is easier than writing.
          </li>
          <li>
            <span className="font-medium text-fg">Quick text changes</span>
            {' — '}Use a small fast model (Llama 3.1 8B, Qwen 2.5 7B) to save cost.
          </li>
          <li>
            <span className="font-medium text-fg">Each agent definition</span>
            {' — '}can use a different model from a different provider.
          </li>
        </ul>
      </div>

      {error && (
        <div className="rounded-sm border border-red-700/50 bg-red-900/20 px-4 py-3 text-sm text-red-400">
          {error}
        </div>
      )}

      {loading && (
        <p className="text-sm text-muted">Loading…</p>
      )}

      {!loading && defs.length === 0 && (
        <div className="rounded-sm border border-dashed border-rule bg-surface-950 p-10 text-center">
          <p className="text-sm text-dim">No agent definitions yet. Create one to get started.</p>
        </div>
      )}

      {/* Agent grid */}
      <div className="grid gap-4 md:grid-cols-2 xl:grid-cols-3">
        {defs.map(def => (
          <AgentCard
            key={def.id}
            def={def}
            onEdit={d => setEditTarget(d)}
            onDelete={handleDelete}
            onSetDefault={handleSetDefault}
            onPrompts={id => setPromptsTarget(id)}
            onToggleEnabled={handleToggleEnabled}
          />
        ))}
      </div>

      {/* Create modal */}
      {showCreate && (
        <AgentForm
          onClose={() => setShowCreate(false)}
          onSaved={load}
        />
      )}

      {/* Edit modal */}
      {editTarget && (
        <AgentForm
          initial={editTarget}
          onClose={() => setEditTarget(null)}
          onSaved={load}
        />
      )}

      {/* Prompt editor */}
      {promptsTarget && (
        <PromptEditor
          agentId={promptsTarget}
          onClose={() => setPromptsTarget(null)}
        />
      )}
    </div>
  )
}
