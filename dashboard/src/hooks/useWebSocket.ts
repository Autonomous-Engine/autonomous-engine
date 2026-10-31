import { useState, useEffect, useRef, useCallback } from 'react'
import type { WebSocketMessage } from '@/types'

interface UseWebSocketReturn {
  lastMessage: WebSocketMessage | null
  subscribe: (channel: string) => void
  unsubscribe: (channel: string) => void
  isConnected: boolean
}

const WS_URL =
  typeof window !== 'undefined'
    ? `${window.location.protocol === 'https:' ? 'wss' : 'ws'}://${window.location.host}/ws`
    : 'ws://localhost:8080/ws'

const BASE_RECONNECT_MS = 1_000
const MAX_RECONNECT_MS = 30_000

export function useWebSocket(): UseWebSocketReturn {
  const [lastMessage, setLastMessage] = useState<WebSocketMessage | null>(null)
  const [isConnected, setIsConnected] = useState(false)
  const wsRef = useRef<WebSocket | null>(null)
  const reconnectDelay = useRef(BASE_RECONNECT_MS)
  const reconnectTimer = useRef<ReturnType<typeof setTimeout> | null>(null)
  const subscribedChannels = useRef<Set<string>>(new Set())
  const isMounted = useRef(true)

  const sendSubscriptions = useCallback((ws: WebSocket) => {
    subscribedChannels.current.forEach((ch) => {
      ws.send(JSON.stringify({ action: 'subscribe', channel: ch }))
    })
  }, [])

  const connect = useCallback(() => {
    if (!isMounted.current) return

    const token = localStorage.getItem('auth_token')
    const url = token ? `${WS_URL}?token=${encodeURIComponent(token)}` : WS_URL
    const ws = new WebSocket(url)
    wsRef.current = ws

    ws.onopen = () => {
      if (!isMounted.current) return
      setIsConnected(true)
      reconnectDelay.current = BASE_RECONNECT_MS
      sendSubscriptions(ws)
    }

    ws.onmessage = (event: MessageEvent) => {
      if (!isMounted.current) return
      try {
        const msg = JSON.parse(event.data as string) as WebSocketMessage
        setLastMessage(msg)
      } catch {
        // ignore malformed messages
      }
    }

    ws.onclose = () => {
      if (!isMounted.current) return
      setIsConnected(false)
      wsRef.current = null
      // exponential backoff reconnect
      reconnectTimer.current = setTimeout(() => {
        reconnectDelay.current = Math.min(
          reconnectDelay.current * 2,
          MAX_RECONNECT_MS,
        )
        connect()
      }, reconnectDelay.current)
    }

    ws.onerror = () => {
      ws.close()
    }
  }, [sendSubscriptions])

  useEffect(() => {
    isMounted.current = true
    connect()
    return () => {
      isMounted.current = false
      if (reconnectTimer.current) clearTimeout(reconnectTimer.current)
      wsRef.current?.close()
    }
  }, [connect])

  const subscribe = useCallback(
    (channel: string) => {
      subscribedChannels.current.add(channel)
      if (wsRef.current?.readyState === WebSocket.OPEN) {
        wsRef.current.send(JSON.stringify({ action: 'subscribe', channel }))
      }
    },
    [],
  )

  const unsubscribe = useCallback((channel: string) => {
    subscribedChannels.current.delete(channel)
    if (wsRef.current?.readyState === WebSocket.OPEN) {
      wsRef.current.send(JSON.stringify({ action: 'unsubscribe', channel }))
    }
  }, [])

  return { lastMessage, subscribe, unsubscribe, isConnected }
}
