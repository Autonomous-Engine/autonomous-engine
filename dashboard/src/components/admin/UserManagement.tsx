import { useState, useEffect } from 'react'
import { useApi } from '@/hooks/useApi'
import type { UserEntry } from '@/types'

// ─── Helper components ────────────────────────────────────────────────────────

function Modal({
  title,
  onClose,
  children,
}: {
  title: string
  onClose: () => void
  children: React.ReactNode
}) {
  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 px-4">
      <div className="w-full max-w-md rounded-sm border border-rule bg-surface-950 p-6 shadow-2xl">
        <div className="mb-4 flex items-center justify-between">
          <h2 className="text-sm font-semibold text-fg">{title}</h2>
          <button
            onClick={onClose}
            className="rounded-sm p-1 text-muted transition-colors hover:bg-surface-800 hover:text-fg"
            aria-label="Close"
          >
            ✕
          </button>
        </div>
        {children}
      </div>
    </div>
  )
}

function RoleBadge({ role }: { role: UserEntry['role'] }) {
  const colors: Record<UserEntry['role'], string> = {
    admin: 'bg-red-900/60 text-red-300',
    operator: 'bg-blue-900/60 text-blue-300',
    viewer: 'bg-surface-700 text-muted',
  }
  return (
    <span className={`inline-flex items-center rounded-full px-2 py-0.5 text-xs font-medium ${colors[role]}`}>
      {role}
    </span>
  )
}

function ProviderBadge({ provider }: { provider: string }) {
  const colors: Record<string, string> = {
    local: 'bg-surface-700 text-muted',
    saml: 'bg-blue-900/60 text-blue-300',
    ldap: 'bg-green-900/60 text-green-300',
  }
  const color = colors[provider] ?? 'bg-surface-700 text-muted'
  return (
    <span className={`inline-flex items-center rounded-full px-2 py-0.5 text-xs font-medium ${color}`}>
      {provider}
    </span>
  )
}

