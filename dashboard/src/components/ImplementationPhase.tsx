import { Fragment } from 'react'
import type { ImplementationPhase, ImplementationRun, PhaseEvidence } from '@/types/controller'
import RunRepair from './RunRepair'

// ─── Implementation phase ─────────────────────────────────────────────────────
//
// Badges and the per-subtask run table for the implementation controller
// (M3-02). Every label is a state the agent recorded in implementation_runs
// or on a console event; nothing here derives a phase from other data, and a
// subtask with no recorded run shows no phase.

const PHASE_BADGE: Record<ImplementationPhase, string> = {
  inspect:        'bg-surface-700/40 text-muted border border-sharp/40',
  test_authored:  'bg-blue-500/20 text-blue-300 border border-blue-500/30',
  red_observed:   'bg-red-500/20 text-red-300 border border-red-500/30',
  implement:      'bg-cyan-500/20 text-cyan-300 border border-cyan-500/30',
  green_observed: 'bg-emerald-500/20 text-emerald-300 border border-emerald-500/30',
  diff_reviewed:  'bg-teal-500/20 text-teal-300 border border-teal-500/30',
  final_verified: 'bg-emerald-600/30 text-emerald-200 border border-emerald-400/50',
  blocked:        'bg-red-600/20 text-red-400 border border-red-600/30',
  failed:         'bg-red-500/20 text-red-300 border border-red-500/30',
}

const NEUTRAL = 'bg-surface-700/40 text-muted border border-sharp/40'

export function phaseBadgeClass(phase: string): string {
  return PHASE_BADGE[phase as ImplementationPhase] ?? NEUTRAL
}

export function phaseLabel(phase: string): string {
  return phase.replace(/_/g, ' ')
}

export function PhaseBadge({ phase, title }: { phase: string; title?: string }) {
  return (
    <span
      data-testid="phase-badge"
      title={title}
      className={`rounded px-1.5 py-0.5 text-xs font-medium whitespace-nowrap ${phaseBadgeClass(phase)}`}
    >
      {phaseLabel(phase)}
    </span>
  )
}

function shortCall(id: string): string {
  return id.length > 14 ? `${id.slice(0, 14)}…` : id
}

export function evidenceText(ev: PhaseEvidence | null): string {
  if (!ev) return 'not observed'
  const exit = ev.exit_code === null ? 'no exit code' : `exit ${ev.exit_code}`
  return `${ev.command || ev.tool} · ${exit} · call ${shortCall(ev.tool_call_id)}`
}

// runTitle is the hover text of a run's badge: attempt and evidence.
export function runTitle(run: ImplementationRun): string {
  return `attempt ${run.attempt} · red: ${evidenceText(run.red)} · green: ${evidenceText(run.green)}`
}

// runsByTask indexes runs by task id.
export function runsByTask(runs: ImplementationRun[] | undefined): Record<string, ImplementationRun> {
  const out: Record<string, ImplementationRun> = {}
  for (const r of runs ?? []) out[r.task_id] = r
  return out
}

export function SubtaskPhase({ run }: { run: ImplementationRun | undefined }) {
  if (!run) return <span className="text-dim" title="no implementation run recorded">—</span>
  return <PhaseBadge phase={run.state} title={runTitle(run)} />
}

// ImplementationRuns lists each task's latest run with the tool calls that
// proved red and green.
export default function ImplementationRuns({ runs }: { runs: ImplementationRun[] }) {
  if (runs.length === 0) {
    return <p className="text-xs text-dim italic">No implementation run recorded.</p>
  }
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-xs">
        <thead>
          <tr className="border-b border-rule text-dim text-left">
            <th className="pb-2 pr-4 font-medium">Task</th>
            <th className="pb-2 pr-4 font-medium">Attempt</th>
            <th className="pb-2 pr-4 font-medium">Phase</th>
            <th className="pb-2 pr-4 font-medium">Red evidence</th>
            <th className="pb-2 font-medium">Green evidence</th>
          </tr>
        </thead>
        <tbody>
          {runs.map((run) => (
            <Fragment key={run.id}>
              <tr className="border-b border-rule/30 last:border-0" data-testid="implementation-run">
                <td className="py-2 pr-4 font-mono text-muted">{run.task_id.slice(0, 8)}</td>
                <td className="py-2 pr-4 text-muted">{run.attempt}</td>
                <td className="py-2 pr-4"><PhaseBadge phase={run.state} title={runTitle(run)} /></td>
                <td className="py-2 pr-4 font-mono text-muted">{evidenceText(run.red)}</td>
                <td className="py-2 font-mono text-muted">{evidenceText(run.green)}</td>
              </tr>
              {(run.repair || run.budget) && (
                <tr className="border-b border-rule/30" data-testid="implementation-run-repair">
                  <td colSpan={5} className="pb-3 pl-4">
                    <RunRepair run={run} />
                  </td>
                </tr>
              )}
            </Fragment>
          ))}
        </tbody>
      </table>
    </div>
  )
}
