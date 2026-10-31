// dashboard/src/hooks/usePrompts.ts
//
// REST + WebSocket-driven hooks for the unified interaction surface.
// The existing `useWebSocket` (singleton) gives us `subscribe` + `lastMessage`;
// these hooks wrap it to produce prompt-specific reactive state.

import { useCallback, useEffect, useState } from 'react'
import type { Prompt, AnswerInput, PromptWSEvent } from '@/types/prompts'
import { useWebSocket } from './useWebSocket'

const PROMPTS_CHANNEL = 'prompts'
const TOKEN_KEY = 'auth_token'

function authHeaders(): Record<string, string> {
  const headers: Record<string, string> = { 'Content-Type': 'application/json' }
  const token = localStorage.getItem(TOKEN_KEY)
  if (token) headers['Authorization'] = `Bearer ${token}`
  return headers
}

function isPromptEvent(
  msg: unknown,
): msg is PromptWSEvent {
  if (!msg || typeof msg !== 'object') return false
  const t = (msg as { type?: unknown }).type
  return (
    t === 'prompt_requested' ||
    t === 'prompt_answered' ||
    t === 'prompt_revoked'
  )
}

/**
 * usePendingPromptsCount returns the count of pending prompts.
 * Refetches on every prompt WS event (requested/answered/revoked).
 */
export function usePendingPromptsCount(): { count: number; refresh: () => void } {
  const [count, setCount] = useState(0)
  const { subscribe, lastMessage } = useWebSocket()

  const refresh = useCallback(async () => {
    try {
      const res = await fetch('/api/prompts?state=pending', { headers: authHeaders() })
      if (!res.ok) return
      const list: Prompt[] = await res.json()
      setCount(list.length)
    } catch {
      // Swallow network errors — bell stays at last-known count.
    }
  }, [])

  useEffect(() => {
    subscribe(PROMPTS_CHANNEL)
    refresh()
  }, [subscribe, refresh])

  useEffect(() => {
    if (isPromptEvent(lastMessage)) {
      if (lastMessage.type === 'prompt_requested') {
        setCount((c) => c + 1)
      } else {
        setCount((c) => Math.max(0, c - 1))
      }
    }
  }, [lastMessage])

  return { count, refresh }
}

/**
 * usePromptList returns the current list of prompts with optional filters.
 * Refetches on every prompt WS event.
 */
export function usePromptList(
  filters?: { productId?: string; state?: string },
): { prompts: Prompt[]; refresh: () => void; loading: boolean } {
  const [prompts, setPrompts] = useState<Prompt[]>([])
  const [loading, setLoading] = useState(true)
  const { subscribe, lastMessage } = useWebSocket()

  const refresh = useCallback(async () => {
    const qs = new URLSearchParams()
    if (filters?.productId) qs.set('product_id', filters.productId)
    if (filters?.state) qs.set('state', filters.state)
    const url = '/api/prompts' + (qs.toString() ? '?' + qs.toString() : '')
    setLoading(true)
    try {
      const res = await fetch(url, { headers: authHeaders() })
      if (res.ok) {
        setPrompts(await res.json())
      }
    } finally {
      setLoading(false)
    }
  }, [filters?.productId, filters?.state])

  useEffect(() => {
    subscribe(PROMPTS_CHANNEL)
    refresh()
  }, [subscribe, refresh])

  useEffect(() => {
    if (isPromptEvent(lastMessage)) refresh()
  }, [lastMessage, refresh])

  return { prompts, refresh, loading }
}

/**
 * usePrompt returns a single prompt by id, or null while loading / not found.
 */
export function usePrompt(id: string | null): {
  prompt: Prompt | null
  loading: boolean
  refresh: () => void
} {
  const [prompt, setPrompt] = useState<Prompt | null>(null)
  const [loading, setLoading] = useState(false)

  const refresh = useCallback(async () => {
    if (!id) {
      setPrompt(null)
      return
    }
    setLoading(true)
    try {
      const res = await fetch(`/api/prompts/${id}`, { headers: authHeaders() })
      if (!res.ok) {
        setPrompt(null)
        return
      }
      setPrompt(await res.json())
    } finally {
      setLoading(false)
    }
  }, [id])

  useEffect(() => {
    refresh()
  }, [refresh])

  return { prompt, loading, refresh }
}

/**
 * answerPrompt POSTs the user's answer to a prompt.
 */
export async function answerPrompt(
  id: string,
  input: AnswerInput,
): Promise<void> {
  const res = await fetch(`/api/prompts/${id}/answer`, {
    method: 'POST',
    headers: authHeaders(),
    body: JSON.stringify(input),
  })
  if (!res.ok) {
    throw new Error(`answerPrompt: ${res.status} ${await res.text()}`)
  }
}

/**
 * revokePrompt POSTs a revocation reason for a prompt.
 */
export async function revokePrompt(id: string, reason: string): Promise<void> {
  const res = await fetch(`/api/prompts/${id}/revoke`, {
    method: 'POST',
    headers: authHeaders(),
    body: JSON.stringify({ reason }),
  })
  if (!res.ok) {
    throw new Error(`revokePrompt: ${res.status} ${await res.text()}`)
  }
}

/**
 * revokeCodexEntry POSTs a revoke request for a codex entry (any section).
 * The backend cascades through the existing revoked-consumer: codex entry
 * state → 'revoked', codex version bumped, qdrant point payload.revoked
 * flipped. Safe to call on any codex entry regardless of source.
 */
export async function revokeCodexEntry(
  id: string,
  table: 'codex_modules' | 'codex_patterns' | 'codex_anti_patterns' | 'codex_sections',
  reason?: string,
): Promise<void> {
  const res = await fetch('/api/codex/entries/revoke', {
    method: 'POST',
    headers: authHeaders(),
    body: JSON.stringify({ id, table, reason: reason ?? '' }),
  })
  if (!res.ok) {
    throw new Error(`revokeCodexEntry: ${res.status} ${await res.text()}`)
  }
}
