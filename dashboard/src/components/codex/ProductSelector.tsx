import { useCallback, useEffect, useState } from 'react'
import { useApi } from '@/hooks/useApi'
import type { Product } from '@/types'

const STORAGE_KEY = 'codex.selectedProductId'

export function useProductSelection(): [string | null, (id: string | null) => void] {
  const [id, setId] = useState<string | null>(() => {
    if (typeof window === 'undefined') return null
    return window.localStorage.getItem(STORAGE_KEY)
  })
  const set = useCallback((next: string | null) => {
    setId(next)
    if (typeof window !== 'undefined') {
      if (next) window.localStorage.setItem(STORAGE_KEY, next)
      else window.localStorage.removeItem(STORAGE_KEY)
    }
  }, [])
  return [id, set]
}

interface ProductSelectorProps {
  value: string | null
  onChange: (id: string | null) => void
}

export function ProductSelector({ value, onChange }: ProductSelectorProps) {
  const { get } = useApi()
  const [products, setProducts] = useState<Product[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    let alive = true
    setLoading(true)
    get<Product[]>('/admin/products')
      .then((list) => {
        if (!alive) return
        setProducts(list ?? [])
        setError(null)
        // Auto-select first product if none remembered and exactly one exists.
        if (!value && list && list.length === 1) onChange(list[0].id)
      })
      .catch((err: unknown) => {
        if (!alive) return
        setError(err instanceof Error ? err.message : 'Failed to load products')
      })
      .finally(() => alive && setLoading(false))
    return () => {
      alive = false
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  if (error) {
    return (
      <span className="rounded bg-red-500/20 px-2 py-1 text-xs text-red-300">
        {error}
      </span>
    )
  }

  return (
    <label className="flex items-center gap-2 text-sm text-muted">
      <span className="text-dim">Product:</span>
      <select
        value={value ?? ''}
        onChange={(e) => onChange(e.target.value || null)}
        disabled={loading}
        className="rounded-sm border border-rule bg-surface-800 px-2.5 py-1.5 text-sm text-fg focus:border-accent focus:outline-none focus:ring-1 focus:ring-accent/30 disabled:opacity-50"
      >
        <option value="">{loading ? 'Loading…' : '— Select a product —'}</option>
        {products.map((p) => (
          <option key={p.id} value={p.id}>
            {p.name}
          </option>
        ))}
      </select>
    </label>
  )
}
