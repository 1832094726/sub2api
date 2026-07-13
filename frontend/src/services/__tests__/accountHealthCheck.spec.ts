import { describe, expect, it, vi } from 'vitest'

import {
  parseAccountTestSSE,
  runAccountHealthChecks,
  type AccountHealthCheckResult
} from '../accountHealthCheck'

describe('account health check service', () => {
  it('parses terminal success and error events from account test SSE', () => {
    expect(parseAccountTestSSE('data: {"type":"test_complete","success":true}\n\n')).toEqual({
      success: true,
      error: ''
    })
    expect(parseAccountTestSSE('data: {"type":"error","error":"token invalidated"}\n\n')).toEqual({
      success: false,
      error: 'token invalidated'
    })
  })

  it('limits concurrency and reports each completed account', async () => {
    let active = 0
    let maxActive = 0
    const progress: AccountHealthCheckResult[] = []
    const check = vi.fn(async (accountId: number) => {
      active += 1
      maxActive = Math.max(maxActive, active)
      await new Promise(resolve => setTimeout(resolve, 5))
      active -= 1
      return { accountId, success: accountId % 2 === 0, error: accountId % 2 === 0 ? '' : 'failed' }
    })

    const summary = await runAccountHealthChecks([1, 2, 3, 4, 5], {
      concurrency: 2,
      check,
      onProgress: result => progress.push(result)
    })

    expect(maxActive).toBe(2)
    expect(progress).toHaveLength(5)
    expect(summary).toMatchObject({ total: 5, completed: 5, success: 2, failed: 3 })
  })
})
