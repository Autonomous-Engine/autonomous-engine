import type { CodexChangeNote, CodexEvent, CodexPin } from '@/types'

// ─── Codex pin ────────────────────────────────────────────────────────────────
//
// Codex coherence Part 1: the codex version and modules a work item was
// planned against, and what each later codex change did with it — a re-plan
// (with the modules that changed), an escalation once the automatic re-plan
// cap or the budget was reached, an operator's "continue as is", or a note on
// an item past planning that was left alone ("codex changed since planning").

const MAX_MODULES = 3

export function moduleList(modules: string[]): string {
  if (modules.length <= MAX_MODULES) return modules.join(', ')
  return `${modules.slice(0, MAX_MODULES).join(', ')} +${modules.length - MAX_MODULES}`
}

// CodexPinBadge is the work item card's codex line: "codex v12 · auth,
// billing", and an amber flag when the codex changed since planning while the
// item stayed on its plan. Nothing for an unpinned item.
export function CodexPinBadge({ pin, note }: { pin?: CodexPin | null; note?: CodexChangeNote | null }) {
  if (!pin && !note) return null
  return (
    <span className="inline-flex flex-wrap items-center gap-1 text-[10px]">
      {pin && (
        <span
          className="rounded-sm bg-surface-700/40 px-1.5 py-0.5 font-mono text-muted border border-rule/40"
          title={`Planned against codex v${pin.product_version} on ${pin.branch}${pin.modules.length ? ` (modules: ${pin.modules.join(', ')})` : ''}`}
        >
          <span>codex v{pin.product_version}</span>
          {pin.modules.length > 0 && (
            <>
              <span className="text-dim"> · </span>
              <span>{moduleList(pin.modules)}</span>
            </>
          )}
        </span>
      )}
      {note && (
        <span
          className="rounded-sm bg-amber-500/20 px-1.5 py-0.5 text-amber-300 border border-amber-500/30"
          title={note.reason}
        >
          codex changed since planning
        </span>
      )}
    </span>
  )
}

const KIND_CLASS: Record<string, string> = {
  replanned: 'bg-sky-500/20 text-sky-300 border border-sky-500/30',
  escalated: 'bg-red-500/20 text-red-300 border border-red-500/30',
  noted:     'bg-amber-500/20 text-amber-300 border border-amber-500/30',
  continued: 'bg-emerald-500/20 text-emerald-300 border border-emerald-500/30',
}

export function codexEventClass(kind: string): string {
  return KIND_CLASS[kind] ?? 'bg-surface-700/40 text-muted border border-sharp/40'
}

export function codexEventSummary(e: CodexEvent): string {
  switch (e.kind) {
    case 'replanned':
      return e.automatic ? 're-planned' : 're-planned (operator)'
    case 'escalated':
      return 're-plan escalated'
    case 'noted':
      return 'codex changed since planning'
    case 'continued':
      return 'continued as is'
    default:
      return e.kind
  }
}

// CodexEventLine is one codex event in the activity log.
export function CodexEventLine({ event }: { event: CodexEvent }) {
  return (
    <span className="inline-flex flex-wrap items-center gap-2">
      <span className={`rounded px-1.5 py-0.5 text-xs font-medium ${codexEventClass(event.kind)}`}>{codexEventSummary(event)}</span>
      <span className="font-mono text-muted">v{event.from_version} → v{event.to_version}</span>
      {event.modules.length > 0 && <span className="text-fg">modules: {event.modules.join(', ')}</span>}
      {event.sections.length > 0 && <span className="text-fg">sections: {event.sections.join(', ')}</span>}
      {event.reason && <span className="text-dim italic basis-full break-words">{event.reason}</span>}
    </span>
  )
}
