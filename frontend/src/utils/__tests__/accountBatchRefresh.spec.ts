import { describe, expect, it } from 'vitest'
import { summarizeBatchRefresh } from '../accountBatchRefresh'

describe('summarizeBatchRefresh', () => {
  it('only exposes missing-refresh-token accounts as deletable', () => {
    const summary = summarizeBatchRefresh({
      total: 4,
      success: 1,
      failed: 3,
      result_counts: {
        refreshed_and_verified: 1,
        missing_refresh_token: 2,
        token_unchanged: 1,
        refresh_token_invalidated: 3,
        unsupported_refresh_token: 4,
        transport_failed: 5,
        empty_access_token: 6
      },
      errors: [
        { account_id: 11, code: 'missing_refresh_token', error: 'missing' },
        { account_id: 12, code: 'token_unchanged', error: 'unchanged' },
        { account_id: 13, code: 'missing_refresh_token', error: 'missing' }
      ]
    })

    expect(summary.missingRefreshTokenIds).toEqual([11, 13])
    expect(summary.verified).toBe(1)
    expect(summary.unchanged).toBe(1)
    expect(summary.invalidatedRefreshToken).toBe(3)
    expect(summary.unsupportedRefreshToken).toBe(4)
    expect(summary.transportFailed).toBe(5)
    expect(summary.emptyAccessToken).toBe(6)
    expect(summary.otherFailed).toBe(0)
  })

  it('deduplicates deletable account IDs', () => {
    const summary = summarizeBatchRefresh({
      total: 2,
      success: 0,
      failed: 2,
      errors: [
        { account_id: 9, code: 'missing_refresh_token', error: 'missing' },
        { account_id: 9, code: 'missing_refresh_token', error: 'missing again' }
      ]
    })

    expect(summary.missingRefreshTokenIds).toEqual([9])
  })
})
