import { useState, useEffect } from 'react'
import { NavLink, Outlet, useNavigate } from 'react-router-dom'
import { useWebSocket } from '@/hooks/useWebSocket'
import { useTheme } from '@/hooks/useTheme'
import CredentialAlertBanner from './CredentialAlertBanner'
import ServiceHealthBanner from './ServiceHealthBanner'
import IndexingActivity from './IndexingActivity'
import { NotificationBell } from './NotificationBell'
import { PromptSheet } from './PromptSheet'
import ChatProvider from '@/features/chat/ChatProvider'
import ChatOverlay from '@/features/chat/ChatOverlay'

interface NavItem {
  to: string
  label: string
  icon: string
}

const NAV_ITEMS: NavItem[] = [
  { to: '/', label: 'Pipeline', icon: '⬡' },
  { to: '/agents', label: 'Agents', icon: '⚙' },
  { to: '/approvals', label: 'Approvals', icon: '✓' },
  { to: '/codex', label: 'Codex', icon: '⊡' },
  { to: '/code', label: 'Code', icon: '⌘' },
  { to: '/commits', label: 'Commits', icon: '⊙' },
  { to: '/metrics', label: 'Metrics', icon: '▸' },
  { to: '/settings', label: 'Settings', icon: '⚑' },
  { to: '/chat', label: 'Chat', icon: '◬' },
]

const ADMIN_NAV_ITEMS: NavItem[] = [
  { to: '/admin/products', label: 'Products', icon: '◧' },
  { to: '/admin/pending-changes', label: 'PR Gates', icon: '⊘' },
  { to: '/admin/credentials', label: 'Credentials', icon: '⚿' },
  { to: '/admin/providers', label: 'Providers', icon: '◈' },
  { to: '/admin/embeddings', label: 'Embeddings', icon: '⋯' },
  { to: '/admin/reindex', label: 'Reindex', icon: '↻' },
  { to: '/admin/integrations', label: 'Integrations', icon: '⇄' },
  { to: '/admin/system', label: 'System', icon: '⚒' },
  { to: '/admin/audit', label: 'Audit', icon: '≡' },
  { to: '/admin/auth', label: 'Auth', icon: '⚷' },
  { to: '/admin/users', label: 'Users', icon: '◉' },
  { to: '/admin/prompts', label: 'Prompts', icon: '✎' },
  { to: '/admin/agents', label: 'Agents', icon: '⊕' },
  { to: '/admin/config-versions', label: 'Config Versions', icon: '#' },
  { to: '/admin/rollouts', label: 'Rollouts', icon: '⇪' },
  { to: '/admin/webhook/dead-letters', label: 'Dead Letters', icon: '✗' },
  { to: '/admin/codex/watched-branches', label: 'Watched Branches', icon: '⌥' },
]

