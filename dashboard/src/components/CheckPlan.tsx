import type { CheckPlan, CheckTarget, TaskCheckPlan } from '@/types/checks'

// ─── Impact-selected fast checks (M3-04) ─────────────────────────────────────
//
// Shows, per subtask, the fast checks the implementation agent selected for
// its change and why each one ran, next to the full suite the final gate
// runs regardless. Everything comes from the agent's check_plan artifacts;
// nothing is derived here.

interface ArtifactLike {
  artifact_type: string
  content: unknown
  created_at: string
}

function asObject(content: unknown): Record<string, unknown> | null {
  if (typeof content === 'string') {
    try {
      const parsed = JSON.parse(content)
      return parsed && typeof parsed === 'object' ? (parsed as Record<string, unknown>) : null
    } catch {
      return null
    }
  }
  return content && typeof content === 'object' ? (content as Record<string, unknown>) : null
}

// isCheckPlanArtifact reports an artifact whose content is a check plan
// (type "check_plan"), so the generic artifact list can leave it out.
export function isCheckPlanArtifact(content: unknown): boolean {
  return asObject(content)?.type === 'check_plan'
}

// checkPlansByTask returns each task's latest plan from the artifacts.
export function checkPlansByTask(artifacts: ArtifactLike[] | undefined): Record<string, TaskCheckPlan> {
  const out: Record<string, TaskCheckPlan> = {}
  for (const a of artifacts ?? []) {
    const c = asObject(a.content)
    if (!c || c.type !== 'check_plan' || typeof c.task_id !== 'string') continue
    const plan = c.plan as CheckPlan | undefined
    if (!plan || !Array.isArray(plan.targets)) continue
    const computedAt = typeof c.computed_at === 'string' ? c.computed_at : a.created_at
    const prev = out[c.task_id]
    if (!prev || Date.parse(computedAt) >= Date.parse(prev.computed_at)) {
      out[c.task_id] = { task_id: c.task_id, computed_at: computedAt, plan }
    }
  }
  return out
}

export function targetText(t: CheckTarget): string {
  if (!t.command) return `not run: ${t.not_run_reason ?? 'no command'}`
  return t.skipped ? `${t.command} — skipped: ${t.skipped}` : t.command
}

function fullSuiteText(plan: CheckPlan): string {
  const test = (plan.full ?? []).find((t) => t.kind === 'test')
  return test ? targetText(test) : 'repository verification policy'
}

export default function CheckPlans({
  plans,
  titles,
}: {
  plans: Record<string, TaskCheckPlan>
  titles?: Record<string, string>
}) {
  const entries = Object.values(plans).sort((a, b) => a.computed_at.localeCompare(b.computed_at))
  if (entries.length === 0) {
    return <p className="text-xs text-dim italic">No check plan recorded.</p>
  }
  return (
    <div className="space-y-4">
      {entries.map(({ task_id, plan }) => (
        <div key={task_id} data-testid="check-plan">
          <p className="text-xs text-muted mb-1">
            <span className="text-fg">{titles?.[task_id] ?? task_id.slice(0, 8)}</span>
            {' · '}gate runs <span className="font-mono">{fullSuiteText(plan)}</span>
            {plan.fallback && <span className="text-amber-300"> · fell back to the full suite: {plan.fallback}</span>}
          </p>
          <table className="w-full text-xs">
            <thead>
              <tr className="border-b border-rule text-dim text-left">
                <th className="pb-1 pr-4 font-medium">Kind</th>
                <th className="pb-1 pr-4 font-medium">Fast check</th>
                <th className="pb-1 font-medium">Why</th>
              </tr>
            </thead>
            <tbody>
              {plan.targets.map((t, i) => (
                <tr key={`${t.kind}-${i}`} className="border-b border-rule/30 last:border-0 align-top" data-testid="check-target">
                  <td className="py-1 pr-4 text-muted">
                    {t.kind}
                    {t.advisory && <span className="text-dim" title="reported to the agent; never fails the fast loop"> (advisory)</span>}
                  </td>
                  <td className={`py-1 pr-4 font-mono ${t.command ? 'text-muted' : 'text-dim italic'}`}>{targetText(t)}</td>
                  <td className="py-1 text-muted">{t.reason}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ))}
    </div>
  )
}
