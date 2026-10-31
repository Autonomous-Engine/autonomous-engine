import { useCallback, useEffect, useState } from 'react'
import { useGitStrategy } from '@/hooks/useCodexV2'
import type { GitStrategy } from '@/types/codex-v2'

interface Props {
  repoId: string
}

const PATTERN_COLORS: Record<string, string> = {
  trunk_based: 'border-green-500/20 bg-green-500/10 text-green-400',
  gitflow: 'border-purple-500/20 bg-purple-500/10 text-purple-400',
  github_flow: 'border-blue-500/20 bg-blue-500/10 text-blue-400',
}

export default function GitStrategyCard({ repoId }: Props) {
  const { fetchGitStrategy } = useGitStrategy()
  const [strategy, setStrategy] = useState<GitStrategy | null>(null)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const load = useCallback(async () => {
    setLoading(true)
    setError(null)
    try {
      const data = await fetchGitStrategy(repoId)
      setStrategy(data)
    } catch (err: unknown) {
      const msg = err instanceof Error ? err.message : ''
      if (msg.includes('404')) {
        setStrategy(null)
      } else {
        setError(msg || 'Failed to load git strategy')
      }
    } finally {
      setLoading(false)
    }
  }, [repoId, fetchGitStrategy])

  useEffect(() => {
    load()
  }, [load])

  if (loading) return <p className="font-mono text-sm text-muted">Loading git strategy...</p>

  if (error) {
    return (
      <div className="rounded-sm border border-red-500/30 bg-red-500/10 p-2 text-sm text-red-400">
        {error}
      </div>
    )
  }

  if (!strategy) return null

  const colorClass = PATTERN_COLORS[strategy.pattern] || 'border-rule bg-surface-700/40 text-muted'

  return (
    <div className="inline-flex items-center gap-3 rounded-sm border border-rule bg-surface-800 px-4 py-3">
      <span className={`rounded-sm border px-2 py-0.5 font-mono text-xs ${colorClass}`}>
        {strategy.pattern.replace(/_/g, ' ')}
      </span>

      <div className="flex items-center gap-3 font-mono text-sm">
        <span className="text-muted">
          main: <span className="text-fg">{strategy.main_branch}</span>
        </span>
        <span className="text-muted">
          merge: <span className="text-fg">{strategy.merge_strategy}</span>
        </span>
        <span className="text-muted">
          PR: {strategy.pr_required ? (
            <span className="text-accent">required</span>
          ) : (
            <span className="text-dim">no</span>
          )}
        </span>
      </div>

      {strategy.long_lived_branches.length > 0 && (
        <div className="flex items-center gap-1">
          {strategy.long_lived_branches.map((branch) => (
            <span
              key={branch}
              className="rounded-sm border border-rule bg-surface-900 px-2 py-0.5 font-mono text-xs text-muted"
            >
              {branch}
            </span>
          ))}
        </div>
      )}
    </div>
  )
}
