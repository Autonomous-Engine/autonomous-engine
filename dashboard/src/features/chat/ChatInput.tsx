import { useState, useRef, useEffect } from 'react'

interface Props {
  onSend: (content: string) => void
  disabled?: boolean
  placeholder?: string
}

export default function ChatInput({ onSend, disabled, placeholder }: Props) {
  const [value, setValue] = useState('')
  const ref = useRef<HTMLTextAreaElement>(null)

  useEffect(() => {
    if (!disabled) ref.current?.focus()
  }, [disabled])

  function handleKeyDown(e: React.KeyboardEvent) {
    if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) {
      e.preventDefault()
      handleSend()
    }
  }

  function handleSend() {
    const trimmed = value.trim()
    if (!trimmed || disabled) return
    onSend(trimmed)
    setValue('')
  }

  return (
    <div className="border-t border-rule p-3">
      <div className="flex gap-2">
        <textarea
          ref={ref}
          value={value}
          onChange={e => setValue(e.target.value)}
          onKeyDown={handleKeyDown}
          disabled={disabled}
          placeholder={placeholder || 'Ask anything... (Ctrl+Enter to send)'}
          rows={2}
          className="flex-1 resize-none rounded border border-rule bg-surface-800 px-3 py-2 font-mono text-sm text-fg placeholder:text-dim focus:border-accent focus:outline-none disabled:opacity-50"
        />
        <button
          onClick={handleSend}
          disabled={disabled || !value.trim()}
          className="self-end rounded bg-accent px-3 py-2 font-mono text-xs text-surface-950 hover:bg-accent/80 transition-colors disabled:opacity-30"
        >
          Send
        </button>
      </div>
    </div>
  )
}
