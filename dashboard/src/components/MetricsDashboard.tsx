import { useEffect, useState } from 'react'
import { useApi } from '@/hooks/useApi'
import {
  BarChart,
  Bar,
  PieChart,
  Pie,
  Cell,
  XAxis,
  YAxis,
  CartesianGrid,
  Tooltip,
  ResponsiveContainer,
  Legend,
} from 'recharts'

interface ModelUsage {
  name: string
  calls: number
  input_tokens: number
  output_tokens: number
  total_tokens: number
}

interface MetricsSummary {
  total_work_items: number
  by_state: Record<string, number>
  completed_last_7_days: number
  avg_cycle_time_hours: number
  qa_pass_rate: number
  total_inference_calls: number
  total_tokens_used: number
  active_agents: number
  by_provider: ModelUsage[]
  by_model: ModelUsage[]
}

const TOOLTIP_STYLE = {
  backgroundColor: '#1e293b',
  border: '1px solid #334155',
  borderRadius: '6px',
  color: '#e2e8f0',
  fontSize: '12px',
}

const STATE_ORDER = [
  'intake',
  'prd',
  'research',
  'planning',
  'implementation',
  'verification',
  'review',
  'human_review',
  'qa',
  'passed',
  'failed',
  'blocked',
  'rejected',
  'released',
]

const STATE_COLORS: Record<string, string> = {
  intake: '#6366f1',
  prd: '#8b5cf6',
  research: '#3b82f6',
  planning: '#06b6d4',
  implementation: '#10b981',
  verification: '#818cf8',
  review: '#f59e0b',
  human_review: '#f97316',
  qa: '#84cc16',
  passed: '#22c55e',
  failed: '#ef4444',
  blocked: '#f97316',
  rejected: '#6b7280',
  released: '#14b8a6',
}

interface SectionProps {
  title: string
  children: React.ReactNode
}

function Section({ title, children }: SectionProps) {
  return (
    <div className="rounded-sm border border-rule bg-surface-800 p-5">
      <h3 className="mb-4 text-sm font-semibold text-fg">{title}</h3>
      {children}
    </div>
  )
}

const USAGE_COLORS = ['#6366f1', '#8b5cf6', '#3b82f6', '#06b6d4', '#10b981', '#f59e0b', '#f97316', '#ef4444', '#ec4899', '#14b8a6']

function fmtTokens(n: number): string {
  if (n >= 1_000_000) return (n / 1_000_000).toFixed(1) + 'M'
  if (n >= 1_000) return (n / 1_000).toFixed(1) + 'k'
  return n.toLocaleString()
}

function EmptyChart() {
  return (
    <div className="flex h-[220px] items-center justify-center rounded-sm border border-dashed border-rule bg-surface-900/50">
      <p className="text-sm text-dim">No data yet — submit work items to see metrics</p>
    </div>
  )
}

