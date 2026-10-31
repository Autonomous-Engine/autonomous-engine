import { useEffect, useMemo, useRef, useState } from 'react'
import type { GraphNodeData, CodexSection, CodexPattern } from '@/types'
import { RISK_COLORS } from './palette'

interface NodeDetailPanelProps {
  node: GraphNodeData | null
  onClose: () => void
  relatedInsights?: CodexSection[]
  relatedPatterns?: CodexPattern[]
  allModuleNames?: string[]
  onNavigateModule?: (moduleName: string) => void
}

function SectionTypeBadge({ sectionType }: { sectionType: string }) {
  const colors: Record<string, string> = {
    architecture: 'bg-violet-500/15 text-violet-400 border-violet-500/20',
    domain: 'bg-cyan-500/15 text-cyan-400 border-cyan-500/20',
    process: 'bg-amber-500/15 text-amber-400 border-amber-500/20',
    testing: 'bg-emerald-500/15 text-emerald-400 border-emerald-500/20',
    style: 'bg-pink-500/15 text-pink-400 border-pink-500/20',
  }
  const cls = colors[sectionType?.toLowerCase()] ?? 'bg-surface-700/40 text-muted border-rule'
  return (
    <span className={`inline-flex items-center rounded-sm border px-1.5 py-0.5 font-mono text-[10px] ${cls}`}>
      {sectionType}
    </span>
  )
}

function HighlightedText({
  text,
  allModuleNames,
  onNavigateModule,
}: {
  text: string
  allModuleNames: string[]
  onNavigateModule?: (name: string) => void
}) {
  const parts = useMemo(() => {
    if (allModuleNames.length === 0) return [{ text, isModule: false, name: '' }]

    const sorted = [...allModuleNames].sort((a, b) => b.length - a.length)
    const escaped = sorted.map((n) => n.replace(/[.*+?^${}()|[\]\\]/g, '\\$&'))
    const regex = new RegExp(`\\b(${escaped.join('|')})\\b`, 'gi')

    const result: { text: string; isModule: boolean; name: string }[] = []
    let lastIndex = 0
    let match: RegExpExecArray | null
    while ((match = regex.exec(text)) !== null) {
      if (match.index > lastIndex) {
        result.push({ text: text.slice(lastIndex, match.index), isModule: false, name: '' })
      }
      const matchedName = allModuleNames.find((n) => n.toLowerCase() === match![0].toLowerCase()) ?? match[0]
      result.push({ text: match[0], isModule: true, name: matchedName })
      lastIndex = match.index + match[0].length
    }
    if (lastIndex < text.length) {
      result.push({ text: text.slice(lastIndex), isModule: false, name: '' })
    }
    return result
  }, [text, allModuleNames])

  return (
    <span>
      {parts.map((p, i) =>
        p.isModule ? (
          <button
            key={i}
            type="button"
            onClick={() => onNavigateModule?.(p.name)}
            className="rounded-sm bg-accent/15 px-1 py-0.5 font-mono text-accent hover:bg-accent/25 transition-colors"
            title={`Navigate to ${p.name}`}
          >
            {p.text}
          </button>
        ) : (
          <span key={i}>{p.text}</span>
        ),
      )}
    </span>
  )
}

