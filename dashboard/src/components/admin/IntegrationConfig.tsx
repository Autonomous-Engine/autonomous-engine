import { useState, useEffect, useCallback } from 'react'
import { useApi } from '@/hooks/useApi'
import type { CredentialEntry, GitHubAccount } from '@/types'

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

interface JiraConfig {
  url: string
  email: string
  api_token: string
  project_key: string
}

interface JiraIngestConfig {
  enabled: boolean
  jql_filter: string
  polling_interval: number
  ingest_label: string
}

function defaultJira(): JiraConfig {
  return { url: '', email: '', api_token: '', project_key: '' }
}
function defaultJiraIngest(): JiraIngestConfig {
  return { enabled: false, jql_filter: '', polling_interval: 120, ingest_label: 'agent-ingested' }
}

// What /admin/credentials returns in place of a secret value (crypto.MaskValue).
const MASKED_VALUE = '••••••••'

function credVal(creds: CredentialEntry[], key: string): string {
  return creds.find(c => c.key_name === key)?.value ?? ''
}

export default function IntegrationConfig() {
  const { get, post, put, del } = useApi()
  const [githubOpen, setGithubOpen] = useState(true)
  const [jiraOpen, setJiraOpen] = useState(true)

  const [accounts, setAccounts] = useState<GitHubAccount[]>([])
  const [addingAccount, setAddingAccount] = useState(false)
  const [newToken, setNewToken] = useState('')
  const [newLabel, setNewLabel] = useState('')
  const [showNewToken, setShowNewToken] = useState(false)
  const [addError, setAddError] = useState('')
  const [addSaving, setAddSaving] = useState(false)
  const [testingId, setTestingId] = useState<string | null>(null)
  const [webhookSecret, setWebhookSecret] = useState('')
  const [webhookSaving, setWebhookSaving] = useState(false)
  const [hasLegacyToken, setHasLegacyToken] = useState(false)

  const [jira, setJira] = useState<JiraConfig>(defaultJira())
  const [jiraIngest, setJiraIngest] = useState<JiraIngestConfig>(defaultJiraIngest())

  const [showWebhookSecret, setShowWebhookSecret] = useState(false)
  const [showJiraToken, setShowJiraToken] = useState(false)

  const [jiraSaving, setJiraSaving] = useState(false)
  const [jiraTesting, setJiraTesting] = useState(false)
  const [jiraTestResult, setJiraTestResult] = useState<string | null>(null)
  const [jiraError, setJiraError] = useState('')

  // Jira ingest state
  const [ingestSaving, setIngestSaving] = useState(false)
  const [ingestSaveResult, setIngestSaveResult] = useState<string | null>(null)
  const [ingestTicketCount, setIngestTicketCount] = useState<number | null>(null)

  // Jira re-discovery state
  const [rediscovering, setRediscovering] = useState(false)
  const [rediscoveryResult, setRediscoveryResult] = useState<JiraDiscoveryResult | null>(null)
  const [rediscoveryError, setRediscoveryError] = useState('')
  const [rediscoveryStatusMapping, setRediscoveryStatusMapping] = useState<Record<string, string>>({})
  const [rediscoveryIssueTypeMap, setRediscoveryIssueTypeMap] = useState<Record<string, string>>({})
  const [rediscoveryPriorityMap, setRediscoveryPriorityMap] = useState<Record<string, string>>({})
  const [rediscoveryJQL, setRediscoveryJQL] = useState('')
  const [rediscoveryConfirming, setRediscoveryConfirming] = useState(false)
  const [rediscoverySaved, setRediscoverySaved] = useState(false)

  const fetchConfig = useCallback(async () => {
    try {
      const accts = await get<GitHubAccount[]>('/admin/github/accounts')
      if (accts) {
        setAccounts(accts)
        if (accts.length === 0) {
          const ghCreds = await get<CredentialEntry[]>('/admin/credentials?category=github')
          if (ghCreds?.some(c => c.key_name === 'token' && c.value && c.value !== MASKED_VALUE)) {
            setHasLegacyToken(true)
          }
        }
      }
    } catch { /* ignore */ }

    try {
      const ghCreds = await get<CredentialEntry[]>('/admin/credentials?category=github')
      if (ghCreds?.length) {
        setWebhookSecret(credVal(ghCreds, 'webhook_secret'))
      }
    } catch { /* ignore */ }

    try {
      const jiraCreds = await get<CredentialEntry[]>('/admin/credentials?category=jira')
      if (jiraCreds?.length) {
        setJira({
          url: credVal(jiraCreds, 'url'),
          email: credVal(jiraCreds, 'email'),
          api_token: credVal(jiraCreds, 'api_token'),
          project_key: credVal(jiraCreds, 'project_key'),
        })
      }
    } catch { /* ignore */ }

    try {
      const ingestCfg = await get<Record<string, string>>('/admin/config/jira-ingest')
      if (ingestCfg) {
        setJiraIngest({
          enabled: ingestCfg.ingest_enabled === 'true',
          jql_filter: ingestCfg.ingest_jql ?? '',
          polling_interval: ingestCfg.polling_interval ? parseInt(ingestCfg.polling_interval, 10) : 120,
          ingest_label: ingestCfg.ingest_label || 'agent-ingested',
        })
      }
    } catch { /* ignore */ }

    try {
      const countResp = await get<{ count: number }>('/admin/jira/ingested-count')
      if (countResp) setIngestTicketCount(countResp.count)
    } catch { /* ignore */ }
  }, [get])

  useEffect(() => { fetchConfig() }, [fetchConfig])

  const [migrating, setMigrating] = useState(false)
  const [migrateError, setMigrateError] = useState('')

  async function migrateLegacy() {
    setMigrating(true)
    setMigrateError('')
    try {
      const acct = await post<GitHubAccount & { linked_repos: number }>('/admin/github/accounts/migrate-legacy', {})
      if (acct) {
        setAccounts(prev => [...prev, acct])
        setHasLegacyToken(false)
      }
    } catch (e: unknown) {
      setMigrateError(e instanceof Error ? e.message : 'Migration failed')
    } finally {
      setMigrating(false)
    }
  }

  async function addAccount() {
    if (!newToken.trim()) { setAddError('Token is required'); return }
    setAddSaving(true)
    setAddError('')
    try {
      const acct = await post<GitHubAccount>('/admin/github/accounts/create', { token: newToken.trim(), label: newLabel.trim() })
      if (acct) {
        setAccounts(prev => [...prev, acct])
        setNewToken('')
        setNewLabel('')
        setAddingAccount(false)
        setHasLegacyToken(false)
      }
    } catch (e: unknown) {
      setAddError(e instanceof Error ? e.message : 'Failed to add account')
    } finally {
      setAddSaving(false)
    }
  }

  async function removeAccount(id: string) {
    try {
      await del('/admin/github/accounts/delete?id=' + id)
      setAccounts(prev => prev.filter(a => a.id !== id))
    } catch { /* ignore */ }
  }

  async function testAccount(id: string) {
    setTestingId(id)
    try {
      const result = await post<{ valid: boolean; username: string; error: string }>('/admin/github/accounts/test?id=' + id, {})
      if (result) {
        setAccounts(prev => prev.map(a => a.id === id ? { ...a, valid: result.valid, last_validated_at: new Date().toISOString() } : a))
      }
    } catch { /* ignore */ }
    finally { setTestingId(null) }
  }

  async function saveWebhookSecret() {
    // The list endpoint returns secrets masked; saving the mask back would
    // replace the real secret with it and every delivery would then fail.
    if (webhookSecret === '' || webhookSecret === MASKED_VALUE) return
    setWebhookSaving(true)
    try {
      await post('/admin/credentials/create', {
        scope: 'global', scope_target: 'default',
        key_name: 'webhook_secret', value: webhookSecret,
        category: 'github', is_secret: true, description: 'GitHub webhook_secret',
      })
    } catch { /* ignore */ }
    finally { setWebhookSaving(false) }
  }

  async function saveJira() {
    setJiraSaving(true)
    setJiraError('')
    try {
      const fields: Array<[string, string, boolean]> = [
        ['url', jira.url, false],
        ['email', jira.email, false],
        ['api_token', jira.api_token, true],
        ['project_key', jira.project_key, false],
      ]
      // See note above re: scope/scope_target — store at global/default so
      // ResolveCredential finds it; the category field already disambiguates jira.
      await Promise.all(fields.map(([key, value, is_secret]) =>
        post('/admin/credentials/create', {
          scope: 'global',
          scope_target: 'default',
          key_name: key,
          value,
          category: 'jira',
          is_secret,
          description: `Jira ${key}`,
        })
      ))
    } catch (e: unknown) {
      setJiraError(e instanceof Error ? e.message : 'Save failed')
    } finally {
      setJiraSaving(false)
    }
  }

  async function testJira() {
    setJiraTesting(true)
    setJiraTestResult(null)
    try {
      await post('/admin/credentials/test', { category: 'jira' })
      setJiraTestResult('Connection successful')
    } catch (e: unknown) {
      setJiraTestResult(e instanceof Error ? e.message : 'Connection failed')
    } finally {
      setJiraTesting(false)
    }
  }

  async function saveIngestConfig() {
    setIngestSaving(true)
    setIngestSaveResult(null)
    try {
      // GetConfig's resolver chain only reaches global/default, so jira ingest
      // config must live there (category=jira disambiguates it from other keys).
      await put('/admin/config/update', [
        { key_name: 'ingest_enabled', value: String(jiraIngest.enabled), value_type: 'bool', category: 'jira', scope: 'global', scope_target: 'default' },
        { key_name: 'ingest_jql', value: jiraIngest.jql_filter, value_type: 'string', category: 'jira', scope: 'global', scope_target: 'default' },
        { key_name: 'polling_interval', value: String(jiraIngest.polling_interval), value_type: 'int', category: 'jira', scope: 'global', scope_target: 'default' },
        { key_name: 'ingest_label', value: jiraIngest.ingest_label, value_type: 'string', category: 'jira', scope: 'global', scope_target: 'default' },
      ])
      setIngestSaveResult('Ingest config saved.')
    } catch (e: unknown) {
      setIngestSaveResult(e instanceof Error ? e.message : 'Save failed')
    } finally {
      setIngestSaving(false)
    }
  }

  async function handleRediscover() {
    if (!jira.project_key) {
      setRediscoveryError('Configure a project key first.')
      return
    }
    setRediscovering(true)
    setRediscoveryError('')
    setRediscoveryResult(null)
    setRediscoverySaved(false)
    try {
      const result = await post<JiraDiscoveryResult>('/onboarding/discover-jira', {
        project_key: jira.project_key,
      })
      if (result) {
        setRediscoveryResult(result)
        setRediscoveryStatusMapping(result.proposed_mapping.status_mapping ?? {})
        setRediscoveryIssueTypeMap(result.proposed_mapping.issue_type_map ?? {})
        setRediscoveryPriorityMap(result.proposed_mapping.priority_map ?? {})
        setRediscoveryJQL(result.proposed_mapping.ingest_jql ?? '')
      }
    } catch (e: unknown) {
      setRediscoveryError(e instanceof Error ? e.message : 'Re-discovery failed')
    } finally {
      setRediscovering(false)
    }
  }

  async function handleRediscoveryConfirm() {
    if (!rediscoveryResult) return
    setRediscoveryConfirming(true)
    try {
      await post('/onboarding/save-jira-mapping', {
        status_mapping: rediscoveryStatusMapping,
        issue_type_map: rediscoveryIssueTypeMap,
        priority_map: rediscoveryPriorityMap,
        ingest_jql: rediscoveryJQL,
        ingest_label: rediscoveryResult.proposed_mapping.ingest_label,
      })
      setRediscoverySaved(true)
      // Refresh ingest JQL in the ingest config panel
      setJiraIngest(prev => ({ ...prev, jql_filter: rediscoveryJQL }))
    } catch (e: unknown) {
      setRediscoveryError(e instanceof Error ? e.message : 'Failed to save re-discovered mapping')
    } finally {
      setRediscoveryConfirming(false)
    }
  }

  function inputClass() {
    return 'w-full rounded-sm border border-rule bg-surface-950 px-3 py-2 text-sm text-fg focus:outline-none focus:ring-1 focus:ring-accent'
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

  return (
    <div className="space-y-4">
      <h1 className="text-lg font-semibold text-fg">Integration Configuration</h1>

      {/* GitHub Accounts Section */}
      <div className="rounded-sm border border-rule bg-surface-900 shadow-sm">
        <button
          onClick={() => setGithubOpen(v => !v)}
          className="flex w-full items-center justify-between px-5 py-4 text-left"
        >
          <div className="flex items-center gap-2">
            <svg className="h-5 w-5 text-muted" fill="currentColor" viewBox="0 0 24 24">
              <path fillRule="evenodd" d="M12 2C6.477 2 2 6.484 2 12.017c0 4.425 2.865 8.18 6.839 9.504.5.092.682-.217.682-.483 0-.237-.008-.868-.013-1.703-2.782.605-3.369-1.343-3.369-1.343-.454-1.158-1.11-1.466-1.11-1.466-.908-.62.069-.608.069-.608 1.003.07 1.531 1.032 1.531 1.032.892 1.53 2.341 1.088 2.91.832.092-.647.35-1.088.636-1.338-2.22-.253-4.555-1.113-4.555-4.951 0-1.093.39-1.988 1.029-2.688-.103-.253-.446-1.272.098-2.65 0 0 .84-.27 2.75 1.026A9.564 9.564 0 0112 6.844c.85.004 1.705.115 2.504.337 1.909-1.296 2.747-1.027 2.747-1.027.546 1.379.202 2.398.1 2.651.64.7 1.028 1.595 1.028 2.688 0 3.848-2.339 4.695-4.566 4.943.359.309.678.92.678 1.855 0 1.338-.012 2.419-.012 2.747 0 .268.18.58.688.482A10.019 10.019 0 0022 12.017C22 6.484 17.522 2 12 2z" clipRule="evenodd" />
            </svg>
            <span className="text-sm font-medium text-fg">GitHub Accounts</span>
            <span className="rounded-full bg-surface-700 px-2 py-0.5 text-xs text-muted">{accounts.length}</span>
          </div>
          <svg className={`h-4 w-4 text-muted transition-transform ${githubOpen ? 'rotate-180' : ''}`} fill="none" stroke="currentColor" viewBox="0 0 24 24">
            <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M19 9l-7 7-7-7" />
          </svg>
        </button>

        {githubOpen && (
          <div className="border-t border-rule px-5 py-4 space-y-4">
            {hasLegacyToken && accounts.length === 0 && (
              <div className="rounded-sm border border-amber-700/50 bg-amber-900/20 px-4 py-3">
                <p className="text-xs text-amber-300 mb-2">
                  You have a legacy global GitHub token configured. Migrate it to the new multi-account system to continue.
                </p>
                {migrateError && <p className="text-xs text-red-400 mb-2">{migrateError}</p>}
                <button
                  onClick={migrateLegacy}
                  disabled={migrating}
                  className="rounded bg-amber-700 px-3 py-1.5 text-xs font-medium text-white hover:bg-amber-600 disabled:opacity-50"
                >
                  {migrating ? 'Migrating…' : 'Migrate Account'}
                </button>
              </div>
            )}

            {accounts.length === 0 && !addingAccount && !hasLegacyToken && (
              <p className="text-xs text-muted">No GitHub accounts connected. Add one to browse and assign repositories.</p>
            )}

            {accounts.map(acct => (
              <div key={acct.id} className="flex items-center justify-between rounded-sm border border-rule bg-surface-950 px-4 py-3">
                <div className="flex items-center gap-3">
                  {acct.avatar_url ? (
                    <img src={acct.avatar_url} alt="" className="h-8 w-8 rounded-full" />
                  ) : (
                    <div className="flex h-8 w-8 items-center justify-center rounded-full bg-surface-700 text-xs text-muted">GH</div>
                  )}
                  <div>
                    <span className="text-sm font-medium text-fg">{acct.username}{acct.label && <span className="ml-1.5 rounded bg-surface-700 px-1.5 py-0.5 text-xs text-muted">{acct.label}</span>}</span>
                    <div className="flex items-center gap-2 mt-0.5">
                      {acct.valid ? (
                        <span className="rounded-full bg-emerald-900/60 px-2 py-0.5 text-xs text-emerald-300">Connected</span>
                      ) : (
                        <span className="rounded-full bg-red-900/60 px-2 py-0.5 text-xs text-red-300">Token Invalid</span>
                      )}
                      {acct.last_validated_at && (
                        <span className="text-xs text-dim">checked {new Date(acct.last_validated_at).toLocaleString()}</span>
                      )}
                    </div>
                  </div>
                </div>
                <div className="flex items-center gap-2">
                  <button
                    onClick={() => testAccount(acct.id)}
                    disabled={testingId === acct.id}
                    className="rounded-sm border border-rule px-2 py-1 text-xs text-muted hover:bg-surface-700 disabled:opacity-50"
                  >
                    {testingId === acct.id ? 'Testing...' : 'Test'}
                  </button>
                  <button
                    onClick={() => removeAccount(acct.id)}
                    className="rounded-sm border border-rule px-2 py-1 text-xs text-red-400 hover:bg-red-900/30"
                  >
                    Remove
                  </button>
                </div>
              </div>
            ))}

            {addingAccount ? (
              <div className="rounded-sm border border-rule bg-surface-950 px-4 py-3 space-y-3">
                <label className="block text-xs text-muted">Personal Access Token</label>
                <div className="flex gap-2">
                  <input
                    type={showNewToken ? 'text' : 'password'}
                    value={newToken}
                    onChange={e => setNewToken(e.target.value)}
                    placeholder="ghp_..."
                    className={inputClass()}
                    autoFocus
                  />
                  {revealBtn(showNewToken, () => setShowNewToken(v => !v))}
                </div>
                <label className="block text-xs text-muted">Label <span className="text-dim">(optional — e.g. &quot;personal&quot;, &quot;AE org&quot;)</span></label>
                <input
                  type="text"
                  value={newLabel}
                  onChange={e => setNewLabel(e.target.value)}
                  placeholder="e.g. AE org"
                  className={inputClass()}
                />
                {addError && <p className="text-xs text-red-400">{addError}</p>}
                <div className="flex gap-2">
                  <button
                    onClick={addAccount}
                    disabled={addSaving}
                    className="rounded bg-accent px-3 py-1.5 text-sm font-medium text-surface-900 hover:bg-accent-hover disabled:opacity-50"
                  >
                    {addSaving ? 'Adding...' : 'Add Account'}
                  </button>
                  <button
                    onClick={() => { setAddingAccount(false); setNewToken(''); setNewLabel(''); setAddError('') }}
                    className="rounded-sm border border-rule px-3 py-1.5 text-sm text-muted hover:bg-surface-700"
                  >
                    Cancel
                  </button>
                </div>
              </div>
            ) : (
              <button
                onClick={() => setAddingAccount(true)}
                className="flex items-center gap-2 rounded-sm border border-dashed border-rule px-4 py-2.5 text-sm text-muted hover:border-accent hover:text-accent w-full justify-center"
              >
                <svg className="h-4 w-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                  <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M12 4v16m8-8H4" />
                </svg>
                Add GitHub Account
              </button>
            )}

            <div className="border-t border-rule pt-4">
              <label className="mb-1 block text-xs text-muted">Webhook Secret</label>
              <p className="mb-2 text-xs text-dim">
                Required for GitHub webhooks: enter the same secret in the repository&apos;s webhook settings on GitHub.
                The webhook listener checks every delivery&apos;s signature against it (falling back to GITHUB_WEBHOOK_SECRET)
                and refuses all deliveries while no secret is configured. Changes apply without a restart.
              </p>
              <div className="flex gap-2">
                <input
                  type={showWebhookSecret ? 'text' : 'password'}
                  value={webhookSecret}
                  onChange={e => setWebhookSecret(e.target.value)}
                  placeholder="Webhook secret"
                  className={inputClass()}
                />
                {revealBtn(showWebhookSecret, () => setShowWebhookSecret(v => !v))}
              </div>
              <div className="mt-2">
                <button
                  onClick={saveWebhookSecret}
                  disabled={webhookSaving || webhookSecret === '' || webhookSecret === MASKED_VALUE}
                  className="rounded bg-accent px-3 py-1.5 text-sm font-medium text-surface-900 hover:bg-accent-hover disabled:opacity-50"
                >
                  {webhookSaving ? 'Saving...' : 'Save Webhook Secret'}
                </button>
              </div>
            </div>
          </div>
        )}
      </div>

      {/* Jira Section */}
      <div className="rounded-sm border border-rule bg-surface-900 shadow-sm">
        <button
          onClick={() => setJiraOpen(v => !v)}
          className="flex w-full items-center justify-between px-5 py-4 text-left"
        >
          <div className="flex items-center gap-2">
            <svg className="h-5 w-5 text-blue-400" fill="currentColor" viewBox="0 0 24 24">
              <path d="M11.571 11.429L6.857 6.714A2.571 2.571 0 0110.5 3.14l4.714 4.715a2.571 2.571 0 010 3.636l-4.714 4.714a2.571 2.571 0 01-3.636-3.635l4.707-4.707z" />
              <path d="M12.429 12.571l4.714 4.715a2.571 2.571 0 01-3.636 3.635L8.793 16.207a2.571 2.571 0 010-3.636l4.714-4.714a2.571 2.571 0 013.636 3.636l-4.714 4.714z" opacity=".5" />
            </svg>
            <span className="text-sm font-medium text-fg">Jira</span>
            {/* Jira status badge — purely informational; Jira is optional */}
            {jiraTestResult === 'Connection successful' ? (
              <span className="rounded-full bg-emerald-900/60 px-2 py-0.5 text-xs text-emerald-300">Connected</span>
            ) : jiraTestResult && jiraTestResult !== 'Connection successful' ? (
              <span className="rounded-full bg-red-900/60 px-2 py-0.5 text-xs text-red-300">Connection Error</span>
            ) : jira.url && jira.api_token ? (
              <span className="rounded-full bg-surface-700 px-2 py-0.5 text-xs text-muted">Configured</span>
            ) : (
              <span className="rounded-full bg-surface-700/40 px-2 py-0.5 text-xs text-dim">Not Configured — Optional</span>
            )}
          </div>
          <svg className={`h-4 w-4 text-muted transition-transform ${jiraOpen ? 'rotate-180' : ''}`} fill="none" stroke="currentColor" viewBox="0 0 24 24">
            <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M19 9l-7 7-7-7" />
          </svg>
        </button>

        {jiraOpen && (
          <div className="border-t border-rule px-5 py-4 space-y-6">
            {/* Optional integration note */}
            {!jira.url && (
              <div className="rounded-sm border border-rule/50 bg-surface-800/30 px-4 py-3">
                <p className="text-xs text-muted">
                  Jira integration syncs work items to your Jira instance for stakeholder visibility.
                  The system works fully without it.
                </p>
              </div>
            )}
            {/* Connection fields */}
            <div>
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
              {jiraError && <p className="mt-2 text-xs text-red-400">{jiraError}</p>}
              {jiraTestResult && (
                <p className={`mt-2 text-xs ${jiraTestResult.includes('successful') ? 'text-emerald-400' : 'text-red-400'}`}>
                  {jiraTestResult}
                </p>
              )}
              <div className="mt-4 flex gap-2">
                <button
                  onClick={testJira}
                  disabled={jiraTesting}
                  className="rounded-sm border border-rule px-3 py-1.5 text-sm text-muted hover:bg-surface-700 disabled:opacity-50"
                >
                  {jiraTesting ? 'Testing...' : 'Test Connection'}
                </button>
                <button
                  onClick={saveJira}
                  disabled={jiraSaving}
                  className="rounded bg-accent px-3 py-1.5 text-sm font-medium text-surface-900 hover:bg-accent-hover disabled:opacity-50"
                >
                  {jiraSaving ? 'Saving...' : 'Save'}
                </button>
              </div>
            </div>

            {/* Ingest subsection */}
            <div className="rounded-sm border border-rule bg-surface-950 p-4">
              <div className="mb-3 flex items-center justify-between">
                <h4 className="text-sm font-semibold text-fg">Ingest</h4>
                <div className="flex items-center gap-2">
                  {ingestTicketCount !== null && (
                    <span className="rounded-full bg-indigo-900 px-2 py-0.5 text-xs text-accent">
                      {ingestTicketCount} ingested
                    </span>
                  )}
                  {jiraIngest.enabled && (
                    <span className="rounded-full bg-emerald-900 px-2 py-0.5 text-xs text-emerald-300">
                      Ingest active
                    </span>
                  )}
                </div>
              </div>

              <div className="space-y-3">
                {/* Enable Ingest toggle */}
                <div className="flex items-center justify-between">
                  <label className="text-xs text-muted">Enable Ingest</label>
                  <button
                    type="button"
                    onClick={() => setJiraIngest(c => ({ ...c, enabled: !c.enabled }))}
                    className={[
                      'relative inline-flex h-5 w-9 items-center rounded-full transition-colors focus:outline-none focus:ring-2 focus:ring-accent focus:ring-offset-2 focus:ring-offset-surface-950',
                      jiraIngest.enabled ? 'bg-accent' : 'bg-surface-700',
                    ].join(' ')}
                    role="switch"
                    aria-checked={jiraIngest.enabled}
                  >
                    <span
                      className={[
                        'inline-block h-3.5 w-3.5 transform rounded-full bg-white transition-transform',
                        jiraIngest.enabled ? 'translate-x-4' : 'translate-x-1',
                      ].join(' ')}
                    />
                  </button>
                </div>

                {/* JQL Filter */}
                <div>
                  <label className="mb-1 block text-xs text-muted">JQL Filter</label>
                  <input
                    type="text"
                    value={jiraIngest.jql_filter}
                    onChange={e => setJiraIngest(c => ({ ...c, jql_filter: e.target.value }))}
                    placeholder="project = PROJ AND labels = agent-ingested ORDER BY created DESC"
                    className={inputClass()}
                  />
                </div>

                {/* Polling Interval */}
                <div>
                  <label className="mb-1 block text-xs text-muted">Polling Interval (seconds)</label>
                  <input
                    type="number"
                    min={10}
                    value={jiraIngest.polling_interval}
                    onChange={e => setJiraIngest(c => ({ ...c, polling_interval: parseInt(e.target.value, 10) || 120 }))}
                    className={inputClass()}
                  />
                </div>

                {/* Ingest Label */}
                <div>
                  <label className="mb-1 block text-xs text-muted">Ingest Label</label>
                  <input
                    type="text"
                    value={jiraIngest.ingest_label}
                    onChange={e => setJiraIngest(c => ({ ...c, ingest_label: e.target.value }))}
                    placeholder="agent-ingested"
                    className={inputClass()}
                  />
                </div>
              </div>

              {ingestSaveResult && (
                <p className={`mt-2 text-xs ${ingestSaveResult.includes('saved') ? 'text-emerald-400' : 'text-red-400'}`}>
                  {ingestSaveResult}
                </p>
              )}
              <div className="mt-4 flex gap-2">
                <button
                  onClick={saveIngestConfig}
                  disabled={ingestSaving}
                  className="rounded bg-accent px-3 py-1.5 text-sm font-medium text-surface-900 hover:bg-accent-hover disabled:opacity-50"
                >
                  {ingestSaving ? 'Saving...' : 'Save Ingest Config'}
                </button>
                <button
                  onClick={handleRediscover}
                  disabled={rediscovering}
                  className="flex items-center gap-2 rounded-sm border border-rule px-3 py-1.5 text-sm text-muted hover:bg-surface-700 disabled:opacity-50"
                >
                  {rediscovering && (
                    <svg className="h-3.5 w-3.5 animate-spin" fill="none" viewBox="0 0 24 24">
                      <circle className="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" strokeWidth="4" />
                      <path className="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8v8H4z" />
                    </svg>
                  )}
                  {rediscovering ? 'Discovering...' : 'Re-discover Workflow'}
                </button>
              </div>
            </div>

            {/* Re-discovery results */}
            {(rediscoveryError || rediscoveryResult) && (
              <div className="rounded-sm border border-rule bg-surface-950 p-4 space-y-4">
                {rediscoveryError && <p className="text-xs text-red-400">{rediscoveryError}</p>}
                {rediscoveryResult && (
                  <>
                    <p className="text-xs font-semibold text-muted">
                      Re-discovered: {rediscoveryResult.project.name} ({rediscoveryResult.project.key})
                    </p>

                    {/* Status Mapping */}
                    <div>
                      <h5 className="mb-2 text-xs font-semibold text-muted">Status Mapping</h5>
                      <table className="w-full text-xs">
                        <thead>
                          <tr className="border-b border-rule">
                            <th className="pb-1 text-left text-dim">Internal State</th>
                            <th className="pb-1 text-left text-dim">Jira Status</th>
                          </tr>
                        </thead>
                        <tbody>
                          {Object.keys(rediscoveryStatusMapping).map(internal => (
                            <tr key={internal} className="border-b border-rule">
                              <td className="py-1 pr-3 text-muted">{internal}</td>
                              <td className="py-1">
                                <select
                                  value={rediscoveryStatusMapping[internal] ?? ''}
                                  onChange={e => setRediscoveryStatusMapping(m => ({ ...m, [internal]: e.target.value }))}
                                  className="w-full rounded-sm border border-rule bg-surface-900 px-2 py-0.5 text-xs text-fg focus:outline-none focus:ring-1 focus:ring-accent"
                                >
                                  <option value="">— none —</option>
                                  {rediscoveryResult.project.statuses.map(s => (
                                    <option key={s.id} value={s.name}>{s.name}</option>
                                  ))}
                                </select>
                              </td>
                            </tr>
                          ))}
                        </tbody>
                      </table>
                    </div>

                    {/* Issue Type Mapping */}
                    <div>
                      <h5 className="mb-2 text-xs font-semibold text-muted">Issue Type Mapping</h5>
                      <table className="w-full text-xs">
                        <thead>
                          <tr className="border-b border-rule">
                            <th className="pb-1 text-left text-dim">Internal Type</th>
                            <th className="pb-1 text-left text-dim">Jira Issue Type</th>
                          </tr>
                        </thead>
                        <tbody>
                          {Object.keys(rediscoveryIssueTypeMap).map(internal => (
                            <tr key={internal} className="border-b border-rule">
                              <td className="py-1 pr-3 text-muted">{internal}</td>
                              <td className="py-1">
                                <select
                                  value={rediscoveryIssueTypeMap[internal] ?? ''}
                                  onChange={e => setRediscoveryIssueTypeMap(m => ({ ...m, [internal]: e.target.value }))}
                                  className="w-full rounded-sm border border-rule bg-surface-900 px-2 py-0.5 text-xs text-fg focus:outline-none focus:ring-1 focus:ring-accent"
                                >
                                  <option value="">— none —</option>
                                  {rediscoveryResult.project.issue_types.map(t => (
                                    <option key={t.id} value={t.name}>{t.name}</option>
                                  ))}
                                </select>
                              </td>
                            </tr>
                          ))}
                        </tbody>
                      </table>
                    </div>

                    {/* Priority Mapping */}
                    <div>
                      <h5 className="mb-2 text-xs font-semibold text-muted">Priority Mapping</h5>
                      <table className="w-full text-xs">
                        <thead>
                          <tr className="border-b border-rule">
                            <th className="pb-1 text-left text-dim">Internal Priority</th>
                            <th className="pb-1 text-left text-dim">Jira Priority</th>
                          </tr>
                        </thead>
                        <tbody>
                          {Object.keys(rediscoveryPriorityMap).map(internal => (
                            <tr key={internal} className="border-b border-rule">
                              <td className="py-1 pr-3 text-muted">{internal}</td>
                              <td className="py-1">
                                <select
                                  value={rediscoveryPriorityMap[internal] ?? ''}
                                  onChange={e => setRediscoveryPriorityMap(m => ({ ...m, [internal]: e.target.value }))}
                                  className="w-full rounded-sm border border-rule bg-surface-900 px-2 py-0.5 text-xs text-fg focus:outline-none focus:ring-1 focus:ring-accent"
                                >
                                  <option value="">— none —</option>
                                  {rediscoveryResult.project.priorities.map(p => (
                                    <option key={p.id} value={p.name}>{p.name}</option>
                                  ))}
                                </select>
                              </td>
                            </tr>
                          ))}
                        </tbody>
                      </table>
                    </div>

                    {/* Ingest JQL */}
                    <div>
                      <h5 className="mb-1 text-xs font-semibold text-muted">Ingest JQL</h5>
                      <input
                        type="text"
                        value={rediscoveryJQL}
                        onChange={e => setRediscoveryJQL(e.target.value)}
                        className={inputClass()}
                        placeholder="project = PROJ AND labels = agent-ingested ORDER BY created DESC"
                      />
                    </div>

                    {rediscoverySaved ? (
                      <p className="text-xs text-emerald-400">Mapping updated successfully.</p>
                    ) : (
                      <button
                        onClick={handleRediscoveryConfirm}
                        disabled={rediscoveryConfirming}
                        className="rounded bg-emerald-700 px-4 py-2 text-sm font-medium text-white hover:bg-emerald-600 disabled:opacity-50"
                      >
                        {rediscoveryConfirming ? 'Saving...' : 'Confirm Updated Mapping'}
                      </button>
                    )}
                  </>
                )}
              </div>
            )}
          </div>
        )}
      </div>
    </div>
  )
}
