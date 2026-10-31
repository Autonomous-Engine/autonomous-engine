import { useState, useEffect } from 'react'
import { useApi } from '@/hooks/useApi'
import { useWebSocket } from '@/hooks/useWebSocket'

interface ServiceStatus {
  name: string
  status: string
  message: string
  critical: boolean
  last_check: string
}

export default function ServiceHealthBanner() {
  const [services, setServices] = useState<ServiceStatus[]>([])
  const [expanded, setExpanded] = useState(false)
  const [dismissed, setDismissed] = useState<Set<string>>(new Set())
  const api = useApi()
  const { lastMessage, subscribe, unsubscribe } = useWebSocket()

  useEffect(() => {
    api.get<{ services: ServiceStatus[] }>('/system/health')
      .then(res => setServices(res.services ?? []))
      .catch(() => {})
  }, [api])

  useEffect(() => {
    subscribe('service_health')
    return () => unsubscribe('service_health')
  }, [subscribe, unsubscribe])

  useEffect(() => {
    if (!lastMessage || lastMessage.channel !== 'service_health') return
    const data = lastMessage.data as { services: ServiceStatus[]; changes: ServiceStatus[] }
    if (data.services) {
      setServices(data.services)
      const recovered = new Set(dismissed)
      for (const c of (data.changes ?? [])) {
        if (c.status !== 'healthy') {
          recovered.delete(c.name)
        }
      }
      setDismissed(recovered)
    }
  }, [lastMessage, dismissed])

  const unhealthy = services.filter(s => s.status !== 'healthy' && !dismissed.has(s.name))

  if (unhealthy.length === 0) return null

  const criticalCount = unhealthy.filter(s => s.critical).length

  return (
    <div className={`border-b px-4 py-2 ${criticalCount > 0 ? 'border-red-500/30 bg-red-500/10' : 'border-yellow-500/30 bg-yellow-500/10'}`}>
      <div className="flex items-center justify-between">
        <button
          onClick={() => setExpanded(!expanded)}
          className="flex items-center gap-2 text-xs font-medium"
        >
          <span className={`h-2 w-2 rounded-full animate-pulse ${criticalCount > 0 ? 'bg-red-500' : 'bg-yellow-500'}`} />
          <span className={criticalCount > 0 ? 'text-red-400' : 'text-yellow-400'}>
            {unhealthy.length} service{unhealthy.length !== 1 ? 's' : ''} unhealthy
            {criticalCount > 0 && ` (${criticalCount} critical)`}
          </span>
          <svg
            xmlns="http://www.w3.org/2000/svg"
            viewBox="0 0 16 16"
            fill="currentColor"
            className={`h-3 w-3 text-dim transition-transform ${expanded ? 'rotate-180' : ''}`}
          >
            <path fillRule="evenodd" d="M4.22 6.22a.75.75 0 011.06 0L8 8.94l2.72-2.72a.75.75 0 111.06 1.06l-3.25 3.25a.75.75 0 01-1.06 0L4.22 7.28a.75.75 0 010-1.06z" clipRule="evenodd" />
          </svg>
        </button>
        <button
          onClick={() => setDismissed(new Set(unhealthy.map(s => s.name)))}
          className="text-xs text-dim hover:text-fg"
        >
          dismiss
        </button>
      </div>

      {expanded && (
        <div className="mt-2 space-y-1">
          {unhealthy.map(s => (
            <div key={s.name} className="flex items-center gap-3 rounded bg-surface-900/50 px-3 py-1.5 text-xs">
              <span className={`h-1.5 w-1.5 rounded-full ${s.critical ? 'bg-red-500' : 'bg-yellow-500'}`} />
              <span className="font-mono font-medium text-fg">{s.name}</span>
              <span className="text-dim">{s.message || s.status}</span>
              {s.critical && <span className="rounded bg-red-500/20 px-1.5 py-0.5 text-red-400">critical</span>}
            </div>
          ))}
        </div>
      )}
    </div>
  )
}
