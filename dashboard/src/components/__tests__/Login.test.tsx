import { describe, expect, it, vi, beforeEach } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'

const navigate = vi.fn()
vi.mock('react-router-dom', async importOriginal => {
  const mod = await importOriginal<typeof import('react-router-dom')>()
  return { ...mod, useNavigate: () => navigate }
})

const api = {
  get: (path: string) => {
    if (path === '/auth/providers') return Promise.resolve([{ id: 'local', provider_type: 'local', name: 'Local', enabled: true, is_primary: true, config: {} }])
    return Promise.resolve([])
  },
  // The backend nests the flag under user; the page read it top-level and
  // never prompted for the first-run password change (F2).
  post: () => Promise.resolve({ token: 't0k', user: { id: 'u', email: 'admin@localhost', name: '', role: 'admin', password_change_required: true } }),
}
vi.mock('@/hooks/useApi', () => ({ useApi: () => api }))

describe('Login first-run password change', () => {
  beforeEach(() => {
    // Node's experimental localStorage shadows jsdom's here and lacks clear().
    const store = new Map<string, string>()
    vi.stubGlobal('localStorage', {
      getItem: (k: string) => store.get(k) ?? null,
      setItem: (k: string, v: string) => { store.set(k, String(v)) },
      removeItem: (k: string) => { store.delete(k) },
      clear: () => store.clear(),
    })
    navigate.mockClear()
  })

  it('sends the user to /change-password when the login response requires it', async () => {
    const { default: Login } = await import('../Login')
    render(<MemoryRouter><Login /></MemoryRouter>)
    fireEvent.change(await screen.findByLabelText(/email/i), { target: { value: 'admin@localhost' } })
    fireEvent.change(screen.getByLabelText(/password/i), { target: { value: 'secret' } })
    fireEvent.submit(screen.getByLabelText(/email/i).closest('form')!)
    await waitFor(() => expect(navigate).toHaveBeenCalledWith('/change-password'))
  })
})
