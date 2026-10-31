import { useCallback, useEffect, useState } from 'react'
import { useApi } from '@/hooks/useApi'
import { ZONE_POLICIES, ZONE_POLICY_HELP } from '@/types/zones'
import type { CodeZone, ZonePolicy } from '@/types/zones'

// RepositoryZonesEditor is Admin → Products → Repository → Zones (codex
// zones, migration 063): list a repository's zones, edit their patterns and
// policy, freeze (with a reason) and thaw, add and delete. Every change is a
// codex version (section:zones) on the server, so work planned against the
// old zones can be re-planned.
//
//   GET    /admin/repositories/zones?id=   list
//   POST   /admin/repositories/zones?id=   create
//   PUT    /admin/zones?id=                edit patterns / policy / description
//   DELETE /admin/zones?id=                delete
//   POST   /admin/zones/freeze?id=         {reason}
//   POST   /admin/zones/thaw?id=

export function parsePatterns(text: string): string[] {
  const out: string[] = []
  for (const raw of text.split(/[\n,]/)) {
    const p = raw.trim()
    if (p && !out.includes(p)) out.push(p)
  }
  return out
}

function errorText(e: unknown, fallback: string): string {
  return e instanceof Error ? e.message : fallback
}

const policyClass: Record<ZonePolicy, string> = {
  autonomous: 'text-emerald-300',
  human_review: 'text-amber-300',
  escalate: 'text-red-300',
}

export interface RepositoryZonesEditorProps {
  repositoryId: string
}

