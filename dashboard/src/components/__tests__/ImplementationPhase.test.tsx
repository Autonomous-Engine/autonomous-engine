import { describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import ImplementationRuns, { PhaseBadge, SubtaskPhase, evidenceText, phaseBadgeClass, runsByTask } from '../ImplementationPhase'
import type { ImplementationRun } from '@/types/controller'

const greenRun: ImplementationRun = {
  id: 'run-1',
  work_item_id: 'w1',
  task_id: 'task-aaaaaaaa-1',
  attempt: 2,
  state: 'green_observed',
  started_at: '2026-10-03T12:00:00Z',
  updated_at: '2026-10-03T12:05:00Z',
  last_event_seq: 14,
  red: { seq: 6, tool_call_id: 'call_red_0001', tool: 'run_command', command: 'go test ./...', exit_code: 1, at: '2026-10-03T12:01:00Z' },
  green: { seq: 12, tool_call_id: 'call_green_0002', tool: 'run_command', command: 'go test ./...', exit_code: 0, at: '2026-10-03T12:04:00Z' },
}

const implementRun: ImplementationRun = {
  ...greenRun,
  id: 'run-2',
  task_id: 'task-bbbbbbbb-2',
  attempt: 1,
  state: 'implement',
  red: null,
  green: null,
}

describe('ImplementationPhase', () => {
  it('labels a badge with exactly the recorded state', () => {
    render(<PhaseBadge phase="red_observed" />)
    const badge = screen.getByTestId('phase-badge')
    expect(badge.textContent).toBe('red observed')
    expect(badge.className).toContain('red')
  })

  it('shows an unknown future state verbatim in the neutral style', () => {
    render(<PhaseBadge phase="some_new_state" />)
    expect(screen.getByTestId('phase-badge').textContent).toBe('some new state')
    expect(phaseBadgeClass('some_new_state')).toContain('text-muted')
  })

  it('shows no phase for a subtask without a recorded run', () => {
    render(<SubtaskPhase run={undefined} />)
    expect(screen.queryByTestId('phase-badge')).toBeNull()
    expect(screen.getByTitle('no implementation run recorded')).toBeTruthy()
  })

  it('subtask badge names the attempt and the red/green calls', () => {
    render(<SubtaskPhase run={greenRun} />)
    const badge = screen.getByTestId('phase-badge')
    expect(badge.textContent).toBe('green observed')
    expect(badge.getAttribute('title')).toContain('attempt 2')
    expect(badge.getAttribute('title')).toContain('call call_red_0001')
    expect(badge.getAttribute('title')).toContain('exit 0')
  })

  it('lists each run with its evidence, and "not observed" when the tools showed none', () => {
    render(<ImplementationRuns runs={[greenRun, implementRun]} />)
    const rows = screen.getAllByTestId('implementation-run')
    expect(rows).toHaveLength(2)
    expect(rows[0].textContent).toContain('go test ./... · exit 1 · call call_red_0001')
    expect(rows[0].textContent).toContain('go test ./... · exit 0 · call call_green_000…')
    expect(rows[1].textContent).toContain('implement')
    expect(rows[1].textContent).toContain('not observed')
    expect(rows[1].textContent).not.toContain('green observed')
  })

  it('renders the empty state', () => {
    render(<ImplementationRuns runs={[]} />)
    expect(screen.getByText('No implementation run recorded.')).toBeTruthy()
  })

  it('indexes runs by task id', () => {
    const idx = runsByTask([greenRun, implementRun])
    expect(idx['task-aaaaaaaa-1'].state).toBe('green_observed')
    expect(runsByTask(undefined)).toEqual({})
    expect(evidenceText(null)).toBe('not observed')
  })
})

// ConsoleView labels each event with the phase the agent recorded with it.
vi.mock('@/hooks/useWebSocket', () => ({
  useWebSocket: () => ({ lastMessage: null, subscribe: () => {}, unsubscribe: () => {} }),
}))
vi.mock('@/hooks/useApi', () => ({
  useApi: () => ({
    get: () =>
      Promise.resolve([
        { work_item_id: 'w1', task_id: 't1', agent_type: 'implementation-agent', tool: 'write_file', arguments: '{"path":"a_test.go"}', result: 'ok', is_error: false, timestamp: '2026-10-03T12:00:00Z', phase: 'test_authored' },
        { work_item_id: 'w1', task_id: 't1', agent_type: 'implementation-agent', tool: 'run_command', arguments: '{"command":"go test ./..."}', result: 'FAIL', is_error: false, timestamp: '2026-10-03T12:00:01Z', phase: 'red_observed', exit_code: 1 },
        { work_item_id: 'w1', task_id: 't1', agent_type: 'research-agent', tool: 'read_file', arguments: '{"path":"x"}', result: 'x', is_error: false, timestamp: '2026-10-03T12:00:02Z' },
      ]),
  }),
}))

describe('ConsoleView phases', () => {
  it('labels events with their recorded phase and exit code, and only those', async () => {
    const { default: ConsoleView } = await import('../ConsoleView')
    render(<ConsoleView workItemId="w1" />)
    await waitFor(() => expect(screen.getAllByTestId('phase-badge')).toHaveLength(2))
    const badges = screen.getAllByTestId('phase-badge').map((b) => b.textContent)
    expect(badges).toEqual(['test authored', 'red observed'])
    const exits = screen.getAllByTestId('exit-code')
    expect(exits).toHaveLength(1)
    expect(exits[0].textContent).toBe('exit 1')
  })
})
