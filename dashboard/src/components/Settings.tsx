import { useEffect, useState, useCallback } from 'react'
import { useApi } from '@/hooks/useApi'
import type { User } from '@/types'

interface ConfigEntry {
  id: string
  scope: string
  scope_target: string
  key_name: string
  value: string
  value_type: string
  category: string
  description: string
  updated_by: string | null
  updated_at: string
}

const ROLE_BADGE: Record<User['role'], string> = {
  admin: 'bg-red-500/20 text-red-300',
  operator: 'bg-accent/10 text-accent',
  viewer: 'bg-surface-700/40 text-muted',
}

export default function Settings() {
  const { get } = useApi()
  const [entries, setEntries] = useState<ConfigEntry[]>([])
  const [users, setUsers] = useState<User[]>([])
  const [loadingConfig, setLoadingConfig] = useState(true)
  const [loadingUsers, setLoadingUsers] = useState(true)
  const [configError, setConfigError] = useState<string | null>(null)
  const [usersError, setUsersError] = useState<string | null>(null)

  const fetchConfig = useCallback(async () => {
    try {
      setConfigError(null)
      const data = await get<ConfigEntry[]>('/admin/config')
      setEntries(data ?? [])
    } catch (err) {
      setConfigError(err instanceof Error ? err.message : 'Failed to load config')
    } finally {
      setLoadingConfig(false)
    }
  }, [get])

  const fetchUsers = useCallback(async () => {
    try {
      setUsersError(null)
      const data = await get<User[]>('/admin/users')
      setUsers(data)
    } catch (err) {
      setUsersError(err instanceof Error ? err.message : 'Failed to load users')
    } finally {
      setLoadingUsers(false)
    }
  }, [get])

  useEffect(() => {
    fetchConfig()
    fetchUsers()
  }, [fetchConfig, fetchUsers])

  const grouped = entries.reduce<Record<string, ConfigEntry[]>>((acc, e) => {
    const cat = e.category || 'general'
    ;(acc[cat] ??= []).push(e)
    return acc
  }, {})

  return (
    <div className="flex flex-col gap-6 max-w-3xl">
      {/* Header */}
      <div>
        <h2 className="text-lg font-semibold text-fg">Settings</h2>
        <p className="text-sm text-dim">
          System configuration and user management
        </p>
      </div>

      {/* System Config */}
      <section>
        <div className="mb-3 flex items-center justify-between">
          <h3 className="text-sm font-semibold text-muted">
            System Configuration
          </h3>
          {configError && (
            <span className="rounded bg-red-500/20 px-2 py-0.5 text-xs text-red-300">
              {configError}
            </span>
          )}
        </div>

        <div className="rounded-sm border border-rule bg-surface-800 px-4">
          {loadingConfig ? (
            <div className="flex h-32 items-center justify-center">
              <div className="h-6 w-6 animate-spin rounded-full border-2 border-accent border-t-transparent" />
            </div>
          ) : entries.length > 0 ? (
            <div className="divide-y divide-rule/40">
              {Object.entries(grouped).map(([category, items]) => (
                <div key={category} className="py-3">
                  <p className="mb-2 text-xs font-medium uppercase tracking-wider text-dim">
                    {category}
                  </p>
                  {items.map((entry) => (
                    <div
                      key={entry.id}
                      className="flex items-center justify-between border-b border-rule/20 py-2 last:border-0"
                    >
                      <div className="flex flex-col gap-0.5">
                        <span className="font-mono text-sm text-fg">
                          {entry.key_name}
                        </span>
                        {entry.description && (
                          <span className="text-xs text-dim">
                            {entry.description}
                          </span>
                        )}
                        <span className="text-xs text-dim">
                          {entry.scope}
                          {entry.scope_target ? ` → ${entry.scope_target}` : ''}
                        </span>
                      </div>
                      <div className="flex items-center gap-2">
                        <span className="rounded bg-surface-700/50 px-2 py-0.5 font-mono text-xs text-fg">
                          {entry.value}
                        </span>
                        <span className="text-xs text-dim">
                          {entry.value_type}
                        </span>
                      </div>
                    </div>
                  ))}
                </div>
              ))}
            </div>
          ) : (
            <p className="py-6 text-center text-sm text-dim">
              No configuration entries yet. Add entries via the API or admin tools.
            </p>
          )}
        </div>
      </section>

      {/* User Management */}
      <section>
        <div className="mb-3 flex items-center justify-between">
          <h3 className="text-sm font-semibold text-muted">User Management</h3>
          {usersError && (
            <span className="rounded bg-red-500/20 px-2 py-0.5 text-xs text-red-300">
              {usersError}
            </span>
          )}
        </div>

        <div className="overflow-hidden rounded-sm border border-rule">
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b border-rule bg-surface-850">
                <th className="px-4 py-3 text-left text-xs font-medium uppercase tracking-wider text-dim">
                  Name
                </th>
                <th className="px-4 py-3 text-left text-xs font-medium uppercase tracking-wider text-dim">
                  Email
                </th>
                <th className="px-4 py-3 text-left text-xs font-medium uppercase tracking-wider text-dim">
                  Role
                </th>
                <th className="px-4 py-3 text-left text-xs font-medium uppercase tracking-wider text-dim">
                  Joined
                </th>
              </tr>
            </thead>
            <tbody className="divide-y divide-rule/40">
              {loadingUsers ? (
                <tr>
                  <td colSpan={4} className="py-8 text-center">
                    <div className="flex justify-center">
                      <div className="h-6 w-6 animate-spin rounded-full border-2 border-accent border-t-transparent" />
                    </div>
                  </td>
                </tr>
              ) : users.length === 0 ? (
                <tr>
                  <td
                    colSpan={4}
                    className="px-4 py-8 text-center text-sm text-dim"
                  >
                    No users found
                  </td>
                </tr>
              ) : (
                users.map((user) => (
                  <tr
                    key={user.id}
                    className="bg-surface-800 transition-colors hover:bg-surface-850"
                  >
                    <td className="px-4 py-3 font-medium text-fg">
                      {user.name}
                    </td>
                    <td className="px-4 py-3 text-muted">{user.email}</td>
                    <td className="px-4 py-3">
                      <span
                        className={[
                          'rounded px-2 py-0.5 text-xs font-medium',
                          ROLE_BADGE[user.role],
                        ].join(' ')}
                      >
                        {user.role}
                      </span>
                    </td>
                    <td className="px-4 py-3 text-dim">
                      {new Date(user.created_at).toLocaleDateString()}
                    </td>
                  </tr>
                ))
              )}
            </tbody>
          </table>
        </div>
      </section>

      {/* Info banner */}
      <div className="rounded-sm border border-rule/40 bg-surface-850 px-4 py-3">
        <p className="text-xs text-dim">
          Configuration entries can be managed via the API. User management is
          read-only in v1.
        </p>
      </div>
    </div>
  )
}
