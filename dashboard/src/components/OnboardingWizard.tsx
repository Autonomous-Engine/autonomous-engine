import { useState, useEffect, useCallback, useRef } from 'react'
import { useSearchParams } from 'react-router-dom'
import { useApi } from '@/hooks/useApi'
import { useWebSocket } from '@/hooks/useWebSocket'
import type { Product, Repository, RepoDependency, CredentialEntry, GraphResponse, RiskLevel, CodexModule, CodexPattern, CodexSection, GitHubAccount } from '@/types'
import { CandidateCard } from '@/components/onboarding/CandidateCard'
import type { Candidate, Evidence } from '@/types/onboarding'
import VerificationPolicyStep from '@/components/onboarding/VerificationPolicyStep'
import ZonesStep from '@/components/onboarding/ZonesStep'

// ─── WS alert payload shapes (dashboard-backend bridge, T19) ──────────────────
// All three arrive on the "alerts" WS channel.
interface OnboardingPrefillProgress {
  kind: 'onboarding_prefill_progress'
  product_id: string
  phase: string
  files_processed: number
  total_files: number
}
interface OnboardingPrefillCompleted {
  kind: 'onboarding_prefill_completed'
  product_id: string
  modules_found: number
  patterns_found: number
  drifts: Array<{ question_key: string; previous: string; current: string }>
}
interface OnboardingDriftDetected {
  kind: 'onboarding_drift_detected'
  product_id: string
  drifts: Array<{ question_key: string; previous: string; current: string }>
}


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

// ─── Shared helpers ───────────────────────────────────────────────────────────

function inputClass(extra = '') {
  return `w-full rounded-sm border border-rule bg-surface-950 px-3 py-2 text-sm text-fg focus:outline-none focus:ring-1 focus:ring-accent ${extra}`
}

