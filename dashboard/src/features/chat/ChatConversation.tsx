import { useEffect, useRef } from 'react'
import { useApi } from '@/hooks/useApi'
import { useChatMessages } from './useChatMessages'
import { useChat } from './ChatProvider'
import ChatMessageComponent from './ChatMessage'
import ChatInput from './ChatInput'
import ProductSelector from './ProductSelector'
import ProviderSelector from './ProviderSelector'

interface Props {
  sessionId: string | null
}

export default function ChatConversation({ sessionId: propSessionId }: Props) {
  const { activeSessionId, pageContext, loadSessionProduct, loadSessionProvider } = useChat()
  const sessionId = propSessionId || activeSessionId
  const { messages, loading, sending, sendMessage, approveProposal, rejectProposal } = useChatMessages(sessionId)
  const api = useApi()
  const scrollRef = useRef<HTMLDivElement>(null)
  const loadedSessionRef = useRef<string | null>(null)

  useEffect(() => {
    if (!sessionId || sessionId === loadedSessionRef.current) return
    loadedSessionRef.current = sessionId
    api.get<{
      product_id?: string
      product_name?: string
      provider_id?: string
      provider_name?: string
      provider_type?: string
      provider_model?: string
    }>(`/chat/sessions/${sessionId}`)
      .then(res => {
        if (res.product_id && res.product_name) {
          loadSessionProduct({ id: res.product_id, name: res.product_name })
        } else {
          loadSessionProduct(null)
        }
        if (res.provider_id && res.provider_name) {
          loadSessionProvider({
            id: res.provider_id,
            name: res.provider_name,
            provider_type: res.provider_type || '',
            model: res.provider_model || '',
          })
        } else {
          loadSessionProvider(null)
        }
      })
      .catch(() => {
        loadSessionProduct(null)
        loadSessionProvider(null)
      })
  }, [sessionId, api, loadSessionProduct, loadSessionProvider])

  useEffect(() => {
    if (scrollRef.current) {
      scrollRef.current.scrollTop = scrollRef.current.scrollHeight
    }
  }, [messages])

  return (
    <div className="flex flex-1 flex-col overflow-hidden">
      <div className="flex items-center gap-2 border-b border-rule px-3 py-2 flex-wrap">
        <span className="text-xs text-dim">Product:</span>
        <ProductSelector sessionId={sessionId} />
        <span className="text-xs text-dim ml-2">Model:</span>
        <ProviderSelector sessionId={sessionId} />
      </div>

      <div ref={scrollRef} className="flex-1 overflow-y-auto p-3 space-y-3">
        {loading && (
          <div className="text-center text-xs text-dim py-4">Loading messages...</div>
        )}
        {messages.length === 0 && !loading && (
          <div className="flex flex-1 items-center justify-center text-sm text-dim py-12">
            {sessionId ? 'Select a product above so the AI can access its codebase and codex.' : 'Start a new conversation.'}
          </div>
        )}
        {messages.map(msg => (
          <ChatMessageComponent
            key={msg.id}
            message={msg}
            onApproveProposal={approveProposal}
            onRejectProposal={rejectProposal}
          />
        ))}
        {sending && (
          <div className="flex justify-start">
            <div className="rounded-lg bg-surface-800 px-3 py-2">
              <div className="flex gap-1">
                <span className="h-2 w-2 rounded-full bg-accent/60 animate-bounce" style={{ animationDelay: '0ms' }} />
                <span className="h-2 w-2 rounded-full bg-accent/60 animate-bounce" style={{ animationDelay: '150ms' }} />
                <span className="h-2 w-2 rounded-full bg-accent/60 animate-bounce" style={{ animationDelay: '300ms' }} />
              </div>
            </div>
          </div>
        )}
      </div>

      <ChatInput
        onSend={(content) => sendMessage(content, pageContext)}
        disabled={sending}
      />
    </div>
  )
}
