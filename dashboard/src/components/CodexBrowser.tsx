import { useEffect, useState, useCallback, useMemo } from 'react'
import { useApi } from '@/hooks/useApi'
import { RevokeButton } from './codex/RevokeButton'
import { CodexSearchBar } from './codex/CodexSearchBar'
import TopologyPanel from './codex/TopologyPanel'
import EdgeInspector from './codex/EdgeInspector'
import type { CodexModule, CodexPattern, CodexSection, CodexVersion } from '@/types'

type Tab = 'modules' | 'patterns' | 'sections'

// ── Risk badge ──────────────────────────────────────────────────────────────

function RiskBadge({ level }: { level: string }) {
  const colors: Record<string, string> = {
    high: 'bg-red-500/15 text-red-400 border-red-500/20',
    medium: 'bg-amber-500/15 text-amber-400 border-amber-500/20',
    low: 'bg-emerald-500/15 text-emerald-400 border-emerald-500/20',
  }
  const cls = colors[level?.toLowerCase()] ?? 'bg-surface-700/40 text-muted border-rule'
  return (
    <span className={`inline-flex items-center rounded-sm border px-2 py-0.5 font-mono text-xs ${cls}`}>
      {level ?? 'unknown'}
    </span>
  )
}

// ── Section-type badge ───────────────────────────────────────────────────────

function SectionTypeBadge({ sectionType }: { sectionType: string }) {
  const colors: Record<string, string> = {
    architecture: 'bg-violet-500/15 text-violet-400 border-violet-500/20',
    module: 'bg-blue-500/15 text-blue-400 border-blue-500/20',
    pattern: 'bg-indigo-500/15 text-indigo-400 border-indigo-500/20',
    anti_pattern: 'bg-red-500/15 text-red-400 border-red-500/20',
    testing: 'bg-emerald-500/15 text-emerald-400 border-emerald-500/20',
    style: 'bg-pink-500/15 text-pink-400 border-pink-500/20',
    process: 'bg-amber-500/15 text-amber-400 border-amber-500/20',
    domain: 'bg-cyan-500/15 text-cyan-400 border-cyan-500/20',
  }
  const cls = colors[sectionType?.toLowerCase()] ?? 'bg-surface-700/40 text-muted border-rule'
  return (
    <span className={`inline-flex items-center rounded-sm border px-2 py-0.5 font-mono text-xs ${cls}`}>
      {sectionType}
    </span>
  )
}

// ── Module card ──────────────────────────────────────────────────────────────

function ModuleCard({ module: m, onRevoked, productId }: { module: CodexModule; onRevoked: () => void; productId: string }) {
  const [expanded, setExpanded] = useState(false)

  return (
    <div className="rounded-sm border border-rule bg-surface-800 transition-colors hover:border-sharp">
      <button
        type="button"
        className="w-full px-5 py-4 text-left"
        onClick={() => setExpanded((v) => !v)}
      >
        <div className="flex items-start justify-between gap-2">
          <div className="flex items-center gap-2 flex-wrap flex-1 min-w-0">
            <span className="text-sm font-medium text-fg">{m.name}</span>
            <RiskBadge level={m.risk_level} />
          </div>
          <div className="flex items-center gap-2 shrink-0">
            <RevokeButton
              id={m.id}
              table="codex_modules"
              name={m.name}
              onRevoked={onRevoked}
            />
            <span className="font-mono text-xs text-dim">
              {expanded ? '▲' : '▼'}
            </span>
          </div>
        </div>
        <p className="mt-1 text-xs text-muted line-clamp-2">{m.purpose}</p>
      </button>

      {expanded && (
        <div className="border-t border-rule px-5 py-4 space-y-3">
          {m.dependencies?.length > 0 && (
            <div>
              <p className="section-label mb-2">Dependencies</p>
              <div className="flex flex-wrap gap-1.5">
                {m.dependencies.map((dep, i) => (
                  <span
                    key={i}
                    className="rounded-sm border border-rule bg-surface-900 px-2 py-0.5 font-mono text-xs text-muted"
                  >
                    {dep}
                  </span>
                ))}
              </div>
            </div>
          )}

          {m.dependents?.length > 0 && (
            <div>
              <p className="section-label mb-2">Dependents</p>
              <div className="flex flex-wrap gap-1.5">
                {m.dependents.map((dep, i) => (
                  <span
                    key={i}
                    className="rounded-sm border border-accent/20 bg-accent/10 px-2 py-0.5 font-mono text-xs text-accent-dim"
                  >
                    {dep}
                  </span>
                ))}
              </div>
            </div>
          )}

          {m.test_coverage_notes && (
            <div>
              <p className="section-label mb-2">Test Coverage</p>
              <p className="text-xs text-muted leading-relaxed">
                {m.test_coverage_notes}
              </p>
            </div>
          )}

          <EdgeInspector entryId={m.id} entryType="module" productId={productId} />
        </div>
      )}
    </div>
  )
}

