import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest'
import { fireEvent, render, screen, waitFor, cleanup } from '@testing-library/react'
import VerificationPolicyForm, { buildPolicyInput } from '../VerificationPolicyForm'
import RepositoryVerificationPolicyEditor, { policyEndpoints } from '../RepositoryVerificationPolicyEditor'
import VerificationPolicyStep from '../onboarding/VerificationPolicyStep'
import type { RepositoryVerificationPolicyView } from '@/types/verificationPolicy'
import type { Repository } from '@/types'

function unsavedView(overrides: Partial<RepositoryVerificationPolicyView> = {}): RepositoryVerificationPolicyView {
  return {
    repository_id: 'repo-1',
    policy: {
      repository_id: 'repo-1',
      test_command: null,
      required_checks: ['test'],
      no_checks_policy: 'block',
      security_review_paths: ['auth*', 'migrations/**', 'go.mod'],
      source: 'default',
    },
    found: false,
    detected_test_command: 'go test ./... -count=1',
    clone_available: true,
    language: 'go',
    suggested_security_review_paths: ['auth*', 'migrations/**', 'go.mod'],
    selectable_checks: ['test', 'build', 'lint'],
    ...overrides,
  }
}

afterEach(() => {
  cleanup()
  vi.restoreAllMocks()
})

describe('buildPolicyInput', () => {
  it('sends null for a command equal to detection or blank, the text otherwise', () => {
    expect(buildPolicyInput(' go test ./... ', 'go test ./...', ['test'], 'block', '').test_command).toBeNull()
    expect(buildPolicyInput('   ', '', ['test'], 'block', '').test_command).toBeNull()
    expect(buildPolicyInput('make check', 'npm test', ['test'], 'block', '').test_command).toBe('make check')
  })

  it('splits, trims and de-duplicates security paths', () => {
    const input = buildPolicyInput('', '', [], 'partial_with_human_review', 'auth*\n\n  auth* \nsecrets/**\n')
    expect(input.security_review_paths).toEqual(['auth*', 'secrets/**'])
    expect(input.required_checks).toEqual([])
    expect(input.no_checks_policy).toBe('partial_with_human_review')
  })
})

describe('VerificationPolicyForm', () => {
  it('preselects block, prefills the detected command and says nothing is saved yet', () => {
    render(<VerificationPolicyForm view={unsavedView()} onSave={vi.fn()} />)
    expect(screen.getByTestId('vp-status').textContent).toMatch(/No verification policy saved yet/)
    expect(screen.getByTestId('vp-status').textContent).toMatch(/blocked/)
    const block = screen.getByDisplayValue('block') as HTMLInputElement
    const partial = screen.getByDisplayValue('partial_with_human_review') as HTMLInputElement
    expect(block.checked).toBe(true)
    expect(partial.checked).toBe(false)
    expect((screen.getByLabelText('Test command') as HTMLInputElement).value).toBe('go test ./... -count=1')
    expect((screen.getByLabelText('Require test') as HTMLInputElement).checked).toBe(true)
    expect((screen.getByLabelText('Require lint') as HTMLInputElement).checked).toBe(false)
  })

  it('warns that every run will block when no command exists and block is chosen', () => {
    render(<VerificationPolicyForm view={unsavedView({ detected_test_command: '' })} onSave={vi.fn()} />)
    expect(screen.getByTestId('vp-will-block')).toBeTruthy()
    fireEvent.click(screen.getByDisplayValue('partial_with_human_review'))
    expect(screen.queryByTestId('vp-will-block')).toBeNull()
  })

  it('warns that requiring lint caps the outcome at partially verified', () => {
    render(<VerificationPolicyForm view={unsavedView()} onSave={vi.fn()} />)
    expect(screen.queryByTestId('vp-unimplemented')).toBeNull()
    fireEvent.click(screen.getByLabelText('Require lint'))
    expect(screen.getByTestId('vp-unimplemented').textContent).toMatch(/lint/)
  })

  it('submits the explicit answers', async () => {
    const onSave = vi.fn().mockResolvedValue(undefined)
    render(<VerificationPolicyForm view={unsavedView()} onSave={onSave} />)
    fireEvent.change(screen.getByLabelText('Test command'), { target: { value: 'make test' } })
    fireEvent.click(screen.getByLabelText('Require build'))
    fireEvent.click(screen.getByDisplayValue('partial_with_human_review'))
    fireEvent.change(screen.getByLabelText(/Security-review paths/), { target: { value: 'auth*\npayments/**' } })
    fireEvent.click(screen.getByText('Save policy'))
    await waitFor(() => expect(onSave).toHaveBeenCalledTimes(1))
    expect(onSave).toHaveBeenCalledWith({
      test_command: 'make test',
      required_checks: ['test', 'build'],
      no_checks_policy: 'partial_with_human_review',
      security_review_paths: ['auth*', 'payments/**'],
    })
  })

  it('shows who saved a stored policy', () => {
    const view = unsavedView({ found: true })
    view.policy = { ...view.policy, source: 'admin', set_by: 'user:abc' }
    render(<VerificationPolicyForm view={view} onSave={vi.fn()} />)
    expect(screen.getByTestId('vp-status').textContent).toMatch(/Saved via admin by user:abc/)
  })
})

