// dashboard/src/components/NotificationBell.tsx
//
// Header bell with a badge showing pending user-interaction prompts.
// Clicking opens the PromptSheet (Task 27).

import { usePendingPromptsCount } from '@/hooks/usePrompts'

interface NotificationBellProps {
  onOpen: () => void
}

export function NotificationBell({ onOpen }: NotificationBellProps) {
  const { count } = usePendingPromptsCount()
  const label =
    count === 0
      ? 'Notifications — no pending prompts'
      : `Notifications — ${count} pending`

  return (
    <button
      type="button"
      onClick={onOpen}
      aria-label={label}
      title={label}
      className="relative flex h-8 w-8 items-center justify-center rounded text-muted hover:bg-surface-800 hover:text-fg transition"
    >
      {/* Bell glyph — inline SVG to avoid a new icon dep */}
      <svg
        xmlns="http://www.w3.org/2000/svg"
        width="16"
        height="16"
        viewBox="0 0 24 24"
        fill="none"
        stroke="currentColor"
        strokeWidth="2"
        strokeLinecap="round"
        strokeLinejoin="round"
        aria-hidden="true"
      >
        <path d="M6 8a6 6 0 0 1 12 0c0 7 3 9 3 9H3s3-2 3-9" />
        <path d="M10.3 21a1.94 1.94 0 0 0 3.4 0" />
      </svg>
      {count > 0 && (
        <span className="absolute -right-0.5 -top-0.5 flex h-[1.1rem] min-w-[1.1rem] items-center justify-center rounded-full bg-lime-400 px-1 text-[0.6rem] font-medium tabular-nums leading-none text-surface-900">
          {count > 99 ? '99+' : count}
        </span>
      )}
    </button>
  )
}
