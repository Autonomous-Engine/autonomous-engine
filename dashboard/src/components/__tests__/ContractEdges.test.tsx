import { describe, expect, it } from 'vitest'
import { render, screen } from '@testing-library/react'
import ContractEdges, { contractStatusClass } from '../ContractEdges'
import type { ContractEdge } from '@/types'

const edge: ContractEdge = {
  consumer_task_id: 'c0000000-0000-0000-0000-000000000000',
  consumer_title: 'Implement in web',
  consumer_repository: 'web',
  producer_task_id: 'p0000000-0000-0000-0000-000000000000',
  producer_title: 'Implement in api',
  producer_repository: 'api',
  contracts: [{ type: 'rest', name: 'GET /api/users/{id}', producer_repo_id: 'r-api', consumer_repo_id: 'r-web', source: 'codex' }],
  last_check: null,
}

describe('ContractEdges', () => {
  it('renders nothing without edges (and for an older backend)', () => {
    const { container } = render(<ContractEdges />)
    expect(container.innerHTML).toBe('')
  })

  it('shows producer → consumer, the contract, and that it has not been checked', () => {
    render(<ContractEdges edges={[edge]} />)
    expect(screen.getByText('api')).toBeTruthy()
    expect(screen.getByText('web')).toBeTruthy()
    expect(screen.getByText('GET /api/users/{id}')).toBeTruthy()
    expect(screen.getByText('not checked yet')).toBeTruthy()
  })

  it('shows a mismatch with the producer change and the consumer reference', () => {
    const checked: ContractEdge = {
      ...edge,
      last_check: {
        status: 'mismatch',
        checked_at: '2026-10-03T12:00:00Z',
        results: [{
          contract: edge.contracts[0],
          status: 'mismatch',
          mismatches: [{ producer_change: 'removed field email from the GET /api/users/{id} response (openapi.json)', consumer_ref: 'src/user.ts:4: return body.email' }],
        }],
      },
    }
    render(<ContractEdges edges={[checked]} />)
    expect(screen.getAllByText('mismatch').length).toBe(2)
    expect(screen.getByText('src/user.ts:4: return body.email')).toBeTruthy()
    expect(screen.getByText(/removed field email/)).toBeTruthy()
  })

  it('colours unknown statuses neutrally', () => {
    expect(contractStatusClass('ok')).toContain('emerald')
    expect(contractStatusClass('unchecked')).toContain('amber')
    expect(contractStatusClass('')).toContain('text-muted')
  })
})
