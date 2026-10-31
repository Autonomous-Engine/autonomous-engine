import { describe, expect, it } from 'vitest'
import { fireEvent, render, screen } from '@testing-library/react'
import VerificationEvidence, { checkDuration, isHttpUrl, outcomeBadgeClass, shortSha } from '../VerificationEvidence'
import type { ImplementationVerification } from '@/types/evidence'

const partial: ImplementationVerification = {
  id: 'v1',
  created_at: '2026-09-10T13:00:00Z',
  evidence: {
    schema_version: 1,
    work_id: 'w1',
    task_id: 'task-1',
    repository_id: null,
    base_sha: '0123456789abcdef0123456789abcdef01234567',
    head_sha: '89abcdef0123456789abcdef0123456789abcdef',
    branch: 'ae/feature-x',
    changed_files: ['internal/a.go', 'internal/a_test.go'],
    checks: [
      {
        kind: 'build', command: 'go build ./...', exit_code: 0, status: 'passed',
        started_at: '2026-09-10T12:00:00Z', finished_at: '2026-09-10T12:01:30Z',
        output_ref: 'https://artifacts.example/build.log',
      },
      {
        kind: 'test', command: 'go test ./...', exit_code: null, status: 'skipped',
        skip_reason: 'no test runner configured for repository',
        output_ref: 'artifact:abc',
      },
    ],
    outcome: 'partially_verified',
    started_at: '2026-09-10T12:00:00Z',
    finished_at: '2026-09-10T12:01:30Z',
    artifact_refs: ['artifact:diff-1'],
  },
}

describe('VerificationEvidence', () => {
  it('renders the empty state when no evidence exists', () => {
    render(<VerificationEvidence verification={null} />)
    expect(screen.getByText(/No verification evidence recorded/)).toBeTruthy()
  })

  it('shows outcome badge, short SHAs, branch and every check with skip reasons', () => {
    render(<VerificationEvidence verification={partial} />)

    const badge = screen.getByTestId('evidence-outcome')
    expect(badge.textContent).toBe('partially verified')
    expect(badge.className).toContain('amber')

    expect(screen.getByText('0123456789ab')).toBeTruthy()
    expect(screen.getByText('89abcdef0123')).toBeTruthy()
    expect(screen.getByText('ae/feature-x')).toBeTruthy()
    expect(screen.getByLabelText(`Copy Head SHA ${partial.evidence.head_sha}`)).toBeTruthy()

    expect(screen.getByText('go build ./...')).toBeTruthy()
    expect(screen.getByText('go test ./...')).toBeTruthy()
    expect(screen.getByText('no test runner configured for repository')).toBeTruthy()
    expect(screen.getByText('skipped')).toBeTruthy()
    expect(screen.getByText('passed')).toBeTruthy()
    expect(screen.getByText('1m 30s')).toBeTruthy()

    const link = screen.getByRole('link', { name: 'output' })
    expect(link.getAttribute('href')).toBe('https://artifacts.example/build.log')
    expect(link.getAttribute('target')).toBe('_blank')
    // Non-URL references are shown verbatim, never linked.
    expect(screen.getByText('artifact:abc').tagName).toBe('CODE')

    expect(screen.getByText(/advance only to mandatory human review/)).toBeTruthy()
  })

  it('shows the security-review notice only when the evidence carries the flag', () => {
    const { unmount } = render(<VerificationEvidence verification={partial} />)
    expect(screen.queryByTestId('evidence-security-review')).toBeNull()
    unmount()
    const flagged: ImplementationVerification = {
      ...partial,
      evidence: { ...partial.evidence, flags: ['security_review_required'] },
    }
    render(<VerificationEvidence verification={flagged} />)
    expect(screen.getByTestId('evidence-security-review').textContent).toMatch(/Security review required/)
  })

  it('expands the changed-files list on demand', () => {
    render(<VerificationEvidence verification={partial} />)
    expect(screen.queryByTestId('evidence-changed-files')).toBeNull()
    expect(screen.getByText('2')).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: 'Show' }))
    const list = screen.getByTestId('evidence-changed-files')
    expect(list.textContent).toContain('internal/a.go')
    expect(list.textContent).toContain('internal/a_test.go')
    fireEvent.click(screen.getByRole('button', { name: 'Hide' }))
    expect(screen.queryByTestId('evidence-changed-files')).toBeNull()
  })

  it('colours outcomes per policy and greys unknown outcomes', () => {
    expect(outcomeBadgeClass('verified')).toContain('emerald')
    expect(outcomeBadgeClass('partially_verified')).toContain('amber')
    expect(outcomeBadgeClass('failed')).toContain('red')
    expect(outcomeBadgeClass('unverified')).toContain('text-muted')
    expect(outcomeBadgeClass('something_new')).toBe(outcomeBadgeClass('unverified'))
  })

  it('helpers', () => {
    expect(shortSha('89abcdef0123456789abcdef0123456789abcdef')).toBe('89abcdef0123')
    expect(shortSha('abc')).toBe('abc')
    expect(isHttpUrl('https://x/y')).toBe(true)
    expect(isHttpUrl('artifact:abc')).toBe(false)
    expect(checkDuration({})).toBe('—')
    expect(checkDuration({ started_at: '2026-01-01T00:00:00Z', finished_at: '2026-01-01T00:00:00.250Z' })).toBe('250ms')
    expect(checkDuration({ started_at: '2026-01-01T00:00:00Z', finished_at: '2026-01-01T00:02:05Z' })).toBe('2m 5s')
  })
})