// ── Pattern card ─────────────────────────────────────────────────────────────

function PatternCard({ pattern, onRevoked, productId }: { pattern: CodexPattern; onRevoked: () => void; productId: string }) {
  const [expanded, setExpanded] = useState(false)

  return (
    <div className="rounded-sm border border-rule bg-surface-800 transition-colors hover:border-sharp">
      <button
        type="button"
        className="w-full px-5 py-4 text-left"
        onClick={() => setExpanded((v) => !v)}
      >
        <div className="flex items-start justify-between gap-2">
          <div>
            <span className="text-sm font-medium text-fg">
              {pattern.name}
            </span>
            <span className="ml-2 rounded-sm border border-rule bg-surface-900 px-1.5 py-0.5 font-mono text-xs text-muted">
              {pattern.category}
            </span>
          </div>
          <div className="flex items-center gap-2 shrink-0">
            <RevokeButton
              id={pattern.id}
              table="codex_patterns"
              name={pattern.name}
              onRevoked={onRevoked}
            />
            <span className="font-mono text-xs text-dim">
              {expanded ? '▲' : '▼'}
            </span>
          </div>
        </div>
        <p className="mt-1 text-xs text-muted line-clamp-2">
          {pattern.when_to_use}
        </p>
      </button>

      {expanded && (
        <div className="border-t border-rule px-5 py-4 space-y-3">
          {pattern.rationale && (
            <div>
              <p className="section-label mb-2">Rationale</p>
              <p className="text-xs text-muted leading-relaxed">
                {pattern.rationale}
              </p>
            </div>
          )}

          {pattern.key_rules?.length > 0 && (
            <div>
              <p className="section-label mb-2">Key Rules</p>
              <ul className="list-disc list-inside space-y-0.5">
                {pattern.key_rules.map((rule, i) => (
                  <li key={i} className="text-xs text-muted">
                    {rule}
                  </li>
                ))}
              </ul>
            </div>
          )}

          {pattern.common_mistakes?.length > 0 && (
            <div>
              <p className="section-label mb-2">Common Mistakes</p>
              <ul className="list-disc list-inside space-y-0.5">
                {pattern.common_mistakes.map((mistake, i) => (
                  <li key={i} className="text-xs text-red-400/80">
                    {mistake}
                  </li>
                ))}
              </ul>
            </div>
          )}

          {pattern.reference_files?.length > 0 && (
            <div>
              <p className="section-label mb-2">Reference Files</p>
              <div className="flex flex-wrap gap-1.5">
                {pattern.reference_files.map((file, i) => (
                  <span
                    key={i}
                    className="rounded-sm border border-rule bg-surface-900 px-2 py-0.5 font-mono text-xs text-muted"
                  >
                    {file}
                  </span>
                ))}
              </div>
            </div>
          )}

          <EdgeInspector entryId={pattern.id} entryType="pattern" productId={productId} />
        </div>
      )}
    </div>
  )
}

