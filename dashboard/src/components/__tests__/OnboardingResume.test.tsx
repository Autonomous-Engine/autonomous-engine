import { describe, expect, it, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'

const api = {
  get: (path: string) => {
    if (path === '/admin/products') return Promise.resolve([{ id: 'p1', name: 'enmime', description: '' }])
    if (path.startsWith('/admin/repositories')) return Promise.resolve([])
    return Promise.resolve([])
  },
  post: () => Promise.resolve({}),
  put: () => Promise.resolve({}),
  del: () => Promise.resolve({}),
}
vi.mock('@/hooks/useApi', () => ({ useApi: () => api }))
vi.mock('@/hooks/useWebSocket', () => ({
  useWebSocket: () => ({ lastMessage: null, subscribe: () => {}, unsubscribe: () => {}, isConnected: false }),
}))

// A reload sent the wizard back to step 1 and lost its progress (F12). The
// step and product travel in the URL so a reload resumes where it was.
describe('OnboardingWizard resume', () => {
  it('opens the step named in the URL for the product named in the URL', async () => {
    const { default: OnboardingWizard } = await import('../OnboardingWizard')
    render(
      <MemoryRouter initialEntries={['/onboarding?product_id=p1&step=repositories']}>
        <OnboardingWizard />
      </MemoryRouter>,
    )
    expect(await screen.findByRole('heading', { name: 'Add Repositories' })).toBeTruthy()
    expect(screen.getByText(/Setting up/).textContent).toContain('enmime')
  })
})
