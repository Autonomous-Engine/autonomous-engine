import { useCallback, useEffect, useState } from 'react'
import { useContractEdges } from '@/hooks/useCodexV2'
import type { ContractEdge } from '@/types/codex-v2'

interface Props {
  entryId: string
  entryType: 'module' | 'pattern' | 'anti_pattern' | 'section'
  productId: string
}

export default function EdgeInspector({ entryId, entryType, productId }: Props) {
  const { fetchContractEdges } = useContractEdges()
  const [incoming, setIncoming] = useState<ContractEdge[]>([])
  const [outgoing, setOutgoing] = useState<ContractEdge[]>([])
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const load = useCallback(async () => {
    if (!productId) return
    setLoading(true)
    setError(null)
    try {
      const allEdges = await fetchContractEdges({ product_id: productId })
      setIncoming(allEdges.filter((e) => e.consumer_repo_id === entryId))
      setOutgoing(allEdges.filter((e) => e.producer_repo_id === entryId))
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : 'Failed to load edges')
      setIncoming([])
      setOutgoing([])
    } finally {
      setLoading(false)
    }
  }, [entryId, productId, fetchContractEdges])

  useEffect(() => {
    load()
  }, [load])

  if (loading) return <p className="font-mono text-sm text-muted">Loading edges...</p>

  if (error) {
    return (
      <div className="rounded-sm border border-red-500/30 bg-red-500/10 p-2 text-sm text-red-400">
        {error}
      </div>
    )
  }

  const hasEdges = incoming.length > 0 || outgoing.length > 0

  if (!hasEdges) {
    return (
      <p className="font-mono text-sm text-muted">
        No contract edges for this {entryType.replace(/_/g, ' ')}.
      </p>
    )
  }

  return (
    <div className="space-y-3">
      <h4 className="font-display text-sm text-fg">Contract Edges</h4>

      {incoming.length > 0 && (
        <div>
          <p className="section-label mb-2">Incoming (consumer)</p>
          <ul className="space-y-1">
            {incoming.map((edge) => (
              <li key={edge.hash} className="flex items-center gap-2 text-xs">
                <span className="text-accent">&larr;</span>
                <span className="rounded-sm border border-accent/20 bg-accent/10 px-1 py-0.5 font-mono text-accent-dim">
                  {edge.contract_type}
                </span>
                <span className="text-muted">{edge.contract_name}</span>
                <span className="truncate font-mono text-dim">
                  {edge.producer_file}:{edge.producer_line}
                </span>
              </li>
            ))}
          </ul>
        </div>
      )}

      {outgoing.length > 0 && (
        <div>
          <p className="section-label mb-2">Outgoing (producer)</p>
          <ul className="space-y-1">
            {outgoing.map((edge) => (
              <li key={edge.hash} className="flex items-center gap-2 text-xs">
                <span className="text-blue-400">&rarr;</span>
                <span className="rounded-sm border border-accent/20 bg-accent/10 px-1 py-0.5 font-mono text-accent-dim">
                  {edge.contract_type}
                </span>
                <span className="text-muted">{edge.contract_name}</span>
                <span className="truncate font-mono text-dim">
                  {edge.consumer_file}:{edge.consumer_line}
                </span>
              </li>
            ))}
          </ul>
        </div>
      )}
    </div>
  )
}
