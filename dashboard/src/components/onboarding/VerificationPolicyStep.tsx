import { useState } from 'react'
import type { Repository } from '@/types'
import RepositoryVerificationPolicyEditor from '@/components/RepositoryVerificationPolicyEditor'

// ─── Step 2b: Verification Policy ─────────────────────────────────────────────

interface VerificationPolicyStepProps {
  repos: Repository[]
  onNext: () => void
}

// VerificationPolicyStep asks, per repository, the structured verification
// questions (M3-03): test command (detected, editable), required checks, and
// what happens when no executable check exists — "block" preselected
// (block by default). Every repository must have a saved answer before
// the wizard continues; an unsaved repository is governed by the fail-closed
// default (block).
export default function VerificationPolicyStep({ repos, onNext }: VerificationPolicyStepProps) {
  const [saved, setSaved] = useState<Record<string, boolean>>({})
  const allSaved = repos.length > 0 && repos.every(r => saved[r.id])

  return (
    <div className="space-y-6">
      <div>
        <h2 className="text-base font-semibold text-fg">Verification Policy</h2>
        <p className="mt-1 text-sm text-muted">
          Tell the engine how each repository is verified. A repository with no executable checks is
          blocked unless you explicitly accept partial verification with mandatory human review.
        </p>
      </div>
      {repos.length === 0 && (
        <p className="text-sm text-dim">No repositories to configure. Go back and add one.</p>
      )}
      {repos.map(repo => (
        <div key={repo.id} className="rounded-sm border border-rule bg-surface-900 p-4">
          <div className="mb-3 flex items-center justify-between">
            <h3 className="text-sm font-semibold text-fg">{repo.name}</h3>
            {saved[repo.id]
              ? <span className="text-xs text-emerald-400">✓ policy saved</span>
              : <span className="text-xs text-amber-300">not saved</span>}
          </div>
          <RepositoryVerificationPolicyEditor
            repositoryId={repo.id}
            mode="onboarding"
            onLoaded={v => setSaved(s => ({ ...s, [repo.id]: s[repo.id] || v.found }))}
            onSaved={() => setSaved(s => ({ ...s, [repo.id]: true }))}
          />
        </div>
      ))}
      <div className="flex justify-end">
        <button
          onClick={onNext}
          disabled={!allSaved}
          className="rounded bg-accent px-4 py-2 text-sm font-medium text-surface-900 hover:bg-accent-hover disabled:opacity-50"
        >
          Continue
        </button>
      </div>
    </div>
  )
}