function revealBtn(show: boolean, toggle: () => void) {
  return (
    <button
      type="button"
      onClick={toggle}
      className="rounded-sm border border-rule px-2 py-2 text-muted hover:text-fg"
      title={show ? 'Hide' : 'Show'}
    >
      {show ? (
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
  )
}

// Ordered list of conversation questions and the human-facing prompt for each.
// The backend pre-fill produces at most one Candidate per question_key below.
const QUESTION_TEXT: Record<string, string> = {
  architecture_overview: 'Describe your high-level system architecture.',
  tech_stack: 'What is your primary technology stack?',
  patterns_discovered: 'What coding patterns does your team consistently follow?',
  anti_patterns: 'What patterns does your team explicitly avoid?',
  branching_strategy: 'Describe your Git branching strategy.',
  pr_process: 'What is your pull-request process?',
  deployment_process: 'How does your deployment pipeline work?',
  domain_knowledge: 'What are the core domain concepts?',
  critical_constraints: 'Any regulatory or compliance constraints?',
  testing_philosophy: 'What is your testing philosophy?',
  test_conventions: 'Test-suite conventions?',
  autonomous_scope: 'Which changes can the autonomous system make unsupervised?',
  review_triggers: 'Which conditions always trigger human review?',
  risk_zones: 'Which parts of the codebase are highest risk?',
}
const CONVERSATION_ORDER = Object.keys(QUESTION_TEXT)

// ─── Step 0: Select / Create Product ─────────────────────────────────────────

interface SelectProductStepProps {
  initialProductId: string | null
  onProductSelected: (product: Product) => void
}

function SelectProductStep({ initialProductId, onProductSelected }: SelectProductStepProps) {
  const { get, post } = useApi()
  const [products, setProducts] = useState<Product[]>([])
  const [loading, setLoading] = useState(true)
  const [selectedId, setSelectedId] = useState(initialProductId ?? '')
  const [showCreate, setShowCreate] = useState(false)
  const [newName, setNewName] = useState('')
  const [newDesc, setNewDesc] = useState('')
  const [creating, setCreating] = useState(false)
  const [error, setError] = useState('')

  useEffect(() => {
    get<Product[]>('/admin/products')
      .then(data => {
        setProducts(data)
        if (initialProductId) {
          const found = data.find(p => p.id === initialProductId)
          if (found) { onProductSelected(found); return }
        }
        if (data.length === 1) setSelectedId(data[0].id)
      })
      .catch((err) => {
        console.error('Failed to fetch:', err)
        setError(err instanceof Error ? err.message : 'Failed to load products')
      })
      .finally(() => setLoading(false))
  }, [get, initialProductId, onProductSelected])

  async function handleCreateProduct() {
    if (!newName.trim()) { setError('Name is required.'); return }
    setCreating(true)
    setError('')
    try {
      const product = await post<Product>('/admin/products/create', {
        name: newName.trim(),
        description: newDesc.trim(),
      })
      if (product) {
        setProducts(prev => [...prev, product])
        onProductSelected(product)
      }
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Failed to create product')
    } finally {
      setCreating(false)
    }
  }

  function handleContinue() {
    const product = products.find(p => p.id === selectedId)
    if (product) onProductSelected(product)
  }

  if (loading) {
    return (
      <div className="flex h-32 items-center justify-center">
        <div className="h-6 w-6 animate-spin rounded-full border-2 border-accent border-t-transparent" />
      </div>
    )
  }

  return (
    <div className="space-y-6">
      <div>
        <h2 className="text-base font-semibold text-fg">Select or Create Product</h2>
        <p className="mt-1 text-sm text-muted">
          Choose an existing product or create a new one to begin setup.
        </p>
      </div>

      {products.length > 0 && !showCreate && (
        <div className="space-y-3">
          <div>
            <label className="mb-1 block text-xs text-muted">Existing Products</label>
            <select
              value={selectedId}
              onChange={e => setSelectedId(e.target.value)}
              className={inputClass()}
            >
              <option value="">— Select a product —</option>
              {products.map(p => (
                <option key={p.id} value={p.id}>{p.name}</option>
              ))}
            </select>
          </div>
          <div className="flex items-center gap-3">
            <button
              onClick={handleContinue}
              disabled={!selectedId}
              className="rounded bg-accent px-4 py-2 text-sm font-medium text-surface-900 hover:bg-accent-hover disabled:opacity-50"
            >
              Continue with Selected
            </button>
            <span className="text-sm text-dim">or</span>
            <button
              onClick={() => setShowCreate(true)}
              className="rounded-sm border border-rule px-4 py-2 text-sm text-muted hover:bg-surface-800"
            >
              Create New Product
            </button>
          </div>
        </div>
      )}

      {(showCreate || products.length === 0) && (
        <div className="rounded-sm border border-rule bg-surface-900 p-5 space-y-4">
          <h3 className="text-sm font-semibold text-fg">New Product</h3>
          <div>
            <label className="mb-1 block text-xs text-muted">Product Name</label>
            <input
              type="text"
              value={newName}
              onChange={e => setNewName(e.target.value)}
              placeholder="My Product"
              className={inputClass()}
              autoFocus
            />
          </div>
          <div>
            <label className="mb-1 block text-xs text-muted">Description</label>
            <textarea
              value={newDesc}
              onChange={e => setNewDesc(e.target.value)}
              placeholder="Brief description…"
              rows={2}
              className={inputClass('resize-none')}
            />
          </div>
          {error && <p className="text-xs text-red-400">{error}</p>}
          <div className="flex items-center gap-3">
            <button
              onClick={handleCreateProduct}
              disabled={creating}
              className="rounded bg-accent px-4 py-2 text-sm font-medium text-surface-900 hover:bg-accent-hover disabled:opacity-50"
            >
              {creating ? 'Creating…' : 'Create Product'}
            </button>
            {products.length > 0 && (
              <button
                onClick={() => setShowCreate(false)}
                className="rounded-sm border border-rule px-4 py-2 text-sm text-muted hover:bg-surface-800"
              >
                Cancel
              </button>
            )}
          </div>
        </div>
      )}
    </div>
  )
}

// ─── Jira discovery types ─────────────────────────────────────────────────────

interface JiraDiscoveryResult {
  project: {
    key: string
    name: string
    issue_types: Array<{ id: string; name: string }>
    statuses: Array<{ id: string; name: string; category: string }>
    priorities: Array<{ id: string; name: string }>
  }
  proposed_mapping: {
    status_mapping: Record<string, string>
    issue_type_map: Record<string, string>
    priority_map: Record<string, string>
    ingest_jql: string
    ingest_label: string
  }
}

// ─── Step 1: Configure Credentials ───────────────────────────────────────────

type LLMProvider = 'openai' | 'anthropic' | 'ollama' | 'vllm' | 'custom'

interface LLMForm {
  provider: LLMProvider
  endpoint: string
  api_key: string
}

interface JiraForm {
  url: string
  email: string
  api_token: string
  project_key: string
}

type TestResult = 'idle' | 'ok' | 'error'

interface CredentialsStepProps {
  onNext: () => void
}

function CredentialsStep({ onNext }: CredentialsStepProps) {
  const { get, post } = useApi()

  const [llm, setLlm] = useState<LLMForm>({
    provider: 'ollama',
    endpoint: 'http://host.docker.internal:11434',
    api_key: '',
  })
  const [jira, setJira] = useState<JiraForm>({ url: '', email: '', api_token: '', project_key: '' })

  // GitHub multi-account state
  const [accounts, setAccounts] = useState<GitHubAccount[]>([])
  const [newGhToken, setNewGhToken] = useState('')
  const [showGhToken, setShowGhToken] = useState(false)
  const [ghAdding, setGhAdding] = useState(false)
  const [ghAddError, setGhAddError] = useState('')

  const [showLlmKey, setShowLlmKey] = useState(false)
  const [showJiraToken, setShowJiraToken] = useState(false)

  const [llmTest, setLlmTest] = useState<{ result: TestResult; msg: string }>({ result: 'idle', msg: '' })
  const [jiraTestResult, setJiraTestResult] = useState<string | null>(null)

  // Jira auto-discovery
  const [discovering, setDiscovering] = useState(false)
  const [discoveryError, setDiscoveryError] = useState('')
  const [discoveryResult, setDiscoveryResult] = useState<JiraDiscoveryResult | null>(null)
  const [statusMapping, setStatusMapping] = useState<Record<string, string>>({})
  const [issueTypeMap, setIssueTypeMap] = useState<Record<string, string>>({})
  const [priorityMap, setPriorityMap] = useState<Record<string, string>>({})
  const [ingestJQL, setIngestJQL] = useState('')
  const [confirmingSave, setConfirmingSave] = useState(false)
  const [mappingSaved, setMappingSaved] = useState(false)

  const [saving, setSaving] = useState(false)
  const [error, setError] = useState('')

  // Load existing credentials on mount and auto-verify
  useEffect(() => {
    async function loadExisting() {
      try {
        // Load LLM credentials
        const llmCreds = await get<CredentialEntry[]>('/admin/credentials?category=llm')
        if (llmCreds.length > 0) {
          const findVal = (key: string) => {
            const entry = llmCreds.find(c => c.key_name === key)
            return entry ? (entry.is_secret ? '••••••••' : entry.value) : ''
          }
          const provider = findVal('provider')
          const endpoint = findVal('endpoint')

          setLlm({
            provider: provider && provider !== '••••••••' ? provider as LLMProvider : 'ollama',
            endpoint: endpoint && endpoint !== '••••••••' ? endpoint : 'http://host.docker.internal:11434',
            api_key: '',
          })

          // LLM credentials exist — auto-test
          try {
            await post('/admin/credentials/test', { category: 'llm' })
            setLlmTest({ result: 'ok', msg: 'Already configured — connection verified' })
          } catch {
            setLlmTest({ result: 'error', msg: 'Credentials exist but connection failed' })
          }
        }

        // Load Jira credentials (optional)
        const jiraCreds = await get<CredentialEntry[]>('/admin/credentials?category=jira')
        if (jiraCreds.length > 0) {
          const findVal = (key: string) => {
            const entry = jiraCreds.find(c => c.key_name === key)
            return entry ? (entry.is_secret ? '' : entry.value) : ''
          }
          setJira({
            url: findVal('url'),
            email: findVal('email'),
            api_token: '',
            project_key: findVal('project_key'),
          })
        }
      } catch (err) {
        console.error('Failed to load existing credentials:', err)
      }
    }
    loadExisting()
  }, []) // eslint-disable-line react-hooks/exhaustive-deps

  // Fetch GitHub accounts on mount
  useEffect(() => {
    get<GitHubAccount[]>('/admin/github/accounts').then(a => { if (a) setAccounts(a) }).catch(() => {})
  }, [get])

  async function addGhAccount() {
    if (!newGhToken.trim()) { setGhAddError('Token is required'); return }
    setGhAdding(true); setGhAddError('')
    try {
      const acct = await post<GitHubAccount>('/admin/github/accounts/create', { token: newGhToken.trim() })
      if (acct) { setAccounts(prev => [...prev, acct]); setNewGhToken('') }
    } catch (e: unknown) {
      setGhAddError(e instanceof Error ? e.message : 'Failed to add account')
    } finally { setGhAdding(false) }
  }

  const llmOk = llmTest.result === 'ok'
  const ghOk = accounts.length > 0
  const canContinue = llmOk && ghOk

  async function testLLM() {
    setLlmTest({ result: 'idle', msg: '' })
    try {
      await post('/admin/credentials/test', { category: 'llm' })
      setLlmTest({ result: 'ok', msg: 'Connection successful' })
    } catch (e: unknown) {
      setLlmTest({ result: 'error', msg: e instanceof Error ? e.message : 'Connection failed' })
    }
  }

  async function testJira() {
    try {
      await post('/admin/credentials/test', { category: 'jira' })
      setJiraTestResult('Connection successful')
    } catch (e: unknown) {
      setJiraTestResult(e instanceof Error ? e.message : 'Connection failed')
    }
  }

  async function handleAutoDiscover() {
    if (!jira.project_key) { setDiscoveryError('Enter a project key before discovering.'); return }
    setDiscovering(true)
    setDiscoveryError('')
    setDiscoveryResult(null)
    setMappingSaved(false)
    try {
      const result = await post<JiraDiscoveryResult>('/onboarding/discover-jira', {
        project_key: jira.project_key,
        jira_url: jira.url,
        jira_email: jira.email,
        jira_token: jira.api_token,
      })
      if (result) {
        setDiscoveryResult(result)
        setStatusMapping(result.proposed_mapping.status_mapping ?? {})
        setIssueTypeMap(result.proposed_mapping.issue_type_map ?? {})
        setPriorityMap(result.proposed_mapping.priority_map ?? {})
        setIngestJQL(result.proposed_mapping.ingest_jql ?? '')
      }
    } catch (e: unknown) {
      setDiscoveryError(e instanceof Error ? e.message : 'Discovery failed')
    } finally {
      setDiscovering(false)
    }
  }

  async function handleConfirmMapping() {
    if (!discoveryResult) return
    setConfirmingSave(true)
    try {
      await post('/onboarding/save-jira-mapping', {
        status_mapping: statusMapping,
        issue_type_map: issueTypeMap,
        priority_map: priorityMap,
        ingest_jql: ingestJQL,
        ingest_label: discoveryResult.proposed_mapping.ingest_label,
      })
      setMappingSaved(true)
    } catch (e: unknown) {
      setDiscoveryError(e instanceof Error ? e.message : 'Failed to save mapping')
    } finally {
      setConfirmingSave(false)
    }
  }

  async function handleSaveAndContinue() {
    setSaving(true)
    setError('')
    try {
      const PLACEHOLDER = '••••••••'
      // All credentials are stored at scope='global' / scope_target='default' —
      // the only place ResolveCredential's chain terminates. category
      // disambiguates llm vs github vs jira; scope_target does not.
      const credentialPayloads = [
        { scope: 'global', scope_target: 'default', key_name: 'provider', value: llm.provider, category: 'llm', is_secret: false, description: 'LLM provider' },
        { scope: 'global', scope_target: 'default', key_name: 'endpoint', value: llm.endpoint, category: 'llm', is_secret: false, description: 'LLM endpoint' },
        // Skip api_key if it's the placeholder (already stored in registry)
        ...(llm.api_key && llm.api_key !== PLACEHOLDER
          ? [{ scope: 'global', scope_target: 'default', key_name: 'api_key', value: llm.api_key, category: 'llm', is_secret: true, description: 'LLM API key' }]
          : []),
        // GitHub accounts are managed via /admin/github/accounts — no legacy token payload needed.
        { scope: 'global', scope_target: 'default', key_name: 'url', value: jira.url, category: 'jira', is_secret: false, description: 'Jira URL' },
        { scope: 'global', scope_target: 'default', key_name: 'email', value: jira.email, category: 'jira', is_secret: false, description: 'Jira email' },
        // Skip api_token if it's the placeholder (already stored in registry)
        ...(jira.api_token && jira.api_token !== PLACEHOLDER
          ? [{ scope: 'global', scope_target: 'default', key_name: 'api_token', value: jira.api_token, category: 'jira', is_secret: true, description: 'Jira API token' }]
          : []),
        { scope: 'global', scope_target: 'default', key_name: 'project_key', value: jira.project_key, category: 'jira', is_secret: false, description: 'Jira project key' },
      ]
      await Promise.all(credentialPayloads.filter(p => p.value).map(payload =>
        post('/admin/credentials/create', payload)
      ))
      onNext()
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Failed to save credentials')
    } finally {
      setSaving(false)
    }
  }

  function statusBadge(r: TestResult) {
    if (r === 'ok') return <span className="text-xs text-emerald-400">Passed</span>
    if (r === 'error') return <span className="text-xs text-red-400">Failed</span>
    return <span className="text-xs text-dim">Not tested</span>
  }

  return (
    <div className="space-y-6">
      <div>
        <h2 className="text-base font-semibold text-fg">Configure Credentials</h2>
        <p className="mt-1 text-sm text-muted">
          LLM and GitHub connections are required. Jira is optional.
        </p>
      </div>

      {/* Blockers */}
      {(!llmOk || !ghOk) && (
        <div className="rounded-sm border border-red-700/50 bg-red-900/10 px-4 py-3 space-y-1">
          <p className="text-xs font-semibold text-red-400">Blockers — must resolve to continue:</p>
          {!llmOk && <p className="text-xs text-red-300">• LLM connection not verified</p>}
          {!ghOk && <p className="text-xs text-red-300">• No GitHub account connected</p>}
        </div>
      )}

      {/* LLM Setup */}
      <div className="rounded-sm border border-rule bg-surface-900 p-5">
        <div className="mb-4 flex items-center justify-between">
          <h3 className="text-sm font-semibold text-fg">LLM Setup</h3>
          {statusBadge(llmTest.result)}
        </div>
        <div className="grid grid-cols-2 gap-3">
          <div>
            <label className="mb-1 block text-xs text-muted">Provider</label>
            <select
              value={llm.provider}
              onChange={e => {
                const p = e.target.value as LLMProvider
                setLlm(l => ({
                  ...l,
                  provider: p,
                  // Auto-fill endpoint when switching to Ollama
                  endpoint: p === 'ollama' ? 'http://host.docker.internal:11434' : l.endpoint,
                }))
              }}
              className={inputClass()}
            >
              <option value="ollama">Ollama (local)</option>
              <option value="openai">OpenAI</option>
              <option value="anthropic">Anthropic</option>
              <option value="vllm">vLLM</option>
              <option value="custom">Custom</option>
            </select>
          </div>
          <div className="col-span-2">
            <label className="mb-1 block text-xs text-muted">Endpoint</label>
            <input
              type="text"
              value={llm.endpoint}
              onChange={e => setLlm(l => ({ ...l, endpoint: e.target.value }))}
              placeholder="http://host.docker.internal:11434"
              className={inputClass()}
            />
          </div>
          <div className="col-span-2">
            <label className="mb-1 block text-xs text-muted">API Key</label>
            <div className="flex gap-2">
              <input
                type={showLlmKey ? 'text' : 'password'}
                value={llm.api_key}
                onChange={e => setLlm(l => ({ ...l, api_key: e.target.value }))}
                placeholder="sk-..."
                className={inputClass()}
              />
              {revealBtn(showLlmKey, () => setShowLlmKey(v => !v))}
            </div>
          </div>
        </div>
        {llmTest.msg && (
          <p className={`mt-2 text-xs ${llmTest.result === 'ok' ? 'text-emerald-400' : 'text-red-400'}`}>
            {llmTest.msg}
          </p>
        )}
        <button
          onClick={testLLM}
          className="mt-3 rounded-sm border border-rule px-3 py-1.5 text-sm text-muted hover:bg-surface-700"
        >
          Test Connection
        </button>
      </div>

      {/* GitHub Accounts */}
      <div className="rounded-sm border border-rule bg-surface-900 p-5">
        <div className="mb-4 flex items-center justify-between">
          <h3 className="text-sm font-semibold text-fg">GitHub Accounts</h3>
          {ghOk ? <span className="text-xs text-emerald-400">Passed</span> : <span className="text-xs text-dim">Not connected</span>}
        </div>
        <div className="space-y-3">
          {accounts.map(a => (
            <div key={a.id} className="flex items-center gap-3 rounded-sm border border-rule bg-surface-950 px-3 py-2">
              {a.avatar_url && <img src={a.avatar_url} alt="" className="h-6 w-6 rounded-full" />}
              <span className="text-sm text-fg">{a.username}</span>
              <span className="rounded-full bg-emerald-900/60 px-2 py-0.5 text-xs text-emerald-300">Connected</span>
            </div>
          ))}
          <div className="flex gap-2">
            <input
              type={showGhToken ? 'text' : 'password'}
              value={newGhToken}
              onChange={e => setNewGhToken(e.target.value)}
              placeholder="ghp_... (Personal Access Token)"
              className="w-full rounded-sm border border-rule bg-surface-950 px-3 py-2 text-sm text-fg focus:outline-none focus:ring-1 focus:ring-accent"
            />
            {revealBtn(showGhToken, () => setShowGhToken(v => !v))}
            <button onClick={addGhAccount} disabled={ghAdding}
              className="rounded bg-accent px-3 py-1.5 text-sm font-medium text-surface-900 hover:bg-accent-hover disabled:opacity-50">
              {ghAdding ? 'Adding...' : 'Add'}
            </button>
          </div>
          {ghAddError && <p className="text-xs text-red-400">{ghAddError}</p>}
        </div>
      </div>

      {/* Jira Setup — optional */}
      <div className="rounded-sm border border-rule/50 bg-surface-900/60 p-5 opacity-90">
        <div className="mb-1 flex items-center justify-between">
          <h3 className="text-sm font-semibold text-muted">Jira Setup</h3>
          <span className="rounded-full bg-surface-700/50 px-2 py-0.5 text-xs text-muted">Optional</span>
        </div>
        <p className="mb-4 text-xs text-dim">
          Jira integration syncs work items to your Jira instance for stakeholder visibility.
          The system works fully without it.{' '}
          <span className="text-accent-dim cursor-pointer hover:text-accent">Configure later from Admin → Integrations.</span>
        </p>
        <div className="grid grid-cols-2 gap-3">
          <div className="col-span-2">
            <label className="mb-1 block text-xs text-muted">Jira URL</label>
            <input
              type="text"
              value={jira.url}
              onChange={e => setJira(j => ({ ...j, url: e.target.value }))}
              placeholder="https://yourorg.atlassian.net"
              className={inputClass()}
            />
          </div>
          <div>
            <label className="mb-1 block text-xs text-muted">Email</label>
            <input
              type="text"
              value={jira.email}
              onChange={e => setJira(j => ({ ...j, email: e.target.value }))}
              placeholder="user@example.com"
              className={inputClass()}
            />
          </div>
          <div>
            <label className="mb-1 block text-xs text-muted">Project Key</label>
            <input
              type="text"
              value={jira.project_key}
              onChange={e => setJira(j => ({ ...j, project_key: e.target.value }))}
              placeholder="PROJ"
              className={inputClass()}
            />
          </div>
          <div className="col-span-2">
            <label className="mb-1 block text-xs text-muted">API Token</label>
            <div className="flex gap-2">
              <input
                type={showJiraToken ? 'text' : 'password'}
                value={jira.api_token}
                onChange={e => setJira(j => ({ ...j, api_token: e.target.value }))}
                placeholder="API token"
                className={inputClass()}
              />
              {revealBtn(showJiraToken, () => setShowJiraToken(v => !v))}
            </div>
          </div>
        </div>
        {jiraTestResult && (
          <p className={`mt-2 text-xs ${jiraTestResult.includes('successful') ? 'text-emerald-400' : 'text-red-400'}`}>
            {jiraTestResult}
          </p>
        )}
        <div className="mt-3 flex gap-2">
          <button
            onClick={testJira}
            className="rounded-sm border border-rule px-3 py-1.5 text-sm text-muted hover:bg-surface-700"
          >
            Test Connection
          </button>
          <button
            onClick={handleAutoDiscover}
            disabled={discovering}
            className="flex items-center gap-2 rounded bg-indigo-700 px-3 py-1.5 text-sm font-medium text-white hover:bg-accent disabled:opacity-50"
          >
            {discovering && (
              <svg className="h-4 w-4 animate-spin" fill="none" viewBox="0 0 24 24">
                <circle className="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" strokeWidth="4" />
                <path className="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8v8H4z" />
              </svg>
            )}
            {discovering ? 'Discovering...' : 'Auto-Discover'}
          </button>
        </div>

        {discoveryError && <p className="mt-2 text-xs text-red-400">{discoveryError}</p>}

        {discoveryResult && (
          <div className="mt-5 space-y-4 border-t border-rule pt-4">
            <div>
              <p className="text-xs font-semibold text-muted">
                Discovered: {discoveryResult.project.name} ({discoveryResult.project.key})
              </p>
              <p className="mt-0.5 text-xs text-dim">
                {discoveryResult.project.statuses.length} statuses &middot;{' '}
                {discoveryResult.project.issue_types.length} issue types &middot;{' '}
                {discoveryResult.project.priorities.length} priorities
              </p>
            </div>
            <div>
              <h4 className="mb-2 text-xs font-semibold text-muted">Status Mapping</h4>
              <table className="w-full text-xs">
                <thead>
                  <tr className="border-b border-rule">
                    <th className="pb-1 text-left text-dim">Internal State</th>
                    <th className="pb-1 text-left text-dim">Jira Status</th>
                  </tr>
                </thead>
                <tbody>
                  {Object.keys(statusMapping).map(internal => (
                    <tr key={internal} className="border-b border-rule">
                      <td className="py-1 pr-3 text-muted">{internal}</td>
                      <td className="py-1">
                        <select
                          value={statusMapping[internal] ?? ''}
                          onChange={e => setStatusMapping(m => ({ ...m, [internal]: e.target.value }))}
                          className="w-full rounded-sm border border-rule bg-surface-950 px-2 py-0.5 text-xs text-fg focus:outline-none focus:ring-1 focus:ring-accent"
                        >
                          <option value="">— none —</option>
                          {discoveryResult.project.statuses.map(s => (
                            <option key={s.id} value={s.name}>{s.name}</option>
                          ))}
                        </select>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
            <div>
              <h4 className="mb-2 text-xs font-semibold text-muted">Issue Type Mapping</h4>
              <table className="w-full text-xs">
                <thead>
                  <tr className="border-b border-rule">
                    <th className="pb-1 text-left text-dim">Internal Type</th>
                    <th className="pb-1 text-left text-dim">Jira Issue Type</th>
                  </tr>
                </thead>
                <tbody>
                  {Object.keys(issueTypeMap).map(internal => (
                    <tr key={internal} className="border-b border-rule">
                      <td className="py-1 pr-3 text-muted">{internal}</td>
                      <td className="py-1">
                        <select
                          value={issueTypeMap[internal] ?? ''}
                          onChange={e => setIssueTypeMap(m => ({ ...m, [internal]: e.target.value }))}
                          className="w-full rounded-sm border border-rule bg-surface-950 px-2 py-0.5 text-xs text-fg focus:outline-none focus:ring-1 focus:ring-accent"
                        >
                          <option value="">— none —</option>
                          {discoveryResult.project.issue_types.map(t => (
                            <option key={t.id} value={t.name}>{t.name}</option>
                          ))}
                        </select>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
            <div>
              <h4 className="mb-2 text-xs font-semibold text-muted">Priority Mapping</h4>
              <table className="w-full text-xs">
                <thead>
                  <tr className="border-b border-rule">
                    <th className="pb-1 text-left text-dim">Internal Priority</th>
                    <th className="pb-1 text-left text-dim">Jira Priority</th>
                  </tr>
                </thead>
                <tbody>
                  {Object.keys(priorityMap).map(internal => (
                    <tr key={internal} className="border-b border-rule">
                      <td className="py-1 pr-3 text-muted">{internal}</td>
                      <td className="py-1">
                        <select
                          value={priorityMap[internal] ?? ''}
                          onChange={e => setPriorityMap(m => ({ ...m, [internal]: e.target.value }))}
                          className="w-full rounded-sm border border-rule bg-surface-950 px-2 py-0.5 text-xs text-fg focus:outline-none focus:ring-1 focus:ring-accent"
                        >
                          <option value="">— none —</option>
                          {discoveryResult.project.priorities.map(p => (
                            <option key={p.id} value={p.name}>{p.name}</option>
                          ))}
                        </select>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
            <div>
              <h4 className="mb-1 text-xs font-semibold text-muted">Ingest JQL</h4>
              <input
                type="text"
                value={ingestJQL}
                onChange={e => setIngestJQL(e.target.value)}
                className="w-full rounded-sm border border-rule bg-surface-950 px-3 py-2 text-xs text-fg focus:outline-none focus:ring-1 focus:ring-accent"
                placeholder="project = PROJ AND labels = autonomous-ingest ORDER BY created DESC"
              />
            </div>
            {mappingSaved ? (
              <p className="text-xs text-emerald-400">Mapping saved successfully.</p>
            ) : (
              <button
                onClick={handleConfirmMapping}
                disabled={confirmingSave}
                className="rounded bg-emerald-700 px-4 py-2 text-sm font-medium text-white hover:bg-emerald-600 disabled:opacity-50"
              >
                {confirmingSave ? 'Saving...' : 'Confirm Mapping'}
              </button>
            )}
          </div>
        )}
      </div>

      {error && <p className="text-sm text-red-400">{error}</p>}

      {!canContinue && (
        <p className="text-xs text-dim">
          Verify LLM connection and add at least one GitHub account to unlock Continue.
        </p>
      )}

      <button
        onClick={handleSaveAndContinue}
        disabled={saving || !canContinue}
        className="rounded bg-accent px-4 py-2 text-sm font-medium text-surface-900 hover:bg-accent-hover disabled:opacity-50"
      >
        {saving ? 'Saving...' : 'Save & Continue'}
      </button>
    </div>
  )
}

// ─── Step 2: Add Repositories ─────────────────────────────────────────────────

interface RepoEntry {
  github_url: string
  name: string
  base_branch: string
  merge_strategy: 'squash' | 'merge' | 'rebase'
  cloneStatus: 'pending' | 'cloning' | 'cloned' | 'error'
  cloneError?: string
  accessStatus: 'pending' | 'ok' | 'error'
  accessMsg?: string
}

interface AddRepositoriesStepProps {
  product: Product
  onNext: (repos: Repository[]) => void
}

function AddRepositoriesStep({ product, onNext }: AddRepositoriesStepProps) {
  const { post, get } = useApi()
  const { lastMessage, subscribe, unsubscribe, isConnected } = useWebSocket()
  const [repoList, setRepoList] = useState<RepoEntry[]>([])
  const [cloningAll, setCloningAll] = useState(false)

  // ── Pre-fill progress readout (fed by T19 WS bridge) ──────────────────────
  // Shape: { phase, files_processed, total_files } while scanning;
  // { modules_found, patterns_found } on completion.
  const [prefillProgress, setPrefillProgress] = useState<{
    phase: string
    files_processed: number
    total_files: number
  } | null>(null)
  const [prefillDone, setPrefillDone] = useState<{
    modules_found: number
    patterns_found: number
    questions_answered: number
  } | null>(null)

  useEffect(() => {
    subscribe('alerts')
    return () => unsubscribe('alerts')
  }, [subscribe, unsubscribe])

  useEffect(() => {
    if (!lastMessage || lastMessage.channel !== 'alerts') return
    const payload = lastMessage.data as
      | OnboardingPrefillProgress
      | OnboardingPrefillCompleted
      | null
    if (!payload) return
    if (payload.kind === 'onboarding_prefill_progress' && payload.product_id === product.id) {
      setPrefillProgress({
        phase: payload.phase,
        files_processed: payload.files_processed,
        total_files: payload.total_files,
      })
    } else if (payload.kind === 'onboarding_prefill_completed' && payload.product_id === product.id) {
      // modules_found + patterns_found come back from the server; the
      // "X/N questions" readout is best computed from the candidates API,
      // but we approximate here using the completed-event contract. The
      // questions count = total candidates that came back with a value
      // (i.e. confidence !== not_detected). Since the completed event
      // doesn't carry that, we fall back to fetching candidates once.
      setPrefillProgress({
        phase: 'complete',
        files_processed: 0,
        total_files: 0,
      })
      // Fetch candidates once to derive answered-question count honestly.
      get<{ candidates: Candidate[] }>(`/onboarding/candidates?product_id=${product.id}`)
        .then(res => {
          const answered = (res?.candidates ?? []).filter(
            c => c.confidence !== 'not_detected',
          ).length
          setPrefillDone({
            modules_found: payload.modules_found,
            patterns_found: payload.patterns_found,
            questions_answered: answered,
          })
        })
        .catch(() => {
          // Fail honestly: if we can't read candidates, show what we know.
          setPrefillDone({
            modules_found: payload.modules_found,
            patterns_found: payload.patterns_found,
            questions_answered: 0,
          })
        })
    }
  }, [lastMessage, product.id, get])

  // GitHub account picker state
  const [ghAccounts, setGhAccounts] = useState<GitHubAccount[]>([])
  const [selectedAccountId, setSelectedAccountId] = useState('')

  // repo browser state
  const [ghRepos, setGhRepos] = useState<GitHubRepo[]>([])
  const [loadingRepos, setLoadingRepos] = useState(false)
  const [reposError, setReposError] = useState('')
  const [search, setSearch] = useState('')
  const [selectedRepo, setSelectedRepo] = useState<GitHubRepo | null>(null)
  const [mergeStrategy, setMergeStrategy] = useState<'squash' | 'merge' | 'rebase'>('squash')
  const [addMode, setAddMode] = useState<'browser' | 'manual'>('browser')
  const searchRef = useRef<HTMLInputElement>(null)
  const [browserError, setBrowserError] = useState('')
  const [addingRepo, setAddingRepo] = useState(false)

  // per-repo pull state (repoIdx -> bool/message)
  const [pullingIdx, setPullingIdx] = useState<Record<number, boolean>>({})
  const [pullMsgIdx, setPullMsgIdx] = useState<Record<number, string>>({})

  // manual entry state
  const [githubUrl, setGithubUrl] = useState('')
  const [repoName, setRepoName] = useState('')
  const [baseBranch, setBaseBranch] = useState('main')
  const [manualMerge, setManualMerge] = useState<'squash' | 'merge' | 'rebase'>('squash')
  const [manualError, setManualError] = useState('')

  // repo IDs indexed by repoList position (to support Pull Latest)
  const [repoIds, setRepoIds] = useState<Record<number, string>>({})

  // Load already-added repos for this product on mount
  useEffect(() => {
    get<Repository[]>(`/admin/repositories?product_id=${product.id}`)
      .then(existing => {
        if (existing && existing.length > 0) {
          const entries: RepoEntry[] = existing.map(r => ({
            github_url: r.github_url,
            name: r.name,
            base_branch: r.base_branch || 'main',
            merge_strategy: (r.merge_strategy || 'squash') as 'squash' | 'merge' | 'rebase',
            // Use last_indexed_commit as a proxy for cloned (set by knowledge indexer after clone+analysis)
            // or fall back to checking clone_path non-empty from a previous session.
            cloneStatus: r.last_indexed_commit ? 'cloned' as const : 'pending' as const,
            accessStatus: 'ok' as const,
          }))
          const idMap: Record<number, string> = {}
          existing.forEach((r, i) => { idMap[i] = r.id })
          setRepoIds(idMap)
          setRepoList(entries)
          // Auto-clone any repos that haven't been indexed yet (older repos added before this fix)
          const uncloned = entries.filter(r => r.cloneStatus === 'pending')
          if (uncloned.length > 0) {
            autoCloneRepos(entries)
          }
        }
      })
      .catch(() => {})
  }, [get, product.id]) // eslint-disable-line react-hooks/exhaustive-deps

  // Load GitHub accounts, then auto-select the first one
  useEffect(() => {
    get<GitHubAccount[]>('/admin/github/accounts').then(a => {
      if (a?.length) { setGhAccounts(a); setSelectedAccountId(a[0].id) }
    }).catch(() => {})
  }, [get])

  // When the selected account changes, fetch repos for that account
  useEffect(() => {
    if (!selectedAccountId) return
    setGhRepos([])
    setLoadingRepos(true)
    setReposError('')
    get<GitHubRepo[]>('/admin/github/accounts/repos?id=' + selectedAccountId)
      .then(r => { if (r) setGhRepos(r) })
      .catch(e => setReposError(e instanceof Error ? e.message : 'Failed to load repos'))
      .finally(() => setLoadingRepos(false))
  }, [selectedAccountId, get])

  useEffect(() => {
    if (addMode === 'browser') searchRef.current?.focus()
  }, [addMode])

  const filtered = ghRepos.filter(r => {
    const q = search.toLowerCase()
    return !q || r.full_name.toLowerCase().includes(q) || (r.description ?? '').toLowerCase().includes(q)
  })

  function formatUpdated(iso: string) {
    if (!iso) return ''
    return new Date(iso).toLocaleDateString(undefined, { month: 'short', day: 'numeric', year: 'numeric' })
  }

  function handleUrlChange(val: string) {
    setGithubUrl(val)
    const match = val.match(/github\.com\/[^/]+\/([^/]+?)(?:\.git)?$/)
    if (match) setRepoName(match[1])
  }

  async function handleAddFromBrowser() {
    if (!selectedRepo) { setBrowserError('Select a repository first.'); return }
    if (repoList.find(r => r.github_url === selectedRepo.full_name)) {
      setBrowserError('Repository already added.')
      return
    }
    setBrowserError('')
    setAddingRepo(true)
    const newIdx = repoList.length
    // Optimistically add with 'cloning' status
    setRepoList(prev => [...prev, {
      github_url: selectedRepo.full_name,
      name: selectedRepo.name,
      base_branch: selectedRepo.default_branch,
      merge_strategy: mergeStrategy,
      cloneStatus: 'cloning',
      accessStatus: 'pending',
    }])
    try {
      const resp = await post<{ id?: string; clone_success?: boolean; clone_message?: string }>(
        '/admin/repositories/create', {
          product_id: product.id,
          github_url: selectedRepo.full_name,
          name: selectedRepo.name,
          base_branch: selectedRepo.default_branch,
          merge_strategy: mergeStrategy,
          github_account_id: selectedAccountId,
        }
      )
      if (resp?.id) {
        setRepoIds(m => ({ ...m, [newIdx]: resp.id! }))
      }
      const cloneOk = resp?.clone_success ?? false
      const cloneErr = cloneOk ? undefined : (resp?.clone_message ?? 'Clone failed')
      setRepoList(prev => prev.map((r, i) => i === newIdx
        ? { ...r, cloneStatus: cloneOk ? 'cloned' as const : 'error' as const, cloneError: cloneErr }
        : r
      ))
    } catch (e: unknown) {
      setRepoList(prev => prev.map((r, i) => i === newIdx
        ? { ...r, cloneStatus: 'error' as const, cloneError: e instanceof Error ? e.message : 'Failed to add' }
        : r
      ))
    } finally {
      setAddingRepo(false)
      setSelectedRepo(null)
    }
  }

  async function handleAddManual() {
    if (!githubUrl.trim()) { setManualError('GitHub URL is required.'); return }
    setManualError('')
    const name = repoName.trim() || githubUrl.trim()
    setAddingRepo(true)
    const newIdx = repoList.length
    setRepoList(prev => [...prev, {
      github_url: githubUrl.trim(),
      name,
      base_branch: baseBranch,
      merge_strategy: manualMerge,
      cloneStatus: 'cloning',
      accessStatus: 'pending',
    }])
    setGithubUrl('')
    setRepoName('')
    try {
      const resp = await post<{ id?: string; clone_success?: boolean; clone_message?: string }>(
        '/admin/repositories/create', {
          product_id: product.id,
          github_url: githubUrl.trim(),
          name,
          base_branch: baseBranch,
          merge_strategy: manualMerge,
          github_account_id: selectedAccountId,
        }
      )
      if (resp?.id) {
        setRepoIds(m => ({ ...m, [newIdx]: resp.id! }))
      }
      const cloneOk = resp?.clone_success ?? false
      const cloneErr = cloneOk ? undefined : (resp?.clone_message ?? 'Clone failed')
      setRepoList(prev => prev.map((r, i) => i === newIdx
        ? { ...r, cloneStatus: cloneOk ? 'cloned' as const : 'error' as const, cloneError: cloneErr }
        : r
      ))
    } catch (e: unknown) {
      setRepoList(prev => prev.map((r, i) => i === newIdx
        ? { ...r, cloneStatus: 'error' as const, cloneError: e instanceof Error ? e.message : 'Failed to add' }
        : r
      ))
    } finally {
      setAddingRepo(false)
    }
  }

  async function handlePullRepo(idx: number) {
    const repoId = repoIds[idx]
    if (!repoId) return
    setPullingIdx(p => ({ ...p, [idx]: true }))
    setPullMsgIdx(m => ({ ...m, [idx]: '' }))
    try {
      const result = await post<{ success?: boolean; message?: string }>(
        `/admin/repositories/pull?id=${repoId}`, {}
      )
      setPullMsgIdx(m => ({ ...m, [idx]: result?.success ? 'Pulled' : (result?.message ?? 'Pull failed') }))
    } catch (e: unknown) {
      setPullMsgIdx(m => ({ ...m, [idx]: e instanceof Error ? e.message : 'Pull failed' }))
    } finally {
      setPullingIdx(p => ({ ...p, [idx]: false }))
    }
  }

  async function handleTestAccess(idx: number) {
    const repo = repoList[idx]
    setRepoList(prev => prev.map((r, i) => i === idx ? { ...r, accessStatus: 'pending', accessMsg: '' } : r))
    try {
      await post('/admin/repositories/test-access', { github_url: repo.github_url })
      setRepoList(prev => prev.map((r, i) => i === idx ? { ...r, accessStatus: 'ok', accessMsg: 'Access verified' } : r))
    } catch (e: unknown) {
      setRepoList(prev => prev.map((r, i) => i === idx ? { ...r, accessStatus: 'error', accessMsg: e instanceof Error ? e.message : 'Access denied' } : r))
    }
  }

  // Auto-clone repos that are in pending state
  async function autoCloneRepos(_repos: RepoEntry[]) {
    setCloningAll(true)
    try {
      console.log('auto-clone: starting for product', product.id)
      const result = await post<{ success?: boolean; results?: Array<{ name: string; success: boolean; message: string }> }>(
        '/onboarding/clone-repos', { product_id: product.id }
      )
      console.log('auto-clone: result', result)
      // Update each repo based on the response
      if (result?.results) {
        setRepoList(prev => prev.map(r => {
          const match = result.results?.find(cr => cr.name === r.name)
          if (match) {
            return { ...r, cloneStatus: match.success ? 'cloned' as const : 'error' as const, cloneError: match.success ? undefined : match.message }
          }
          return r
        }))
      } else {
        // No detailed results — mark all as cloned if overall success
        setRepoList(prev => prev.map(r => r.cloneStatus === 'pending' || r.cloneStatus === 'cloning' ? { ...r, cloneStatus: 'cloned' as const } : r))
      }
    } catch (e: unknown) {
      console.error('auto-clone: failed', e)
      setRepoList(prev => prev.map(r => r.cloneStatus === 'pending' || r.cloneStatus === 'cloning' ? {
        ...r,
        cloneStatus: 'error' as const,
        cloneError: e instanceof Error ? e.message : 'Clone failed',
      } : r))
    }
    setCloningAll(false)
  }

  const clonedCount = repoList.filter(r => r.cloneStatus === 'cloned').length
  const canProceed = clonedCount > 0

  async function handleContinue() {
    try {
      const repos = await get<Repository[]>(`/admin/repositories?product_id=${product.id}`)
      onNext(repos)
    } catch (_) {
      onNext([])
    }
  }

  return (
    <div className="space-y-6">
      <div>
        <h2 className="text-base font-semibold text-fg">Add Repositories</h2>
        <p className="mt-1 text-sm text-muted">
          Add the GitHub repositories for <span className="text-accent">{product.name}</span>. At least one must be cloned to proceed.
        </p>
      </div>

      {/* Clone status overview */}
      {repoList.length > 0 && (
        <div className="rounded-sm border border-rule bg-surface-900 p-4">
          <p className="mb-2 text-xs font-semibold text-muted">Clone Status</p>
          <div className="space-y-1">
            {repoList.map((r, i) => (
              <div key={i} className="flex items-center gap-2 text-xs">
                {r.cloneStatus === 'cloned' ? (
                  <span className="text-emerald-400">✓</span>
                ) : r.cloneStatus === 'error' ? (
                  <span className="text-red-400">✗</span>
                ) : r.cloneStatus === 'cloning' ? (
                  <span className="animate-spin text-accent-dim">⟳</span>
                ) : (
                  <span className="text-dim">○</span>
                )}
                <span className={r.cloneStatus === 'cloned' ? 'text-fg' : 'text-muted'}>
                  {r.name}
                </span>
                {r.cloneError && <span className="text-red-400">— {r.cloneError}</span>}
              </div>
            ))}
          </div>
        </div>
      )}

      {/* Add repo panel */}
      <div className="rounded-sm border border-rule bg-surface-900 overflow-hidden">
        {/* Tab bar */}
        <div className="flex border-b border-rule">
          <button
            onClick={() => setAddMode('browser')}
            className={`px-4 py-2.5 text-xs font-medium transition-colors ${addMode === 'browser' ? 'border-b-2 border-accent text-accent' : 'text-muted hover:text-fg'}`}
          >
            Browse GitHub Repos
          </button>
          <button
            onClick={() => setAddMode('manual')}
            className={`px-4 py-2.5 text-xs font-medium transition-colors ${addMode === 'manual' ? 'border-b-2 border-accent text-accent' : 'text-muted hover:text-fg'}`}
          >
            Manual Entry
          </button>
        </div>

        {addMode === 'browser' ? (
          <div className="space-y-0">
            {/* Account picker */}
            <div className="border-b border-rule px-4 py-3">
              <label className="mb-1 block text-xs text-muted">GitHub Account</label>
              <select value={selectedAccountId} onChange={e => setSelectedAccountId(e.target.value)}
                className="w-full rounded-sm border border-rule bg-surface-950 px-3 py-2 text-sm text-fg focus:outline-none focus:ring-1 focus:ring-accent">
                {ghAccounts.map(a => <option key={a.id} value={a.id}>{a.username}{a.label ? ` (${a.label})` : ''}</option>)}
              </select>
            </div>
            {/* Search */}
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
            <div className="max-h-64 overflow-y-auto">
              {loadingRepos ? (
                <div className="flex items-center justify-center py-10 text-sm text-muted">
                  <svg className="mr-2 h-4 w-4 animate-spin" fill="none" viewBox="0 0 24 24">
                    <circle className="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" strokeWidth="4" />
                    <path className="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8v8H4z" />
                  </svg>
                  Loading repositories…
                </div>
              ) : reposError ? (
                <div className="px-5 py-6 text-center">
                  <p className="text-sm text-red-400">{reposError}</p>
                  <button onClick={() => setAddMode('manual')} className="mt-2 text-xs text-accent-dim hover:underline">
                    Switch to manual entry
                  </button>
                </div>
              ) : filtered.length === 0 ? (
                <div className="px-5 py-6 text-center text-sm text-muted">
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
                        className={`cursor-pointer px-4 py-3 transition-colors ${isSelected ? 'bg-indigo-900/40 ring-1 ring-inset ring-indigo-600' : 'hover:bg-surface-800/50'}`}
                      >
                        <div className="flex items-start gap-3">
                          <div className={`mt-0.5 h-4 w-4 flex-shrink-0 rounded-full border-2 transition-colors ${isSelected ? 'border-accent bg-indigo-500' : 'border-rule'}`} />
                          <div className="min-w-0 flex-1">
                            <div className="flex flex-wrap items-center gap-1.5">
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

            {/* Browser controls */}
            <div className="border-t border-rule px-4 py-3 space-y-2">
              <div className="flex items-center gap-3">
                <div className="flex-1">
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
                <div className="mt-5 flex-shrink-0">
                  <button
                    onClick={handleAddFromBrowser}
                    disabled={!selectedRepo || addingRepo}
                    className="rounded bg-accent px-4 py-2 text-sm font-medium text-surface-900 hover:bg-accent-hover disabled:opacity-50"
                  >
                    {addingRepo ? 'Adding & Cloning…' : 'Add Selected'}
                  </button>
                </div>
              </div>
              {selectedRepo && (
                <p className="text-xs text-muted">
                  Selected: <span className="font-medium text-fg">{selectedRepo.full_name}</span>
                  {' '}· branch <span className="font-medium text-fg">{selectedRepo.default_branch}</span>
                </p>
              )}
              {browserError && <p className="text-xs text-red-400">{browserError}</p>}
            </div>
          </div>
        ) : (
          /* Manual entry */
          <div className="space-y-4 p-5">
            <div>
              <label className="mb-1 block text-xs text-muted">GitHub URL</label>
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
                value={repoName}
                onChange={e => setRepoName(e.target.value)}
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
                  value={manualMerge}
                  onChange={e => setManualMerge(e.target.value as 'squash' | 'merge' | 'rebase')}
                  className={inputClass()}
                >
                  <option value="squash">Squash</option>
                  <option value="merge">Merge</option>
                  <option value="rebase">Rebase</option>
                </select>
              </div>
            </div>
            {manualError && <p className="text-xs text-red-400">{manualError}</p>}
            <button
              onClick={handleAddManual}
              disabled={addingRepo}
              className="rounded bg-accent px-4 py-2 text-sm font-medium text-surface-900 hover:bg-accent-hover disabled:opacity-50"
            >
              {addingRepo ? 'Adding & Cloning…' : 'Add Repository'}
            </button>
          </div>
        )}
      </div>

      {/* Repo list */}
      {repoList.length > 0 && (
        <div className="space-y-3">
          {repoList.map((repo, idx) => (
            <div key={idx} className="rounded-sm border border-rule bg-surface-900 p-4">
              <div className="flex items-start justify-between gap-4">
                <div className="space-y-0.5">
                  <p className="text-sm font-medium text-fg">{repo.name}</p>
                  <p className="text-xs text-dim">{repo.github_url}</p>
                  <p className="text-xs text-muted">
                    {repo.base_branch} · {repo.merge_strategy}
                  </p>
                  {repo.cloneStatus === 'error' && repo.cloneError && (
                    <p className="text-xs text-red-400 max-w-xs truncate" title={repo.cloneError}>
                      {repo.cloneError}
                    </p>
                  )}
                </div>
                <div className="flex flex-col items-end gap-2">
                  <div className="flex items-center gap-2">
                    <span className={`text-xs ${repo.accessStatus === 'ok' ? 'text-emerald-400' : repo.accessStatus === 'error' ? 'text-red-400' : 'text-dim'}`}>
                      {repo.accessStatus === 'ok' ? 'Access OK' : repo.accessStatus === 'error' ? repo.accessMsg : 'Not tested'}
                    </span>
                    <button
                      onClick={() => handleTestAccess(idx)}
                      className="rounded-sm border border-rule px-2 py-1 text-xs text-muted hover:bg-surface-700"
                    >
                      Test Access
                    </button>
                  </div>
                  <div className="flex items-center gap-2 flex-wrap justify-end">
                    {repo.cloneStatus === 'cloned' && (
                      <span className="text-xs text-emerald-400">Cloned</span>
                    )}
                    {repo.cloneStatus === 'error' && (
                      <span className="text-xs text-red-400" title={repo.cloneError}>Clone failed</span>
                    )}
                    {repo.cloneStatus === 'cloning' && (
                      <span className="flex items-center gap-1 text-xs text-accent-dim">
                        <svg className="h-3 w-3 animate-spin" fill="none" viewBox="0 0 24 24">
                          <circle className="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" strokeWidth="4" />
                          <path className="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8v8H4z" />
                        </svg>
                        Cloning…
                      </span>
                    )}
                    {/* Pull Latest — only available once we have the repo ID */}
                    {repoIds[idx] && repo.cloneStatus !== 'cloning' && (
                      <button
                        onClick={() => handlePullRepo(idx)}
                        disabled={pullingIdx[idx]}
                        className="rounded-sm border border-rule px-2 py-1 text-xs text-muted hover:bg-surface-700 disabled:opacity-50"
                      >
                        {pullingIdx[idx] ? 'Pulling…' : 'Pull Latest'}
                      </button>
                    )}
                    {pullMsgIdx[idx] && (
                      <span className={`text-xs ${pullMsgIdx[idx] === 'Pulled' ? 'text-emerald-400' : 'text-red-400'}`}>
                        {pullMsgIdx[idx]}
                      </span>
                    )}
                    <button
                      onClick={() => setRepoList(prev => prev.filter((_, i) => i !== idx))}
                      className="rounded-sm border border-red-800 px-2 py-1 text-xs text-red-400 hover:bg-red-900/30"
                    >
                      Remove
                    </button>
                  </div>
                </div>
              </div>
            </div>
          ))}

          {cloningAll && (
            <p className="text-sm text-muted animate-pulse">Cloning repositories automatically…</p>
          )}
        </div>
      )}

      {!canProceed && repoList.length > 0 && (
        <p className="text-xs text-amber-400">
          At least one repository must be successfully cloned to proceed.
        </p>
      )}

      {/* Pre-fill progress readout — silent background scan, T19 WS-fed */}
      {clonedCount > 0 && (
        <div className="rounded-sm border border-rule bg-surface-900 p-3">
          {!isConnected ? (
            <p className="text-xs text-dim">
              <span className="inline-block h-2 w-2 rounded-full bg-surface-700 animate-pulse mr-2" />
              Connecting to scan progress stream…
            </p>
          ) : prefillDone ? (
            <p className="text-xs text-emerald-300">
              Scan complete — {prefillDone.modules_found} module
              {prefillDone.modules_found === 1 ? '' : 's'},{' '}
              {prefillDone.patterns_found} pattern
              {prefillDone.patterns_found === 1 ? '' : 's'} found. Pre-fill{' '}
              {prefillDone.questions_answered}/{CONVERSATION_ORDER.length} questions answered.
            </p>
          ) : prefillProgress && prefillProgress.phase !== 'idle' ? (
            <p className="text-xs text-muted">
              <span className="inline-block h-2 w-2 rounded-full bg-indigo-500 animate-pulse mr-2" />
              Scanning your repo — {prefillProgress.files_processed} of{' '}
              {prefillProgress.total_files || '?'} files processed
              {prefillProgress.phase ? ` (${prefillProgress.phase})` : ''}.
            </p>
          ) : (
            <p className="text-xs text-dim">
              Background scan will start automatically after clone completes.
            </p>
          )}
        </div>
      )}

      <button
        onClick={handleContinue}
        disabled={!canProceed}
        className="rounded bg-accent px-4 py-2 text-sm font-medium text-surface-900 hover:bg-accent-hover disabled:opacity-50"
      >
        Continue
      </button>
    </div>
  )
}

// ─── Step 3: Map Dependencies ─────────────────────────────────────────────────

const DEP_TYPES: Array<{ value: string; label: string; sourceLabel: string; targetLabel: string; sentence: string }> = [
  { value: 'api_consumer', label: 'API Consumer', sourceLabel: 'Consumes the API', targetLabel: 'Provides the API', sentence: 'calls APIs exposed by' },
  { value: 'shared_library', label: 'Shared Library', sourceLabel: 'Imports from', targetLabel: 'Provides the library', sentence: 'imports packages from' },
  { value: 'deployment', label: 'Deployment', sourceLabel: 'Deploys after', targetLabel: 'Must deploy first', sentence: 'must deploy after' },
  { value: 'data', label: 'Data', sourceLabel: 'Reads data from', targetLabel: 'Writes data to the store', sentence: 'reads data written by' },
]

interface MapDependenciesStepProps {
  product: Product
  repos: Repository[]
  onNext: () => void
}

function MapDependenciesStep({ product, repos, onNext }: MapDependenciesStepProps) {
  const { post, del, get } = useApi()
  const [deps, setDeps] = useState<RepoDependency[]>([])
  const [sourceId, setSourceId] = useState(repos[0]?.id ?? '')
  const [targetId, setTargetId] = useState(repos[1]?.id ?? repos[0]?.id ?? '')
  const [depType, setDepType] = useState(DEP_TYPES[0].value)
  const [description, setDescription] = useState('')
  const [noDeps, setNoDeps] = useState(false)
  const [error, setError] = useState('')

  useEffect(() => {
    get<RepoDependency[]>(`/admin/dependencies?product_id=${product.id}`)
      .then(data => setDeps(data ?? []))
      .catch(() => {})
  }, [product.id, get])

  // ── Suggested cross-repo imports (from `cross_repo_imports` candidate) ────
  const [suggestions, setSuggestions] = useState<Evidence[]>([])
  const [suggestionsLoading, setSuggestionsLoading] = useState(true)
  const [suggestionsError, setSuggestionsError] = useState<string | null>(null)

  useEffect(() => {
    let cancelled = false
    setSuggestionsLoading(true)
    get<{ candidates: Candidate[] }>(`/onboarding/candidates?product_id=${product.id}`)
      .then(res => {
        if (cancelled) return
        const c = (res?.candidates ?? []).find(
          x => x.question_key === 'cross_repo_imports',
        )
        if (c && c.confidence !== 'not_detected') {
          setSuggestions(c.evidence ?? [])
        } else {
          setSuggestions([])
        }
      })
      .catch((e: unknown) => {
        if (cancelled) return
        setSuggestionsError(e instanceof Error ? e.message : 'Failed to load suggestions')
      })
      .finally(() => { if (!cancelled) setSuggestionsLoading(false) })
    return () => { cancelled = true }
  }, [product.id, get])

  // Pre-fill the manual form from a suggestion. Never auto-saves — user must
  // still click "Add Dependency".
  function applySuggestion(ev: Evidence) {
    const label = ev.name || ev.path || ev.detail || ev.kind
    // Try to match suggestion.name against a repo name to pre-select target.
    if (ev.name) {
      const target = repos.find(
        r => r.name === ev.name || r.github_url.endsWith(`/${ev.name}`),
      )
      if (target && target.id !== sourceId) setTargetId(target.id)
    }
    const detail = ev.detail ? ` · ${ev.detail}` : ''
    setDescription(`Detected import: ${label}${detail}`)
  }

  async function handleAddDep() {
    if (sourceId === targetId) { setError('Source and target must differ.'); return }
    setError('')
    try {
      const res = await post<{ id: string }>(`/admin/dependencies/create`, {
        source_repo_id: sourceId,
        target_repo_id: targetId,
        dependency_type: depType,
        description: description.trim(),
      })
      if (res?.id) {
        setDeps(prev => {
          const existing = prev.find(d => d.id === res.id)
          if (existing) return prev
          return [...prev, {
            id: res.id,
            source_repo_id: sourceId,
            target_repo_id: targetId,
            dependency_type: depType,
            description: description.trim(),
            discovered_by: 'manual',
            created_at: new Date().toISOString(),
          }]
        })
      }
      setDescription('')
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Failed to add dependency')
    }
  }

  async function handleDeleteDep(depId: string) {
    try {
      await del(`/admin/dependencies/delete?id=${depId}`)
      setDeps(d => d.filter(x => x.id !== depId))
    } catch (_) {
      // no-op
    }
  }

  function repoName(id: string) {
    return repos.find(r => r.id === id)?.name ?? id
  }

  return (
    <div className="space-y-6">
      <div>
        <h2 className="text-base font-semibold text-fg">Map Dependencies</h2>
        <p className="mt-1 text-sm text-muted">
          Tell us how your repos in <span className="text-accent">{product.name}</span> relate to each other.
          When one repo consumes another's API, shares a library, or depends on its deployment,
          mapping that relationship lets agents coordinate changes across repos and flag breaking impacts.
        </p>
        <p className="mt-2 text-xs text-dim">
          Skip this step if your product has a single repo or no cross-repo relationships.
        </p>
      </div>

      {/* Suggested cross-repo imports (best-effort, degrades gracefully) */}
      {!suggestionsLoading && suggestions.length > 0 && (
        <div className="rounded-sm border border-amber-700 bg-amber-900/10 p-4 space-y-2">
          <h3 className="text-sm font-semibold text-amber-200">
            Suggested cross-repo imports
          </h3>
          <p className="text-xs text-muted">
            We detected these imports between your repos. Click
            <span className="font-medium text-muted"> Add as dependency </span>
            to pre-fill the form — nothing is saved until you press
            <span className="font-medium text-muted"> Add Dependency</span>.
          </p>
          <ul className="space-y-1.5">
            {suggestions.map((ev, i) => (
              <li
                key={i}
                className="flex items-start justify-between gap-3 rounded-sm border border-amber-800/40 bg-surface-950 px-3 py-2"
              >
                <div className="min-w-0 flex-1">
                  <p className="truncate text-xs font-mono text-amber-100">
                    {ev.name || ev.path || ev.detail || ev.kind}
                  </p>
                  {(ev.path || ev.lines) && (
                    <p className="truncate text-[11px] text-dim">
                      {ev.path}
                      {ev.lines ? `:${ev.lines}` : ''}
                    </p>
                  )}
                  {ev.detail && ev.detail !== ev.name && (
                    <p className="truncate text-[11px] text-dim">{ev.detail}</p>
                  )}
                </div>
                <button
                  type="button"
                  onClick={() => applySuggestion(ev)}
                  className="shrink-0 rounded-sm border border-amber-700 bg-amber-900/30 px-2 py-1 text-[11px] font-medium text-amber-100 hover:bg-amber-800/50"
                >
                  Add as dependency
                </button>
              </li>
            ))}
          </ul>
        </div>
      )}
      {suggestionsError && (
        <p className="text-xs text-dim">
          (Suggestions unavailable: {suggestionsError})
        </p>
      )}

      {repos.length >= 2 && !noDeps && (
        <div className="rounded-sm border border-rule bg-surface-900 p-5 space-y-4">
          <h3 className="text-sm font-semibold text-fg">Add Dependency</h3>
          <div>
            <label className="mb-1 block text-xs text-muted">Relationship type</label>
            <select value={depType} onChange={e => setDepType(e.target.value)} className={inputClass('max-w-xs')}>
              {DEP_TYPES.map(t => <option key={t.value} value={t.value}>{t.label}</option>)}
            </select>
          </div>
          <div className="grid grid-cols-2 gap-3">
            <div>
              <label className="mb-1 block text-xs text-accent">{DEP_TYPES.find(t => t.value === depType)?.sourceLabel ?? 'Source'}</label>
              <select value={sourceId} onChange={e => setSourceId(e.target.value)} className={inputClass()}>
                {repos.map(r => <option key={r.id} value={r.id}>{r.name}</option>)}
              </select>
            </div>
            <div>
              <label className="mb-1 block text-xs text-accent">{DEP_TYPES.find(t => t.value === depType)?.targetLabel ?? 'Target'}</label>
              <select value={targetId} onChange={e => setTargetId(e.target.value)} className={inputClass()}>
                {repos.map(r => <option key={r.id} value={r.id}>{r.name}</option>)}
              </select>
            </div>
          </div>
          {sourceId && targetId && sourceId !== targetId && (
            <p className="rounded-sm bg-surface-950 px-3 py-2 text-sm text-muted">
              <span className="font-medium text-fg">{repoName(sourceId)}</span>
              {' '}{DEP_TYPES.find(t => t.value === depType)?.sentence ?? 'depends on'}{' '}
              <span className="font-medium text-fg">{repoName(targetId)}</span>
            </p>
          )}
          <div>
            <label className="mb-1 block text-xs text-muted">Notes (optional)</label>
            <input
              type="text"
              value={description}
              onChange={e => setDescription(e.target.value)}
              placeholder="e.g. uses /api/v2/users endpoint"
              className={inputClass('max-w-md')}
            />
          </div>
          {error && <p className="text-xs text-red-400">{error}</p>}
          <button
            onClick={handleAddDep}
            className="rounded bg-accent px-4 py-2 text-sm font-medium text-surface-900 hover:bg-accent-hover"
          >
            Add Dependency
          </button>
        </div>
      )}

      {deps.length > 0 && (
        <div className="space-y-2">
          <h3 className="text-sm font-semibold text-fg">Defined Dependencies</h3>
          {deps.map(dep => (
            <div key={dep.id} className="flex items-center justify-between rounded-sm border border-rule bg-surface-950 px-4 py-2.5">
              <span className="text-sm text-fg">
                <span className="font-medium text-accent">{repoName(dep.source_repo_id)}</span>
                <span className="mx-1.5 text-dim">{DEP_TYPES.find(t => t.value === dep.dependency_type)?.sentence ?? '→'}</span>
                <span className="font-medium text-accent">{repoName(dep.target_repo_id)}</span>
              </span>
              <button
                onClick={() => handleDeleteDep(dep.id)}
                className="rounded-sm border border-red-800 px-2 py-1 text-xs text-red-400 hover:bg-red-900/30"
              >
                Delete
              </button>
            </div>
          ))}
        </div>
      )}

      <label className="flex items-center gap-2 cursor-pointer">
        <input
          type="checkbox"
          checked={noDeps}
          onChange={e => setNoDeps(e.target.checked)}
          className="rounded-sm border-rule"
        />
        <span className="text-sm text-muted">No cross-repo dependencies</span>
      </label>

      <button
        onClick={onNext}
        className="rounded bg-accent px-4 py-2 text-sm font-medium text-surface-900 hover:bg-accent-hover"
      >
        Continue
      </button>
    </div>
  )
}

// ─── Step 4: Codebase Analysis ────────────────────────────────────────────────

interface CodebaseAnalysisStepProps {
  product: Product
  repos: Repository[]
  onNext: () => void
}

function CodebaseAnalysisStep({ product, repos, onNext }: CodebaseAnalysisStepProps) {
  const { post } = useApi()
  const [kicked, setKicked] = useState(false)
  const [error, setError] = useState('')

  async function handleRunAnalysis() {
    setError('')
    try {
      for (const repo of repos) {
        await post(
          `/admin/repositories/analyze?id=${repo.id}`,
          { product_id: product.id }
        )
      }
      setKicked(true)
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Failed to start analysis')
    }
  }

  return (
    <div className="space-y-6">
      <div>
        <h2 className="text-base font-semibold text-fg">Codebase Analysis</h2>
        <p className="mt-1 text-sm text-muted">
          Kick off a scan of your repositories to discover modules and patterns.
          Analysis runs in the background — you can track progress in the activity bar at the top of the dashboard.
        </p>
      </div>

      <div className="rounded-sm border border-rule bg-surface-900 p-4">
        <p className="text-sm text-muted mb-3">
          {repos.length} {repos.length === 1 ? 'repository' : 'repositories'} will be analyzed:
        </p>
        <ul className="space-y-1.5">
          {repos.map(r => (
            <li key={r.id} className="flex items-center gap-2 text-sm">
              <span className="text-accent">•</span>
              <span className="font-medium text-fg">{r.name}</span>
            </li>
          ))}
        </ul>
      </div>

      {error && <p className="text-sm text-red-400">{error}</p>}

      {kicked && (
        <div className="rounded-sm border border-accent/30 bg-accent/5 px-4 py-3">
          <p className="text-sm text-accent">
            Analysis started. You can continue setup — progress is visible in the activity bar above.
          </p>
        </div>
      )}

      <div className="flex items-center gap-4">
        {!kicked ? (
          <>
            <button
              onClick={async () => { await handleRunAnalysis(); onNext() }}
              className="rounded bg-accent px-4 py-2 text-sm font-medium text-surface-900 hover:bg-accent-hover"
            >
              Start Analysis &amp; Continue
            </button>
            <button
              onClick={onNext}
              className="rounded border border-rule px-4 py-2 text-sm font-medium text-muted hover:text-fg hover:border-sharp"
            >
              Skip for Now
            </button>
          </>
        ) : (
          <button
            onClick={onNext}
            className="rounded bg-accent px-4 py-2 text-sm font-medium text-surface-900 hover:bg-accent-hover"
          >
            Continue
          </button>
        )}
      </div>
    </div>
  )
}

// ─── Step 5: Interactive Conversation ─────────────────────────────────────────

interface InteractiveConversationStepProps {
  product: Product
  repos: Repository[]
  onNext: () => void
}

function InteractiveConversationStep({ product, repos, onNext }: InteractiveConversationStepProps) {
  const { get, post } = useApi()
  const [candidates, setCandidates] = useState<Record<string, Candidate>>({})
  const [loading, setLoading] = useState(true)
  const [regenerating, setRegenerating] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const loadCandidates = useCallback(async () => {
    setLoading(true)
    setError(null)
    try {
      const res = await get<{ candidates: Candidate[] }>(`/onboarding/candidates?product_id=${product.id}`)
      const byKey: Record<string, Candidate> = {}
      for (const c of res.candidates ?? []) byKey[c.question_key] = c
      setCandidates(byKey)
    } catch (e: unknown) {
      // Fail Honestly: surface the real error, do NOT fall back to an empty
      // "no candidates found" state that pretends pre-fill succeeded.
      setError(e instanceof Error ? e.message : String(e))
      setCandidates({})
    } finally {
      setLoading(false)
    }
  }, [get, product.id])

  useEffect(() => {
    loadCandidates()
  }, [loadCandidates])

  async function handleReview(
    questionKey: string,
    status: 'confirmed' | 'edited' | 'rejected',
    userValue?: string,
  ) {
    // Backend contract: POST body field is "status" (not "user_status").
    // See cmd/onboarding/main.go candidatesReviewHandler.
    await post('/onboarding/candidates/review', {
      product_id: product.id,
      question_key: questionKey,
      status,
      user_value: userValue,
    })
    // Refetch so user_status / user_value / reviewed_at reflect the server state.
    await loadCandidates()
  }

  async function handleRegenerate() {
    setRegenerating(true)
    setError(null)
    try {
      await post('/onboarding/candidates/regenerate', { product_id: product.id })
      // Indexer republishes candidates asynchronously; give it a moment then refetch.
      setTimeout(() => { void loadCandidates() }, 2000)
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setRegenerating(false)
    }
  }

  // Order candidates by CONVERSATION_ORDER; drop keys the backend hasn't
  // published yet. The chat walks through `ordered` left-to-right; whatever
  // isn't yet published simply doesn't appear — Re-detect fills gaps later.
  const ordered = CONVERSATION_ORDER
    .map(k => candidates[k])
    .filter((c): c is Candidate => !!c)

  const missingKeys = CONVERSATION_ORDER.filter(k => !candidates[k])

  // The current active question is the first one still pending. Questions
  // above it in the list are resolved; questions below are queued.
  const currentIndex = ordered.findIndex(c => c.user_status === 'pending')
  const current = currentIndex >= 0 ? ordered[currentIndex] : null
  const history = currentIndex >= 0 ? ordered.slice(0, currentIndex) : ordered
  const answeredCount = ordered.filter(c => c.user_status !== 'pending').length
  const total = ordered.length
  const allReviewed = total > 0 && current === null
  const hasCandidates = total > 0

  return (
    <div className="space-y-4">
      <div className="flex items-start justify-between gap-3">
        <div>
          <h2 className="text-base font-semibold text-fg">Conversation</h2>
          <p className="mt-1 text-sm text-muted">
            Reviewing what we detected for <span className="text-accent">{product.name}</span>
            {' — repos: '}{repos.map(r => r.name).join(', ')}.
          </p>
        </div>
        <button
          type="button"
          onClick={handleRegenerate}
          disabled={regenerating}
          className="rounded-sm border border-rule px-3 py-1.5 text-xs text-fg hover:bg-surface-900 disabled:opacity-50"
        >
          {regenerating ? 'Re-detecting…' : 'Re-detect'}
        </button>
      </div>

      {hasCandidates && (
        <div className="rounded-sm border border-rule bg-surface-900 px-4 py-3">
          <div className="flex items-center justify-between text-xs text-muted">
            <span>{answeredCount} of {total} answered{missingKeys.length > 0 ? ` · ${missingKeys.length} still scanning` : ''}</span>
            <span className="font-mono">{Math.round((answeredCount / Math.max(total, 1)) * 100)}%</span>
          </div>
          <div className="mt-2 h-1.5 rounded bg-surface-950 overflow-hidden">
            <div
              className="h-full bg-indigo-500 transition-[width] duration-300"
              style={{ width: `${(answeredCount / Math.max(total, 1)) * 100}%` }}
            />
          </div>
        </div>
      )}

      {error && (
        <div className="rounded-sm border border-red-700/50 bg-red-900/15 px-4 py-3 space-y-2">
          <p className="text-sm font-semibold text-red-300">Could not load pre-fill candidates</p>
          <p className="text-xs text-red-200 font-mono break-all">{error}</p>
          <p className="text-xs text-red-300/80">
            Check <span className="font-mono">docker compose logs dashboard-backend</span> and{' '}
            <span className="font-mono">docker compose logs knowledge-indexer</span>.
          </p>
          <button
            type="button"
            onClick={loadCandidates}
            className="mt-1 rounded-sm border border-red-700 px-3 py-1 text-xs text-red-200 hover:bg-red-900/40"
          >
            Retry
          </button>
        </div>
      )}

      {loading && !error && (
        <div className="flex items-center gap-3 rounded-sm border border-rule bg-surface-900 px-4 py-3">
          <div className="h-4 w-4 animate-spin rounded-full border-2 border-accent border-t-transparent" />
          <p className="text-sm text-muted">Loading conversation…</p>
        </div>
      )}

      {!loading && !error && !hasCandidates && (
        <div className="rounded-sm border border-amber-700/50 bg-amber-900/10 px-4 py-3 flex items-start gap-3">
          <div className="mt-0.5 h-4 w-4 shrink-0 animate-spin rounded-full border-2 border-amber-500 border-t-transparent" />
          <div className="space-y-1">
            <p className="text-sm text-amber-300">Analysis still running.</p>
            <p className="text-xs text-amber-300/80">
              The background scan hasn&apos;t finished yet. You can wait and press{' '}
              <span className="font-mono">Re-detect</span>, or skip this step and come back later.
            </p>
          </div>
        </div>
      )}

      {/* Chat transcript: completed turns, then the active one. */}
      {!loading && !error && hasCandidates && (
        <div className="space-y-3">
          {history.map(c => (
            <CompletedTurn
              key={c.question_key}
              candidate={c}
              questionText={QUESTION_TEXT[c.question_key]}
            />
          ))}

          {current && (
            <ActiveTurn
              key={current.question_key}
              candidate={current}
              questionText={QUESTION_TEXT[current.question_key]}
              onReview={(status, uv) => handleReview(current.question_key, status, uv)}
            />
          )}

          {allReviewed && (
            <div className="rounded-sm border border-emerald-700/50 bg-emerald-900/15 px-4 py-3">
              <p className="text-sm font-semibold text-emerald-300">All questions answered.</p>
              <p className="mt-1 text-xs text-emerald-300/80">
                Review locked in for all {total} questions. Click Continue to proceed to the codex review.
              </p>
            </div>
          )}
        </div>
      )}

      <div className="flex items-center gap-3 pt-2">
        {hasCandidates && !allReviewed ? (
          <>
            <button
              type="button"
              onClick={onNext}
              disabled={!allReviewed}
              className="rounded bg-accent px-4 py-2 text-sm font-medium text-surface-900 hover:bg-accent-hover disabled:cursor-not-allowed disabled:opacity-50"
            >
              Continue to Codex Review
            </button>
            <span className="text-xs text-dim">
              {total - answeredCount} remaining
            </span>
          </>
        ) : (
          <button
            type="button"
            onClick={onNext}
            className="rounded bg-accent px-4 py-2 text-sm font-medium text-surface-900 hover:bg-accent-hover"
          >
            {allReviewed ? 'Continue to Codex Review' : 'Skip for Now'}
          </button>
        )}
      </div>
    </div>
  )
}

// Collapsed row for a question the user already answered. Shows the question,
// how they resolved it (confirmed / edited / rejected), and the final value.
function CompletedTurn({ candidate, questionText }: { candidate: Candidate; questionText: string }) {
  const statusColor: Record<string, string> = {
    confirmed: 'text-emerald-400',
    edited:    'text-accent-dim',
    rejected:  'text-dim',
  }
  const statusLabel: Record<string, string> = {
    confirmed: '✓ confirmed',
    edited:    '✎ edited',
    rejected:  '↷ skipped',
  }
  const status = candidate.user_status
  const shownValue = candidate.user_value ?? (status === 'rejected' ? '' : candidate.value)
  return (
    <div className="rounded-sm border border-rule bg-surface-900/50 px-4 py-3 space-y-1">
      <div className="flex items-center justify-between gap-3">
        <p className="text-xs text-muted">{questionText}</p>
        <span className={`shrink-0 text-[10px] font-mono ${statusColor[status] ?? 'text-dim'}`}>
          {statusLabel[status] ?? status}
        </span>
      </div>
      {shownValue && (
        <p className="whitespace-pre-wrap text-sm text-muted">{shownValue}</p>
      )}
      {status === 'rejected' && !shownValue && (
        <p className="text-xs italic text-dim">Skipped.</p>
      )}
    </div>
  )
}

// The currently-active chat turn. Renders as: AE's message (prompt + suggested
// answer + confidence), followed by inline action buttons. For not_detected
// candidates, the full CandidateCard free-text textarea shows up.
function ActiveTurn({
  candidate,
  questionText,
  onReview,
}: {
  candidate: Candidate
  questionText: string
  onReview: (status: 'confirmed' | 'edited' | 'rejected', userValue?: string) => Promise<void>
}) {
  return (
    <div className="rounded-sm border border-indigo-700/40 bg-indigo-900/10 p-4">
      <div className="mb-3 flex items-center gap-2">
        <span className="flex h-6 w-6 items-center justify-center rounded-full bg-accent text-xs font-semibold text-surface-900">AE</span>
        <span className="text-xs text-muted">Next question</span>
      </div>
      <CandidateCard
        candidate={candidate}
        questionText={questionText}
        onReview={onReview}
      />
    </div>
  )
}

// ─── Step 6: Codex Review ─────────────────────────────────────────────────────

interface CodexReviewStepProps {
  product: Product
  repos: Repository[]
  onNext: () => void
  onBack?: () => void
}

function CodexReviewStep({ product, repos, onNext, onBack }: CodexReviewStepProps) {
  const { get } = useApi()
  const { lastMessage, subscribe, unsubscribe, isConnected } = useWebSocket()
  const [stats, setStats] = useState<{ modules: number; patterns: number; last_updated?: string } | null>(null)
  const [graph, setGraph] = useState<GraphResponse | null>(null)
  const [modules, setModules] = useState<CodexModule[]>([])
  const [patterns, setPatterns] = useState<CodexPattern[]>([])
  const [sections, setSections] = useState<CodexSection[]>([])
  const [showModules, setShowModules] = useState(false)
  const [showPatterns, setShowPatterns] = useState(false)
  const [showInsights, setShowInsights] = useState(false)
  const [loading, setLoading] = useState(true)
  const [fetchError, setFetchError] = useState<string | null>(null)

  // ── Drift banner (T19 WS-fed, kind=onboarding_drift_detected) ─────────────
  // Fail-honest: never show unless a real, non-empty drift message arrived
  // for THIS product_id.
  const [drifts, setDrifts] = useState<Array<{
    question_key: string
    previous: string
    current: string
  }>>([])

  useEffect(() => {
    subscribe('alerts')
    return () => unsubscribe('alerts')
  }, [subscribe, unsubscribe])

  useEffect(() => {
    if (!lastMessage || lastMessage.channel !== 'alerts') return
    const payload = lastMessage.data as OnboardingDriftDetected | null
    if (!payload || payload.kind !== 'onboarding_drift_detected') return
    if (payload.product_id !== product.id) return
    if (!Array.isArray(payload.drifts) || payload.drifts.length === 0) return
    setDrifts(payload.drifts)
  }, [lastMessage, product.id])

  const fetchStats = useCallback(async () => {
    setLoading(true)
    setFetchError(null)
    try {
      const [statsData, graphData, modulesData, patternsData, sectionsData] = await Promise.all([
        get<{ modules: number; patterns: number; last_updated?: string }>(
          `/admin/products/codex-stats?product_id=${product.id}`
        ),
        get<GraphResponse>(`/codex/graph?product_id=${product.id}`).catch(() => null),
        get<CodexModule[]>(`/codex/modules?product_id=${product.id}`).catch(() => [] as CodexModule[]),
        get<CodexPattern[]>(`/codex/patterns?product_id=${product.id}`).catch(() => [] as CodexPattern[]),
        get<CodexSection[]>(`/codex/sections?product_id=${product.id}`).catch(() => [] as CodexSection[]),
      ])
      setStats(statsData)
      setGraph(graphData)
      setModules(modulesData)
      setPatterns(patternsData)
      setSections(sectionsData)
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err)
      setFetchError(message)
      setStats(null)
      setGraph(null)
    } finally {
      setLoading(false)
    }
  }, [get, product.id])

  useEffect(() => { fetchStats() }, [fetchStats])

  const hasContent = stats !== null && (stats.modules > 0 || stats.patterns > 0)

  // Graph-derived: risk breakdown + top load-bearing modules (by dependent count).
  // Only compute when we actually have graph data.
  const internalNodes = (graph?.nodes ?? []).filter(n => !n.external)
  const riskCounts: Record<RiskLevel, number> = { low: 0, medium: 0, high: 0, unknown: 0 }
  for (const n of internalNodes) riskCounts[n.risk_level] = (riskCounts[n.risk_level] ?? 0) + 1
  const topModules = [...internalNodes]
    .sort((a, b) => (b.dependents_count - a.dependents_count) || a.name.localeCompare(b.name))
    .slice(0, 10)

  return (
    <div className="space-y-6">
      <div>
        <h2 className="text-base font-semibold text-fg">Codex Review</h2>
        <p className="mt-1 text-sm text-muted">
          Review the discovered modules and patterns before approving the codex.
        </p>
      </div>

      {/* Drift banner — only rendered when REAL drifts arrive for this product */}
      {drifts.length > 0 && (
        <div
          role="alert"
          className="rounded-sm border border-amber-600 bg-amber-900/20 p-4 space-y-2"
        >
          <h3 className="text-sm font-semibold text-amber-200">
            Codex drift detected for {drifts.length} question
            {drifts.length === 1 ? '' : 's'}
          </h3>
          <p className="text-xs text-amber-100/90">
            The latest scan disagrees with your previously confirmed answers for:{' '}
            <span className="font-mono text-amber-200">
              {drifts.map(d => d.question_key).join(', ')}
            </span>
            . Review?
          </p>
          <ul className="space-y-1 text-xs text-muted max-h-40 overflow-y-auto">
            {drifts.map((d, i) => (
              <li key={i} className="font-mono">
                <span className="text-amber-200">{d.question_key}</span>:{' '}
                was &quot;{(d.previous ?? '').slice(0, 60)}
                {(d.previous ?? '').length > 60 ? '…' : ''}&quot; → now &quot;
                {(d.current ?? '').slice(0, 60)}
                {(d.current ?? '').length > 60 ? '…' : ''}&quot;
              </li>
            ))}
          </ul>
          {onBack && (
            <button
              type="button"
              onClick={onBack}
              className="rounded-sm border border-amber-600 bg-amber-900/40 px-3 py-1 text-xs font-medium text-amber-100 hover:bg-amber-800/60"
            >
              Review answers
            </button>
          )}
        </div>
      )}
      {!isConnected && (
        <p className="text-[11px] text-dim">
          <span className="inline-block h-2 w-2 rounded-full bg-surface-700 animate-pulse mr-2" />
          Drift alerts stream connecting…
        </p>
      )}

      {loading ? (
        <div className="flex h-20 items-center justify-center">
          <div className="h-6 w-6 animate-spin rounded-full border-2 border-accent border-t-transparent" />
        </div>
      ) : (
        <>
          {/* High-level counts */}
          <div className="grid grid-cols-4 gap-px rounded-sm overflow-hidden bg-surface-800 border border-rule">
            <StatTile label="Modules"  value={stats?.modules ?? 0} />
            <StatTile label="Patterns" value={stats?.patterns ?? 0} tone={stats?.patterns === 0 ? 'warn' : undefined} />
            <StatTile label="Edges"    value={graph?.meta.edge_count ?? 0} />
            <StatTile label="External" value={(graph?.nodes ?? []).filter(n => n.external).length} />
          </div>

          {/* Risk distribution */}
          {internalNodes.length > 0 && (
            <div className="rounded-sm border border-rule bg-surface-900 p-4 space-y-2">
              <p className="text-sm font-medium text-muted">Risk breakdown</p>
              <RiskBar counts={riskCounts} total={internalNodes.length} />
              <div className="flex flex-wrap gap-3 text-xs text-muted">
                {(['high','medium','low','unknown'] as RiskLevel[]).map(r => (
                  <span key={r}>
                    <span className={`inline-block h-2 w-2 rounded-full mr-1 ${riskDot(r)}`} />
                    {r}: <span className="text-fg font-mono">{riskCounts[r]}</span>
                  </span>
                ))}
              </div>
            </div>
          )}

          {/* Load-bearing modules — what breaks if each one moves */}
          {topModules.length > 0 && (
            <div className="rounded-sm border border-rule bg-surface-900 overflow-hidden">
              <div className="flex items-center justify-between px-4 py-2 border-b border-rule">
                <p className="text-sm font-medium text-muted">
                  Top {topModules.length} load-bearing modules
                </p>
                <a href={`/codex?product_id=${product.id}`} target="_blank" rel="noreferrer"
                   className="text-xs text-accent hover:text-accent">
                  Open full codex browser →
                </a>
              </div>
              <table className="w-full text-sm">
                <thead>
                  <tr className="bg-surface-950/40 text-[10px] uppercase tracking-wide text-dim">
                    <th className="text-left font-mono px-3 py-2">Module</th>
                    <th className="text-left font-mono px-3 py-2">Risk</th>
                    <th className="text-left font-mono px-3 py-2">Purpose</th>
                    <th className="text-right font-mono px-3 py-2">Deps</th>
                    <th className="text-right font-mono px-3 py-2">Dependents</th>
                  </tr>
                </thead>
                <tbody>
                  {topModules.map(m => (
                    <tr key={m.id} className="border-t border-rule">
                      <td className="px-3 py-2 font-mono text-xs text-fg truncate max-w-[180px]">{m.name}</td>
                      <td className="px-3 py-2">
                        <span className={`inline-flex items-center gap-1 text-[10px] uppercase tracking-wide ${riskTextClass(m.risk_level)}`}>
                          <span className={`inline-block h-1.5 w-1.5 rounded-full ${riskDot(m.risk_level)}`} />
                          {m.risk_level}
                        </span>
                      </td>
                      <td className="px-3 py-2 text-xs text-muted truncate max-w-[320px]">
                        {m.purpose ? m.purpose.split('.')[0] : <span className="italic text-dim">—</span>}
                      </td>
                      <td className="px-3 py-2 text-right font-mono text-xs text-muted">{m.dependencies_count}</td>
                      <td className="px-3 py-2 text-right font-mono text-xs text-fg">{m.dependents_count}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
          {/* Discovered Modules — collapsible full list */}
          {modules.length > 0 && (
            <div className="rounded-sm border border-rule bg-surface-900 overflow-hidden">
              <button
                type="button"
                onClick={() => setShowModules(v => !v)}
                className="flex w-full items-center justify-between px-4 py-3 text-left hover:bg-surface-800/50"
              >
                <p className="text-sm font-medium text-muted">
                  Discovered Modules ({modules.length})
                </p>
                <span className="text-xs text-dim">{showModules ? '▲ collapse' : '▼ expand'}</span>
              </button>
              {showModules && (
                <div className="border-t border-rule max-h-80 overflow-y-auto">
                  {modules.map(m => (
                    <div key={m.id} className="border-b border-rule px-4 py-3 last:border-b-0">
                      <div className="flex items-center gap-2">
                        <span className="font-mono text-sm text-fg">{m.name}</span>
                        <span className={`rounded-sm border px-1.5 py-0.5 text-[10px] uppercase tracking-wide ${
                          m.risk_level === 'high' ? 'border-red-500/30 text-red-400' :
                          m.risk_level === 'medium' ? 'border-amber-500/30 text-amber-400' :
                          'border-emerald-500/30 text-emerald-400'
                        }`}>{m.risk_level}</span>
                        {m.dependencies?.length > 0 && (
                          <span className="text-[10px] text-dim">{m.dependencies.length} deps</span>
                        )}
                        {m.dependents?.length > 0 && (
                          <span className="text-[10px] text-dim">{m.dependents.length} dependents</span>
                        )}
                      </div>
                      {m.purpose && (
                        <p className="mt-1 text-xs text-muted line-clamp-2">{m.purpose}</p>
                      )}
                    </div>
                  ))}
                </div>
              )}
            </div>
          )}

          {/* Discovered Patterns — collapsible full list */}
          {patterns.length > 0 && (
            <div className="rounded-sm border border-rule bg-surface-900 overflow-hidden">
              <button
                type="button"
                onClick={() => setShowPatterns(v => !v)}
                className="flex w-full items-center justify-between px-4 py-3 text-left hover:bg-surface-800/50"
              >
                <p className="text-sm font-medium text-muted">
                  Discovered Patterns ({patterns.length})
                </p>
                <span className="text-xs text-dim">{showPatterns ? '▲ collapse' : '▼ expand'}</span>
              </button>
              {showPatterns && (
                <div className="border-t border-rule max-h-80 overflow-y-auto">
                  {patterns.map(p => (
                    <div key={p.id} className="border-b border-rule px-4 py-3 last:border-b-0">
                      <div className="flex items-center gap-2">
                        <span className="text-sm font-medium text-fg">{p.name}</span>
                        <span className="rounded-sm border border-rule bg-surface-950 px-1.5 py-0.5 font-mono text-[10px] text-muted">
                          {p.category}
                        </span>
                      </div>
                      {p.when_to_use && (
                        <p className="mt-1 text-xs text-muted line-clamp-2">{p.when_to_use}</p>
                      )}
                      {p.key_rules?.length > 0 && (
                        <ul className="mt-1 space-y-0.5">
                          {p.key_rules.slice(0, 3).map((rule, i) => (
                            <li key={i} className="text-xs text-dim">• {rule}</li>
                          ))}
                          {p.key_rules.length > 3 && (
                            <li className="text-xs text-dim">…and {p.key_rules.length - 3} more</li>
                          )}
                        </ul>
                      )}
                    </div>
                  ))}
                </div>
              )}
            </div>
          )}

          {/* Codebase Insights — deep analysis results from LLM */}
          {sections.filter(s => {
            const c = s.content as Record<string, string> | null
            return c && typeof c === 'object' && 'source' in c && c.source === 'deep_analysis'
          }).length > 0 && (
            <div className="rounded-sm border border-rule bg-surface-900 overflow-hidden">
              <button
                type="button"
                onClick={() => setShowInsights(v => !v)}
                className="flex w-full items-center justify-between px-4 py-3 text-left hover:bg-surface-800/50"
              >
                <p className="text-sm font-medium text-accent">
                  Codebase Insights ({sections.filter(s => {
                    const c = s.content as Record<string, string> | null
                    return c && typeof c === 'object' && 'source' in c && c.source === 'deep_analysis'
                  }).length})
                </p>
                <span className="text-xs text-dim">{showInsights ? '▲ collapse' : '▼ expand'}</span>
              </button>
              {showInsights && (
                <div className="border-t border-rule divide-y divide-rule max-h-[32rem] overflow-y-auto">
                  {sections
                    .filter(s => {
                      const c = s.content as Record<string, string> | null
                      return c && typeof c === 'object' && 'source' in c && c.source === 'deep_analysis'
                    })
                    .map(s => {
                      const c = s.content as Record<string, string>
                      return (
                        <div key={s.id} className="px-4 py-3">
                          <div className="flex items-center gap-2 mb-1">
                            <span className="text-sm font-medium text-fg">{s.name}</span>
                            <span className="rounded-sm border border-accent/20 bg-accent/10 px-1.5 py-0.5 font-mono text-[10px] text-accent-dim">
                              {s.section_type}
                            </span>
                          </div>
                          <p className="text-xs text-muted whitespace-pre-wrap leading-relaxed">
                            {c.analysis}
                          </p>
                        </div>
                      )
                    })}
                </div>
              )}
            </div>
          )}
        </>
      )}

      <div className="rounded-sm border border-rule bg-surface-900 p-4">
        <p className="text-sm font-medium text-muted mb-2">Repositories analyzed:</p>
        <div className="space-y-1">
          {repos.map(r => (
            <p key={r.id} className="text-sm text-muted">
              <span className="text-accent">{r.name}</span> — {r.github_url}
            </p>
          ))}
        </div>
        {graph?.generated_at && (
          <p className="mt-2 text-xs text-dim">
            Last indexed {new Date(graph.generated_at).toLocaleString()}
          </p>
        )}
      </div>

      {/* Quality signals — honest callouts when something looks off */}
      {!loading && !fetchError && stats && stats.patterns === 0 && stats.modules > 0 && (
        <div className="rounded-sm border border-amber-700/50 bg-amber-900/10 px-4 py-3">
          <p className="text-sm font-medium text-amber-300">No coding patterns detected</p>
          <p className="mt-1 text-xs text-amber-300/80">
            The pattern extractor ran against {stats.modules} module(s) and found zero repeated conventions.
            That's unusual for a codebase of this size. Most likely: the language is not deeply supported by our extractors,
            or the files are too diverse to cluster. Agents will still work — they'll just have less codex context to lean on.
          </p>
        </div>
      )}

      {!loading && !fetchError && graph && graph.meta.truncated && (
        <div className="rounded-sm border border-amber-700/50 bg-amber-900/10 px-4 py-3">
          <p className="text-sm font-medium text-amber-300">Graph truncated</p>
          <p className="mt-1 text-xs text-amber-300/80">
            Showing top {graph.nodes.length} of {graph.meta.node_count}+ modules by connectedness.
            Use the full Codex Browser to see everything.
          </p>
        </div>
      )}

      {fetchError && !loading && (
        <div className="rounded-sm border border-red-700/50 bg-red-900/15 px-4 py-3 space-y-2">
          <p className="text-sm font-semibold text-red-300">Could not load Codex stats</p>
          <p className="text-xs text-red-200 font-mono break-all">{fetchError}</p>
          <p className="text-xs text-red-300/80">
            This is a backend or connectivity failure — not a sign that analysis worked.
            Check <span className="font-mono">docker compose logs dashboard-backend</span> and <span className="font-mono">docker compose logs knowledge-indexer</span>.
          </p>
          <button
            onClick={fetchStats}
            className="mt-1 rounded-sm border border-red-700 px-3 py-1 text-xs text-red-200 hover:bg-red-900/40"
          >
            Retry
          </button>
        </div>
      )}

      {!fetchError && !hasContent && !loading && (
        <div className="rounded-sm border border-amber-700/50 bg-amber-900/10 px-4 py-3">
          <p className="text-sm text-amber-300">
            Codex has 0 modules and 0 patterns for this product yet.
          </p>
          <p className="mt-1 text-xs text-amber-300/80">
            If analysis is still running, results will appear once it finishes.
            You can skip this step and review the codex later from the Codex tab.
          </p>
        </div>
      )}

      <div className="flex items-center gap-4">
        <button
          onClick={onNext}
          className="rounded bg-accent px-4 py-2 text-sm font-medium text-surface-900 hover:bg-accent-hover"
        >
          {hasContent ? 'Approve Codex' : 'Skip for Now'}
        </button>
      </div>
    </div>
  )
}

// ─── Codex Review helpers ─────────────────────────────────────────────────────

function StatTile({ label, value, tone }: { label: string; value: number; tone?: 'warn' }) {
  const color = tone === 'warn' && value === 0 ? 'text-amber-300' : 'text-accent-dim'
  return (
    <div className="bg-surface-900 p-5 text-center">
      <p className={`text-3xl font-bold ${color}`}>{value}</p>
      <p className="mt-1 text-xs uppercase tracking-wide text-dim font-mono">{label}</p>
    </div>
  )
}

function RiskBar({ counts, total }: { counts: Record<RiskLevel, number>; total: number }) {
  if (total === 0) return null
  const order: RiskLevel[] = ['high', 'medium', 'low', 'unknown']
  return (
    <div className="flex h-2 rounded overflow-hidden">
      {order.map(r => {
        const pct = (counts[r] / total) * 100
        if (pct === 0) return null
        return (
          <div
            key={r}
            className={riskBg(r)}
            style={{ width: `${pct}%` }}
            title={`${r}: ${counts[r]} (${pct.toFixed(0)}%)`}
          />
        )
      })}
    </div>
  )
}

function riskBg(r: RiskLevel): string {
  switch (r) {
    case 'high':    return 'bg-red-500'
    case 'medium':  return 'bg-amber-500'
    case 'low':     return 'bg-emerald-500'
    case 'unknown': return 'bg-dim'
  }
}
function riskDot(r: RiskLevel): string {
  switch (r) {
    case 'high':    return 'bg-red-400'
    case 'medium':  return 'bg-amber-400'
    case 'low':     return 'bg-emerald-400'
    case 'unknown': return 'bg-muted'
  }
}
function riskTextClass(r: RiskLevel): string {
  switch (r) {
    case 'high':    return 'text-red-300'
    case 'medium':  return 'text-amber-300'
    case 'low':     return 'text-emerald-300'
    case 'unknown': return 'text-muted'
  }
}

// ─── Step 7: Validation ───────────────────────────────────────────────────────

interface ValidationStepProps {
  product: Product
}

function ValidationStep({ product }: ValidationStepProps) {
  const { post } = useApi()
  const [running, setRunning] = useState(false)
  const [results, setResults] = useState<Array<{ check: string; passed: boolean; message: string }>>([])
  const [done, setDone] = useState(false)
  const [error, setError] = useState('')

  async function handleValidate() {
    setRunning(true)
    setError('')
    try {
      // useApi prepends /api; also the backend uses query-param style not path-param.
      const data = await post<Array<{ check: string; passed: boolean; message: string }>>(
        `/admin/products/validate?product_id=${product.id}`,
        {}
      )
      if (data) { setResults(data); setDone(true) }
    } catch (e: unknown) {
      const raw = e instanceof Error ? e.message : 'Validation failed'
      // Agent A wired POST /api/onboarding/start-validation to return 412
      // Precondition Failed when approve-codex has not been called. Surface
      // that specific case with an actionable message rather than dumping
      // the raw "API POST ... → 412: ..." string, which looks like a 5xx.
      if (/→\s*412\b/.test(raw) || /precondition failed/i.test(raw)) {
        setError('Approve the Codex in Step 6 before running validation.')
      } else {
        setError(raw)
      }
    } finally {
      setRunning(false)
    }
  }

  return (
    <div className="space-y-6">
      <div>
        <h2 className="text-base font-semibold text-fg">Validation</h2>
        <p className="mt-1 text-sm text-muted">
          Run a final validation to confirm <span className="text-accent">{product.name}</span> is ready for autonomous engineering.
        </p>
      </div>

      {results.length > 0 && (
        <div className="space-y-2">
          {results.map((r, i) => (
            <div key={i} className={[
              'flex items-start gap-3 rounded-sm border px-4 py-3',
              r.passed ? 'border-emerald-700/50 bg-emerald-900/10' : 'border-red-700/50 bg-red-900/10',
            ].join(' ')}>
              <span className={r.passed ? 'text-emerald-400' : 'text-red-400'}>
                {r.passed ? '✓' : '✗'}
              </span>
              <div>
                <p className={`text-sm font-medium ${r.passed ? 'text-emerald-200' : 'text-red-200'}`}>{r.check}</p>
                <p className="text-xs text-muted">{r.message}</p>
              </div>
            </div>
          ))}
        </div>
      )}

      {error && <p className="text-sm text-red-400">{error}</p>}

      {done && results.every(r => r.passed) ? (
        <div className="space-y-4">
          <div className="rounded-sm border border-emerald-700/50 bg-emerald-900/10 px-4 py-4">
            <p className="text-sm font-semibold text-emerald-300">Setup Complete</p>
            <p className="mt-1 text-sm text-muted">
              {product.name} is configured and ready. The autonomous engine will begin processing work items.
            </p>
          </div>
          <a
            href="/"
            className="inline-block rounded bg-accent px-4 py-2 text-sm font-medium text-surface-900 hover:bg-accent-hover"
          >
            Go to Dashboard
          </a>
        </div>
      ) : (
        <div className="flex items-center gap-4">
          <button
            onClick={handleValidate}
            disabled={running}
            className="rounded bg-accent px-4 py-2 text-sm font-medium text-surface-900 hover:bg-accent-hover disabled:opacity-50"
          >
            {running ? 'Validating…' : 'Run Validation'}
          </button>
          <a
            href="/"
            className="rounded border border-rule px-4 py-2 text-sm font-medium text-muted hover:text-fg hover:border-sharp"
          >
            Finish Later
          </a>
        </div>
      )}
    </div>
  )
}

// ─── Step definitions ─────────────────────────────────────────────────────────

type OnboardingPhase =
  | 'product'
  | 'credentials'
  | 'repositories'
  | 'verification'
  | 'dependencies'
  | 'analysis'
  | 'conversation'
  | 'codex'
  | 'zones'
  | 'validation'

const STEPS: Array<{ key: OnboardingPhase; label: string }> = [
  { key: 'product', label: 'Product' },
  { key: 'credentials', label: 'Credentials' },
  { key: 'repositories', label: 'Repositories' },
  { key: 'verification', label: 'Verification' },
  { key: 'dependencies', label: 'Dependencies' },
  { key: 'analysis', label: 'Analysis' },
  { key: 'conversation', label: 'Conversation' },
  { key: 'codex', label: 'Codex' },
  { key: 'zones', label: 'Zones' },
  { key: 'validation', label: 'Validation' },
]

// ─── Wizard Shell ─────────────────────────────────────────────────────────────

export default function OnboardingWizard() {
  const [searchParams] = useSearchParams()
  const initialProductId = searchParams.get('product_id')

  const [phase, setPhase] = useState<OnboardingPhase>('product')
  const [selectedProduct, setSelectedProduct] = useState<Product | null>(null)
  const [repos, setRepos] = useState<Repository[]>([])

  const currentIndex = STEPS.findIndex(s => s.key === phase)

  const handleProductSelected = useCallback((product: Product) => {
    setSelectedProduct(product)
    setPhase('credentials')
  }, [])

  return (
    <div className="fixed inset-0 z-40 flex items-start justify-center overflow-y-auto bg-black/60 backdrop-blur-sm p-6">
      <div className="my-8 w-full max-w-2xl rounded-sm border border-rule bg-surface-950 shadow-2xl">
        {/* Header */}
        <div className="flex items-start justify-between border-b border-rule px-8 py-5">
          <div>
            <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32" className="h-8 w-8">
              <rect width="32" height="32" rx="3" fill="#0a0c10"/>
              <rect x="4" y="4" width="24" height="24" rx="1" fill="none" stroke="#c8fc5b" strokeWidth="1.25"/>
              <text x="14.5" y="20" fontFamily="Geist Mono,ui-monospace,monospace" fontSize="11" fontWeight="500" fill="#c8fc5b" textAnchor="middle" letterSpacing="0.5">AE</text>
              <polygon points="25.5,23.7 26.1,24.5 27.1,24.6 26.7,25.5 27.1,26.4 26.1,26.5 25.5,27.3 24.9,26.5 23.9,26.4 24.3,25.5 23.9,24.6 24.9,24.5" fill="#c8fc5b"/>
              <circle cx="25.5" cy="25.5" r="0.55" fill="#0a0c10"/>
            </svg>
            <h1 className="mt-3 text-lg font-semibold text-fg">Autonomous Engine Setup</h1>
            <p className="mt-0.5 text-sm text-muted">
              {selectedProduct ? (
                <>Setting up <span className="text-accent">{selectedProduct.name}</span></>
              ) : (
                'Complete the following steps to get started.'
              )}
            </p>
          </div>
          <a
            href="/admin/products"
            className="rounded-sm border border-rule px-2.5 py-1 text-xs text-muted hover:border-sharp hover:text-fg"
            title="Close and return to products"
          >
            ✕ Close
          </a>
        </div>

        {/* Step indicators */}
        <div className="flex overflow-x-auto border-b border-rule">
          {STEPS.map((step, idx) => (
            <button
              key={step.key}
              onClick={() => { if (idx <= currentIndex && selectedProduct) setPhase(step.key) }}
              disabled={idx > currentIndex || !selectedProduct}
              className={[
                'flex flex-1 min-w-0 items-center justify-center gap-1.5 py-3 text-xs',
                idx < currentIndex ? 'text-accent-dim cursor-pointer hover:text-accent' : idx === currentIndex ? 'text-fg' : 'text-dim cursor-default',
              ].join(' ')}
            >
              <span className={[
                'flex h-5 w-5 shrink-0 items-center justify-center rounded-full text-xs font-bold',
                idx < currentIndex ? 'bg-accent text-surface-900' : idx === currentIndex ? 'bg-accent text-surface-900' : 'bg-surface-700 text-muted',
              ].join(' ')}>
                {idx < currentIndex ? '✓' : idx + 1}
              </span>
              <span className="hidden sm:inline truncate">{step.label}</span>
            </button>
          ))}
        </div>

        {/* Step content */}
        <div className="px-8 py-6">
          {phase === 'product' && (
            <SelectProductStep
              initialProductId={initialProductId}
              onProductSelected={handleProductSelected}
            />
          )}
          {phase === 'credentials' && (
            <CredentialsStep onNext={() => setPhase('repositories')} />
          )}
          {phase === 'repositories' && selectedProduct && (
            <AddRepositoriesStep
              product={selectedProduct}
              onNext={(r) => { setRepos(r); setPhase('verification') }}
            />
          )}
          {phase === 'verification' && selectedProduct && (
            <VerificationPolicyStep
              repos={repos}
              onNext={() => setPhase('dependencies')}
            />
          )}
          {phase === 'dependencies' && selectedProduct && (
            <MapDependenciesStep
              product={selectedProduct}
              repos={repos}
              onNext={() => setPhase('analysis')}
            />
          )}
          {phase === 'analysis' && selectedProduct && (
            <CodebaseAnalysisStep
              product={selectedProduct}
              repos={repos}
              onNext={() => setPhase('conversation')}
            />
          )}
          {phase === 'conversation' && selectedProduct && (
            <InteractiveConversationStep
              product={selectedProduct}
              repos={repos}
              onNext={() => setPhase('codex')}
            />
          )}
          {phase === 'codex' && selectedProduct && (
            <CodexReviewStep
              product={selectedProduct}
              repos={repos}
              onNext={() => setPhase('zones')}
              onBack={() => setPhase('conversation')}
            />
          )}
          {phase === 'zones' && selectedProduct && (
            <ZonesStep
              productId={selectedProduct.id}
              onNext={() => setPhase('validation')}
            />
          )}
          {phase === 'validation' && selectedProduct && (
            <ValidationStep product={selectedProduct} />
          )}
        </div>
      </div>
    </div>
  )
}
