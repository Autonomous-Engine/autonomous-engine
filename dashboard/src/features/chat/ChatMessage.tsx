import ChatMarkdown from './ChatMarkdown'
import ProposalCard from './ProposalCard'
import type { ChatMessage as ChatMessageType } from './types'

interface Props {
  message: ChatMessageType
  onApproveProposal: (messageId: string) => void
  onRejectProposal: (messageId: string) => void
}

export default function ChatMessage({ message, onApproveProposal, onRejectProposal }: Props) {
  const isUser = message.role === 'user'

  return (
    <div className={['flex', isUser ? 'justify-end' : 'justify-start'].join(' ')}>
      <div className={[
        'max-w-[85%] rounded-lg px-3 py-2',
        isUser
          ? 'bg-accent/15 text-fg'
          : 'bg-surface-800 text-fg',
      ].join(' ')}>
        {message.tool_calls && message.tool_calls.length > 0 && (
          <div className="flex flex-wrap gap-1 mb-1.5">
            {message.tool_calls.map((tc, i) => (
              <span
                key={i}
                className="inline-block rounded bg-amber-500/15 px-1.5 py-0.5 font-mono text-xs text-amber-300"
                title={tc.args_preview}
              >
                {tc.tool}
              </span>
            ))}
          </div>
        )}
        <div className="prose prose-sm prose-invert max-w-none text-sm">
          {isUser ? (
            <p className="whitespace-pre-wrap m-0">{message.content}</p>
          ) : (
            <ChatMarkdown>{message.content}</ChatMarkdown>
          )}
        </div>
        {message.proposal && (
          <ProposalCard
            proposal={message.proposal}
            messageId={message.id}
            onApprove={onApproveProposal}
            onReject={onRejectProposal}
          />
        )}
        <p className="mt-1 text-right text-xs text-dim">
          {new Date(message.created_at).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })}
        </p>
      </div>
    </div>
  )
}