export default function RepositoryZonesEditor({ repositoryId }: RepositoryZonesEditorProps) {
  const { get, post, put, del } = useApi()
  const [zones, setZones] = useState<CodeZone[] | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const [editing, setEditing] = useState<string | null>(null)
  const [editPatterns, setEditPatterns] = useState('')
  const [editPolicy, setEditPolicy] = useState<ZonePolicy>('human_review')
  const [editDescription, setEditDescription] = useState('')
  const [freezing, setFreezing] = useState<string | null>(null)
  const [freezeReason, setFreezeReason] = useState('')
  const [adding, setAdding] = useState(false)
  const [newName, setNewName] = useState('')
  const [newPatterns, setNewPatterns] = useState('')
  const [newPolicy, setNewPolicy] = useState<ZonePolicy>('human_review')
  const [newDescription, setNewDescription] = useState('')
  const id = encodeURIComponent(repositoryId)

  const load = useCallback(async () => {
    try {
      setZones(await get<CodeZone[]>(`/admin/repositories/zones?id=${id}`))
    } catch (e) {
      setError(errorText(e, 'Failed to load zones'))
    }
  }, [get, id])

  useEffect(() => { load() }, [load])

  const replace = (z: CodeZone) => setZones(zs => (zs ?? []).map(x => (x.id === z.id ? z : x)))

  async function run(action: () => Promise<void>) {
    setBusy(true)
    setError(null)
    try {
      await action()
    } catch (e) {
      setError(errorText(e, 'Zone change failed'))
    } finally {
      setBusy(false)
    }
  }

  function startEdit(z: CodeZone) {
    setEditing(z.id)
    setFreezing(null)
    setEditPatterns(z.patterns.join('\n'))
    setEditPolicy(z.policy)
    setEditDescription(z.description)
  }

  const saveEdit = (z: CodeZone) => run(async () => {
    const updated = await put<CodeZone>(`/admin/zones?id=${encodeURIComponent(z.id)}`, {
      patterns: parsePatterns(editPatterns),
      policy: editPolicy,
      description: editDescription,
    })
    replace(updated)
    setEditing(null)
  })

  const freeze = (z: CodeZone) => run(async () => {
    replace(await post<CodeZone>(`/admin/zones/freeze?id=${encodeURIComponent(z.id)}`, { reason: freezeReason.trim() }))
    setFreezing(null)
    setFreezeReason('')
  })

  const thaw = (z: CodeZone) => run(async () => {
    replace(await post<CodeZone>(`/admin/zones/thaw?id=${encodeURIComponent(z.id)}`))
  })

  const remove = (z: CodeZone) => run(async () => {
    await del<void>(`/admin/zones?id=${encodeURIComponent(z.id)}`)
    setZones(zs => (zs ?? []).filter(x => x.id !== z.id))
  })

  const create = () => run(async () => {
    const created = await post<CodeZone>(`/admin/repositories/zones?id=${id}`, {
      name: newName.trim(),
      patterns: parsePatterns(newPatterns),
      policy: newPolicy,
      description: newDescription.trim(),
    })
    setZones(zs => [...(zs ?? []), created].sort((a, b) => a.name.localeCompare(b.name)))
    setAdding(false)
    setNewName('')
    setNewPatterns('')
    setNewDescription('')
  })

  if (zones === null) {
    return error
      ? <p role="alert" className="text-xs text-red-400">{error}</p>
      : <p className="text-xs text-dim">Loading zones…</p>
  }

  return (
    <div className="space-y-3">
      <p className="text-xs text-dim">
        Globs match like security-review paths: a pattern without “/” matches any path segment; “**” matches any
        number of directories. When files fall in several zones the strictest policy applies; a frozen zone stops
        new work touching it.
      </p>
      {zones.length === 0 && <p className="text-xs text-dim">No zones yet: every change is autonomous.</p>}
      {zones.length > 0 && (
        <table className="w-full text-xs">
          <thead>
            <tr className="text-left text-dim">
              <th className="py-1 pr-2 font-medium">Zone</th>
              <th className="py-1 pr-2 font-medium">Policy</th>
              <th className="py-1 pr-2 font-medium">Patterns</th>
              <th className="py-1 pr-2 font-medium">Status</th>
              <th className="py-1 font-medium" />
            </tr>
          </thead>
          <tbody>
            {zones.map(z => (
              <tr key={z.id} data-testid={`zone-row-${z.name}`} className="border-t border-rule align-top">
                <td className="py-2 pr-2">
                  <span className="font-mono text-fg">{z.name}</span>
                  {z.description && <p className="mt-0.5 text-dim">{z.description}</p>}
                  <p className="mt-0.5 text-dim">source: {z.source}</p>
                </td>
                <td className={`py-2 pr-2 font-mono ${policyClass[z.policy]}`}>{z.policy}</td>
                <td className="py-2 pr-2 font-mono text-muted">
                  {editing === z.id ? (
                    <div className="space-y-2">
                      <textarea
                        aria-label={`Patterns for ${z.name}`}
                        value={editPatterns}
                        onChange={e => setEditPatterns(e.target.value)}
                        rows={Math.max(3, editPatterns.split('\n').length)}
                        className="w-full rounded-sm border border-rule bg-surface-900 p-1 font-mono text-xs text-fg"
                      />
                      <select
                        aria-label={`Policy for ${z.name}`}
                        value={editPolicy}
                        onChange={e => setEditPolicy(e.target.value as ZonePolicy)}
                        className="w-full rounded-sm border border-rule bg-surface-900 p-1 text-xs text-fg"
                      >
                        {ZONE_POLICIES.map(p => <option key={p} value={p}>{p}</option>)}
                      </select>
                      <p className="text-dim">{ZONE_POLICY_HELP[editPolicy]}</p>
                      <input
                        aria-label={`Description for ${z.name}`}
                        value={editDescription}
                        onChange={e => setEditDescription(e.target.value)}
                        className="w-full rounded-sm border border-rule bg-surface-900 p-1 text-xs text-fg"
                      />
                      <div className="flex gap-2">
                        <button disabled={busy || parsePatterns(editPatterns).length === 0} onClick={() => saveEdit(z)} className="rounded-sm bg-accent px-2 py-1 text-surface-900 disabled:opacity-50">Save zone</button>
                        <button onClick={() => setEditing(null)} className="rounded-sm border border-rule px-2 py-1 text-muted">Cancel</button>
                      </div>
                    </div>
                  ) : (
                    z.patterns.map(p => <div key={p}>{p}</div>)
                  )}
                </td>
                <td className="py-2 pr-2">
                  {z.status === 'frozen'
                    ? <span className="text-red-300">frozen: {z.frozen_reason || 'no reason given'}</span>
                    : <span className="text-muted">{z.status}</span>}
                  {freezing === z.id && (
                    <div className="mt-2 space-y-2">
                      <input
                        aria-label={`Freeze reason for ${z.name}`}
                        value={freezeReason}
                        onChange={e => setFreezeReason(e.target.value)}
                        placeholder="Why is this zone frozen?"
                        className="w-full rounded-sm border border-rule bg-surface-900 p-1 text-xs text-fg"
                      />
                      <div className="flex gap-2">
                        <button disabled={busy || !freezeReason.trim()} onClick={() => freeze(z)} className="rounded-sm border border-red-800 px-2 py-1 text-red-300 disabled:opacity-50">Confirm freeze</button>
                        <button onClick={() => setFreezing(null)} className="rounded-sm border border-rule px-2 py-1 text-muted">Cancel</button>
                      </div>
                    </div>
                  )}
                </td>
                <td className="py-2">
                  <div className="flex flex-wrap justify-end gap-1">
                    <button disabled={busy} onClick={() => startEdit(z)} className="rounded-sm border border-rule px-2 py-1 text-muted hover:bg-surface-700">Edit</button>
                    {z.status === 'frozen'
                      ? <button disabled={busy} onClick={() => thaw(z)} className="rounded-sm border border-rule px-2 py-1 text-muted hover:bg-surface-700">Thaw</button>
                      : <button disabled={busy} onClick={() => { setFreezing(z.id); setEditing(null); setFreezeReason('') }} className="rounded-sm border border-rule px-2 py-1 text-muted hover:bg-surface-700">Freeze</button>}
                    <button disabled={busy} onClick={() => remove(z)} className="rounded-sm border border-red-800 px-2 py-1 text-red-400 hover:bg-red-900/30">Delete</button>
                  </div>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {adding ? (
        <div className="space-y-2 rounded-sm border border-rule p-3">
          <input aria-label="New zone name" value={newName} onChange={e => setNewName(e.target.value)} placeholder="Zone name" className="w-full rounded-sm border border-rule bg-surface-900 p-1 text-xs text-fg" />
          <textarea aria-label="New zone patterns" value={newPatterns} onChange={e => setNewPatterns(e.target.value)} placeholder="One glob per line, e.g. payments/**" rows={3} className="w-full rounded-sm border border-rule bg-surface-900 p-1 font-mono text-xs text-fg" />
          <select aria-label="New zone policy" value={newPolicy} onChange={e => setNewPolicy(e.target.value as ZonePolicy)} className="w-full rounded-sm border border-rule bg-surface-900 p-1 text-xs text-fg">
            {ZONE_POLICIES.map(p => <option key={p} value={p}>{p}</option>)}
          </select>
          <p className="text-xs text-dim">{ZONE_POLICY_HELP[newPolicy]}</p>
          <input aria-label="New zone description" value={newDescription} onChange={e => setNewDescription(e.target.value)} placeholder="Description (optional)" className="w-full rounded-sm border border-rule bg-surface-900 p-1 text-xs text-fg" />
          <div className="flex gap-2">
            <button disabled={busy || !newName.trim() || parsePatterns(newPatterns).length === 0} onClick={create} className="rounded-sm bg-accent px-2 py-1 text-xs text-surface-900 disabled:opacity-50">Create zone</button>
            <button onClick={() => setAdding(false)} className="rounded-sm border border-rule px-2 py-1 text-xs text-muted">Cancel</button>
          </div>
        </div>
      ) : (
        <button onClick={() => setAdding(true)} className="rounded-sm border border-rule px-2 py-1 text-xs text-muted hover:bg-surface-700">Add zone</button>
      )}
      {error && <p role="alert" className="text-xs text-red-400">{error}</p>}
    </div>
  )
}