function StatusBadge({ locked }: { locked: boolean }) {
  return locked ? (
    <span className="inline-flex items-center rounded-full bg-red-900/60 px-2 py-0.5 text-xs font-medium text-red-300">
      Locked
    </span>
  ) : (
    <span className="inline-flex items-center rounded-full bg-emerald-900/60 px-2 py-0.5 text-xs font-medium text-emerald-300">
      Active
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

// ─── Edit User Modal ──────────────────────────────────────────────────────────

function EditUserModal({
  user,
  onClose,
  onSave,
}: {
  user: UserEntry
  onClose: () => void
  onSave: (id: string, name: string, role: UserEntry['role']) => Promise<void>
}) {
  const [name, setName] = useState(user.name)
  const [role, setRole] = useState<UserEntry['role']>(user.role)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState('')

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    setSaving(true)
    try {
      await onSave(user.id, name, role)
      onClose()
    } catch {
      setError('Failed to update user.')
    } finally {
      setSaving(false)
    }
  }

  return (
    <Modal title={`Edit User — ${user.email}`} onClose={onClose}>
      {error && (
        <div className="mb-3 rounded-sm border border-red-700/40 bg-red-900/20 px-3 py-2 text-xs text-red-300">
          {error}
        </div>
      )}
      <form onSubmit={handleSubmit} className="space-y-4">
        <FormRow label="Name">
          <input
            type="text"
            value={name}
            onChange={(e) => setName(e.target.value)}
            required
            className="w-full rounded-sm border border-rule bg-surface-900 px-3 py-2 text-sm text-fg focus:border-accent focus:outline-none focus:ring-1 focus:ring-accent"
          />
        </FormRow>
        <FormRow label="Role">
          <select
            value={role}
            onChange={(e) => setRole(e.target.value as UserEntry['role'])}
            className="w-full rounded-sm border border-rule bg-surface-900 px-3 py-2 text-sm text-fg focus:border-accent focus:outline-none focus:ring-1 focus:ring-accent"
          >
            <option value="viewer">Viewer</option>
            <option value="operator">Operator</option>
            <option value="admin">Admin</option>
          </select>
        </FormRow>
        <div className="flex justify-end gap-2 pt-2">
          <button
            type="button"
            onClick={onClose}
            className="rounded-sm border border-rule bg-surface-800 px-3 py-1.5 text-xs font-medium text-fg transition-colors hover:bg-surface-700"
          >
            Cancel
          </button>
          <button
            type="submit"
            disabled={saving}
            className="rounded-sm bg-accent px-3 py-1.5 text-xs font-semibold text-surface-900 transition-colors hover:bg-accent-hover disabled:opacity-60"
          >
            {saving ? 'Saving…' : 'Save Changes'}
          </button>
        </div>
      </form>
    </Modal>
  )
}

// ─── Reset Password Modal ─────────────────────────────────────────────────────

function ResetPasswordModal({
  generatedPassword,
  onClose,
}: {
  generatedPassword: string
  onClose: () => void
}) {
  const [copied, setCopied] = useState(false)

  function handleCopy() {
    navigator.clipboard.writeText(generatedPassword)
    setCopied(true)
    setTimeout(() => setCopied(false), 2000)
  }

  return (
    <Modal title="Password Reset" onClose={onClose}>
      <p className="mb-3 text-xs text-muted">
        The user's password has been reset. Share this temporary password with them securely.
      </p>
      <div className="flex items-center gap-2 rounded-sm border border-rule bg-surface-900 px-3 py-2">
        <code className="flex-1 font-mono text-sm text-emerald-300">{generatedPassword}</code>
        <button
          onClick={handleCopy}
          className="rounded-sm border border-rule bg-surface-800 px-2 py-1 text-xs text-fg transition-colors hover:bg-surface-700"
        >
          {copied ? 'Copied!' : 'Copy'}
        </button>
      </div>
      <div className="mt-4 flex justify-end">
        <button
          onClick={onClose}
          className="rounded-sm bg-accent px-3 py-1.5 text-xs font-semibold text-surface-900 transition-colors hover:bg-accent-hover"
        >
          Done
        </button>
      </div>
    </Modal>
  )
}

// ─── Create User Modal ────────────────────────────────────────────────────────

function CreateUserModal({
  onClose,
  onCreate,
}: {
  onClose: () => void
  onCreate: (data: {
    email: string
    name: string
    role: UserEntry['role']
    password: string
    password_change_required: boolean
  }) => Promise<void>
}) {
  const [email, setEmail] = useState('')
  const [name, setName] = useState('')
  const [role, setRole] = useState<UserEntry['role']>('viewer')
  const [password, setPassword] = useState('')
  const [requireChange, setRequireChange] = useState(true)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState('')

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    if (password.length < 12) {
      setError('Password must be at least 12 characters.')
      return
    }
    setSaving(true)
    try {
      await onCreate({ email, name, role, password, password_change_required: requireChange })
      onClose()
    } catch {
      setError('Failed to create user.')
    } finally {
      setSaving(false)
    }
  }

  return (
    <Modal title="Create User" onClose={onClose}>
      {error && (
        <div className="mb-3 rounded-sm border border-red-700/40 bg-red-900/20 px-3 py-2 text-xs text-red-300">
          {error}
        </div>
      )}
      <form onSubmit={handleSubmit} className="space-y-4">
        <FormRow label="Email">
          <input
            type="email"
            value={email}
            onChange={(e) => setEmail(e.target.value)}
            required
            className="w-full rounded-sm border border-rule bg-surface-900 px-3 py-2 text-sm text-fg focus:border-accent focus:outline-none focus:ring-1 focus:ring-accent"
            placeholder="user@example.com"
          />
        </FormRow>
        <FormRow label="Name">
          <input
            type="text"
            value={name}
            onChange={(e) => setName(e.target.value)}
            required
            className="w-full rounded-sm border border-rule bg-surface-900 px-3 py-2 text-sm text-fg focus:border-accent focus:outline-none focus:ring-1 focus:ring-accent"
            placeholder="Full Name"
          />
        </FormRow>
        <FormRow label="Role">
          <select
            value={role}
            onChange={(e) => setRole(e.target.value as UserEntry['role'])}
            className="w-full rounded-sm border border-rule bg-surface-900 px-3 py-2 text-sm text-fg focus:border-accent focus:outline-none focus:ring-1 focus:ring-accent"
          >
            <option value="viewer">Viewer</option>
            <option value="operator">Operator</option>
            <option value="admin">Admin</option>
          </select>
        </FormRow>
        <FormRow label="Password (min 12 characters)">
          <input
            type="password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            required
            minLength={12}
            className="w-full rounded-sm border border-rule bg-surface-900 px-3 py-2 text-sm text-fg focus:border-accent focus:outline-none focus:ring-1 focus:ring-accent"
            placeholder="••••••••••••"
          />
        </FormRow>
        <div className="flex items-center gap-2">
          <input
            id="require-change"
            type="checkbox"
            checked={requireChange}
            onChange={(e) => setRequireChange(e.target.checked)}
            className="h-3.5 w-3.5 rounded-sm border-rule bg-surface-800 text-indigo-600 focus:ring-accent"
          />
          <label htmlFor="require-change" className="text-xs text-muted">
            Require password change on first login
          </label>
        </div>
        <div className="flex justify-end gap-2 pt-2">
          <button
            type="button"
            onClick={onClose}
            className="rounded-sm border border-rule bg-surface-800 px-3 py-1.5 text-xs font-medium text-fg transition-colors hover:bg-surface-700"
          >
            Cancel
          </button>
          <button
            type="submit"
            disabled={saving}
            className="rounded-sm bg-accent px-3 py-1.5 text-xs font-semibold text-surface-900 transition-colors hover:bg-accent-hover disabled:opacity-60"
          >
            {saving ? 'Creating…' : 'Create User'}
          </button>
        </div>
      </form>
    </Modal>
  )
}

