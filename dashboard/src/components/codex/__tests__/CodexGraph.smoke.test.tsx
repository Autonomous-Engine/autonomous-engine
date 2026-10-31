import { describe, expect, it, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import { CodexGraph } from '../CodexGraph'
import type { GraphResponse } from '@/types'

// ReactFlow requires measured DOM; stub ResizeObserver for jsdom.
class RO { observe() {} unobserve() {} disconnect() {} }
vi.stubGlobal('ResizeObserver', RO)
vi.stubGlobal('DOMRect', class { static fromRect() { return { x: 0, y: 0, width: 800, height: 600, top: 0, left: 0, right: 800, bottom: 600 } } })

const fakeResponse: GraphResponse = {
  product_id: 'p1',
  product_name: 'Demo',
  generated_at: '2026-04-18T00:00:00Z',
  meta: { node_count: 3, edge_count: 2, truncated: false },
  nodes: [
    {
      id: 'a', name: 'alpha', purpose: 'first', risk_level: 'low',
      owner_domain: '', test_coverage_notes: '', public_interface_summary: '',
      dependencies: ['beta'], dependents: [], dependencies_count: 1, dependents_count: 0,
      external: false,
    },
    {
      id: 'b', name: 'beta', purpose: 'second', risk_level: 'high',
      owner_domain: '', test_coverage_notes: '', public_interface_summary: '',
      dependencies: ['pgx'], dependents: ['alpha'], dependencies_count: 1, dependents_count: 1,
      external: false,
    },
    {
      id: 'external:pgx', name: 'pgx', purpose: 'ext', risk_level: 'unknown',
      owner_domain: '', test_coverage_notes: '', public_interface_summary: '',
      dependencies: [], dependents: [], dependencies_count: 0, dependents_count: 0,
      external: true,
    },
  ],
  edges: [
    { id: 'e1', source: 'a', target: 'b', kind: 'depends_on', resolved: true },
    { id: 'e2', source: 'b', target: 'external:pgx', kind: 'depends_on', resolved: false },
  ],
}

describe('CodexGraph smoke', () => {
  it('renders a node for each item in data.nodes', async () => {
    render(<CodexGraph data={fakeResponse} />)
    // ReactFlow renders nodes asynchronously; wait for each to appear.
    expect(await screen.findByLabelText(/alpha/i)).toBeTruthy()
    expect(await screen.findByLabelText(/beta/i)).toBeTruthy()
    expect(await screen.findByLabelText(/pgx/i)).toBeTruthy()
  })

  it('renders the empty-state card when the server reports no indexed modules', () => {
    render(<CodexGraph data={{ ...fakeResponse, nodes: [], edges: [], meta: { ...fakeResponse.meta, node_count: 0, edge_count: 0, empty_reason: 'no_modules_indexed' } }} />)
    expect(screen.getByText(/no indexed modules yet/i)).toBeTruthy()
  })

  it('renders the error state with a Retry button when error is passed', () => {
    const onRetry = vi.fn()
    render(<CodexGraph data={null} error="boom" onRetry={onRetry} />)
    const btn = screen.getByRole('button', { name: /retry/i })
    btn.click()
    expect(onRetry).toHaveBeenCalledOnce()
  })
})
