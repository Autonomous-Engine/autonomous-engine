import { describe, expect, it } from 'vitest'
import { render, screen } from '@testing-library/react'
import RunRepair, { formatMinutes, remainingBudget } from '../RunRepair'
import ImplementationRuns from '../ImplementationPhase'
import type { ImplementationRun } from '@/types/controller'

const FP_A = 'ebcb42d96cd7aa11bb22cc33dd44ee55ff66778899aabbccddeeff0011223344'
const FP_B = '1a386f3aa8d2aa11bb22cc33dd44ee55ff66778899aabbccddeeff0011223344'

const stopped: ImplementationRun = {
  id: 'run-77777777-1',
  work_item_id: 'w1',
  task_id: 'task-cccccccc-3',
  attempt: 1,
  state: 'blocked',
  started_at: '2026-10-03T12:00:00Z',
  updated_at: '2026-10-03T12:40:00Z',
  last_event_seq: 30,
  red: null,
  green: null,
  repair: {
    max_repairs: 3,
    repairs: 1,
    runs: 2,
    stop_reason: 'repeated_failure',
    stop_detail: 'ebcb42d96cd7 failed again after repair attempt 1',
    failures: [
      { fingerprint: FP_A, category: 'test_assert', file: 'feature_test.go', line: 7, test: 'Test_alpha', message: 'got alpha, want done', ownership: 'changed_file', retryable: true, count: 2, first_run: 1, last_run: 2, last_call_id: 'precommit-cccccccc-2', raw_ref: 'work_item_artifact:abc' },
      { fingerprint: FP_B, category: 'tooling', message: 'sh: 1: frobtest: not found', ownership: 'environment', retryable: false, count: 1, first_run: 1, last_run: 1 },
    ],
    attempts: [{ attempt: 1, after_call_id: 'precommit-cccccccc-1', fingerprints: [FP_A, FP_B], outcome: 'repeated_failure' }],
    check_runs: [
      { run: 1, call_id: 'precommit-cccccccc-1', seq: 9, passed: false, fingerprints: [FP_A, FP_B] },
      { run: 2, call_id: 'precommit-cccccccc-2', seq: 20, passed: false, fingerprints: [FP_A], repeated: [FP_A] },
    ],
    revisions: [
      { type: 'plan_revision', seq: 10, evidence_call_id: 'precommit-cccccccc-1', evidence_seq: 9, fingerprint: FP_B, reason: 'the environment the plan assumes is not available', revision: 'revised plan: install frobtest first' },
    ],
  },
  budget: {
    token_limit: 500000,
    tokens_used: 125000,
    active_limit_seconds: 3600,
    active_seconds: 1500,
    paused_seconds: 10800,
    paused: false,
    started_at: '2026-10-03T12:00:00Z',
    updated_at: '2026-10-03T12:40:00Z',
  },
}

describe('RunRepair', () => {
  it('groups failure records by fingerprint with their run counts', () => {
    render(<RunRepair run={stopped} />)
    const rows = screen.getAllByTestId('failure-record')
    expect(rows).toHaveLength(2)
    expect(rows[0].textContent).toContain('ebcb42d96cd7')
    expect(rows[0].textContent).toContain('×2')
    expect(rows[0].textContent).toContain('feature_test.go:7')
    expect(rows[0].textContent).toContain('Test_alpha')
    expect(rows[1].textContent).toContain('environment (not repairable)')
    expect(screen.getByText('stopped: the same failure survived a repair')).toBeTruthy()
  })

  it('lists repair attempts with what the next check run showed', () => {
    render(<RunRepair run={stopped} />)
    expect(screen.getByTestId('repair-attempts').textContent).toContain('repair 1 after precommit-cccccccc-1 (2 records) → repeated_failure')
  })

  it('links each plan revision to the record and check run that contradicted the plan', () => {
    render(<RunRepair run={stopped} />)
    const rev = screen.getByTestId('plan-revisions')
    expect(rev.textContent).toContain('revised plan: install frobtest first')
    expect(rev.textContent).toContain('from check run precommit-cccccccc-1 (event 9)')
    const link = rev.querySelector('a')!
    expect(link.textContent).toBe('1a386f3aa8d2')
    const target = document.getElementById(link.getAttribute('href')!.slice(1))
    expect(target?.textContent).toContain('frobtest: not found')
  })

  it('shows the remaining token and active-time budget, excluding paused time', () => {
    render(<RunRepair run={stopped} />)
    const b = screen.getByTestId('run-budget').textContent!
    expect(b).toContain('375,000 left')
    expect(b).toContain('active 25m / 1h 0m')
    expect(b).toContain('35m left')
    expect(b).toContain('3h 0m awaiting input excluded')
    expect(remainingBudget({ ...stopped.budget!, tokens_used: 600000 }).tokens).toBe(0)
    expect(formatMinutes(0)).toBe('0m')
  })

  it('renders nothing for a run without repair or budget, and adds a detail row to the runs table', () => {
    const plain = { ...stopped, id: 'run-2', repair: undefined, budget: undefined }
    const { container } = render(<RunRepair run={plain} />)
    expect(container.textContent).toBe('')
    render(<ImplementationRuns runs={[stopped, plain]} />)
    expect(screen.getAllByTestId('implementation-run')).toHaveLength(2)
    expect(screen.getAllByTestId('implementation-run-repair')).toHaveLength(1)
  })

  it('marks an exhausted budget', () => {
    render(<RunRepair run={{ ...stopped, budget: { ...stopped.budget!, exhausted: 'active_time', exhausted_detail: '1h0m0s of 1h0m0s active' } }} />)
    expect(screen.getByText('exhausted: active time')).toBeTruthy()
  })
})
