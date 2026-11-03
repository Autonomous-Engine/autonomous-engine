import { describe, expect, it, vi } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'

const post = vi.fn()
let postResolve: (v: unknown) => void = () => {}

vi.mock('@/hooks/useWebSocket', () => ({
  useWebSocket: () => ({ lastMessage: null, subscribe: () => {}, unsubscribe: () => {}, isConnected: true }),
}))
// One stable api object: the step keys its fetch effect on `get`, so a mock
// that returns new functions per render would refetch forever.
const api = {
  get: (path: string) => {
    if (path.includes('codex-stats')) return Promise.resolve({ modules: 8, patterns: 1 })
    if (path.includes('/codex/graph')) return Promise.resolve({ meta: { edge_count: 25, external_count: 1 }, nodes: [], edges: [] })
    return Promise.resolve([])
  },
  post: (path: string, body: unknown) => {
    post(path, body)
    return new Promise(resolve => { postResolve = resolve })
  },
}
vi.mock('@/hooks/useApi', () => ({ useApi: () => api }))

// "Approve Codex" was onClick={onNext}: it advanced the wizard and approved
// nothing, so every reviewed onboarding answer was discarded (F28).
describe('CodexReviewStep approve', () => {
  it('posts approve-codex for the product and advances only once it succeeds', async () => {
    const { CodexReviewStep } = await import('../OnboardingWizard')
    const onNext = vi.fn()
    render(<CodexReviewStep product={{ id: 'p-1', name: 'enmime' } as never} repos={[]} onNext={onNext} />)

    const button = await screen.findByRole('button', { name: 'Approve Codex' })
    fireEvent.click(button)

    await waitFor(() => expect(post).toHaveBeenCalledWith('/onboarding/approve-codex', { product_id: 'p-1' }))
    expect(onNext).not.toHaveBeenCalled()

    postResolve({ status: 'approved', version_number: 3 })
    await waitFor(() => expect(onNext).toHaveBeenCalledTimes(1))
  })
})
