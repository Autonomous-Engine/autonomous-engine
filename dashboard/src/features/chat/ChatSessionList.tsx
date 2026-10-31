import { useState } from 'react'
import { useChatSessions } from './useChatSessions'
import type { ChatSession } from './types'

interface Props {
  activeSessionId: string | null
  onSelectSession: (id: string) => void
  onNewChat: () => void
}

function formatDate(dateStr: string): string {
  const d = new Date(dateStr)
  const now = new Date()
  const diffMs = now.getTime() - d.getTime()
  const diffDays = Math.floor(diffMs / 86400000)
  if (diffDays === 0) return 'Today'
  if (diffDays === 1) return 'Yesterday'
  if (diffDays < 7) return `${diffDays} days ago`
  return d.toLocaleDateString()
}

function RelBadge({ rel }: { rel: string }) {
  const colors: Record<string, string> = {
    created: 'text-green-400',
    modified: 'text-amber-400',
    referenced: 'text-blue-400',
  }
  return (
    <span className={`text-xs ${colors[rel] || 'text-dim'}`}>
      {rel}
    </span>
  )
}

export default function ChatSessionList({ activeSessionId, onSelectSession, onNewChat }: Props) {
  const { sessions, loading, archiveSession, deleteSession } = useChatSessions()
  const [menuOpen, setMenuOpen] = useState<string | null>(null)
  const [confirmDelete, setConfirmDelete] = useState<string | null>(null)

  const groups: { label: string; items: ChatSession[] }[] = []
  let currentLabel = ''
  for (const s of sessions) {
    const label = formatDate(s.updated_at)
    if (label !== currentLabel) {
      groups.push({ label, items: [] })
      currentLabel = label
    }
    groups[groups.length - 1].items.push(s)
  }

  return (
    <div className="flex flex-col h-full">
      <div className="p-3 border-b border-rule">
        <button
          onClick={onNewChat}
          className="w-full rounded bg-accent px-3 py-2 font-mono text-xs text-surface-950 hover:bg-accent/80 transition-colors"
        >
          + New Chat
        </button>
      </div>

      <div className="flex-1 overflow-y-auto">
        {loading && <p className="p-3 text-xs text-dim">Loading...</p>}
        {groups.map(group => (
          <div key={group.label}>
            <p className="section-label px-3 pt-3 pb-1">{group.label}</p>
            {group.items.map(session => (
              <div
                key={session.id}
                className={[
                  'group relative border-b border-rule/30 transition-colors',
                  session.id === activeSessionId
                    ? 'bg-accent/10'
                    : 'hover:bg-surface-800',
                ].join(' ')}
              >
                <button
                  onClick={() => onSelectSession(session.id)}
                  className="w-full text-left px-3 py-2 pr-8"
                >
                  <p className="text-sm text-fg truncate">
                    {session.title || 'Untitled session'}
                  </p>
                  {session.last_message && (
                    <p className="text-xs text-dim truncate mt-0.5">
                      {session.last_message}
                    </p>
                  )}
                  {session.linked_work_items.length > 0 && (
                    <div className="flex flex-wrap gap-1 mt-1">
                      {session.linked_work_items.map(lwi => (
                        <span key={lwi.work_item_id} className="inline-flex items-center gap-0.5 text-xs">
                          <span className="text-dim">{lwi.title?.slice(0, 20) || lwi.work_item_id.slice(0, 8)}</span>
                          <RelBadge rel={lwi.relationship} />
                        </span>
                      ))}
                    </div>
                  )}
                </button>

                {/* Action menu trigger */}
                <button
                  onClick={(e) => {
                    e.stopPropagation()
                    setMenuOpen(menuOpen === session.id ? null : session.id)
                    setConfirmDelete(null)
                  }}
                  className="absolute right-1 top-2 rounded p-1 text-dim opacity-0 group-hover:opacity-100 hover:bg-surface-700 hover:text-fg transition-all"
                >
                  <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" fill="currentColor" className="h-3.5 w-3.5">
                    <circle cx="8" cy="3" r="1.5" />
                    <circle cx="8" cy="8" r="1.5" />
                    <circle cx="8" cy="13" r="1.5" />
                  </svg>
                </button>

                {/* Dropdown menu */}
                {menuOpen === session.id && (
                  <div className="absolute right-1 top-8 z-20 w-36 rounded border border-rule bg-surface-900 py-1 shadow-lg">
                    <button
                      onClick={(e) => {
                        e.stopPropagation()
                        archiveSession(session.id)
                        setMenuOpen(null)
                      }}
                      className="flex w-full items-center gap-2 px-3 py-1.5 text-left text-xs text-muted hover:bg-surface-800 hover:text-fg transition-colors"
                    >
                      <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" fill="currentColor" className="h-3 w-3">
                        <path d="M2 3h12a1 1 0 011 1v1H1V4a1 1 0 011-1zm0 3h12v7a1 1 0 01-1 1H3a1 1 0 01-1-1V6zm4 2a.5.5 0 000 1h4a.5.5 0 000-1H6z" />
                      </svg>
                      Archive
                    </button>
                    {confirmDelete === session.id ? (
                      <button
                        onClick={(e) => {
                          e.stopPropagation()
                          deleteSession(session.id)
                          setMenuOpen(null)
                          setConfirmDelete(null)
                        }}
                        className="flex w-full items-center gap-2 px-3 py-1.5 text-left text-xs text-red-400 hover:bg-red-500/10 transition-colors"
                      >
                        <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" fill="currentColor" className="h-3 w-3">
                          <path d="M5.5 5.5A.5.5 0 016 6v6a.5.5 0 01-1 0V6a.5.5 0 01.5-.5zm2.5 0a.5.5 0 01.5.5v6a.5.5 0 01-1 0V6a.5.5 0 01.5-.5zm3 .5a.5.5 0 00-1 0v6a.5.5 0 001 0V6z" />
                          <path fillRule="evenodd" d="M14.5 3a1 1 0 01-1 1H13v9a2 2 0 01-2 2H5a2 2 0 01-2-2V4h-.5a1 1 0 01-1-1V2a1 1 0 011-1H6a1 1 0 011-1h2a1 1 0 011 1h3.5a1 1 0 011 1v1z" />
                        </svg>
                        Confirm delete
                      </button>
                    ) : (
                      <button
                        onClick={(e) => {
                          e.stopPropagation()
                          setConfirmDelete(session.id)
                        }}
                        className="flex w-full items-center gap-2 px-3 py-1.5 text-left text-xs text-red-400/70 hover:bg-red-500/10 hover:text-red-400 transition-colors"
                      >
                        <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" fill="currentColor" className="h-3 w-3">
                          <path fillRule="evenodd" d="M14.5 3a1 1 0 01-1 1H13v9a2 2 0 01-2 2H5a2 2 0 01-2-2V4h-.5a1 1 0 01-1-1V2a1 1 0 011-1H6a1 1 0 011-1h2a1 1 0 011 1h3.5a1 1 0 011 1v1z" />
                        </svg>
                        Delete
                      </button>
                    )}
                  </div>
                )}
              </div>
            ))}
          </div>
        ))}
        {!loading && sessions.length === 0 && (
          <p className="p-3 text-xs text-dim text-center">No conversations yet.</p>
        )}
      </div>
    </div>
  )
}