describe('policyEndpoints', () => {
  it('routes admin edits to dashboard-backend and onboarding to the onboarding service', () => {
    expect(policyEndpoints('admin', 'r 1')).toEqual({
      load: '/admin/repositories/verification-policy?id=r%201',
      save: '/admin/repositories/verification-policy?id=r%201',
    })
    expect(policyEndpoints('onboarding', 'r1')).toEqual({
      load: '/onboarding/verification-policy?repository_id=r1',
      save: '/onboarding/verification-policy',
    })
  })
})

// fetch-level tests of the editor and the wizard step: useApi goes through
// global fetch, so stub it and assert on method, URL and body.
describe('RepositoryVerificationPolicyEditor and VerificationPolicyStep', () => {
  let calls: Array<{ method: string; url: string; body: unknown }>

  beforeEach(() => {
    calls = []
    const store: Record<string, string> = {}
    vi.stubGlobal('localStorage', {
      getItem: (k: string) => store[k] ?? null,
      setItem: (k: string, v: string) => { store[k] = v },
      removeItem: (k: string) => { delete store[k] },
    })
    vi.stubGlobal('fetch', vi.fn(async (url: string, init: RequestInit) => {
      const body = init.body ? JSON.parse(String(init.body)) : undefined
      calls.push({ method: init.method ?? 'GET', url, body })
      const view = unsavedView()
      if (init.method === 'PUT' || init.method === 'POST') {
        view.found = true
        view.policy = { ...view.policy, ...(body as object), source: init.method === 'PUT' ? 'admin' : 'onboarding' }
      }
      return new Response(JSON.stringify(view), { status: 200, headers: { 'Content-Type': 'application/json' } })
    }))
  })

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('admin mode loads and saves with PUT', async () => {
    render(<RepositoryVerificationPolicyEditor repositoryId="repo-1" mode="admin" />)
    await screen.findByText('Save policy')
    expect(calls[0]).toMatchObject({ method: 'GET', url: '/api/admin/repositories/verification-policy?id=repo-1' })
    fireEvent.click(screen.getByText('Save policy'))
    await screen.findByText('Saved.')
    expect(calls[1]).toMatchObject({
      method: 'PUT',
      url: '/api/admin/repositories/verification-policy?id=repo-1',
      body: { test_command: null, required_checks: ['test'], no_checks_policy: 'block' },
    })
  })

  it('the wizard step continues only after every repository has a saved policy', async () => {
    const onNext = vi.fn()
    const repos = [{ id: 'repo-1', name: 'svc' }] as Repository[]
    render(<VerificationPolicyStep repos={repos} onNext={onNext} />)
    await screen.findByText('Save policy')
    const cont = screen.getByText('Continue') as HTMLButtonElement
    expect(cont.disabled).toBe(true)
    expect(calls[0]).toMatchObject({ method: 'GET', url: '/api/onboarding/verification-policy?repository_id=repo-1' })

    fireEvent.click(screen.getByText('Save policy'))
    await screen.findByText('✓ policy saved')
    expect(calls[1]).toMatchObject({
      method: 'POST',
      url: '/api/onboarding/verification-policy',
      body: { repository_id: 'repo-1', no_checks_policy: 'block' },
    })
    expect(cont.disabled).toBe(false)
    fireEvent.click(cont)
    expect(onNext).toHaveBeenCalled()
  })
})
