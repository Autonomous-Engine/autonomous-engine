import { useState, useEffect, useCallback, useRef } from 'react'
import { useNavigate } from 'react-router-dom'
import { useApi } from '@/hooks/useApi'
import AutoIndexToggle from '@/components/codex/AutoIndexToggle'
import RepositoryVerificationPolicyEditor from '@/components/RepositoryVerificationPolicyEditor'
import RepositoryZonesEditor from '@/components/RepositoryZonesEditor'
import type { Product, Repository, RepoDependency, GitHubAccount } from '@/types'

// ─── GitHub repo browser types ────────────────────────────────────────────────

interface GitHubRepo {
  full_name: string
  name: string
  description: string
  private: boolean
  language: string
  updated_at: string
  html_url: string
  default_branch: string
  size_kb: number
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

function formatDate(iso?: string): string {
  if (!iso) return '—'
  return new Date(iso).toLocaleString()
}

function inputClass(extra = '') {
  return `w-full rounded-sm border border-rule bg-surface-950 px-3 py-2 text-sm text-fg focus:outline-none focus:ring-1 focus:ring-accent ${extra}`
}

function Badge({ color, label }: { color: string; label: string }) {
  return (
    <span className={`inline-flex items-center rounded-full px-2 py-0.5 text-xs font-medium ${color}`}>
      {label}
    </span>
  )
}

// ─── Verification policy toggle ───────────────────────────────────────────────
//
// Partially verified work is blocked by default. A
// product owner may opt in, after which partially verified implementations
// advance only to mandatory human review and are never auto-released.

function PartialVerificationToggle({
  productId,
  initialValue,
  onChanged,
}: {
  productId: string
  initialValue: boolean
  onChanged?: (optIn: boolean) => void
}) {
  const { post } = useApi()
  const [enabled, setEnabled] = useState(initialValue)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => { setEnabled(initialValue) }, [initialValue])

  const handleToggle = useCallback(async () => {
    const newValue = !enabled
    setSaving(true)
    setError(null)
    try {
      await post('/admin/products/partial-verification', { product_id: productId, opt_in: newValue })
      setEnabled(newValue)
      onChanged?.(newValue)
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : 'Failed to update verification policy')
    } finally {
      setSaving(false)
    }
  }, [productId, enabled, post, onChanged])

  return (
    <div className="rounded-sm border border-rule bg-surface-950 px-4 py-3">
      <div className="flex flex-wrap items-center gap-3">
        <label className="flex cursor-pointer items-center gap-2">
          <button
            type="button"
            role="switch"
            aria-checked={enabled}
            disabled={saving}
            onClick={handleToggle}
            className={[
              'relative inline-flex h-6 w-11 shrink-0 rounded-full border-2 border-transparent transition-colors',
              enabled ? 'bg-amber-500' : 'bg-surface-700',
              saving ? 'opacity-50' : '',
            ].join(' ')}
          >
            <span
              className={[
                'pointer-events-none inline-block h-5 w-5 rounded-full bg-surface-900 shadow transition-transform',
                enabled ? 'translate-x-5' : 'translate-x-0',
              ].join(' ')}
            />
          </button>
          <span className="text-sm font-medium text-fg">
            Allow partially verified work (requires human review)
          </span>
        </label>
        {saving && <span className="font-mono text-xs text-dim">Saving...</span>}
        {error && <span className="font-mono text-xs text-red-400">{error}</span>}
      </div>
      <p className="mt-2 text-xs text-muted">
        Default policy: block when required behavioral verification cannot run. When enabled, an
        implementation whose test run was skipped because no test runner was detected, while its other
        checks (import diagnostics) passed, may become <span className="text-amber-300">partially verified</span>,
        but it advances only to mandatory human review; it is never represented as verified or released
        automatically. The evidence names every skipped check and the reason. Missing configuration, broken
        dependencies (including unresolved imports), timeouts, and a failing test runner are still failures,
        not partial verification.
      </p>
    </div>
  )
}

// M2-06: products.contracts_strict. Off (default): a cross-repository
// contract the PR-time checkers cannot evaluate is recorded as "unchecked"
// on the PR and never blocks. On: it blocks the consumer's PR like a
// mismatch.
function ContractsStrictToggle({
  productId,
  initialValue,
  onChanged,
}: {
  productId: string
  initialValue: boolean
  onChanged?: (strict: boolean) => void
}) {
  const { post } = useApi()
  const [enabled, setEnabled] = useState(initialValue)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => { setEnabled(initialValue) }, [initialValue])

  const handleToggle = useCallback(async () => {
    const newValue = !enabled
    setSaving(true)
    setError(null)
    try {
      await post('/admin/products/contracts-strict', { product_id: productId, strict: newValue })
      setEnabled(newValue)
      onChanged?.(newValue)
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : 'Failed to update contract policy')
    } finally {
      setSaving(false)
    }
  }, [productId, enabled, post, onChanged])

  return (
    <div className="mt-3 rounded-sm border border-rule bg-surface-950 px-4 py-3">
      <div className="flex flex-wrap items-center gap-3">
        <label className="flex cursor-pointer items-center gap-2">
          <button
            type="button"
            role="switch"
            aria-checked={enabled}
            aria-label="Block on unchecked cross-repository contracts"
            disabled={saving}
            onClick={handleToggle}
            className={[
              'relative inline-flex h-6 w-11 shrink-0 rounded-full border-2 border-transparent transition-colors',
              enabled ? 'bg-amber-500' : 'bg-surface-700',
              saving ? 'opacity-50' : '',
            ].join(' ')}
          >
            <span
              className={[
                'pointer-events-none inline-block h-5 w-5 rounded-full bg-surface-900 shadow transition-transform',
                enabled ? 'translate-x-5' : 'translate-x-0',
              ].join(' ')}
            />
          </button>
          <span className="text-sm font-medium text-fg">
            Block on unchecked cross-repository contracts (strict)
          </span>
        </label>
        {saving && <span className="font-mono text-xs text-dim">Saving...</span>}
        {error && <span className="font-mono text-xs text-red-400">{error}</span>}
      </div>
      <p className="mt-2 text-xs text-muted">
        Before a consumer repository's PR opens, its contracts with the producer repository (REST routes and
        response fields, event payloads, database columns) are checked against both branches. A mismatch always
        blocks. A contract the checkers cannot evaluate is recorded as unchecked; with strict on, it blocks too.
      </p>
    </div>
  )
}

// ─── Modals ───────────────────────────────────────────────────────────────────

interface CreateProductModalProps {
  onClose: () => void
  onCreated: (p: Product) => void
}

