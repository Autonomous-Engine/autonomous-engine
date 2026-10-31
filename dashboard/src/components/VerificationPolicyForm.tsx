import { useEffect, useState } from 'react'
import type {
  NoChecksPolicy,
  RepositoryVerificationPolicyInput,
  RepositoryVerificationPolicyView,
  RequiredCheckKind,
} from '@/types/verificationPolicy'
import { UNIMPLEMENTED_CHECKS } from '@/types/verificationPolicy'

// VerificationPolicyForm is the one form for a repository's verification
// policy (M3-03). The onboarding wizard and the admin repository editor both
// render it; only where it saves differs.
//
// The three questions are explicit and structured:
//   1. the test command (prefilled with the detected one, editable);
//   2. the required checks;
//   3. what happens when no executable check exists — "block" preselected

export interface VerificationPolicyFormProps {
  view: RepositoryVerificationPolicyView
  onSave: (input: RepositoryVerificationPolicyInput) => Promise<void>
  saving?: boolean
  error?: string | null
  // Prefix for element ids, so several forms can share a page.
  idPrefix?: string
}

// buildPolicyInput turns the form state into the request body. A test
// command equal to the detected one (or blank) is sent as null: "use
// detection", so a later change in the repository's runner is picked up.
export function buildPolicyInput(
  testCommand: string,
  detected: string,
  requiredChecks: RequiredCheckKind[],
  noChecksPolicy: NoChecksPolicy,
  securityPathsText: string,
): RepositoryVerificationPolicyInput {
  const cmd = testCommand.trim()
  return {
    test_command: cmd === '' || cmd === detected.trim() ? null : cmd,
    required_checks: requiredChecks,
    no_checks_policy: noChecksPolicy,
    security_review_paths: securityPathsText
      .split('\n')
      .map(s => s.trim())
      .filter((s, i, all) => s !== '' && all.indexOf(s) === i),
  }
}

