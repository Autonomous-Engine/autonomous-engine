import { useState, useEffect, useCallback } from 'react'
import { useApi } from '@/hooks/useApi'
import type { ConfigEntry } from '@/types'

interface PoolSizes {
  research: number
  implementation: number
  qa: number
}

interface EscalationConfig {
  max_retries: number
  confidence_threshold: number
  state_timeout_minutes: number
}

interface HumanReviewConfig {
  enabled: boolean
}

function findVal(entries: ConfigEntry[], key: string, fallback: string): string {
  return entries.find(e => e.key_name === key)?.value ?? fallback
}

export default function SystemConfig() {
  const { get, put } = useApi()
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState('')

  const [poolSizes, setPoolSizes] = useState<PoolSizes>({ research: 2, implementation: 3, qa: 1 })
  const [escalation, setEscalation] = useState<EscalationConfig>({ max_retries: 3, confidence_threshold: 0.7, state_timeout_minutes: 60 })
  const [humanReview, setHumanReview] = useState<HumanReviewConfig>({ enabled: true })

  const [poolSaving, setPoolSaving] = useState(false)
  const [escalationSaving, setEscalationSaving] = useState(false)
  const [humanReviewSaving, setHumanReviewSaving] = useState(false)

  const [poolSaved, setPoolSaved] = useState(false)
  const [escalationSaved, setEscalationSaved] = useState(false)
  const [humanReviewSaved, setHumanReviewSaved] = useState(false)

  const fetchConfig = useCallback(async () => {
    setLoading(true)
    setError('')
    try {
      const entries = await get<ConfigEntry[]>('/admin/config')
      if (entries?.length) {
        setPoolSizes({
          research: Number(findVal(entries, 'pool_size_research', '2')),
          implementation: Number(findVal(entries, 'pool_size_implementation', '3')),
          qa: Number(findVal(entries, 'pool_size_qa', '1')),
        })
        setEscalation({
          max_retries: Number(findVal(entries, 'max_retries', '3')),
          confidence_threshold: Number(findVal(entries, 'confidence_threshold', '0.7')),
          state_timeout_minutes: Number(findVal(entries, 'state_timeout_minutes', '60')),
        })
        setHumanReview({
          enabled: findVal(entries, 'human_review_enabled', 'true') === 'true',
        })
      }
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Failed to load config')
    } finally {
      setLoading(false)
    }
  }, [get])

  useEffect(() => { fetchConfig() }, [fetchConfig])

  function flashSaved(setter: (v: boolean) => void) {
    setter(true)
    setTimeout(() => setter(false), 2000)
  }

  async function savePoolSizes() {
    setPoolSaving(true)
    setError('')
    try {
      await put('/admin/config/update', [
        { key_name: 'pool_size_research', value: String(poolSizes.research), value_type: 'int', category: 'system', description: 'Research agent pool size', scope: 'global', scope_target: 'default' },
        { key_name: 'pool_size_implementation', value: String(poolSizes.implementation), value_type: 'int', category: 'system', description: 'Implementation agent pool size', scope: 'global', scope_target: 'default' },
        { key_name: 'pool_size_qa', value: String(poolSizes.qa), value_type: 'int', category: 'system', description: 'QA agent pool size', scope: 'global', scope_target: 'default' },
      ])
      flashSaved(setPoolSaved)
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Save failed')
    } finally {
      setPoolSaving(false)
    }
  }

  async function saveEscalation() {
    setEscalationSaving(true)
    setError('')
    try {
      await put('/admin/config/update', [
        { key_name: 'max_retries', value: String(escalation.max_retries), value_type: 'int', category: 'system', description: 'Max retries before escalation', scope: 'global', scope_target: 'default' },
        { key_name: 'confidence_threshold', value: String(escalation.confidence_threshold), value_type: 'int', category: 'system', description: 'Confidence threshold for escalation', scope: 'global', scope_target: 'default' },
        { key_name: 'state_timeout_minutes', value: String(escalation.state_timeout_minutes), value_type: 'int', category: 'system', description: 'State timeout in minutes', scope: 'global', scope_target: 'default' },
      ])
      flashSaved(setEscalationSaved)
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Save failed')
    } finally {
      setEscalationSaving(false)
    }
  }

  async function saveHumanReview() {
    setHumanReviewSaving(true)
    setError('')
    try {
      await put('/admin/config/update', [
        { key_name: 'human_review_enabled', value: String(humanReview.enabled), value_type: 'bool', category: 'system', description: 'Global human review toggle', scope: 'global', scope_target: 'default' },
      ])
      flashSaved(setHumanReviewSaved)
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Save failed')
    } finally {
      setHumanReviewSaving(false)
    }
  }

  function inputClass() {
    return 'w-24 rounded-sm border border-rule bg-surface-950 px-3 py-2 text-sm text-fg focus:outline-none focus:ring-1 focus:ring-accent'
  }

  return (
    <div className="space-y-6">
      <h1 className="text-lg font-semibold text-fg">System Configuration</h1>

      {error && <p className="text-sm text-red-400">{error}</p>}
      {loading && <p className="text-sm text-muted">Loading...</p>}

      {/* Pool Sizes */}
      <div className="rounded-sm border border-rule bg-surface-900 p-5 shadow-sm">
        <h2 className="mb-4 text-sm font-semibold text-fg">Pool Sizes</h2>
        <div className="flex flex-wrap gap-6">
          <div>
            <label className="mb-1 block text-xs text-muted">Research Agents</label>
            <input
              type="number"
              min={1}
              value={poolSizes.research}
              onChange={e => setPoolSizes(p => ({ ...p, research: Number(e.target.value) }))}
              className={inputClass()}
            />
          </div>
          <div>
            <label className="mb-1 block text-xs text-muted">Implementation Agents</label>
            <input
              type="number"
              min={1}
              value={poolSizes.implementation}
              onChange={e => setPoolSizes(p => ({ ...p, implementation: Number(e.target.value) }))}
              className={inputClass()}
            />
          </div>
          <div>
            <label className="mb-1 block text-xs text-muted">QA Agents</label>
            <input
              type="number"
              min={1}
              value={poolSizes.qa}
              onChange={e => setPoolSizes(p => ({ ...p, qa: Number(e.target.value) }))}
              className={inputClass()}
            />
          </div>
        </div>
        <div className="mt-4 flex items-center gap-3">
          <button
            onClick={savePoolSizes}
            disabled={poolSaving}
            className="rounded bg-accent px-3 py-1.5 text-sm font-medium text-surface-900 hover:bg-accent-hover disabled:opacity-50"
          >
            {poolSaving ? 'Saving...' : 'Save'}
          </button>
          {poolSaved && <span className="text-xs text-emerald-400">Saved</span>}
        </div>
      </div>

      {/* Escalation */}
      <div className="rounded-sm border border-rule bg-surface-900 p-5 shadow-sm">
        <h2 className="mb-4 text-sm font-semibold text-fg">Escalation</h2>
        <div className="flex flex-wrap gap-6">
          <div>
            <label className="mb-1 block text-xs text-muted">Max Retries</label>
            <input
              type="number"
              min={0}
              value={escalation.max_retries}
              onChange={e => setEscalation(c => ({ ...c, max_retries: Number(e.target.value) }))}
              className={inputClass()}
            />
          </div>
          <div>
            <label className="mb-1 block text-xs text-muted">Confidence Threshold</label>
            <input
              type="number"
              min={0}
              max={1}
              step={0.05}
              value={escalation.confidence_threshold}
              onChange={e => setEscalation(c => ({ ...c, confidence_threshold: Number(e.target.value) }))}
              className={inputClass()}
            />
          </div>
          <div>
            <label className="mb-1 block text-xs text-muted">State Timeout (minutes)</label>
            <input
              type="number"
              min={1}
              value={escalation.state_timeout_minutes}
              onChange={e => setEscalation(c => ({ ...c, state_timeout_minutes: Number(e.target.value) }))}
              className={inputClass()}
            />
          </div>
        </div>
        <div className="mt-4 flex items-center gap-3">
          <button
            onClick={saveEscalation}
            disabled={escalationSaving}
            className="rounded bg-accent px-3 py-1.5 text-sm font-medium text-surface-900 hover:bg-accent-hover disabled:opacity-50"
          >
            {escalationSaving ? 'Saving...' : 'Save'}
          </button>
          {escalationSaved && <span className="text-xs text-emerald-400">Saved</span>}
        </div>
      </div>

      {/* Human Review */}
      <div className="rounded-sm border border-rule bg-surface-900 p-5 shadow-sm">
        <h2 className="mb-4 text-sm font-semibold text-fg">Human Review</h2>
        <div className="flex items-center gap-3">
          <button
            type="button"
            role="switch"
            aria-checked={humanReview.enabled}
            onClick={() => setHumanReview(h => ({ ...h, enabled: !h.enabled }))}
            className={[
              'relative inline-flex h-6 w-11 items-center rounded-full transition-colors focus:outline-none focus:ring-2 focus:ring-accent focus:ring-offset-2 focus:ring-offset-surface-900',
              humanReview.enabled ? 'bg-accent' : 'bg-surface-700',
            ].join(' ')}
          >
            <span
              className={[
                'inline-block h-4 w-4 rounded-full bg-white shadow-sm transition-transform',
                humanReview.enabled ? 'translate-x-6' : 'translate-x-1',
              ].join(' ')}
            />
          </button>
          <span className="text-sm text-muted">
            Global Human Review {humanReview.enabled ? 'Enabled' : 'Disabled'}
          </span>
        </div>
        <div className="mt-4 flex items-center gap-3">
          <button
            onClick={saveHumanReview}
            disabled={humanReviewSaving}
            className="rounded bg-accent px-3 py-1.5 text-sm font-medium text-surface-900 hover:bg-accent-hover disabled:opacity-50"
          >
            {humanReviewSaving ? 'Saving...' : 'Save'}
          </button>
          {humanReviewSaved && <span className="text-xs text-emerald-400">Saved</span>}
        </div>
      </div>
    </div>
  )
}
