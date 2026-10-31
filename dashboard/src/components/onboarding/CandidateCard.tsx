import { useState } from 'react'
import type { Candidate, UserStatus } from '@/types/onboarding'

interface Props {
  candidate: Candidate
  questionText: string
  onReview: (status: 'confirmed' | 'edited' | 'rejected', userValue?: string) => Promise<void>
}

function confidenceClasses(conf: Candidate['confidence']): string {
  switch (conf) {
    case 'high':
      return 'border-emerald-700 bg-emerald-900/20'
    case 'medium':
      return 'border-amber-700 bg-amber-900/20'
    case 'low':
      return 'border-rule bg-surface-900/60'
    case 'not_detected':
      return 'border-red-800 bg-red-900/20'
  }
}

function confidenceBadgeClasses(conf: Candidate['confidence']): string {
  switch (conf) {
    case 'high':
      return 'border-emerald-600 text-emerald-300'
    case 'medium':
      return 'border-amber-600 text-amber-300'
    case 'low':
      return 'border-sharp text-muted'
    case 'not_detected':
      return 'border-red-700 text-red-300'
  }
}

function confidenceLabel(conf: Candidate['confidence']): string {
  if (conf === 'not_detected') return 'Not detected'
  return conf.charAt(0).toUpperCase() + conf.slice(1)
}

function statusBadgeClasses(status: UserStatus): string {
  switch (status) {
    case 'pending':
      return 'border-rule bg-surface-900 text-muted'
    case 'confirmed':
      return 'border-emerald-600 bg-emerald-900/30 text-emerald-200'
    case 'edited':
      return 'border-indigo-600 bg-indigo-900/30 text-accent'
    case 'rejected':
      return 'border-sharp bg-surface-800/60 text-fg'
  }
}

