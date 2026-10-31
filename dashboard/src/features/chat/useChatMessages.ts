import { useState, useEffect, useCallback, useRef } from 'react'
import { useApi } from '@/hooks/useApi'
import { useWebSocket } from '@/hooks/useWebSocket'
import { useChat } from './ChatProvider'
import type { ChatMessage, PageContext } from './types'

const SEND_TIMEOUT_MS = 120_000

export function useChatMessages(sessionId: string | null) {
  const [messages, setMessages] = useState<ChatMessage[]>([])
  const [loading, setLoading] = useState(false)
  const { sendingSessions, markSending, clearSending, ensureSession } = useChat()
  const sending = sessionId ? sendingSessions.has(sessionId) : false
  const api = useApi()
  const { lastMessage, subscribe, unsubscribe } = useWebSocket()
  const subscribedRef = useRef(false)
  const sendTimeoutRef = useRef<ReturnType<typeof setTimeout> | null>(null)

  useEffect(() => {
    if (!sessionId) {
      setMessages([])
      return
    }
    setLoading(true)
    api.get<{ messages: ChatMessage[] }>(`/chat/sessions/${sessionId}/messages`)
      .then(res => {
        setMessages(res.messages)
        if (sendingSessions.has(sessionId)) {
          const last = res.messages[res.messages.length - 1]
          if (last && last.role === 'assistant') {
            clearSending(sessionId)
          }
        }
      })
      .catch(err => console.error('Failed to load messages:', err))
      .finally(() => setLoading(false))
  }, [sessionId, api])

  useEffect(() => {
    if (!subscribedRef.current) {
      subscribe('chat')
      subscribedRef.current = true
    }
    return () => {
      if (subscribedRef.current) {
        unsubscribe('chat')
        subscribedRef.current = false
      }
    }
  }, [subscribe, unsubscribe])

  useEffect(() => {
    if (!lastMessage || lastMessage.channel !== 'chat' || !sessionId) return
    const data = lastMessage.data as {
      session_id: string
      message_id: string
      content: string
      tool_calls_summary?: { tool: string; args_preview: string }[]
      proposal?: ChatMessage['proposal']
    }
    if (data.session_id !== sessionId) return
    if (sendTimeoutRef.current) clearTimeout(sendTimeoutRef.current)
    clearSending(sessionId)
    setMessages(prev => [
      ...prev.filter(m => m.id !== 'optimistic'),
      {
        id: data.message_id,
        role: 'assistant',
        content: data.content,
        tool_calls: data.tool_calls_summary,
        proposal: data.proposal,
        created_at: new Date().toISOString(),
      },
    ])
  }, [lastMessage, sessionId, clearSending])

  const sendMessage = useCallback(async (content: string, pageContext?: PageContext) => {
    if (!content.trim()) return

    let sid = sessionId
    if (!sid) {
      try {
        sid = await ensureSession(pageContext)
      } catch (err) {
        console.error('Failed to create chat session:', err)
        return
      }
    }

    markSending(sid)
    if (sendTimeoutRef.current) clearTimeout(sendTimeoutRef.current)
    sendTimeoutRef.current = setTimeout(() => {
      clearSending(sid)
    }, SEND_TIMEOUT_MS)

    setMessages(prev => [
      ...prev,
      {
        id: 'optimistic',
        role: 'user',
        content,
        created_at: new Date().toISOString(),
      },
    ])
    try {
      const res = await api.post<{ message_id: string }>(`/chat/sessions/${sid}/messages`, {
        content,
        page_context: pageContext,
      })
      setMessages(prev => prev.map(m => m.id === 'optimistic' ? { ...m, id: res.message_id } : m))
    } catch (err) {
      console.error('Failed to send message:', err)
      if (sendTimeoutRef.current) clearTimeout(sendTimeoutRef.current)
      clearSending(sid)
      setMessages(prev => prev.filter(m => m.id !== 'optimistic'))
    }
  }, [sessionId, api, markSending, clearSending, ensureSession])

  const approveProposal = useCallback(async (messageId: string) => {
    if (!sessionId) return
    try {
      await api.post(`/chat/sessions/${sessionId}/proposals/${messageId}/approve`)
      setMessages(prev => prev.map(m =>
        m.id === messageId && m.proposal
          ? { ...m, proposal: { ...m.proposal, status: 'approved' as const } }
          : m
      ))
    } catch (err) {
      console.error('Failed to approve proposal:', err)
    }
  }, [sessionId, api])

  const rejectProposal = useCallback(async (messageId: string) => {
    if (!sessionId) return
    try {
      await api.post(`/chat/sessions/${sessionId}/proposals/${messageId}/reject`)
      setMessages(prev => prev.map(m =>
        m.id === messageId && m.proposal
          ? { ...m, proposal: { ...m.proposal, status: 'rejected' as const } }
          : m
      ))
    } catch (err) {
      console.error('Failed to reject proposal:', err)
    }
  }, [sessionId, api])

  return { messages, loading, sending, sendMessage, approveProposal, rejectProposal }
}
