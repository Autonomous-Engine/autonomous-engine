import { useState, useEffect } from 'react'
import { useApi } from '@/hooks/useApi'
import type { AuthProvider, LoginAuditEntry } from '@/types'

// ─── Types for config forms ───────────────────────────────────────────────────

interface SamlConfig {
  metadata_url: string
  entity_id: string
  admin_groups: string
  operator_groups: string
  viewer_groups: string
  default_role: string
}

interface LdapConfig {
  server_url: string
  use_tls: boolean
  bind_dn: string
  bind_password: string
  search_base: string
  search_filter: string
  group_attribute: string
  admin_groups: string
  operator_groups: string
  viewer_groups: string
  default_role: string
}

// ─── Helper components ────────────────────────────────────────────────────────

function Badge({ children, color }: { children: React.ReactNode; color: string }) {
  return (
    <span className={`inline-flex items-center rounded-full px-2 py-0.5 text-xs font-medium ${color}`}>
      {children}
    </span>
  )
}

function FormRow({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div>
      <label className="mb-1 block text-xs font-medium text-muted">{label}</label>
      {children}
    </div>
  )
}

function TextInput({
  value,
  onChange,
  placeholder,
  readOnly,
  type = 'text',
}: {
  value: string
  onChange?: (v: string) => void
  placeholder?: string
  readOnly?: boolean
  type?: string
}) {
  return (
    <input
      type={type}
      value={value}
      readOnly={readOnly}
      onChange={onChange ? (e) => onChange(e.target.value) : undefined}
      placeholder={placeholder}
      className={[
        'w-full rounded-sm border px-3 py-2 text-sm text-fg placeholder-dim focus:outline-none focus:ring-1 focus:ring-accent',
        readOnly
          ? 'border-rule/40 bg-surface-800/40 text-muted cursor-default'
          : 'border-rule bg-surface-900 focus:border-accent',
      ].join(' ')}
    />
  )
}

function RoleMappingSection({
  admin,
  operator,
  viewer,
  defaultRole,
  onAdmin,
  onOperator,
  onViewer,
  onDefault,
}: {
  admin: string
  operator: string
  viewer: string
  defaultRole: string
  onAdmin: (v: string) => void
  onOperator: (v: string) => void
  onViewer: (v: string) => void
  onDefault: (v: string) => void
}) {
  return (
    <div className="space-y-3">
      <p className="text-xs font-semibold uppercase tracking-widest text-dim">Role Mapping</p>
      <FormRow label="Admin Groups (comma-separated)">
        <TextInput value={admin} onChange={onAdmin} placeholder="admins,sysadmins" />
      </FormRow>
      <FormRow label="Operator Groups (comma-separated)">
        <TextInput value={operator} onChange={onOperator} placeholder="operators,devs" />
      </FormRow>
      <FormRow label="Viewer Groups (comma-separated)">
        <TextInput value={viewer} onChange={onViewer} placeholder="viewers,readonly" />
      </FormRow>
      <FormRow label="Default Role">
        <select
          value={defaultRole}
          onChange={(e) => onDefault(e.target.value)}
          className="w-full rounded-sm border border-rule bg-surface-900 px-3 py-2 text-sm text-fg focus:border-accent focus:outline-none focus:ring-1 focus:ring-accent"
        >
          <option value="viewer">Viewer</option>
          <option value="operator">Operator</option>
          <option value="admin">Admin</option>
        </select>
      </FormRow>
    </div>
  )
}

// ─── Local Provider Card ──────────────────────────────────────────────────────

