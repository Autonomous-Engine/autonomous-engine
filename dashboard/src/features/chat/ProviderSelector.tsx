import { useState, useEffect } from 'react'
import { useApi } from '@/hooks/useApi'
import { useChat } from './ChatProvider'
import type { LLMProvider } from './types'

interface Props {
  sessionId: string | null
}

export default function ProviderSelector({ sessionId }: Props) {
  const [providers, setProviders] = useState<LLMProvider[]>([])
  const [loading, setLoading] = useState(false)
  const [open, setOpen] = useState(false)
  const { sessionProvider, setSessionProvider, loadSessionProvider } = useChat()
  const api = useApi()

  useEffect(() => {
    if (!open) return
    setLoading(true)
    api.get<{ providers: LLMProvider[] }>('/llm-providers')
      .then(res => setProviders(res.providers ?? []))
      .catch(() => setProviders([]))
      .finally(() => setLoading(false))
  }, [open, api])

  async function handleSelect(provider: LLMProvider) {
    const sp = { id: provider.id, name: provider.name, provider_type: provider.provider_type, model: provider.model }
    if (sessionId) {
      try {
        await setSessionProvider(sessionId, sp)
      } catch (err) {
        console.error('Failed to set provider:', err)
      }
    } else {
      loadSessionProvider(sp)
    }
    setOpen(false)
  }

  const label = sessionProvider
    ? `${sessionProvider.name} (${sessionProvider.model || sessionProvider.provider_type})`
    : 'Default'

  return (
    <div className="relative">
      <button
        onClick={() => setOpen(!open)}
        className="flex items-center gap-1.5 rounded bg-surface-700/50 px-2 py-1 text-xs text-muted hover:bg-surface-700 hover:text-fg transition-colors"
      >
        <span className={`h-1.5 w-1.5 rounded-full ${sessionProvider ? 'bg-blue-400' : 'bg-dim'}`} />
        {label}
        <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" fill="currentColor" className="h-3 w-3">
          <path fillRule="evenodd" d="M4.22 6.22a.75.75 0 011.06 0L8 8.94l2.72-2.72a.75.75 0 111.06 1.06l-3.25 3.25a.75.75 0 01-1.06 0L4.22 7.28a.75.75 0 010-1.06z" clipRule="evenodd" />
        </svg>
      </button>

      {open && (
        <div className="absolute left-0 top-full z-20 mt-1 w-64 rounded border border-rule bg-surface-800 shadow-lg">
          {loading ? (
            <div className="px-3 py-2 text-xs text-dim">Loading providers...</div>
          ) : providers.length === 0 ? (
            <div className="px-3 py-2 text-xs text-dim">No providers found</div>
          ) : (
            <div className="max-h-48 overflow-y-auto py-1">
              {providers.map(p => (
                <button
                  key={p.id}
                  onClick={() => handleSelect(p)}
                  className={[
                    'w-full px-3 py-1.5 text-left text-xs hover:bg-surface-700 transition-colors',
                    sessionProvider?.id === p.id ? 'text-accent' : 'text-fg',
                  ].join(' ')}
                >
                  <div className="flex items-center gap-1.5">
                    <span className="font-medium">{p.name}</span>
                    {p.is_default && <span className="text-dim">(default)</span>}
                  </div>
                  <div className="text-dim">{p.provider_type} / {p.model}</div>
                </button>
              ))}
            </div>
          )}
        </div>
      )}
    </div>
  )
}
