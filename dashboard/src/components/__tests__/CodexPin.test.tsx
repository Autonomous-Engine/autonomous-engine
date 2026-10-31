import { describe, expect, it } from 'vitest'
import { render, screen } from '@testing-library/react'
import { CodexPinBadge, CodexEventLine, codexEventSummary } from '../CodexPin'
import type { CodexChangeNote, CodexEvent, CodexPin } from '@/types'

const pin: CodexPin = { product_version: 12, branch: 'main', modules: ['auth', 'billing'], pinned_at: '2026-10-30T10:00:00Z' }

const replanned: CodexEvent = {
  id: 'e1', kind: 'replanned', automatic: true, branch: 'main', from_version: 12, to_version: 14,
  modules: ['billing'], sections: [], item_state: 'implementation',
  reason: 'codex_changed: modules billing (v12 → v14)', created_at: '2026-10-30T11:00:00Z',
}

describe('CodexPinBadge', () => {
  it('shows the pinned codex version and modules', () => {
    render(<CodexPinBadge pin={pin} />)
    expect(screen.getByText('codex v12')).toBeTruthy()
    expect(screen.getByText('auth, billing')).toBeTruthy()
  })

  it('renders nothing for an unpinned item', () => {
    const { container } = render(<CodexPinBadge />)
    expect(container.textContent).toBe('')
  })

  it('flags an item whose codex changed since planning', () => {
    const note: CodexChangeNote = { kind: 'noted', to_version: 14, modules: ['auth'], sections: [], reason: 'codex changed since planning: modules auth (v12 → v14)', at: '2026-10-30T11:00:00Z' }
    render(<CodexPinBadge pin={pin} note={note} />)
    const flag = screen.getByText('codex changed since planning')
    expect(flag.getAttribute('title')).toContain('modules auth (v12 → v14)')
  })

  it('shows more than three modules as a count', () => {
    render(<CodexPinBadge pin={{ ...pin, modules: ['a', 'b', 'c', 'd', 'e'] }} />)
    expect(screen.getByText('a, b, c +2')).toBeTruthy()
  })
})

describe('CodexEventLine', () => {
  it('shows a re-plan with the modules that changed and the versions', () => {
    render(<CodexEventLine event={replanned} />)
    expect(screen.getByText('re-planned')).toBeTruthy()
    expect(screen.getByText('v12 → v14')).toBeTruthy()
    expect(screen.getByText('modules: billing')).toBeTruthy()
  })

  it('says who chose an operator re-plan and shows changed sections', () => {
    render(<CodexEventLine event={{ ...replanned, automatic: false, modules: [], sections: ['risk_zones'] }} />)
    expect(screen.getByText('re-planned (operator)')).toBeTruthy()
    expect(screen.getByText('sections: risk_zones')).toBeTruthy()
  })

  it('names every kind', () => {
    expect(codexEventSummary({ ...replanned, kind: 'escalated' })).toBe('re-plan escalated')
    expect(codexEventSummary({ ...replanned, kind: 'noted' })).toBe('codex changed since planning')
    expect(codexEventSummary({ ...replanned, kind: 'continued', automatic: false })).toBe('continued as is')
  })
})
