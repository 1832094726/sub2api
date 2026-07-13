interface ProxyCandidate {
  id: number
  status?: string
}

export const DEFAULT_ACCOUNT_PRIORITY = 10

export function buildAccountCreateDefaults(proxies: ProxyCandidate[]) {
  const firstActiveProxy = proxies.find(proxy => proxy.status === 'active')
  return {
    priority: DEFAULT_ACCOUNT_PRIORITY,
    openaiPassthroughEnabled: true,
    proxyId: firstActiveProxy?.id ?? null
  }
}