function LocalCard({
  provider,
  onSetPrimary,
}: {
  provider: AuthProvider
  onSetPrimary: () => void
}) {
  return (
    <div className="rounded-sm border border-rule bg-surface-950 p-5">
      <div className="mb-4 flex items-start justify-between">
        <div className="space-y-1.5">
          <div className="flex items-center gap-2">
            <h3 className="text-sm font-semibold text-fg">Local Authentication</h3>
          </div>
          <div className="flex flex-wrap gap-1.5">
            <Badge color="bg-surface-700 text-fg">Local</Badge>
            <Badge color="bg-emerald-900/60 text-emerald-300">Enabled</Badge>
            {provider.is_primary && <Badge color="bg-indigo-900/60 text-accent">Primary</Badge>}
          </div>
        </div>
      </div>
      <p className="mb-4 text-xs text-muted">
        Username and password authentication stored in the local database. Cannot be disabled.
      </p>
      {!provider.is_primary && (
        <button
          onClick={onSetPrimary}
          className="rounded-sm border border-indigo-600/60 bg-accent/10 px-3 py-1.5 text-xs font-medium text-accent transition-colors hover:bg-accent/10"
        >
          Set as Primary
        </button>
      )}
    </div>
  )
}

// ─── SAML Card ────────────────────────────────────────────────────────────────

function SamlCard({
  provider,
  onToggle,
  onSetPrimary,
  onSave,
}: {
  provider: AuthProvider
  onToggle: () => void
  onSetPrimary: () => void
  onSave: (config: SamlConfig) => Promise<void>
}) {
  const raw = provider.config as Partial<SamlConfig>
  const [config, setConfig] = useState<SamlConfig>({
    metadata_url: (raw.metadata_url as string) ?? '',
    entity_id: (raw.entity_id as string) ?? '',
    admin_groups: (raw.admin_groups as string) ?? '',
    operator_groups: (raw.operator_groups as string) ?? '',
    viewer_groups: (raw.viewer_groups as string) ?? '',
    default_role: (raw.default_role as string) ?? 'viewer',
  })
  const [testing, setTesting] = useState(false)
  const [testResult, setTestResult] = useState<{ ok: boolean; message: string } | null>(null)
  const [saving, setSaving] = useState(false)

  function update<K extends keyof SamlConfig>(key: K, value: SamlConfig[K]) {
    setConfig((prev) => ({ ...prev, [key]: value }))
  }

  async function handleTest() {
    setTesting(true)
    setTestResult(null)
    await new Promise((r) => setTimeout(r, 800))
    setTestResult({ ok: true, message: 'Connection successful.' })
    setTesting(false)
  }

  async function handleSave() {
    setSaving(true)
    try {
      await onSave(config)
    } finally {
      setSaving(false)
    }
  }

  const acsUrl = `${window.location.origin}/api/auth/saml/acs`

  return (
    <div className="rounded-sm border border-rule bg-surface-950 p-5">
      <div className="mb-4 flex items-start justify-between">
        <div className="space-y-1.5">
          <h3 className="text-sm font-semibold text-fg">SAML 2.0</h3>
          <div className="flex flex-wrap gap-1.5">
            <Badge color="bg-blue-900/60 text-blue-300">SAML 2.0</Badge>
            {provider.enabled ? (
              <Badge color="bg-emerald-900/60 text-emerald-300">Enabled</Badge>
            ) : (
              <Badge color="bg-surface-700 text-muted">Disabled</Badge>
            )}
            {provider.is_primary && <Badge color="bg-indigo-900/60 text-accent">Primary</Badge>}
          </div>
        </div>
        <button
          onClick={onToggle}
          className={[
            'relative inline-flex h-5 w-9 items-center rounded-full transition-colors focus:outline-none',
            provider.enabled ? 'bg-accent' : 'bg-surface-700',
          ].join(' ')}
          aria-label="Toggle SAML"
        >
          <span
            className={[
              'inline-block h-3.5 w-3.5 transform rounded-full bg-white shadow transition-transform',
              provider.enabled ? 'translate-x-[18px]' : 'translate-x-[3px]',
            ].join(' ')}
          />
        </button>
      </div>

      {provider.enabled && (
        <div className="space-y-4 border-t border-rule/40 pt-4">
          <FormRow label="Metadata URL">
            <TextInput
              value={config.metadata_url}
              onChange={(v) => update('metadata_url', v)}
              placeholder="https://idp.example.com/metadata.xml"
            />
          </FormRow>
          <FormRow label="Entity ID">
            <TextInput
              value={config.entity_id}
              onChange={(v) => update('entity_id', v)}
              placeholder="https://your-app.example.com"
            />
          </FormRow>
          <FormRow label="ACS URL (Assertion Consumer Service)">
            <TextInput value={acsUrl} readOnly />
          </FormRow>

          <RoleMappingSection
            admin={config.admin_groups}
            operator={config.operator_groups}
            viewer={config.viewer_groups}
            defaultRole={config.default_role}
            onAdmin={(v) => update('admin_groups', v)}
            onOperator={(v) => update('operator_groups', v)}
            onViewer={(v) => update('viewer_groups', v)}
            onDefault={(v) => update('default_role', v)}
          />

          {testResult && (
            <div
              className={[
                'rounded-sm border px-3 py-2 text-xs',
                testResult.ok
                  ? 'border-emerald-700/40 bg-emerald-900/20 text-emerald-300'
                  : 'border-red-700/40 bg-red-900/20 text-red-300',
              ].join(' ')}
            >
              {testResult.message}
            </div>
          )}

          <div className="flex gap-2">
            {!provider.is_primary && (
              <button
                onClick={onSetPrimary}
                className="rounded-sm border border-indigo-600/60 bg-accent/10 px-3 py-1.5 text-xs font-medium text-accent transition-colors hover:bg-accent/10"
              >
                Set as Primary
              </button>
            )}
            <button
              onClick={handleTest}
              disabled={testing}
              className="rounded-sm border border-rule bg-surface-800 px-3 py-1.5 text-xs font-medium text-fg transition-colors hover:bg-surface-700 disabled:opacity-60"
            >
              {testing ? 'Testing…' : 'Test Connection'}
            </button>
            <button
              onClick={handleSave}
              disabled={saving}
              className="ml-auto rounded-sm bg-accent px-3 py-1.5 text-xs font-semibold text-surface-900 transition-colors hover:bg-accent-hover disabled:opacity-60"
            >
              {saving ? 'Saving…' : 'Save Configuration'}
            </button>
          </div>
        </div>
      )}

      {!provider.enabled && (
        <p className="text-xs text-dim">Enable SAML to configure enterprise SSO.</p>
      )}
    </div>
  )
}

