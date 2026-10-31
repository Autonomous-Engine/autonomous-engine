import { useState, useEffect, useCallback } from 'react'
import { useApi } from '@/hooks/useApi'

// ─── Types ───────────────────────────────────────────────────────────────────

interface Product {
  id: string
  name: string
}

interface Repository {
  id: string
  name: string
  base_branch: string
}

interface BranchesResponse {
  branches: string[]
  default_branch: string
}

interface CommitSummary {
  sha: string
  author_name: string
  author_email: string
  date: string
  message: string
}

interface CommitListResponse {
  commits: CommitSummary[]
  total: number
  page: number
  per_page: number
}

interface CommitDetail {
  sha: string
  author_name: string
  author_email: string
  date: string
  message: string
  body: string
  stats: Record<string, unknown>
  diff: string
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

function relativeDate(iso: string): string {
  const d = new Date(iso)
  const now = Date.now()
  const diff = now - d.getTime()
  const mins = Math.floor(diff / 60000)
  if (mins < 60) return `${mins}m ago`
  const hrs = Math.floor(mins / 60)
  if (hrs < 24) return `${hrs}h ago`
  const days = Math.floor(hrs / 24)
  if (days < 30) return `${days}d ago`
  return d.toLocaleDateString()
}

function renderDiff(diff: string) {
  const lines = diff.split('\n')
  return (
    <pre className="overflow-x-auto font-mono text-xs">
      {lines.map((line, i) => {
        let className = 'px-4 '
        if (line.startsWith('diff --git'))
          className += 'text-muted font-semibold mt-2 border-t border-rule/40 pt-1'
        else if (line.startsWith('@@'))
          className += 'text-blue-400 bg-blue-950/20'
        else if (line.startsWith('+'))
          className += 'text-emerald-400 bg-emerald-950/30'
        else if (line.startsWith('-'))
          className += 'text-red-400 bg-red-950/30'
        else className += 'text-dim'
        return (
          <div key={i} className={className}>
            {line || ' '}
          </div>
        )
      })}
    </pre>
  )
}

// ─── Select Component ────────────────────────────────────────────────────────

const selectClass =
  'rounded-sm border border-rule bg-surface-900 px-3 py-1.5 text-sm text-fg focus:border-accent focus:outline-none focus:ring-1 focus:ring-accent'

// ─── CommitBrowser ───────────────────────────────────────────────────────────

export default function CommitBrowser() {
  const { get } = useApi()

  // Selector state
  const [products, setProducts] = useState<Product[]>([])
  const [repos, setRepos] = useState<Repository[]>([])
  const [branches, setBranches] = useState<string[]>([])

  const [selectedProduct, setSelectedProduct] = useState<string>('')
  const [selectedRepo, setSelectedRepo] = useState<string>('')
  const [selectedBranch, setSelectedBranch] = useState<string>('')

  // Commit list state
  const [commits, setCommits] = useState<CommitSummary[]>([])
  const [totalCommits, setTotalCommits] = useState(0)
  const [page, setPage] = useState(1)
  const [perPage] = useState(30)
  const [loadingCommits, setLoadingCommits] = useState(false)

  // Expanded commit state
  const [expandedSha, setExpandedSha] = useState<string | null>(null)
  const [commitDetail, setCommitDetail] = useState<CommitDetail | null>(null)
  const [loadingDetail, setLoadingDetail] = useState(false)

  const totalPages = Math.max(1, Math.ceil(totalCommits / perPage))

  // ── Fetch products on mount ──────────────────────────────────────────────

  useEffect(() => {
    let cancelled = false
    get<Product[]>('/admin/products').then((data) => {
      if (cancelled) return
      setProducts(data)
      if (data.length > 0) {
        setSelectedProduct(data[0].id)
      }
    }).catch(() => {})
    return () => { cancelled = true }
  }, [get])

  // ── Fetch repos when product changes ─────────────────────────────────────

  useEffect(() => {
    if (!selectedProduct) {
      setRepos([])
      setSelectedRepo('')
      return
    }
    let cancelled = false
    get<Repository[]>(`/admin/repositories?product_id=${selectedProduct}`).then((data) => {
      if (cancelled) return
      setRepos(data)
      if (data.length > 0) {
        setSelectedRepo(data[0].id)
      } else {
        setSelectedRepo('')
      }
    }).catch(() => {})
    return () => { cancelled = true }
  }, [selectedProduct, get])

  // ── Fetch branches when repo changes ─────────────────────────────────────

  useEffect(() => {
    if (!selectedRepo) {
      setBranches([])
      setSelectedBranch('')
      return
    }
    let cancelled = false
    get<BranchesResponse>(`/repos/branches?repo_id=${selectedRepo}`).then((data) => {
      if (cancelled) return
      setBranches(data.branches)
      setSelectedBranch(data.default_branch)
    }).catch(() => {})
    return () => { cancelled = true }
  }, [selectedRepo, get])

  // ── Fetch commits when branch/page changes ───────────────────────────────

  const fetchCommits = useCallback(async () => {
    if (!selectedRepo || !selectedBranch) {
      setCommits([])
      setTotalCommits(0)
      return
    }
    setLoadingCommits(true)
    try {
      const data = await get<CommitListResponse>(
        `/repos/commits?repo_id=${selectedRepo}&ref=${encodeURIComponent(selectedBranch)}&page=${page}&per_page=${perPage}`,
      )
      setCommits(data.commits)
      setTotalCommits(data.total)
    } catch {
      setCommits([])
      setTotalCommits(0)
    } finally {
      setLoadingCommits(false)
    }
  }, [selectedRepo, selectedBranch, page, perPage, get])

  useEffect(() => {
    fetchCommits()
  }, [fetchCommits])

  // Reset page when branch changes
  useEffect(() => {
    setPage(1)
    setExpandedSha(null)
    setCommitDetail(null)
  }, [selectedBranch])

  // ── Toggle expanded commit ───────────────────────────────────────────────

  async function toggleCommit(sha: string) {
    if (expandedSha === sha) {
      setExpandedSha(null)
      setCommitDetail(null)
      return
    }
    setExpandedSha(sha)
    setCommitDetail(null)
    setLoadingDetail(true)
    try {
      const detail = await get<CommitDetail>(
        `/repos/commit?repo_id=${selectedRepo}&sha=${sha}`,
      )
      setCommitDetail(detail)
    } catch {
      setCommitDetail(null)
    } finally {
      setLoadingDetail(false)
    }
  }

  // ── Render ───────────────────────────────────────────────────────────────

  return (
    <div className="flex flex-col h-full">
      {/* Top selectors */}
      <div className="flex items-center gap-3 border-b border-rule bg-surface-950 px-4 py-3">
        <div className="flex items-center gap-2">
          <label className="text-xs font-medium text-muted">Product</label>
          <select
            className={selectClass}
            value={selectedProduct}
            onChange={(e) => setSelectedProduct(e.target.value)}
          >
            {products.map((p) => (
              <option key={p.id} value={p.id}>
                {p.name}
              </option>
            ))}
          </select>
        </div>

        <div className="flex items-center gap-2">
          <label className="text-xs font-medium text-muted">Repo</label>
          <select
            className={selectClass}
            value={selectedRepo}
            onChange={(e) => setSelectedRepo(e.target.value)}
          >
            {repos.map((r) => (
              <option key={r.id} value={r.id}>
                {r.name}
              </option>
            ))}
          </select>
        </div>

        <div className="flex items-center gap-2">
          <label className="text-xs font-medium text-muted">Branch</label>
          <select
            className={selectClass}
            value={selectedBranch}
            onChange={(e) => setSelectedBranch(e.target.value)}
          >
            {branches.map((b) => (
              <option key={b} value={b}>
                {b}
              </option>
            ))}
          </select>
        </div>
      </div>

      {/* Commit list */}
      <div className="flex-1 overflow-y-auto">
        {loadingCommits ? (
          <div className="flex items-center justify-center py-16">
            <span className="font-mono text-sm text-muted">Loading commits...</span>
          </div>
        ) : commits.length === 0 ? (
          <div className="flex items-center justify-center py-16">
            <span className="font-mono text-sm text-dim">No commits found</span>
          </div>
        ) : (
          <>
            {commits.map((commit, idx) => (
              <div key={commit.sha}>
                {/* Commit row */}
                <button
                  type="button"
                  onClick={() => toggleCommit(commit.sha)}
                  className={[
                    'flex w-full items-center gap-4 px-4 py-2.5 text-left transition-colors',
                    idx % 2 === 1 ? 'bg-surface-950/50' : '',
                    expandedSha === commit.sha
                      ? 'bg-accent/5'
                      : 'hover:bg-surface-800',
                  ].join(' ')}
                >
                  <span className="font-mono text-sm text-accent shrink-0">
                    {commit.sha.slice(0, 7)}
                  </span>
                  <span className="text-sm text-fg truncate flex-1">
                    {commit.message}
                  </span>
                  <span className="text-xs text-muted shrink-0">
                    {commit.author_name}
                  </span>
                  <span className="font-mono text-xs text-dim shrink-0">
                    {relativeDate(commit.date)}
                  </span>
                </button>

                {/* Expanded detail */}
                {expandedSha === commit.sha && (
                  <div className="border-y border-rule/40 bg-surface-950 px-4 py-4">
                    {loadingDetail ? (
                      <div className="flex items-center justify-center py-8">
                        <span className="font-mono text-sm text-muted">
                          Loading diff...
                        </span>
                      </div>
                    ) : commitDetail ? (
                      <div className="space-y-4">
                        {/* Full message + body */}
                        <div>
                          <p className="text-sm font-semibold text-fg">
                            {commitDetail.message}
                          </p>
                          {commitDetail.body && (
                            <pre className="mt-2 whitespace-pre-wrap font-mono text-xs text-muted">
                              {commitDetail.body}
                            </pre>
                          )}
                          <div className="mt-2 flex items-center gap-4 text-xs text-dim">
                            <span>{commitDetail.author_name} &lt;{commitDetail.author_email}&gt;</span>
                            <span>{new Date(commitDetail.date).toLocaleString()}</span>
                            <span className="font-mono text-accent">{commitDetail.sha}</span>
                          </div>
                        </div>

                        {/* Diff stats */}
                        {commitDetail.stats && (
                          <div>
                            <p className="text-xs font-medium text-muted mb-1">Stats</p>
                            <div className="font-mono text-xs text-dim">
                              {Object.entries(commitDetail.stats).map(([key, val]) => (
                                <span key={key} className="mr-4">
                                  {key}: {String(val)}
                                </span>
                              ))}
                            </div>
                          </div>
                        )}

                        {/* Diff */}
                        {commitDetail.diff && (
                          <div>
                            <p className="text-xs font-medium text-muted mb-1">Diff</p>
                            <div className="rounded-sm border border-rule/40 bg-surface-900 overflow-hidden">
                              {renderDiff(commitDetail.diff)}
                            </div>
                          </div>
                        )}

                        {/* Close button */}
                        <div className="flex justify-end">
                          <button
                            type="button"
                            onClick={() => {
                              setExpandedSha(null)
                              setCommitDetail(null)
                            }}
                            className="rounded-sm border border-rule px-3 py-1 text-xs text-muted hover:bg-surface-800 hover:text-fg transition-colors"
                          >
                            Close
                          </button>
                        </div>
                      </div>
                    ) : (
                      <div className="flex items-center justify-center py-8">
                        <span className="font-mono text-sm text-dim">
                          Failed to load commit details
                        </span>
                      </div>
                    )}
                  </div>
                )}
              </div>
            ))}

            {/* Pagination */}
            <div className="flex items-center justify-between border-t border-rule px-4 py-3">
              <button
                type="button"
                disabled={page <= 1}
                onClick={() => setPage((p) => Math.max(1, p - 1))}
                className={[
                  'rounded-sm px-3 py-1 text-xs font-semibold transition-colors',
                  page <= 1
                    ? 'text-dim cursor-not-allowed'
                    : 'bg-accent text-surface-900 hover:bg-accent-hover',
                ].join(' ')}
              >
                Previous
              </button>
              <span className="font-mono text-xs text-muted">
                Page {page} of {totalPages}
              </span>
              <button
                type="button"
                disabled={page >= totalPages}
                onClick={() => setPage((p) => Math.min(totalPages, p + 1))}
                className={[
                  'rounded-sm px-3 py-1 text-xs font-semibold transition-colors',
                  page >= totalPages
                    ? 'text-dim cursor-not-allowed'
                    : 'bg-accent text-surface-900 hover:bg-accent-hover',
                ].join(' ')}
              >
                Next
              </button>
            </div>
          </>
        )}
      </div>
    </div>
  )
}