function CreateProductModal({ onClose, onCreated }: CreateProductModalProps) {
  const { post } = useApi()
  const [name, setName] = useState('')
  const [description, setDescription] = useState('')
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState('')

  async function handleSubmit() {
    if (!name.trim()) { setError('Name is required.'); return }
    setSaving(true)
    setError('')
    try {
      const product = await post<Product>('/admin/products/create', { name: name.trim(), description: description.trim() })
      if (product) onCreated(product)
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Failed to create product')
    } finally {
      setSaving(false)
    }
  }

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 p-4">
      <div className="w-full max-w-md rounded-sm border border-rule bg-surface-950 shadow-2xl">
        <div className="border-b border-rule px-6 py-4">
          <h3 className="text-sm font-semibold text-fg">Create Product</h3>
        </div>
        <div className="space-y-4 p-6">
          <div>
            <label className="mb-1 block text-xs text-muted">Product Name</label>
            <input
              type="text"
              value={name}
              onChange={e => setName(e.target.value)}
              placeholder="My Product"
              className={inputClass()}
              autoFocus
            />
          </div>
          <div>
            <label className="mb-1 block text-xs text-muted">Description</label>
            <textarea
              value={description}
              onChange={e => setDescription(e.target.value)}
              placeholder="Brief description…"
              rows={3}
              className={inputClass('resize-none')}
            />
          </div>
          {error && <p className="text-xs text-red-400">{error}</p>}
        </div>
        <div className="flex justify-end gap-3 border-t border-rule px-6 py-4">
          <button onClick={onClose} className="rounded-sm border border-rule px-4 py-2 text-sm text-muted hover:bg-surface-800">
            Cancel
          </button>
          <button
            onClick={handleSubmit}
            disabled={saving}
            className="rounded bg-accent px-4 py-2 text-sm font-medium text-surface-900 hover:bg-accent-hover disabled:opacity-50"
          >
            {saving ? 'Creating…' : 'Create'}
          </button>
        </div>
      </div>
    </div>
  )
}

interface AddRepoModalProps {
  productId: string
  onClose: () => void
  onAdded: (r: Repository) => void
}

