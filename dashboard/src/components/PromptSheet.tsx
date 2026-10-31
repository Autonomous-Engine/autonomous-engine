// dashboard/src/components/PromptSheet.tsx
//
// Slide-out sheet listing pending prompts with an expanded detail pane.
// Opened by NotificationBell in the header. Submits answers via
// answerPrompt and advances to the next pending prompt automatically.

import { useEffect, useState, type FormEvent } from 'react'
import type { Prompt, SaveAsOverride } from '@/types/prompts'
import {
  usePromptList,
  answerPrompt,
} from '@/hooks/usePrompts'
import Markdown from './Markdown'

interface PromptSheetProps {
  open: boolean
  onClose: () => void
}

const TYPE_LABELS: Record<Prompt['prompt_type'], string> = {
  approval: 'APPROVAL',
  clarification: 'CLARIFY',
  escalation: 'ESCALATE',
  block_question: 'BLOCK-Q',
}

function timeAgo(iso: string): string {
  const ms = Date.now() - new Date(iso).getTime()
  const mins = Math.floor(ms / 60_000)
  if (mins < 1) return 'now'
  if (mins < 60) return `${mins}m`
  const hrs = Math.floor(mins / 60)
  if (hrs < 24) return `${hrs}h`
  return `${Math.floor(hrs / 24)}d`
}

export function PromptSheet({ open, onClose }: PromptSheetProps) {
  const { prompts, refresh } = usePromptList({ state: 'pending' })
  const [selectedId, setSelectedId] = useState<string | null>(null)

  // Keep a selection when the list changes — if the selected prompt drops
  // off (answered), fall through to the next one.
  useEffect(() => {
    if (!open) return
    if (prompts.length === 0) {
      setSelectedId(null)
      return
    }
    if (!selectedId || !prompts.find((p) => p.id === selectedId)) {
      setSelectedId(prompts[0].id)
    }
  }, [open, prompts, selectedId])

  if (!open) return null

  const selected = prompts.find((p) => p.id === selectedId) ?? null

  return (
    <>
      {/* Backdrop — click to close */}
      <div
        className="fixed inset-0 z-30 bg-black/40"
        onClick={onClose}
        aria-hidden="true"
      />
      <aside
        role="dialog"
        aria-label="Pending prompts"
        className="fixed inset-y-0 right-0 z-40 flex w-full flex-col border-l border-rule bg-surface-950 shadow-2xl md:w-[480px]"
      >
        <header className="flex items-center justify-between border-b border-rule px-4 py-3">
          <h2 className="text-sm font-semibold tracking-wide text-fg">
            Pending prompts{' '}
            <span className="ml-1 text-muted">({prompts.length})</span>
          </h2>
          <button
            type="button"
            onClick={onClose}
            aria-label="Close"
            className="text-muted hover:text-fg"
          >
            ✕
          </button>
        </header>

        <div className="max-h-[38%] overflow-y-auto border-b border-rule">
          {prompts.length === 0 ? (
            <div className="p-8 text-center text-sm text-muted">
              You&apos;re all caught up.
            </div>
          ) : (
            prompts.map((p) => (
              <button
                key={p.id}
                type="button"
                onClick={() => setSelectedId(p.id)}
                className={`w-full border-l-2 px-4 py-3 text-left transition ${
                  p.id === selectedId
                    ? 'border-lime-400 bg-surface-800/60'
                    : 'border-transparent hover:bg-surface-800/30'
                }`}
              >
                <div className="flex items-center justify-between text-[0.6rem] font-mono tracking-widest text-muted">
                  <span>{TYPE_LABELS[p.prompt_type]}</span>
                  <span>{timeAgo(p.requested_at)}</span>
                </div>
                <div className="mt-1 text-sm text-fg">{p.headline}</div>
                <div className="mt-1 text-xs text-dim">
                  {p.asking_agent} · {p.work_item_id.slice(0, 8)}
                </div>
              </button>
            ))
          )}
        </div>

        <div className="flex-1 overflow-y-auto">
          {selected ? (
            <PromptDetailPane
              prompt={selected}
              onSubmitted={() => {
                setSelectedId(null)
                refresh()
              }}
            />
          ) : (
            <div className="p-8 text-center text-sm text-dim">
              Select a prompt to view and respond.
            </div>
          )}
        </div>
      </aside>
    </>
  )
}

