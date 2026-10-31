import { describe, expect, it } from 'vitest'
import { render, screen } from '@testing-library/react'
import BaseSnapshot, { eventKindClass, sha12 } from '../BaseSnapshot'
import type { BaseShaEvent, BaseShaPin } from '@/types'

const pin: BaseShaPin = {
  repository_id: 'r1',
  repository_name: 'ae-test-app',
  base_ref: 'main',
  base_sha: '7af0d7084cd8b7c8a96b50da4311b560bebcb8e8',
  resolved_at: '2026-10-03T12:00:00Z',
}

const events: BaseShaEvent[] = [
  {
    id: 'e1', repository_id: 'r1', repository_name: 'ae-test-app', kind: 'resolved',
    old_sha: '', new_sha: pin.base_sha, detail: 'intake: refs/heads/main via ls-remote', created_at: '2026-10-03T12:00:00Z',
  },
  {
    id: 'e2', repository_id: 'r1', repository_name: 'ae-test-app', kind: 'drift_detected',
    old_sha: pin.base_sha, new_sha: '0123456789abcdef0123456789abcdef01234567',
    detail: 'implementation-agent finalize: pinned base is not an ancestor of HEAD', created_at: '2026-10-03T13:00:00Z',
  },
]

describe('BaseSnapshot', () => {
  it('says the item is unpinned when there is no pin', () => {
    render(<BaseSnapshot pins={[]} events={[]} />)
    expect(screen.getByText(/No pinned base commit/)).toBeTruthy()
  })

  it('tolerates an older backend that sends neither field', () => {
    render(<BaseSnapshot />)
    expect(screen.getByText(/No pinned base commit/)).toBeTruthy()
  })

  it('shows Base: <ref> @ <sha12> with the full SHA on hover', () => {
    render(<BaseSnapshot pins={[pin]} events={[]} />)
    expect(screen.getByText(/Base: main @/)).toBeTruthy()
    const code = screen.getByText('7af0d7084cd8')
    expect(code.getAttribute('title')).toBe(pin.base_sha)
  })

  it('names the repository when several are pinned', () => {
    render(<BaseSnapshot pins={[pin, { ...pin, repository_id: 'r2', repository_name: 'api', base_sha: '0123456789abcdef0123456789abcdef01234567' }]} />)
    expect(screen.getByText(/ae-test-app:/)).toBeTruthy()
    expect(screen.getByText(/api:/)).toBeTruthy()
  })

  it('lists the events, drift included, with old → new', () => {
    render(<BaseSnapshot pins={[pin]} events={events} />)
    expect(screen.getByText('resolved')).toBeTruthy()
    expect(screen.getByText('drift_detected')).toBeTruthy()
    expect(screen.getByText(/not an ancestor of HEAD/)).toBeTruthy()
    expect(screen.getByText('0123456789ab')).toBeTruthy()
  })

  it('helpers', () => {
    expect(sha12('')).toBe('')
    expect(sha12(pin.base_sha)).toBe('7af0d7084cd8')
    expect(eventKindClass('drift_detected')).toContain('amber')
    expect(eventKindClass('future_kind')).toContain('text-muted')
  })
})
