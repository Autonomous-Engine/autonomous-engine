import type { ViewMode } from './CodexGraph'

interface GraphToolbarProps {
  viewMode: ViewMode
  onViewModeChange: (v: ViewMode) => void
  onFitView: () => void
  onRefresh: () => void
}

export function GraphToolbar({
  viewMode,
  onViewModeChange,
  onFitView,
  onRefresh,
}: GraphToolbarProps) {
  return (
    <div className="flex items-center gap-2" role="toolbar" aria-label="Graph toolbar">
      <div className="inline-flex rounded-sm border border-rule bg-surface-800 p-0.5 font-mono text-xs">
        {(['force', 'layered'] as ViewMode[]).map((m) => (
          <button
            key={m}
            type="button"
            onClick={() => onViewModeChange(m)}
            aria-pressed={viewMode === m}
            className={`rounded-sm px-2.5 py-1 font-medium transition-colors ${
              viewMode === m
                ? 'bg-accent/15 text-accent'
                : 'text-muted hover:text-fg'
            }`}
          >
            {m === 'force' ? 'Force' : 'Layered'}
          </button>
        ))}
      </div>

      <button
        type="button"
        onClick={onFitView}
        className="rounded-sm border border-rule bg-surface-800 px-2.5 py-1 font-mono text-xs text-muted transition-colors hover:border-accent hover:text-accent"
        title="Fit graph to screen (press 'f')"
      >
        Fit view
      </button>

      <button
        type="button"
        onClick={onRefresh}
        className="rounded-sm border border-rule bg-surface-800 px-2.5 py-1 font-mono text-xs text-muted transition-colors hover:border-accent hover:text-accent"
        title="Refetch graph data"
      >
        Refresh
      </button>
    </div>
  )
}
