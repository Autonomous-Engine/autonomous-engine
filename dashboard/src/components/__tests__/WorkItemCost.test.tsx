import { describe, expect, it } from 'vitest'
import { formatCost } from '../WorkItemDetail'

// "Est. cost ~$0.0628" was shown for a local Ollama run from a hardcoded
// GPT-4o-ish rate (F33). Cost comes from the provider-aware endpoint and
// is only shown when every call was priced.
describe('formatCost', () => {
  it('shows the priced total, says when there is no pricing, and dashes when unknown', () => {
    expect(formatCost({ cost_usd: 0.0628, cost_unknown: false, unpriced_calls: 0 })).toBe('$0.0628')
    expect(formatCost({ cost_usd: null, cost_unknown: true, unpriced_calls: 45 })).toBe('no pricing (local model)')
    expect(formatCost(null)).toBe('—')
  })
})