export default function Layout() {
  const { isConnected } = useWebSocket()
  const { theme, toggle: toggleTheme } = useTheme()
  const navigate = useNavigate()
  const [promptSheetOpen, setPromptSheetOpen] = useState(false)
  const [appVersion, setAppVersion] = useState('...')

  useEffect(() => {
    fetch('/api/version')
      .then(r => r.json())
      .then(d => setAppVersion(d.version ?? 'dev'))
      .catch(() => setAppVersion('dev'))
  }, [])

  function handleLogout() {
    localStorage.removeItem('auth_token')
    navigate('/login')
  }

  return (
    <ChatProvider>
    <div className="flex h-screen overflow-hidden bg-surface-900">
      {/* Sidebar */}
      <aside className="flex w-56 flex-col border-r border-rule bg-surface-950">
        {/* Logo / brand */}
        <div className="flex h-14 items-center gap-2.5 border-b border-rule px-4">
          <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32" className="h-7 w-7">
            <rect width="32" height="32" rx="3" fill="#0a0c10"/>
            <rect x="4" y="4" width="24" height="24" rx="1" fill="none" stroke="#c8fc5b" strokeWidth="1.25"/>
            <text x="14.5" y="20" fontFamily="Geist Mono,ui-monospace,monospace" fontSize="11" fontWeight="500" fill="#c8fc5b" textAnchor="middle" letterSpacing="0.5">AE</text>
            <polygon points="25.5,23.7 26.1,24.5 27.1,24.6 26.7,25.5 27.1,26.4 26.1,26.5 25.5,27.3 24.9,26.5 23.9,26.4 24.3,25.5 23.9,24.6 24.9,24.5" fill="#c8fc5b"/>
            <circle cx="25.5" cy="25.5" r="0.55" fill="#0a0c10"/>
          </svg>
          <span className="font-display text-sm tracking-wide text-fg">
            Autonomous Engine
          </span>
        </div>

        {/* Navigation */}
        <nav className="flex-1 overflow-y-auto px-2 py-3">
          <div className="space-y-0.5">
            {NAV_ITEMS.map(({ to, label, icon }) => (
              <NavLink
                key={to}
                to={to}
                end={to === '/'}
                className={({ isActive }) =>
                  [
                    'flex items-center gap-3 rounded-sm px-3 py-2 font-mono text-sm transition-colors',
                    isActive
                      ? 'bg-accent/10 text-accent'
                      : 'text-muted hover:bg-surface-800 hover:text-fg',
                  ].join(' ')
                }
              >
                <span className="w-4 text-center text-base leading-none">
                  {icon}
                </span>
                {label}
              </NavLink>
            ))}
          </div>

          {/* Admin section */}
          <div className="mt-4">
            <div className="mb-2 border-t border-rule pt-3">
              <p className="section-label px-3">Admin</p>
            </div>
            <div className="space-y-0.5">
              {ADMIN_NAV_ITEMS.map(({ to, label, icon }) => (
                <NavLink
                  key={to}
                  to={to}
                  className={({ isActive }) =>
                    [
                      'flex items-center gap-3 rounded-sm px-3 py-2 font-mono text-sm transition-colors',
                      isActive
                        ? 'bg-accent/10 text-accent'
                        : 'text-muted hover:bg-surface-800 hover:text-fg',
                    ].join(' ')
                  }
                >
                  <span className="w-4 text-center text-base leading-none">
                    {icon}
                  </span>
                  {label}
                </NavLink>
              ))}
            </div>
          </div>
        </nav>

        {/* Footer */}
        <div className="border-t border-rule px-4 py-3 space-y-1">
          <button
            onClick={toggleTheme}
            className="flex w-full items-center gap-3 rounded-sm px-3 py-2 font-mono text-sm text-muted transition-colors hover:bg-surface-800 hover:text-fg"
          >
            <span className="w-4 text-center text-base leading-none">
              {theme === 'dark' ? '☀' : '☾'}
            </span>
            {theme === 'dark' ? 'Light mode' : 'Dark mode'}
          </button>
          <button
            onClick={handleLogout}
            className="flex w-full items-center gap-3 rounded-sm px-3 py-2 font-mono text-sm text-muted transition-colors hover:bg-surface-800 hover:text-fg"
          >
            <span className="w-4 text-center text-base leading-none">⏻</span>
            Logout
          </button>
          <p className="px-3 font-mono text-xs text-dim">{appVersion}</p>
        </div>
      </aside>

      {/* Main content */}
      <div className="flex flex-1 flex-col overflow-hidden">
        {/* Top bar */}
        <header className="flex h-14 items-center justify-between border-b border-rule bg-surface-950 px-6">
          <h1 className="font-display text-sm text-fg">
            Autonomous Engineering System
          </h1>
          <div className="flex items-center gap-3">
            <NotificationBell onOpen={() => setPromptSheetOpen(true)} />
            <span
              className={[
                'h-2 w-2 rounded-full',
                isConnected ? 'bg-accent' : 'bg-red-500',
              ].join(' ')}
            />
            <span className="font-mono text-xs text-muted">
              {isConnected ? 'Live' : 'Disconnected'}
            </span>
          </div>
        </header>

        <PromptSheet open={promptSheetOpen} onClose={() => setPromptSheetOpen(false)} />

        {/* Service health banner */}
        <ServiceHealthBanner />

        {/* Credential health banner */}
        <CredentialAlertBanner />

        {/* Live indexing activity */}
        <IndexingActivity />

        {/* Page content */}
        <main className="relative z-10 flex-1 overflow-y-auto p-6">
          <Outlet />
        </main>
      </div>
    </div>
    <ChatOverlay />
    </ChatProvider>
  )
}