export function NodeDetailPanel({
  node,
  onClose,
  relatedInsights = [],
  relatedPatterns = [],
  allModuleNames = [],
  onNavigateModule,
}: NodeDetailPanelProps) {
  const closeBtnRef = useRef<HTMLButtonElement>(null)
  const reduceMotion =
    typeof window !== 'undefined' &&
    window.matchMedia?.('(prefers-reduced-motion: reduce)').matches

  const [expandedInsights, setExpandedInsights] = useState<Set<string>>(new Set())

  useEffect(() => {
    if (node && closeBtnRef.current) {
      closeBtnRef.current.focus()
    }
    setExpandedInsights(new Set())
  }, [node])

  if (!node) return null

  const palette = RISK_COLORS[node.risk_level] ?? RISK_COLORS.unknown

  const toggleInsight = (id: string) => {
    setExpandedInsights((prev) => {
      const next = new Set(prev)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })
  }

  return (
    <aside
      role="region"
      aria-live="polite"
      aria-label={`Module detail for ${node.name}`}
      className={`absolute right-0 top-0 z-20 h-full w-[420px] overflow-y-auto border-l border-rule bg-surface-900/95 p-5 shadow-2xl ${
        reduceMotion ? '' : 'transition-transform duration-300'
      }`}
    >
      <div className="mb-4 flex items-start justify-between gap-3">
        <div className="min-w-0">
          <div className="mb-1 flex items-center gap-2">
            <span
              className="inline-block h-2.5 w-2.5 rounded-full"
              style={{ background: palette.border }}
              aria-hidden
            />
            <span
              className="rounded-sm px-2 py-0.5 font-mono text-xs font-medium"
              style={{ color: palette.text, background: palette.fill }}
            >
              {node.risk_level}
            </span>
            {node.external && (
              <span className="rounded-sm border border-rule px-2 py-0.5 font-mono text-xs text-muted">
                external
              </span>
            )}
            {relatedInsights.length > 0 && (
              <span className="rounded-sm bg-indigo-500/15 border border-indigo-500/20 px-2 py-0.5 font-mono text-xs text-indigo-400">
                {relatedInsights.length} insight{relatedInsights.length !== 1 ? 's' : ''}
              </span>
            )}
          </div>
          <h3 className="truncate font-display text-base text-fg">{node.name}</h3>
          {node.owner_domain && (
            <p className="mt-0.5 font-mono text-xs text-dim">Owner: {node.owner_domain}</p>
          )}
        </div>
        <button
          ref={closeBtnRef}
          type="button"
          onClick={onClose}
          aria-label="Close detail panel"
          className="rounded-sm border border-rule px-2 py-1 font-mono text-xs text-muted transition-colors hover:border-accent hover:text-accent"
        >
          ✕
        </button>
      </div>

      {node.purpose && (
        <section className="mb-4">
          <h4 className="section-label mb-2">Purpose</h4>
          <p className="text-sm leading-relaxed text-muted">{node.purpose}</p>
        </section>
      )}

      {/* Related Deep Analysis Insights */}
      {relatedInsights.length > 0 && !node.external && (
        <section className="mb-4">
          <h4 className="section-label mb-2">
            Related Insights ({relatedInsights.length})
          </h4>
          <div className="flex flex-col gap-2">
            {relatedInsights.map((s) => {
              const content = s.content as Record<string, string>
              const analysis = content?.analysis ?? ''
              const isExpanded = expandedInsights.has(s.id)
              const excerpt = analysis.length > 200 ? analysis.slice(0, 200) + '...' : analysis

              return (
                <div
                  key={s.id}
                  className="rounded-sm border border-rule bg-surface-800/60 p-3"
                >
                  <div className="flex items-center gap-2 mb-1.5">
                    <SectionTypeBadge sectionType={s.section_type} />
                    <span className="font-mono text-xs text-fg truncate">{s.name}</span>
                  </div>
                  <div className="text-xs leading-relaxed text-muted whitespace-pre-wrap">
                    <HighlightedText
                      text={isExpanded ? analysis : excerpt}
                      allModuleNames={allModuleNames}
                      onNavigateModule={onNavigateModule}
                    />
                  </div>
                  {analysis.length > 200 && (
                    <button
                      type="button"
                      onClick={() => toggleInsight(s.id)}
                      className="mt-1.5 font-mono text-[10px] text-accent hover:text-accent/80"
                    >
                      {isExpanded ? 'show less' : 'show more'}
                    </button>
                  )}
                </div>
              )
            })}
          </div>
        </section>
      )}

      {/* Related Patterns */}
      {relatedPatterns.length > 0 && !node.external && (
        <section className="mb-4">
          <h4 className="section-label mb-2">
            Patterns ({relatedPatterns.length})
          </h4>
          <div className="flex flex-col gap-2">
            {relatedPatterns.map((p) => (
              <div
                key={p.id}
                className="rounded-sm border border-indigo-500/20 bg-indigo-500/5 p-2.5"
              >
                <span className="font-mono text-xs text-indigo-400">{p.name}</span>
                {p.rationale && (
                  <p className="mt-1 text-xs text-muted">{p.rationale}</p>
                )}
                {p.reference_files?.length > 0 && (
                  <div className="mt-1.5 flex flex-wrap gap-1">
                    {p.reference_files.slice(0, 3).map((f, i) => (
                      <span key={i} className="rounded-sm bg-surface-700/40 px-1.5 py-0.5 font-mono text-[10px] text-dim">
                        {f}
                      </span>
                    ))}
                  </div>
                )}
              </div>
            ))}
          </div>
        </section>
      )}

      {node.public_interface_summary && !node.external && (
        <section className="mb-4">
          <h4 className="section-label mb-2">Public interface</h4>
          <p className="rounded-sm border border-rule bg-surface-950 p-2 font-mono text-xs text-muted">
            {node.public_interface_summary}
          </p>
        </section>
      )}

      {node.test_coverage_notes && !node.external && (
        <section className="mb-4">
          <h4 className="section-label mb-2">Test coverage</h4>
          <p className="text-xs leading-relaxed text-dim">{node.test_coverage_notes}</p>
        </section>
      )}

      {node.dependencies?.length > 0 && (
        <section className="mb-4">
          <h4 className="section-label mb-2">
            Dependencies ({node.dependencies.length})
          </h4>
          <div className="flex flex-wrap gap-1.5">
            {node.dependencies.map((d, i) => (
              <button
                key={i}
                type="button"
                onClick={() => onNavigateModule?.(d)}
                className="rounded-sm border border-rule bg-surface-800 px-2 py-0.5 font-mono text-xs text-muted hover:border-accent hover:text-accent transition-colors"
              >
                {d}
              </button>
            ))}
          </div>
        </section>
      )}

      {node.dependents?.length > 0 && (
        <section className="mb-4">
          <h4 className="section-label mb-2">
            Dependents ({node.dependents.length})
          </h4>
          <div className="flex flex-wrap gap-1.5">
            {node.dependents.map((d, i) => (
              <button
                key={i}
                type="button"
                onClick={() => onNavigateModule?.(d)}
                className="rounded-sm border border-accent/20 bg-accent/10 px-2 py-0.5 font-mono text-xs text-accent-dim hover:border-accent hover:text-accent transition-colors"
              >
                {d}
              </button>
            ))}
          </div>
        </section>
      )}

      {node.external && (
        <p className="mt-2 font-mono text-xs italic text-dim">
          This dependency is not indexed in the current product. It may be a third-party
          package or a module in another product's scope.
        </p>
      )}
    </aside>
  )
}
