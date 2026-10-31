import { useEffect, useState } from 'react'
import { useApi } from '@/hooks/useApi'
import { ZONE_POLICIES, ZONE_POLICY_HELP } from '@/types/zones'
import type { RepoZoneProposal, ZonePolicy, ZoneTemplate } from '@/types/zones'

// ─── Step: Codex zones ────────────────────────────────────────────────────────
//
// Shows, per repository, the zones onboarding proposes (codex zones,
// migration 063): the codex-config.yaml defaults (guarded → human_review:
// migrations, CI, containers, deploy/infra, auth/security; restricted →
// escalate: env files, secrets, keys — *.example excluded; everything else
// stays autonomous) and a guarded zone for
// every high-risk area named in the risk_zones answer. The operator keeps or
// drops each zone and may change its policy (e.g. raise a risk zone to
// escalate), then confirms: POST /onboarding/zones sets the repository's
// zones to the confirmed list once (an empty list is a recorded "no zones"
// choice). The onboarding service is unauthenticated, so it never changes
// existing zones: a repository whose zones are already set is shown
// read-only and counts as confirmed; zones are edited, weakened, frozen or
// deleted in Admin → Products → Repository → Zones.

interface ZonesStepProps {
  productId: string
  onNext: () => void
}

interface DraftZone extends ZoneTemplate {
  include: boolean
}

