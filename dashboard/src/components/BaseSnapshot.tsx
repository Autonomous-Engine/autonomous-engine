import type { BaseShaEvent, BaseShaPin } from '@/types'

// ─── Base snapshot ────────────────────────────────────────────────────────────
//
// The repository snapshot a work item is built against (M2-02): one pinned
// base commit per repository, resolved at intake and copied to subtasks, and
// the audit trail of those pins (resolved, drift_detected, repinned). Drift is
// shown, never hidden.

export function sha12(sha: string): string {
  return sha ? sha.slice(0, 12) : ''
}

const KIND_CLASS: Record<string, string> = {
  resolved:       'bg-emerald-500/20 text-emerald-300 border border-emerald-500/30',
  drift_detected: 'bg-amber-500/20 text-amber-300 border border-amber-500/30',
  repinned:       'bg-sky-500/20 text-sky-300 border border-sky-500/30',
}

export function eventKindClass(kind: string): string {
  return KIND_CLASS[kind] ?? 'bg-surface-700/40 text-muted border border-sharp/40'
}

export default function BaseSnapshot({ pins, events }: { pins?: BaseShaPin[]; events?: BaseShaEvent[] }) {
  const pinList = pins ?? []
  const eventList = events ?? []
  if (pinList.length === 0 && eventList.length === 0) {
    return (
      <p className="text-xs text-dim italic">
        No pinned base commit — this item predates snapshot pinning and follows each repository's base branch.
      </p>
    )
  }
  return (
    <div className="space-y-3">
      {pinList.length > 0 && (
        <ul className="space-y-1">
          {pinList.map((p) => (
            <li key={p.repository_id} className="text-xs text-muted">
              {pinList.length > 1 && <span className="text-fg">{p.repository_name || p.repository_id}: </span>}
              <span>Base: {p.base_ref} @ </span>
              <code className="font-mono text-fg" title={p.base_sha}>{sha12(p.base_sha)}</code>
            </li>
          ))}
        </ul>
      )}
      {eventList.length > 0 && (
        <ol className="space-y-1.5" aria-label="Base SHA events">
          {eventList.map((e) => (
            <li key={e.id} className="text-xs text-muted flex flex-wrap items-baseline gap-x-2">
              <span className={`rounded-sm px-1.5 py-0.5 text-[10px] font-mono ${eventKindClass(e.kind)}`}>{e.kind}</span>
              <span className="text-dim">{new Date(e.created_at).toLocaleString()}</span>
              {e.repository_name && <span>{e.repository_name}</span>}
              {e.old_sha && (
                <span>
                  <code className="font-mono" title={e.old_sha}>{sha12(e.old_sha)}</code> →{' '}
                </span>
              )}
              {e.new_sha && <code className="font-mono text-fg" title={e.new_sha}>{sha12(e.new_sha)}</code>}
              {e.detail && <span className="basis-full text-dim break-words">{e.detail}</span>}
            </li>
          ))}
        </ol>
      )}
    </div>
  )
}
