import { useState, useEffect, useCallback, useRef } from 'react'
import { useSearchParams } from 'react-router-dom'
import Editor, { type Monaco } from '@monaco-editor/react'
import type { editor } from 'monaco-editor'
import { useApi } from '@/hooks/useApi'

// ─── Types ───────────────────────────────────────────────────────────────────

interface Product {
  id: string
  name: string
}

interface Repository {
  id: string
  name: string
  github_url: string
  base_branch: string
  clone_path: string
}

interface BranchesResponse {
  branches: string[]
  default_branch: string
}

interface TreeEntry {
  name: string
  type: 'blob' | 'tree'
  path: string
}

interface TreeResponse {
  entries: TreeEntry[]
  ref: string
  repo_path: string
}

interface BlobResponse {
  content: string
  path: string
  size: number
  ref: string
  binary?: boolean
  truncated?: boolean
}

interface OpenTab {
  path: string
  label: string
  content: string
  language: string
  binary?: boolean
  size?: number
}

interface CodexPatternEntry {
  id: string
  name: string
  category: string
  when_to_use: string
  reference_files: string[]
  rationale: string
  key_rules: string[]
  common_mistakes: string[]
}

interface CodexModuleEntry {
  id: string
  name: string
  purpose: string
  dependencies: string[]
  dependents: string[]
  risk_level: string
  test_coverage_notes: string
}

interface CodexSectionEntry {
  id: string
  section_type: string
  name: string
  content: unknown
  version: number
  updated_at: string
}

// ─── Language Detection ─────────────────────────────────────────────────────

const EXT_LANG: Record<string, string> = {
  ts: 'typescript', tsx: 'typescript', js: 'javascript', jsx: 'javascript',
  mjs: 'javascript', cjs: 'javascript',
  go: 'go', py: 'python', rb: 'ruby', rs: 'rust',
  java: 'java', kt: 'kotlin', scala: 'scala',
  c: 'c', h: 'c', cpp: 'cpp', hpp: 'cpp', cc: 'cpp',
  cs: 'csharp', swift: 'swift', m: 'objective-c',
  json: 'json', yaml: 'yaml', yml: 'yaml', toml: 'ini',
  xml: 'xml', html: 'html', htm: 'html', svg: 'xml',
  css: 'css', scss: 'scss', less: 'less',
  md: 'markdown', mdx: 'markdown',
  sql: 'sql', graphql: 'graphql', gql: 'graphql',
  sh: 'shell', bash: 'shell', zsh: 'shell', fish: 'shell',
  ps1: 'powershell', bat: 'bat',
  dockerfile: 'dockerfile',
  makefile: 'makefile',
  r: 'r', lua: 'lua', perl: 'perl', php: 'php',
  proto: 'protobuf', tf: 'hcl',
}

const FILENAME_LANG: Record<string, string> = {
  Dockerfile: 'dockerfile', Makefile: 'makefile', Jenkinsfile: 'groovy',
  Vagrantfile: 'ruby', Gemfile: 'ruby', Rakefile: 'ruby',
  '.gitignore': 'ini', '.env': 'ini', '.editorconfig': 'ini',
  'go.mod': 'go', 'go.sum': 'go',
  'tsconfig.json': 'json', 'package.json': 'json',
}

function detectLanguage(filePath: string): string {
  const name = filePath.split('/').pop() || ''
  if (FILENAME_LANG[name]) return FILENAME_LANG[name]
  const ext = name.includes('.') ? name.split('.').pop()!.toLowerCase() : ''
  return EXT_LANG[ext] || 'plaintext'
}

// ─── File Icons ─────────────────────────────────────────────────────────────

const ICON_MAP: Record<string, string> = {
  typescript: '⟨⟩', javascript: '⟨⟩', go: '◇', python: '◆',
  rust: '⚙', java: '☕', ruby: '◈', json: '{}', yaml: '⊞',
  markdown: '¶', sql: '⊡', html: '◁', css: '◀', shell: '$',
  dockerfile: '▦', makefile: '▤', default: '○',
}

function fileIcon(lang: string): string {
  return ICON_MAP[lang] || ICON_MAP.default
}

function dirIcon(open: boolean): string {
  return open ? '▾' : '▸'
}

// ─── Import Resolution ──────────────────────────────────────────────────────

interface ImportMatch {
  path: string
  startCol: number
  endCol: number
}

function extractImportsFromLine(line: string): ImportMatch[] {
  const results: ImportMatch[] = []
  const patterns = [
    // JS/TS: import ... from '...' or import '...'
    /(?:from\s+|import\s+)['"]([^'"]+)['"]/g,
    // JS/TS: require('...')
    /require\s*\(\s*['"]([^'"]+)['"]\s*\)/g,
    // Go: "github.com/..." or "./..."
    /^\s*"([^"]+)"\s*$/g,
    // Python: from X import ... (capture X)
    /^\s*from\s+(\S+)\s+import/g,
  ]
  for (const pat of patterns) {
    let m: RegExpExecArray | null
    while ((m = pat.exec(line)) !== null) {
      const full = m[0]
      const importPath = m[1]
      const startInMatch = full.indexOf(importPath)
      const startCol = m.index + startInMatch + 1
      results.push({
        path: importPath,
        startCol,
        endCol: startCol + importPath.length,
      })
    }
  }
  return results
}

function resolveImportPath(
  importStr: string,
  currentFilePath: string,
  rootEntries: TreeEntry[],
  childrenMap: Map<string, TreeEntry[]>,
): string | null {
  // Handle @/ alias → src/
  let resolved = importStr
  if (resolved.startsWith('@/')) {
    resolved = 'src/' + resolved.slice(2)
  }

  // Handle relative paths
  if (resolved.startsWith('./') || resolved.startsWith('../')) {
    const currentDir = currentFilePath.includes('/')
      ? currentFilePath.substring(0, currentFilePath.lastIndexOf('/'))
      : ''
    const parts = currentDir ? currentDir.split('/') : []
    for (const seg of resolved.split('/')) {
      if (seg === '.') continue
      if (seg === '..') { parts.pop(); continue }
      parts.push(seg)
    }
    resolved = parts.join('/')
  }

  // Try the path directly, then with common extensions
  const extensions = ['', '.ts', '.tsx', '.js', '.jsx', '.go', '.py', '/index.ts', '/index.tsx', '/index.js']
  for (const ext of extensions) {
    const candidate = resolved + ext
    if (fileExistsInTree(candidate, rootEntries, childrenMap)) {
      return candidate
    }
  }
  return null
}

