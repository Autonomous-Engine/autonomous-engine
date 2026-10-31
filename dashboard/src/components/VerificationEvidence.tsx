import { useState } from 'react'
import type {
  ImplementationVerification,
  VerificationCheck,
  VerificationCheckStatus,
  VerificationOutcome,
} from '@/types/evidence'

// ─── Verification evidence ────────────────────────────────────────────────────
//
// Renders one implementation_verifications record: outcome, the exact SHAs the
// checks ran against, the git-derived changed files, and every check with its
// status, skip reason, exit code and output reference. Shown on the work item
// detail page (M0-08). The section never softens what the producer recorded.

// ─── Helpers ──────────────────────────────────────────────────────────────────

const OUTCOME_BADGE: Record<VerificationOutcome, string> = {
  verified:           'bg-emerald-500/20 text-emerald-300 border border-emerald-500/30',
  partially_verified: 'bg-amber-500/20 text-amber-300 border border-amber-500/30',
  unverified:         'bg-surface-700/40 text-muted border border-sharp/40',
  failed:             'bg-red-500/20 text-red-300 border border-red-500/30',
}

export function outcomeBadgeClass(outcome: string): string {
  // Unknown or future outcomes render in the neutral "unverified" style; the
  // raw label is still shown so nothing is hidden (D1: fail closed).
  return OUTCOME_BADGE[outcome as VerificationOutcome] ?? OUTCOME_BADGE.unverified
}

function outcomeLabel(outcome: string): string {
  return outcome.replace(/_/g, ' ')
}

function checkStatusClass(status: VerificationCheckStatus | string): string {
  switch (status) {
    case 'passed':  return 'text-emerald-300'
    case 'failed':  return 'text-red-300'
    case 'skipped': return 'text-amber-300'
    case 'not_run': return 'text-dim'
    default:        return 'text-muted'
  }
}

export function shortSha(sha: string): string {
  return sha.length > 12 ? sha.slice(0, 12) : sha
}

export function isHttpUrl(ref: string): boolean {
  return /^https?:\/\//i.test(ref)
}

function formatTs(ts?: string): string {
  if (!ts) return '—'
  const d = new Date(ts)
  return Number.isNaN(d.getTime()) ? ts : d.toLocaleString()
}

export function checkDuration(check: Pick<VerificationCheck, 'started_at' | 'finished_at'>): string {
  if (!check.started_at || !check.finished_at) return '—'
  const ms = new Date(check.finished_at).getTime() - new Date(check.started_at).getTime()
  if (Number.isNaN(ms) || ms < 0) return '—'
  if (ms < 1000) return `${ms}ms`
  if (ms < 60_000) return `${(ms / 1000).toFixed(1)}s`
  return `${Math.floor(ms / 60_000)}m ${Math.round((ms % 60_000) / 1000)}s`
}

// ─── Small pieces ─────────────────────────────────────────────────────────────

function CopyButton({ value, label }: { value: string; label: string }) {
  const [copied, setCopied] = useState(false)

  async function copy() {
    try {
      await navigator.clipboard.writeText(value)
      setCopied(true)
      setTimeout(() => setCopied(false), 1500)
    } catch {
      // Clipboard access can be denied (insecure context, permissions); the
      // full value is still available via the title tooltip.
    }
  }

  return (
    <button
      type="button"
      onClick={copy}
      aria-label={`Copy ${label}`}
      title={`Copy ${label}`}
      className="rounded-sm border border-rule px-1.5 py-0.5 text-[10px] text-dim hover:border-sharp hover:text-muted"
    >
      {copied ? 'Copied' : 'Copy'}
    </button>
  )
}

function ShaField({ label, sha }: { label: string; sha: string }) {
  return (
    <div>
      <dt className="text-xs text-dim">{label}</dt>
      <dd className="mt-0.5 flex items-center gap-2">
        {sha ? (
          <>
            <code className="font-mono text-xs text-fg" title={sha}>{shortSha(sha)}</code>
            <CopyButton value={sha} label={`${label} ${sha}`} />
          </>
        ) : (
          <span className="text-xs text-dim italic">not recorded</span>
        )}
      </dd>
    </div>
  )
}