export function PromptDetailPane({
  prompt,
  onSubmitted,
}: {
  prompt: Prompt
  onSubmitted: () => void
}) {
  const [actionId, setActionId] = useState<string>('')
  const [answerText, setAnswerText] = useState('')
  const [saveAs, setSaveAs] = useState<SaveAsOverride>('codex_fact')
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState<string | null>(null)

  // Reset form state when a different prompt is selected.
  useEffect(() => {
    setActionId('')
    setAnswerText('')
    setSaveAs('codex_fact')
    setError(null)
  }, [prompt.id])

  const canSubmit =
    !submitting && (actionId !== '' || answerText.trim().length > 0)

  async function handleSubmit(e: FormEvent) {
    e.preventDefault()
    if (!canSubmit) return
    setSubmitting(true)
    setError(null)
    try {
      await answerPrompt(prompt.id, {
        action_chosen: actionId || undefined,
        answer_text: answerText.trim() || undefined,
        save_as_override: saveAs,
      })
      onSubmitted()
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <form onSubmit={handleSubmit} className="space-y-4 p-6 text-fg">
      <h3 className="text-lg font-semibold">{prompt.headline}</h3>
      <Markdown>
        {prompt.prompt_type === 'approval' && typeof prompt.context_payload?.prd_content === 'string'
          ? prompt.context_payload.prd_content
          : prompt.question}
      </Markdown>

      {prompt.actions.length > 0 && (
        <div className="flex flex-wrap gap-2">
          {prompt.actions.map((a) => (
            <button
              key={a.id}
              type="button"
              onClick={() => setActionId(a.id)}
              className={`border px-3 py-1.5 text-xs font-mono uppercase tracking-wide transition ${
                actionId === a.id
                  ? a.semantic === 'cancel'
                    ? 'border-red-400 bg-red-400 text-surface-900'
                    : 'border-lime-400 bg-lime-400 text-surface-900'
                  : 'border-rule text-fg hover:border-lime-400'
              }`}
            >
              {a.label}
            </button>
          ))}
        </div>
      )}

      {prompt.prompt_type !== 'approval' && (
        <>
          <label className="block">
            <span className="text-xs font-mono uppercase tracking-wider text-muted">
              Free text (optional)
            </span>
            <textarea
              rows={3}
              value={answerText}
              onChange={(e) => setAnswerText(e.target.value)}
              placeholder="Explain or elaborate…"
              className="mt-1 w-full resize-y rounded-sm border border-rule bg-surface-950 p-2 text-sm text-fg focus:border-lime-400 focus:outline-none"
            />
          </label>

          <fieldset>
            <legend className="text-xs font-mono uppercase tracking-wider text-muted">
              Save this answer as
            </legend>
            <div className="mt-1 flex gap-6 text-xs text-fg">
              <label className="flex items-center gap-1.5">
                <input
                  type="radio"
                  name="saveAs"
                  checked={saveAs === 'codex_fact'}
                  onChange={() => setSaveAs('codex_fact')}
                />
                codex fact
              </label>
              <label className="flex items-center gap-1.5">
                <input
                  type="radio"
                  name="saveAs"
                  checked={saveAs === 'ephemeral'}
                  onChange={() => setSaveAs('ephemeral')}
                />
                just this run
              </label>
            </div>
          </fieldset>
        </>
      )}

      {prompt.prompt_type === 'approval' && actionId === 'approve' && (
        <label className="block">
          <span className="text-xs font-mono uppercase tracking-wider text-muted">
            Guidance for the AI (optional)
          </span>
          <textarea
            rows={3}
            value={answerText}
            onChange={(e) => setAnswerText(e.target.value)}
            placeholder="Answer open questions, provide direction, or leave blank to let the AI decide…"
            className="mt-1 w-full resize-y rounded-sm border border-rule bg-surface-950 p-2 text-sm text-fg focus:border-lime-400 focus:outline-none"
          />
        </label>
      )}

      {prompt.prompt_type === 'approval' && actionId === 'reject' && (
        <label className="block">
          <span className="text-xs font-mono uppercase tracking-wider text-muted">
            Rejection reason
          </span>
          <textarea
            rows={2}
            value={answerText}
            onChange={(e) => setAnswerText(e.target.value)}
            placeholder="What needs to change…"
            className="mt-1 w-full resize-y rounded-sm border border-rule bg-surface-950 p-2 text-sm text-fg focus:border-red-400 focus:outline-none"
          />
        </label>
      )}

      {error && (
        <div className="rounded-sm border border-red-500/40 bg-red-500/10 p-2 text-xs text-red-200">
          {error}
        </div>
      )}

      <div className="flex items-center justify-between border-t border-rule pt-4">
        <a
          href={`/work-items/${prompt.work_item_id}`}
          className="text-xs text-muted hover:text-lime-400"
        >
          View work item →
        </a>
        <button
          type="submit"
          disabled={!canSubmit}
          className="bg-lime-400 px-4 py-2 font-mono text-xs uppercase tracking-wide text-surface-900 transition disabled:opacity-40"
        >
          {submitting ? 'Submitting…' : prompt.prompt_type === 'approval' ? (actionId === 'reject' ? 'Reject' : 'Approve') : 'Submit'}
        </button>
      </div>
    </form>
  )
}
