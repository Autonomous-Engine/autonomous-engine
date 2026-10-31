import type { ContractCheckStatus, ContractEdge } from '@/types'

// ─── Cross-repository contracts ──────────────────────────────────────────────
//
// The contract edges between a work item's repository subtasks (M2-06): the
// consumer subtask waits for the producer subtask, and before the consumer's
// PR opens pr-coordinator checks every contract against both heads. Shown
// per edge: producer → consumer, each contract's type and name, and the last
// check's status (a mismatch names the producer change and the consumer's
// file:line; "unchecked" is never a pass).

const STATUS_CLASS: Record<ContractCheckStatus, string> = {
  ok:        'bg-emerald-500/20 text-emerald-300 border border-emerald-500/30',
  mismatch:  'bg-red-500/20 text-red-300 border border-red-500/30',
  unchecked: 'bg-amber-500/20 text-amber-300 border border-amber-500/30',
}

export function contractStatusClass(status: string): string {
  return STATUS_CLASS[status as ContractCheckStatus] ?? 'bg-surface-700/40 text-muted border border-sharp/40'
}

export default function ContractEdges({ edges }: { edges?: ContractEdge[] }) {
  const list = edges ?? []
  if (list.length === 0) return null
  return (
    <ul className="space-y-3" aria-label="Contract edges">
      {list.map((e) => {
        const results = e.last_check?.results ?? []
        return (
          <li key={`${e.consumer_task_id}-${e.producer_task_id}`} className="text-xs text-muted space-y-1">
            <div className="flex flex-wrap items-baseline gap-x-2">
              <span className="text-fg" title={e.producer_title}>{e.producer_repository || e.producer_task_id.slice(0, 8)}</span>
              <span aria-hidden="true">→</span>
              <span className="text-fg" title={e.consumer_title}>{e.consumer_repository || e.consumer_task_id.slice(0, 8)}</span>
              <span className={`rounded-sm px-1.5 py-0.5 text-[10px] font-mono ${contractStatusClass(e.last_check?.status ?? '')}`}>
                {e.last_check ? e.last_check.status : 'not checked yet'}
              </span>
              {e.last_check?.checked_at && (
                <span className="text-dim">{new Date(e.last_check.checked_at).toLocaleString()}</span>
              )}
            </div>
            <ul className="ml-3 space-y-1">
              {(e.contracts ?? []).map((c, i) => {
                const r = results.find((x) => x.contract?.type === c.type && x.contract?.name === c.name)
                return (
                  <li key={`${c.type}-${c.name}-${i}`} className="flex flex-wrap items-baseline gap-x-2">
                    <code className="font-mono text-[10px] text-dim">{c.type}</code>
                    <code className="font-mono text-fg">{c.name}</code>
                    <span className="text-dim">({c.source === 'criteria' && c.criterion_id ? `declared by ${c.criterion_id}` : c.source})</span>
                    {r && (
                      <span className={`rounded-sm px-1.5 py-0.5 text-[10px] font-mono ${contractStatusClass(r.status)}`}>{r.status}</span>
                    )}
                    {r?.mismatches?.map((m, j) => (
                      <span key={j} className="basis-full text-red-300 break-words">
                        {m.producer_change} — consumer: <code className="font-mono">{m.consumer_ref}</code>
                      </span>
                    ))}
                    {r && r.status !== 'mismatch' && (r.details?.length ?? 0) > 0 && (
                      <span className="basis-full text-dim break-words">{r.details!.join('; ')}</span>
                    )}
                  </li>
                )
              })}
            </ul>
          </li>
        )
      })}
    </ul>
  )
}
