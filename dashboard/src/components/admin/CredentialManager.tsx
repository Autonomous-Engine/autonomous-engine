import { useState, useEffect, useCallback } from 'react'
import { useApi } from '@/hooks/useApi'
import type { CredentialEntry, CredentialCategory, CredentialScope } from '@/types'

const CATEGORIES: CredentialCategory[] = ['llm', 'github', 'jira', 'system', 'dashboard']

const CATEGORY_LABELS: Record<CredentialCategory, string> = {
  llm: 'LLM',
  github: 'GitHub',
  jira: 'Jira',
  system: 'System',
  dashboard: 'Dashboard',
}

const SCOPE_BADGE: Record<CredentialScope, string> = {
  global: 'bg-surface-700 text-fg',
  integration: 'bg-blue-700 text-blue-100',
  agent_type: 'bg-green-700 text-green-100',
  agent_instance: 'bg-purple-700 text-purple-100',
  github_account: 'bg-amber-700 text-amber-100',
}

function formatDate(iso: string): string {
  return new Date(iso).toLocaleString()
}

interface RevealModalProps {
  credId: string
  onClose: () => void
}

function RevealModal({ credId, onClose }: RevealModalProps) {
  const { post } = useApi()
  const [password, setPassword] = useState('')
  const [revealed, setRevealed] = useState<string | null>(null)
  const [countdown, setCountdown] = useState(30)
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)

  useEffect(() => {
    if (revealed === null) return
    if (countdown <= 0) { onClose(); return }
    const timer = setTimeout(() => setCountdown(c => c - 1), 1000)
    return () => clearTimeout(timer)
  }, [revealed, countdown, onClose])

  async function handleReveal() {
    setError('')
    setLoading(true)
    try {
      const res = await post<{ value: string }>(`/admin/credentials/reveal?id=${credId}`, { password })
      setRevealed(res.value)
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Failed to reveal')
    } finally {
      setLoading(false)
    }
  }

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/60">
      <div className="w-96 rounded-sm border border-rule bg-surface-900 p-6 shadow-xl">
        <h2 className="mb-4 text-base font-semibold text-fg">Reveal Credential</h2>
        {!revealed ? (
          <>
            <p className="mb-3 text-sm text-muted">Enter your password to view the credential value.</p>
            <input
              type="password"
              className="mb-3 w-full rounded-sm border border-rule bg-surface-950 px-3 py-2 text-sm text-fg focus:outline-none focus:ring-1 focus:ring-accent"
              placeholder="Your password"
              value={password}
              onChange={e => setPassword(e.target.value)}
              onKeyDown={e => e.key === 'Enter' && handleReveal()}
            />
            {error && <p className="mb-2 text-xs text-red-400">{error}</p>}
            <div className="flex justify-end gap-2">
              <button onClick={onClose} className="rounded px-3 py-1.5 text-sm text-muted hover:text-fg">Cancel</button>
              <button
                onClick={handleReveal}
                disabled={loading || !password}
                className="rounded bg-accent px-3 py-1.5 text-sm font-medium text-surface-900 hover:bg-accent-hover disabled:opacity-50"
              >
                {loading ? 'Revealing...' : 'Reveal'}
              </button>
            </div>
          </>
        ) : (
          <>
            <p className="mb-2 text-xs text-muted">Value visible for {countdown}s</p>
            <div className="mb-4 break-all rounded-sm border border-rule bg-surface-950 p-3 font-mono text-sm text-emerald-300">
              {revealed}
            </div>
            <div className="flex justify-end">
              <button onClick={onClose} className="rounded bg-surface-700 px-3 py-1.5 text-sm text-fg hover:bg-surface-700">Close</button>
            </div>
          </>
        )}
      </div>
    </div>
  )
}

interface EditModalProps {
  cred: CredentialEntry
  onClose: () => void
  onSaved: () => void
}