export default function VerificationPolicyForm({ view, onSave, saving, error, idPrefix = 'vp' }: VerificationPolicyFormProps) {
  const policy = view.policy
  const [testCommand, setTestCommand] = useState(policy.test_command ?? view.detected_test_command ?? '')
  const [required, setRequired] = useState<RequiredCheckKind[]>(policy.required_checks ?? ['test'])
  const [noChecks, setNoChecks] = useState<NoChecksPolicy>(policy.no_checks_policy ?? 'block')
  const [paths, setPaths] = useState((policy.security_review_paths ?? []).join('\n'))

  useEffect(() => {
    setTestCommand(view.policy.test_command ?? view.detected_test_command ?? '')
    setRequired(view.policy.required_checks ?? ['test'])
    setNoChecks(view.policy.no_checks_policy ?? 'block')
    setPaths((view.policy.security_review_paths ?? []).join('\n'))
  }, [view])

  const selectable: RequiredCheckKind[] = view.selectable_checks?.length ? view.selectable_checks : ['test', 'build', 'lint']
  const hasCommand = testCommand.trim() !== ''
  const unimplementedRequired = required.filter(k => UNIMPLEMENTED_CHECKS.includes(k))

  function toggle(kind: RequiredCheckKind) {
    setRequired(cur => cur.includes(kind) ? cur.filter(k => k !== kind) : [...cur, kind])
  }

  async function submit(e: React.FormEvent) {
    e.preventDefault()
    await onSave(buildPolicyInput(testCommand, view.detected_test_command ?? '', required, noChecks, paths))
  }

  return (
    <form onSubmit={submit} className="space-y-4" aria-label="Repository verification policy">
      {!view.found ? (
        <p data-testid="vp-status" className="rounded-sm border border-amber-500/30 bg-amber-500/10 px-3 py-2 text-xs text-amber-200">
          No verification policy saved yet. Until you save one, the fail-closed default applies:
          runs with no executable checks are <strong>blocked</strong>.
        </p>
      ) : (
        <p data-testid="vp-status" className="text-xs text-dim">
          Saved via {view.policy.source}{view.policy.set_by ? ` by ${view.policy.set_by}` : ''}
          {view.policy.set_at ? ` on ${new Date(view.policy.set_at).toLocaleString()}` : ''}.
        </p>
      )}

      {/* 1. Test command */}
      <div>
        <label htmlFor={`${idPrefix}-test-command`} className="mb-1 block text-xs font-medium text-fg">
          Test command
        </label>
        <input
          id={`${idPrefix}-test-command`}
          type="text"
          value={testCommand}
          onChange={e => setTestCommand(e.target.value)}
          placeholder={view.detected_test_command || 'none detected'}
          className="w-full rounded-sm border border-rule bg-surface-950 px-3 py-2 font-mono text-sm text-fg placeholder-dim focus:outline-none focus:ring-1 focus:ring-accent"
        />
        <p className="mt-1 text-xs text-dim">
          {view.detected_test_command
            ? <>Detected: <span className="font-mono">{view.detected_test_command}</span>. Leave it to follow detection, or edit to override.</>
            : view.clone_available
              ? 'No test runner was detected in the clone. Enter a command to override detection.'
              : 'The clone is not available yet, so nothing could be detected. Enter a command to override detection.'}
        </p>
      </div>

      {/* 2. Required checks */}
      <fieldset>
        <legend className="mb-1 text-xs font-medium text-fg">Required checks</legend>
        <div className="flex flex-wrap gap-4">
          {selectable.map(kind => (
            <label key={kind} className="flex items-center gap-1.5 text-sm text-muted">
              <input
                type="checkbox"
                checked={required.includes(kind)}
                onChange={() => toggle(kind)}
                aria-label={`Require ${kind}`}
              />
              {kind}
              {UNIMPLEMENTED_CHECKS.includes(kind) && <span className="text-xs text-dim">(not run yet)</span>}
            </label>
          ))}
        </div>
        {unimplementedRequired.length > 0 && (
          <p data-testid="vp-unimplemented" className="mt-1 text-xs text-amber-300">
            The agent cannot run {unimplementedRequired.join(', ')} yet: requiring it records the check as not run,
            so this repository's work can be at best partially verified.
          </p>
        )}
      </fieldset>

      {/* 3. No executable checks */}
      <fieldset>
        <legend className="mb-1 text-xs font-medium text-fg">
          What should happen when no executable checks exist?
        </legend>
        <div className="space-y-1.5">
          <label className="flex items-start gap-2 text-sm text-muted">
            <input
              type="radio"
              name={`${idPrefix}-no-checks`}
              value="block"
              checked={noChecks === 'block'}
              onChange={() => setNoChecks('block')}
            />
            <span><span className="text-fg">Block</span> — fail verification and hold the work item (recommended).</span>
          </label>
          <label className="flex items-start gap-2 text-sm text-muted">
            <input
              type="radio"
              name={`${idPrefix}-no-checks`}
              value="partial_with_human_review"
              checked={noChecks === 'partial_with_human_review'}
              onChange={() => setNoChecks('partial_with_human_review')}
            />
            <span>
              <span className="text-fg">Partially verified, with mandatory human review</span> — never released
              automatically; also requires the product's partial-verification opt-in.
            </span>
          </label>
        </div>
        {!hasCommand && noChecks === 'block' && (
          <p data-testid="vp-will-block" className="mt-1 text-xs text-red-300">
            No test command is configured or detected: every implementation run in this repository will be blocked.
          </p>
        )}
      </fieldset>

      {/* Security review paths */}
      <div>
        <label htmlFor={`${idPrefix}-security-paths`} className="mb-1 block text-xs font-medium text-fg">
          Security-review paths (one glob per line)
        </label>
        <textarea
          id={`${idPrefix}-security-paths`}
          rows={5}
          value={paths}
          onChange={e => setPaths(e.target.value)}
          className="w-full rounded-sm border border-rule bg-surface-950 px-3 py-2 font-mono text-xs text-fg focus:outline-none focus:ring-1 focus:ring-accent"
        />
        <p className="mt-1 text-xs text-dim">
          Changes touching these paths require security review even when every check passes.
          {view.suggested_security_review_paths?.length > 0 && (
            <>
              {' '}
              <button
                type="button"
                onClick={() => setPaths(view.suggested_security_review_paths.join('\n'))}
                className="text-accent-dim hover:underline"
              >
                Reset to suggested{view.language ? ` (${view.language})` : ''}
              </button>
            </>
          )}
        </p>
      </div>

      {error && <p role="alert" className="text-xs text-red-400">{error}</p>}

      <button
        type="submit"
        disabled={saving}
        className="rounded bg-accent px-3 py-1.5 text-sm font-medium text-surface-900 hover:bg-accent-hover disabled:opacity-50"
      >
        {saving ? 'Saving…' : 'Save policy'}
      </button>
    </form>
  )
}