// ── Section card ─────────────────────────────────────────────────────────────

function SectionCard({ section: s, onRevoked, productId }: { section: CodexSection; onRevoked: () => void; productId: string }) {
  const [expanded, setExpanded] = useState(false)

  const isDeepAnalysis = useMemo(() => {
    const c = s.content as Record<string, string> | null
    return c && typeof c === 'object' && 'source' in c && c.source === 'deep_analysis'
  }, [s.content])

  const renderedContent = useMemo(() => {
    if (s.content === null || s.content === undefined) return null
    if (typeof s.content === 'string') return s.content
    const c = s.content as Record<string, string>
    if (c && typeof c === 'object' && 'analysis' in c && typeof c.analysis === 'string') {
      return c.analysis
    }
    try {
      return JSON.stringify(s.content, null, 2)
    } catch {
      return String(s.content)
    }
  }, [s.content])

  return (
    <div className="rounded-sm border border-rule bg-surface-800 transition-colors hover:border-sharp">
      <button
        type="button"
        className="w-full px-5 py-4 text-left"
        onClick={() => setExpanded((v) => !v)}
      >
        <div className="flex items-start justify-between gap-2">
          <div className="flex items-center gap-2 flex-wrap flex-1 min-w-0">
            <span className="text-sm font-medium text-fg">{s.name}</span>
            <SectionTypeBadge sectionType={s.section_type} />
            <span className="rounded-sm border border-rule bg-surface-900 px-1.5 py-0.5 font-mono text-xs text-dim">
              v{s.version}
            </span>
          </div>
          <div className="flex items-center gap-2 shrink-0">
            <RevokeButton
              id={s.id}
              table="codex_sections"
              name={s.name}
              onRevoked={onRevoked}
            />
            <span className="font-mono text-xs text-dim">
              {expanded ? '▲' : '▼'}
            </span>
          </div>
        </div>
      </button>

      {expanded && (
        <div className="border-t border-rule px-5 py-4 space-y-3">
          {renderedContent && (
            isDeepAnalysis ? (
              <div className="rounded-sm border border-rule bg-surface-950 p-4 text-sm text-muted leading-relaxed whitespace-pre-wrap">
                {renderedContent}
              </div>
            ) : (
              <pre className="overflow-x-auto rounded-sm border border-rule bg-surface-950 p-3 font-mono text-xs text-muted leading-relaxed whitespace-pre-wrap">
                {renderedContent}
              </pre>
            )
          )}

          <EdgeInspector entryId={s.id} entryType="section" productId={productId} />
        </div>
      )}
    </div>
  )
}

// ── Main component ───────────────────────────────────────────────────────────