function fileExistsInTree(
  path: string,
  rootEntries: TreeEntry[],
  childrenMap: Map<string, TreeEntry[]>,
): boolean {
  const allEntries = [...rootEntries]
  for (const children of childrenMap.values()) {
    allEntries.push(...children)
  }
  return allEntries.some(e => e.path === path && e.type === 'blob')
}

// ─── File Tree Node ──────────────────────────────────────────────────────────

function FileTreeNode({
  entry, depth, expanded, childrenMap, activePath, loadingPaths,
  onToggleDir, onSelectFile,
}: {
  entry: TreeEntry
  depth: number
  expanded: Set<string>
  childrenMap: Map<string, TreeEntry[]>
  activePath: string | null
  loadingPaths: Set<string>
  onToggleDir: (path: string) => void
  onSelectFile: (path: string) => void
}) {
  const isDir = entry.type === 'tree'
  const isOpen = expanded.has(entry.path)
  const isActive = activePath === entry.path
  const isLoading = loadingPaths.has(entry.path)
  const children = childrenMap.get(entry.path)
  const lang = !isDir ? detectLanguage(entry.path) : ''

  return (
    <div>
      <button
        onClick={() => (isDir ? onToggleDir(entry.path) : onSelectFile(entry.path))}
        className={[
          'flex w-full items-center gap-1.5 py-0.5 pr-3 font-mono text-[13px] transition-colors',
          isActive
            ? 'bg-accent/10 text-accent'
            : 'text-muted hover:bg-surface-800 hover:text-fg',
        ].join(' ')}
        style={{ paddingLeft: `${depth * 14 + 8}px` }}
        title={entry.path}
      >
        <span className="w-4 shrink-0 text-center text-[11px] leading-none text-dim">
          {isDir
            ? (isLoading ? '…' : dirIcon(isOpen))
            : fileIcon(lang)}
        </span>
        <span className="truncate">{entry.name}</span>
      </button>

      {isDir && isOpen && children && (
        <div>
          {sortEntries(children).map((child) => (
            <FileTreeNode
              key={child.path}
              entry={child}
              depth={depth + 1}
              expanded={expanded}
              childrenMap={childrenMap}
              activePath={activePath}
              loadingPaths={loadingPaths}
              onToggleDir={onToggleDir}
              onSelectFile={onSelectFile}
            />
          ))}
        </div>
      )}
    </div>
  )
}

function sortEntries(entries: TreeEntry[]): TreeEntry[] {
  return [...entries].sort((a, b) => {
    if (a.type !== b.type) return a.type === 'tree' ? -1 : 1
    return a.name.localeCompare(b.name)
  })
}

// ─── Quick Open (Ctrl+P) ────────────────────────────────────────────────────

function QuickOpen({
  visible, query, onQueryChange, results, onSelect, onClose,
}: {
  visible: boolean
  query: string
  onQueryChange: (q: string) => void
  results: TreeEntry[]
  onSelect: (path: string) => void
  onClose: () => void
}) {
  const inputRef = useRef<HTMLInputElement>(null)

  useEffect(() => {
    if (visible) inputRef.current?.focus()
  }, [visible])

  if (!visible) return null

  return (
    <div className="absolute inset-0 z-50 flex justify-center pt-16" onClick={onClose}>
      <div
        className="h-fit w-[500px] rounded-sm border border-rule bg-surface-950 shadow-2xl"
        onClick={(e) => e.stopPropagation()}
      >
        <input
          ref={inputRef}
          type="text"
          value={query}
          onChange={(e) => onQueryChange(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Escape') onClose()
            if (e.key === 'Enter' && results.length > 0) {
              onSelect(results[0].path)
              onClose()
            }
          }}
          placeholder="Search files by name…"
          className="w-full border-b border-rule bg-surface-950 px-4 py-3 text-sm text-fg placeholder-dim outline-none"
        />
        <div className="max-h-80 overflow-y-auto">
          {results.length === 0 && query && (
            <div className="px-4 py-3 text-sm text-dim">No files found</div>
          )}
          {results.slice(0, 30).map((entry) => (
            <button
              key={entry.path}
              onClick={() => { onSelect(entry.path); onClose() }}
              className="flex w-full items-center gap-2 px-4 py-1.5 text-left font-mono text-[13px] text-muted transition-colors hover:bg-surface-800 hover:text-fg"
            >
              <span className="w-4 shrink-0 text-center text-[11px] text-dim">
                {fileIcon(detectLanguage(entry.path))}
              </span>
              <span className="truncate">{entry.name}</span>
              <span className="ml-auto truncate text-[11px] text-dim">{entry.path}</span>
            </button>
          ))}
        </div>
      </div>
    </div>
  )
}

// ─── Codex Matching ─────────────────────────────────────────────────────────

