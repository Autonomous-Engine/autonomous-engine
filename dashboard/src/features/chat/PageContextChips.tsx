import type { PageContext } from './types'

interface Props {
  context: PageContext
  onDismiss?: () => void
}

export default function PageContextChips({ context, onDismiss }: Props) {
  if (!context.page || context.page === 'unknown') return null

  const labels: string[] = []
  if (context.title) {
    labels.push(context.title)
  } else if (context.workItemId) {
    labels.push(`Work Item ${context.workItemId.slice(0, 8)}`)
  } else {
    labels.push(context.page.charAt(0).toUpperCase() + context.page.slice(1))
  }

  return (
    <div className="flex flex-wrap gap-1.5 px-3 py-2 border-b border-rule">
      <span className="text-xs text-dim">Context:</span>
      {labels.map((label, i) => (
        <span
          key={i}
          className="inline-flex items-center gap-1 rounded bg-surface-700/50 px-2 py-0.5 text-xs text-muted"
        >
          {label}
          {onDismiss && (
            <button onClick={onDismiss} className="ml-0.5 text-dim hover:text-fg">
              &times;
            </button>
          )}
        </span>
      ))}
    </div>
  )
}
