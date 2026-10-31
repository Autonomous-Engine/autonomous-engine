import { useCallback, useEffect, useState } from 'react'
import { useApi } from '@/hooks/useApi'
import type { Product, Repository } from '@/types'
import WatchedBranchesAdmin from './WatchedBranchesAdmin'

export default function WatchedBranchesPage() {
  const { get } = useApi()
  const [products, setProducts] = useState<Product[]>([])
  const [repos, setRepos] = useState<Repository[]>([])
  const [selectedProductId, setSelectedProductId] = useState<string | null>(null)
  const [selectedRepoId, setSelectedRepoId] = useState<string | null>(null)
  const [loadingProducts, setLoadingProducts] = useState(true)
  const [loadingRepos, setLoadingRepos] = useState(false)

  useEffect(() => {
    get<Product[]>('/admin/products')
      .then((data) => {
        setProducts(data)
        if (data.length > 0) setSelectedProductId(data[0].id)
      })
      .catch(() => setProducts([]))
      .finally(() => setLoadingProducts(false))
  }, [get])

  const fetchRepos = useCallback(async () => {
    if (!selectedProductId) {
      setRepos([])
      setSelectedRepoId(null)
      return
    }
    setLoadingRepos(true)
    try {
      const data = await get<Repository[]>(`/admin/repositories?product_id=${selectedProductId}`)
      setRepos(data)
      setSelectedRepoId(data.length > 0 ? data[0].id : null)
    } catch {
      setRepos([])
      setSelectedRepoId(null)
    } finally {
      setLoadingRepos(false)
    }
  }, [selectedProductId, get])

  useEffect(() => {
    fetchRepos()
  }, [fetchRepos])

  if (loadingProducts) {
    return (
      <div className="flex h-64 items-center justify-center">
        <div className="h-8 w-8 animate-spin rounded-full border-2 border-accent border-t-transparent" />
      </div>
    )
  }

  return (
    <div className="space-y-6">
      <div>
        <h2 className="font-display text-lg text-fg">Watched Branches</h2>
        <p className="mt-0.5 font-mono text-sm text-muted">
          Configure which branches are tracked for automatic codex re-indexing.
        </p>
      </div>

      <div className="flex items-center gap-4">
        <div className="flex items-center gap-2">
          <label className="font-mono text-xs text-muted">Product</label>
          <select
            value={selectedProductId ?? ''}
            onChange={(e) => setSelectedProductId(e.target.value || null)}
            className="rounded-sm border border-rule bg-surface-950 px-3 py-1.5 font-mono text-sm text-fg focus:outline-none focus:border-accent"
          >
            <option value="">Select a product…</option>
            {products.map((p) => (
              <option key={p.id} value={p.id}>{p.name}</option>
            ))}
          </select>
        </div>

        <div className="flex items-center gap-2">
          <label className="font-mono text-xs text-muted">Repository</label>
          <select
            value={selectedRepoId ?? ''}
            onChange={(e) => setSelectedRepoId(e.target.value || null)}
            disabled={loadingRepos || repos.length === 0}
            className="rounded-sm border border-rule bg-surface-950 px-3 py-1.5 font-mono text-sm text-fg focus:outline-none focus:border-accent disabled:opacity-50"
          >
            <option value="">
              {loadingRepos ? 'Loading…' : repos.length === 0 ? 'No repos' : 'Select a repo…'}
            </option>
            {repos.map((r) => (
              <option key={r.id} value={r.id}>{r.name}</option>
            ))}
          </select>
        </div>
      </div>

      {selectedRepoId ? (
        <WatchedBranchesAdmin repoId={selectedRepoId} />
      ) : (
        <div className="flex h-48 items-center justify-center rounded-sm border border-rule bg-surface-800">
          <p className="font-mono text-sm text-muted">
            Select a product and repository to manage watched branches.
          </p>
        </div>
      )}
    </div>
  )
}
