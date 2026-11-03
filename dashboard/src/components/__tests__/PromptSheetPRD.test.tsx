import { describe, expect, it, vi } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import type { Prompt } from '@/types/prompts'

const post = vi.fn(() => Promise.resolve({}))
const api = { get: () => Promise.resolve([]), post, put: () => Promise.resolve({}), del: () => Promise.resolve({}) }
vi.mock('@/hooks/useApi', () => ({ useApi: () => api }))
const answerPrompt = vi.fn((_id: string, _body: unknown) => Promise.resolve({}))
vi.mock('@/hooks/usePrompts', () => ({
  usePromptList: () => ({ prompts: [], loading: false, error: null, refresh: () => {} }),
  answerPrompt: (id: string, body: unknown) => answerPrompt(id, body),
}))

const prdPrompt: Prompt = {
  id: 'p1', work_item_id: 'w1', product_id: 'prod', asking_agent: 'product-agent',
  trigger: 'approval:prd', prompt_type: 'approval',
  headline: 'PRD ready: Add parser option', question: '# PRD\n\n…',
  actions: [{ id: 'approve', label: 'Approve', semantic: 'positive' as never }, { id: 'reject', label: 'Reject', semantic: 'negative' as never }],
  state: 'pending' as Prompt['state'], requested_at: '2026-11-03T02:28:05Z',
}

// PRD review offered Approve or Reject only, and the submit button was a
// second "Approve" (F34). A reviewer can now request changes with feedback.
describe('PromptDetailPane PRD request changes', () => {
  it('sends the feedback to revise-prd and does not answer the prompt itself', async () => {
    const { PromptDetailPane } = await import('../PromptSheet')
    const onSubmitted = vi.fn()
    render(<PromptDetailPane prompt={prdPrompt} onSubmitted={onSubmitted} />)

    expect(screen.getByRole('button', { name: 'Submit decision' })).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: 'Request changes' }))
    fireEvent.change(screen.getByLabelText(/what should change/i), { target: { value: 'Use the existing Option type.' } })
    fireEvent.click(screen.getByRole('button', { name: 'Send back for changes' }))

    await waitFor(() => expect(post).toHaveBeenCalledWith('/admin/work-items/revise-prd?id=w1', { reason: 'Use the existing Option type.' }))
    expect(answerPrompt).not.toHaveBeenCalled()
    await waitFor(() => expect(onSubmitted).toHaveBeenCalled())
  })
})

// Only a PRD approval can be sent back for changes; another approval
// prompt must not show a button that 409s (review minor, re-graded).
describe('PromptDetailPane non-PRD approval', () => {
  it('has no Request changes button', async () => {
    const { PromptDetailPane } = await import('../PromptSheet')
    render(<PromptDetailPane prompt={{ ...prdPrompt, id: 'p2', trigger: 'agent_request' }} onSubmitted={() => {}} />)
    expect(screen.queryByRole('button', { name: 'Request changes' })).toBeNull()
  })
})
