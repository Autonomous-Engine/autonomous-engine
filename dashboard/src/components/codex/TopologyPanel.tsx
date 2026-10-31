import { useCallback, useEffect, useMemo, useState } from 'react'
import { useApi } from '@/hooks/useApi'
import { useTopology } from '@/hooks/useCodexV2'
import type { TopologyEntry } from '@/types/codex-v2'
import type { Repository } from '@/types'

interface Props {
  productId: string
}

export default function TopologyPanel({ productId }: Props) {
  const { get } = useApi()
  const { fetchTopology } = useTopology()
  const [entries, setEntries] = useState<TopologyEntry[]>([])
  const [repos, setRepos] = useState<Repository[]>([])
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const repoNames = useMemo(() => {
    const map = new Map<string, string>()
    for (const r of repos) map.set(r.id, r.name)
    return map
  }, [repos])

  const load = useCallback(async () => {
    setLoading(true)
    setError(null)
    try {
      const [data, repoData] = await Promise.all([
        fetchTopology(productId),
        get<Repository[]>(`/admin/repositories?product_id=${productId}`),
      ])
      setEntries(data)
      setRepos(repoData)
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : 'Failed to load topology')
      setEntries([])
    } finally {
      setLoading(false)
    }
  }, [productId, fetchTopology, get])

  useEffect(() => {
    load()
  }, [load])

  const grouped = entries.reduce<Record<string, TopologyEntry[]>>((acc, e) => {
    const key = e.role || 'unknown'
    if (!acc[key]) acc[key] = []
    acc[key].push(e)
    return acc
  }, {})

  if (loading) return <p className="font-mono text-sm text-muted">Loading topology...</p>

  if (error) {
    return (
      <div className="rounded-sm border border-red-500/30 bg-red-500/10 p-3 text-sm text-red-400">
        {error}
      </div>
    )
  }

  if (entries.length === 0) {
    return <p className="font-mono text-sm text-muted">No topology data available.</p>
  }

  return (
    <div className="flex flex-wrap gap-3">
      {Object.entries(grouped).map(([role, items]) => (
        <div
          key={role}
          className="min-w-[200px] rounded-sm border border-rule bg-surface-800 p-4 transition-colors hover:border-sharp"
        >
          <h3 className="section-label mb-3">{role}</h3>
          <ul className="space-y-1">
            {items.map((item) => (
              <li key={item.repo_id} className="flex items-center justify-between gap-2 text-sm">
                <span className="truncate text-fg">{repoNames.get(item.repo_id) ?? item.repo_id}</span>
                <span className="shrink-0 rounded-sm border border-accent/20 bg-accent/10 px-1.5 py-0.5 font-mono text-xs text-accent-dim">
                  {Math.round(item.confidence * 100)}%
                </span>
              </li>
            ))}
          </ul>
        </div>
      ))}
    </div>
  )
}
