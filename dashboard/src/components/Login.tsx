import { useState, useEffect } from 'react'
import { useNavigate } from 'react-router-dom'
import { useApi } from '@/hooks/useApi'
import type { AuthProvider } from '@/types'

export default function Login() {
  const navigate = useNavigate()
  const { get, post } = useApi()

  const [providers, setProviders] = useState<AuthProvider[]>([])
  const [localExpanded, setLocalExpanded] = useState(false)

  // Local form state
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')

  // LDAP form state
  const [ldapUsername, setLdapUsername] = useState('')
  const [ldapPassword, setLdapPassword] = useState('')

  const [loading, setLoading] = useState(false)
  const [error, setError] = useState('')

  useEffect(() => {
    // Check for token in URL (SAML callback)
    const params = new URLSearchParams(window.location.search)
    const urlToken = params.get('token')
    if (urlToken) {
      localStorage.setItem('auth_token', urlToken)
      navigate('/')
      return
    }

    // If already logged in, redirect
    if (localStorage.getItem('auth_token')) {
      navigate('/')
      return
    }

    get<AuthProvider[]>('/auth/providers')
      .then((data) => setProviders(data))
      .catch(() => {
        // Default to local if providers endpoint fails
        setProviders([
          { id: 'local', provider_type: 'local', name: 'Local', enabled: true, is_primary: true, config: {} },
        ])
      })
  }, [get, navigate])

  const samlProvider = providers.find((p) => p.provider_type === 'saml' && p.enabled)
  const ldapProvider = providers.find((p) => p.provider_type === 'ldap' && p.enabled)
  const localProvider = providers.find((p) => p.provider_type === 'local')
  // samlProvider and ldapProvider are used directly in the JSX below

  async function handleLocalSubmit(e: React.FormEvent) {
    e.preventDefault()
    setError('')
    setLoading(true)
    try {
      const res = await post<{ token: string; password_change_required?: boolean }>(
        '/auth/login',
        { email, password },
      )
      localStorage.setItem('auth_token', res.token)
      if (res.password_change_required) {
        navigate('/change-password')
      } else {
        navigate('/')
      }
    } catch (err: unknown) {
      const message = err instanceof Error ? err.message : 'Login failed'
      if (message.toLowerCase().includes('locked')) {
        setError('Account is locked. Please contact an administrator.')
      } else {
        setError('Invalid credentials. Please try again.')
      }
    } finally {
      setLoading(false)
    }
  }

  async function handleLdapSubmit(e: React.FormEvent) {
    e.preventDefault()
    setError('')
    setLoading(true)
    try {
      const res = await post<{ token: string; password_change_required?: boolean }>(
        '/auth/login/ldap',
        { username: ldapUsername, password: ldapPassword },
      )
      localStorage.setItem('auth_token', res.token)
      if (res.password_change_required) {
        navigate('/change-password')
      } else {
        navigate('/')
      }
    } catch (err: unknown) {
      const message = err instanceof Error ? err.message : 'Login failed'
      if (message.toLowerCase().includes('locked')) {
        setError('Account is locked. Please contact an administrator.')
      } else {
        setError('Invalid corporate credentials. Please try again.')
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
            <h1 className="text-xl font-semibold text-fg">Autonomous Engine</h1>
            <p className="mt-1 text-sm text-muted">Sign in to your account</p>
          </div>
        </div>

        {/* Card */}
        <div className="rounded-sm border border-rule bg-surface-950 p-6 shadow-xl">
          {error && (
            <div className="mb-4 rounded-sm border border-red-700/40 bg-red-900/20 px-3 py-2 text-sm text-red-300">
              {error}
            </div>
          )}

          {/* SAML SSO button — shown when SAML is enabled */}
          {samlProvider && (
            <div className="mb-4">
              <button
                type="button"
                onClick={() => { window.location.href = '/api/auth/saml/login' }}
                className="flex w-full items-center justify-center gap-2 rounded-sm bg-accent px-4 py-2.5 text-sm font-semibold text-surface-900 shadow-sm transition-colors hover:bg-accent-hover focus:outline-none focus:ring-2 focus:ring-accent focus:ring-offset-2 focus:ring-offset-surface-950"
              >
                <svg className="h-4 w-4" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={2}>
                  <path strokeLinecap="round" strokeLinejoin="round" d="M15 7a2 2 0 012 2m4 0a6 6 0 01-7.743 5.743L11 17H9v2H7v2H4a1 1 0 01-1-1v-2.586a1 1 0 01.293-.707l5.964-5.964A6 6 0 1121 9z" />
                </svg>
                Sign in with SSO
              </button>
            </div>
          )}

          {/* LDAP form — shown when LDAP is enabled and no SAML */}
          {ldapProvider && (
            <form onSubmit={handleLdapSubmit} className="space-y-4">
              <div>
                <p className="mb-3 text-center text-xs font-semibold uppercase tracking-widest text-dim">
                  Corporate Login
                </p>
              </div>
              <div>
                <label htmlFor="ldap-username" className="mb-1.5 block text-xs font-medium text-muted">
                  Username
                </label>
                <input
                  id="ldap-username"
                  type="text"
                  autoComplete="username"
                  value={ldapUsername}
                  onChange={(e) => setLdapUsername(e.target.value)}
                  required
                  className="w-full rounded-sm border border-rule bg-surface-900 px-3 py-2 text-sm text-fg placeholder-dim focus:border-accent focus:outline-none focus:ring-1 focus:ring-accent"
                  placeholder="domain\\username"
                />
              </div>
              <div>
                <label htmlFor="ldap-password" className="mb-1.5 block text-xs font-medium text-muted">
                  Password
                </label>
                <input
                  id="ldap-password"
                  type="password"
                  autoComplete="current-password"
                  value={ldapPassword}
                  onChange={(e) => setLdapPassword(e.target.value)}
                  required
                  className="w-full rounded-sm border border-rule bg-surface-900 px-3 py-2 text-sm text-fg placeholder-dim focus:border-accent focus:outline-none focus:ring-1 focus:ring-accent"
                  placeholder="••••••••••••"
                />
              </div>
              <button
                type="submit"
                disabled={loading}
                className="w-full rounded-sm bg-accent px-4 py-2.5 text-sm font-semibold text-surface-900 shadow-sm transition-colors hover:bg-accent-hover focus:outline-none focus:ring-2 focus:ring-accent focus:ring-offset-2 focus:ring-offset-surface-950 disabled:cursor-not-allowed disabled:opacity-60"
              >
                {loading ? 'Signing in…' : 'Corporate Login'}
              </button>
            </form>
          )}

          {/* Divider between SAML/LDAP and local */}
          {(samlProvider || ldapProvider) && localProvider && (
            <div className="my-4 flex items-center gap-3">
              <div className="flex-1 border-t border-rule" />
              <button
                type="button"
                onClick={() => setLocalExpanded((v) => !v)}
                className="text-xs text-dim transition-colors hover:text-muted"
              >
                {localExpanded ? 'Hide local login' : 'Or use local login'}
              </button>
              <div className="flex-1 border-t border-rule" />
            </div>
          )}

          {/* Local login form */}
          {(!samlProvider && !ldapProvider) || localExpanded ? (
            <form onSubmit={handleLocalSubmit} className="space-y-4">
              {(!samlProvider && !ldapProvider) && (
                <div>
                  <p className="mb-3 text-center text-xs font-semibold uppercase tracking-widest text-dim">
                    Local Account
                  </p>
                </div>
              )}
              <div>
                <label htmlFor="email" className="mb-1.5 block text-xs font-medium text-muted">
                  Email
                </label>
                <input
                  id="email"
                  type="email"
                  autoComplete="email"
                  value={email}
                  onChange={(e) => setEmail(e.target.value)}
                  required
                  className="w-full rounded-sm border border-rule bg-surface-900 px-3 py-2 text-sm text-fg placeholder-dim focus:border-accent focus:outline-none focus:ring-1 focus:ring-accent"
                  placeholder="you@example.com"
                />
              </div>
              <div>
                <label htmlFor="password" className="mb-1.5 block text-xs font-medium text-muted">
                  Password
                </label>
                <input
                  id="password"
                  type="password"
                  autoComplete="current-password"
                  value={password}
                  onChange={(e) => setPassword(e.target.value)}
                  required
                  className="w-full rounded-sm border border-rule bg-surface-900 px-3 py-2 text-sm text-fg placeholder-dim focus:border-accent focus:outline-none focus:ring-1 focus:ring-accent"
                  placeholder="••••••••••••"
                />
              </div>
              <button
                type="submit"
                disabled={loading}
                className="w-full rounded-sm bg-accent px-4 py-2.5 text-sm font-semibold text-surface-900 shadow-sm transition-colors hover:bg-accent-hover focus:outline-none focus:ring-2 focus:ring-accent focus:ring-offset-2 focus:ring-offset-surface-950 disabled:cursor-not-allowed disabled:opacity-60"
              >
                {loading ? 'Signing in…' : 'Sign In'}
              </button>
            </form>
          ) : null}
        </div>
      </div>
    </div>
  )
}