export function CandidateCard({ candidate, questionText, onReview }: Props) {
  const [editing, setEditing] = useState(false)
  const [editValue, setEditValue] = useState(candidate.user_value ?? candidate.value)
  const [showEvidence, setShowEvidence] = useState(false)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<string | null>(null)

  async function act(status: 'confirmed' | 'edited' | 'rejected', uv?: string) {
    setSaving(true)
    setError(null)
    try {
      await onReview(status, uv)
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Failed to save decision')
    } finally {
      setSaving(false)
    }
  }

  const reviewed = candidate.user_status !== 'pending'
  const displayedValue = candidate.user_value ?? candidate.value
  const evidenceCount = candidate.evidence?.length ?? 0

  // Confidence=not_detected → extractor had nothing useful. Let the user
  // answer free-text OR skip. Previously this branch was read-only, which
  // left the Continue button permanently disabled.
  if (candidate.confidence === 'not_detected') {
    // Read-only view once the user has answered or skipped.
    if (reviewed) {
      return (
        <div className={`rounded-sm border ${confidenceClasses('not_detected')} p-4 space-y-2`}>
          <div className="flex items-start justify-between gap-3">
            <h3 className="text-sm font-semibold text-fg">{questionText}</h3>
            <span className={`shrink-0 rounded-sm border px-2 py-0.5 text-[10px] uppercase tracking-wide ${statusBadgeClasses(candidate.user_status)}`}>
              {candidate.user_status}
            </span>
          </div>
          {candidate.user_value
            ? <p className="whitespace-pre-wrap rounded bg-surface-950/60 px-3 py-2 text-sm text-fg">{candidate.user_value}</p>
            : <p className="text-xs italic text-dim">Skipped — no answer provided.</p>}
          {candidate.reviewed_at && (
            <p className="text-xs text-dim">reviewed {new Date(candidate.reviewed_at).toLocaleString()}</p>
          )}
        </div>
      )
    }

    // Interactive: textarea + Save + Skip. Use the SAME editValue state as
    // the main flow so the `Save edit` code path below is reused.
    return (
      <div className={`rounded-sm border ${confidenceClasses('not_detected')} p-4 space-y-3`}>
        <div className="flex items-start justify-between gap-3">
          <div>
            <p className="text-[10px] uppercase tracking-wide text-dim font-mono">{candidate.question_key}</p>
            <h3 className="text-sm font-semibold text-fg">{questionText}</h3>
          </div>
          <span className={`shrink-0 rounded-sm border px-2 py-0.5 text-[10px] uppercase tracking-wide ${confidenceBadgeClasses('not_detected')}`}>
            Not detected
          </span>
        </div>
        <p className="text-xs text-muted">
          I could not detect this automatically. Answer in your own words, or skip if you don't know.
        </p>
        <textarea
          aria-label={`Answer ${questionText}`}
          value={editValue}
          onChange={e => setEditValue(e.target.value)}
          rows={3}
          placeholder="Type your answer here…"
          className="w-full rounded-sm border border-rule bg-surface-950 px-3 py-2 text-sm text-fg placeholder-dim focus:outline-none focus:ring-1 focus:ring-accent"
        />
        {error && <p className="text-xs text-red-400">{error}</p>}
        <div className="flex gap-2">
          <button
            type="button"
            disabled={saving || !editValue.trim()}
            onClick={() => { void act('edited', editValue.trim()) }}
            aria-label="Save this answer"
            className="rounded bg-accent px-3 py-1.5 text-xs font-medium text-surface-900 hover:bg-accent-hover disabled:opacity-50"
          >
            {saving ? 'Saving…' : 'Save answer'}
          </button>
          <button
            type="button"
            disabled={saving}
            onClick={() => { void act('rejected') }}
            aria-label="Skip this question — I don't know"
            className="rounded-sm border border-rule px-3 py-1.5 text-xs text-fg hover:bg-surface-900 disabled:opacity-50"
          >
            Skip
          </button>
        </div>
      </div>
    )
  }

  return (
    <div className={`rounded-sm border ${confidenceClasses(candidate.confidence)} p-4 space-y-3`}>
      <div className="flex items-start justify-between gap-3">
        <div className="space-y-1">
          <p className="text-[10px] uppercase tracking-wide text-dim font-mono">{candidate.question_key}</p>
          <h3 className="text-sm font-semibold text-fg">{questionText}</h3>
        </div>
        <div className="flex shrink-0 flex-col items-end gap-1">
          <span className={`rounded-sm border px-2 py-0.5 text-[10px] uppercase tracking-wide ${confidenceBadgeClasses(candidate.confidence)}`}>
            {confidenceLabel(candidate.confidence)}
          </span>
          <span className={`rounded-sm border px-2 py-0.5 text-[10px] uppercase tracking-wide ${statusBadgeClasses(candidate.user_status)}`}>
            {candidate.user_status}
          </span>
        </div>
      </div>

      {!editing && (
        <div className="whitespace-pre-wrap rounded bg-surface-950/60 px-3 py-2 text-sm text-fg">
          {displayedValue}
        </div>
      )}
      {editing && (
        <textarea
          aria-label={`Edit answer for ${questionText}`}
          value={editValue}
          onChange={e => setEditValue(e.target.value)}
          rows={4}
          className="w-full rounded-sm border border-rule bg-surface-950 px-3 py-2 text-sm text-fg focus:outline-none focus:ring-1 focus:ring-accent"
        />
      )}

      <button
        type="button"
        aria-expanded={showEvidence}
        aria-label={showEvidence ? 'Hide evidence' : 'Show how we got this answer'}
        onClick={() => setShowEvidence(v => !v)}
        className="text-xs text-muted hover:text-fg"
      >
        {showEvidence ? 'Hide' : 'Show'} how we got this ({candidate.extractor}, {evidenceCount} citation{evidenceCount === 1 ? '' : 's'})
      </button>
      {showEvidence && (
        <ul className="space-y-1 rounded bg-surface-950/60 px-3 py-2 text-xs text-muted">
          {evidenceCount === 0 && <li className="italic text-dim">No citations recorded.</li>}
          {candidate.evidence?.map((e, i) => (
            <li key={i}>
              <span className="font-mono text-muted">{e.kind}</span>
              {e.path && <> — <span className="font-mono">{e.path}</span></>}
              {e.name && <> {e.path ? '·' : '—'} {e.name}</>}
              {e.lines && <> lines {e.lines}</>}
              {e.detail && <> — {e.detail}</>}
            </li>
          ))}
        </ul>
      )}

      {error && <p className="text-xs text-red-400">{error}</p>}

      {!reviewed && !editing && (
        <div className="flex flex-wrap gap-2">
          <button
            type="button"
            disabled={saving}
            onClick={() => act('confirmed')}
            aria-label="Confirm this answer is correct"
            className="rounded bg-emerald-700 px-3 py-1.5 text-xs font-medium text-white hover:bg-emerald-600 disabled:opacity-50"
          >
            Confirm
          </button>
          <button
            type="button"
            disabled={saving}
            onClick={() => { setEditValue(displayedValue); setEditing(true) }}
            aria-label="Edit this answer"
            className="rounded-sm border border-rule px-3 py-1.5 text-xs text-fg hover:bg-surface-900 disabled:opacity-50"
          >
            Edit...
          </button>
          <button
            type="button"
            disabled={saving}
            onClick={() => act('rejected')}
            aria-label="Reject this answer — not quite right"
            className="rounded-sm border border-red-800 px-3 py-1.5 text-xs text-red-300 hover:bg-red-900/20 disabled:opacity-50"
          >
            Not quite
          </button>
        </div>
      )}

      {editing && (
        <div className="flex gap-2">
          <button
            type="button"
            disabled={saving || !editValue.trim()}
            onClick={() => {
              void act('edited', editValue.trim()).then(() => setEditing(false))
            }}
            aria-label="Save edited answer"
            className="rounded bg-accent px-3 py-1.5 text-xs font-medium text-surface-900 hover:bg-accent-hover disabled:opacity-50"
          >
            Save edit
          </button>
          <button
            type="button"
            disabled={saving}
            onClick={() => { setEditing(false); setEditValue(candidate.user_value ?? candidate.value) }}
            aria-label="Cancel editing"
            className="rounded-sm border border-rule px-3 py-1.5 text-xs text-fg hover:bg-surface-900 disabled:opacity-50"
          >
            Cancel
          </button>
        </div>
      )}

      {reviewed && !editing && (
        <p className="text-xs text-muted">
          Status: <span className="font-medium text-fg">{candidate.user_status}</span>
          {candidate.reviewed_at && <> &middot; reviewed {new Date(candidate.reviewed_at).toLocaleString()}</>}
        </p>
      )}
    </div>
  )
}