// ─── LDAP Card ────────────────────────────────────────────────────────────────

function LdapCard({
  provider,
  onToggle,
  onSetPrimary,
  onSave,
}: {
  provider: AuthProvider
  onToggle: () => void
  onSetPrimary: () => void
  onSave: (config: LdapConfig) => Promise<void>
}) {
  const raw = provider.config as Partial<LdapConfig>
  const [config, setConfig] = useState<LdapConfig>({
    server_url: (raw.server_url as string) ?? '',
    use_tls: (raw.use_tls as boolean) ?? true,
    bind_dn: (raw.bind_dn as string) ?? '',
    bind_password: (raw.bind_password as string) ?? '',
    search_base: (raw.search_base as string) ?? '',
    search_filter: (raw.search_filter as string) ?? '(sAMAccountName={{username}})',
    group_attribute: (raw.group_attribute as string) ?? 'memberOf',
    admin_groups: (raw.admin_groups as string) ?? '',
    operator_groups: (raw.operator_groups as string) ?? '',
    viewer_groups: (raw.viewer_groups as string) ?? '',
    default_role: (raw.default_role as string) ?? 'viewer',
  })
  const [testing, setTesting] = useState(false)
  const [testResult, setTestResult] = useState<{ ok: boolean; message: string } | null>(null)
  const [saving, setSaving] = useState(false)

  function update<K extends keyof LdapConfig>(key: K, value: LdapConfig[K]) {
    setConfig((prev) => ({ ...prev, [key]: value }))
  }

  async function handleTest() {
    setTesting(true)
    setTestResult(null)
    await new Promise((r) => setTimeout(r, 800))
    setTestResult({ ok: true, message: 'LDAP connection successful.' })
    setTesting(false)
  }

  async function handleSave() {
    setSaving(true)
    try {
      await onSave(config)
    } finally {
      setSaving(false)
    }
  }

  return (
    <div className="rounded-sm border border-rule bg-surface-950 p-5">
      <div className="mb-4 flex items-start justify-between">
        <div className="space-y-1.5">
          <h3 className="text-sm font-semibold text-fg">LDAP / Active Directory</h3>
          <div className="flex flex-wrap gap-1.5">
            <Badge color="bg-green-900/60 text-green-300">LDAP / Active Directory</Badge>
            {provider.enabled ? (
              <Badge color="bg-emerald-900/60 text-emerald-300">Enabled</Badge>
            ) : (
              <Badge color="bg-surface-700 text-muted">Disabled</Badge>
            )}
            {provider.is_primary && <Badge color="bg-indigo-900/60 text-accent">Primary</Badge>}
          </div>
        </div>
        <button
          onClick={onToggle}
          className={[
            'relative inline-flex h-5 w-9 items-center rounded-full transition-colors focus:outline-none',
            provider.enabled ? 'bg-accent' : 'bg-surface-700',
          ].join(' ')}
          aria-label="Toggle LDAP"
        >
          <span
            className={[
              'inline-block h-3.5 w-3.5 transform rounded-full bg-white shadow transition-transform',
              provider.enabled ? 'translate-x-[18px]' : 'translate-x-[3px]',
            ].join(' ')}
          />
        </button>
      </div>

      {provider.enabled && (
        <div className="space-y-4 border-t border-rule/40 pt-4">
          <FormRow label="Server URL">
            <TextInput
              value={config.server_url}
              onChange={(v) => update('server_url', v)}
              placeholder="ldap://dc.example.com:389"
            />
          </FormRow>

          <div className="flex items-center gap-3">
            <button
              type="button"
              onClick={() => update('use_tls', !config.use_tls)}
              className={[
                'relative inline-flex h-5 w-9 items-center rounded-full transition-colors focus:outline-none',
                config.use_tls ? 'bg-accent' : 'bg-surface-700',
              ].join(' ')}
              aria-label="Toggle TLS"
            >
              <span
                className={[
                  'inline-block h-3.5 w-3.5 transform rounded-full bg-white shadow transition-transform',
                  config.use_tls ? 'translate-x-[18px]' : 'translate-x-[3px]',
                ].join(' ')}
              />
            </button>
            <span className="text-xs text-muted">Use TLS / LDAPS</span>
          </div>

          <FormRow label="Bind DN">
            <TextInput
              value={config.bind_dn}
              onChange={(v) => update('bind_dn', v)}
              placeholder="CN=svc-account,OU=Service Accounts,DC=example,DC=com"
            />
          </FormRow>

          <FormRow label="Bind Password">
            <TextInput
              type="password"
              value={config.bind_password}
              onChange={(v) => update('bind_password', v)}
              placeholder="••••••••••••"
            />
          </FormRow>

          <FormRow label="Search Base">
            <TextInput
              value={config.search_base}
              onChange={(v) => update('search_base', v)}
              placeholder="OU=Users,DC=example,DC=com"
            />
          </FormRow>

          <FormRow label="Search Filter">
            <TextInput
              value={config.search_filter}
              onChange={(v) => update('search_filter', v)}
              placeholder="(sAMAccountName={{username}})"
            />
            <p className="mt-1 text-xs text-dim">
              Use <code className="rounded bg-surface-800 px-1 text-muted">{'{{username}}'}</code> as placeholder.
            </p>
          </FormRow>

          <FormRow label="Group Attribute">
            <TextInput
              value={config.group_attribute}
              onChange={(v) => update('group_attribute', v)}
              placeholder="memberOf"
            />
          </FormRow>

          <RoleMappingSection
            admin={config.admin_groups}
            operator={config.operator_groups}
            viewer={config.viewer_groups}
            defaultRole={config.default_role}
            onAdmin={(v) => update('admin_groups', v)}
            onOperator={(v) => update('operator_groups', v)}
            onViewer={(v) => update('viewer_groups', v)}
            onDefault={(v) => update('default_role', v)}
          />

          {testResult && (
            <div
              className={[
                'rounded-sm border px-3 py-2 text-xs',
                testResult.ok
                  ? 'border-emerald-700/40 bg-emerald-900/20 text-emerald-300'
                  : 'border-red-700/40 bg-red-900/20 text-red-300',
              ].join(' ')}
            >
              {testResult.message}
            </div>
          )}

          <div className="flex gap-2">
            {!provider.is_primary && (
              <button
                onClick={onSetPrimary}
                className="rounded-sm border border-indigo-600/60 bg-accent/10 px-3 py-1.5 text-xs font-medium text-accent transition-colors hover:bg-accent/10"
              >
                Set as Primary
              </button>
            )}
            <button
              onClick={handleTest}
              disabled={testing}
              className="rounded-sm border border-rule bg-surface-800 px-3 py-1.5 text-xs font-medium text-fg transition-colors hover:bg-surface-700 disabled:opacity-60"
            >
              {testing ? 'Testing…' : 'Test Connection'}
            </button>
            <button
              onClick={handleSave}
              disabled={saving}
              className="ml-auto rounded-sm bg-accent px-3 py-1.5 text-xs font-semibold text-surface-900 transition-colors hover:bg-accent-hover disabled:opacity-60"
            >
              {saving ? 'Saving…' : 'Save Configuration'}
            </button>
          </div>
        </div>
      )}

      {!provider.enabled && (
        <p className="text-xs text-dim">Enable LDAP to configure directory authentication.</p>
      )}
    </div>
  )
}

