import { useState, useEffect, useCallback } from 'react'
import { useApi } from '@/hooks/useApi'
import type { ChatSession } from './types'

export function useChatSessions() {
  const [sessions, setSessions] = useState<ChatSession[]>([])
  const [loading, setLoading] = useState(false)
  const api = useApi()

  const loadSessions = useCallback(async () => {
    setLoading(true)
    try {
      const res = await api.get<{ sessions: ChatSession[] }>('/chat/sessions?limit=50')
      setSessions(res.sessions)
    } catch (err) {
      console.error('Failed to load chat sessions:', err)
    } finally {
      setLoading(false)
    }
  }, [api])

  useEffect(() => {
    loadSessions()
  }, [loadSessions])

  const deleteSession = useCallback(async (id: string) => {
    try {
      await api.del(`/chat/sessions/${id}`)
      setSessions(prev => prev.filter(s => s.id !== id))
    } catch (err) {
      console.error('Failed to delete session:', err)
    }
  }, [api])

  const archiveSession = useCallback(async (id: string) => {
    try {
      await api.post(`/chat/sessions/${id}/archive`)
      setSessions(prev => prev.filter(s => s.id !== id))
    } catch (err) {
      console.error('Failed to archive session:', err)
    }
  }, [api])

  const updateTitle = useCallback(async (id: string, title: string) => {
    try {
      await api.patch(`/chat/sessions/${id}`, { title })
      setSessions(prev => prev.map(s => s.id === id ? { ...s, title } : s))
    } catch (err) {
      console.error('Failed to update session title:', err)
    }
  }, [api])

  return { sessions, loading, loadSessions, deleteSession, archiveSession, updateTitle }
}