function AddRepoModal({ productId, onClose, onAdded }: AddRepoModalProps) {
  const { get, post } = useApi()

  // GitHub account selection
  const [accounts, setAccounts] = useState<GitHubAccount[]>([])
  const [selectedAccountId, setSelectedAccountId] = useState<string>('')

  // browser vs manual mode
  const [mode, setMode] = useState<'browser' | 'manual'>('browser')

  // repo browser state
  const [repos, setRepos] = useState<GitHubRepo[]>([])
  const [loadingRepos, setLoadingRepos] = useState(false)
  const [reposError, setReposError] = useState('')
  const [search, setSearch] = useState('')
  const [selectedRepo, setSelectedRepo] = useState<GitHubRepo | null>(null)
  const searchRef = useRef<HTMLInputElement>(null)

  // shared fields
  const [mergeStrategy, setMergeStrategy] = useState<'squash' | 'merge' | 'rebase'>('squash')
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState('')

  // clone result state (shown after successful add)
  const [addedRepo, setAddedRepo] = useState<Repository | null>(null)
  const [cloneSuccess, setCloneSuccess] = useState<boolean | null>(null)
  const [cloneMessage, setCloneMessage] = useState('')

  // manual entry state
  const [githubUrl, setGithubUrl] = useState('')
  const [name, setName] = useState('')
  const [baseBranch, setBaseBranch] = useState('main')

  // fetch GitHub accounts on mount
  useEffect(() => {
    get<GitHubAccount[]>('/admin/github/accounts').then(accts => {
      if (accts?.length) {
        setAccounts(accts)
        setSelectedAccountId(accts[0].id)
      }
    }).catch(() => {})
  }, [get])

  // fetch repos for the selected account
  useEffect(() => {
    if (!selectedAccountId) return
    setRepos([])
    setLoadingRepos(true)
    setReposError('')
    get<GitHubRepo[]>('/admin/github/accounts/repos?id=' + selectedAccountId)
      .then(r => { if (r) setRepos(r) })
      .catch(e => setReposError(e instanceof Error ? e.message : 'Failed to load repos'))
      .finally(() => setLoadingRepos(false))
  }, [selectedAccountId, get])

  useEffect(() => {
    if (mode === 'browser') searchRef.current?.focus()
  }, [mode])

  const filtered = repos.filter(r => {
    const q = search.toLowerCase()
    return !q || r.full_name.toLowerCase().includes(q) || (r.description ?? '').toLowerCase().includes(q)
  })

  function handleUrlChange(val: string) {
    setGithubUrl(val)
    const match = val.match(/github\.com\/[^/]+\/([^/]+?)(?:\.git)?$/)
    if (match) setName(match[1])
  }

  async function handleSubmit() {
    if (mode === 'browser' && !selectedRepo) { setError('Select a repository first.'); return }
    if (mode === 'manual' && !githubUrl.trim()) { setError('GitHub URL is required.'); return }

    const payload = mode === 'browser'
      ? {
          product_id: productId,
          github_url: selectedRepo!.full_name,
          name: selectedRepo!.name,
          base_branch: selectedRepo!.default_branch,
          merge_strategy: mergeStrategy,
          github_account_id: selectedAccountId || undefined,
        }
      : {
          product_id: productId,
          github_url: githubUrl.trim(),
          name: name.trim() || githubUrl.trim(),
          base_branch: baseBranch,
          merge_strategy: mergeStrategy,
          github_account_id: selectedAccountId || undefined,
        }

    setSaving(true)
    setError('')
    try {
      const resp = await post<Repository & { clone_success?: boolean; clone_message?: string }>(
        '/admin/repositories/create', payload
      )
      if (resp) {
        setAddedRepo(resp)
        setCloneSuccess(resp.clone_success ?? null)
        setCloneMessage(resp.clone_message ?? '')
        onAdded(resp)
      }
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Failed to add repository')
    } finally {
      setSaving(false)
    }
  }

  function formatUpdated(iso: string) {
    if (!iso) return ''
    const d = new Date(iso)
    return d.toLocaleDateString(undefined, { month: 'short', day: 'numeric', year: 'numeric' })
  }

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 p-4">
      <div className="flex w-full max-w-2xl flex-col rounded-sm border border-rule bg-surface-950 shadow-2xl" style={{ maxHeight: '90vh' }}>
        {/* Header */}
        <div className="flex items-center justify-between border-b border-rule px-6 py-4">
          <h3 className="text-sm font-semibold text-fg">Add Repository</h3>
          <button onClick={onClose} className="text-muted hover:text-fg">
            <svg className="h-4 w-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
              <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M6 18L18 6M6 6l12 12" />
            </svg>
          </button>
        </div>

        {/* Body */}
        <div className="flex min-h-0 flex-1 flex-col overflow-hidden">
          {/* Account picker */}
          <div className="mb-3 px-4 pt-3">
            <label className="mb-1 block text-xs text-muted">GitHub Account</label>
            <select
              value={selectedAccountId}
              onChange={e => setSelectedAccountId(e.target.value)}
              className={inputClass()}
            >
              {accounts.length === 0 && <option value="">No accounts configured</option>}
              {accounts.map(a => (
                <option key={a.id} value={a.id}>{a.username}{a.label ? ` (${a.label})` : ''}{!a.valid ? ' (invalid token)' : ''}</option>
              ))}
            </select>
          </div>

          {mode === 'browser' ? (
            <>
              {/* Search bar */}
              <div className="border-b border-rule px-4 py-3">
                <input
                  ref={searchRef}
                  type="text"
                  value={search}
                  onChange={e => setSearch(e.target.value)}
                  placeholder="Search repos by name or description…"
                  className={inputClass()}
                />
              </div>

              {/* Repo list */}
              <div className="flex-1 overflow-y-auto">
                {loadingRepos ? (
                  <div className="flex items-center justify-center py-16 text-sm text-muted">
                    <svg className="mr-2 h-4 w-4 animate-spin" fill="none" viewBox="0 0 24 24">
                      <circle className="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" strokeWidth="4" />
                      <path className="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8v8H4z" />
                    </svg>
                    Loading repositories…
                  </div>
                ) : reposError ? (
                  <div className="px-6 py-8 text-center">
                    <p className="text-sm text-red-400">{reposError}</p>
                    <button
                      onClick={() => setMode('manual')}
                      className="mt-3 text-xs text-accent-dim hover:underline"
                    >
                      Switch to manual entry
                    </button>
                  </div>
                ) : filtered.length === 0 ? (
                  <div className="px-6 py-8 text-center text-sm text-muted">
                    {search ? 'No repos match your search.' : 'No repositories found.'}
                  </div>
                ) : (
                  <ul className="divide-y divide-slate-800">
                    {filtered.map(repo => {
                      const isSelected = selectedRepo?.full_name === repo.full_name
                      return (
                        <li
                          key={repo.full_name}
                          onClick={() => setSelectedRepo(isSelected ? null : repo)}
                          className={`cursor-pointer px-5 py-3 transition-colors ${isSelected ? 'bg-indigo-900/40 ring-1 ring-inset ring-indigo-600' : 'hover:bg-surface-800/50'}`}
                        >
                          <div className="flex items-start gap-3">
                            {/* Selection indicator */}
                            <div className={`mt-0.5 h-4 w-4 flex-shrink-0 rounded-full border-2 transition-colors ${isSelected ? 'border-accent bg-indigo-500' : 'border-rule'}`} />
                            <div className="min-w-0 flex-1">
                              <div className="flex flex-wrap items-center gap-2">
                                <span className="text-sm font-semibold text-fg">{repo.full_name}</span>
                                {repo.private ? (
                                  <span className="rounded-full bg-amber-900/50 px-1.5 py-0.5 text-xs text-amber-300">private</span>
                                ) : (
                                  <span className="rounded-full bg-surface-700 px-1.5 py-0.5 text-xs text-muted">public</span>
                                )}
                                {repo.language && (
                                  <span className="rounded-full bg-indigo-900/50 px-1.5 py-0.5 text-xs text-accent">{repo.language}</span>
                                )}
                              </div>
                              {repo.description && (
                                <p className="mt-0.5 truncate text-xs text-muted">{repo.description}</p>
                              )}
                              <p className="mt-0.5 text-xs text-dim">
                                Branch: <span className="text-muted">{repo.default_branch}</span>
                                {repo.updated_at && <> · Updated {formatUpdated(repo.updated_at)}</>}
                              </p>
                            </div>
                          </div>
                        </li>
                      )
                    })}
                  </ul>
                )}
              </div>

              {/* Merge strategy + manual link */}
              <div className="border-t border-rule px-5 py-4 space-y-3">
                <div className="flex items-center gap-4">
                  <label className="w-32 flex-shrink-0 text-xs text-muted">Merge Strategy</label>
                  <select
                    value={mergeStrategy}
                    onChange={e => setMergeStrategy(e.target.value as 'squash' | 'merge' | 'rebase')}
                    className={inputClass('flex-1')}
                  >
                    <option value="squash">Squash</option>
                    <option value="merge">Merge</option>
                    <option value="rebase">Rebase</option>
                  </select>
                </div>
                {selectedRepo && (
                  <p className="text-xs text-muted">
                    Selected: <span className="font-medium text-fg">{selectedRepo.full_name}</span>
                    {' '}· branch <span className="font-medium text-fg">{selectedRepo.default_branch}</span>
                  </p>
                )}
                {error && <p className="text-xs text-red-400">{error}</p>}
                <p className="text-xs text-dim">
                  Need a repo from another org?{' '}
                  <button onClick={() => setMode('manual')} className="text-accent-dim hover:underline">
                    Manual entry
                  </button>
                </p>
              </div>
            </>
          ) : (
            /* Manual entry mode */
            <div className="space-y-4 overflow-y-auto p-6">
              <div>
                <label className="mb-1 block text-xs text-muted">GitHub URL (org/repo)</label>
                <input
                  type="text"
                  value={githubUrl}
                  onChange={e => handleUrlChange(e.target.value)}
                  placeholder="https://github.com/org/repo"
                  className={inputClass()}
                  autoFocus
                />
              </div>
              <div>
                <label className="mb-1 block text-xs text-muted">Display Name</label>
                <input
                  type="text"
                  value={name}
                  onChange={e => setName(e.target.value)}
                  placeholder="Auto-filled from URL"
                  className={inputClass()}
                />
              </div>
              <div className="grid grid-cols-2 gap-3">
                <div>
                  <label className="mb-1 block text-xs text-muted">Base Branch</label>
                  <select
                    value={baseBranch}
                    onChange={e => setBaseBranch(e.target.value)}
                    className={inputClass()}
                  >
                    <option value="main">main</option>
                    <option value="master">master</option>
                    <option value="develop">develop</option>
                  </select>
                </div>
                <div>
                  <label className="mb-1 block text-xs text-muted">Merge Strategy</label>
                  <select
                    value={mergeStrategy}
                    onChange={e => setMergeStrategy(e.target.value as 'squash' | 'merge' | 'rebase')}
                    className={inputClass()}
                  >
                    <option value="squash">Squash</option>
                    <option value="merge">Merge</option>
                    <option value="rebase">Rebase</option>
                  </select>
                </div>
              </div>
              {error && <p className="text-xs text-red-400">{error}</p>}
              <p className="text-xs text-dim">
                <button onClick={() => setMode('browser')} className="text-accent-dim hover:underline">
                  ← Back to repo browser
                </button>
              </p>
            </div>
          )}
        </div>

        {/* Clone result banner — shown after successful add */}
        {addedRepo && cloneSuccess !== null && (
          <div className={`border-t px-6 py-3 flex items-center gap-2 text-sm ${cloneSuccess ? 'border-emerald-700/50 bg-emerald-900/20 text-emerald-300' : 'border-red-700/50 bg-red-900/20 text-red-300'}`}>
            <span>{cloneSuccess ? '✓' : '✗'}</span>
            <span>{cloneMessage || (cloneSuccess ? 'Cloned successfully' : 'Clone failed')}</span>
          </div>
        )}

        {/* Footer */}
        <div className="flex justify-end gap-3 border-t border-rule px-6 py-4">
          <button onClick={onClose} className="rounded-sm border border-rule px-4 py-2 text-sm text-muted hover:bg-surface-800">
            {addedRepo ? 'Close' : 'Cancel'}
          </button>
          {!addedRepo && (
            <button
              onClick={handleSubmit}
              disabled={saving || (mode === 'browser' && !selectedRepo)}
              className="rounded bg-accent px-4 py-2 text-sm font-medium text-surface-900 hover:bg-accent-hover disabled:opacity-50"
            >
              {saving ? 'Adding & Cloning…' : mode === 'browser' ? 'Add Selected' : 'Add Repository'}
            </button>
          )}
        </div>
      </div>
    </div>
  )
}