function EditModal({ cred, onClose, onSaved }: EditModalProps) {
  const { post } = useApi()
  const [value, setValue] = useState('')
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)

  async function handleSave() {
    setError('')
    setLoading(true)
    try {
      await post('/admin/credentials/create', {
        id: cred.id,
        scope: cred.scope,
        scope_target: cred.scope_target,
        key_name: cred.key_name,
        value,
        category: cred.category,
        is_secret: cred.is_secret,
        description: cred.description,
      })
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
      <div className="w-96 rounded-sm border border-rule bg-surface-900 p-6 shadow-xl">
        <h2 className="mb-4 text-base font-semibold text-fg">Edit Credential</h2>
        <p className="mb-2 text-sm text-muted">Key: <span className="font-mono text-fg">{cred.key_name}</span></p>
        <input
          type={cred.is_secret ? 'password' : 'text'}
          className="mb-3 w-full rounded-sm border border-rule bg-surface-950 px-3 py-2 text-sm text-fg focus:outline-none focus:ring-1 focus:ring-accent"
          placeholder="New value"
          value={value}
          onChange={e => setValue(e.target.value)}
        />
        {error && <p className="mb-2 text-xs text-red-400">{error}</p>}
        <div className="flex justify-end gap-2">
          <button onClick={onClose} className="rounded px-3 py-1.5 text-sm text-muted hover:text-fg">Cancel</button>
          <button
            onClick={handleSave}
            disabled={loading || !value}
            className="rounded bg-accent px-3 py-1.5 text-sm font-medium text-surface-900 hover:bg-accent-hover disabled:opacity-50"
          >
            {loading ? 'Saving...' : 'Save'}
          </button>
        </div>
      </div>
    </div>
  )
}

interface RotateModalProps {
  cred: CredentialEntry
  onClose: () => void
  onSaved: () => void
}

function RotateModal({ cred, onClose, onSaved }: RotateModalProps) {
  const { post } = useApi()
  const [value, setValue] = useState('')
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)

  async function handleRotate() {
    setError('')
    setLoading(true)
    try {
      await post(`/admin/credentials/rotate?id=${cred.id}`, { value })
      onSaved()
      onClose()
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Failed to rotate')
    } finally {
      setLoading(false)
    }
  }

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/60">
      <div className="w-96 rounded-sm border border-rule bg-surface-900 p-6 shadow-xl">
        <h2 className="mb-4 text-base font-semibold text-fg">Rotate Credential</h2>
        <p className="mb-2 text-sm text-muted">Key: <span className="font-mono text-fg">{cred.key_name}</span></p>
        <input
          type="password"
          className="mb-3 w-full rounded-sm border border-rule bg-surface-950 px-3 py-2 text-sm text-fg focus:outline-none focus:ring-1 focus:ring-accent"
          placeholder="New secret value"
          value={value}
          onChange={e => setValue(e.target.value)}
        />
        {error && <p className="mb-2 text-xs text-red-400">{error}</p>}
        <div className="flex justify-end gap-2">
          <button onClick={onClose} className="rounded px-3 py-1.5 text-sm text-muted hover:text-fg">Cancel</button>
          <button
            onClick={handleRotate}
            disabled={loading || !value}
            className="rounded bg-amber-600 px-3 py-1.5 text-sm font-medium text-white hover:bg-amber-500 disabled:opacity-50"
          >
            {loading ? 'Rotating...' : 'Rotate'}
          </button>
        </div>
      </div>
    </div>
  )
}

interface AddModalProps {
  onClose: () => void
  onSaved: () => void
}

