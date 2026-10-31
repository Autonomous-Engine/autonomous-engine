import { useState } from 'react'
import { revokeCodexEntry } from '@/hooks/usePrompts'

type CodexTable =
  | 'codex_modules'
  | 'codex_patterns'
  | 'codex_anti_patterns'
  | 'codex_sections'

interface RevokeButtonProps {
  id: string
  table: CodexTable
  name: string
  onRevoked: () => void
}

export function RevokeButton({ id, table, name, onRevoked }: RevokeButtonProps) {
  const [open, setOpen] = useState(false)
  const [reason, setReason] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState<string | null>(null)

  async function handleConfirm() {
    setSubmitting(true)
    setError(null)
    try {
      await revokeCodexEntry(id, table, reason.trim() || undefined)
      setOpen(false)
      setReason('')
      onRevoked()
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <>
      <button
        type="button"
        onClick={(e) => {
          e.stopPropagation()
          setOpen(true)
        }}
        className="font-mono text-xs text-dim hover:text-red-400 transition"
        title="Revoke this codex entry"
      >
        revoke
      </button>

      {open && (
        <>
          <div
            className="fixed inset-0 z-50 bg-black/60"
            onClick={() => setOpen(false)}
            aria-hidden="true"
          />
          <div
            role="dialog"
            aria-label={`Revoke ${name}`}
            className="fixed left-1/2 top-1/2 z-50 w-full max-w-md -translate-x-1/2 -translate-y-1/2 rounded-sm border border-rule bg-surface-950 p-6 shadow-2xl"
            onClick={(e) => e.stopPropagation()}
          >
            <h3 className="font-display text-sm text-fg">
              Revoke codex entry
            </h3>
            <p className="mt-2 text-xs text-muted leading-relaxed">
              <span className="font-mono text-fg">{name}</span> will be
              marked as revoked. The codex version will bump, in-flight work
              items tagged against the current version will be flagged for
              re-planning, and future <code className="font-mono text-accent-dim">past_decisions_query</code>{' '}
              retrievals will exclude the paired qdrant point. This is
              recoverable — the row stays in the audit log.
            </p>

            <label className="mt-4 block">
              <span className="section-label">Reason (optional)</span>
              <textarea
                rows={2}
                value={reason}
                onChange={(e) => setReason(e.target.value)}
                placeholder="Why are you revoking this?"
                className="mt-1 w-full rounded-sm border border-rule bg-surface-900 p-2 font-mono text-sm text-fg placeholder-dim focus:border-accent focus:outline-none"
              />
            </label>

            {error && (
              <div className="mt-3 rounded-sm border border-red-500/30 bg-red-500/10 p-2 text-xs text-red-400">
                {error}
              </div>
            )}

            <div className="mt-5 flex justify-end gap-2">
              <button
                type="button"
                onClick={() => setOpen(false)}
                disabled={submitting}
                className="rounded-sm border border-sharp px-4 py-2 font-mono text-xs text-fg transition-colors hover:border-accent hover:text-accent"
              >
                Cancel
              </button>
              <button
                type="button"
                onClick={handleConfirm}
                disabled={submitting}
                className="rounded-sm bg-red-500 px-4 py-2 font-mono text-xs uppercase tracking-wide text-white disabled:opacity-40"
              >
                {submitting ? 'Revoking…' : 'Revoke'}
              </button>
            </div>
          </div>
        </>
      )}
    </>
  )
}
