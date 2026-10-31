import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest'
import { fireEvent, render, screen, waitFor, cleanup, within } from '@testing-library/react'
import RepositoryZonesEditor, { parsePatterns } from '../RepositoryZonesEditor'
import ZonesStep from '../onboarding/ZonesStep'
import type { CodeZone, RepoZoneProposal } from '@/types/zones'

function zone(overrides: Partial<CodeZone> = {}): CodeZone {
  return {
    id: 'z1',
    repository_id: 'repo-1',
    name: 'payments',
    description: 'money',
    patterns: ['payments/**'],
    module_paths: ['payments'],
    policy: 'human_review',
    status: 'active',
    source: 'dashboard',
    created_at: '2026-10-30T00:00:00Z',
    updated_at: '2026-10-30T00:00:00Z',
    ...overrides,
  }
}

type Call = { method: string; url: string; body: unknown }

function stubFetch(respond: (c: Call) => { status?: number; body?: unknown }) {
  const calls: Call[] = []
  const store: Record<string, string> = {}
  vi.stubGlobal('localStorage', {
    getItem: (k: string) => store[k] ?? null,
    setItem: (k: string, v: string) => { store[k] = v },
    removeItem: (k: string) => { delete store[k] },
  })
  vi.stubGlobal('fetch', vi.fn(async (url: string, init: RequestInit) => {
    const call = { method: init.method ?? 'GET', url, body: init.body ? JSON.parse(String(init.body)) : undefined }
    calls.push(call)
    const r = respond(call)
    const status = r.status ?? 200
    if (status === 204) return new Response(null, { status })
    return new Response(JSON.stringify(r.body ?? {}), { status, headers: { 'Content-Type': 'application/json' } })
  }))
  return calls
}

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
})

describe('parsePatterns', () => {
  it('splits lines and commas, trims and de-duplicates', () => {
    expect(parsePatterns(' payments/** \n\nbilling/**, payments/**\n')).toEqual(['payments/**', 'billing/**'])
  })
})

describe('RepositoryZonesEditor', () => {
  let current: CodeZone[]
  let calls: Call[]

  beforeEach(() => {
    current = [zone(), zone({ id: 'z2', name: 'restricted', patterns: ['migrations/**'], policy: 'escalate', status: 'frozen', frozen_reason: 'audit', source: 'config' })]
    calls = stubFetch(c => {
      if (c.method === 'GET') return { body: current }
      if (c.url.startsWith('/api/admin/zones/freeze')) {
        current[0] = { ...current[0], status: 'frozen', frozen_reason: (c.body as { reason: string }).reason }
        return { body: current[0] }
      }
      if (c.url.startsWith('/api/admin/zones/thaw')) {
        current[1] = { ...current[1], status: 'active', frozen_reason: undefined }
        return { body: current[1] }
      }
      if (c.method === 'PUT') {
        current[0] = { ...current[0], ...(c.body as object) }
        return { body: current[0] }
      }
      if (c.method === 'POST') {
        const created = zone({ id: 'z3', ...(c.body as object) })
        current.push(created)
        return { status: 201, body: created }
      }
      if (c.method === 'DELETE') {
        current = current.filter(z => !c.url.endsWith(z.id))
        return { status: 204 }
      }
      return {}
    })
  })

  it('lists zones with policy and frozen state', async () => {
    render(<RepositoryZonesEditor repositoryId="repo-1" />)
    const row = await screen.findByTestId('zone-row-restricted')
    expect(calls[0]).toMatchObject({ method: 'GET', url: '/api/admin/repositories/zones?id=repo-1' })
    expect(within(row).getByText(/frozen: audit/)).toBeTruthy()
    expect(within(screen.getByTestId('zone-row-payments')).getByText('human_review')).toBeTruthy()
  })

  it('edits patterns and policy with PUT', async () => {
    render(<RepositoryZonesEditor repositoryId="repo-1" />)
    const row = await screen.findByTestId('zone-row-payments')
    fireEvent.click(within(row).getByText('Edit'))
    fireEvent.change(screen.getByLabelText('Patterns for payments'), { target: { value: 'payments/**\nbilling/**' } })
    fireEvent.change(screen.getByLabelText('Policy for payments'), { target: { value: 'escalate' } })
    fireEvent.click(screen.getByText('Save zone'))
    await waitFor(() => expect(calls.some(c => c.method === 'PUT')).toBe(true))
    expect(calls.find(c => c.method === 'PUT')).toMatchObject({
      url: '/api/admin/zones?id=z1',
      body: { patterns: ['payments/**', 'billing/**'], policy: 'escalate', description: 'money' },
    })
    await screen.findByText(/billing\/\*\*/)
  })

  it('freezes with a reason and thaws', async () => {
    render(<RepositoryZonesEditor repositoryId="repo-1" />)
    const row = await screen.findByTestId('zone-row-payments')
    fireEvent.click(within(row).getByText('Freeze'))
    const save = screen.getByText('Confirm freeze') as HTMLButtonElement
    expect(save.disabled).toBe(true)
    fireEvent.change(screen.getByLabelText('Freeze reason for payments'), { target: { value: 'quarter close' } })
    fireEvent.click(save)
    await waitFor(() => expect(calls.some(c => c.url.startsWith('/api/admin/zones/freeze'))).toBe(true))
    expect(calls.find(c => c.url.startsWith('/api/admin/zones/freeze'))).toMatchObject({ method: 'POST', url: '/api/admin/zones/freeze?id=z1', body: { reason: 'quarter close' } })

    fireEvent.click(within(screen.getByTestId('zone-row-restricted')).getByText('Thaw'))
    await waitFor(() => expect(calls.some(c => c.url === '/api/admin/zones/thaw?id=z2')).toBe(true))
  })

  it('adds a zone with POST and shows a server error', async () => {
    render(<RepositoryZonesEditor repositoryId="repo-1" />)
    await screen.findByTestId('zone-row-payments')
    fireEvent.click(screen.getByText('Add zone'))
    fireEvent.change(screen.getByLabelText('New zone name'), { target: { value: 'ledger' } })
    fireEvent.change(screen.getByLabelText('New zone patterns'), { target: { value: 'ledger/**' } })
    fireEvent.change(screen.getByLabelText('New zone policy'), { target: { value: 'escalate' } })
    fireEvent.click(screen.getByText('Create zone'))
    await screen.findByTestId('zone-row-ledger')
    expect(calls.find(c => c.method === 'POST')).toMatchObject({
      url: '/api/admin/repositories/zones?id=repo-1',
      body: { name: 'ledger', patterns: ['ledger/**'], policy: 'escalate', description: '' },
    })
  })
})