export default function MetricsDashboard() {
  const { get } = useApi()
  const [summary, setSummary] = useState<MetricsSummary | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    get<MetricsSummary>('/metrics/summary')
      .then(data => setSummary(data))
      .catch((err) => {
        console.error('Failed to fetch:', err)
        setError(err instanceof Error ? err.message : 'Failed to load metrics')
      })
      .finally(() => setLoading(false))
  }, [get])

  if (loading) {
    return (
      <div className="flex h-64 items-center justify-center">
        <div className="h-8 w-8 animate-spin rounded-full border-2 border-accent border-t-transparent" />
      </div>
    )
  }

  const hasData = summary !== null && summary.total_work_items > 0

  // Build by-state bar chart data (ordered)
  const byStateData = STATE_ORDER
    .filter(s => summary?.by_state?.[s] !== undefined)
    .map(s => ({ state: s, count: summary!.by_state[s] }))

  // Build quality pie data from passed/failed/blocked
  const qualityData = [
    { name: 'Passed QA', value: summary?.by_state?.['passed'] ?? 0, color: '#22c55e' },
    { name: 'Failed QA', value: summary?.by_state?.['failed'] ?? 0, color: '#ef4444' },
    { name: 'Blocked', value: summary?.by_state?.['blocked'] ?? 0, color: '#f97316' },
  ].filter(d => d.value > 0)

  const cycleTimeHours = summary?.avg_cycle_time_hours ?? 0
  const cycleTimeDisplay = cycleTimeHours >= 1
    ? `${cycleTimeHours.toFixed(1)}h`
    : cycleTimeHours > 0
      ? `${Math.round(cycleTimeHours * 60)}m`
      : '—'

  const passRatePct = summary
    ? Math.round((summary.qa_pass_rate ?? 0) * 100)
    : 0

  const roiCards = [
    {
      label: 'Total Work Items',
      value: summary?.total_work_items?.toString() ?? '0',
      sub: 'all time',
    },
    {
      label: 'Completed (7d)',
      value: summary?.completed_last_7_days?.toString() ?? '0',
      sub: 'last 7 days',
    },
    {
      label: 'Avg Cycle Time',
      value: cycleTimeDisplay,
      sub: 'intake → passed',
    },
    {
      label: 'QA Pass Rate',
      value: passRatePct > 0 ? `${passRatePct}%` : '—',
      sub: 'no rework needed',
    },
    {
      label: 'Inference Calls',
      value: summary?.total_inference_calls?.toLocaleString() ?? '0',
      sub: 'total LLM calls',
    },
    {
      label: 'Active Agents',
      value: summary?.active_agents?.toString() ?? '0',
      sub: 'currently running',
    },
  ]

  return (
    <div className="flex flex-col gap-6">
      {/* Header */}
      <div>
        <h2 className="text-lg font-semibold text-fg">Metrics Dashboard</h2>
        <p className="text-sm text-dim">System performance overview — live data</p>
      </div>

      {error && (
        <div className="rounded-sm border border-red-700/50 bg-red-900/20 px-4 py-3 text-sm text-red-300">
          {error}
        </div>
      )}

      {!hasData && !error && (
        <div className="rounded-sm border border-rule bg-surface-800 px-6 py-10 text-center">
          <p className="text-muted">
            No data yet — submit work items to see metrics
          </p>
        </div>
      )}

      {/* ROI Summary cards */}
      <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-6">
        {roiCards.map((card) => (
          <div
            key={card.label}
            className="rounded-sm border border-rule bg-surface-800 p-4"
          >
            <p className="text-xs text-dim">{card.label}</p>
            <p className="mt-1 text-2xl font-bold text-fg">{card.value}</p>
            <p className="mt-0.5 text-xs text-dim">{card.sub}</p>
          </div>
        ))}
      </div>

      {/* Charts grid */}
      <div className="grid gap-5 lg:grid-cols-2">
        {/* Work items by state bar chart */}
        <Section title="Pipeline — Items by State">
          {byStateData.length === 0 ? (
            <EmptyChart />
          ) : (
            <ResponsiveContainer width="100%" height={220}>
              <BarChart data={byStateData} margin={{ top: 4, right: 16, left: -10, bottom: 0 }}>
                <CartesianGrid strokeDasharray="3 3" stroke="#334155" />
                <XAxis
                  dataKey="state"
                  tick={{ fontSize: 10, fill: '#64748b' }}
                  axisLine={false}
                  tickLine={false}
                />
                <YAxis
                  tick={{ fontSize: 11, fill: '#64748b' }}
                  axisLine={false}
                  tickLine={false}
                  allowDecimals={false}
                />
                <Tooltip contentStyle={TOOLTIP_STYLE} cursor={{ fill: '#1e293b' }} />
                <Bar dataKey="count" radius={[4, 4, 0, 0]} name="Items">
                  {byStateData.map((entry) => (
                    <Cell
                      key={entry.state}
                      fill={STATE_COLORS[entry.state] ?? '#6366f1'}
                    />
                  ))}
                </Bar>
              </BarChart>
            </ResponsiveContainer>
          )}
        </Section>

        {/* Quality pie chart */}
        <Section title="Quality — QA Outcome Distribution">
          {qualityData.length === 0 ? (
            <EmptyChart />
          ) : (
            <ResponsiveContainer width="100%" height={220}>
              <PieChart>
                <Pie
                  data={qualityData}
                  cx="50%"
                  cy="50%"
                  innerRadius={60}
                  outerRadius={90}
                  paddingAngle={3}
                  dataKey="value"
                  label={({ name, percent }) =>
                    `${name} ${(percent * 100).toFixed(0)}%`
                  }
                  labelLine={{ stroke: '#475569', strokeWidth: 1 }}
                >
                  {qualityData.map((entry, index) => (
                    <Cell key={`cell-${index}`} fill={entry.color} />
                  ))}
                </Pie>
                <Tooltip
                  contentStyle={TOOLTIP_STYLE}
                  formatter={(value: number) => [`${value}`, '']}
                />
                <Legend
                  iconType="circle"
                  iconSize={8}
                  wrapperStyle={{ fontSize: '11px', color: '#94a3b8' }}
                />
              </PieChart>
            </ResponsiveContainer>
          )}
        </Section>

        {/* Usage by Provider */}
        <Section title="Usage by Provider">
          {!summary?.by_provider?.length ? (
            <EmptyChart />
          ) : (
            <div className="space-y-4">
              <ResponsiveContainer width="100%" height={220}>
                <BarChart data={summary.by_provider} margin={{ top: 4, right: 16, left: -10, bottom: 0 }}>
                  <CartesianGrid strokeDasharray="3 3" stroke="#334155" />
                  <XAxis dataKey="name" tick={{ fontSize: 11, fill: '#64748b' }} axisLine={false} tickLine={false} />
                  <YAxis tick={{ fontSize: 11, fill: '#64748b' }} axisLine={false} tickLine={false} allowDecimals={false} />
                  <Tooltip contentStyle={TOOLTIP_STYLE} cursor={{ fill: '#1e293b' }} formatter={(v: number) => v.toLocaleString()} />
                  <Bar dataKey="calls" name="Calls" radius={[4, 4, 0, 0]}>
                    {summary.by_provider.map((_, i) => (
                      <Cell key={i} fill={USAGE_COLORS[i % USAGE_COLORS.length]} />
                    ))}
                  </Bar>
                </BarChart>
              </ResponsiveContainer>
              <div className="space-y-2">
                {summary.by_provider.map((p, i) => (
                  <div key={p.name} className="flex items-center justify-between rounded-sm border border-rule bg-surface-900 px-3 py-2">
                    <div className="flex items-center gap-2">
                      <span className="h-2.5 w-2.5 rounded-full" style={{ backgroundColor: USAGE_COLORS[i % USAGE_COLORS.length] }} />
                      <span className="text-sm font-medium text-fg capitalize">{p.name}</span>
                    </div>
                    <div className="flex gap-4 text-xs text-muted">
                      <span>{p.calls.toLocaleString()} calls</span>
                      <span>{fmtTokens(p.input_tokens)} in</span>
                      <span>{fmtTokens(p.output_tokens)} out</span>
                      <span className="font-medium text-fg">{fmtTokens(p.total_tokens)} total</span>
                    </div>
                  </div>
                ))}
              </div>
            </div>
          )}
        </Section>

        {/* Usage by Model */}
        <Section title="Usage by Model">
          {!summary?.by_model?.length ? (
            <EmptyChart />
          ) : (
            <div className="space-y-4">
              <ResponsiveContainer width="100%" height={Math.max(220, summary.by_model.length * 36 + 40)}>
                <BarChart data={summary.by_model} layout="vertical" margin={{ top: 4, right: 16, left: 10, bottom: 0 }}>
                  <CartesianGrid strokeDasharray="3 3" stroke="#334155" horizontal={false} />
                  <XAxis type="number" tick={{ fontSize: 11, fill: '#64748b' }} axisLine={false} tickLine={false} allowDecimals={false} />
                  <YAxis type="category" dataKey="name" tick={{ fontSize: 10, fill: '#94a3b8' }} axisLine={false} tickLine={false} width={140} />
                  <Tooltip contentStyle={TOOLTIP_STYLE} cursor={{ fill: '#1e293b' }} formatter={(v: number) => v.toLocaleString()} />
                  <Bar dataKey="calls" name="Calls" radius={[0, 4, 4, 0]}>
                    {summary.by_model.map((_, i) => (
                      <Cell key={i} fill={USAGE_COLORS[i % USAGE_COLORS.length]} />
                    ))}
                  </Bar>
                </BarChart>
              </ResponsiveContainer>
              <div className="space-y-2">
                {summary.by_model.map((m, i) => (
                  <div key={m.name} className="flex items-center justify-between rounded-sm border border-rule bg-surface-900 px-3 py-2">
                    <div className="flex items-center gap-2">
                      <span className="h-2.5 w-2.5 rounded-full" style={{ backgroundColor: USAGE_COLORS[i % USAGE_COLORS.length] }} />
                      <span className="text-sm font-medium text-fg">{m.name}</span>
                    </div>
                    <div className="flex gap-4 text-xs text-muted">
                      <span>{m.calls.toLocaleString()} calls</span>
                      <span>{fmtTokens(m.input_tokens)} in</span>
                      <span>{fmtTokens(m.output_tokens)} out</span>
                      <span className="font-medium text-fg">{fmtTokens(m.total_tokens)} total</span>
                    </div>
                  </div>
                ))}
              </div>
            </div>
          )}
        </Section>

        {/* Active state summary */}
        <Section title="Active Work — Current Pipeline Breakdown">
          {byStateData.length === 0 ? (
            <EmptyChart />
          ) : (
            <div className="space-y-3">
              {byStateData
                .filter(d => !['passed', 'failed', 'rejected', 'released'].includes(d.state))
                .map(({ state, count }) => {
                  const total = byStateData
                    .filter(d => !['passed', 'failed', 'rejected', 'released'].includes(d.state))
                    .reduce((sum, d) => sum + d.count, 0)
                  const pct = total > 0 ? Math.round((count / total) * 100) : 0
                  return (
                    <div key={state}>
                      <div className="mb-1 flex items-center justify-between text-xs">
                        <span className="text-muted capitalize">{state.replace('_', ' ')}</span>
                        <span className="font-medium text-muted">{count}</span>
                      </div>
                      <div className="h-2 overflow-hidden rounded-full bg-surface-700">
                        <div
                          className="h-full rounded-full transition-all"
                          style={{
                            width: `${pct}%`,
                            backgroundColor: STATE_COLORS[state] ?? '#6366f1',
                          }}
                        />
                      </div>
                    </div>
                  )
                })}
              <p className="pt-1 text-xs text-dim">
                Active items currently in flight through the pipeline
              </p>
            </div>
          )}
        </Section>
      </div>
    </div>
  )
}
