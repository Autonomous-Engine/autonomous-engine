import { Link } from 'react-router-dom'
import type { WorkItemOutcome } from '@/types/configVersions'

// M4-05: the configuration snapshot a work item runs under, with a link to
// its diff against what the current default would capture now, and the
// item's recorded production outcome.

export function shortSnapshotId(id: string): string {
  return id.slice(0, 12)
}

export function configDiffLink(id: string, against = 'current'): string {
  const params = new URLSearchParams({ a: id, b: against })
  return `/admin/config-versions?${params.toString()}`
}

export function formatUsd(v: number | null | undefined): string {
  if (v === null || v === undefined) return 'unknown'
  if (v === 0) return '$0.00'
  return v < 0.01 ? `$${v.toFixed(4)}` : `$${v.toFixed(2)}`
}

const DECISION_LABEL: Record<string, string> = {
  release: 'released',
  legacy_release: 'released (no evidence)',
  human_review: 'human review',
  block: 'blocked',
}

// outcomeFacts lists the outcome's recorded facts as short labels, in a
// fixed order; facts that are unknown (null) are omitted, not shown as "no".
export function outcomeFacts(o: WorkItemOutcome): string[] {
  const out: string[] = []
  if (o.release_decision) out.push(DECISION_LABEL[o.release_decision] ?? o.release_decision)
  if (o.human_review_required) out.push('human review required')
  if (o.qa_result) out.push(`QA ${o.qa_result}`)
  if (o.merge_sha) out.push(`merged ${o.merge_sha.slice(0, 7)}`)
  if (o.human_edits_after_review === true) out.push('human edits after review')
  if (o.review_defects) {
    const d = o.review_defects
    out.push(`defects ${d.critical ?? 0}/${d.major ?? 0}/${d.minor ?? 0}`)
  }
  if (o.reverted_at) out.push(o.revert_detection === 'heuristic' ? 'reverted (heuristic)' : 'reverted')
  if (o.false_success) out.push('false success')
  out.push(`cost ${formatUsd(o.cost_usd)}`)
  return out
}

export default function ConfigSnapshotBadge({ snapshotId, outcome }: { snapshotId?: string | null; outcome?: WorkItemOutcome | null }) {
  if (!snapshotId && !outcome) return null
  return (
    <span className="inline-flex flex-wrap items-center gap-1.5" data-testid="config-snapshot-badge">
      {snapshotId ? (
        <Link
          to={configDiffLink(snapshotId)}
          title={`Configuration snapshot ${snapshotId} — diff against the current default`}
          className="rounded bg-teal-700/30 px-1.5 py-0.5 font-mono text-xs text-teal-300 border border-teal-700/40 hover:bg-teal-700/50"
        >
          config {shortSnapshotId(snapshotId)} · diff
        </Link>
      ) : (
        <span className="rounded bg-surface-700/40 px-1.5 py-0.5 text-xs text-dim border border-rule/40">no config snapshot</span>
      )}
      {outcome && (
        <span className="rounded bg-surface-700/40 px-1.5 py-0.5 text-xs text-muted border border-rule/40" title="Production outcome (work_item_outcomes)">
          {outcomeFacts(outcome).join(' · ')}
        </span>
      )}
    </span>
  )
}
