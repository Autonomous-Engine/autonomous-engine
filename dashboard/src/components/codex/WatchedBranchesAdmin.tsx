import { useCallback, useEffect, useState } from 'react'
import { useWatchedBranches } from '@/hooks/useCodexV2'
import type { WatchedBranch } from '@/types/codex-v2'

interface Props {
  repoId: string
}

export default function WatchedBranchesAdmin({ repoId }: Props) {
  const { fetchBranches, addBranch, disableBranch } = useWatchedBranches()
  const [branches, setBranches] = useState<WatchedBranch[]>([])
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const [newBranch, setNewBranch] = useState('')
  const [adding, setAdding] = useState(false)

  const load = useCallback(async () => {
    setLoading(true)
    setError(null)
    try {
      const data = await fetchBranches(repoId)
      setBranches(data)
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : 'Failed to load branches')
      setBranches([])
    } finally {
      setLoading(false)
    }
  }, [repoId, fetchBranches])

  useEffect(() => {
    load()
  }, [load])

  const handleAdd = useCallback(async () => {
    if (!newBranch.trim()) return
    setAdding(true)
    setError(null)
    try {
      await addBranch({
        repo_id: repoId,
        branch: newBranch.trim(),
        source: 'manual',
        added_by: 'dashboard',
      })
      setNewBranch('')
      await load()
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : 'Failed to add branch')
    } finally {
      setAdding(false)
    }
  }, [repoId, newBranch, addBranch, load])

  const handleDisable = useCallback(
    async (branch: string) => {
      setError(null)
      try {
        await disableBranch({ repo_id: repoId, branch })
        await load()
      } catch (err: unknown) {
        setError(err instanceof Error ? err.message : 'Failed to disable branch')
      }
    },
    [repoId, disableBranch, load],
  )

  return (
    <div className="rounded-sm border border-rule bg-surface-800 p-5">
      <h3 className="font-display mb-4 text-lg text-fg">Watched Branches</h3>

      <form
        onSubmit={(e) => { e.preventDefault(); handleAdd() }}
        className="mb-4 flex gap-2"
      >
        <input
          type="text"
          value={newBranch}
          onChange={(e) => setNewBranch(e.target.value)}
          placeholder="branch name"
          className="flex-1 rounded-sm border border-rule bg-surface-900 px-3 py-1.5 font-mono text-sm text-fg placeholder-dim"
        />
        <button
          type="submit"
          disabled={adding || !newBranch.trim()}
          className="rounded-sm bg-accent px-4 py-1.5 font-mono text-sm font-medium text-surface-900 transition-colors hover:bg-accent-hover disabled:opacity-40"
        >
          {adding ? 'Adding...' : 'Add'}
        </button>
      </form>

      {error && (
        <div className="mb-3 rounded-sm border border-red-500/30 bg-red-500/10 p-3 text-sm text-red-400">
          {error}
        </div>
      )}

      {loading ? (
        <p className="font-mono text-sm text-muted">Loading branches...</p>
      ) : branches.length === 0 ? (
        <p className="font-mono text-sm text-muted">No watched branches configured.</p>
      ) : (
        <div className="overflow-x-auto">
          <table className="w-full text-sm">
            <thead className="text-left font-mono text-xs uppercase tracking-wider text-muted">
              <tr>
                <th className="px-3 py-2">Branch</th>
                <th className="px-3 py-2">Source</th>
                <th className="px-3 py-2">Added By</th>
                <th className="px-3 py-2">Added At</th>
                <th className="px-3 py-2">Last Snapshot</th>
                <th className="px-3 py-2">Status</th>
                <th className="px-3 py-2"></th>
              </tr>
            </thead>
            <tbody className="divide-y divide-rule">
              {branches.map((wb) => (
                <tr
                  key={wb.branch}
                  className={wb.enabled ? '' : 'opacity-50'}
                >
                  <td className="px-3 py-2 font-mono text-xs text-fg">{wb.branch}</td>
                  <td className="px-3 py-2 font-mono text-muted">{wb.source}</td>
                  <td className="px-3 py-2 font-mono text-muted">{wb.added_by}</td>
                  <td className="px-3 py-2 font-mono text-dim">
                    {new Date(wb.added_at).toLocaleString()}
                  </td>
                  <td className="px-3 py-2 font-mono text-xs text-dim">
                    {wb.last_snapshot_commit ? (
                      <span title={wb.last_snapshot_commit}>
                        {wb.last_snapshot_commit.slice(0, 8)}
                        {wb.last_snapshot_time && (
                          <span className="ml-1 text-muted">
                            {new Date(wb.last_snapshot_time).toLocaleDateString()}
                          </span>
                        )}
                      </span>
                    ) : (
                      <span className="text-dim">none</span>
                    )}
                  </td>
                  <td className="px-3 py-2">
                    {wb.enabled ? (
                      <span className="rounded-sm border border-green-500/20 bg-green-500/10 px-1.5 py-0.5 font-mono text-xs text-green-400">
                        Enabled
                      </span>
                    ) : (
                      <span className="rounded-sm border border-rule bg-surface-700/40 px-1.5 py-0.5 font-mono text-xs text-dim">
                        Disabled
                      </span>
                    )}
                  </td>
                  <td className="px-3 py-2">
                    {wb.enabled && (
                      <button
                        type="button"
                        onClick={() => handleDisable(wb.branch)}
                        className="rounded-sm border border-red-500/20 bg-red-500/10 px-2 py-0.5 font-mono text-xs text-red-400 hover:bg-red-500/20"
                      >
                        Disable
                      </button>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  )
}