// ─── Delete Confirmation ──────────────────────────────────────────────────────

function DeleteConfirmModal({
  user,
  onClose,
  onConfirm,
}: {
  user: UserEntry
  onClose: () => void
  onConfirm: () => Promise<void>
}) {
  const [deleting, setDeleting] = useState(false)

  async function handleDelete() {
    setDeleting(true)
    await onConfirm()
    setDeleting(false)
  }

  return (
    <Modal title="Delete User" onClose={onClose}>
      <p className="mb-4 text-sm text-muted">
        Are you sure you want to delete <span className="font-semibold text-fg">{user.email}</span>?
        This action cannot be undone.
      </p>
      <div className="flex justify-end gap-2">
        <button
          onClick={onClose}
          className="rounded-sm border border-rule bg-surface-800 px-3 py-1.5 text-xs font-medium text-fg transition-colors hover:bg-surface-700"
        >
          Cancel
        </button>
        <button
          onClick={handleDelete}
          disabled={deleting}
          className="rounded-sm bg-red-700 px-3 py-1.5 text-xs font-semibold text-white transition-colors hover:bg-red-600 disabled:opacity-60"
        >
          {deleting ? 'Deleting…' : 'Delete'}
        </button>
      </div>
    </Modal>
  )
}

// ─── Main Component ───────────────────────────────────────────────────────────

interface ResetResult {
  generated_password: string
}

