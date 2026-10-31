// ─── Acceptance criteria ──────────────────────────────────────────────────────
//
// The technical brief's acceptance criteria and file evidence map (M2-04):
// what behaviour must hold when the work is done, how it is checked, and why
// each file is believed to exist. A path with no codex/lsp/repo evidence is
// a hypothesis and is shown as one — never hidden.

export interface EvidenceRef {
  kind: string
  ref?: string
  confidence: number
  basis?: string
  note?: string
}

export interface CriterionFile {
  path: string
  evidence: EvidenceRef
}

export interface Criterion {
  id: string
  text: string
  source: { kind: string; quote?: string; reason?: string }
  files?: CriterionFile[]
  risk: string
  required_checks?: string[]
}

export interface BriefCriteriaData {
  schema_version: number
  criteria: Criterion[]
  file_evidence?: Record<string, EvidenceRef>
}

// briefCriteriaFrom reads the criteria out of a technical_brief artifact's
// content (an object, or its JSON string). null when the brief has none.
export function briefCriteriaFrom(content: unknown): BriefCriteriaData | null {
  let c: unknown = content
  if (typeof c === 'string') {
    try {
      c = JSON.parse(c)
    } catch {
      return null
    }
  }
  if (!c || typeof c !== 'object') return null
  const crit = (c as Record<string, unknown>).criteria
  if (!crit || typeof crit !== 'object') return null
  const data = crit as BriefCriteriaData
  if (!Array.isArray(data.criteria)) return null
  return data
}

const RISK_CLASS: Record<string, string> = {
  high:   'bg-red-500/20 text-red-300 border border-red-500/30',
  medium: 'bg-amber-500/20 text-amber-300 border border-amber-500/30',
  low:    'bg-emerald-500/20 text-emerald-300 border border-emerald-500/30',
}

const KIND_CLASS: Record<string, string> = {
  codex:      'bg-sky-500/20 text-sky-300 border border-sky-500/30',
  lsp:        'bg-sky-500/20 text-sky-300 border border-sky-500/30',
  repo:       'bg-emerald-500/20 text-emerald-300 border border-emerald-500/30',
  new:        'bg-violet-500/20 text-violet-300 border border-violet-500/30',
  hypothesis: 'bg-amber-500/20 text-amber-300 border border-amber-500/30',
}

export function evidenceKindClass(kind: string): string {
  return KIND_CLASS[kind] ?? 'bg-surface-700/40 text-muted border border-sharp/40'
}

export default function BriefCriteria({ data }: { data: BriefCriteriaData | null }) {
  if (!data) {
    return (
      <p className="text-xs text-dim italic">
        No acceptance criteria — this brief predates criteria extraction.
      </p>
    )
  }
  const evidence = Object.entries(data.file_evidence ?? {}).sort(([a], [b]) => a.localeCompare(b))
  return (
    <div className="space-y-4">
      {data.criteria.length === 0 ? (
        <p className="text-xs text-dim italic">The brief lists no criteria.</p>
      ) : (
        <ul className="space-y-2" aria-label="Acceptance criteria">
          {data.criteria.map((c) => (
            <li key={c.id} className="text-xs text-muted">
              <div className="flex flex-wrap items-baseline gap-x-2 gap-y-1">
                <code className="font-mono text-fg">{c.id}</code>
                <span className={`rounded-sm px-1.5 py-0.5 text-[10px] font-mono ${RISK_CLASS[c.risk] ?? ''}`}>risk: {c.risk}</span>
                {(c.required_checks ?? []).map((k) => (
                  <span key={k} className="rounded-sm px-1.5 py-0.5 text-[10px] font-mono bg-surface-700/40 border border-sharp/40">{k}</span>
                ))}
                <span className="basis-full text-fg">{c.text}</span>
                <span className="basis-full text-dim">
                  {c.source.kind === 'user' ? <>user: “{c.source.quote}”</> : <>derived: {c.source.reason}</>}
                </span>
              </div>
            </li>
          ))}
        </ul>
      )}
      {evidence.length > 0 && (
        <div className="overflow-x-auto">
          <table className="w-full text-xs" aria-label="File evidence">
            <thead>
              <tr className="border-b border-rule text-dim text-left">
                <th className="pb-2 pr-4 font-medium">Path</th>
                <th className="pb-2 pr-4 font-medium">Evidence</th>
                <th className="pb-2 font-medium">Ref</th>
              </tr>
            </thead>
            <tbody>
              {evidence.map(([path, ev]) => (
                <tr key={path} className="border-b border-rule/30 last:border-0 align-top">
                  <td className="py-1.5 pr-4 font-mono text-fg break-all">{path}</td>
                  <td className="py-1.5 pr-4">
                    <span className={`rounded-sm px-1.5 py-0.5 text-[10px] font-mono ${evidenceKindClass(ev.kind)}`}>{ev.kind}</span>
                    {ev.basis && <span className="ml-1 text-dim">({ev.basis})</span>}
                  </td>
                  <td className="py-1.5 text-muted break-words">
                    {ev.ref && <code className="font-mono">{ev.ref}</code>}
                    {ev.note && <span className="block text-dim">{ev.note}</span>}
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
