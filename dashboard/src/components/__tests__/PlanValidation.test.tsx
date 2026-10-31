import { describe, expect, it } from 'vitest'
import { render, screen } from '@testing-library/react'
import PlanValidation, { attemptsPhrase, isCurrentPlanValidation, isPlanValidationArtifact, planValidationFrom } from '../PlanValidation'

const record = {
  name: 'plan-validation',
  schema_version: 1,
  attempt: 2,
  violations: [
    {
      code: 'orphan_criterion',
      severity: 'error',
      criterion_id: 'AC-2',
      message: 'AC-2 ("Tax is computed on the discounted subtotal") is owned by no task; add it to the criterion_ids of the task that satisfies it',
    },
    {
      code: 'no_criteria',
      severity: 'error',
      task_index: 2,
      message: 'task 2 "Compute tax" cites no acceptance criterion',
    },
    {
      code: 'hypothesis_only_task',
      severity: 'warning',
      task_index: 3,
      message: 'task 3 "Style": every file it names is a hypothesis (web/a.css)',
    },
  ],
}

describe('planValidationFrom', () => {
  it('reads the record from an object or a JSON string', () => {
    expect(planValidationFrom(record)?.violations).toHaveLength(3)
    expect(planValidationFrom(JSON.stringify(record))?.attempt).toBe(2)
  })

  it('is not a brief, and a brief is not a record', () => {
    expect(isPlanValidationArtifact(record)).toBe(true)
    expect(isPlanValidationArtifact({ narrative: 'x', criteria: {} })).toBe(false)
    expect(planValidationFrom({ narrative: 'x' })).toBeNull()
    expect(planValidationFrom('not json')).toBeNull()
  })
})

describe('isCurrentPlanValidation', () => {
  it('is current until a newer brief or subtask exists', () => {
    const at = '2026-10-03T12:00:00Z'
    expect(isCurrentPlanValidation(at, [])).toBe(true)
    expect(isCurrentPlanValidation(at, ['2026-10-03T11:00:00Z', undefined])).toBe(true)
    expect(isCurrentPlanValidation(at, ['2026-10-03T11:00:00Z', '2026-10-03T12:30:00Z'])).toBe(false)
  })
})

describe('PlanValidation', () => {
  it('shows when the record was written', () => {
    render(<PlanValidation data={planValidationFrom(record)!} recordedAt="2026-10-03T12:00:00Z" />)
    expect(screen.getByText(/^Recorded /)).toBeTruthy()
  })

  it('lists every violation with its code, criterion or task, and message', () => {
    render(<PlanValidation data={planValidationFrom(record)!} />)
    expect(screen.getByText(/rejected after 2 attempts/)).toBeTruthy()
    expect(screen.getByText(/2 violations, 1 warning/)).toBeTruthy()
    expect(screen.getByText('orphan_criterion')).toBeTruthy()
    expect(screen.getByText('AC-2')).toBeTruthy()
    expect(screen.getByText('task 2')).toBeTruthy()
    expect(screen.getByText(/is owned by no task/)).toBeTruthy()
    expect(screen.getByText('hypothesis_only_task')).toBeTruthy()
  })

  // R9: a second repair round may run, and the record's note says why the
  // planner stopped (orphan threshold, spend).
  it('names the repair rounds and shows the note', () => {
    const note = 'The repair round fixed 1 of the 2 violation(s) it was asked to fix, but a second repair round was not tried.'
    render(<PlanValidation data={planValidationFrom({ ...record, attempt: 3, note: ` ${note}` })!} />)
    expect(screen.getByText(/after 3 attempts \(the decomposition and 2 repair rounds\)/)).toBeTruthy()
    expect(screen.getByText(note)).toBeTruthy()
    expect(attemptsPhrase(2)).toBe('the decomposition and one repair round')
    expect(planValidationFrom(record)?.note).toBeUndefined()
  })
})