interface AddDependencyModalProps {
  repos: Repository[]
  onClose: () => void
  onAdded: (d: RepoDependency) => void
}

const DEP_TYPES = ['api_consumer', 'shared_library', 'deployment', 'data']

function AddDependencyModal({ repos, onClose, onAdded }: AddDependencyModalProps) {
  const { post } = useApi()
  const [sourceId, setSourceId] = useState(repos[0]?.id ?? '')
  const [targetId, setTargetId] = useState(repos[1]?.id ?? repos[0]?.id ?? '')
  const [depType, setDepType] = useState('api_consumer')
  const [description, setDescription] = useState('')
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState('')

  async function handleSubmit() {
    if (sourceId === targetId) { setError('Source and target must differ.'); return }
    setSaving(true)
    setError('')
    try {
      const dep = await post<RepoDependency>(`/admin/dependencies/create`, {
        source_repo_id: sourceId,
        target_repo_id: targetId,
        dependency_type: depType,
        description: description.trim(),
      })
      if (dep) onAdded(dep)
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Failed to add dependency')
    } finally {
      setSaving(false)
    }
  }

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 p-4">
      <div className="w-full max-w-md rounded-sm border border-rule bg-surface-950 shadow-2xl">
        <div className="border-b border-rule px-6 py-4">
          <h3 className="text-sm font-semibold text-fg">Add Dependency</h3>
        </div>
        <div className="space-y-4 p-6">
          <div className="grid grid-cols-2 gap-3">
            <div>
              <label className="mb-1 block text-xs text-muted">Source Repo</label>
              <select value={sourceId} onChange={e => setSourceId(e.target.value)} className={inputClass()}>
                {repos.map(r => <option key={r.id} value={r.id}>{r.name}</option>)}
              </select>
            </div>
            <div>
              <label className="mb-1 block text-xs text-muted">Target Repo</label>
              <select value={targetId} onChange={e => setTargetId(e.target.value)} className={inputClass()}>
                {repos.map(r => <option key={r.id} value={r.id}>{r.name}</option>)}
              </select>
            </div>
          </div>
          <div>
            <label className="mb-1 block text-xs text-muted">Dependency Type</label>
            <select value={depType} onChange={e => setDepType(e.target.value)} className={inputClass()}>
              {DEP_TYPES.map(t => <option key={t} value={t}>{t}</option>)}
            </select>
          </div>
          <div>
            <label className="mb-1 block text-xs text-muted">Description</label>
            <input
              type="text"
              value={description}
              onChange={e => setDescription(e.target.value)}
              placeholder="Optional description…"
              className={inputClass()}
            />
          </div>
          {error && <p className="text-xs text-red-400">{error}</p>}
        </div>
        <div className="flex justify-end gap-3 border-t border-rule px-6 py-4">
          <button onClick={onClose} className="rounded-sm border border-rule px-4 py-2 text-sm text-muted hover:bg-surface-800">
            Cancel
          </button>
          <button
            onClick={handleSubmit}
            disabled={saving}
            className="rounded bg-accent px-4 py-2 text-sm font-medium text-surface-900 hover:bg-accent-hover disabled:opacity-50"
          >
            {saving ? 'Adding…' : 'Add Dependency'}
          </button>
        </div>
      </div>
    </div>
  )
}

// ─── Product expanded view ────────────────────────────────────────────────────

interface ProductDetailProps {
  product: Product
  onStartOnboarding: (productId: string) => void
  // Called after the D2 opt-in flag is persisted so the parent list stays in
  // sync when the row is collapsed and re-expanded.
  onPolicyChanged?: (productId: string, optIn: boolean) => void
}

