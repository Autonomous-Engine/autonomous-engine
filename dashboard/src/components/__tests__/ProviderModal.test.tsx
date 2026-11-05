import { describe, expect, it, vi } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'

const post = vi.fn(() => Promise.resolve({}))
const api = { get: () => Promise.resolve([]), post, put: () => Promise.resolve({}), del: () => Promise.resolve({}) }
vi.mock('@/hooks/useApi', () => ({ useApi: () => api }))

// The model could only be set by typing {"model": "..."} into a raw JSON
// textarea (F14); the modal now has a Model field that lands in settings.
describe('ProviderModal model field', () => {
  it('sends the typed model as settings.model on create', async () => {
    const { ProviderModal } = await import('../admin/ProvidersPage')
    render(<ProviderModal initial={undefined} onClose={() => {}} onSaved={() => {}} />)

    fireEvent.change(screen.getByLabelText('Name'), { target: { value: 'Local Ollama' } })
    fireEvent.change(screen.getByLabelText('Model'), { target: { value: 'qwen3.6:35b' } })
    fireEvent.click(screen.getByRole('button', { name: 'Add Provider' }))

    await waitFor(() => expect(post).toHaveBeenCalled())
    const [path, body] = post.mock.calls[0] as unknown as [string, { settings: { model?: string } }]
    expect(path).toBe('/admin/providers/create')
    expect(body.settings.model).toBe('qwen3.6:35b')
  })
})
