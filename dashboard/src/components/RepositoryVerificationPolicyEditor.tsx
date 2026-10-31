import { useCallback, useEffect, useState } from 'react'
import { useApi } from '@/hooks/useApi'
import VerificationPolicyForm from '@/components/VerificationPolicyForm'
import type {
  RepositoryVerificationPolicyInput,
  RepositoryVerificationPolicyView,
} from '@/types/verificationPolicy'

// RepositoryVerificationPolicyEditor loads and saves one repository's
// verification policy through either surface:
//   - 'admin':      GET/PUT /api/admin/repositories/verification-policy?id=
//                   (source='admin', set_by = the signed-in user)
//   - 'onboarding': GET/POST /api/onboarding/verification-policy
//                   (source='onboarding')
// Both render the same VerificationPolicyForm.

export interface RepositoryVerificationPolicyEditorProps {
  repositoryId: string
  mode: 'admin' | 'onboarding'
  onSaved?: (view: RepositoryVerificationPolicyView) => void
  onLoaded?: (view: RepositoryVerificationPolicyView) => void
}

export function policyEndpoints(mode: 'admin' | 'onboarding', repositoryId: string) {
  const id = encodeURIComponent(repositoryId)
  return mode === 'admin'
    ? { load: `/admin/repositories/verification-policy?id=${id}`, save: `/admin/repositories/verification-policy?id=${id}` }
    : { load: `/onboarding/verification-policy?repository_id=${id}`, save: '/onboarding/verification-policy' }
}

export default function RepositoryVerificationPolicyEditor({ repositoryId, mode, onSaved, onLoaded }: RepositoryVerificationPolicyEditorProps) {
  const { get, put, post } = useApi()
  const [view, setView] = useState<RepositoryVerificationPolicyView | null>(null)
  const [loadError, setLoadError] = useState<string | null>(null)
  const [saveError, setSaveError] = useState<string | null>(null)
  const [saving, setSaving] = useState(false)
  const [savedNote, setSavedNote] = useState('')
  const endpoints = policyEndpoints(mode, repositoryId)

  useEffect(() => {
    let cancelled = false
    setLoadError(null)
    get<RepositoryVerificationPolicyView>(endpoints.load)
      .then(v => {
        if (cancelled) return
        setView(v)
        onLoaded?.(v)
      })
      .catch(e => { if (!cancelled) setLoadError(e instanceof Error ? e.message : 'Failed to load verification policy') })
    return () => { cancelled = true }
  }, [get, endpoints.load]) // eslint-disable-line react-hooks/exhaustive-deps

  const save = useCallback(async (input: RepositoryVerificationPolicyInput) => {
    setSaving(true)
    setSaveError(null)
    setSavedNote('')
    try {
      const v = mode === 'admin'
        ? await put<RepositoryVerificationPolicyView>(endpoints.save, input)
        : await post<RepositoryVerificationPolicyView>(endpoints.save, { repository_id: repositoryId, ...input })
      setView(v)
      setSavedNote('Saved.')
      onSaved?.(v)
    } catch (e) {
      setSaveError(e instanceof Error ? e.message : 'Failed to save verification policy')
    } finally {
      setSaving(false)
    }
  }, [mode, put, post, endpoints.save, repositoryId, onSaved])

  if (loadError) {
    return <p role="alert" className="text-xs text-red-400">{loadError}</p>
  }
  if (!view) {
    return <p className="text-xs text-dim">Loading verification policy…</p>
  }
  return (
    <div>
      <VerificationPolicyForm view={view} onSave={save} saving={saving} error={saveError} idPrefix={`vp-${repositoryId}`} />
      {savedNote && <p className="mt-2 text-xs text-emerald-400">{savedNote}</p>}
    </div>
  )
}