export default function ZonesStep({ productId, onNext }: ZonesStepProps) {
  const { get, post } = useApi()
  const [proposals, setProposals] = useState<RepoZoneProposal[] | null>(null)
  const [drafts, setDrafts] = useState<Record<string, DraftZone[]>>({})
  const [confirmed, setConfirmed] = useState<Record<string, boolean>>({})
  const [errors, setErrors] = useState<Record<string, string>>({})
  const [loadError, setLoadError] = useState<string | null>(null)
  const [saving, setSaving] = useState<string | null>(null)

  useEffect(() => {
    let cancelled = false
    get<RepoZoneProposal[]>(`/onboarding/zones/proposed?product_id=${encodeURIComponent(productId)}`)
      .then(ps => {
        if (cancelled) return
        setProposals(ps)
        const d: Record<string, DraftZone[]> = {}
        const c: Record<string, boolean> = {}
        for (const p of ps) {
          d[p.repository_id] = p.zones.map(z => ({ ...z, include: true }))
          c[p.repository_id] = p.existing
        }
        setDrafts(d)
        setConfirmed(c)
      })
      .catch(e => { if (!cancelled) setLoadError(e instanceof Error ? e.message : 'Failed to load proposed zones') })
    return () => { cancelled = true }
  }, [get, productId])

  function update(repoId: string, name: string, change: Partial<DraftZone>) {
    setDrafts(d => ({ ...d, [repoId]: (d[repoId] ?? []).map(z => (z.name === name ? { ...z, ...change } : z)) }))
    setConfirmed(c => ({ ...c, [repoId]: false }))
  }

  async function confirm(p: RepoZoneProposal) {
    setSaving(p.repository_id)
    setErrors(e => ({ ...e, [p.repository_id]: '' }))
    try {
      const zones: ZoneTemplate[] = (drafts[p.repository_id] ?? [])
        .filter(z => z.include)
        .map(z => ({ name: z.name, description: z.description, patterns: z.patterns, policy: z.policy, source: z.source }))
      await post('/onboarding/zones', { repository_id: p.repository_id, zones })
      setConfirmed(c => ({ ...c, [p.repository_id]: true }))
    } catch (e) {
      setErrors(er => ({ ...er, [p.repository_id]: e instanceof Error ? e.message : 'Failed to save zones' }))
    } finally {
      setSaving(null)
    }
  }

  const allConfirmed = proposals !== null && proposals.every(p => confirmed[p.repository_id])

  return (
    <div className="space-y-6">
      <div>
        <h2 className="text-base font-semibold text-fg">Codex Zones</h2>
        <p className="mt-1 text-sm text-muted">
          Zones decide where agents may change code on their own, where a human must approve the PR, and where
          work stops for a human decision. Review the proposed zones for each repository and confirm them.
        </p>
        <ul className="mt-2 space-y-0.5 text-xs text-dim">
          {ZONE_POLICIES.map(p => <li key={p}><span className="font-mono">{p}</span>: {ZONE_POLICY_HELP[p]}</li>)}
        </ul>
      </div>
      {loadError && <p role="alert" className="text-sm text-red-400">{loadError}</p>}
      {proposals === null && !loadError && <p className="text-sm text-dim">Loading proposed zones…</p>}
      {proposals?.length === 0 && <p className="text-sm text-dim">No repositories to configure.</p>}
      {proposals?.map(p => (
        <div key={p.repository_id} className="rounded-sm border border-rule bg-surface-900 p-4">
          <div className="mb-3 flex items-center justify-between">
            <h3 className="text-sm font-semibold text-fg">{p.repository_name}</h3>
            {confirmed[p.repository_id]
              ? <span className="text-xs text-emerald-400">✓ zones confirmed</span>
              : <span className="text-xs text-amber-300">not confirmed</span>}
          </div>
          {p.existing ? (
            <>
              <p className="mb-2 text-xs text-dim">
                This repository already has zones{p.frozen?.length ? ` (frozen: ${p.frozen.join(', ')})` : ''}{p.zones.length === 0 ? ' set (none)' : ''}.
                Onboarding cannot change them; edit, weaken, freeze or delete zones in Admin → Products → Repository → Zones.
              </p>
              <ul className="space-y-1">
                {p.zones.map(z => (
                  <li key={z.name} data-testid={`existing-zone-${z.name}`} className="font-mono text-xs text-muted">
                    {z.name}: {z.policy} — {z.patterns.join(', ')}
                  </li>
                ))}
              </ul>
            </>
          ) : (
            <>
              <div className="space-y-2">
                {(drafts[p.repository_id] ?? []).map(z => (
                  <div key={z.name} data-testid={`proposed-zone-${z.name}`} className={`rounded-sm border border-rule p-2 ${z.include ? '' : 'opacity-50'}`}>
                    <div className="flex flex-wrap items-center gap-3">
                      <label className="flex items-center gap-2 text-sm text-fg">
                        <input
                          type="checkbox"
                          aria-label={`Include ${z.name}`}
                          checked={z.include}
                          onChange={e => update(p.repository_id, z.name, { include: e.target.checked })}
                        />
                        <span className="font-mono">{z.name}</span>
                      </label>
                      <select
                        aria-label={`Policy for ${z.name}`}
                        value={z.policy}
                        onChange={e => update(p.repository_id, z.name, { policy: e.target.value as ZonePolicy })}
                        className="rounded-sm border border-rule bg-surface-950 p-1 text-xs text-fg"
                      >
                        {ZONE_POLICIES.map(pol => <option key={pol} value={pol}>{pol}</option>)}
                      </select>
                      <span className="text-xs text-dim">from {z.source === 'config' ? 'codex-config defaults' : 'risk_zones answer'}</span>
                    </div>
                    {z.description && <p className="mt-1 text-xs text-dim">{z.description}</p>}
                    <p className="mt-1 font-mono text-xs text-muted">{z.patterns.join(', ')}</p>
                  </div>
                ))}
              </div>
              <p className="mt-2 text-xs text-dim">
                Confirming sets this repository's zones once (unticking everything records "no zones"); later changes are made in
                the admin dashboard.
              </p>
              {errors[p.repository_id] && <p role="alert" className="mt-2 text-xs text-red-400">{errors[p.repository_id]}</p>}
              {!confirmed[p.repository_id] && (
                <button
                  onClick={() => confirm(p)}
                  disabled={saving === p.repository_id}
                  className="mt-3 rounded-sm border border-accent px-3 py-1 text-xs text-accent hover:bg-surface-700 disabled:opacity-50"
                >
                  {`Confirm zones for ${p.repository_name}`}
                </button>
              )}
            </>
          )}
        </div>
      ))}
      <div className="flex justify-end">
        <button
          onClick={onNext}
          disabled={!allConfirmed}
          className="rounded bg-accent px-4 py-2 text-sm font-medium text-surface-900 hover:bg-accent-hover disabled:opacity-50"
        >
          Continue
        </button>
      </div>
    </div>
  )
}
