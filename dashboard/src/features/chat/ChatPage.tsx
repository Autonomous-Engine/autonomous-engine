import { useState, useEffect, useCallback, useRef } from 'react'
import { useParams } from 'react-router-dom'
import { useChat } from './ChatProvider'
import ChatSessionList from './ChatSessionList'
import ChatConversation from './ChatConversation'

const MIN_SIDEBAR = 200
const MAX_SIDEBAR = 480
const DEFAULT_SIDEBAR = 288
const STORAGE_KEY = 'chat-sidebar-width'

function loadWidth(): number {
  try {
    const v = localStorage.getItem(STORAGE_KEY)
    if (v) {
      const n = parseInt(v, 10)
      if (n >= MIN_SIDEBAR && n <= MAX_SIDEBAR) return n
    }
  } catch { /* ignore */ }
  return DEFAULT_SIDEBAR
}

export default function ChatPage() {
  const { sessionId: urlSessionId } = useParams<{ sessionId?: string }>()
  const { activeSessionId, setActiveSessionId, startNewSession } = useChat()
  const [selectedSessionId, setSelectedSessionId] = useState<string | null>(
    urlSessionId || activeSessionId
  )
  const [sidebarWidth, setSidebarWidth] = useState(loadWidth)
  const dragging = useRef(false)
  const startX = useRef(0)
  const startW = useRef(DEFAULT_SIDEBAR)

  useEffect(() => {
    if (urlSessionId) {
      setSelectedSessionId(urlSessionId)
    }
  }, [urlSessionId])

  // Sync selected session back to provider so overlay can resume it
  useEffect(() => {
    if (selectedSessionId) {
      setActiveSessionId(selectedSessionId)
    }
  }, [selectedSessionId, setActiveSessionId])

  // Pick up newly created sessions from lazy creation
  useEffect(() => {
    if (activeSessionId && !selectedSessionId) {
      setSelectedSessionId(activeSessionId)
    }
  }, [activeSessionId, selectedSessionId])

  const onMouseDown = useCallback((e: React.MouseEvent) => {
    e.preventDefault()
    dragging.current = true
    startX.current = e.clientX
    startW.current = sidebarWidth
    document.body.style.cursor = 'col-resize'
    document.body.style.userSelect = 'none'
  }, [sidebarWidth])

  useEffect(() => {
    function onMouseMove(e: MouseEvent) {
      if (!dragging.current) return
      const delta = e.clientX - startX.current
      const next = Math.min(MAX_SIDEBAR, Math.max(MIN_SIDEBAR, startW.current + delta))
      setSidebarWidth(next)
    }
    function onMouseUp() {
      if (!dragging.current) return
      dragging.current = false
      document.body.style.cursor = ''
      document.body.style.userSelect = ''
      try { localStorage.setItem(STORAGE_KEY, String(sidebarWidth)) } catch { /* ignore */ }
    }
    window.addEventListener('mousemove', onMouseMove)
    window.addEventListener('mouseup', onMouseUp)
    return () => {
      window.removeEventListener('mousemove', onMouseMove)
      window.removeEventListener('mouseup', onMouseUp)
    }
  }, [sidebarWidth])

  return (
    <div className="flex h-full -m-6 overflow-hidden">
      <div
        className="relative flex-shrink-0 border-r border-rule bg-surface-950"
        style={{ width: sidebarWidth }}
      >
        <ChatSessionList
          activeSessionId={selectedSessionId}
          onSelectSession={setSelectedSessionId}
          onNewChat={async () => {
            startNewSession()
            setSelectedSessionId(null)
          }}
        />
        {/* Resize handle */}
        <div
          onMouseDown={onMouseDown}
          className="absolute inset-y-0 right-0 w-1.5 cursor-col-resize hover:bg-accent/30 active:bg-accent/50 transition-colors z-10"
        />
      </div>

      <div className="flex flex-1 flex-col">
        <ChatConversation sessionId={selectedSessionId} />
      </div>
    </div>
  )
}
