import { useState, useCallback, useRef, useEffect } from 'react'
import { createPortal } from 'react-dom'
import { useNavigate, useLocation } from 'react-router-dom'
import { useChat } from './ChatProvider'
import ChatConversation from './ChatConversation'

const MIN_WIDTH = 320
const MAX_WIDTH = 900
const DEFAULT_WIDTH = 440
const STORAGE_KEY = 'chat-overlay-width'

function loadWidth(): number {
  try {
    const v = localStorage.getItem(STORAGE_KEY)
    if (v) {
      const n = parseInt(v, 10)
      if (n >= MIN_WIDTH && n <= MAX_WIDTH) return n
    }
  } catch { /* ignore */ }
  return DEFAULT_WIDTH
}

export default function ChatOverlay() {
  const { isOpen, activeSessionId, openChat, closeChat, startNewSession } = useChat()
  const navigate = useNavigate()
  const location = useLocation()
  const onChatPage = location.pathname.startsWith('/chat')
  const [width, setWidth] = useState(loadWidth)
  const dragging = useRef(false)
  const startX = useRef(0)
  const startWidth = useRef(DEFAULT_WIDTH)

  const onMouseDown = useCallback((e: React.MouseEvent) => {
    e.preventDefault()
    dragging.current = true
    startX.current = e.clientX
    startWidth.current = width
    document.body.style.cursor = 'col-resize'
    document.body.style.userSelect = 'none'
  }, [width])

  useEffect(() => {
    function onMouseMove(e: MouseEvent) {
      if (!dragging.current) return
      const delta = startX.current - e.clientX
      const next = Math.min(MAX_WIDTH, Math.max(MIN_WIDTH, startWidth.current + delta))
      setWidth(next)
    }
    function onMouseUp() {
      if (!dragging.current) return
      dragging.current = false
      document.body.style.cursor = ''
      document.body.style.userSelect = ''
      try { localStorage.setItem(STORAGE_KEY, String(width)) } catch { /* ignore */ }
    }
    window.addEventListener('mousemove', onMouseMove)
    window.addEventListener('mouseup', onMouseUp)
    return () => {
      window.removeEventListener('mousemove', onMouseMove)
      window.removeEventListener('mouseup', onMouseUp)
    }
  }, [width])

  return createPortal(
    <>
      {!isOpen && !onChatPage && (
        <button
          onClick={() => openChat()}
          className="fixed bottom-6 right-6 z-50 flex h-12 w-12 items-center justify-center rounded-full bg-accent text-surface-950 shadow-lg hover:bg-accent/80 transition-colors"
          title="Open chat"
        >
          <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="currentColor" className="h-6 w-6">
            <path d="M4 4h16a2 2 0 012 2v10a2 2 0 01-2 2H7l-4 4V6a2 2 0 012-2z" />
          </svg>
        </button>
      )}

      {isOpen && !onChatPage && (
        <div
          className="fixed inset-y-0 right-0 z-50 flex flex-col border-l border-rule bg-surface-900 shadow-2xl"
          style={{ width }}
        >
          {/* Resize handle */}
          <div
            onMouseDown={onMouseDown}
            className="absolute inset-y-0 left-0 w-1.5 cursor-col-resize hover:bg-accent/30 active:bg-accent/50 transition-colors z-10"
          />

          <div className="flex items-center justify-between border-b border-rule px-4 py-3">
            <h2 className="font-mono text-sm text-fg">Chat</h2>
            <div className="flex items-center gap-2">
              <button
                onClick={startNewSession}
                className="rounded px-2 py-1 text-xs text-muted hover:bg-surface-800 hover:text-fg transition-colors"
                title="New chat"
              >
                + New
              </button>
              {activeSessionId && (
                <button
                  onClick={() => {
                    closeChat()
                    navigate(`/chat/sessions/${activeSessionId}`)
                  }}
                  className="rounded px-2 py-1 text-xs text-muted hover:bg-surface-800 hover:text-fg transition-colors"
                  title="Open full view"
                >
                  Expand
                </button>
              )}
              <button
                onClick={closeChat}
                className="rounded px-2 py-1 text-xs text-muted hover:bg-surface-800 hover:text-fg transition-colors"
              >
                &times;
              </button>
            </div>
          </div>

          <ChatConversation sessionId={activeSessionId} />
        </div>
      )}
    </>,
    document.body,
  )
}
