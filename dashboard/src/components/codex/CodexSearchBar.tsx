import { useCallback, useEffect, useRef, useState } from 'react'
import { useApi } from '@/hooks/useApi'

type Kind = 'module' | 'pattern' | 'anti_pattern' | 'section'

interface Hit {
  kind: Kind
  id: string
  name: string
  snippet: string
  source: string
  state: string
  name_hit: boolean
  source_commit_sha: string
}

const KIND_BADGE: Record<Kind, string> = {
  module: 'border-blue-500/20 bg-blue-500/10 text-blue-400',
  pattern: 'border-indigo-500/20 bg-indigo-500/10 text-indigo-400',
  anti_pattern: 'border-red-500/20 bg-red-500/10 text-red-400',
  section: 'border-cyan-500/20 bg-cyan-500/10 text-cyan-400',
}

export function CodexSearchBar() {
  const { get } = useApi()
  const [query, setQuery] = useState('')
  const [results, setResults] = useState<Hit[]>([])
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const debounceTimer = useRef<ReturnType<typeof setTimeout> | null>(null)

  const runSearch = useCallback(async (q: string) => {
    if (q.trim() === '') {
      setResults([])
      setError(null)
      return
    }
    setLoading(true)
    setError(null)
    try {
      const data = await get<Hit[]>(`/codex/search?q=${encodeURIComponent(q)}`)
      setResults(data)
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
      setResults([])
    } finally {
      setLoading(false)
    }
  }, [get])

  useEffect(() => {
    if (debounceTimer.current) clearTimeout(debounceTimer.current)
    debounceTimer.current = setTimeout(() => runSearch(query), 300)
    return () => {
      if (debounceTimer.current) clearTimeout(debounceTimer.current)
    }
  }, [query, runSearch])

  return (
    <div className="flex flex-col gap-3">
      <div className="relative">
        <input
          type="search"
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          placeholder="Search codex across modules, patterns, anti-patterns, sections…"
          className="w-full rounded-sm border border-rule bg-surface-800 px-3 py-2 pr-20 font-mono text-sm text-fg placeholder-dim focus:border-accent focus:outline-none"
        />
        {loading && (
          <span className="absolute right-3 top-1/2 -translate-y-1/2 font-mono text-xs text-muted">
            searching…
          </span>
        )}
        {!loading && query && (
          <button
            type="button"
            onClick={() => setQuery('')}
            className="absolute right-2 top-1/2 -translate-y-1/2 rounded-sm px-2 py-0.5 font-mono text-xs text-dim hover:bg-surface-700 hover:text-fg"
            aria-label="Clear"
          >
            ✕
          </button>
        )}
      </div>

      {error && (
        <div className="rounded-sm border border-red-500/30 bg-red-500/10 p-2 text-xs text-red-400">
          {error}
        </div>
      )}

      {query.trim() !== '' && !loading && results.length === 0 && !error && (
        <div className="rounded-sm border border-rule bg-surface-800 p-6 text-center font-mono text-sm text-muted">
          No matches for &ldquo;{query}&rdquo;.
        </div>
      )}

      {results.length > 0 && (
        <div className="divide-y divide-rule rounded-sm border border-rule bg-surface-800">
          {results.map((hit) => (
            <div key={`${hit.kind}:${hit.id}`} className="px-4 py-3">
              <div className="flex items-center gap-2 font-mono text-[0.6rem] uppercase tracking-widest">
                <span
                  className={`rounded-sm border px-1.5 py-0.5 ${KIND_BADGE[hit.kind]}`}
                >
                  {hit.kind.replace('_', ' ')}
                </span>
                {hit.source === 'user_interaction' && (
                  <span className="rounded-sm border border-accent/20 bg-accent/10 px-1.5 py-0.5 text-accent-dim">
                    user
                  </span>
                )}
                <span className="text-dim">{hit.state}</span>
                {hit.source_commit_sha && (
                  <span className="text-dim" title={hit.source_commit_sha}>
                    {hit.source_commit_sha.slice(0, 8)}
                  </span>
                )}
              </div>
              <div className="mt-1 text-sm font-medium text-fg">
                {hit.name}
              </div>
              {hit.snippet && (
                <div className="mt-1 line-clamp-2 text-xs text-muted">
                  {hit.snippet}
                </div>
              )}
            </div>
          ))}
        </div>
      )}
    </div>
  )
}
