import { useEffect, useState, useRef, useCallback } from 'react'
import { useApi } from '@/hooks/useApi'
import { useWebSocket } from '@/hooks/useWebSocket'
import Markdown from './Markdown'

interface ChatMessage {
  id: string
  role: 'user' | 'assistant'
  content: string
  tool_calls?: { tool: string; args_preview: string }[]
  created_at: string
}

interface ChatPanelProps {
  workItemId: string
  workItemTitle: string
  isOpen: boolean
  onClose: () => void
}

export default function ChatPanel({ workItemId, workItemTitle, isOpen, onClose }: ChatPanelProps) {
  const { get, post } = useApi()
  const { lastMessage, subscribe, unsubscribe } = useWebSocket()
  const [messages, setMessages] = useState<ChatMessage[]>([])
  const [input, setInput] = useState('')
  const [sending, setSending] = useState(false)
  const [sessionId, setSessionId] = useState<string | null>(null)
  const [loaded, setLoaded] = useState(false)
  const scrollRef = useRef<HTMLDivElement>(null)
  const inputRef = useRef<HTMLTextAreaElement>(null)

  // Load history on open
  useEffect(() => {
    if (!isOpen || loaded) return
    get<{ session_id: string; messages: ChatMessage[] }>(`/chat/history?work_item_id=${workItemId}`)
      .then((data) => {
        setSessionId(data.session_id)
        setMessages(data.messages)
        setLoaded(true)
      })
      .catch((err) => {
        console.error('Failed to load chat history:', err)
        setLoaded(true)
      })
  }, [isOpen, loaded, workItemId, get])

  // Subscribe to chat WS channel
  useEffect(() => {
    if (!isOpen) return
    subscribe('chat')
    return () => unsubscribe('chat')
  }, [isOpen, subscribe, unsubscribe])

  // Handle incoming WS messages
  useEffect(() => {
    if (!lastMessage || lastMessage.channel !== 'chat') return
    const data = lastMessage.data as {
      work_item_id: string
      session_id: string
      message_id: string
      content: string
      tool_calls_summary?: { tool: string; args_preview: string }[]
      timestamp: string
    }
    if (data.work_item_id !== workItemId) return
    if (sessionId && data.session_id !== sessionId) return

    setMessages((prev) => {
      if (prev.some((m) => m.id === data.message_id)) return prev
      return [
        ...prev,
        {
          id: data.message_id,
          role: 'assistant',
          content: data.content,
          tool_calls: data.tool_calls_summary,
          created_at: data.timestamp,
        },
      ]
    })
    setSending(false)
  }, [lastMessage, workItemId, sessionId])

  // Auto-scroll
  useEffect(() => {
    if (scrollRef.current) {
      scrollRef.current.scrollTop = scrollRef.current.scrollHeight
    }
  }, [messages, sending])

  // Focus input on open
  useEffect(() => {
    if (isOpen && inputRef.current) {
      setTimeout(() => inputRef.current?.focus(), 100)
    }
  }, [isOpen])

  const handleSend = useCallback(async () => {
    if (!input.trim() || sending) return
    const message = input.trim()
    setInput('')
    setSending(true)

    // Optimistically add user message
    const tempId = `temp-${Date.now()}`
    setMessages((prev) => [
      ...prev,
      { id: tempId, role: 'user', content: message, created_at: new Date().toISOString() },
    ])

    try {
      const resp = await post<{ session_id: string; status: string }>('/chat/send', {
        work_item_id: workItemId,
        message,
      })
      setSessionId(resp.session_id)
    } catch (err) {
      console.error('Failed to send message:', err)
      setSending(false)
      setMessages((prev) => prev.filter((m) => m.id !== tempId))
    }
  }, [input, sending, workItemId, post])

  const handleKeyDown = useCallback(
    (e: React.KeyboardEvent) => {
      if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) {
        e.preventDefault()
        handleSend()
      }
    },
    [handleSend],
  )

  if (!isOpen) return null

  return (
    <div className="fixed top-0 right-0 h-full w-[440px] bg-surface-900 border-l border-rule shadow-2xl z-50 flex flex-col">
      {/* Header */}
      <div className="flex items-center justify-between px-4 py-3 border-b border-rule bg-surface-850">
        <div className="flex-1 min-w-0">
          <h3 className="text-sm font-semibold text-fg truncate">Chat</h3>
          <p className="text-xs text-dim truncate">{workItemTitle}</p>
        </div>
        <button
          type="button"
          onClick={onClose}
          className="ml-2 p-1 rounded text-dim hover:text-fg hover:bg-surface-700 transition-colors"
        >
          <svg className="h-4 w-4" fill="none" stroke="currentColor" strokeWidth={2} viewBox="0 0 24 24">
            <path strokeLinecap="round" strokeLinejoin="round" d="M6 18L18 6M6 6l12 12" />
          </svg>
        </button>
      </div>

      {/* Messages */}
      <div ref={scrollRef} className="flex-1 overflow-y-auto px-4 py-3 space-y-3">
        {messages.length === 0 && !sending && (
          <div className="text-center text-dim text-xs py-8">
            Ask the agent about this work item, its code, or request changes.
          </div>
        )}

        {messages.map((msg) => (
          <div key={msg.id}>
            {/* Tool call chips */}
            {msg.role === 'assistant' && msg.tool_calls && msg.tool_calls.length > 0 && (
              <div className="flex flex-wrap gap-1 mb-1.5">
                {msg.tool_calls.map((tc, i) => (
                  <span
                    key={i}
                    className="inline-flex items-center gap-1 rounded bg-amber-500/10 border border-amber-500/20 px-1.5 py-0.5 text-[10px] text-amber-300"
                  >
                    {tc.tool}
                  </span>
                ))}
              </div>
            )}

            <div
              className={[
                'rounded-lg px-3 py-2 text-sm max-w-[85%]',
                msg.role === 'user'
                  ? 'ml-auto bg-accent/20 text-fg border border-accent/30'
                  : 'mr-auto bg-surface-800 text-fg border border-rule',
              ].join(' ')}
            >
              {msg.role === 'assistant' ? (
                <Markdown>{msg.content}</Markdown>
              ) : (
                <p className="whitespace-pre-wrap">{msg.content}</p>
              )}
            </div>
          </div>
        ))}

        {sending && (
          <div className="mr-auto bg-surface-800 border border-rule rounded-lg px-3 py-2 text-sm">
            <div className="flex items-center gap-2 text-dim">
              <div className="flex gap-1">
                <span className="w-1.5 h-1.5 bg-muted rounded-full animate-bounce" style={{ animationDelay: '0ms' }} />
                <span className="w-1.5 h-1.5 bg-muted rounded-full animate-bounce" style={{ animationDelay: '150ms' }} />
                <span className="w-1.5 h-1.5 bg-muted rounded-full animate-bounce" style={{ animationDelay: '300ms' }} />
              </div>
              Thinking...
            </div>
          </div>
        )}
      </div>

      {/* Input */}
      <div className="border-t border-rule bg-surface-850 p-3">
        <div className="flex gap-2">
          <textarea
            ref={inputRef}
            value={input}
            onChange={(e) => setInput(e.target.value)}
            onKeyDown={handleKeyDown}
            placeholder="Ask about code, request changes..."
            rows={2}
            className="flex-1 rounded-sm border border-rule bg-surface-950 px-3 py-2 text-sm text-fg placeholder-dim resize-none focus:outline-none focus:border-accent focus:ring-1 focus:ring-accent/30"
            disabled={sending}
          />
          <button
            type="button"
            onClick={handleSend}
            disabled={!input.trim() || sending}
            className="self-end rounded-sm bg-accent px-3 py-2 text-sm font-medium text-white hover:bg-accent/80 transition-colors disabled:opacity-50 disabled:cursor-not-allowed"
          >
            Send
          </button>
        </div>
        <p className="mt-1 text-[10px] text-dim">Ctrl+Enter to send</p>
      </div>
    </div>
  )
}