function AddModal({ onClose, onSaved }: AddModalProps) {
  const { post } = useApi()
  const [form, setForm] = useState({
    category: 'llm' as CredentialCategory,
    scope: 'global' as CredentialScope,
    scope_target: '',
    key_name: '',
    value: '',
    is_secret: true,
    description: '',
  })
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)

  function update<K extends keyof typeof form>(key: K, val: (typeof form)[K]) {
    setForm(f => ({ ...f, [key]: val }))
  }

  async function handleAdd() {
    setError('')
    setLoading(true)
    try {
      const payload = { ...form }
      // Global scope is stored with scope_target='default' so it matches the
      // resolver chain in internal/configstore.ResolveCredential. Other pages
      // (OnboardingWizard, SystemConfig) already use this convention.
      if (payload.scope === 'global') { payload.scope_target = 'default' }
      await post('/admin/credentials/create', payload)
      onSaved()
      onClose()
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Failed to add')
    } finally {
      setLoading(false)
    }
  }

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/60">
      <div className="w-[440px] rounded-sm border border-rule bg-surface-900 p-6 shadow-xl">
        <h2 className="mb-4 text-base font-semibold text-fg">Add Credential</h2>
        <div className="space-y-3">
          <div>
            <label className="mb-1 block text-xs text-muted">Category</label>
            <select
              value={form.category}
              onChange={e => update('category', e.target.value as CredentialCategory)}
              className="w-full rounded-sm border border-rule bg-surface-950 px-3 py-2 text-sm text-fg focus:outline-none focus:ring-1 focus:ring-accent"
            >
              {CATEGORIES.map(c => <option key={c} value={c}>{CATEGORY_LABELS[c]}</option>)}
            </select>
          </div>
          <div>
            <label className="mb-1 block text-xs text-muted">Scope</label>
            <select
              value={form.scope}
              onChange={e => update('scope', e.target.value as CredentialScope)}
              className="w-full rounded-sm border border-rule bg-surface-950 px-3 py-2 text-sm text-fg focus:outline-none focus:ring-1 focus:ring-accent"
            >
              <option value="global">Global</option>
              <option value="integration">Integration</option>
              <option value="agent_type">Agent Type</option>
              <option value="agent_instance">Agent Instance</option>
            </select>
          </div>
          <div>
            <label className="mb-1 block text-xs text-muted">Scope Target</label>
            <input
              type="text"
              value={form.scope_target}
              onChange={e => update('scope_target', e.target.value)}
              className="w-full rounded-sm border border-rule bg-surface-950 px-3 py-2 text-sm text-fg focus:outline-none focus:ring-1 focus:ring-accent"
              placeholder="e.g. default, research, ..."
            />
          </div>
          <div>
            <label className="mb-1 block text-xs text-muted">Key Name</label>
            <input
              type="text"
              value={form.key_name}
              onChange={e => update('key_name', e.target.value)}
              className="w-full rounded-sm border border-rule bg-surface-950 px-3 py-2 text-sm text-fg focus:outline-none focus:ring-1 focus:ring-accent"
              placeholder="e.g. api_key, token, ..."
            />
          </div>
          <div>
            <label className="mb-1 block text-xs text-muted">Value</label>
            <input
              type={form.is_secret ? 'password' : 'text'}
              value={form.value}
              onChange={e => update('value', e.target.value)}
              className="w-full rounded-sm border border-rule bg-surface-950 px-3 py-2 text-sm text-fg focus:outline-none focus:ring-1 focus:ring-accent"
              placeholder="Credential value"
            />
          </div>
          <label className="flex items-center gap-2 text-sm text-muted">
            <input
              type="checkbox"
              checked={form.is_secret}
              onChange={e => update('is_secret', e.target.checked)}
              className="rounded-sm border-rule"
            />
            Secret (mask value)
          </label>
          <div>
            <label className="mb-1 block text-xs text-muted">Description</label>
            <textarea
              value={form.description}
              onChange={e => update('description', e.target.value)}
              rows={2}
              className="w-full rounded-sm border border-rule bg-surface-950 px-3 py-2 text-sm text-fg focus:outline-none focus:ring-1 focus:ring-accent"
              placeholder="Optional description"
            />
          </div>
        </div>
        {error && <p className="mt-2 text-xs text-red-400">{error}</p>}
        <div className="mt-4 flex justify-end gap-2">
          <button onClick={onClose} className="rounded px-3 py-1.5 text-sm text-muted hover:text-fg">Cancel</button>
          <button
            onClick={handleAdd}
            disabled={loading || !form.key_name || !form.value}
            className="rounded bg-accent px-3 py-1.5 text-sm font-medium text-surface-900 hover:bg-accent-hover disabled:opacity-50"
          >
            {loading ? 'Adding...' : 'Add Credential'}
          </button>
        </div>
      </div>
    </div>
  )
}

