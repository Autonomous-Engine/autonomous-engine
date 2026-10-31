import { describe, expect, it } from 'vitest'
import { render, screen } from '@testing-library/react'
import BriefCriteria, { briefCriteriaFrom, evidenceKindClass } from '../BriefCriteria'

const artifact = {
  brief: 'prose',
  narrative: 'prose',
  criteria: {
    schema_version: 1,
    criteria: [
      {
        id: 'AC-1',
        text: 'Submitting the form with an empty email shows an error',
        source: { kind: 'user', quote: 'must not accept an empty email' },
        risk: 'medium',
        required_checks: ['test', 'build'],
      },
      {
        id: 'AC-2',
        text: 'Existing submissions keep working',
        source: { kind: 'derived', reason: 'the handler is shared' },
        risk: 'low',
      },
    ],
    file_evidence: {
      'src/components/Contact.tsx': { kind: 'repo', ref: 'call_3', confidence: 0.9, basis: 'clone' },
      'src/old/Legacy.tsx': { kind: 'hypothesis', confidence: 0, note: 'not found in the clone' },
    },
  },
}

describe('briefCriteriaFrom', () => {
  it('reads criteria from an object or a JSON string', () => {
    expect(briefCriteriaFrom(artifact)?.criteria).toHaveLength(2)
    expect(briefCriteriaFrom(JSON.stringify(artifact))?.criteria).toHaveLength(2)
  })

  it('returns null for a legacy brief or junk', () => {
    expect(briefCriteriaFrom({ brief: 'x' })).toBeNull()
    expect(briefCriteriaFrom('not json')).toBeNull()
    expect(briefCriteriaFrom(null)).toBeNull()
  })
})

describe('BriefCriteria', () => {
  it('renders id, text, risk, checks and source for each criterion', () => {
    render(<BriefCriteria data={briefCriteriaFrom(artifact)} />)
    expect(screen.getByText('AC-1')).toBeTruthy()
    expect(screen.getByText('Submitting the form with an empty email shows an error')).toBeTruthy()
    expect(screen.getByText('risk: medium')).toBeTruthy()
    expect(screen.getByText('test')).toBeTruthy()
    expect(screen.getByText('build')).toBeTruthy()
    expect(screen.getByText(/must not accept an empty email/)).toBeTruthy()
    expect(screen.getByText(/derived: the handler is shared/)).toBeTruthy()
  })

  it('renders the file evidence table with path, kind and ref, hypotheses included', () => {
    render(<BriefCriteria data={briefCriteriaFrom(artifact)} />)
    expect(screen.getByRole('table', { name: 'File evidence' })).toBeTruthy()
    expect(screen.getByText('src/components/Contact.tsx')).toBeTruthy()
    expect(screen.getByText('call_3')).toBeTruthy()
    expect(screen.getByText('src/old/Legacy.tsx')).toBeTruthy()
    expect(screen.getByText('hypothesis').className).toContain('amber')
    expect(screen.getByText('not found in the clone')).toBeTruthy()
  })

  it('says so when the brief has no criteria', () => {
    render(<BriefCriteria data={null} />)
    expect(screen.getByText(/No acceptance criteria/)).toBeTruthy()
  })

  it('has a neutral class for an unknown evidence kind', () => {
    expect(evidenceKindClass('whatever')).toContain('text-muted')
  })
})
