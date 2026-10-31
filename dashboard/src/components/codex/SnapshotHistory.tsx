import { useCallback, useEffect, useState } from 'react'
import { useSnapshotHistory } from '@/hooks/useCodexV2'
import type { CodexSnapshot } from '@/types/codex-v2'
import SnapshotDiff from './SnapshotDiff'

interface Props {
  repoId: string
}

export default function SnapshotHistory({ repoId }: Props) {
  const { fetchSnapshots } = useSnapshotHistory()
  const [snapshots, setSnapshots] = useState<CodexSnapshot[]>([])
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [branchFilter, setBranchFilter] = useState('')

  const [fromSHA, setFromSHA] = useState<string | null>(null)
  const [toSHA, setToSHA] = useState<string | null>(null)
  const [showDiff, setShowDiff] = useState(false)

  const load = useCallback(async () => {
    setLoading(true)
    setError(null)
    try {
      const data = await fetchSnapshots(repoId, branchFilter || undefined)
      setSnapshots(data)
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : 'Failed to load snapshots')
      setSnapshots([])
    } finally {
      setLoading(false)
    }
  }, [repoId, branchFilter, fetchSnapshots])

  useEffect(() => {
    load()
  }, [load])

  const branches = Array.from(new Set(snapshots.map((s) => s.branch))).sort()

  const toggleSelection = useCallback((sha: string) => {
    setFromSHA((prev) => {
      if (prev === sha) return null
      if (prev === null) return sha
      setToSHA(sha)
      return prev
    })
  }, [])

  const handleCompare = useCallback(() => {
    if (fromSHA && toSHA) setShowDiff(true)
  }, [fromSHA, toSHA])

  const resetDiff = useCallback(() => {
    setShowDiff(false)
    setFromSHA(null)
    setToSHA(null)
  }, [])

  if (showDiff && fromSHA && toSHA) {
    return (
      <div>
        <button
          type="button"
          onClick={resetDiff}
          className="mb-3 rounded-sm border border-sharp bg-transparent px-3 py-1 font-mono text-sm text-muted transition-colors hover:border-accent hover:text-accent"
        >
          &larr; Back to history
        </button>
        <SnapshotDiff repoId={repoId} fromSHA={fromSHA} toSHA={toSHA} />
      </div>
    )
  }

  return (
    <div>
      <div className="mb-3 flex flex-wrap items-end gap-3">
        <label className="font-mono text-sm text-muted">
          Branch
          <select
            value={branchFilter}
            onChange={(e) => setBranchFilter(e.target.value)}
            className="mt-1 block rounded-sm border border-rule bg-surface-900 px-2 py-1 font-mono text-sm text-fg"
          >
            <option value="">All</option>
            {branches.map((b) => (
              <option key={b} value={b}>{b}</option>
            ))}
          </select>
        </label>
        <button
          type="button"
          disabled={!fromSHA || !toSHA}
          onClick={handleCompare}
          className="rounded-sm bg-accent px-4 py-1 font-mono text-sm font-medium text-surface-900 transition-colors hover:bg-accent-hover disabled:opacity-40"
        >
          Compare
        </button>
        {(fromSHA || toSHA) && (
          <span className="font-mono text-xs text-muted">
            {fromSHA && toSHA
              ? `${fromSHA.slice(0, 8)} vs ${toSHA.slice(0, 8)}`
              : 'Select two commits to compare'}
          </span>
        )}
      </div>

      {error && (
        <div className="mb-3 rounded-sm border border-red-500/30 bg-red-500/10 p-3 text-sm text-red-400">
          {error}
        </div>
      )}

      {loading ? (
        <p className="font-mono text-sm text-muted">Loading snapshots...</p>
      ) : snapshots.length === 0 ? (
        <p className="font-mono text-sm text-muted">No snapshots found.</p>
      ) : (
        <div className="overflow-x-auto rounded-sm border border-rule">
          <table className="w-full text-sm">
            <thead className="bg-surface-800 text-left font-mono text-xs uppercase tracking-wider text-muted">
              <tr>
                <th className="px-3 py-2 w-10">Sel</th>
                <th className="px-3 py-2">Commit SHA</th>
                <th className="px-3 py-2">Branch</th>
                <th className="px-3 py-2">Extracted At</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-rule">
              {snapshots.map((snap) => {
                const isSelected = snap.commit_sha === fromSHA || snap.commit_sha === toSHA
                return (
                  <tr key={snap.commit_sha} className={isSelected ? 'bg-surface-800' : ''}>
                    <td className="px-3 py-2">
                      <input
                        type="checkbox"
                        checked={isSelected}
                        onChange={() => toggleSelection(snap.commit_sha)}
                        className="accent-accent"
                      />
                    </td>
                    <td className="px-3 py-2 font-mono text-xs text-fg">
                      {snap.commit_sha.slice(0, 12)}
                    </td>
                    <td className="px-3 py-2 font-mono text-muted">{snap.branch}</td>
                    <td className="px-3 py-2 font-mono text-dim">
                      {new Date(snap.extracted_at).toLocaleString()}
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        </div>
      )}
    </div>
  )
}
