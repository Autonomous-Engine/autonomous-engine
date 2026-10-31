import { useEffect, useMemo, useState } from 'react'
import { useWebSocket } from '@/hooks/useWebSocket'

// Matches internal/credmonitor/alert.go:AlertPayload
interface CredentialAlert {
  kind: 'credential'
  category: string
  key_name: string
  severity: 'info' | 'warning' | 'critical'
  status: 'expiring_soon' | 'expired' | 'invalid' | 'recovered'
  message: string
  credential_id: string
  expires_at?: string
  emitted: string
}

/**
 * CredentialAlertBanner subscribes to the "alerts" WebSocket channel and shows
 * a persistent banner at the top of the dashboard whenever a credential is
 * invalid, expired, or expiring soon. "Recovered" alerts clear the banner for
 * that credential.
 *
 * Place in the top-level Layout above the main content area.
 */
export default function CredentialAlertBanner() {
  const { lastMessage, subscribe, unsubscribe } = useWebSocket()
  const [active, setActive] = useState<Record<string, CredentialAlert>>({})

  useEffect(() => {
    subscribe('alerts')
    return () => unsubscribe('alerts')
  }, [subscribe, unsubscribe])

  useEffect(() => {
    if (!lastMessage || lastMessage.channel !== 'alerts') return
    const payload = lastMessage.data as CredentialAlert | null
    if (!payload || payload.kind !== 'credential') return

    setActive((prev) => {
      if (payload.status === 'recovered') {
        const next = { ...prev }
        delete next[payload.credential_id]
        return next
      }
      return { ...prev, [payload.credential_id]: payload }
    })
  }, [lastMessage])

  const worst = useMemo<CredentialAlert | null>(() => {
    const all = Object.values(active)
    if (all.length === 0) return null
    const crit = all.find((a) => a.severity === 'critical')
    if (crit) return crit
    const warn = all.find((a) => a.severity === 'warning')
    if (warn) return warn
    return null
  }, [active])

  if (!worst) return null

  const bgClass =
    worst.severity === 'critical'
      ? 'bg-rose-900/80 border-rose-700 text-rose-50'
      : 'bg-amber-900/80 border-amber-700 text-amber-50'

  return (
    <div
      className={`flex items-center gap-3 border-b px-4 py-2 text-sm ${bgClass}`}
      role="alert"
    >
      <span className="font-semibold tracking-wide uppercase text-xs">
        Credential
      </span>
      <span className="flex-1 truncate">{worst.message}</span>
      <a
        href="/admin/credentials"
        className="shrink-0 rounded-sm border border-current/40 px-2 py-0.5 text-xs font-medium hover:bg-white/10"
      >
        Rotate →
      </a>
    </div>
  )
}
