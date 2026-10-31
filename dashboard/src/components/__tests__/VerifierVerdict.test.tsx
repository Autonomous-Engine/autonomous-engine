import { describe, expect, it } from 'vitest'
import { render, screen } from '@testing-library/react'
import VerifierVerdict, { isVerifierArtifact, verifierVerdictFrom } from '../VerifierVerdict'

const artifact = {
  kind: 'verifier',
  decision: 'request_changes',
  gate: { action: 'request_changes', reason: 'criteria not met: AC-2' },
  feedback: 'INDEPENDENT VERIFIER: REQUEST_CHANGES',
  branch: 'agent/x',
  head_sha: 'abc123def4567890',
  reviewed_sha: 'abc123def4567890',
  defects: { critical: 1, major: 0, minor: 0 },
  verifier_model_shared_with_author: true,
  verifier_version: '1',
  verdict: {
    schema_version: 1,
    per_criterion: [
      { id: 'AC-1', status: 'met', evidence: ['call_3', 'user.go:13'], note: '' },
      { id: 'AC-2', status: 'not_met', evidence: [], note: 'DisplayName(nil) panics' },
    ],
    defects: [
      {
        severity: 'critical',
        file: 'user.go',
        line: 14,
        category: 'nil_dereference',
        description: 'u.First on a nil *User',
        reproduction: 'DisplayName(nil)',
      },
    ],
    overall: 'request_changes',
    model_info: { provider: 'ollama', model_id: 'qwen3', source: 'review_agent_model', verifier_model_shared_with_author: true },
  },
}

describe('verifierVerdictFrom', () => {
  it('reads a verifier artifact from an object or a JSON string', () => {
    const d = verifierVerdictFrom(artifact)
    expect(d?.decision).toBe('request_changes')
    expect(d?.perCriterion).toHaveLength(2)
    expect(d?.defects[0].line).toBe(14)
    expect(verifierVerdictFrom(JSON.stringify(artifact))?.sharedWithAuthor).toBe(true)
  })

  it('is distinct from the second-pass review', () => {
    expect(isVerifierArtifact(artifact)).toBe(true)
    expect(isVerifierArtifact({ kind: 'second_pass', decision: 'approve', feedback: 'LGTM' })).toBe(false)
    expect(isVerifierArtifact({ decision: 'approve', feedback: 'legacy review' })).toBe(false)
    expect(verifierVerdictFrom({ kind: 'second_pass' })).toBeNull()
    expect(verifierVerdictFrom('not json')).toBeNull()
  })

  it('tolerates a verdict without criteria or defects', () => {
    const d = verifierVerdictFrom({ kind: 'verifier', decision: 'escalate', verdict: { error: 'not verifiable: no clone' } })
    expect(d?.perCriterion).toEqual([])
    expect(d?.defects).toEqual([])
    expect(d?.error).toContain('no clone')
  })
})

describe('VerifierVerdict', () => {
  it('renders the criteria table and the defects with file:line', () => {
    render(<VerifierVerdict data={verifierVerdictFrom(artifact)!} />)
    expect(screen.getByTestId('verifier-decision').textContent).toBe('Changes requested')
    expect(screen.getByText('AC-1')).toBeTruthy()
    expect(screen.getByText('not met')).toBeTruthy()
    expect(screen.getByText('DisplayName(nil) panics')).toBeTruthy()
    expect(screen.getByText('user.go:14')).toBeTruthy()
    expect(screen.getByText('Reproduce: DisplayName(nil)')).toBeTruthy()
    expect(screen.getByTestId('verifier-shared-model')).toBeTruthy()
  })

  it('labels a deferred approval as not final', () => {
    render(<VerifierVerdict data={verifierVerdictFrom({ ...artifact, decision: 'approve_pending_second_pass' })!} />)
    expect(screen.getByTestId('verifier-decision').textContent).toBe('Approved — second-pass review decides')
  })

  it('shows the gaps and the subtask the stage verified before the PR (R14)', () => {
    const gap = 'criterion AC-3 is non-code: satisfied by the PR text; verify manually'
    const d = verifierVerdictFrom({ ...artifact, task_id: 'task-1', verdict: { ...artifact.verdict, gaps: [gap, 7] } })!
    expect(d.taskId).toBe('task-1')
    expect(d.gaps).toEqual([gap])
    render(<VerifierVerdict data={d} />)
    expect(screen.getByTestId('verifier-gaps').textContent).toContain(gap)
  })

  it('has no gaps section without gaps', () => {
    render(<VerifierVerdict data={verifierVerdictFrom(artifact)!} />)
    expect(screen.queryByTestId('verifier-gaps')).toBeNull()
  })

  it('says when nothing could be verified', () => {
    const d = verifierVerdictFrom({ kind: 'verifier', decision: 'escalate', gate: { reason: 'not verifiable: no clone' }, verdict: {} })!
    render(<VerifierVerdict data={d} />)
    expect(screen.getByTestId('verifier-decision').textContent).toBe('Escalated to human review')
    expect(screen.getByText('No acceptance criteria were recorded for this work item.')).toBeTruthy()
    expect(screen.queryByTestId('verifier-shared-model')).toBeNull()
  })
})
