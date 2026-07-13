import { buildApiUrl } from '@/api/url'

export interface AccountHealthCheckResult {
  accountId: number
  success: boolean
  error: string
}

export interface AccountHealthCheckSummary {
  total: number
  completed: number
  success: number
  failed: number
  cancelled: boolean
  results: AccountHealthCheckResult[]
}

interface ParsedAccountTest {
  success: boolean
  error: string
}

interface RunAccountHealthChecksOptions {
  concurrency?: number
  signal?: AbortSignal
  check?: (accountId: number, signal?: AbortSignal) => Promise<AccountHealthCheckResult>
  onProgress?: (result: AccountHealthCheckResult, summary: AccountHealthCheckSummary) => void
}

export function parseAccountTestSSE(payload: string): ParsedAccountTest {
  let terminal: ParsedAccountTest | null = null

  for (const line of payload.split('\n')) {
    if (!line.startsWith('data: ')) continue
    const raw = line.slice(6).trim()
    if (!raw) continue
    try {
      const event = JSON.parse(raw) as { type?: string; success?: boolean; error?: string }
      if (event.type === 'error') {
        terminal = { success: false, error: event.error || 'Account test failed' }
      } else if (event.type === 'test_complete') {
        terminal = {
          success: event.success === true,
          error: event.success === true ? '' : event.error || 'Account test failed'
        }
      }
    } catch {
      // Ignore malformed diagnostic events and continue looking for a terminal event.
    }
  }

  return terminal || { success: false, error: 'Account test returned no completion event' }
}

export async function checkAccountHealth(accountId: number, signal?: AbortSignal): Promise<AccountHealthCheckResult> {
  try {
    const response = await fetch(buildApiUrl(`/admin/accounts/${accountId}/test`), {
      method: 'POST',
      headers: {
        Authorization: `Bearer ${localStorage.getItem('auth_token') || ''}`,
        'Content-Type': 'application/json'
      },
      body: JSON.stringify({ model_id: '', prompt: '', mode: 'default' }),
      signal
    })
    if (!response.ok) {
      return { accountId, success: false, error: `HTTP ${response.status}` }
    }
    const parsed = parseAccountTestSSE(await response.text())
    return { accountId, ...parsed }
  } catch (error) {
    const message = error instanceof Error ? error.message : String(error)
    return { accountId, success: false, error: message }
  }
}

export async function runAccountHealthChecks(
  accountIds: number[],
  options: RunAccountHealthChecksOptions = {}
): Promise<AccountHealthCheckSummary> {
  const ids = [...new Set(accountIds)]
  const summary: AccountHealthCheckSummary = {
    total: ids.length,
    completed: 0,
    success: 0,
    failed: 0,
    cancelled: false,
    results: []
  }
  const check = options.check || checkAccountHealth
  const concurrency = Math.max(1, Math.min(options.concurrency || 4, ids.length || 1))
  let cursor = 0

  const worker = async () => {
    while (cursor < ids.length) {
      if (options.signal?.aborted) {
        summary.cancelled = true
        return
      }
      const accountId = ids[cursor]
      cursor += 1
      const result = await check(accountId, options.signal)
      summary.results.push(result)
      summary.completed += 1
      if (result.success) summary.success += 1
      else summary.failed += 1
      options.onProgress?.(result, summary)
    }
  }

  await Promise.all(Array.from({ length: concurrency }, () => worker()))
  if (options.signal?.aborted) summary.cancelled = true
  return summary
}
