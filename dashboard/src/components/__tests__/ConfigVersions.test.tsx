import { describe, expect, it } from 'vitest'
import { render, screen } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import ConfigSnapshotBadge, { configDiffLink, formatUsd, outcomeFacts, shortSnapshotId } from '../ConfigSnapshotBadge'
import { DiffView, SnapshotRollupTable, costCell, defectsCell, formatRate, itemCostCell, parsePrice } from '../admin/ConfigVersions'
import type { ConfigSnapshotRollup, WorkItemOutcome } from '@/types/configVersions'

const SNAP = 'a'.repeat(52) + '0123456789ab'

const rollup = (over: Partial<ConfigSnapshotRollup> = {}): ConfigSnapshotRollup => ({
  id: SNAP, agent_type: 'implementation-agent', provider: 'anthropic', model_id: 'claude-x',
  agent_definition_id: null, agent_definition_name: 'full-stack-engineer', created_at: '2026-10-03T00:00:00Z',
  items: 4, verified: 3, verified_rate: 0.75, defect_data_items: 0, defects_critical: 0, defects_major: 0, defects_minor: 0,
  human_edit_known: 2, human_edited: 1, human_edit_rate: 0.5, false_success: 0, reverted: 0,
  inference_calls: 10, tokens_in: 100, tokens_out: 50, cost_usd: null, cost_unknown: true, unpriced_calls: 10,
  is_current_default: true, referenced_by_items: 4,
  item_cost_usd: null, item_cost_unknown: true, item_cost_unknown_items: 1, ...over,
})

const outcome = (over: Partial<WorkItemOutcome> = {}): WorkItemOutcome => ({
  work_item_id: 'w', config_snapshot_id: SNAP, verified_head_sha: 'abc', reviewed_head_sha: 'abc', release_decision: 'release',
  review_defects: null, human_review_required: false, human_edits_after_review: null, qa_result: 'pass',
  pr_number: 7, merge_sha: '1234567890', merged_at: '2026-10-03T00:00:00Z', reverted_at: null, revert_detection: null,
  false_success: false, tokens_in: 1, tokens_out: 1, cost_usd: null, ...over,
})

describe('config snapshot helpers', () => {
  it('formats ids, links and money', () => {
    expect(shortSnapshotId(SNAP)).toBe('aaaaaaaaaaaa')
    expect(configDiffLink(SNAP)).toBe(`/admin/config-versions?a=${SNAP}&b=current`)
    expect(formatUsd(null)).toBe('unknown')
    expect(formatUsd(0.0042)).toBe('$0.0042')
    expect(formatUsd(12.345)).toBe('$12.35')
  })

  it('never shows unknown cost or missing defect data as zero', () => {
    expect(costCell(rollup())).toBe('unknown (10 unpriced)')
    expect(costCell(rollup({ cost_unknown: false, cost_usd: 1.5, unpriced_calls: 0 }))).toBe('$1.50')
    expect(itemCostCell(rollup())).toBe('unknown (1 items)')
    expect(itemCostCell(rollup({ item_cost_unknown: false, item_cost_usd: 2, item_cost_unknown_items: 0 }))).toBe('$2.00')
    expect(itemCostCell(rollup({ items: 0 }))).toBe('—')
    expect(defectsCell(rollup())).toBe('no data')
    expect(defectsCell(rollup({ defect_data_items: 2, defects_major: 1 }))).toBe('0/1/0')
    expect(formatRate(null)).toBe('—')
    expect(formatRate(0.75)).toBe('75%')
  })

  it('parses price fields', () => {
    expect(parsePrice('')).toEqual({ value: null })
    expect(parsePrice(' 3.5 ')).toEqual({ value: 3.5 })
    expect(parsePrice('-1').error).toBeTruthy()
    expect(parsePrice('abc').error).toBeTruthy()
  })

  it('lists only known outcome facts', () => {
    const facts = outcomeFacts(outcome({ human_edits_after_review: true, reverted_at: '2026-10-04T00:00:00Z', revert_detection: 'heuristic' }))
    expect(facts).toEqual(['released', 'QA pass', 'merged 1234567', 'human edits after review', 'reverted (heuristic)', 'cost unknown'])
    // human_edits_after_review null (unknown) is omitted, not "no edits"
    expect(outcomeFacts(outcome()).some(f => f.includes('human edits'))).toBe(false)
  })
})

describe('ConfigSnapshotBadge', () => {
  it('links the snapshot to its diff against the current default', () => {
    render(<MemoryRouter><ConfigSnapshotBadge snapshotId={SNAP} outcome={outcome()} /></MemoryRouter>)
    const link = screen.getByText(/config aaaaaaaaaaaa · diff/)
    expect(link.getAttribute('href')).toBe(configDiffLink(SNAP))
    expect(screen.getByText(/QA pass/)).toBeTruthy()
  })

  it('renders nothing for an older backend', () => {
    const { container } = render(<MemoryRouter><ConfigSnapshotBadge /></MemoryRouter>)
    expect(container.innerHTML).toBe('')
  })
})

describe('SnapshotRollupTable / DiffView', () => {
  it('renders one row per snapshot with its rollups', () => {
    render(<MemoryRouter><SnapshotRollupTable rows={[rollup(), rollup({ id: 'b'.repeat(64), agent_type: '*', provider: '', model_id: '', is_current_default: false })]} /></MemoryRouter>)
    expect(screen.getAllByTestId('snapshot-row')).toHaveLength(2)
    expect(screen.getAllByText('75%')).toHaveLength(2)
    expect(screen.getByText(/pipeline \(intake\)/)).toBeTruthy()
    expect(screen.getByText('resolved per agent')).toBeTruthy()
    expect(screen.getAllByText('current')).toHaveLength(1)
  })

  it('shows the empty state', () => {
    render(<MemoryRouter><SnapshotRollupTable rows={[]} /></MemoryRouter>)
    expect(screen.getByText(/No configuration snapshots recorded yet/)).toBeTruthy()
  })

  it('renders changes and the identical case', () => {
    const { rerender } = render(<DiffView diff={{ a: SNAP, b: 'c'.repeat(64), b_is_current: true, identical: false,
      changes: [{ path: 'model.model_id', kind: 'changed', before: 'claude-x', after: 'claude-y' }] }} />)
    expect(screen.getByText('model.model_id')).toBeTruthy()
    expect(screen.getByText('claude-y')).toBeTruthy()
    rerender(<DiffView diff={{ a: SNAP, b: SNAP, b_is_current: false, identical: true, changes: [] }} />)
    expect(screen.getByText('Identical configuration.')).toBeTruthy()
  })
})
