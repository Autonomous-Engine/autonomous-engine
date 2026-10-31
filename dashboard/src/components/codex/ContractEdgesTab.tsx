import { useCallback, useEffect, useMemo, useState } from 'react'
import { useApi } from '@/hooks/useApi'
import { useContractEdges } from '@/hooks/useCodexV2'
import type { ContractEdge } from '@/types/codex-v2'
import type { Repository } from '@/types'

interface Props {
  productId: string
}

const PAGE_SIZE = 25

const CONTRACT_TYPES = ['', 'grpc', 'rest', 'event', 'graphql', 'queue'] as const

export default function ContractEdgesTab({ productId }: Props) {
  const { get } = useApi()
  const { fetchContractEdges } = useContractEdges()
  const [edges, setEdges] = useState<ContractEdge[]>([])
  const [repos, setRepos] = useState<Repository[]>([])
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [offset, setOffset] = useState(0)

  const [contractType, setContractType] = useState('')
  const [producerRepo, setProducerRepo] = useState('')
  const [consumerRepo, setConsumerRepo] = useState('')

  const [selected, setSelected] = useState<ContractEdge | null>(null)

  const repoNames = useMemo(() => {
    const map = new Map<string, string>()
    for (const r of repos) map.set(r.id, r.name)
    return map
  }, [repos])

  useEffect(() => {
    get<Repository[]>(`/admin/repositories?product_id=${productId}`)
      .then(setRepos)
      .catch(() => setRepos([]))
  }, [productId, get])

  const load = useCallback(async () => {
    setLoading(true)
    setError(null)
    try {
      const data = await fetchContractEdges({
        product_id: productId,
        contract_type: contractType || undefined,
        producer_repo: producerRepo || undefined,
        consumer_repo: consumerRepo || undefined,
        limit: PAGE_SIZE,
        offset,
      })
      setEdges(data)
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : 'Failed to load contract edges')
      setEdges([])
    } finally {
      setLoading(false)
    }
  }, [productId, contractType, producerRepo, consumerRepo, offset, fetchContractEdges])

  useEffect(() => {
    load()
  }, [load])

  const resetFilters = useCallback(() => {
    setContractType('')
    setProducerRepo('')
    setConsumerRepo('')
    setOffset(0)
  }, [])

  return (
    <div className="flex gap-4">
      <div className="min-w-0 flex-1">
        {/* Filter row */}
        <div className="mb-3 flex flex-wrap items-end gap-3">
          <label className="font-mono text-sm text-muted">
            Type
            <select
              value={contractType}
              onChange={(e) => { setContractType(e.target.value); setOffset(0) }}
              className="mt-1 block rounded-sm border border-rule bg-surface-900 px-2 py-1 font-mono text-sm text-fg"
            >
              <option value="">All</option>
              {CONTRACT_TYPES.filter(Boolean).map((t) => (
                <option key={t} value={t}>{t}</option>
              ))}
            </select>
          </label>
          <label className="font-mono text-sm text-muted">
            Producer
            <input
              type="text"
              value={producerRepo}
              onChange={(e) => { setProducerRepo(e.target.value); setOffset(0) }}
              placeholder="repo id"
              className="mt-1 block rounded-sm border border-rule bg-surface-900 px-2 py-1 font-mono text-sm text-fg placeholder-dim"
            />
          </label>
          <label className="font-mono text-sm text-muted">
            Consumer
            <input
              type="text"
              value={consumerRepo}
              onChange={(e) => { setConsumerRepo(e.target.value); setOffset(0) }}
              placeholder="repo id"
              className="mt-1 block rounded-sm border border-rule bg-surface-900 px-2 py-1 font-mono text-sm text-fg placeholder-dim"
            />
          </label>
          <button
            type="button"
            onClick={resetFilters}
            className="rounded-sm border border-sharp bg-transparent px-3 py-1 font-mono text-sm text-muted transition-colors hover:border-accent hover:text-accent"
          >
            Reset
          </button>
        </div>

        {error && (
          <div className="mb-3 rounded-sm border border-red-500/30 bg-red-500/10 p-3 text-sm text-red-400">
            {error}
          </div>
        )}

        {loading ? (
          <p className="font-mono text-sm text-muted">Loading edges...</p>
        ) : edges.length === 0 ? (
          <p className="font-mono text-sm text-muted">No contract edges found.</p>
        ) : (
          <div className="overflow-x-auto rounded-sm border border-rule">
            <table className="w-full text-sm">
              <thead className="bg-surface-800 text-left font-mono text-xs uppercase tracking-wider text-muted">
                <tr>
                  <th className="px-3 py-2">Type</th>
                  <th className="px-3 py-2">Name</th>
                  <th className="px-3 py-2">Producer</th>
                  <th className="px-3 py-2">Consumer</th>
                  <th className="px-3 py-2">Confidence</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-rule">
                {edges.map((edge) => (
                  <tr
                    key={edge.hash}
                    onClick={() => setSelected(edge)}
                    className={[
                      'cursor-pointer transition-colors hover:bg-surface-800',
                      selected?.hash === edge.hash ? 'bg-surface-800' : '',
                    ].join(' ')}
                  >
                    <td className="px-3 py-2">
                      <span className="rounded-sm border border-accent/20 bg-accent/10 px-1.5 py-0.5 font-mono text-xs text-accent-dim">
                        {edge.contract_type}
                      </span>
                    </td>
                    <td className="px-3 py-2 text-fg">{edge.contract_name}</td>
                    <td className="px-3 py-2 font-mono text-muted" title={edge.producer_repo_id}>
                      {repoNames.get(edge.producer_repo_id) ?? edge.producer_repo_id}
                    </td>
                    <td className="px-3 py-2 font-mono text-muted" title={edge.consumer_repo_id}>
                      {repoNames.get(edge.consumer_repo_id) ?? edge.consumer_repo_id}
                    </td>
                    <td className="px-3 py-2 font-mono text-dim">{Math.round(edge.confidence * 100)}%</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}

        {/* Pagination */}
        <div className="mt-3 flex items-center gap-3">
          <button
            type="button"
            disabled={offset === 0}
            onClick={() => setOffset((o) => Math.max(0, o - PAGE_SIZE))}
            className="rounded-sm border border-sharp bg-transparent px-3 py-1 font-mono text-sm text-muted transition-colors hover:border-accent hover:text-accent disabled:opacity-40"
          >
            Prev
          </button>
          <span className="font-mono text-xs text-dim">
            Showing {offset + 1}&#8211;{offset + edges.length}
          </span>
          <button
            type="button"
            disabled={edges.length < PAGE_SIZE}
            onClick={() => setOffset((o) => o + PAGE_SIZE)}
            className="rounded-sm border border-sharp bg-transparent px-3 py-1 font-mono text-sm text-muted transition-colors hover:border-accent hover:text-accent disabled:opacity-40"
          >
            Next
          </button>
        </div>
      </div>

      {/* Side pane */}
      {selected && (
        <div className="w-80 shrink-0 rounded-sm border border-rule bg-surface-800 p-4">
          <div className="mb-3 flex items-center justify-between">
            <h3 className="font-display text-sm text-fg">Edge Detail</h3>
            <button
              type="button"
              onClick={() => setSelected(null)}
              className="text-dim hover:text-fg transition-colors"
            >
              &times;
            </button>
          </div>
          <dl className="space-y-2 text-sm">
            <div>
              <dt className="font-mono text-xs uppercase tracking-wider text-muted">Contract</dt>
              <dd className="text-fg">{selected.contract_type} / {selected.contract_name}</dd>
            </div>
            <div>
              <dt className="font-mono text-xs uppercase tracking-wider text-muted">Producer file</dt>
              <dd className="break-all font-mono text-xs text-muted">
                {selected.producer_file}:{selected.producer_line}
              </dd>
            </div>
            <div>
              <dt className="font-mono text-xs uppercase tracking-wider text-muted">Consumer file</dt>
              <dd className="break-all font-mono text-xs text-muted">
                {selected.consumer_file}:{selected.consumer_line}
              </dd>
            </div>
            <div>
              <dt className="font-mono text-xs uppercase tracking-wider text-muted">Confidence</dt>
              <dd className="text-fg">{Math.round(selected.confidence * 100)}%</dd>
            </div>
            <div>
              <dt className="font-mono text-xs uppercase tracking-wider text-muted">Extracted</dt>
              <dd className="font-mono text-muted">{new Date(selected.extracted_at).toLocaleString()}</dd>
            </div>
          </dl>
        </div>
      )}
    </div>
  )
}
