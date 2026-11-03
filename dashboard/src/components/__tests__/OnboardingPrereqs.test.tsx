import { describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'

let providers: unknown[] = []
let embeddings: unknown[] = []
const api = {
  get: (path: string) => {
    if (path.startsWith('/admin/providers')) return Promise.resolve(providers)
    if (path.startsWith('/admin/embedding-providers')) return Promise.resolve(embeddings)
    if (path.startsWith('/admin/github/accounts')) return Promise.resolve([])
    return Promise.resolve([])
  },
  post: () => Promise.resolve({}),
}
vi.mock('@/hooks/useApi', () => ({ useApi: () => api }))

// Onboarding depended on setup done on other admin pages (a default LLM
// provider with a model, a default embedding provider) and never said so;
// a new user hit raw 409s and "not connected" blockers instead (F15).
describe('PrerequisitesPanel', () => {
  it('lists the missing prerequisites with links and reports not ready', async () => {
    providers = []
    embeddings = []
    const { PrerequisitesPanel } = await import('../OnboardingWizard')
    const onStatus = vi.fn()
    render(<MemoryRouter><PrerequisitesPanel onStatus={onStatus} /></MemoryRouter>)

    await waitFor(() => expect(onStatus).toHaveBeenLastCalledWith(false))
    expect(screen.getByText(/LLM provider with a model/)).toBeTruthy()
    expect(screen.getByText(/Embedding provider/)).toBeTruthy()
    expect(screen.getAllByText('Missing').length).toBe(2)
    expect(screen.getByRole('link', { name: /Providers/ }).getAttribute('href')).toBe('/admin/providers')
    expect(screen.getByRole('link', { name: /Embeddings/ }).getAttribute('href')).toBe('/admin/embeddings')
    expect(screen.getAllByText(/optional/i).length).toBeGreaterThan(0) // GitHub account
  })

  it('reports ready once a default provider with a model and a default embedding exist', async () => {
    providers = [{ id: 'a', name: 'Local', provider_type: 'ollama', is_default: true, enabled: true, settings: { model: 'qwen3.6:35b' } }]
    embeddings = [{ id: 'e', name: 'Emb', is_default: true, enabled: true }]
    const { PrerequisitesPanel } = await import('../OnboardingWizard')
    const onStatus = vi.fn()
    render(<MemoryRouter><PrerequisitesPanel onStatus={onStatus} /></MemoryRouter>)
    await waitFor(() => expect(onStatus).toHaveBeenLastCalledWith(true))
    expect(screen.getAllByText('Ready').length).toBe(2)
  })
})