function ProductDetail({ product, onStartOnboarding, onPolicyChanged }: ProductDetailProps) {
  const { get, post, put, del } = useApi()
  const [repos, setRepos] = useState<Repository[]>([])
  const [deps, setDeps] = useState<RepoDependency[]>([])
  const [loadingRepos, setLoadingRepos] = useState(true)
  const [detailError, setDetailError] = useState<string | null>(null)
  const [allAccounts, setAllAccounts] = useState<GitHubAccount[]>([])
  useEffect(() => {
    get<GitHubAccount[]>('/admin/github/accounts').then(a => { if (a) setAllAccounts(a) }).catch(() => {})
  }, [get])
  const accountsMap = Object.fromEntries(allAccounts.map(a => [a.id, a]))
  const [showAddRepo, setShowAddRepo] = useState(false)
  const [showAddDep, setShowAddDep] = useState(false)
  const [confirmDeleteRepo, setConfirmDeleteRepo] = useState<string | null>(null)
  const [confirmDeleteDep, setConfirmDeleteDep] = useState<string | null>(null)
  const [editingRepo, setEditingRepo] = useState<string | null>(null)
  const [editFields, setEditFields] = useState<Partial<Repository>>({})
  // Repository whose verification policy editor is open (M3-03).
  const [policyRepo, setPolicyRepo] = useState<string | null>(null)
  const [zonesRepo, setZonesRepo] = useState<string | null>(null)
  const [reindexing, setReindexing] = useState(false)
  const [reindexMsg, setReindexMsg] = useState('')
  const [tokenOverrides, setTokenOverrides] = useState<Record<string, string>>({})
  const [copyFrom, setCopyFrom] = useState<Record<string, string>>({})
  const [pulling, setPulling] = useState<Record<string, boolean>>({})
  const [pullMsg, setPullMsg] = useState<Record<string, string>>({})

  const fetchRepos = useCallback(async () => {
    setLoadingRepos(true)
    setDetailError(null)
    try {
      const [repoData, depData] = await Promise.all([
        get<Repository[]>(`/admin/repositories?product_id=${product.id}`),
        get<RepoDependency[]>(`/admin/dependencies?product_id=${product.id}`),
      ])
      setRepos(repoData)
      setDeps(depData)
    } catch (err) {
      console.error('Failed to fetch:', err)
      setDetailError(err instanceof Error ? err.message : 'Failed to load data')
    } finally {
      setLoadingRepos(false)
    }
  }, [get, product.id])

  useEffect(() => { fetchRepos() }, [fetchRepos])

  async function handleDeleteRepo(repoId: string) {
    try {
      await del(`/admin/repositories/delete?id=${repoId}`)
      setRepos(r => r.filter(x => x.id !== repoId))
    } catch (err) {
      console.error('Failed to fetch:', err)
      setDetailError(err instanceof Error ? err.message : 'Failed to delete repository')
    } finally {
      setConfirmDeleteRepo(null)
    }
  }

  async function handleDeleteDep(depId: string) {
    try {
      await del(`/admin/dependencies/delete?id=${depId}`)
      setDeps(d => d.filter(x => x.id !== depId))
    } catch (err) {
      console.error('Failed to fetch:', err)
      setDetailError(err instanceof Error ? err.message : 'Failed to delete dependency')
    } finally {
      setConfirmDeleteDep(null)
    }
  }

  async function handleSaveRepoEdit(repoId: string) {
    try {
      await put(`/admin/repositories/update?id=${repoId}`, editFields)
      setRepos(r => r.map(x => x.id === repoId ? { ...x, ...editFields } : x))
    } catch (err) {
      console.error('Failed to fetch:', err)
      setDetailError(err instanceof Error ? err.message : 'Failed to save repository changes')
    } finally {
      setEditingRepo(null)
      setEditFields({})
    }
  }

  async function handleCopyConfig(repoId: string, fromId: string) {
    const source = repos.find(r => r.id === fromId)
    if (!source) return
    const fields = {
      base_branch: source.base_branch,
      merge_strategy: source.merge_strategy,
      branch_prefix: source.branch_prefix,
    }
    try {
      await put(`/admin/repositories/update?id=${repoId}`, fields)
      setRepos(r => r.map(x => x.id === repoId ? { ...x, ...fields } : x))
    } catch (err) {
      console.error('Failed to fetch:', err)
      setDetailError(err instanceof Error ? err.message : 'Failed to copy config')
    }
  }

  async function handlePullRepo(repoId: string) {
    setPulling(p => ({ ...p, [repoId]: true }))
    setPullMsg(m => ({ ...m, [repoId]: '' }))
    try {
      const result = await post<{ success?: boolean; message?: string }>(
        `/admin/repositories/pull?id=${repoId}`, {}
      )
      setPullMsg(m => ({ ...m, [repoId]: result?.success ? 'Pulled' : (result?.message ?? 'Pull failed') }))
    } catch (e: unknown) {
      setPullMsg(m => ({ ...m, [repoId]: e instanceof Error ? e.message : 'Pull failed' }))
    } finally {
      setPulling(p => ({ ...p, [repoId]: false }))
    }
  }

  async function handleReindexAll() {
    setReindexing(true)
    setReindexMsg('')
    try {
      await post(`/admin/products/reindex?product_id=${product.id}`, {})
      setReindexMsg('Re-indexing triggered for all repositories.')
    } catch (e: unknown) {
      setReindexMsg(e instanceof Error ? e.message : 'Failed to trigger re-index')
    } finally {
      setReindexing(false)
    }
  }

  function repoName(id: string) {
    return repos.find(r => r.id === id)?.name ?? id
  }

  return (
    <div className="border-t border-rule bg-surface-900 px-6 pb-6 pt-4">
      {detailError && (
        <div className="mb-4 rounded-sm border border-red-700/50 bg-red-900/20 px-4 py-3 text-sm text-red-300">
          {detailError}
        </div>
      )}
      {/* Action buttons */}
      <div className="mb-5 flex items-center gap-3">
        <button
          onClick={() => onStartOnboarding(product.id)}
          className="rounded bg-accent px-3 py-1.5 text-sm font-medium text-surface-900 hover:bg-accent-hover"
        >
          Start Onboarding
        </button>
        <button
          onClick={handleReindexAll}
          disabled={reindexing}
          className="rounded-sm border border-rule px-3 py-1.5 text-sm text-muted hover:bg-surface-700 disabled:opacity-50"
        >
          {reindexing ? 'Re-indexing…' : 'Re-index All Repos'}
        </button>
        {reindexMsg && (
          <span className="text-xs text-emerald-400">{reindexMsg}</span>
        )}
        <AutoIndexToggle productId={product.id} />
      </div>

      {/* Verification policy (D2) */}
      <div className="mb-6">
        <h4 className="mb-2 text-sm font-semibold text-fg">Verification policy</h4>
        <PartialVerificationToggle
          productId={product.id}
          initialValue={product.partial_verification_opt_in === true}
          onChanged={(optIn) => onPolicyChanged?.(product.id, optIn)}
        />
        <ContractsStrictToggle productId={product.id} initialValue={product.contracts_strict === true} />
      </div>

      {/* Repositories */}
      <div className="mb-6">
        <div className="mb-3 flex items-center justify-between">
          <h4 className="text-sm font-semibold text-fg">Repositories</h4>
          <button
            onClick={() => setShowAddRepo(true)}
            className="rounded bg-accent px-3 py-1 text-xs font-medium text-surface-900 hover:bg-accent-hover"
          >
            + Add Repository
          </button>
        </div>

        {loadingRepos ? (
          <div className="flex h-12 items-center justify-center">
            <div className="h-5 w-5 animate-spin rounded-full border-2 border-accent border-t-transparent" />
          </div>
        ) : repos.length === 0 ? (
          <p className="text-sm text-dim">No repositories yet.</p>
        ) : (
          <div className="overflow-x-auto rounded-sm border border-rule">
            <table className="w-full text-sm">
              <thead>
                <tr className="border-b border-rule bg-surface-950 text-left">
                  <th className="px-3 py-2.5 text-xs font-medium text-muted">Name</th>
                  <th className="px-3 py-2.5 text-xs font-medium text-muted">GitHub URL</th>
                  <th className="px-3 py-2.5 text-xs font-medium text-muted">Base Branch</th>
                  <th className="px-3 py-2.5 text-xs font-medium text-muted">Merge Strategy</th>
                  <th className="px-3 py-2.5 text-xs font-medium text-muted">Last Indexed</th>
                  <th className="px-3 py-2.5 text-xs font-medium text-muted">Status</th>
                  <th className="px-3 py-2.5 text-xs font-medium text-muted">Actions</th>
                </tr>
              </thead>
              <tbody>
                {repos.map(repo => (
                  editingRepo === repo.id ? (
                    <tr key={repo.id} className="border-b border-rule bg-indigo-900/10">
                      <td className="px-3 py-2 text-fg">{repo.name}</td>
                      <td className="px-3 py-2 text-muted text-xs">{repo.github_url}</td>
                      <td className="px-3 py-2">
                        <select
                          value={editFields.base_branch ?? repo.base_branch}
                          onChange={e => setEditFields(f => ({ ...f, base_branch: e.target.value }))}
                          className="rounded-sm border border-rule bg-surface-950 px-2 py-1 text-xs text-fg"
                        >
                          <option value="main">main</option>
                          <option value="master">master</option>
                          <option value="develop">develop</option>
                        </select>
                      </td>
                      <td className="px-3 py-2">
                        <select
                          value={editFields.merge_strategy ?? repo.merge_strategy}
                          onChange={e => setEditFields(f => ({ ...f, merge_strategy: e.target.value as 'squash' | 'merge' | 'rebase' }))}
                          className="rounded-sm border border-rule bg-surface-950 px-2 py-1 text-xs text-fg"
                        >
                          <option value="squash">squash</option>
                          <option value="merge">merge</option>
                          <option value="rebase">rebase</option>
                        </select>
                      </td>
                      <td className="px-3 py-2">
                        <input
                          type="text"
                          value={editFields.branch_prefix ?? repo.branch_prefix}
                          onChange={e => setEditFields(f => ({ ...f, branch_prefix: e.target.value }))}
                          placeholder="ae/"
                          className="w-24 rounded-sm border border-rule bg-surface-950 px-2 py-1 text-xs text-fg"
                        />
                      </td>
                      <td className="px-3 py-2" colSpan={2}>
                        <div className="flex gap-2">
                          <button
                            onClick={() => handleSaveRepoEdit(repo.id)}
                            className="rounded bg-emerald-700 px-2 py-1 text-xs text-white hover:bg-emerald-600"
                          >
                            Save
                          </button>
                          <button
                            onClick={() => { setEditingRepo(null); setEditFields({}) }}
                            className="rounded-sm border border-rule px-2 py-1 text-xs text-muted hover:bg-surface-700"
                          >
                            Cancel
                          </button>
                        </div>
                      </td>
                    </tr>
                  ) : (
                    <tr key={repo.id} className="border-b border-rule hover:bg-surface-800/30">
                      <td className="px-3 py-2 font-medium text-fg">
                        {repo.name}
                        {repo.github_account_id && accountsMap[repo.github_account_id] && (
                          <span className="ml-2 inline-flex items-center gap-1 rounded-full bg-surface-700 px-2 py-0.5 text-xs text-muted">
                            {accountsMap[repo.github_account_id].avatar_url && (
                              <img src={accountsMap[repo.github_account_id].avatar_url} alt="" className="h-3 w-3 rounded-full" />
                            )}
                            {accountsMap[repo.github_account_id].username}
                          </span>
                        )}
                      </td>
                      <td className="px-3 py-2 max-w-[180px] truncate text-xs text-muted">
                        <a href={repo.github_url} target="_blank" rel="noreferrer" className="hover:text-accent-dim">
                          {repo.github_url}
                        </a>
                      </td>
                      <td className="px-3 py-2 text-muted">{repo.base_branch}</td>
                      <td className="px-3 py-2 text-muted">{repo.merge_strategy}</td>
                      <td className="px-3 py-2 text-xs text-muted">{formatDate(repo.last_indexed_at)}</td>
                      <td className="px-3 py-2">
                        {repo.clone_path ? (
                          <Badge color="bg-emerald-700/40 text-emerald-300" label="Cloned" />
                        ) : (
                          <Badge color="bg-surface-700/40 text-muted" label="Not cloned" />
                        )}
                      </td>
                      <td className="px-3 py-2">
                        <div className="flex items-center gap-2">
                          {/* Copy config from */}
                          <select
                            value={copyFrom[repo.id] ?? ''}
                            onChange={e => {
                              const val = e.target.value
                              setCopyFrom(c => ({ ...c, [repo.id]: val }))
                              if (val) handleCopyConfig(repo.id, val)
                            }}
                            className="rounded-sm border border-rule bg-surface-950 px-1 py-1 text-xs text-muted"
                          >
                            <option value="">Copy config from…</option>
                            {repos.filter(r => r.id !== repo.id).map(r => (
                              <option key={r.id} value={r.id}>{r.name}</option>
                            ))}
                          </select>
                          {/* Token override */}
                          <input
                            type="password"
                            placeholder="Token override"
                            value={tokenOverrides[repo.id] ?? ''}
                            onChange={e => setTokenOverrides(t => ({ ...t, [repo.id]: e.target.value }))}
                            className="w-28 rounded-sm border border-rule bg-surface-950 px-2 py-1 text-xs text-fg placeholder-dim"
                          />
                          {/* Pull Latest */}
                          <button
                            onClick={() => handlePullRepo(repo.id)}
                            disabled={pulling[repo.id]}
                            className="rounded-sm border border-rule px-2 py-1 text-xs text-muted hover:bg-surface-700 disabled:opacity-50"
                          >
                            {pulling[repo.id] ? 'Pulling…' : 'Pull Latest'}
                          </button>
                          {pullMsg[repo.id] && (
                            <span className={`text-xs ${pullMsg[repo.id] === 'Pulled' ? 'text-emerald-400' : 'text-red-400'}`}>
                              {pullMsg[repo.id]}
                            </span>
                          )}
                          <button
                            onClick={() => { setEditingRepo(repo.id); setEditFields({}) }}
                            className="rounded-sm border border-rule px-2 py-1 text-xs text-muted hover:bg-surface-700"
                          >
                            Edit
                          </button>
                          <button
                            onClick={() => setPolicyRepo(p => p === repo.id ? null : repo.id)}
                            aria-pressed={policyRepo === repo.id}
                            className={`rounded-sm border px-2 py-1 text-xs hover:bg-surface-700 ${policyRepo === repo.id ? 'border-accent text-accent' : 'border-rule text-muted'}`}
                          >
                            Verification
                          </button>
                          <button
                            onClick={() => setZonesRepo(p => p === repo.id ? null : repo.id)}
                            aria-pressed={zonesRepo === repo.id}
                            className={`rounded-sm border px-2 py-1 text-xs hover:bg-surface-700 ${zonesRepo === repo.id ? 'border-accent text-accent' : 'border-rule text-muted'}`}
                          >
                            Zones
                          </button>
                          {confirmDeleteRepo === repo.id ? (
                            <>
                              <span className="text-xs text-red-400">Remove?</span>
                              <button
                                onClick={() => handleDeleteRepo(repo.id)}
                                className="rounded bg-red-700 px-2 py-1 text-xs text-white hover:bg-red-600"
                              >
                                Yes
                              </button>
                              <button
                                onClick={() => setConfirmDeleteRepo(null)}
                                className="rounded-sm border border-rule px-2 py-1 text-xs text-muted hover:bg-surface-700"
                              >
                                No
                              </button>
                            </>
                          ) : (
                            <button
                              onClick={() => setConfirmDeleteRepo(repo.id)}
                              className="rounded-sm border border-red-800 px-2 py-1 text-xs text-red-400 hover:bg-red-900/30"
                            >
                              Remove
                            </button>
                          )}
                        </div>
                      </td>
                    </tr>
                  )
                ))}
              </tbody>
            </table>
          </div>
        )}
        {policyRepo && repos.some(r => r.id === policyRepo) && (
          <div data-testid="repo-verification-policy" className="mt-4 rounded-sm border border-rule bg-surface-950 p-4">
            <div className="mb-3 flex items-center justify-between">
              <h5 className="text-sm font-semibold text-fg">
                Verification policy — {repoName(policyRepo)}
              </h5>
              <button
                onClick={() => setPolicyRepo(null)}
                className="rounded-sm border border-rule px-2 py-1 text-xs text-muted hover:bg-surface-700"
              >
                Close
              </button>
            </div>
            <p className="mb-3 text-xs text-dim">
              Repositories without a saved policy block any run that has no executable checks
              (fail-closed default since M3-03). Partial verification additionally requires the
              product's opt-in above.
            </p>
            <RepositoryVerificationPolicyEditor key={policyRepo} repositoryId={policyRepo} mode="admin" />
          </div>
        )}
        {zonesRepo && repos.some(r => r.id === zonesRepo) && (
          <div data-testid="repo-zones" className="mt-4 rounded-sm border border-rule bg-surface-950 p-4">
            <div className="mb-3 flex items-center justify-between">
              <h5 className="text-sm font-semibold text-fg">
                Codex zones — {repoName(zonesRepo)}
              </h5>
              <button
                onClick={() => setZonesRepo(null)}
                className="rounded-sm border border-rule px-2 py-1 text-xs text-muted hover:bg-surface-700"
              >
                Close
              </button>
            </div>
            <RepositoryZonesEditor key={zonesRepo} repositoryId={zonesRepo} />
          </div>
        )}
      </div>

      {/* Dependencies */}
      <div>
        <div className="mb-3 flex items-center justify-between">
          <h4 className="text-sm font-semibold text-fg">Dependencies</h4>
          {repos.length >= 2 && (
            <button
              onClick={() => setShowAddDep(true)}
              className="rounded bg-accent px-3 py-1 text-xs font-medium text-surface-900 hover:bg-accent-hover"
            >
              + Add Dependency
            </button>
          )}
        </div>
        {deps.length === 0 ? (
          <p className="text-sm text-dim">No dependencies defined.</p>
        ) : (
          <div className="space-y-2">
            {deps.map(dep => (
              <div key={dep.id} className="flex items-center justify-between rounded-sm border border-rule bg-surface-950 px-4 py-2.5">
                <span className="text-sm text-fg">
                  <span className="font-medium text-accent">{repoName(dep.source_repo_id)}</span>
                  <span className="mx-2 text-dim">→</span>
                  <span className="font-medium text-accent">{repoName(dep.target_repo_id)}</span>
                  <span className="ml-2 text-xs text-muted">({dep.dependency_type})</span>
                  {dep.description && (
                    <span className="ml-2 text-xs text-dim">{dep.description}</span>
                  )}
                </span>
                {confirmDeleteDep === dep.id ? (
                  <div className="flex items-center gap-2">
                    <span className="text-xs text-red-400">Delete?</span>
                    <button
                      onClick={() => handleDeleteDep(dep.id)}
                      className="rounded bg-red-700 px-2 py-1 text-xs text-white hover:bg-red-600"
                    >
                      Yes
                    </button>
                    <button
                      onClick={() => setConfirmDeleteDep(null)}
                      className="rounded-sm border border-rule px-2 py-1 text-xs text-muted hover:bg-surface-700"
                    >
                      No
                    </button>
                  </div>
                ) : (
                  <button
                    onClick={() => setConfirmDeleteDep(dep.id)}
                    className="rounded-sm border border-red-800 px-2 py-1 text-xs text-red-400 hover:bg-red-900/30"
                  >
                    Delete
                  </button>
                )}
              </div>
            ))}
          </div>
        )}
      </div>

      {/* Modals */}
      {showAddRepo && (
        <AddRepoModal
          productId={product.id}
          onClose={() => setShowAddRepo(false)}
          onAdded={r => { setRepos(prev => [...prev, r]) }}
        />
      )}
      {showAddDep && (
        <AddDependencyModal
          repos={repos}
          onClose={() => setShowAddDep(false)}
          onAdded={d => { setDeps(prev => [...prev, d]); setShowAddDep(false) }}
        />
      )}
    </div>
  )
}