export default function CredentialManager() {
  const { get, del } = useApi()
  const [activeTab, setActiveTab] = useState<CredentialCategory>('llm')
  const [credentials, setCredentials] = useState<CredentialEntry[]>([])
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState('')

  const [revealId, setRevealId] = useState<string | null>(null)
  const [editCred, setEditCred] = useState<CredentialEntry | null>(null)
  const [rotateCred, setRotateCred] = useState<CredentialEntry | null>(null)
  const [showAdd, setShowAdd] = useState(false)
  const [deleteId, setDeleteId] = useState<string | null>(null)

  const fetchCredentials = useCallback(async () => {
    setLoading(true)
    setError('')
    try {
      const data = await get<CredentialEntry[]>(`/admin/credentials?category=${activeTab}`)
      setCredentials(data ?? [])
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Failed to load credentials')
    } finally {
      setLoading(false)
    }
  }, [get, activeTab])

  useEffect(() => { fetchCredentials() }, [fetchCredentials])

  async function handleDelete(id: string) {
    try {
      await del(`/admin/credentials/delete?id=${id}`)
      fetchCredentials()
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Failed to delete')
    } finally {
      setDeleteId(null)
    }
  }

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <h1 className="text-lg font-semibold text-fg">Credential Manager</h1>
        <button
          onClick={() => setShowAdd(true)}
          className="rounded bg-accent px-3 py-1.5 text-sm font-medium text-surface-900 hover:bg-accent-hover"
        >
          + Add Credential
        </button>
      </div>

      {/* Tab Bar */}
      <div className="flex gap-1 border-b border-rule">
        {CATEGORIES.map(cat => (
          <button
            key={cat}
            onClick={() => setActiveTab(cat)}
            className={[
              'px-4 py-2 text-sm font-medium transition-colors',
              activeTab === cat
                ? 'border-b-2 border-accent text-accent'
                : 'text-muted hover:text-fg',
            ].join(' ')}
          >
            {CATEGORY_LABELS[cat]}
          </button>
        ))}
      </div>

      {error && <p className="text-sm text-red-400">{error}</p>}

      {loading ? (
        <p className="text-sm text-muted">Loading...</p>
      ) : (
        <div className="overflow-x-auto rounded-sm border border-rule">
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b border-rule bg-surface-950 text-left">
                <th className="px-4 py-3 text-xs font-medium uppercase tracking-wider text-muted">Scope</th>
                <th className="px-4 py-3 text-xs font-medium uppercase tracking-wider text-muted">Scope Target</th>
                <th className="px-4 py-3 text-xs font-medium uppercase tracking-wider text-muted">Key Name</th>
                <th className="px-4 py-3 text-xs font-medium uppercase tracking-wider text-muted">Value</th>
                <th className="px-4 py-3 text-xs font-medium uppercase tracking-wider text-muted">Updated</th>
                <th className="px-4 py-3 text-xs font-medium uppercase tracking-wider text-muted">Actions</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-rule/50">
              {credentials.length === 0 && (
                <tr>
                  <td colSpan={6} className="px-4 py-6 text-center text-dim">
                    No credentials in this category.
                  </td>
                </tr>
              )}
              {credentials.map(cred => (
                <tr key={cred.id} className="bg-surface-900 hover:bg-surface-800/50">
                  <td className="px-4 py-3">
                    <span className={`rounded px-2 py-0.5 text-xs font-medium ${SCOPE_BADGE[cred.scope]}`}>
                      {cred.scope}
                    </span>
                  </td>
                  <td className="px-4 py-3 font-mono text-xs text-muted">{cred.scope_target}</td>
                  <td className="px-4 py-3 font-mono text-xs text-fg">{cred.key_name}</td>
                  <td className="px-4 py-3 font-mono text-xs text-muted">
                    {cred.is_secret ? '••••••••' : cred.value}
                  </td>
                  <td className="px-4 py-3 text-xs text-muted">{formatDate(cred.updated_at)}</td>
                  <td className="px-4 py-3">
                    <div className="flex items-center gap-1">
                      {cred.is_secret && (
                        <button
                          onClick={() => setRevealId(cred.id)}
                          title="Reveal"
                          className="rounded p-1 text-muted hover:bg-surface-700 hover:text-fg"
                        >
                          <svg className="h-4 w-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                            <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M15 12a3 3 0 11-6 0 3 3 0 016 0z" />
                            <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M2.458 12C3.732 7.943 7.523 5 12 5c4.478 0 8.268 2.943 9.542 7-1.274 4.057-5.064 7-9.542 7-4.477 0-8.268-2.943-9.542-7z" />
                          </svg>
                        </button>
                      )}
                      <button
                        onClick={() => setEditCred(cred)}
                        title="Edit"
                        className="rounded p-1 text-muted hover:bg-surface-700 hover:text-fg"
                      >
                        <svg className="h-4 w-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                          <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M11 5H6a2 2 0 00-2 2v11a2 2 0 002 2h11a2 2 0 002-2v-5m-1.414-9.414a2 2 0 112.828 2.828L11.828 15H9v-2.828l8.586-8.586z" />
                        </svg>
                      </button>
                      {cred.is_secret && (
                        <button
                          onClick={() => setRotateCred(cred)}
                          title="Rotate"
                          className="rounded p-1 text-muted hover:bg-surface-700 hover:text-amber-300"
                        >
                          <svg className="h-4 w-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                            <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M4 4v5h.582m15.356 2A8.001 8.001 0 004.582 9m0 0H9m11 11v-5h-.581m0 0a8.003 8.003 0 01-15.357-2m15.357 2H15" />
                          </svg>
                        </button>
                      )}
                      <button
                        onClick={() => setDeleteId(cred.id)}
                        title="Delete"
                        className="rounded p-1 text-muted hover:bg-surface-700 hover:text-red-400"
                      >
                        <svg className="h-4 w-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                          <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M19 7l-.867 12.142A2 2 0 0116.138 21H7.862a2 2 0 01-1.995-1.858L5 7m5 4v6m4-6v6m1-10V4a1 1 0 00-1-1h-4a1 1 0 00-1 1v3M4 7h16" />
                        </svg>
                      </button>
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {/* Delete confirm */}
      {deleteId && (
        <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/60">
          <div className="w-80 rounded-sm border border-rule bg-surface-900 p-6 shadow-xl">
            <h2 className="mb-3 text-base font-semibold text-fg">Delete Credential</h2>
            <p className="mb-4 text-sm text-muted">This action cannot be undone. Are you sure?</p>
            <div className="flex justify-end gap-2">
              <button onClick={() => setDeleteId(null)} className="rounded px-3 py-1.5 text-sm text-muted hover:text-fg">Cancel</button>
              <button onClick={() => handleDelete(deleteId)} className="rounded bg-red-600 px-3 py-1.5 text-sm font-medium text-white hover:bg-red-500">Delete</button>
            </div>
          </div>
        </div>
      )}

      {revealId && <RevealModal credId={revealId} onClose={() => setRevealId(null)} />}
      {editCred && <EditModal cred={editCred} onClose={() => setEditCred(null)} onSaved={fetchCredentials} />}
      {rotateCred && <RotateModal cred={rotateCred} onClose={() => setRotateCred(null)} onSaved={fetchCredentials} />}
      {showAdd && <AddModal onClose={() => setShowAdd(false)} onSaved={fetchCredentials} />}
    </div>
  )
}
