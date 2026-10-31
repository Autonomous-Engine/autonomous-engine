import { describe, expect, it, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import { GateTable, OverrideTable, canApproveWithoutOverride, formatInterval, isClosed, verdictClass } from '../admin/Rollouts'
import { guarded, isRolloutRequired, withOverrideReason } from '@/hooks/rolloutGuard'
import type { Rollout, RolloutDecision } from '@/types/rollouts'

const decision = (verdict: RolloutDecision['verdict']): RolloutDecision => ({
  policy: 'rollout-thresholds-v1', verdict, summary: '', gates: [], evaluated_at: '2026-10-03T00:00:00Z',
  cost_note: 'never consulted', report: null,
})

const rollout = (over: Partial<Rollout> = {}): Rollout => ({
  id: 'r1', candidate_snapshot_id: 'c'.repeat(64), baseline_snapshot_id: 'b'.repeat(64), scope: 'global', product_id: null,
  status: 'evaluating', created_by: 'me', created_at: '2026-10-03T00:00:00Z', updated_at: '2026-10-03T00:00:00Z',
  decided_at: null, decided_by: null, decision_report: decision('APPROVED'), notes: '', ...over,
})

describe('rollout helpers', () => {
  it('approves without an override only an evaluating rollout with an APPROVED decision', () => {
    expect(canApproveWithoutOverride(rollout())).toBe(true)
    expect(canApproveWithoutOverride(rollout({ decision_report: decision('MORE_TRIALS') }))).toBe(false)
    // A gate without data is never an approval.
    expect(canApproveWithoutOverride(rollout({ decision_report: decision('INCOMPLETE') }))).toBe(false)
    expect(verdictClass('INCOMPLETE')).toContain('yellow')
    expect(canApproveWithoutOverride(rollout({ status: 'proposed', decision_report: null }))).toBe(false)
    expect(isClosed(rollout({ status: 'blocked' }))).toBe(true)
    expect(isClosed(rollout({ status: 'rolled_back' }))).toBe(true)
    expect(isClosed(rollout({ status: 'approved' }))).toBe(false)
  })

  it('formats intervals and verdict colours', () => {
    expect(formatInterval([-6.67, 13.3])).toBe('[-6.7, +13.3] pp')
    expect(formatInterval(null)).toBe('—')
    expect(verdictClass('BLOCKED')).toContain('red')
    expect(verdictClass('MORE_TRIALS')).toContain('yellow')
    expect(verdictClass('PASS')).toContain('green')
  })

  it('renders gates and the override audit', () => {
    render(<GateTable gates={[{ name: 'pass_rate_regression', verdict: 'MORE_TRIALS', reason: 'interval [+0.0, +33.3] pp contains 0' }]} />)
    expect(screen.getByText('pass_rate_regression')).toBeTruthy()
    expect(screen.getByText('MORE_TRIALS')).toBeTruthy()
    render(<OverrideTable rows={[{ id: 'o1', rollout_id: null, action: 'agents.set_default', scope: 'global',
      before_snapshot_id: 'a'.repeat(64), after_snapshot_id: 'b'.repeat(64), override_reason: 'hotfix', actor: 'admin',
      created_at: '2026-10-03T00:00:00Z' }]} />)
    expect(screen.getByText('hotfix')).toBeTruthy()
    expect(screen.getByText('agents.set_default')).toBeTruthy()
  })
})

describe('default-setting guard client', () => {
  const refusal = new Error('API POST /admin/agents/set-default?id=x → 409: {"code":"rollout_required"}')

  it('recognises the refusal and appends the reason', () => {
    expect(isRolloutRequired(refusal)).toBe(true)
    expect(isRolloutRequired(new Error('API POST /x → 500: boom'))).toBe(false)
    expect(withOverrideReason('/admin/agents/set-default?id=x', 'hot fix')).toBe('/admin/agents/set-default?id=x&override_reason=hot%20fix')
    expect(withOverrideReason('/admin/prompts/create', 'r')).toBe('/admin/prompts/create?override_reason=r')
  })

  it('retries once with the reason the admin gives', async () => {
    const call = vi.fn()
      .mockRejectedValueOnce(refusal)
      .mockResolvedValueOnce('ok')
    await expect(guarded('/admin/agents/set-default?id=x', call, () => 'incident 42')).resolves.toBe('ok')
    expect(call).toHaveBeenLastCalledWith('/admin/agents/set-default?id=x&override_reason=incident%2042')
  })

  it('leaves the refusal standing without a reason', async () => {
    const call = vi.fn().mockRejectedValue(refusal)
    await expect(guarded('/p', call, () => null)).rejects.toThrow('no override reason')
    expect(call).toHaveBeenCalledTimes(1)
  })

  it('passes other errors through untouched', async () => {
    const call = vi.fn().mockRejectedValue(new Error('API PUT /p → 500: boom'))
    const ask = vi.fn()
    await expect(guarded('/p', call, ask)).rejects.toThrow('500')
    expect(ask).not.toHaveBeenCalled()
  })
})
