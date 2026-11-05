import { describe, expect, it, vi } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import type { Prompt } from '@/types/prompts'

const api = { get: () => Promise.resolve([]), post: () => Promise.resolve({}), put: () => Promise.resolve({}), del: () => Promise.resolve({}) }
vi.mock('@/hooks/useApi', () => ({ useApi: () => api }))
const answerPrompt = vi.fn((_id: string, _body: unknown) => Promise.resolve({}))
vi.mock('@/hooks/usePrompts', () => ({
  usePromptList: () => ({ prompts: [], loading: false, error: null, refresh: () => {} }),
  answerPrompt: (id: string, body: unknown) => answerPrompt(id, body),
}))

const clarification: Prompt = {
  id: 'c1', work_item_id: 'w1', product_id: 'prod', asking_agent: 'research-agent',
  trigger: 'clarification' as Prompt['trigger'], prompt_type: 'clarification' as Prompt['prompt_type'],
  headline: 'Which charset should win?', question: 'GB2312 or GB18030?', actions: [],
  state: 'pending' as Prompt['state'], requested_at: '2026-11-03T02:28:05Z',
}

// Every answer defaulted to "codex fact", so Approve and Retry clicks wrote
// empty codex rows and bumped the codex version (F38). The default is
// "just this run"; a codex fact is an explicit choice.
describe('PromptDetailPane save-as default', () => {
  it('defaults to just this run and sends ephemeral', async () => {
    const { PromptDetailPane } = await import('../PromptSheet')
    render(<PromptDetailPane prompt={clarification} onSubmitted={() => {}} />)
    expect((screen.getByLabelText('just this run') as HTMLInputElement).checked).toBe(true)
    expect((screen.getByLabelText('codex fact') as HTMLInputElement).checked).toBe(false)

    fireEvent.change(screen.getByPlaceholderText(/explain or elaborate/i), { target: { value: 'GB18030' } })
    fireEvent.click(screen.getByRole('button', { name: 'Submit' }))
    await waitFor(() => expect(answerPrompt).toHaveBeenCalled())
    const [, body] = answerPrompt.mock.calls[0] as unknown as [string, { save_as_override: string }]
    expect(body.save_as_override).toBe('ephemeral')
  })
})