export default function CodexBrowser({ productId }: { productId?: string | null }) {
  const { get } = useApi()
  const [activeTab, setActiveTab] = useState<Tab>('modules')

  const [modules, setModules] = useState<CodexModule[]>([])
  const [patterns, setPatterns] = useState<CodexPattern[]>([])
  const [sections, setSections] = useState<CodexSection[]>([])
  const [currentVersion, setCurrentVersion] = useState<CodexVersion | null>(null)

  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)

  const [selectedCategory, setSelectedCategory] = useState<string | null>(null)
  const [search, setSearch] = useState('')

  const fetchData = useCallback(async () => {
    if (!productId) {
      setModules([])
      setPatterns([])
      setSections([])
      setCurrentVersion(null)
      setLoading(false)
      return
    }
    try {
      setError(null)
      const qs = `product_id=${encodeURIComponent(productId)}`
      const [modulesData, patternsData, sectionsData, versionData] = await Promise.all([
        get<CodexModule[]>(`/codex/modules?${qs}`),
        get<CodexPattern[]>(`/codex/patterns?${qs}`),
        get<CodexSection[]>(`/codex/sections?${qs}`),
        get<CodexVersion>(`/codex/version?${qs}`),
      ])
      setModules(modulesData)
      setPatterns(patternsData)
      setSections(sectionsData)
      setCurrentVersion(versionData)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to load codex data')
    } finally {
      setLoading(false)
    }
  }, [get, productId])

  useEffect(() => {
    fetchData()
  }, [fetchData])

  const patternCategories = useMemo(() => {
    const cats = new Set(patterns.map((p) => p.category))
    return Array.from(cats).sort()
  }, [patterns])

  const filteredPatterns = useMemo(() => {
    return patterns.filter((p) => {
      const matchCat = selectedCategory ? p.category === selectedCategory : true
      const q = search.toLowerCase()
      const matchSearch =
        !q ||
        p.name.toLowerCase().includes(q) ||
        p.when_to_use.toLowerCase().includes(q) ||
        p.category.toLowerCase().includes(q)
      return matchCat && matchSearch
    })
  }, [patterns, selectedCategory, search])

  if (loading) {
    return (
      <div className="flex h-64 items-center justify-center">
        <div className="h-8 w-8 animate-spin rounded-full border-2 border-accent border-t-transparent" />
      </div>
    )
  }

  const tabs: { id: Tab; label: string; count: number }[] = [
    { id: 'modules', label: 'Modules', count: modules.length },
    { id: 'patterns', label: 'Patterns', count: patterns.length },
    { id: 'sections', label: 'Sections', count: sections.length },
  ]

  return (
    <div className="flex flex-col gap-4">
      {productId && <TopologyPanel productId={productId} />}

      <div className="flex items-center justify-between">
        <div>
          <h2 className="font-display text-lg text-fg">Codex Browser</h2>
          {currentVersion && (
            <p className="font-mono text-xs text-muted">
              Version {currentVersion.version_number} — {currentVersion.change_summary} · {modules.length} module{modules.length !== 1 ? 's' : ''} · {patterns.length} pattern{patterns.length !== 1 ? 's' : ''}
            </p>
          )}
        </div>
        {error && (
          <span className="rounded-sm border border-red-500/30 bg-red-500/10 px-3 py-1 font-mono text-xs text-red-400">
            {error}
          </span>
        )}
      </div>

      <CodexSearchBar />

      {/* Tabs */}
      <div className="flex gap-1 border-b border-rule">
        {tabs.map((tab) => (
          <button
            key={tab.id}
            type="button"
            onClick={() => setActiveTab(tab.id)}
            className={[
              'px-4 py-2 font-mono text-sm transition-colors -mb-px border-b-2',
              activeTab === tab.id
                ? 'border-accent text-accent'
                : 'border-transparent text-muted hover:text-fg',
            ].join(' ')}
          >
            {tab.label}
            <span className="ml-1.5 text-xs text-dim">({tab.count})</span>
          </button>
        ))}
      </div>

      {/* Modules tab */}
      {activeTab === 'modules' && (
        <div className="flex flex-col gap-3">
          {modules.length === 0 ? (
            <div className="flex h-48 items-center justify-center rounded-sm border border-rule bg-surface-800">
              <p className="font-mono text-sm text-dim">No modules found.</p>
            </div>
          ) : (
            modules.map((m) => <ModuleCard key={m.id} module={m} onRevoked={fetchData} productId={productId ?? ''} />)
          )}
        </div>
      )}

      {/* Patterns tab */}
      {activeTab === 'patterns' && (
        <div className="flex flex-col gap-4">
          <input
            type="text"
            placeholder="Search patterns..."
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            className="w-full max-w-sm rounded-sm border border-rule bg-surface-800 px-3 py-2 font-mono text-sm text-fg placeholder-dim focus:border-accent focus:outline-none"
          />

          <div className="flex gap-4">
            <aside className="w-48 shrink-0">
              <div className="rounded-sm border border-rule bg-surface-800">
                <div className="border-b border-rule px-3 py-2">
                  <p className="font-mono text-xs font-medium uppercase tracking-widest text-muted">
                    Categories
                  </p>
                </div>
                <nav className="p-2">
                  <button
                    type="button"
                    onClick={() => setSelectedCategory(null)}
                    className={[
                      'w-full rounded-sm px-3 py-1.5 text-left font-mono text-sm transition-colors',
                      selectedCategory === null
                        ? 'bg-accent/10 text-accent'
                        : 'text-muted hover:bg-surface-700 hover:text-fg',
                    ].join(' ')}
                  >
                    All ({patterns.length})
                  </button>
                  {patternCategories.map((cat) => {
                    const count = patterns.filter((p) => p.category === cat).length
                    return (
                      <button
                        key={cat}
                        type="button"
                        onClick={() =>
                          setSelectedCategory(cat === selectedCategory ? null : cat)
                        }
                        className={[
                          'mt-0.5 w-full rounded-sm px-3 py-1.5 text-left font-mono text-sm transition-colors',
                          selectedCategory === cat
                            ? 'bg-accent/10 text-accent'
                            : 'text-muted hover:bg-surface-700 hover:text-fg',
                        ].join(' ')}
                      >
                        {cat}{' '}
                        <span className="text-xs text-dim">({count})</span>
                      </button>
                    )
                  })}
                </nav>
              </div>
            </aside>

            <div className="flex-1 min-w-0">
              {filteredPatterns.length === 0 ? (
                <div className="flex h-48 items-center justify-center rounded-sm border border-rule bg-surface-800">
                  <p className="font-mono text-sm text-dim">No patterns match your search.</p>
                </div>
              ) : (
                <div className="flex flex-col gap-3">
                  {filteredPatterns.map((pattern) => (
                    <PatternCard key={pattern.id} pattern={pattern} onRevoked={fetchData} productId={productId ?? ''} />
                  ))}
                </div>
              )}
            </div>
          </div>
        </div>
      )}

      {/* Sections tab */}
      {activeTab === 'sections' && (
        <div className="flex flex-col gap-5">
          {sections.length === 0 ? (
            <div className="flex h-48 items-center justify-center rounded-sm border border-rule bg-surface-800">
              <p className="font-mono text-sm text-dim">No sections found.</p>
            </div>
          ) : (
            (() => {
              const typeOrder: Record<string, number> = { architecture: 0, domain: 1, process: 2, testing: 3, style: 4, pattern: 5, anti_pattern: 6, module: 7 }
              const typeLabels: Record<string, string> = {
                architecture: 'Architecture & Structure',
                domain: 'Domain Knowledge',
                process: 'Process & Observability',
                testing: 'Testing',
                style: 'Style & Conventions',
                pattern: 'Patterns',
                anti_pattern: 'Anti-Patterns',
                module: 'Modules',
              }
              const grouped = new Map<string, typeof sections>()
              for (const s of sections) {
                const key = s.section_type
                if (!grouped.has(key)) grouped.set(key, [])
                grouped.get(key)!.push(s)
              }
              const sortedGroups = [...grouped.entries()].sort(
                ([a], [b]) => (typeOrder[a] ?? 99) - (typeOrder[b] ?? 99)
              )
              return sortedGroups.map(([type, items]) => (
                <div key={type}>
                  <h3 className="mb-2 flex items-center gap-2 text-sm font-semibold text-muted">
                    <SectionTypeBadge sectionType={type as CodexSection['section_type']} />
                    <span>{typeLabels[type] ?? type}</span>
                    <span className="font-mono text-xs text-dim">({items.length})</span>
                  </h3>
                  <div className="flex flex-col gap-2">
                    {items.map((s) => (
                      <SectionCard key={s.id} section={s} onRevoked={fetchData} productId={productId ?? ''} />
                    ))}
                  </div>
                </div>
              ))
            })()
          )}
        </div>
      )}
    </div>
  )
}
