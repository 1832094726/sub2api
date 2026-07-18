interface BatchRefreshError {
  account_id: number
  code?: string
  error: string
}

interface BatchRefreshResultLike {
  total: number
  success: number
  failed: number
  errors?: BatchRefreshError[]
  result_counts?: Record<string, number>
}

export interface BatchRefreshSummary {
  verified: number
  missingRefreshToken: number
  unchanged: number
  unverified: number
  invalidatedRefreshToken: number
  unsupportedRefreshToken: number
  transportFailed: number
  emptyAccessToken: number
  otherFailed: number
  missingRefreshTokenIds: number[]
}

export function summarizeBatchRefresh(result: BatchRefreshResultLike): BatchRefreshSummary {
  const counts = result.result_counts || {}
  const missingRefreshTokenIds = [...new Set(
    (result.errors || [])
      .filter(item => item.code === 'missing_refresh_token')
      .map(item => item.account_id)
  )]

  return {
    verified: counts.refreshed_and_verified ?? result.success,
    missingRefreshToken: counts.missing_refresh_token ?? missingRefreshTokenIds.length,
    unchanged: counts.token_unchanged ?? 0,
    unverified: counts.refreshed_but_unverified ?? 0,
    invalidatedRefreshToken: counts.refresh_token_invalidated ?? 0,
    unsupportedRefreshToken: counts.unsupported_refresh_token ?? 0,
    transportFailed: counts.transport_failed ?? 0,
    emptyAccessToken: counts.empty_access_token ?? 0,
    otherFailed: counts.refresh_failed ?? 0,
    missingRefreshTokenIds
  }
}
