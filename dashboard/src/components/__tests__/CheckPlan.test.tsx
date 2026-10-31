import { describe, expect, it } from 'vitest'
import { render, screen } from '@testing-library/react'
import CheckPlans, { checkPlansByTask, isCheckPlanArtifact, targetText } from '../CheckPlan'

const plan = {
  language: 'go',
  changed_files: ['core/core.go'],
  targets: [
    { kind: 'test', command: 'go test -count=1 ./core', scope: ['./core'], reason: 'owner of changed file core/core.go' },
    { kind: 'test', command: 'go test -count=1 ./svc', scope: ['./svc'], reason: 'reverse dependent of ./core' },
    { kind: 'api_compat', scope: ['web:src/a.ts'], reason: 'contract edge: consumer web src/a.ts', not_run_reason: 'no api_compat checker available yet (M2-06)' },
  ],
  full: [{ kind: 'test', command: 'go test ./... -count=1', reason: 'final gate: the repository\'s full test suite' }],
}

describe('CheckPlan', () => {
  it('keeps the latest plan per task and ignores other artifacts', () => {
    const older = { ...plan, targets: [plan.targets[0]] }
    const plans = checkPlansByTask([
      { artifact_type: 'research_finding', created_at: '2026-10-03T10:00:00Z', content: { type: 'check_plan', task_id: 't1', computed_at: '2026-10-03T10:00:00Z', plan: older } },
      { artifact_type: 'research_finding', created_at: '2026-10-03T11:00:00Z', content: JSON.stringify({ type: 'check_plan', task_id: 't1', computed_at: '2026-10-03T11:00:00Z', plan }) },
      { artifact_type: 'research_finding', created_at: '2026-10-03T11:00:00Z', content: { type: 'implementation', task_id: 't1' } },
      { artifact_type: 'prd', created_at: '2026-10-03T11:00:00Z', content: 'not json' },
    ])
    expect(Object.keys(plans)).toEqual(['t1'])
    expect(plans.t1.plan.targets).toHaveLength(3)
  })

  it('shows each fast check with its reason, not-run targets with why, and the gate suite', () => {
    const plans = checkPlansByTask([
      { artifact_type: 'research_finding', created_at: '2026-10-03T11:00:00Z', content: { type: 'check_plan', task_id: 'task-1', computed_at: '2026-10-03T11:00:00Z', plan } },
    ])
    render(<CheckPlans plans={plans} titles={{ 'task-1': 'Add widgets' }} />)
    expect(screen.getByTestId('check-plan').textContent).toContain('Add widgets')
    expect(screen.getByTestId('check-plan').textContent).toContain('gate runs go test ./... -count=1')
    const rows = screen.getAllByTestId('check-target')
    expect(rows).toHaveLength(3)
    expect(rows[1].textContent).toContain('reverse dependent of ./core')
    expect(rows[2].textContent).toContain('not run: no api_compat checker available yet (M2-06)')
  })

  it('marks advisory checks and identifies check_plan artifacts for the generic list filter', () => {
    const advisoryPlan = { ...plan, targets: [{ kind: 'lint', command: 'go vet ./core', reason: 'vet the changed package(s)', advisory: true }] }
    render(<CheckPlans plans={{ t: { task_id: 't', computed_at: '2026-10-03T11:00:00Z', plan: advisoryPlan } }} />)
    expect(screen.getByTestId('check-target').textContent).toContain('lint (advisory)')
    expect(isCheckPlanArtifact({ type: 'check_plan', task_id: 't' })).toBe(true)
    expect(isCheckPlanArtifact(JSON.stringify({ type: 'check_plan' }))).toBe(true)
    expect(isCheckPlanArtifact({ type: 'implementation' })).toBe(false)
    expect(isCheckPlanArtifact('plain text')).toBe(false)
  })

  it('renders a placeholder without plans and text for a target without a command', () => {
    render(<CheckPlans plans={{}} />)
    expect(screen.getByText('No check plan recorded.')).toBeTruthy()
    expect(targetText({ kind: 'migration', reason: 'r' })).toBe('not run: no command')
    expect(targetText({ kind: 'migration', command: 'go test -v -run Migrat ./db', reason: 'r', skipped: 'no runnable migration tests (every migration test skipped)' }))
      .toBe('go test -v -run Migrat ./db — skipped: no runnable migration tests (every migration test skipped)')
  })
})
