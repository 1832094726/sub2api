import { describe, expect, it } from 'vitest'
import { buildAccountCreateDefaults } from '../accountCreateDefaults'

describe('buildAccountCreateDefaults', () => {
  it('uses priority 10, enables passthrough, and selects the first active proxy', () => {
    const defaults = buildAccountCreateDefaults([
      { id: 3, status: 'inactive' },
      { id: 8, status: 'active' },
      { id: 9, status: 'active' }
    ])

    expect(defaults).toEqual({
      priority: 10,
      openaiPassthroughEnabled: true,
      proxyId: 8
    })
  })

  it('leaves proxy empty when no active proxy exists', () => {
    expect(buildAccountCreateDefaults([{ id: 3, status: 'inactive' }]).proxyId).toBeNull()
  })
})
