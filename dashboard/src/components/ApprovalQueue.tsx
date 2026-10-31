// dashboard/src/components/ApprovalQueue.tsx
//
// Unified prompt list — replaces the old Code Review + PRD Approvals two-tab
// layout. Every prompt type (approval / clarification / escalation /
// block_question) shows here, filterable. Clicking a row opens a
// right-pane detail view (reuses PromptDetailPane from PromptSheet).
//
// Legacy endpoints /api/work-items/approve-prd + /api/work-items/reject-prd
// continue to work via the Task 24 migration — product-agent publishes a
// prompt alongside the prd_approval state transition, and the approve/reject
// handlers mark the prompt answered. Users reaching this page through either
// the bell or the sidebar see the same unified view.

import { useMemo, useState } from 'react'
import type { Prompt, PromptType } from '@/types/prompts'
import { usePromptList } from '@/hooks/usePrompts'
import { PromptDetailPane } from './PromptSheet'

const TYPE_LABELS: Record<PromptType, string> = {
  approval: 'APPROVAL',
  clarification: 'CLARIFY',
  escalation: 'ESCALATE',
  block_question: 'BLOCK-Q',
}

const TYPE_COLORS: Record<PromptType, string> = {
  approval: 'bg-violet-500/20 text-violet-300',
  clarification: 'bg-sky-500/20 text-sky-300',
  escalation: 'bg-amber-500/20 text-amber-300',
  block_question: 'bg-red-500/20 text-red-300',
}

type TypeFilter = 'all' | PromptType

function timeAgo(iso: string): string {
  const ms = Date.now() - new Date(iso).getTime()
  const mins = Math.floor(ms / 60_000)
  if (mins < 1) return 'now'
  if (mins < 60) return `${mins}m`
  const hrs = Math.floor(mins / 60)
  if (hrs < 24) return `${hrs}h`
  return `${Math.floor(hrs / 24)}d`
}

export default function ApprovalQueue() {
  const { prompts, refresh, loading } = usePromptList({ state: 'pending' })
  const [typeFilter, setTypeFilter] = useState<TypeFilter>('all')
  const [selectedId, setSelectedId] = useState<string | null>(null)

  const filtered = useMemo(
    () =>
      typeFilter === 'all'
        ? prompts
        : prompts.filter((p) => p.prompt_type === typeFilter),
    [prompts, typeFilter],
  )

  const selected = filtered.find((p) => p.id === selectedId) ?? null

  return (
    <div className="mx-auto max-w-7xl">
      <header className="mb-6">
        <h1 className="text-2xl font-semibold text-fg">Approvals</h1>
        <p className="mt-1 text-sm text-muted">
          Every prompt the system needs a human on — PRDs, clarifications,
          escalations, and blocked work — in one place.
        </p>
      </header>

      <div className="mb-5 flex flex-wrap gap-2">
        {(
          [
            'all',
            'approval',
            'clarification',
            'escalation',
            'block_question',
          ] as TypeFilter[]
        ).map((t) => (
          <button
            key={t}
            type="button"
            onClick={() => setTypeFilter(t)}
            className={`border px-3 py-1 text-xs font-mono uppercase tracking-wider transition ${
              typeFilter === t
                ? 'border-lime-400 bg-lime-400 text-surface-900'
                : 'border-rule text-muted hover:border-sharp'
            }`}
          >
            {t === 'all' ? 'all' : t.replace('_', ' ')}
          </button>
        ))}
      </div>

      <div className="grid grid-cols-12 gap-6">
        <div className="col-span-12 rounded-sm border border-rule md:col-span-5">
          {loading && prompts.length === 0 ? (
            <div className="p-8 text-center text-sm text-dim">
              Loading…
            </div>
          ) : filtered.length === 0 ? (
            <div className="p-12 text-center text-sm text-dim">
              {prompts.length === 0
                ? "You're all caught up."
                : 'No prompts match this filter.'}
            </div>
          ) : (
            filtered.map((p: Prompt) => (
              <button
                key={p.id}
                type="button"
                onClick={() => setSelectedId(p.id)}
                className={`block w-full border-b border-l-2 border-rule/40 px-4 py-3 text-left transition last:border-b-0 ${
                  p.id === selectedId
                    ? 'border-l-lime-400 bg-surface-800/40'
                    : 'border-l-transparent hover:bg-surface-800/20'
                }`}
              >
                <div className="flex items-center justify-between gap-2 font-mono text-[0.6rem] tracking-widest">
                  <span
                    className={`rounded px-1.5 py-0.5 ${TYPE_COLORS[p.prompt_type]}`}
                  >
                    {TYPE_LABELS[p.prompt_type]}
                  </span>
                  <span className="text-dim">
                    {timeAgo(p.requested_at)}
                  </span>
                </div>
                <div className="mt-2 text-sm text-fg">{p.headline}</div>
                <div className="mt-1 text-xs text-dim">
                  <span className="font-mono">{p.asking_agent}</span> ·{' '}
                  <span className="font-mono">{p.work_item_id.slice(0, 8)}</span>
                </div>
              </button>
            ))
          )}
        </div>

        <div className="col-span-12 rounded-sm border border-rule md:col-span-7">
          {selected ? (
            <PromptDetailPane
              prompt={selected}
              onSubmitted={() => {
                setSelectedId(null)
                refresh()
              }}
            />
          ) : (
            <div className="flex h-full min-h-[300px] items-center justify-center p-12 text-sm text-dim">
              Select a prompt to view and respond.
            </div>
          )}
        </div>
      </div>
    </div>
  )
}