function OutputRef({ value }: { value?: string }) {
  if (!value) return <span className="text-dim">—</span>
  if (isHttpUrl(value)) {
    return (
      <a
        href={value}
        target="_blank"
        rel="noreferrer"
        className="text-accent hover:underline break-all"
        title={value}
      >
        output
      </a>
    )
  }
  return <code className="font-mono text-[11px] text-muted break-all" title={value}>{value}</code>
}

// ─── Component ────────────────────────────────────────────────────────────────

interface Props {
  verification?: ImplementationVerification | null
}

export default function VerificationEvidence({ verification }: Props) {
  const [showFiles, setShowFiles] = useState(false)

  if (!verification) {
    return (
      <p className="text-xs text-dim italic">
        No verification evidence recorded for this work item yet.
      </p>
    )
  }

  const ev = verification.evidence
  const checks = ev.checks ?? []
  const changedFiles = ev.changed_files ?? []
  const artifactRefs = ev.artifact_refs ?? []
  const skippedCount = checks.filter((c) => c.status === 'skipped' || c.status === 'not_run').length

  return (
    <div className="space-y-4">
      {/* Outcome header */}
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
        <span
          data-testid="evidence-outcome"
          className={`rounded px-2 py-0.5 text-xs font-semibold ${outcomeBadgeClass(ev.outcome)}`}
        >
          {outcomeLabel(ev.outcome)}
        </span>
        <span className="text-xs text-dim">recorded {formatTs(verification.created_at)}</span>
        <span className="text-xs text-dim">schema v{ev.schema_version}</span>
        {ev.task_id && <span className="text-xs text-dim">task <span className="font-mono text-muted">{ev.task_id}</span></span>}
      </div>

      {ev.outcome === 'partially_verified' && (
        <p className="rounded-sm border border-amber-500/30 bg-amber-500/10 px-3 py-2 text-xs text-amber-200">
          Partially verified: {skippedCount} check{skippedCount === 1 ? ' was' : 's were'} skipped or not run.
          This work may advance only to mandatory human review; it is never treated as verified or released automatically.
        </p>
      )}
      {ev.outcome === 'failed' && (
        <p className="rounded-sm border border-red-500/30 bg-red-500/10 px-3 py-2 text-xs text-red-200">
          Verification failed: at least one check failed. The work is held back.
        </p>
      )}
      {ev.outcome === 'unverified' && (
        <p className="rounded-sm border border-rule bg-surface-900 px-3 py-2 text-xs text-muted">
          Unverified: no check ran to completion. The work is blocked until verification runs.
        </p>
      )}
      {(ev.flags ?? []).includes('security_review_required') && (
        <p
          data-testid="evidence-security-review"
          className="rounded-sm border border-amber-500/30 bg-amber-500/10 px-3 py-2 text-xs text-amber-200"
        >
          Security review required: the change touches paths in the repository's security-review list.
          Even verified work goes to human review instead of automatic release.
        </p>
      )}
      {(ev.flags ?? []).includes('zone_review_required') && (
        <p
          data-testid="evidence-zone-review"
          className="rounded-sm border border-amber-500/30 bg-amber-500/10 px-3 py-2 text-xs text-amber-200"
        >
          Zone review required: the change touches human_review codex zones
          ({(ev.zones ?? []).filter(z => z.policy === 'human_review').map(z => z.name).join(', ')}).
          Even verified work goes to human review instead of automatic release.
        </p>
      )}
      {(ev.zones ?? []).length > 0 && (
        <p data-testid="evidence-zones" className="text-xs text-muted">
          <span className="text-dim">Codex zones: </span>
          {(ev.zones ?? []).map(z => `${z.name} (${z.policy})`).join(', ')}
        </p>
      )}
      {ev.reason && (
        <p className="text-xs text-muted">
          <span className="text-dim">Reason: </span>{ev.reason}
        </p>
      )}

      {/* Commit identity */}
      <dl className="grid grid-cols-1 gap-x-6 gap-y-3 sm:grid-cols-2 lg:grid-cols-4">
        <ShaField label="Base SHA" sha={ev.base_sha} />
        <ShaField label="Head SHA" sha={ev.head_sha} />
        <div>
          <dt className="text-xs text-dim">Branch</dt>
          <dd className="mt-0.5 font-mono text-xs text-fg break-all">{ev.branch || <span className="text-dim italic">not recorded</span>}</dd>
        </div>
        <div>
          <dt className="text-xs text-dim">Changed files</dt>
          <dd className="mt-0.5 flex items-center gap-2 text-xs text-fg">
            <span>{changedFiles.length}</span>
            {changedFiles.length > 0 && (
              <button
                type="button"
                onClick={() => setShowFiles((v) => !v)}
                aria-expanded={showFiles}
                className="rounded-sm border border-rule px-1.5 py-0.5 text-[10px] text-dim hover:border-sharp hover:text-muted"
              >
                {showFiles ? 'Hide' : 'Show'}
              </button>
            )}
          </dd>
        </div>
      </dl>

      {showFiles && changedFiles.length > 0 && (
        <ul
          data-testid="evidence-changed-files"
          className="max-h-48 overflow-y-auto rounded-sm border border-rule bg-surface-900 px-3 py-2 font-mono text-[11px] text-muted"
        >
          {changedFiles.map((f) => (
            <li key={f} className="break-all">{f}</li>
          ))}
        </ul>
      )}

      <div className="flex flex-wrap gap-x-6 gap-y-1 text-xs text-dim">
        <span>started <span className="text-muted">{formatTs(ev.started_at)}</span></span>
        <span>finished <span className="text-muted">{formatTs(ev.finished_at)}</span></span>
        {artifactRefs.length > 0 && (
          <span>
            artifacts:{' '}
            {artifactRefs.map((ref, i) => (
              <span key={ref} className="font-mono text-muted">
                {i > 0 && ', '}
                {isHttpUrl(ref)
                  ? <a href={ref} target="_blank" rel="noreferrer" className="text-accent hover:underline">{ref}</a>
                  : ref}
              </span>
            ))}
          </span>
        )}
      </div>

      {/* Checks */}
      <div>
        <h4 className="mb-2 text-xs font-semibold text-muted">Checks ({checks.length})</h4>
        {checks.length === 0 ? (
          <p className="text-xs text-dim italic">No checks were recorded for this run.</p>
        ) : (
          <div className="overflow-x-auto rounded-sm border border-rule">
            <table className="w-full text-xs">
              <thead>
                <tr className="border-b border-rule bg-surface-950 text-left">
                  <th className="px-3 py-2 font-medium text-muted">Kind</th>
                  <th className="px-3 py-2 font-medium text-muted">Command</th>
                  <th className="px-3 py-2 font-medium text-muted">Status</th>
                  <th className="px-3 py-2 font-medium text-muted">Skip reason</th>
                  <th className="px-3 py-2 font-medium text-muted">Exit</th>
                  <th className="px-3 py-2 font-medium text-muted">Duration</th>
                  <th className="px-3 py-2 font-medium text-muted">Output</th>
                </tr>
              </thead>
              <tbody>
                {checks.map((c, i) => (
                  <tr key={`${c.kind}-${c.command}-${i}`} className="border-b border-rule last:border-b-0 align-top">
                    <td className="px-3 py-2 text-muted">{c.kind}</td>
                    <td className="px-3 py-2"><code className="font-mono text-[11px] text-fg break-all">{c.command || '—'}</code></td>
                    <td className={`px-3 py-2 font-semibold ${checkStatusClass(c.status)}`}>{String(c.status).replace(/_/g, ' ')}</td>
                    <td className="px-3 py-2 text-muted">{c.skip_reason || <span className="text-dim">—</span>}</td>
                    <td className="px-3 py-2 font-mono text-muted">{c.exit_code === null || c.exit_code === undefined ? <span className="text-dim">—</span> : c.exit_code}</td>
                    <td className="px-3 py-2 text-muted">{checkDuration(c)}</td>
                    <td className="px-3 py-2"><OutputRef value={c.output_ref} /></td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>
    </div>
  )
}
