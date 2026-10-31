import { useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { useApi } from '@/hooks/useApi'

type Strength = 'weak' | 'medium' | 'strong'

function getPasswordStrength(pw: string): Strength {
  if (pw.length < 8) return 'weak'
  let score = 0
  if (pw.length >= 12) score++
  if (pw.length >= 16) score++
  if (/[A-Z]/.test(pw)) score++
  if (/[a-z]/.test(pw)) score++
  if (/[0-9]/.test(pw)) score++
  if (/[^A-Za-z0-9]/.test(pw)) score++
  if (score <= 3) return 'weak'
  if (score <= 5) return 'medium'
  return 'strong'
}

const strengthLabel: Record<Strength, string> = {
  weak: 'Weak',
  medium: 'Medium',
  strong: 'Strong',
}

const strengthColor: Record<Strength, string> = {
  weak: 'bg-red-500',
  medium: 'bg-yellow-500',
  strong: 'bg-emerald-500',
}

const strengthTextColor: Record<Strength, string> = {
  weak: 'text-red-400',
  medium: 'text-yellow-400',
  strong: 'text-emerald-400',
}

const strengthWidth: Record<Strength, string> = {
  weak: 'w-1/3',
  medium: 'w-2/3',
  strong: 'w-full',
}

export default function ChangePassword() {
  const navigate = useNavigate()
  const { post } = useApi()

  const [currentPassword, setCurrentPassword] = useState('')
  const [newPassword, setNewPassword] = useState('')
  const [confirmPassword, setConfirmPassword] = useState('')
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState('')
  const [validationErrors, setValidationErrors] = useState<string[]>([])

  const strength = newPassword ? getPasswordStrength(newPassword) : null

  function validate(): string[] {
    const errs: string[] = []
    if (newPassword.length < 12) {
      errs.push('New password must be at least 12 characters.')
    }
    if (newPassword !== confirmPassword) {
      errs.push('Passwords do not match.')
    }
    if (newPassword === currentPassword && newPassword.length > 0) {
      errs.push('New password must differ from current password.')
    }
    return errs
  }

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    setError('')
    const errs = validate()
    setValidationErrors(errs)
    if (errs.length > 0) return

    setLoading(true)
    try {
      await post('/auth/change-password', {
        current_password: currentPassword,
        new_password: newPassword,
      })
      navigate('/')
    } catch (err: unknown) {
      const message = err instanceof Error ? err.message : 'Password change failed'
      if (message.includes('401') || message.toLowerCase().includes('incorrect')) {
        setError('Current password is incorrect.')
      } else {
        setError('Failed to change password. Please try again.')
      }
    } finally {
      setLoading(false)
    }
  }

  return (
    <div className="flex min-h-screen items-center justify-center bg-surface-900 px-4">
      <div className="w-full max-w-sm">
        {/* Logo / Title */}
        <div className="mb-8 flex flex-col items-center gap-3">
          <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32" className="h-12 w-12 drop-shadow-lg">
            <rect width="32" height="32" rx="3" fill="#0a0c10"/>
            <rect x="4" y="4" width="24" height="24" rx="1" fill="none" stroke="#c8fc5b" strokeWidth="1.25"/>
            <text x="14.5" y="20" fontFamily="Geist Mono,ui-monospace,monospace" fontSize="11" fontWeight="500" fill="#c8fc5b" textAnchor="middle" letterSpacing="0.5">AE</text>
            <polygon points="25.5,23.7 26.1,24.5 27.1,24.6 26.7,25.5 27.1,26.4 26.1,26.5 25.5,27.3 24.9,26.5 23.9,26.4 24.3,25.5 23.9,24.6 24.9,24.5" fill="#c8fc5b"/>
            <circle cx="25.5" cy="25.5" r="0.55" fill="#0a0c10"/>
          </svg>
          <div className="text-center">
            <h1 className="text-xl font-semibold text-fg">Change Password</h1>
            <p className="mt-1 text-sm text-muted">You must set a new password before continuing.</p>
          </div>
        </div>

        {/* Card */}
        <div className="rounded-sm border border-rule bg-surface-950 p-6 shadow-xl">
          {error && (
            <div className="mb-4 rounded-sm border border-red-700/40 bg-red-900/20 px-3 py-2 text-sm text-red-300">
              {error}
            </div>
          )}

          {validationErrors.length > 0 && (
            <div className="mb-4 rounded-sm border border-amber-700/40 bg-amber-900/20 px-3 py-2">
              <ul className="list-inside list-disc space-y-1">
                {validationErrors.map((e, i) => (
                  <li key={i} className="text-sm text-amber-300">{e}</li>
                ))}
              </ul>
            </div>
          )}

          <form onSubmit={handleSubmit} className="space-y-4">
            <div>
              <label htmlFor="current-password" className="mb-1.5 block text-xs font-medium text-muted">
                Current Password
              </label>
              <input
                id="current-password"
                type="password"
                autoComplete="current-password"
                value={currentPassword}
                onChange={(e) => setCurrentPassword(e.target.value)}
                required
                className="w-full rounded-sm border border-rule bg-surface-900 px-3 py-2 text-sm text-fg placeholder-dim focus:border-accent focus:outline-none focus:ring-1 focus:ring-accent"
                placeholder="••••••••••••"
              />
            </div>

            <div>
              <label htmlFor="new-password" className="mb-1.5 block text-xs font-medium text-muted">
                New Password
              </label>
              <input
                id="new-password"
                type="password"
                autoComplete="new-password"
                value={newPassword}
                onChange={(e) => setNewPassword(e.target.value)}
                required
                className="w-full rounded-sm border border-rule bg-surface-900 px-3 py-2 text-sm text-fg placeholder-dim focus:border-accent focus:outline-none focus:ring-1 focus:ring-accent"
                placeholder="••••••••••••"
              />
              {/* Strength indicator */}
              {newPassword && strength && (
                <div className="mt-2">
                  <div className="mb-1 flex items-center justify-between">
                    <span className="text-xs text-dim">Strength</span>
                    <span className={`text-xs font-medium ${strengthTextColor[strength]}`}>
                      {strengthLabel[strength]}
                    </span>
                  </div>
                  <div className="h-1.5 w-full overflow-hidden rounded-full bg-surface-700">
                    <div
                      className={`h-full rounded-full transition-all duration-300 ${strengthColor[strength]} ${strengthWidth[strength]}`}
                    />
                  </div>
                </div>
              )}
            </div>

            <div>
              <label htmlFor="confirm-password" className="mb-1.5 block text-xs font-medium text-muted">
                Confirm New Password
              </label>
              <input
                id="confirm-password"
                type="password"
                autoComplete="new-password"
                value={confirmPassword}
                onChange={(e) => setConfirmPassword(e.target.value)}
                required
                className={[
                  'w-full rounded-sm border bg-surface-900 px-3 py-2 text-sm text-fg placeholder-dim focus:outline-none focus:ring-1',
                  confirmPassword && confirmPassword !== newPassword
                    ? 'border-red-600 focus:border-red-500 focus:ring-red-500'
                    : 'border-rule focus:border-accent focus:ring-accent',
                ].join(' ')}
                placeholder="••••••••••••"
              />
            </div>

            <button
              type="submit"
              disabled={loading}
              className="w-full rounded-sm bg-accent px-4 py-2.5 text-sm font-semibold text-surface-900 shadow-sm transition-colors hover:bg-accent-hover focus:outline-none focus:ring-2 focus:ring-accent focus:ring-offset-2 focus:ring-offset-surface-950 disabled:cursor-not-allowed disabled:opacity-60"
            >
              {loading ? 'Updating…' : 'Update Password'}
            </button>
          </form>

          {/* Requirements note */}
          <div className="mt-4 rounded-sm border border-rule/40 bg-surface-900/50 px-3 py-2.5">
            <p className="mb-1.5 text-xs font-semibold text-muted">Password requirements</p>
            <ul className="space-y-0.5 text-xs text-dim">
              <li className={newPassword.length >= 12 ? 'text-emerald-400' : ''}>
                {newPassword.length >= 12 ? '✓' : '•'} At least 12 characters
              </li>
              <li className={/[A-Z]/.test(newPassword) ? 'text-emerald-400' : ''}>
                {/[A-Z]/.test(newPassword) ? '✓' : '•'} At least one uppercase letter
              </li>
              <li className={/[0-9]/.test(newPassword) ? 'text-emerald-400' : ''}>
                {/[0-9]/.test(newPassword) ? '✓' : '•'} At least one number
              </li>
              <li className={/[^A-Za-z0-9]/.test(newPassword) ? 'text-emerald-400' : ''}>
                {/[^A-Za-z0-9]/.test(newPassword) ? '✓' : '•'} At least one special character
              </li>
            </ul>
          </div>
        </div>
      </div>
    </div>
  )
}
