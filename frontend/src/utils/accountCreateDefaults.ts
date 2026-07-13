interface ProxyCandidate {
  id: number
  status?: string
}

interface GroupCandidate {
  id: number
  status?: string
  platform?: string
}

export const DEFAULT_ACCOUNT_PRIORITY = 10

export function buildAccountCreateDefaults(
  proxies: ProxyCandidate[],
  groups: GroupCandidate[] = [],
  platform = 'openai'
) {
  const firstActiveProxy = proxies.find(proxy => proxy.status === 'active')
  const firstActiveGroup = groups.find(group => group.status === 'active' && group.platform === platform)
  return {
    priority: DEFAULT_ACCOUNT_PRIORITY,
    openaiPassthroughEnabled: true,
    proxyId: firstActiveProxy?.id ?? null,
    groupIds: firstActiveGroup ? [firstActiveGroup.id] : []
  }
}