// ─── Main component ───────────────────────────────────────────────────────────

export default function ProductManagement() {
  const { get, del } = useApi()
  const navigate = useNavigate()
  const [products, setProducts] = useState<Product[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [showCreateProduct, setShowCreateProduct] = useState(false)
  const [expandedId, setExpandedId] = useState<string | null>(null)
  const [deletingId, setDeletingId] = useState<string | null>(null)
  // Map of product_id → codex stats. Used to disable the Onboard button and
  // show an "Onboarded" badge for products that already have codex content.
  const [codexStats, setCodexStats] = useState<Record<string, { modules: number; patterns: number }>>({})

  const fetchProducts = useCallback(async () => {
    setLoading(true)
    setError(null)
    try {
      const data = await get<Product[]>('/admin/products')
      setProducts(data)

      // Fetch codex stats for each product in parallel. Failures are logged
      // but non-fatal — the Onboard button stays enabled if we can't tell.
      const results = await Promise.allSettled(
        data.map(async (p) => {
          const stats = await get<{ modules: number; patterns: number }>(
            `/admin/products/codex-stats?product_id=${p.id}`
          )
          return [p.id, stats] as const
        }),
      )
      const next: Record<string, { modules: number; patterns: number }> = {}
      for (const r of results) {
        if (r.status === 'fulfilled') {
          const [id, stats] = r.value
          next[id] = stats
        }
      }
      setCodexStats(next)
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Failed to load products')
    } finally {
      setLoading(false)
    }
  }, [get])

  useEffect(() => { fetchProducts() }, [fetchProducts])

  const isOnboarded = useCallback((productID: string): boolean => {
    const s = codexStats[productID]
    return !!s && (s.modules > 0 || s.patterns > 0)
  }, [codexStats])

  function toggleExpand(id: string) {
    setExpandedId(prev => prev === id ? null : id)
  }

  function handleStartOnboarding(productId: string) {
    navigate(`/onboarding?product_id=${productId}`)
  }

  function handlePolicyChanged(productId: string, optIn: boolean) {
    setProducts(prev => prev.map(p => (p.id === productId ? { ...p, partial_verification_opt_in: optIn } : p)))
  }

  async function handleDeleteProduct(productId: string, productName: string) {
    if (!window.confirm(`Delete "${productName}"? This will remove the product and all associated data. This cannot be undone.`)) return
    setDeletingId(productId)
    try {
      await del('/admin/products/delete?id=' + productId)
      setProducts(prev => prev.filter(p => p.id !== productId))
      if (expandedId === productId) setExpandedId(null)
    } catch (e: unknown) {
      alert(e instanceof Error ? e.message : 'Failed to delete product')
    } finally {
      setDeletingId(null)
    }
  }

  if (loading) {
    return (
      <div className="flex h-64 items-center justify-center">
        <div className="h-8 w-8 animate-spin rounded-full border-2 border-accent border-t-transparent" />
      </div>
    )
  }

  return (
    <div className="space-y-6">
      {/* Header */}
      <div className="flex items-center justify-between">
        <div>
          <h2 className="text-lg font-semibold text-fg">Products</h2>
          <p className="mt-0.5 text-sm text-dim">
            Manage products, repositories, and cross-repo dependencies.
          </p>
        </div>
        <button
          onClick={() => setShowCreateProduct(true)}
          className="rounded bg-accent px-4 py-2 text-sm font-medium text-surface-900 hover:bg-accent-hover"
        >
          + Create Product
        </button>
      </div>

      {error && (
        <div className="rounded-sm border border-red-700/50 bg-red-900/20 px-4 py-3 text-sm text-red-300">
          {error}
        </div>
      )}

      {/* Products table */}
      {products.length === 0 ? (
        <div className="rounded-sm border border-rule bg-surface-900 py-16 text-center">
          <p className="text-muted">No products yet. Create your first product to get started.</p>
        </div>
      ) : (
        <div className="rounded-sm border border-rule bg-surface-900 overflow-hidden">
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b border-rule bg-surface-950 text-left">
                <th className="px-4 py-3 text-xs font-medium text-muted">Product Name</th>
                <th className="px-4 py-3 text-xs font-medium text-muted">Description</th>
                <th className="px-4 py-3 text-xs font-medium text-muted">Created</th>
                <th className="px-4 py-3 text-xs font-medium text-muted">Actions</th>
              </tr>
            </thead>
            <tbody>
              {products.map(product => (
                <>
                  <tr
                    key={product.id}
                    className="border-b border-rule hover:bg-surface-800/30 cursor-pointer"
                    onClick={() => toggleExpand(product.id)}
                  >
                    <td className="px-4 py-3 font-medium text-fg">
                      <div className="flex items-center gap-2">
                        <span
                          className={[
                            'transition-transform text-dim text-xs',
                            expandedId === product.id ? 'rotate-90' : '',
                          ].join(' ')}
                        >
                          ▶
                        </span>
                        {product.name}
                      </div>
                    </td>
                    <td className="px-4 py-3 text-muted max-w-xs truncate">
                      {product.description || <span className="text-dim">—</span>}
                    </td>
                    <td className="px-4 py-3 text-xs text-dim">{formatDate(product.created_at)}</td>
                    <td className="px-4 py-3" onClick={e => e.stopPropagation()}>
                      <div className="flex items-center gap-2">
                        {isOnboarded(product.id) ? (
                          <>
                            <span className="inline-flex items-center gap-1 rounded bg-emerald-900/30 px-2 py-0.5 text-xs text-emerald-300">
                              <span className="h-1.5 w-1.5 rounded-full bg-emerald-400" />
                              Onboarded
                            </span>
                            <button
                              onClick={() => handleStartOnboarding(product.id)}
                              className="rounded-sm border border-rule px-2 py-1 text-xs text-dim hover:border-sharp hover:text-muted"
                              title="Re-run the onboarding wizard (will re-index)"
                            >
                              Re-onboard
                            </button>
                          </>
                        ) : (
                          <button
                            onClick={() => handleStartOnboarding(product.id)}
                            className="rounded-sm border border-indigo-700 px-3 py-1 text-xs text-accent-dim hover:bg-indigo-900/30"
                          >
                            Onboard
                          </button>
                        )}
                        <button
                          onClick={() => handleDeleteProduct(product.id, product.name)}
                          disabled={deletingId === product.id}
                          className="rounded-sm border border-rule px-2 py-1 text-xs text-red-400 hover:bg-red-900/30 hover:border-red-700 disabled:opacity-50"
                          title="Delete product"
                        >
                          {deletingId === product.id ? 'Deleting...' : 'Delete'}
                        </button>
                      </div>
                    </td>
                  </tr>
                  {expandedId === product.id && (
                    <tr key={`${product.id}-detail`}>
                      <td colSpan={4} className="p-0">
                        <ProductDetail
                          product={product}
                          onStartOnboarding={handleStartOnboarding}
                          onPolicyChanged={handlePolicyChanged}
                        />
                      </td>
                    </tr>
                  )}
                </>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {/* Create Product modal */}
      {showCreateProduct && (
        <CreateProductModal
          onClose={() => setShowCreateProduct(false)}
          onCreated={p => {
            setProducts(prev => [...prev, p])
            setShowCreateProduct(false)
            setExpandedId(p.id)
          }}
        />
      )}
    </div>
  )
}
