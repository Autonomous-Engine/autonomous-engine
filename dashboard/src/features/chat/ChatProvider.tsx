import { createContext, useContext, useState, useCallback, type ReactNode } from 'react'
import { useApi } from '@/hooks/useApi'
import type { ChatContextValue, PageContext, SessionProduct, SessionProvider } from './types'

const ChatContext = createContext<ChatContextValue | null>(null)

export function useChat(): ChatContextValue {
  const ctx = useContext(ChatContext)
  if (!ctx) throw new Error('useChat must be used within ChatProvider')
  return ctx
}

export default function ChatProvider({ children }: { children: ReactNode }) {
  const [isOpen, setIsOpen] = useState(false)
  const [activeSessionId, setActiveSessionId] = useState<string | null>(null)
  const [pageContext, setPageContext] = useState<PageContext>({ page: 'pipeline' })
  const [sendingSessions, setSendingSessions] = useState<Set<string>>(new Set())
  const [sessionProduct, setSessionProductState] = useState<SessionProduct | null>(null)
  const [sessionProvider, setSessionProviderState] = useState<SessionProvider | null>(null)
  const api = useApi()

  const markSending = useCallback((sessionId: string) => {
    setSendingSessions(prev => new Set(prev).add(sessionId))
  }, [])

  const clearSending = useCallback((sessionId: string) => {
    setSendingSessions(prev => {
      const next = new Set(prev)
      next.delete(sessionId)
      return next
    })
  }, [])

  const setSessionProduct = useCallback(async (sessionId: string, product: SessionProduct) => {
    await api.put(`/chat/sessions/${sessionId}/product`, { product_id: product.id })
    setSessionProductState(product)
  }, [api])

  const loadSessionProduct = useCallback((product: SessionProduct | null) => {
    setSessionProductState(product)
  }, [])

  const setSessionProvider = useCallback(async (sessionId: string, provider: SessionProvider) => {
    await api.put(`/chat/sessions/${sessionId}/provider`, { provider_id: provider.id })
    setSessionProviderState(provider)
  }, [api])

  const loadSessionProvider = useCallback((provider: SessionProvider | null) => {
    setSessionProviderState(provider)
  }, [])

  const ensureSession = useCallback(async (ctx?: PageContext, workItemId?: string): Promise<string> => {
    const res = await api.post<{ id: string }>('/chat/sessions', {
      page_context: ctx || pageContext,
      work_item_id: workItemId || null,
    })
    const sid = res.id
    setActiveSessionId(sid)
    if (sessionProduct) {
      try {
        await api.put(`/chat/sessions/${sid}/product`, { product_id: sessionProduct.id })
      } catch { /* best-effort */ }
    }
    if (sessionProvider) {
      try {
        await api.put(`/chat/sessions/${sid}/provider`, { provider_id: sessionProvider.id })
      } catch { /* best-effort */ }
    }
    return sid
  }, [api, pageContext, sessionProduct, sessionProvider])

  const openChat = useCallback(async (opts?: { workItemId?: string; workItemTitle?: string }) => {
    setIsOpen(true)

    if (activeSessionId) return

    if (opts?.workItemId) {
      try {
        const res = await api.get<{ sessions: { id: string; active_work_item_id: string | null }[] }>('/chat/sessions?limit=50')
        const existing = res.sessions.find(s => s.active_work_item_id === opts.workItemId)
        if (existing) {
          setActiveSessionId(existing.id)
          return
        }
      } catch { /* proceed */ }
      try {
        await ensureSession(
          { page: 'work-item', workItemId: opts.workItemId, title: opts.workItemTitle },
          opts.workItemId,
        )
      } catch (err) {
        console.error('Failed to create chat session:', err)
      }
    }
  }, [activeSessionId, api, ensureSession])

  const closeChat = useCallback(() => {
    setIsOpen(false)
  }, [])

  const startNewSession = useCallback(() => {
    setActiveSessionId(null)
    setSessionProductState(null)
    setSessionProviderState(null)
  }, [])

  const value: ChatContextValue = {
    isOpen,
    activeSessionId,
    pageContext,
    sendingSessions,
    sessionProduct,
    sessionProvider,
    openChat,
    closeChat,
    setPageContext,
    setActiveSessionId,
    startNewSession,
    markSending,
    clearSending,
    setSessionProduct,
    loadSessionProduct,
    setSessionProvider,
    loadSessionProvider,
    ensureSession,
  }

  return <ChatContext.Provider value={value}>{children}</ChatContext.Provider>
}
