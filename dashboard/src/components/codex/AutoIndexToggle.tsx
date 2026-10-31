import { useCallback, useState } from 'react'
import { useApi } from '@/hooks/useApi'

interface Props {
  productId: string
  initialValue?: boolean
}

export default function AutoIndexToggle({ productId, initialValue = false }: Props) {
  const { post } = useApi()
  const [enabled, setEnabled] = useState(initialValue)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const handleToggle = useCallback(async () => {
    const newValue = !enabled
    setSaving(true)
    setError(null)
    try {
      await post('/admin/products/update', {
        id: productId,
        trusted_autoindex: newValue,
      })
      setEnabled(newValue)
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : 'Failed to update setting')
    } finally {
      setSaving(false)
    }
  }, [productId, enabled, post])

  return (
    <div className="inline-flex items-center gap-3">
      <label className="flex cursor-pointer items-center gap-2">
        <button
          type="button"
          role="switch"
          aria-checked={enabled}
          disabled={saving}
          onClick={handleToggle}
          className={[
            'relative inline-flex h-6 w-11 shrink-0 rounded-full border-2 border-transparent transition-colors',
            enabled ? 'bg-accent' : 'bg-surface-700',
            saving ? 'opacity-50' : '',
          ].join(' ')}
        >
          <span
            className={[
              'pointer-events-none inline-block h-5 w-5 rounded-full bg-surface-900 shadow transition-transform',
              enabled ? 'translate-x-5' : 'translate-x-0',
            ].join(' ')}
          />
        </button>
        <span className="font-mono text-sm text-muted">
          Trusted Auto-Index
        </span>
      </label>
      {saving && <span className="font-mono text-xs text-dim">Saving...</span>}
      {error && <span className="font-mono text-xs text-red-400">{error}</span>}
    </div>
  )
}
