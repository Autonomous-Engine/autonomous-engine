// dashboard/src/hooks/useCodexV2.ts
//
// Imperative fetcher hooks for the codex v2 REST endpoints.
// Each hook returns stable async functions via useCallback — no useEffect/useState.

import { useCallback } from 'react'
import { useApi } from './useApi'
import type {
  CodexBlob,
  CodexDiffResponse,
  ContractEdge,
  ContractEdgeFilters,
  GitStrategy,
  CodexSnapshot,
  SnapshotWithBlobs,
  TopologyEntry,
  WatchedBranch,
} from '@/types/codex-v2'

// Re-export types so consumers can import from one place if preferred.
export type {
  CodexBlob,
  CodexDiffResponse,
  ContractEdge,
  ContractEdgeFilters,
  GitStrategy,
  CodexSnapshot,
  SnapshotWithBlobs,
  TopologyEntry,
  WatchedBranch,
}

// ---------------------------------------------------------------------------
// useContractEdges
// ---------------------------------------------------------------------------

export function useContractEdges(): {
  fetchContractEdges(filters: ContractEdgeFilters): Promise<ContractEdge[]>
} {
  const api = useApi()

  const fetchContractEdges = useCallback(
    (filters: ContractEdgeFilters): Promise<ContractEdge[]> => {
      const qs = new URLSearchParams()
      qs.set('product_id', filters.product_id)
      if (filters.contract_type) qs.set('contract_type', filters.contract_type)
      if (filters.producer_repo) qs.set('producer_repo', filters.producer_repo)
      if (filters.consumer_repo) qs.set('consumer_repo', filters.consumer_repo)
      if (filters.limit !== undefined) qs.set('limit', String(filters.limit))
      if (filters.offset !== undefined) qs.set('offset', String(filters.offset))
      return api.get<ContractEdge[]>(`/codex/contract-edges?${qs.toString()}`)
    },
    [api],
  )

  return { fetchContractEdges }
}

// ---------------------------------------------------------------------------
// useTopology
// ---------------------------------------------------------------------------

export function useTopology(): {
  fetchTopology(productId: string): Promise<TopologyEntry[]>
} {
  const api = useApi()

  const fetchTopology = useCallback(
    (productId: string): Promise<TopologyEntry[]> => {
      const qs = new URLSearchParams({ product_id: productId })
      return api.get<TopologyEntry[]>(`/codex/topology?${qs.toString()}`)
    },
    [api],
  )

  return { fetchTopology }
}

// ---------------------------------------------------------------------------
// useSnapshotHistory
// ---------------------------------------------------------------------------

export function useSnapshotHistory(): {
  fetchSnapshots(
    repoId: string,
    branch?: string,
    limit?: number,
  ): Promise<CodexSnapshot[]>
} {
  const api = useApi()

  const fetchSnapshots = useCallback(
    (
      repoId: string,
      branch?: string,
      limit?: number,
    ): Promise<CodexSnapshot[]> => {
      const qs = new URLSearchParams({ repo_id: repoId })
      if (branch) qs.set('branch', branch)
      if (limit !== undefined) qs.set('limit', String(limit))
      return api.get<CodexSnapshot[]>(`/codex/snapshots?${qs.toString()}`)
    },
    [api],
  )

  return { fetchSnapshots }
}

// ---------------------------------------------------------------------------
// useSnapshotDiff
// ---------------------------------------------------------------------------

export function useSnapshotDiff(): {
  fetchDiff(
    repoId: string,
    fromSHA: string,
    toSHA: string,
  ): Promise<CodexDiffResponse>
} {
  const api = useApi()

  const fetchDiff = useCallback(
    (
      repoId: string,
      fromSHA: string,
      toSHA: string,
    ): Promise<CodexDiffResponse> => {
      const qs = new URLSearchParams({
        repo_id: repoId,
        from: fromSHA,
        to: toSHA,
      })
      return api.get<CodexDiffResponse>(`/codex/diff?${qs.toString()}`)
    },
    [api],
  )

  return { fetchDiff }
}

// ---------------------------------------------------------------------------
// useWatchedBranches
// ---------------------------------------------------------------------------

export function useWatchedBranches(): {
  fetchBranches(repoId: string): Promise<WatchedBranch[]>
  addBranch(body: {
    repo_id: string
    branch: string
    source: string
    added_by: string
  }): Promise<void>
  disableBranch(body: { repo_id: string; branch: string }): Promise<void>
} {
  const api = useApi()

  const fetchBranches = useCallback(
    (repoId: string): Promise<WatchedBranch[]> => {
      const qs = new URLSearchParams({ repo_id: repoId })
      return api.get<WatchedBranch[]>(`/codex/watched-branches?${qs.toString()}`)
    },
    [api],
  )

  const addBranch = useCallback(
    (body: {
      repo_id: string
      branch: string
      source: string
      added_by: string
    }): Promise<void> => {
      return api.post<void>('/codex/watched-branches', body)
    },
    [api],
  )

  const disableBranch = useCallback(
    (body: { repo_id: string; branch: string }): Promise<void> => {
      return api.post<void>('/codex/watched-branches/disable', body)
    },
    [api],
  )

  return { fetchBranches, addBranch, disableBranch }
}

// ---------------------------------------------------------------------------
// useGitStrategy
// ---------------------------------------------------------------------------

export function useGitStrategy(): {
  fetchGitStrategy(repoId: string): Promise<GitStrategy>
} {
  const api = useApi()

  const fetchGitStrategy = useCallback(
    (repoId: string): Promise<GitStrategy> => {
      const qs = new URLSearchParams({ repo_id: repoId })
      return api.get<GitStrategy>(`/codex/git-strategy?${qs.toString()}`)
    },
    [api],
  )

  return { fetchGitStrategy }
}

// ---------------------------------------------------------------------------
// useSnapshot
// ---------------------------------------------------------------------------

export function useSnapshot(): {
  fetchSnapshot(repoId: string, commitSha: string): Promise<SnapshotWithBlobs>
} {
  const api = useApi()

  const fetchSnapshot = useCallback(
    (repoId: string, commitSha: string): Promise<SnapshotWithBlobs> => {
      const qs = new URLSearchParams({
        repo_id: repoId,
        commit_sha: commitSha,
      })
      return api.get<SnapshotWithBlobs>(`/codex/snapshot?${qs.toString()}`)
    },
    [api],
  )

  return { fetchSnapshot }
}