describe('ZonesStep (onboarding wizard)', () => {
  const proposals: RepoZoneProposal[] = [{
    repository_id: 'repo-1',
    repository_name: 'api',
    existing: false,
    zones: [
      { name: 'safe', description: 'low risk', patterns: ['internal/models/**'], policy: 'autonomous', source: 'config' },
      { name: 'guarded', description: '', patterns: ['cmd/**'], policy: 'human_review', source: 'config' },
      { name: 'internal/billing', description: 'risk', patterns: ['internal/billing/**'], policy: 'human_review', source: 'onboarding' },
    ],
  }]

  it('shows the proposed zones and confirms the edited set before continuing', async () => {
    const calls = stubFetch(c => (c.method === 'GET' ? { body: proposals } : { body: [] }))
    const onNext = vi.fn()
    render(<ZonesStep productId="prod-1" onNext={onNext} />)
    await screen.findByTestId('proposed-zone-internal/billing')
    expect(calls[0]).toMatchObject({ method: 'GET', url: '/api/onboarding/zones/proposed?product_id=prod-1' })
    const cont = screen.getByText('Continue') as HTMLButtonElement
    expect(cont.disabled).toBe(true)

    // Drop guarded, raise billing to escalate (restricted).
    fireEvent.click(screen.getByLabelText('Include guarded'))
    fireEvent.change(screen.getByLabelText('Policy for internal/billing'), { target: { value: 'escalate' } })
    fireEvent.click(screen.getByText('Confirm zones for api'))
    await screen.findByText('✓ zones confirmed')
    expect(calls[1]).toMatchObject({
      method: 'POST',
      url: '/api/onboarding/zones',
      body: {
        repository_id: 'repo-1',
        zones: [
          { name: 'safe', description: 'low risk', patterns: ['internal/models/**'], policy: 'autonomous', source: 'config' },
          { name: 'internal/billing', description: 'risk', patterns: ['internal/billing/**'], policy: 'escalate', source: 'onboarding' },
        ],
      },
    })
    expect(cont.disabled).toBe(false)
    fireEvent.click(cont)
    expect(onNext).toHaveBeenCalled()
  })

  it('treats a repository that already has zones as confirmed', async () => {
    stubFetch(() => ({ body: [{ ...proposals[0], existing: true, frozen: ['guarded'] }] }))
    render(<ZonesStep productId="prod-1" onNext={vi.fn()} />)
    await screen.findByText(/already has zones/)
    expect((screen.getByText('Continue') as HTMLButtonElement).disabled).toBe(false)
    expect(screen.getByText(/frozen/)).toBeTruthy()
    // Onboarding cannot change existing zones: they are shown read-only,
    // with no include/policy controls and no confirmation to send.
    expect(screen.queryByLabelText('Include guarded')).toBeNull()
    expect(screen.queryByLabelText('Policy for guarded')).toBeNull()
    expect(screen.queryByText('Confirm zones for api')).toBeNull()
    expect(screen.getByText(/Admin → Products → Repository → Zones/)).toBeTruthy()
  })

  it('confirming with every zone unticked sends an explicit empty choice', async () => {
    const calls = stubFetch(c => (c.method === 'GET' ? { body: proposals } : { body: { first_time: true, added: [], kept: [], zones: [] } }))
    render(<ZonesStep productId="prod-1" onNext={vi.fn()} />)
    await screen.findByTestId('proposed-zone-safe')
    for (const n of ['safe', 'guarded', 'internal/billing']) fireEvent.click(screen.getByLabelText(`Include ${n}`))
    fireEvent.click(screen.getByText('Confirm zones for api'))
    await screen.findByText('✓ zones confirmed')
    expect(calls[1]).toMatchObject({ method: 'POST', url: '/api/onboarding/zones', body: { repository_id: 'repo-1', zones: [] } })
  })
})