function matchPatternsToFile(patterns: CodexPatternEntry[], filePath: string): CodexPatternEntry[] {
  if (!filePath) return []
  return patterns.filter(p =>
    p.reference_files?.some(ref => {
      const norm = ref.replace(/^\.\//, '')
      return filePath === norm || filePath.endsWith('/' + norm) || norm.endsWith('/' + filePath) ||
        filePath.startsWith(norm) || norm.startsWith(filePath.substring(0, filePath.lastIndexOf('/') + 1))
    }),
  )
}

function matchModulesToFile(modules: CodexModuleEntry[], filePath: string): CodexModuleEntry[] {
  if (!filePath) return []
  const pathLower = filePath.toLowerCase()
  const segments = pathLower.split('/')
  return modules.filter(m => {
    const nameLower = m.name.toLowerCase().replace(/\s+/g, '-')
    return segments.some(seg => seg === nameLower || seg.includes(nameLower)) ||
      pathLower.includes(nameLower)
  })
}

function matchSectionsToFile(sections: CodexSectionEntry[], filePath: string): CodexSectionEntry[] {
  if (!filePath) return []
  const contentStr = (s: CodexSectionEntry) => {
    if (typeof s.content === 'string') return s.content
    try { return JSON.stringify(s.content) } catch { return '' }
  }
  const name = filePath.split('/').pop() || ''
  const dir = filePath.substring(0, filePath.lastIndexOf('/'))
  return sections.filter(s => {
    const text = contentStr(s).toLowerCase()
    return text.includes(name.toLowerCase()) || (dir && text.includes(dir.toLowerCase()))
  })
}

// ─── Codex Context Panel ────────────────────────────────────────────────────

function CodexPanel({
  patterns, modules, sections, filePath, onNavigateFile,
}: {
  patterns: CodexPatternEntry[]
  modules: CodexModuleEntry[]
  sections: CodexSectionEntry[]
  filePath: string | null
  onNavigateFile: (path: string) => void
}) {
  const [expandedPatterns, setExpandedPatterns] = useState<Set<string>>(new Set())
  const [expandedModules, setExpandedModules] = useState<Set<string>>(new Set())
  const [expandedSections, setExpandedSections] = useState<Set<string>>(new Set())

  const matchedPatterns = filePath ? matchPatternsToFile(patterns, filePath) : []
  const matchedModules = filePath ? matchModulesToFile(modules, filePath) : []
  const matchedSections = filePath ? matchSectionsToFile(sections, filePath).slice(0, 5) : []

  const totalMatches = matchedPatterns.length + matchedModules.length + matchedSections.length

  function toggleSet(set: Set<string>, id: string, setter: (s: Set<string>) => void) {
    const next = new Set(set)
    if (next.has(id)) next.delete(id); else next.add(id)
    setter(next)
  }

  function renderContent(content: unknown): string {
    if (typeof content === 'string') return content
    try { return JSON.stringify(content, null, 2) } catch { return '' }
  }

  if (!filePath) {
    return (
      <div className="flex items-center justify-center h-full">
        <p className="text-xs text-dim">Open a file to see codex context</p>
      </div>
    )
  }

  return (
    <div className="overflow-y-auto h-full">
      {totalMatches === 0 && (
        <div className="px-3 py-4">
          <p className="text-xs text-dim">No codex entries reference this file</p>
        </div>
      )}

      {/* Matched Patterns */}
      {matchedPatterns.length > 0 && (
        <div className="border-b border-rule/40">
          <div className="px-3 py-2">
            <p className="text-[11px] font-semibold text-muted tracking-wide">
              PATTERNS <span className="ml-1 text-accent">{matchedPatterns.length}</span>
            </p>
          </div>
          {matchedPatterns.map(p => {
            const isOpen = expandedPatterns.has(p.id)
            return (
              <div key={p.id} className="border-t border-rule/20">
                <button
                  onClick={() => toggleSet(expandedPatterns, p.id, setExpandedPatterns)}
                  className="flex w-full items-center gap-2 px-3 py-1.5 text-left text-[13px] text-fg transition-colors hover:bg-surface-800"
                >
                  <span className="text-[11px] text-dim">{isOpen ? '▾' : '▸'}</span>
                  <span className="truncate font-medium">{p.name}</span>
                  <span className="ml-auto shrink-0 rounded-sm bg-accent/10 px-1.5 py-0.5 text-[10px] text-accent">
                    {p.category}
                  </span>
                </button>
                {isOpen && (
                  <div className="space-y-2 px-3 pb-3 pt-1">
                    {p.when_to_use && (
                      <div>
                        <p className="text-[10px] font-medium text-muted">WHEN TO USE</p>
                        <p className="text-xs text-dim leading-relaxed">{p.when_to_use}</p>
                      </div>
                    )}
                    {p.rationale && (
                      <div>
                        <p className="text-[10px] font-medium text-muted">RATIONALE</p>
                        <p className="text-xs text-dim leading-relaxed">{p.rationale}</p>
                      </div>
                    )}
                    {p.key_rules?.length > 0 && (
                      <div>
                        <p className="text-[10px] font-medium text-muted">KEY RULES</p>
                        <ul className="space-y-0.5">
                          {p.key_rules.map((r, i) => (
                            <li key={i} className="text-xs text-dim flex gap-1.5">
                              <span className="shrink-0 text-accent">·</span>
                              <span>{r}</span>
                            </li>
                          ))}
                        </ul>
                      </div>
                    )}
                    {p.common_mistakes?.length > 0 && (
                      <div>
                        <p className="text-[10px] font-medium text-red-400">COMMON MISTAKES</p>
                        <ul className="space-y-0.5">
                          {p.common_mistakes.map((m, i) => (
                            <li key={i} className="text-xs text-red-300/70 flex gap-1.5">
                              <span className="shrink-0">⚠</span>
                              <span>{m}</span>
                            </li>
                          ))}
                        </ul>
                      </div>
                    )}
                    {p.reference_files?.length > 0 && (
                      <div>
                        <p className="text-[10px] font-medium text-muted">REFERENCE FILES</p>
                        <div className="flex flex-wrap gap-1">
                          {p.reference_files.map((f, i) => (
                            <button
                              key={i}
                              onClick={() => onNavigateFile(f.replace(/^\.\//, ''))}
                              className="rounded-sm bg-surface-800 px-1.5 py-0.5 font-mono text-[10px] text-accent transition-colors hover:bg-surface-700"
                            >
                              {f.split('/').pop()}
                            </button>
                          ))}
                        </div>
                      </div>
                    )}
                  </div>
                )}
              </div>
            )
          })}
        </div>
      )}

      {/* Matched Modules */}
      {matchedModules.length > 0 && (
        <div className="border-b border-rule/40">
          <div className="px-3 py-2">
            <p className="text-[11px] font-semibold text-muted tracking-wide">
              MODULES <span className="ml-1 text-accent">{matchedModules.length}</span>
            </p>
          </div>
          {matchedModules.map(m => {
            const isOpen = expandedModules.has(m.id)
            const riskColors: Record<string, string> = {
              low: 'text-emerald-400 bg-emerald-500/10',
              medium: 'text-amber-400 bg-amber-500/10',
              high: 'text-red-400 bg-red-500/10',
            }
            const riskClass = riskColors[m.risk_level] || 'text-dim bg-surface-800'
            return (
              <div key={m.id} className="border-t border-rule/20">
                <button
                  onClick={() => toggleSet(expandedModules, m.id, setExpandedModules)}
                  className="flex w-full items-center gap-2 px-3 py-1.5 text-left text-[13px] text-fg transition-colors hover:bg-surface-800"
                >
                  <span className="text-[11px] text-dim">{isOpen ? '▾' : '▸'}</span>
                  <span className="truncate font-medium">{m.name}</span>
                  <span className={`ml-auto shrink-0 rounded-sm px-1.5 py-0.5 text-[10px] ${riskClass}`}>
                    {m.risk_level}
                  </span>
                </button>
                {isOpen && (
                  <div className="space-y-2 px-3 pb-3 pt-1">
                    {m.purpose && (
                      <div>
                        <p className="text-[10px] font-medium text-muted">PURPOSE</p>
                        <p className="text-xs text-dim leading-relaxed">{m.purpose}</p>
                      </div>
                    )}
                    {m.dependencies?.length > 0 && (
                      <div>
                        <p className="text-[10px] font-medium text-muted">DEPENDS ON</p>
                        <div className="flex flex-wrap gap-1">
                          {m.dependencies.map((d, i) => (
                            <span key={i} className="rounded-sm bg-surface-800 px-1.5 py-0.5 text-[10px] text-dim">{d}</span>
                          ))}
                        </div>
                      </div>
                    )}
                    {m.dependents?.length > 0 && (
                      <div>
                        <p className="text-[10px] font-medium text-muted">DEPENDED ON BY</p>
                        <div className="flex flex-wrap gap-1">
                          {m.dependents.map((d, i) => (
                            <span key={i} className="rounded-sm bg-surface-800 px-1.5 py-0.5 text-[10px] text-dim">{d}</span>
                          ))}
                        </div>
                      </div>
                    )}
                    {m.test_coverage_notes && (
                      <div>
                        <p className="text-[10px] font-medium text-muted">TEST COVERAGE</p>
                        <p className="text-xs text-dim leading-relaxed">{m.test_coverage_notes}</p>
                      </div>
                    )}
                  </div>
                )}
              </div>
            )
          })}
        </div>
      )}

      {/* Matched Sections */}
      {matchedSections.length > 0 && (
        <div>
          <div className="px-3 py-2">
            <p className="text-[11px] font-semibold text-muted tracking-wide">
              INSIGHTS <span className="ml-1 text-accent">{matchedSections.length}</span>
            </p>
          </div>
          {matchedSections.map(s => {
            const isOpen = expandedSections.has(s.id)
            const typeIcons: Record<string, string> = {
              architecture: '△', pattern: '◇', anti_pattern: '⚠', testing: '✓',
              style: '◈', process: '→', domain: '◎', module: '□',
            }
            return (
              <div key={s.id} className="border-t border-rule/20">
                <button
                  onClick={() => toggleSet(expandedSections, s.id, setExpandedSections)}
                  className="flex w-full items-center gap-2 px-3 py-1.5 text-left text-[13px] text-fg transition-colors hover:bg-surface-800"
                >
                  <span className="text-[11px] text-dim">{isOpen ? '▾' : '▸'}</span>
                  <span className="w-4 shrink-0 text-center text-[11px] text-dim">{typeIcons[s.section_type] || '·'}</span>
                  <span className="truncate">{s.name}</span>
                </button>
                {isOpen && (
                  <div className="px-3 pb-3 pt-1">
                    <pre className="whitespace-pre-wrap text-xs text-dim leading-relaxed">{renderContent(s.content)}</pre>
                  </div>
                )}
              </div>
            )
          })}
        </div>
      )}
    </div>
  )
}

// ─── Tab Bar ────────────────────────────────────────────────────────────────

function TabBar({
  tabs, activeIndex, onSelect, onClose,
}: {
  tabs: OpenTab[]
  activeIndex: number
  onSelect: (index: number) => void
  onClose: (index: number) => void
}) {
  if (tabs.length === 0) return null
  return (
    <div className="flex items-center gap-0 overflow-x-auto border-b border-rule bg-surface-950">
      {tabs.map((tab, i) => (
        <div
          key={tab.path}
          className={[
            'group flex items-center gap-2 border-r border-rule px-3 py-1.5 text-[13px] font-mono cursor-pointer transition-colors',
            i === activeIndex
              ? 'bg-surface-900 text-fg border-b-2 border-b-accent'
              : 'text-muted hover:bg-surface-800 hover:text-fg',
          ].join(' ')}
          onClick={() => onSelect(i)}
        >
          <span className="text-[11px] text-dim">{fileIcon(tab.language)}</span>
          <span className="truncate max-w-[140px]">{tab.label}</span>
          <button
            onClick={(e) => { e.stopPropagation(); onClose(i) }}
            className="ml-1 text-dim opacity-0 transition-opacity hover:text-fg group-hover:opacity-100"
            title="Close"
          >
            ×
          </button>
        </div>
      ))}
    </div>
  )
}

// ─── Main Component ──────────────────────────────────────────────────────────

export default function CodeBrowser() {
  const { get } = useApi()
  const [searchParams] = useSearchParams()
  const editorRef = useRef<editor.IStandaloneCodeEditor | null>(null)
  const monacoRef = useRef<Monaco | null>(null)
  const disposablesRef = useRef<{ dispose: () => void }[]>([])
  const pendingLineRef = useRef<number | null>(null)
  const lastDeepLink = useRef('')

  // Selector state
  const [products, setProducts] = useState<Product[]>([])
  const [repos, setRepos] = useState<Repository[]>([])
  const [branches, setBranches] = useState<string[]>([])
  const [selectedProductId, setSelectedProductId] = useState('')
  const [selectedRepoId, setSelectedRepoId] = useState('')
  const [selectedBranch, setSelectedBranch] = useState('')

  // Tree state
  const [rootEntries, setRootEntries] = useState<TreeEntry[]>([])
  const [childrenMap, setChildrenMap] = useState<Map<string, TreeEntry[]>>(new Map())
  const [expanded, setExpanded] = useState<Set<string>>(new Set())
  const [loadingPaths, setLoadingPaths] = useState<Set<string>>(new Set())
  const [treeLoading, setTreeLoading] = useState(false)
  const [allFiles, setAllFiles] = useState<TreeEntry[]>([])

  // Tabs state
  const [tabs, setTabs] = useState<OpenTab[]>([])
  const [activeTabIndex, setActiveTabIndex] = useState(-1)
  const [fileLoading, setFileLoading] = useState(false)

  // Codex state
  const [codexPatterns, setCodexPatterns] = useState<CodexPatternEntry[]>([])
  const [codexModules, setCodexModules] = useState<CodexModuleEntry[]>([])
  const [codexSections, setCodexSections] = useState<CodexSectionEntry[]>([])
  const [codexPanelOpen, setCodexPanelOpen] = useState(true)

  // Quick open state
  const [quickOpenVisible, setQuickOpenVisible] = useState(false)
  const [quickOpenQuery, setQuickOpenQuery] = useState('')

  const [error, setError] = useState<string | null>(null)

  const activeTab = activeTabIndex >= 0 && activeTabIndex < tabs.length ? tabs[activeTabIndex] : null

  // Track all file entries for Quick Open search
  const collectAllFiles = useCallback((root: TreeEntry[], children: Map<string, TreeEntry[]>) => {
    const files: TreeEntry[] = []
    for (const e of root) {
      if (e.type === 'blob') files.push(e)
    }
    for (const entries of children.values()) {
      for (const e of entries) {
        if (e.type === 'blob') files.push(e)
      }
    }
    setAllFiles(files)
  }, [])

  // ── Keyboard shortcuts ─────────────────────────────────────────────────────

  useEffect(() => {
    function onKeyDown(e: KeyboardEvent) {
      if ((e.ctrlKey || e.metaKey) && e.key === 'p') {
        e.preventDefault()
        setQuickOpenVisible(v => !v)
        setQuickOpenQuery('')
      }
      if (e.key === 'Escape') {
        setQuickOpenVisible(false)
      }
    }
    window.addEventListener('keydown', onKeyDown)
    return () => window.removeEventListener('keydown', onKeyDown)
  }, [])

  // ── Fetch products ─────────────────────────────────────────────────────────

  useEffect(() => {
    let cancelled = false
    const urlProduct = searchParams.get('product')
    get<Product[]>('/admin/products')
      .then((data) => {
        if (cancelled) return
        setProducts(data)
        const match = urlProduct && data.find(p => p.id === urlProduct || p.name.toLowerCase() === urlProduct.toLowerCase())
        setSelectedProductId(match ? match.id : data.length > 0 ? data[0].id : '')
      })
      .catch((err: Error) => !cancelled && setError(err.message))
    return () => { cancelled = true }
  }, [get, searchParams])

  // ── Fetch codex data for the selected product ──────────────────────────────

  useEffect(() => {
    if (!selectedProductId) {
      setCodexPatterns([]); setCodexModules([]); setCodexSections([])
      return
    }
    let cancelled = false
    const pid = selectedProductId
    Promise.all([
      get<CodexPatternEntry[]>(`/codex/patterns?product_id=${pid}`).catch(() => [] as CodexPatternEntry[]),
      get<CodexModuleEntry[]>(`/codex/modules?product_id=${pid}`).catch(() => [] as CodexModuleEntry[]),
      get<CodexSectionEntry[]>(`/codex/sections?product_id=${pid}`).catch(() => [] as CodexSectionEntry[]),
    ]).then(([patterns, modules, sections]) => {
      if (cancelled) return
      setCodexPatterns(patterns)
      setCodexModules(modules)
      setCodexSections(sections)
    })
    return () => { cancelled = true }
  }, [selectedProductId, get])

  // ── Fetch repos ────────────────────────────────────────────────────────────

  useEffect(() => {
    if (!selectedProductId) { setRepos([]); setSelectedRepoId(''); return }
    let cancelled = false
    setError(null)
    setRepos([]); setSelectedRepoId(''); setBranches([]); setSelectedBranch('')
    resetTree()
    const urlRepo = searchParams.get('repo')
    get<Repository[]>(`/admin/repositories?product_id=${selectedProductId}`)
      .then((data) => {
        if (cancelled) return
        setRepos(data)
        const match = urlRepo && data.find(r => r.id === urlRepo || r.name.toLowerCase() === urlRepo.toLowerCase())
        setSelectedRepoId(match ? match.id : data.length > 0 ? data[0].id : '')
      })
      .catch((err: Error) => !cancelled && setError(err.message))
    return () => { cancelled = true }
  }, [selectedProductId, get])

  // ── Fetch branches ─────────────────────────────────────────────────────────

  useEffect(() => {
    if (!selectedRepoId) { setBranches([]); setSelectedBranch(''); return }
    let cancelled = false
    setError(null)
    setBranches([]); setSelectedBranch('')
    resetTree()
    const urlBranch = searchParams.get('branch')
    get<BranchesResponse>(`/repos/branches?repo_id=${selectedRepoId}`)
      .then((data) => {
        if (cancelled) return
        setBranches(data.branches)
        const match = urlBranch && data.branches.includes(urlBranch)
        setSelectedBranch(match ? urlBranch : data.default_branch)
      })
      .catch((err: Error) => !cancelled && setError(err.message))
    return () => { cancelled = true }
  }, [selectedRepoId, get])

  // ── Fetch root tree ────────────────────────────────────────────────────────

  useEffect(() => {
    if (!selectedRepoId || !selectedBranch) { resetTree(); return }
    let cancelled = false
    setTreeLoading(true)
    resetTree()
    get<TreeResponse>(`/repos/tree?repo_id=${selectedRepoId}&ref=${encodeURIComponent(selectedBranch)}`)
      .then((data) => {
        if (cancelled) return
        setRootEntries(data.entries)
        collectAllFiles(data.entries, new Map())
      })
      .catch((err: Error) => !cancelled && setError(err.message))
      .finally(() => !cancelled && setTreeLoading(false))
    return () => { cancelled = true }
  }, [selectedRepoId, selectedBranch, get, collectAllFiles])

  function resetTree() {
    setRootEntries([]); setChildrenMap(new Map()); setExpanded(new Set())
    setLoadingPaths(new Set()); setAllFiles([])
    setTabs([]); setActiveTabIndex(-1)
  }

  // ── Subtree fetch ──────────────────────────────────────────────────────────

  const fetchSubtree = useCallback(async (dirPath: string) => {
    if (!selectedRepoId || !selectedBranch) return
    setLoadingPaths(prev => new Set(prev).add(dirPath))
    try {
      const data = await get<TreeResponse>(
        `/repos/tree?repo_id=${selectedRepoId}&ref=${encodeURIComponent(selectedBranch)}&path=${encodeURIComponent(dirPath)}`,
      )
      setChildrenMap(prev => {
        const next = new Map(prev)
        next.set(dirPath, data.entries)
        collectAllFiles(rootEntries, next)
        return next
      })
    } catch (err: unknown) {
      if (err instanceof Error) setError(err.message)
    } finally {
      setLoadingPaths(prev => {
        const next = new Set(prev)
        next.delete(dirPath)
        return next
      })
    }
  }, [selectedRepoId, selectedBranch, get, rootEntries, collectAllFiles])

  const handleToggleDir = useCallback((dirPath: string) => {
    setExpanded(prev => {
      const next = new Set(prev)
      if (next.has(dirPath)) {
        next.delete(dirPath)
      } else {
        next.add(dirPath)
        if (!childrenMap.has(dirPath)) fetchSubtree(dirPath)
      }
      return next
    })
  }, [childrenMap, fetchSubtree])

  // ── Open file (tab management) ─────────────────────────────────────────────

  const revealLine = useCallback((line: number) => {
    const ed = editorRef.current
    if (!ed || line < 1) return
    ed.revealLineInCenter(line)
    ed.setSelection({ startLineNumber: line, startColumn: 1, endLineNumber: line, endColumn: 1 })
    ed.createDecorationsCollection([{
      range: { startLineNumber: line, startColumn: 1, endLineNumber: line, endColumn: 1 },
      options: {
        isWholeLine: true,
        className: 'highlighted-line',
        glyphMarginClassName: 'highlighted-glyph',
      },
    }])
  }, [])

  const openFile = useCallback(async (filePath: string, line?: number) => {
    // Check if already open
    const existingIdx = tabs.findIndex(t => t.path === filePath)
    if (existingIdx >= 0) {
      setActiveTabIndex(existingIdx)
      if (line) {
        setTimeout(() => revealLine(line), 50)
      }
      return
    }

    if (!selectedRepoId || !selectedBranch) return
    setFileLoading(true)
    try {
      const blob = await get<BlobResponse>(
        `/repos/blob?repo_id=${selectedRepoId}&ref=${encodeURIComponent(selectedBranch)}&path=${encodeURIComponent(filePath)}`,
      )
      const lang = detectLanguage(filePath)
      const label = filePath.split('/').pop() || filePath
      const newTab: OpenTab = {
        path: filePath,
        label,
        content: blob.binary ? '' : blob.content,
        language: lang,
        binary: blob.binary,
        size: blob.size,
      }
      if (line) pendingLineRef.current = line
      setTabs(prev => {
        const next = [...prev, newTab]
        setActiveTabIndex(next.length - 1)
        return next
      })

      // Expand parent directories so the file is visible in the tree
      const segments = filePath.split('/')
      for (let i = 1; i < segments.length; i++) {
        const dirPath = segments.slice(0, i).join('/')
        setExpanded(prev => {
          const next = new Set(prev)
          next.add(dirPath)
          if (!childrenMap.has(dirPath)) fetchSubtree(dirPath)
          return next
        })
      }
    } catch (err: unknown) {
      if (err instanceof Error) setError(err.message)
    } finally {
      setFileLoading(false)
    }
  }, [tabs, selectedRepoId, selectedBranch, get, childrenMap, fetchSubtree, revealLine])

  // ── Deep-link: open file from URL params after tree loads ─────────────────

  useEffect(() => {
    if (treeLoading || rootEntries.length === 0) return
    const urlFile = searchParams.get('file')
    if (!urlFile) return
    const urlLine = searchParams.get('line') || ''
    const key = `${urlFile}:${urlLine}`
    if (key === lastDeepLink.current) return
    lastDeepLink.current = key
    const line = parseInt(urlLine, 10)
    openFile(urlFile, line > 0 ? line : undefined)
  }, [treeLoading, rootEntries, searchParams, openFile])

  const closeTab = useCallback((index: number) => {
    setTabs(prev => {
      const next = [...prev]
      next.splice(index, 1)
      setActiveTabIndex(current => {
        if (next.length === 0) return -1
        if (current >= next.length) return next.length - 1
        if (current > index) return current - 1
        if (current === index) return Math.min(current, next.length - 1)
        return current
      })
      return next
    })
  }, [])

  // ── Monaco setup: theme + import navigation ────────────────────────────────

  function handleEditorMount(ed: editor.IStandaloneCodeEditor, monaco: Monaco) {
    editorRef.current = ed
    monacoRef.current = monaco

    if (pendingLineRef.current) {
      const line = pendingLineRef.current
      pendingLineRef.current = null
      setTimeout(() => revealLine(line), 100)
    }

    // Define AE dark theme matching our design system
    monaco.editor.defineTheme('ae-dark', {
      base: 'vs-dark',
      inherit: true,
      rules: [
        { token: 'comment', foreground: '6b7280', fontStyle: 'italic' },
        { token: 'keyword', foreground: 'c084fc' },
        { token: 'string', foreground: '86efac' },
        { token: 'number', foreground: 'fbbf24' },
        { token: 'type', foreground: '67e8f9' },
        { token: 'function', foreground: '93c5fd' },
        { token: 'variable', foreground: 'e2e8f0' },
        { token: 'operator', foreground: 'f472b6' },
        { token: 'delimiter', foreground: '94a3b8' },
        { token: 'tag', foreground: 'f87171' },
        { token: 'attribute.name', foreground: 'fbbf24' },
        { token: 'attribute.value', foreground: '86efac' },
      ],
      colors: {
        'editor.background': '#0a0c10',
        'editor.foreground': '#e2e8f0',
        'editor.lineHighlightBackground': '#1a1d27',
        'editor.selectionBackground': '#c8fc5b30',
        'editor.selectionHighlightBackground': '#c8fc5b15',
        'editorCursor.foreground': '#c8fc5b',
        'editorLineNumber.foreground': '#4a5568',
        'editorLineNumber.activeForeground': '#94a3b8',
        'editorIndentGuide.background': '#1e293b',
        'editorIndentGuide.activeBackground': '#334155',
        'editor.findMatchBackground': '#c8fc5b40',
        'editor.findMatchHighlightBackground': '#c8fc5b20',
        'editorBracketMatch.background': '#c8fc5b20',
        'editorBracketMatch.border': '#c8fc5b60',
        'editorGutter.background': '#0a0c10',
        'editorWidget.background': '#0f1117',
        'editorWidget.border': '#1e293b',
        'editorOverviewRuler.border': '#1e293b',
        'minimap.background': '#0a0c10',
        'scrollbarSlider.background': '#334155',
        'scrollbarSlider.hoverBackground': '#475569',
        'scrollbarSlider.activeBackground': '#64748b',
      },
    })
    monaco.editor.setTheme('ae-dark')

    registerImportNavigation(monaco)
  }

  function registerImportNavigation(monaco: Monaco) {
    // Dispose previous registrations
    for (const d of disposablesRef.current) d.dispose()
    disposablesRef.current = []

    const languages = [
      'typescript', 'javascript', 'go', 'python', 'rust', 'ruby',
      'java', 'kotlin', 'scala', 'csharp', 'cpp', 'c', 'php',
    ]

    for (const lang of languages) {
      const d = monaco.languages.registerDefinitionProvider(lang, {
        provideDefinition(model: editor.ITextModel, position: { lineNumber: number; column: number }) {
          const line = model.getLineContent(position.lineNumber)
          const imports = extractImportsFromLine(line)

          for (const imp of imports) {
            if (position.column >= imp.startCol && position.column <= imp.endCol) {
              const currentPath = activeTab?.path || ''
              const resolved = resolveImportPath(imp.path, currentPath, rootEntries, childrenMap)
              if (resolved) {
                openFile(resolved)
                return []
              }
            }
          }
          return []
        },
      })
      disposablesRef.current.push(d)

      // Register a link provider for visual underline hints
      const linkDisposable = monaco.languages.registerLinkProvider(lang, {
        provideLinks(model: editor.ITextModel) {
          const links: { range: { startLineNumber: number; startColumn: number; endLineNumber: number; endColumn: number }; tooltip: string }[] = []
          for (let i = 1; i <= model.getLineCount(); i++) {
            const line = model.getLineContent(i)
            const imports = extractImportsFromLine(line)
            for (const imp of imports) {
              const currentPath = activeTab?.path || ''
              const resolved = resolveImportPath(imp.path, currentPath, rootEntries, childrenMap)
              if (resolved) {
                links.push({
                  range: {
                    startLineNumber: i,
                    startColumn: imp.startCol,
                    endLineNumber: i,
                    endColumn: imp.endCol,
                  },
                  tooltip: `Go to ${resolved}`,
                })
              }
            }
          }
          return { links }
        },
      })
      disposablesRef.current.push(linkDisposable)
    }
  }

  // Re-register navigation when tree data or active tab changes
  useEffect(() => {
    if (monacoRef.current) {
      registerImportNavigation(monacoRef.current)
    }
    // Cleanup on unmount
    return () => {
      for (const d of disposablesRef.current) d.dispose()
      disposablesRef.current = []
    }
  }, [rootEntries, childrenMap, activeTab?.path]) // eslint-disable-line react-hooks/exhaustive-deps

  // ── Quick Open filtering ───────────────────────────────────────────────────

  const quickOpenResults = quickOpenQuery
    ? allFiles.filter(f => {
        const q = quickOpenQuery.toLowerCase()
        return f.name.toLowerCase().includes(q) || f.path.toLowerCase().includes(q)
      })
    : []

  // ── Breadcrumb ─────────────────────────────────────────────────────────────

  function renderBreadcrumb(filePath: string) {
    const segments = filePath.split('/')
    return (
      <div className="flex items-center gap-1 border-b border-rule bg-surface-950 px-4 py-1.5 font-mono text-[13px]">
        {segments.map((seg, idx) => {
          const isLast = idx === segments.length - 1
          const path = segments.slice(0, idx + 1).join('/')
          return (
            <span key={path} className="flex items-center gap-1">
              {idx > 0 && <span className="text-dim">/</span>}
              {isLast ? (
                <span className="text-fg">{seg}</span>
              ) : (
                <button
                  onClick={() => handleToggleDir(path)}
                  className="text-muted transition-colors hover:text-accent"
                >
                  {seg}
                </button>
              )}
            </span>
          )
        })}
        <span className="ml-auto text-[11px] text-dim">
          {activeTab?.language} {activeTab?.size ? `· ${(activeTab.size / 1024).toFixed(1)} KB` : ''}
        </span>
      </div>
    )
  }

  // ── Viewer panel ───────────────────────────────────────────────────────────

  function renderViewer() {
    if (tabs.length === 0) {
      return (
        <div className="flex flex-1 flex-col items-center justify-center gap-3">
          <p className="text-sm text-dim">Select a file to view</p>
          <p className="text-xs text-dim">
            <kbd className="rounded border border-rule bg-surface-800 px-1.5 py-0.5 font-mono text-[11px]">Ctrl+P</kbd>
            {' '}to search files
          </p>
        </div>
      )
    }

    if (fileLoading && !activeTab) {
      return (
        <div className="flex flex-1 items-center justify-center">
          <p className="text-sm text-dim">Loading…</p>
        </div>
      )
    }

    if (!activeTab) return null

    if (activeTab.binary) {
      return (
        <div className="flex flex-col flex-1">
          {renderBreadcrumb(activeTab.path)}
          <div className="flex flex-1 items-center justify-center">
            <p className="text-sm text-dim">
              Binary file ({activeTab.size ? `${(activeTab.size / 1024).toFixed(1)} KB` : 'unknown size'})
            </p>
          </div>
        </div>
      )
    }

    return (
      <div className="flex flex-col flex-1 overflow-hidden">
        {renderBreadcrumb(activeTab.path)}
        <div className="flex-1">
          <Editor
            language={activeTab.language}
            value={activeTab.content}
            theme="ae-dark"
            onMount={handleEditorMount}
            options={{
              readOnly: true,
              minimap: { enabled: true, scale: 2, showSlider: 'mouseover' },
              scrollBeyondLastLine: false,
              fontSize: 13,
              fontFamily: "'Geist Mono', 'JetBrains Mono', 'Fira Code', ui-monospace, monospace",
              fontLigatures: true,
              lineNumbers: 'on',
              renderLineHighlight: 'line',
              cursorStyle: 'line',
              smoothScrolling: true,
              padding: { top: 8, bottom: 8 },
              bracketPairColorization: { enabled: true },
              guides: { bracketPairs: true, indentation: true },
              wordWrap: 'off',
              folding: true,
              foldingStrategy: 'indentation',
              showFoldingControls: 'mouseover',
              links: true,
              renderWhitespace: 'selection',
              matchBrackets: 'always',
              occurrencesHighlight: 'singleFile',
              selectOnLineNumbers: true,
              colorDecorators: true,
              overviewRulerBorder: false,
              contextmenu: true,
              quickSuggestions: false,
              suggest: { showWords: false },
            }}
          />
        </div>
      </div>
    )
  }

  // ── Select styling ─────────────────────────────────────────────────────────

  const selectClass =
    'rounded-sm border border-rule bg-surface-900 px-3 py-1.5 text-sm text-fg focus:border-accent focus:outline-none focus:ring-1 focus:ring-accent'

  // ── Render ─────────────────────────────────────────────────────────────────

  return (
    <div className="relative flex flex-col h-full -m-6 overflow-hidden">
      {/* Quick Open overlay */}
      <QuickOpen
        visible={quickOpenVisible}
        query={quickOpenQuery}
        onQueryChange={setQuickOpenQuery}
        results={quickOpenResults}
        onSelect={openFile}
        onClose={() => setQuickOpenVisible(false)}
      />

      {/* Error banner */}
      {error && (
        <div className="border-b border-rule bg-surface-950 px-4 py-2">
          <p className="text-xs text-red-400">
            {error}
            <button onClick={() => setError(null)} className="ml-2 text-dim hover:text-fg">dismiss</button>
          </p>
        </div>
      )}

      {/* Top selectors bar */}
      <div className="flex items-center gap-3 border-b border-rule bg-surface-950 px-4 py-2">
        <label className="flex items-center gap-2">
          <span className="text-xs font-medium text-muted">Product</span>
          <select className={selectClass} value={selectedProductId} onChange={(e) => setSelectedProductId(e.target.value)}>
            {products.length === 0 && <option value="">Loading…</option>}
            {products.map((p) => <option key={p.id} value={p.id}>{p.name}</option>)}
          </select>
        </label>
        <label className="flex items-center gap-2">
          <span className="text-xs font-medium text-muted">Repository</span>
          <select className={selectClass} value={selectedRepoId} onChange={(e) => setSelectedRepoId(e.target.value)} disabled={repos.length === 0}>
            {repos.length === 0 && <option value="">--</option>}
            {repos.map((r) => <option key={r.id} value={r.id}>{r.name}</option>)}
          </select>
        </label>
        <label className="flex items-center gap-2">
          <span className="text-xs font-medium text-muted">Branch</span>
          <select className={selectClass} value={selectedBranch} onChange={(e) => setSelectedBranch(e.target.value)} disabled={branches.length === 0}>
            {branches.length === 0 && <option value="">--</option>}
            {branches.map((b) => <option key={b} value={b}>{b}</option>)}
          </select>
        </label>
        <div className="ml-auto flex items-center gap-2">
          <button
            onClick={() => { setQuickOpenVisible(true); setQuickOpenQuery('') }}
            className="flex items-center gap-2 rounded-sm border border-rule bg-surface-900 px-3 py-1.5 text-xs text-muted transition-colors hover:bg-surface-800 hover:text-fg"
          >
            <span>Search files</span>
            <kbd className="rounded border border-rule/60 bg-surface-800 px-1 py-0.5 font-mono text-[10px]">Ctrl+P</kbd>
          </button>
          <button
            onClick={() => setCodexPanelOpen(v => !v)}
            className={[
              'flex items-center gap-1.5 rounded-sm border px-3 py-1.5 text-xs transition-colors',
              codexPanelOpen
                ? 'border-accent/40 bg-accent/10 text-accent'
                : 'border-rule bg-surface-900 text-muted hover:bg-surface-800 hover:text-fg',
            ].join(' ')}
            title="Toggle Codex context panel"
          >
            <span className="text-[11px]">⊡</span>
            <span>Codex</span>
            {(codexPatterns.length + codexModules.length + codexSections.length) > 0 && (
              <span className="rounded-full bg-accent/20 px-1.5 text-[10px] text-accent">
                {codexPatterns.length + codexModules.length}
              </span>
            )}
          </button>
        </div>
      </div>

      {/* Main content */}
      <div className="flex flex-1 overflow-hidden">
        {/* File tree sidebar */}
        <div className="w-60 shrink-0 overflow-y-auto border-r border-rule bg-surface-950">
          <div className="px-3 py-2 border-b border-rule/40">
            <p className="text-xs font-medium text-muted">EXPLORER</p>
          </div>
          {treeLoading ? (
            <div className="px-3 py-4"><p className="text-xs text-dim">Loading tree…</p></div>
          ) : rootEntries.length === 0 ? (
            <div className="px-3 py-4">
              <p className="text-xs text-dim">
                {selectedRepoId && selectedBranch ? 'No files found' : 'Select a repository and branch'}
              </p>
            </div>
          ) : (
            <div className="pb-4">
              {sortEntries(rootEntries).map((entry) => (
                <FileTreeNode
                  key={entry.path}
                  entry={entry}
                  depth={0}
                  expanded={expanded}
                  childrenMap={childrenMap}
                  activePath={activeTab?.path || null}
                  loadingPaths={loadingPaths}
                  onToggleDir={handleToggleDir}
                  onSelectFile={openFile}
                />
              ))}
            </div>
          )}
        </div>

        {/* Editor area */}
        <div className="flex flex-1 flex-col overflow-hidden">
          <TabBar
            tabs={tabs}
            activeIndex={activeTabIndex}
            onSelect={setActiveTabIndex}
            onClose={closeTab}
          />
          {renderViewer()}
        </div>

        {/* Codex context panel */}
        {codexPanelOpen && (
          <div className="w-72 shrink-0 overflow-hidden border-l border-rule bg-surface-950 flex flex-col">
            <div className="flex items-center justify-between px-3 py-2 border-b border-rule/40">
              <p className="text-[11px] font-semibold text-muted tracking-wide">CODEX</p>
              <button
                onClick={() => setCodexPanelOpen(false)}
                className="text-dim hover:text-fg text-xs"
                title="Close Codex panel"
              >✕</button>
            </div>
            <div className="flex-1 overflow-y-auto">
              <CodexPanel
                patterns={codexPatterns}
                modules={codexModules}
                sections={codexSections}
                filePath={activeTab?.path || null}
                onNavigateFile={openFile}
              />
            </div>
          </div>
        )}
      </div>
    </div>
  )
}
