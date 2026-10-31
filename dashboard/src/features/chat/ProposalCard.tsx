import type { Proposal } from './types'

interface Props {
  proposal: Proposal
  messageId: string
  onApprove: (messageId: string) => void
  onReject: (messageId: string) => void
}

export default function ProposalCard({ proposal, messageId, onApprove, onReject }: Props) {
  const isPending = proposal.status === 'pending'

  return (
    <div className="mt-2 rounded border border-amber-500/30 bg-amber-500/10 p-3">
      <div className="flex items-center gap-2 mb-2">
        <span className="text-xs font-mono text-amber-400">PROPOSAL</span>
        <span className={[
          'rounded px-1.5 py-0.5 text-xs font-mono',
          proposal.status === 'pending' ? 'bg-amber-500/20 text-amber-300' :
          proposal.status === 'approved' ? 'bg-green-500/20 text-green-300' :
          'bg-red-500/20 text-red-300',
        ].join(' ')}>
          {proposal.status}
        </span>
      </div>
      <p className="text-sm text-fg mb-1">
        <span className="text-muted">Action:</span> {proposal.action.replace(/_/g, ' ')}
      </p>
      {proposal.changes.old && (
        <p className="text-xs text-dim">
          <span className="text-red-400 line-through">{proposal.changes.old}</span>
        </p>
      )}
      {proposal.changes.new && (
        <p className="text-xs text-dim">
          <span className="text-green-400">{proposal.changes.new}</span>
        </p>
      )}
      {proposal.changes.rationale && (
        <p className="text-xs text-dim mt-1">Reason: {proposal.changes.rationale}</p>
      )}
      {isPending && (
        <div className="flex gap-2 mt-3">
          <button
            onClick={() => onApprove(messageId)}
            className="rounded bg-green-600 px-3 py-1 text-xs font-mono text-white hover:bg-green-500 transition-colors"
          >
            Approve
          </button>
          <button
            onClick={() => onReject(messageId)}
            className="rounded bg-red-600/50 px-3 py-1 text-xs font-mono text-white hover:bg-red-500/50 transition-colors"
          >
            Reject
          </button>
        </div>
      )}
    </div>
  )
}
