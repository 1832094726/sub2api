import { describe, expect, it } from 'vitest'
import { emptyGroupPricing, groupPricingFromAPI, groupPricingToAPI } from '../groupsModelPricing'

describe('group model pricing roundtrip', () => {
  it('preserves Fast/Flex/max and context settings without converting multipliers to per-token units', () => {
    const entries = groupPricingFromAPI([{
      platform: 'openai', models: ['gpt-6-astra'], billing_mode: 'token',
      cache_read_price: 0.0000025, fast_multiplier: 2.5, flex_multiplier: 0.5,
      max_reasoning_effort_multiplier: 1.7, long_context_threshold: 272000,
      long_context_multiplier: 2,
    }])
    expect(entries[0].cache_read_price).toBe(2.5)
    const saved = groupPricingToAPI(entries, 'openai')[0]
    expect(saved).toMatchObject({ cache_read_price: 0.0000025, fast_multiplier: 2.5,
      flex_multiplier: 0.5, max_reasoning_effort_multiplier: 1.7,
      long_context_threshold: 272000, long_context_multiplier: 2 })
  })
  it('serializes edited numeric input and supports clearing to inherited defaults', () => {
    const entry = { ...emptyGroupPricing(), models: ['model'], fast_multiplier: '2.5',
      long_context_threshold: '100000', long_context_multiplier: '3' }
    expect(groupPricingToAPI([entry], 'composite')[0]).toMatchObject({
      fast_multiplier: 2.5, long_context_threshold: 100000, long_context_multiplier: 3 })
    entry.fast_multiplier = ''
    expect(groupPricingToAPI([entry], 'composite')[0].fast_multiplier).toBeNull()
  })
})