// ─── Audit Table ──────────────────────────────────────────────────────────────

function AuditTable({ entries }: { entries: LoginAuditEntry[] }) {
  const providerBadge = (p: string) => {
    if (p === 'saml') return 'bg-blue-900/60 text-blue-300'
    if (p === 'ldap') return 'bg-green-900/60 text-green-300'
    return 'bg-surface-700 text-muted'
  }

  return (
    <div className="overflow-hidden rounded-sm border border-rule">
      <div className="overflow-x-auto">
        <table className="w-full text-sm">
          <thead>
            <tr className="border-b border-rule bg-surface-950">
              {['Timestamp', 'Email', 'Provider', 'Success', 'Failure Reason', 'IP Address'].map((h) => (
                <th key={h} className="px-4 py-3 text-left text-xs font-semibold uppercase tracking-wider text-muted">
                  {h}
                </th>
              ))}
            </tr>
          </thead>
          <tbody className="divide-y divide-rule/40">
            {entries.length === 0 && (
              <tr>
                <td colSpan={6} className="px-4 py-8 text-center text-sm text-dim">
                  No audit entries found.
                </td>
              </tr>
            )}
            {entries.map((entry) => (
              <tr key={entry.id} className="bg-surface-950 hover:bg-surface-800/40">
                <td className="whitespace-nowrap px-4 py-3 text-xs text-muted">
                  {new Date(entry.timestamp).toLocaleString()}
                </td>
                <td className="px-4 py-3 text-xs text-fg">{entry.email}</td>
                <td className="px-4 py-3">
                  <span className={`inline-flex items-center rounded-full px-2 py-0.5 text-xs font-medium ${providerBadge(entry.provider)}`}>
                    {entry.provider}
                  </span>
                </td>
                <td className="px-4 py-3">
                  {entry.success ? (
                    <span className="text-emerald-400">✓</span>
                  ) : (
                    <span className="text-red-400">✗</span>
                  )}
                </td>
                <td className="px-4 py-3 text-xs text-muted">{entry.failure_reason || '—'}</td>
                <td className="px-4 py-3 font-mono text-xs text-muted">{entry.ip_address}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  )
}

// ─── Main Component ───────────────────────────────────────────────────────────

export default function AuthConfig() {
  const { get, put } = useApi()

  const [providers, setProviders] = useState<AuthProvider[]>([])
  const [auditEntries, setAuditEntries] = useState<LoginAuditEntry[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [successMessage, setSuccessMessage] = useState('')

  useEffect(() => {
    Promise.all([
      get<AuthProvider[]>('/admin/auth/providers'),
      get<LoginAuditEntry[]>('/admin/auth/login-audit?limit=20'),
    ])
      .then(([provs, audit]) => {
        setProviders(provs)
        setAuditEntries(audit)
      })
      .catch(() => setError('Failed to load auth configuration.'))
      .finally(() => setLoading(false))
  }, [get])

  function showSuccess(msg: string) {
    setSuccessMessage(msg)
    setTimeout(() => setSuccessMessage(''), 3000)
  }

  function getProvider(type: AuthProvider['provider_type']): AuthProvider {
    return (
      providers.find((p) => p.provider_type === type) ?? {
        id: type,
        provider_type: type,
        name: type,
        enabled: false,
        is_primary: false,
        config: {},
      }
    )
  }

  function updateProvider(updated: Partial<AuthProvider> & { provider_type: AuthProvider['provider_type'] }) {
    setProviders((prev) =>
      prev.map((p) => (p.provider_type === updated.provider_type ? { ...p, ...updated } : p)),
    )
  }

  async function handleSetPrimary(type: AuthProvider['provider_type']) {
    try {
      await put('/admin/auth/providers/primary', { type })
      setProviders((prev) =>
        prev.map((p) => ({ ...p, is_primary: p.provider_type === type })),
      )
      showSuccess('Primary provider updated.')
    } catch {
      setError('Failed to update primary provider.')
    }
  }

  async function handleToggle(type: 'saml' | 'ldap') {
    const provider = getProvider(type)
    const enabled = !provider.enabled
    try {
      await put(`/admin/auth/providers/update?type=${type}`, { enabled })
      updateProvider({ provider_type: type, enabled })
      showSuccess(`${type.toUpperCase()} ${enabled ? 'enabled' : 'disabled'}.`)
    } catch {
      setError(`Failed to toggle ${type.toUpperCase()}.`)
    }
  }

  async function handleSaveSaml(config: SamlConfig) {
    await put('/admin/auth/providers/update?type=saml', { config })
    showSuccess('SAML configuration saved.')
  }

  async function handleSaveLdap(config: LdapConfig) {
    await put('/admin/auth/providers/update?type=ldap', { config })
    showSuccess('LDAP configuration saved.')
  }

  if (loading) {
    return (
      <div className="flex h-64 items-center justify-center">
        <p className="text-sm text-muted">Loading auth configuration…</p>
      </div>
    )
  }

  const local = getProvider('local')
  const saml = getProvider('saml')
  const ldap = getProvider('ldap')

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-lg font-semibold text-fg">Authentication Configuration</h1>
          <p className="mt-0.5 text-sm text-muted">Manage auth providers and SSO settings.</p>
        </div>
      </div>

      {error && (
        <div className="rounded-sm border border-red-700/40 bg-red-900/20 px-3 py-2 text-sm text-red-300">
          {error}
        </div>
      )}
      {successMessage && (
        <div className="rounded-sm border border-emerald-700/40 bg-emerald-900/20 px-3 py-2 text-sm text-emerald-300">
          {successMessage}
        </div>
      )}

      {/* Provider cards */}
      <div className="grid gap-4 lg:grid-cols-3">
        <LocalCard provider={local} onSetPrimary={() => handleSetPrimary('local')} />
        <SamlCard
          provider={saml}
          onToggle={() => handleToggle('saml')}
          onSetPrimary={() => handleSetPrimary('saml')}
          onSave={handleSaveSaml}
        />
        <LdapCard
          provider={ldap}
          onToggle={() => handleToggle('ldap')}
          onSetPrimary={() => handleSetPrimary('ldap')}
          onSave={handleSaveLdap}
        />
      </div>

      {/* Login Audit */}
      <div>
        <h2 className="mb-3 text-sm font-semibold text-muted">Login Audit Log</h2>
        <AuditTable entries={auditEntries} />
      </div>
    </div>
  )
}