export default function UserManagement() {
  const { get, post, put, del } = useApi()

  const [users, setUsers] = useState<UserEntry[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [successMessage, setSuccessMessage] = useState('')
  const [search, setSearch] = useState('')

  const [editUser, setEditUser] = useState<UserEntry | null>(null)
  const [deleteUser, setDeleteUser] = useState<UserEntry | null>(null)
  const [resetPasswordResult, setResetPasswordResult] = useState<string | null>(null)
  const [showCreate, setShowCreate] = useState(false)

  // The current user's ID — used to prevent self-deletion
  const [currentUserId] = useState<string | null>(() => {
    try {
      const token = localStorage.getItem('auth_token')
      if (!token) return null
      const payload = JSON.parse(atob(token.split('.')[1]))
      return payload.sub ?? payload.id ?? null
    } catch {
      return null
    }
  })

  function showSuccess(msg: string) {
    setSuccessMessage(msg)
    setTimeout(() => setSuccessMessage(''), 3000)
  }

  useEffect(() => {
    get<UserEntry[]>('/admin/users')
      .then(setUsers)
      .catch(() => setError('Failed to load users.'))
      .finally(() => setLoading(false))
  }, [get])

  const filtered = users.filter(
    (u) =>
      u.email.toLowerCase().includes(search.toLowerCase()) ||
      u.name.toLowerCase().includes(search.toLowerCase()),
  )

  async function handleEdit(id: string, name: string, role: UserEntry['role']) {
    await put(`/admin/users/update?id=${id}`, { name, role })
    setUsers((prev) => prev.map((u) => (u.id === id ? { ...u, name, role } : u)))
    showSuccess('User updated.')
  }

  async function handleResetPassword(id: string) {
    const res = await post<ResetResult>(`/admin/users/reset-password?id=${id}`)
    setResetPasswordResult(res.generated_password)
  }

  async function handleUnlock(id: string) {
    await post(`/admin/users/unlock?id=${id}`)
    setUsers((prev) => prev.map((u) => (u.id === id ? { ...u, locked_at: undefined } : u)))
    showSuccess('User unlocked.')
  }

  async function handleDelete(user: UserEntry) {
    await del(`/admin/users/delete?id=${user.id}`)
    setUsers((prev) => prev.filter((u) => u.id !== user.id))
    setDeleteUser(null)
    showSuccess(`User ${user.email} deleted.`)
  }

  async function handleCreate(data: {
    email: string
    name: string
    role: UserEntry['role']
    password: string
    password_change_required: boolean
  }) {
    const newUser = await post<UserEntry>('/admin/users/create', data)
    setUsers((prev) => [...prev, newUser])
    showSuccess(`User ${newUser.email} created.`)
  }

  if (loading) {
    return (
      <div className="flex h-64 items-center justify-center">
        <p className="text-sm text-muted">Loading users…</p>
      </div>
    )
  }

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-lg font-semibold text-fg">User Management</h1>
          <p className="mt-0.5 text-sm text-muted">Manage users, roles, and access.</p>
        </div>
        <button
          onClick={() => setShowCreate(true)}
          className="rounded-sm bg-accent px-3 py-1.5 text-xs font-semibold text-surface-900 shadow-sm transition-colors hover:bg-accent-hover"
        >
          + Create User
        </button>
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

      {/* Search */}
      <div>
        <input
          type="search"
          value={search}
          onChange={(e) => setSearch(e.target.value)}
          placeholder="Search by email or name…"
          className="w-full max-w-sm rounded-sm border border-rule bg-surface-950 px-3 py-2 text-sm text-fg placeholder-dim focus:border-accent focus:outline-none focus:ring-1 focus:ring-accent"
        />
      </div>

      {/* Table */}
      <div className="overflow-hidden rounded-sm border border-rule">
        <div className="overflow-x-auto">
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b border-rule bg-surface-950">
                {['Email', 'Name', 'Role', 'Provider', 'Last Login', 'Status', 'Actions'].map((h) => (
                  <th
                    key={h}
                    className="px-4 py-3 text-left text-xs font-semibold uppercase tracking-wider text-muted"
                  >
                    {h}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody className="divide-y divide-rule/40">
              {filtered.length === 0 && (
                <tr>
                  <td colSpan={7} className="px-4 py-8 text-center text-sm text-dim">
                    {search ? 'No users match your search.' : 'No users found.'}
                  </td>
                </tr>
              )}
              {filtered.map((user) => (
                <tr key={user.id} className="bg-surface-950 hover:bg-surface-800/40">
                  <td className="px-4 py-3 text-xs text-fg">{user.email}</td>
                  <td className="px-4 py-3 text-xs text-muted">{user.name}</td>
                  <td className="px-4 py-3">
                    <RoleBadge role={user.role} />
                  </td>
                  <td className="px-4 py-3">
                    <ProviderBadge provider={user.auth_provider} />
                  </td>
                  <td className="whitespace-nowrap px-4 py-3 text-xs text-muted">
                    {user.last_login ? new Date(user.last_login).toLocaleString() : 'Never'}
                  </td>
                  <td className="px-4 py-3">
                    <StatusBadge locked={Boolean(user.locked_at)} />
                  </td>
                  <td className="px-4 py-3">
                    <div className="flex flex-wrap items-center gap-1.5">
                      {/* Edit */}
                      <button
                        onClick={() => setEditUser(user)}
                        className="rounded-sm border border-rule bg-surface-800 px-2 py-1 text-xs text-fg transition-colors hover:bg-surface-700"
                      >
                        Edit
                      </button>

                      {/* Reset Password — local users only */}
                      {user.auth_provider === 'local' && (
                        <button
                          onClick={() => handleResetPassword(user.id)}
                          className="rounded-sm border border-rule bg-surface-800 px-2 py-1 text-xs text-fg transition-colors hover:bg-surface-700"
                        >
                          Reset PW
                        </button>
                      )}

                      {/* Unlock — locked users only */}
                      {user.locked_at && (
                        <button
                          onClick={() => handleUnlock(user.id)}
                          className="rounded-sm border border-amber-700/60 bg-amber-900/20 px-2 py-1 text-xs text-amber-300 transition-colors hover:bg-amber-900/40"
                        >
                          Unlock
                        </button>
                      )}

                      {/* Delete — prevent self-deletion */}
                      {user.id !== currentUserId && (
                        <button
                          onClick={() => setDeleteUser(user)}
                          className="rounded-sm border border-red-700/60 bg-red-900/20 px-2 py-1 text-xs text-red-300 transition-colors hover:bg-red-900/40"
                        >
                          Delete
                        </button>
                      )}
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </div>

      {/* Modals */}
      {editUser && (
        <EditUserModal
          user={editUser}
          onClose={() => setEditUser(null)}
          onSave={handleEdit}
        />
      )}

      {resetPasswordResult && (
        <ResetPasswordModal
          generatedPassword={resetPasswordResult}
          onClose={() => setResetPasswordResult(null)}
        />
      )}

      {deleteUser && (
        <DeleteConfirmModal
          user={deleteUser}
          onClose={() => setDeleteUser(null)}
          onConfirm={() => handleDelete(deleteUser)}
        />
      )}

      {showCreate && (
        <CreateUserModal
          onClose={() => setShowCreate(false)}
          onCreate={handleCreate}
        />
      )}
    </div>
  )
}
